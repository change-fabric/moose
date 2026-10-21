package server_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/change-fabric/moose/internal/api"
	"github.com/change-fabric/moose/internal/auth"
	"github.com/change-fabric/moose/internal/connectors/fake"
	"github.com/change-fabric/moose/internal/core"
	"github.com/change-fabric/moose/internal/server"
	"github.com/change-fabric/moose/internal/store/sqlite"
)

const me = "me@example.com"

type env struct {
	t   *testing.T
	srv *httptest.Server
	a   *auth.Authenticator
}

func newEnv(t *testing.T) *env {
	t.Helper()
	ctx := context.Background()
	st, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "moose.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := auth.EnsureBootstrapAdmin(ctx, st, me, time.Now()); err != nil {
		t.Fatal(err)
	}
	conn := fake.New("gdrive",
		core.Document{DocRef: core.DocRef{ID: "doc1", Title: "Roadmap", MimeType: "text/plain", Modified: time.Now()}, Content: "the moose roadmap", ContentType: "text/plain"},
		core.Document{DocRef: core.DocRef{ID: "doc2", Title: "Budget", MimeType: "text/csv", Modified: time.Now()}, Content: "a,b\n1,2", ContentType: "text/csv"},
	)
	svc, err := core.NewService([]core.Connector{conn}, core.Options{})
	if err != nil {
		t.Fatal(err)
	}
	a := &auth.Authenticator{Store: st, Identity: auth.DevIdentity{Login: me}}
	h := server.New(server.Deps{Service: svc, Store: st, Auth: a, Version: "test"})
	e := &env{t: t, srv: httptest.NewServer(h), a: a}
	t.Cleanup(func() { e.srv.Close(); a.Wait() })
	return e
}

func (e *env) do(method, path, token string, body any, out any) (int, http.Header) {
	e.t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, e.srv.URL+path, rd)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if out != nil && resp.StatusCode < 300 {
		if err := json.Unmarshal(data, out); err != nil {
			e.t.Fatalf("%s %s: decode %q: %v", method, path, data, err)
		}
	}
	return resp.StatusCode, resp.Header
}

func (e *env) want(method, path, token string, body any, wantStatus int, out any) http.Header {
	e.t.Helper()
	got, hdr := e.do(method, path, token, body, out)
	if got != wantStatus {
		e.t.Fatalf("%s %s = %d, want %d", method, path, got, wantStatus)
	}
	return hdr
}

func TestEndToEnd(t *testing.T) {
	e := newEnv(t)

	var health map[string]any
	e.want("GET", "/healthz", "", nil, 200, &health)
	if health["ok"] != true || health["version"] != "test" {
		t.Fatalf("healthz = %v", health)
	}

	// No token: 401 with the Bearer challenge.
	hdr := e.want("GET", "/v1/whoami", "", nil, 401, nil)
	if !strings.Contains(hdr.Get("WWW-Authenticate"), `Bearer realm="moose"`) {
		t.Fatalf("WWW-Authenticate = %q", hdr.Get("WWW-Authenticate"))
	}

	var boot auth.BootstrapResponse
	e.want("POST", "/v1/bootstrap/token", "", map[string]string{"name": "dev"}, 201, &boot)
	e.want("POST", "/v1/bootstrap/token", "", map[string]string{"name": "again"}, 410, nil)
	tok := boot.Token

	var who api.WhoAmI
	e.want("GET", "/v1/whoami", tok, nil, 200, &who)
	if who.Login != me || who.Role != auth.RoleAdmin || who.TokenID != boot.ID || who.Identity.Login != me {
		t.Fatalf("whoami = %+v", who)
	}

	var srcs struct{ Sources []api.SourceInfo }
	e.want("GET", "/v1/sources", tok, nil, 200, &srcs)
	if len(srcs.Sources) != 1 || srcs.Sources[0].ID != "gdrive" || !srcs.Sources[0].Healthy {
		t.Fatalf("sources = %+v", srcs)
	}

	var res api.ResultPage
	e.want("GET", "/v1/search?q=roadmap", tok, nil, 200, &res)
	if len(res.Items) != 1 || res.Items[0].ID != "doc1" {
		t.Fatalf("search = %+v", res)
	}
	e.want("GET", "/v1/search", tok, nil, 400, nil)
	e.want("GET", "/v1/search?q="+strings.Repeat("x", 257), tok, nil, 400, nil)
	e.want("GET", "/v1/search?q=x&source=nope", tok, nil, 400, nil)

	var list api.ResultPage
	e.want("GET", "/v1/list", tok, nil, 200, &list)
	if len(list.Items) != 2 {
		t.Fatalf("list = %+v", list)
	}

	var doc core.Document
	e.want("GET", "/v1/documents/gdrive/doc1", tok, nil, 200, &doc)
	if doc.Content != "the moose roadmap" || doc.Title != "Roadmap" {
		t.Fatalf("doc = %+v", doc)
	}
	e.want("GET", "/v1/documents/gdrive/missing", tok, nil, 404, nil)

	// Users and member tokens.
	e.want("POST", "/v1/users", tok, map[string]string{"login": "alice@example.com"}, 201, nil)
	e.want("POST", "/v1/users", tok, map[string]string{"login": "x", "role": "root"}, 400, nil)
	var users struct{ Users []api.UserInfo }
	e.want("GET", "/v1/users", tok, nil, 200, &users)
	if len(users.Users) != 2 {
		t.Fatalf("users = %+v", users)
	}
	var aliceTok api.CreatedToken
	e.want("POST", "/v1/tokens", tok, map[string]any{"name": "alice-laptop", "owner": "alice@example.com"}, 201, &aliceTok)
	if aliceTok.Token == "" || aliceTok.Owner != "alice@example.com" || aliceTok.Expires == nil {
		t.Fatalf("created = %+v", aliceTok)
	}
	// Alice's token used from my device fails the owner cross-check.
	e.want("GET", "/v1/whoami", aliceTok.Token, nil, 403, nil)
	e.want("POST", "/v1/tokens", tok, map[string]any{"name": "ghost", "owner": "ghost@example.com"}, 400, nil)

	var second api.CreatedToken
	e.want("POST", "/v1/tokens", tok, map[string]any{"name": "second"}, 201, &second)
	var toks struct{ Tokens []api.TokenInfo }
	e.want("GET", "/v1/tokens", tok, nil, 200, &toks)
	if len(toks.Tokens) != 2 {
		t.Fatalf("own tokens = %+v", toks)
	}
	e.want("GET", "/v1/tokens?all=1", tok, nil, 200, &toks)
	if len(toks.Tokens) != 3 {
		t.Fatalf("all tokens = %+v", toks)
	}

	// Revoke the second token, then it gets 401.
	e.want("GET", "/v1/whoami", second.Token, nil, 200, nil)
	e.want("DELETE", "/v1/tokens/"+second.ID, tok, nil, 204, nil)
	e.want("GET", "/v1/whoami", second.Token, nil, 401, nil)
	e.want("DELETE", "/v1/tokens/nosuchtoken", tok, nil, 404, nil)

	e.want("GET", "/mcp", tok, nil, 501, nil)
	e.want("GET", "/v1/nope", tok, nil, 404, nil)

	var aud struct{ Events []api.AuditInfo }
	e.want("GET", "/v1/audit?since=1h&limit=500", tok, nil, 200, &aud)
	seen := map[string]int{}
	for _, ev := range aud.Events {
		seen[ev.Action+" "+itoa(ev.Status)]++
	}
	for _, k := range []string{
		"healthz 200", "token.bootstrap 201", "token.bootstrap 410", "whoami 200", "GET /v1/whoami 401", "GET /v1/whoami 403",
		"list_sources 200", "search 200", "search 400", "list 200", "get_document 200", "get_document 404",
		"user.add 201", "user.list 200", "token.create 201", "token.list 200", "token.revoke 204",
	} {
		if seen[k] == 0 {
			t.Errorf("audit missing %q; have %v", k, seen)
		}
	}
	for _, ev := range aud.Events {
		if ev.Action == "get_document" && ev.Status == 200 && (ev.Source != "gdrive" || ev.DocID != "doc1" || ev.Actor != me) {
			t.Errorf("get_document audit = %+v", ev)
		}
	}
}

func TestNonAdminLimits(t *testing.T) {
	e := newEnv(t)
	var boot auth.BootstrapResponse
	e.want("POST", "/v1/bootstrap/token", "", map[string]string{"name": "dev"}, 201, &boot)
	e.want("POST", "/v1/users", boot.Token, map[string]string{"login": me, "role": "member"}, 201, nil)
	tok := boot.Token
	e.want("GET", "/v1/users", tok, nil, 403, nil)
	e.want("GET", "/v1/audit", tok, nil, 403, nil)
	e.want("GET", "/v1/tokens?all=1", tok, nil, 403, nil)
	e.want("POST", "/v1/tokens", tok, map[string]any{"name": "x", "owner": "other@example.com"}, 403, nil)
	e.want("POST", "/v1/tokens", tok, map[string]any{"name": "x", "expires_in_days": 0}, 403, nil)
	e.want("POST", "/v1/tokens", tok, map[string]any{"name": "x", "expires_in_days": 7}, 201, nil)
}

func itoa(n int) string { b, _ := json.Marshal(n); return string(b) }
