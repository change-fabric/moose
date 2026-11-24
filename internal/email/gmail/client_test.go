package gmail_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"google.golang.org/api/option"

	"github.com/change-fabric/moose/internal/email/gmail"
	"github.com/change-fabric/moose/internal/email/provider"
)

func b64(s string) string { return base64.URLEncoding.EncodeToString([]byte(s)) }

func newClient(t *testing.T, h http.Handler) *gmail.Client {
	t.Helper()
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)
	c, err := gmail.New(context.Background(), option.WithEndpoint(ts.URL), option.WithoutAuthentication(), option.WithHTTPClient(ts.Client()))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func TestSearchSendsCompiledQueryAndCursor(t *testing.T) {
	var gotQ, gotToken, gotMax string
	c := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/gmail/v1/users/me/messages":
			gotQ, gotToken, gotMax = r.URL.Query().Get("q"), r.URL.Query().Get("pageToken"), r.URL.Query().Get("maxResults")
			writeJSON(w, map[string]any{"messages": []map[string]string{{"id": "m1"}}, "nextPageToken": "next"})
		case r.URL.Path == "/gmail/v1/users/me/messages/m1":
			writeJSON(w, map[string]any{
				"id": "m1", "threadId": "t1", "labelIds": []string{"INBOX", "UNREAD"}, "internalDate": "1767323045000",
				"payload": map[string]any{"headers": []map[string]string{
					{"name": "From", "value": "Bob <bob@example.com>"},
					{"name": "To", "value": "me@example.com, Al <al@example.com>"},
					{"name": "Subject", "value": "hello"},
				}},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	res, err := c.Search(context.Background(), provider.Query{From: "bob@example.com", Unread: true, Limit: 7, Cursor: "tok"})
	if err != nil {
		t.Fatal(err)
	}
	if gotQ != `from:"bob@example.com" is:unread` || gotToken != "tok" || gotMax != "7" {
		t.Errorf("request q=%q token=%q max=%q", gotQ, gotToken, gotMax)
	}
	if res.NextCursor != "next" || len(res.Messages) != 1 {
		t.Fatalf("result %+v", res)
	}
	m := res.Messages[0]
	if m.From.Email != "bob@example.com" || m.From.Name != "Bob" || len(m.To) != 2 || m.Subject != "hello" || !m.Unread || m.ThreadID != "t1" {
		t.Errorf("message %+v", m)
	}
}

func TestGetMessageBodyAndAttachments(t *testing.T) {
	c := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"id": "m1", "payload": map[string]any{
			"mimeType": "multipart/mixed",
			"parts": []map[string]any{
				{"mimeType": "text/plain", "body": map[string]any{"data": b64("hi there")}},
				{"mimeType": "text/html", "body": map[string]any{"data": b64("<b>hi</b>")}},
				{"mimeType": "application/pdf", "filename": "a.pdf", "body": map[string]any{"attachmentId": "att1", "size": 12}},
			},
		}})
	}))
	m, err := c.GetMessage(context.Background(), "m1")
	if err != nil {
		t.Fatal(err)
	}
	if m.BodyText != "hi there" {
		t.Errorf("body %q", m.BodyText)
	}
	if len(m.Attachments) != 1 || m.Attachments[0].ID != "att1" || m.Attachments[0].Filename != "a.pdf" {
		t.Errorf("attachments %+v", m.Attachments)
	}
}

func TestOpenAttachment(t *testing.T) {
	c := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/attachments/att1") {
			writeJSON(w, map[string]any{"data": b64("pdf-bytes")})
			return
		}
		writeJSON(w, map[string]any{"id": "m1", "payload": map[string]any{"parts": []map[string]any{
			{"mimeType": "application/pdf", "filename": "a.pdf", "body": map[string]any{"attachmentId": "att1", "size": 9}},
		}}})
	}))
	rc, meta, err := c.OpenAttachment(context.Background(), "m1", "att1")
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	b, _ := io.ReadAll(rc)
	if string(b) != "pdf-bytes" || meta.Filename != "a.pdf" {
		t.Errorf("got %q %+v", b, meta)
	}
	if _, _, err := c.OpenAttachment(context.Background(), "m1", "nope"); !errors.Is(err, provider.ErrNotFound) {
		t.Errorf("unknown attachment err = %v", err)
	}
}

func TestDraftAndSendCarryExactBytes(t *testing.T) {
	raw := "From: me@example.com\r\nTo: a@example.com\r\nSubject: s\r\n\r\nbody é\r\n"
	var sent, drafted string
	c := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Raw     string `json:"raw"`
			Message struct {
				Raw string `json:"raw"`
			} `json:"message"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		switch {
		case strings.HasSuffix(r.URL.Path, "/messages/send"):
			sent = body.Raw
			writeJSON(w, map[string]any{"id": "sent1", "threadId": "t9"})
		case strings.HasSuffix(r.URL.Path, "/drafts") && r.Method == http.MethodPost:
			drafted = body.Message.Raw
			writeJSON(w, map[string]any{"id": "d1", "message": map[string]any{"id": "dm1", "threadId": "t9"}})
		case strings.HasSuffix(r.URL.Path, "/drafts/d1") && r.Method == http.MethodGet:
			writeJSON(w, map[string]any{"id": "d1", "message": map[string]any{"raw": b64(raw)}})
		default:
			http.NotFound(w, r)
		}
	}))
	ctx := context.Background()
	d, err := c.CreateDraft(ctx, provider.DraftInput{Raw: []byte(raw), ThreadID: "t9"})
	if err != nil || d.ID != "d1" || d.MessageID != "dm1" {
		t.Fatalf("create %+v %v", d, err)
	}
	got, err := c.GetDraftRaw(ctx, "d1")
	if err != nil || string(got) != raw {
		t.Fatalf("raw %q %v", got, err)
	}
	sm, err := c.SendDraft(ctx, []byte(raw), "t9")
	if err != nil || sm.MessageID != "sent1" {
		t.Fatalf("send %+v %v", sm, err)
	}
	for name, v := range map[string]string{"draft": drafted, "send": sent} {
		if b, err := base64.URLEncoding.DecodeString(v); err != nil || string(b) != raw {
			t.Errorf("%s bytes differ: %q %v", name, b, err)
		}
	}
}

func TestErrorsMapToSentinels(t *testing.T) {
	for status, want := range map[int]error{404: provider.ErrNotFound, 401: provider.ErrAuth, 400: provider.ErrInvalidQuery} {
		c := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"error":{"message":"x"}}`))
		}))
		if _, err := c.GetMessage(context.Background(), "m"); !errors.Is(err, want) {
			t.Errorf("status %d: err = %v, want %v", status, err, want)
		}
	}
}

func TestListMailboxes(t *testing.T) {
	c := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"labels": []map[string]string{{"id": "INBOX", "name": "INBOX", "type": "system"}, {"id": "Label_1", "name": "Acme", "type": "user"}}})
	}))
	mb, err := c.ListMailboxes(context.Background())
	if err != nil || len(mb) != 2 || !mb[0].System || mb[1].System || mb[1].Name != "Acme" {
		t.Fatalf("%+v %v", mb, err)
	}
	if got := c.Capabilities(); got.SearchDialect != provider.DialectGmail || !got.Threads || got.NativeDraftSend {
		t.Errorf("capabilities %+v", got)
	}
}

func TestProfileReadsAddressAndSenderName(t *testing.T) {
	c := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/gmail/v1/users/me/profile":
			writeJSON(w, map[string]any{"emailAddress": "Pat@Example.com"})
		case "/gmail/v1/users/me/settings/sendAs":
			writeJSON(w, map[string]any{"sendAs": []map[string]any{
				{"sendAsEmail": "alias@example.com", "displayName": "Alias"},
				{"sendAsEmail": "pat@example.com", "displayName": "Pat Taylor", "isPrimary": true},
			}})
		default:
			http.NotFound(w, r)
		}
	}))
	p, err := c.Profile(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if p.Address != "Pat@Example.com" || p.DisplayName != "Pat Taylor" {
		t.Fatalf("profile = %+v", p)
	}
}

func TestProfileSurvivesSendAsFailureAndMapsAuthErrors(t *testing.T) {
	c := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/gmail/v1/users/me/profile" {
			writeJSON(w, map[string]any{"emailAddress": "pat@example.com"})
			return
		}
		http.Error(w, "forbidden", http.StatusForbidden)
	}))
	p, err := c.Profile(context.Background())
	if err != nil || p.Address != "pat@example.com" || p.DisplayName != "" {
		t.Fatalf("profile = %+v, %v", p, err)
	}
	bad := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusUnauthorized)
	}))
	if _, err := bad.Profile(context.Background()); !errors.Is(err, provider.ErrAuth) {
		t.Fatalf("401 err = %v, want provider.ErrAuth", err)
	}
}
