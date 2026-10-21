package gdrive

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/api/option"

	"github.com/change-fabric/moose/internal/core"
)

type fakeFile struct {
	meta    map[string]any
	content string
}

type fakeDrive struct {
	t     *testing.T
	mu    sync.Mutex
	files map[string]fakeFile
	// failures queued per route key ("list", "get", "about", "export", "media")
	fail     map[string][]int
	failBody map[int]string
	calls    map[string]int
	lastQ    []string
	lastList []map[string][]string
}

func newFake(t *testing.T) (*fakeDrive, *Connector) {
	t.Helper()
	fd := &fakeDrive{t: t, files: map[string]fakeFile{}, fail: map[string][]int{}, failBody: map[int]string{}, calls: map[string]int{}}
	srv := httptest.NewServer(fd)
	t.Cleanup(srv.Close)
	c, err := NewWithOptions(context.Background(), option.WithEndpoint(srv.URL+"/"), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	c.SetBackoff([]time.Duration{0, 0, 0, 0})
	return fd, c
}

func (fd *fakeDrive) add(id, name, mime, content string) {
	fd.files[id] = fakeFile{meta: map[string]any{
		"id": id, "name": name, "mimeType": mime, "modifiedTime": "2026-01-02T03:04:05Z",
		"webViewLink": "https://drive.example/" + id,
	}, content: content}
}

func writeErr(w http.ResponseWriter, code int, reason string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{
		"code": code, "message": reason, "errors": []map[string]any{{"reason": reason, "message": reason}},
	}})
}

var defaultReason = map[int]string{403: "userRateLimitExceeded", 404: "notFound", 429: "rateLimitExceeded", 500: "backendError", 503: "backendError"}

func (fd *fakeDrive) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	fd.mu.Lock()
	defer fd.mu.Unlock()
	path := r.URL.Path
	if i := strings.Index(path, "/about"); i >= 0 {
		path = path[i:]
	} else if i := strings.Index(path, "/files"); i >= 0 {
		path = path[i:]
	}
	q := r.URL.Query()
	var key, id string
	switch {
	case path == "/about":
		key = "about"
	case path == "/files":
		key = "list"
	case strings.HasSuffix(path, "/export"):
		key, id = "export", strings.TrimSuffix(strings.TrimPrefix(path, "/files/"), "/export")
	case strings.HasPrefix(path, "/files/"):
		id = strings.TrimPrefix(path, "/files/")
		key = "get"
		if q.Get("alt") == "media" {
			key = "media"
		}
	default:
		fd.t.Errorf("unexpected path %q", r.URL.Path)
		writeErr(w, 400, "badRequest")
		return
	}
	fd.calls[key]++
	if key != "about" && key != "export" && q.Get("supportsAllDrives") != "true" {
		fd.t.Errorf("%s call missing supportsAllDrives: %s", key, r.URL.RawQuery)
	}
	if queue := fd.fail[key]; len(queue) > 0 {
		code := queue[0]
		fd.fail[key] = queue[1:]
		reason := fd.failBody[code]
		if reason == "" {
			reason = defaultReason[code]
		}
		writeErr(w, code, reason)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	switch key {
	case "about":
		_ = json.NewEncoder(w).Encode(map[string]any{"user": map[string]any{"emailAddress": "sa@proj.iam.gserviceaccount.com"}})
	case "list":
		if q.Get("includeItemsFromAllDrives") != "true" || q.Get("corpora") != "user" {
			fd.t.Errorf("list missing all-drives params: %s", r.URL.RawQuery)
		}
		fd.lastQ = append(fd.lastQ, q.Get("q"))
		files := []map[string]any{}
		for _, f := range fd.files {
			files = append(files, f.meta)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"files": files, "nextPageToken": "tok2"})
	case "get":
		f, ok := fd.files[id]
		if !ok {
			writeErr(w, 404, "notFound")
			return
		}
		_ = json.NewEncoder(w).Encode(f.meta)
	case "export", "media":
		f, ok := fd.files[id]
		if !ok {
			writeErr(w, 404, "notFound")
			return
		}
		if key == "export" {
			want := map[string]string{"application/vnd.google-apps.spreadsheet": "text/csv"}[f.meta["mimeType"].(string)]
			if want == "" {
				want = "text/plain"
			}
			if got := q.Get("mimeType"); got != want {
				fd.t.Errorf("export mimeType = %q, want %q", got, want)
			}
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write([]byte(f.content))
	}
}

func TestGetMimeBranches(t *testing.T) {
	fd, c := newFake(t)
	fd.add("doc", "Doc", "application/vnd.google-apps.document", "doc text")
	fd.add("pres", "Slides", "application/vnd.google-apps.presentation", "slide text")
	fd.add("sheet", "Sheet", "application/vnd.google-apps.spreadsheet", "a,b\n1,2\n")
	fd.add("txt", "a.txt", "text/plain", "plain")
	fd.add("md", "a.md", "text/markdown", "# md")
	fd.add("csv", "a.csv", "text/csv", "x,y")
	fd.add("json", "a.json", "application/json", `{"k":1}`)
	fd.add("bin", "a.png", "image/png", "\x89PNG")
	fd.add("dir", "Folder", folderMime, "")

	cases := []struct {
		id, ct, content string
	}{
		{"doc", core.ContentTypePlain, "doc text"},
		{"pres", core.ContentTypePlain, "slide text"},
		{"sheet", core.ContentTypeCSV, "a,b\n1,2\n"},
		{"txt", core.ContentTypePlain, "plain"},
		{"md", core.ContentTypeMarkdown, "# md"},
		{"csv", core.ContentTypeCSV, "x,y"},
		{"json", core.ContentTypePlain, `{"k":1}`},
		{"bin", core.ContentTypeMetadataOnly, ""},
	}
	for _, tc := range cases {
		t.Run(tc.id, func(t *testing.T) {
			d, err := c.Get(context.Background(), tc.id, 0)
			if err != nil {
				t.Fatal(err)
			}
			if d.ContentType != tc.ct || d.Content != tc.content || d.Truncated {
				t.Fatalf("got ct=%q content=%q trunc=%v", d.ContentType, d.Content, d.Truncated)
			}
			if d.Source != ID || d.ID != tc.id || d.URL == "" || d.Modified.IsZero() {
				t.Fatalf("bad ref %+v", d.DocRef)
			}
		})
	}
	t.Run("sheet note", func(t *testing.T) {
		d, _ := c.Get(context.Background(), "sheet", 0)
		if d.Snippet != firstSheetNote {
			t.Fatalf("snippet %q", d.Snippet)
		}
	})
	t.Run("folder", func(t *testing.T) {
		d, err := c.Get(context.Background(), "dir", 0)
		if err != nil {
			t.Fatal(err)
		}
		if d.ContentType != core.ContentTypePlain || !strings.Contains(d.Content, "Folder: Folder") || !strings.Contains(d.Content, "a.md") {
			t.Fatalf("folder content %q", d.Content)
		}
		if got := fd.lastQ[len(fd.lastQ)-1]; got != "'dir' in parents and trashed = false" {
			t.Fatalf("folder q %q", got)
		}
	})
}

func TestTruncation(t *testing.T) {
	fd, c := newFake(t)
	fd.add("big", "big.txt", "text/plain", strings.Repeat("x", 100))
	d, err := c.Get(context.Background(), "big", 10)
	if err != nil {
		t.Fatal(err)
	}
	if !d.Truncated || len(d.Content) != 10 {
		t.Fatalf("trunc=%v len=%d", d.Truncated, len(d.Content))
	}
	fd.add("exact", "e.txt", "text/plain", strings.Repeat("x", 10))
	d, _ = c.Get(context.Background(), "exact", 10)
	if d.Truncated || len(d.Content) != 10 {
		t.Fatalf("exact: trunc=%v len=%d", d.Truncated, len(d.Content))
	}
}

func TestNotFoundMapping(t *testing.T) {
	fd, c := newFake(t)
	_, err := c.Get(context.Background(), "missing", 0)
	if !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("missing: %v", err)
	}
	fd.add("hidden", "h", "text/plain", "secret")
	fd.fail["get"] = []int{403}
	fd.failBody[403] = "notFound"
	_, err2 := c.Get(context.Background(), "hidden", 0)
	if !errors.Is(err2, core.ErrNotFound) {
		t.Fatalf("403 notFound: %v", err2)
	}
	if err.Error() != err2.Error() {
		t.Fatalf("errors distinguish existence: %q vs %q", err, err2)
	}
}

func TestRetryThenSuccess(t *testing.T) {
	fd, c := newFake(t)
	fd.add("a", "a.txt", "text/plain", "ok")
	fd.fail["get"] = []int{429, 500, 403}
	fd.fail["media"] = []int{503}
	d, err := c.Get(context.Background(), "a", 0)
	if err != nil || d.Content != "ok" {
		t.Fatalf("d=%+v err=%v", d, err)
	}
	if fd.calls["get"] != 4 || fd.calls["media"] != 2 {
		t.Fatalf("calls %v", fd.calls)
	}
}

func TestRetryExhaustion(t *testing.T) {
	fd, c := newFake(t)
	fd.fail["list"] = []int{503, 503, 503, 503, 503, 503}
	_, err := c.List(context.Background(), "", 0)
	if !errors.Is(err, ErrUpstreamUnavailable) {
		t.Fatalf("err %v", err)
	}
	if fd.calls["list"] != 5 {
		t.Fatalf("calls %d, want 5", fd.calls["list"])
	}
}

func TestNoRetryOnPlainForbidden(t *testing.T) {
	fd, c := newFake(t)
	fd.fail["list"] = []int{400}
	fd.failBody[400] = "badRequest"
	if _, err := c.List(context.Background(), "", 0); err == nil || errors.Is(err, ErrUpstreamUnavailable) {
		t.Fatalf("err %v", err)
	}
	if fd.calls["list"] != 1 {
		t.Fatalf("calls %d", fd.calls["list"])
	}
}

func TestExportTooLarge(t *testing.T) {
	fd, c := newFake(t)
	fd.add("d", "d", "application/vnd.google-apps.document", "x")
	fd.fail["export"] = []int{403}
	fd.failBody[403] = "exportSizeLimitExceeded"
	if _, err := c.Get(context.Background(), "d", 0); !errors.Is(err, ErrDocumentTooLarge) {
		t.Fatalf("err %v", err)
	}
}

func TestListAndSearchQueries(t *testing.T) {
	fd, c := newFake(t)
	fd.add("a", "a", "text/plain", "")
	res, err := c.List(context.Background(), "", 0)
	if err != nil || len(res.Items) != 1 || res.NextPageToken != "tok2" {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	if fd.lastQ[0] != "sharedWithMe = true and trashed = false" {
		t.Fatalf("list q %q", fd.lastQ[0])
	}
	if _, err := c.Search(context.Background(), core.SearchQuery{Text: `it's a\b`}); err != nil {
		t.Fatal(err)
	}
	want := `(fullText contains 'it\'s a\\b' or name contains 'it\'s a\\b') and trashed = false`
	if fd.lastQ[1] != want {
		t.Fatalf("search q %q, want %q", fd.lastQ[1], want)
	}
}

func TestHealth(t *testing.T) {
	_, c := newFake(t)
	if err := c.Health(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := c.Detail(context.Background()); !strings.Contains(got, "sa@proj.iam.gserviceaccount.com") {
		t.Fatalf("detail %q", got)
	}
}

func TestNewRejectsBadKey(t *testing.T) {
	if _, err := New(context.Background(), []byte("{}")); err == nil {
		t.Fatal("expected error")
	}
}
