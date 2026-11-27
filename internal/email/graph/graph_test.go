package graph_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/change-fabric/moose/internal/email/graph"
	"github.com/change-fabric/moose/internal/email/provider"
	"github.com/change-fabric/moose/internal/email/provider/providertest"
	"github.com/change-fabric/moose/internal/graphapi"
)

func TestCapabilities(t *testing.T) {
	c, _ := newGraph(t)
	want := provider.Capabilities{Threads: true, Labels: false, SearchDialect: provider.DialectGraph, NativeDraftSend: true}
	if got := c.Capabilities(); got != want {
		t.Errorf("capabilities = %+v, want %+v", got, want)
	}
}

func seed(t *testing.T, s *stub, box, subject, body string, at time.Time, unread bool) providertest.Seeded {
	t.Helper()
	return s.SeedMessage(t, providertest.SeedMessage{Mailbox: box, From: provider.Address{Email: "bob@example.com"},
		To: []provider.Address{{Email: "me@example.com"}}, Subject: subject, Body: body, Date: at, Unread: unread})
}

func TestSearchTextUsesSearchAndNeverOrderBy(t *testing.T) {
	c, s := newGraph(t)
	hit := seed(t, s, provider.MailboxInbox, "Quarterly plan", "numbers", time.Now(), false)
	seed(t, s, provider.MailboxInbox, "Lunch", "sandwich", time.Now(), false)
	res, err := c.Search(context.Background(), provider.Query{Mailbox: provider.MailboxInbox, Text: `quarterly" OR from:anyone`})
	if err != nil {
		t.Fatal(err)
	}
	// The injected operator is flattened into the phrase, so nothing matches it.
	if len(res.Messages) != 0 {
		t.Errorf("injected text matched %v", res.Messages)
	}
	res, err = c.Search(context.Background(), provider.Query{Mailbox: provider.MailboxInbox, Text: "quarterly"})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Messages) != 1 || res.Messages[0].ID != hit.ID {
		t.Errorf("text search = %+v, want only %s", res.Messages, hit.ID)
	}
	last := s.Requests()[len(s.Requests())-1]
	if !strings.Contains(last, "%24search=") || strings.Contains(last, "orderby") || strings.Contains(last, "filter") {
		t.Errorf("text search request = %s, want $search only", last)
	}
	if !strings.Contains(last, "/me/mailFolders/inbox/messages") {
		t.Errorf("request = %s, want the inbox well-known path", last)
	}
}

func TestSearchUnreadPlusTextIsRefusedBeforeAnyRequest(t *testing.T) {
	c, s := newGraph(t)
	_, err := c.Search(context.Background(), provider.Query{Text: "plan", Unread: true})
	if !errors.Is(err, provider.ErrInvalidQuery) {
		t.Fatalf("err = %v, want ErrInvalidQuery", err)
	}
	if n := len(s.Requests()); n != 0 {
		t.Errorf("refused query made %d requests", n)
	}
}

func TestSearchWithoutMailboxSpansAllFolders(t *testing.T) {
	c, s := newGraph(t)
	seed(t, s, provider.MailboxInbox, "a", "x", time.Now(), false)
	seed(t, s, provider.MailboxSent, "b", "x", time.Now(), false)
	res, err := c.Search(context.Background(), provider.Query{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Messages) != 2 {
		t.Fatalf("got %d messages, want 2", len(res.Messages))
	}
	for _, m := range res.Messages {
		if len(m.Mailboxes) != 1 || (m.Mailboxes[0] != provider.MailboxInbox && m.Mailboxes[0] != provider.MailboxSent) {
			t.Errorf("mailboxes = %v, want a neutral id", m.Mailboxes)
		}
	}
}

func TestCursorOnlyCarriesThePagingToken(t *testing.T) {
	c, s := newGraph(t)
	for i := 0; i < 3; i++ {
		seed(t, s, provider.MailboxInbox, "m", "x", time.Now().Add(time.Duration(i)*time.Minute), false)
	}
	res, err := c.Search(context.Background(), provider.Query{Mailbox: provider.MailboxInbox, Limit: 2})
	if err != nil || res.NextCursor == "" {
		t.Fatalf("first page: %v cursor %q", err, res.NextCursor)
	}
	if strings.Contains(res.NextCursor, "http") || strings.Contains(strings.ToLower(res.NextCursor), "filter") {
		t.Errorf("cursor %q leaks more than a paging token", res.NextCursor)
	}
	for _, bad := range []string{"%24filter=x", "http://evil.example.com/", "%24skip=1&%24select=body", "%zz"} {
		if _, err := c.Search(context.Background(), provider.Query{Mailbox: provider.MailboxInbox, Cursor: bad}); !errors.Is(err, provider.ErrInvalidQuery) {
			t.Errorf("cursor %q err = %v, want ErrInvalidQuery", bad, err)
		}
	}
}

func TestListMailboxesIncludesUserFolders(t *testing.T) {
	c, _ := newGraph(t)
	boxes, err := c.ListMailboxes(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var sys, user []string
	for _, b := range boxes {
		if b.System {
			sys = append(sys, b.ID)
		} else {
			user = append(user, b.ID+"="+b.Name)
		}
	}
	if strings.Join(sys, ",") != "INBOX,SENT,DRAFT,TRASH,SPAM" {
		t.Errorf("system = %v", sys)
	}
	if len(user) != 1 || user[0] != "AAFolderClients==Clients" {
		t.Errorf("user folders = %v", user)
	}
}

func TestMessageInUserFolderKeepsItsGraphID(t *testing.T) {
	c, s := newGraph(t)
	m := s.SeedMessage(t, providertest.SeedMessage{Mailbox: provider.MailboxInbox, From: provider.Address{Email: "b@example.com"}, Subject: "s", Body: "b", Date: time.Now()})
	s.msgs[0].folder = "AAFolderClients="
	got, err := c.GetMessage(context.Background(), m.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Mailboxes) != 1 || got.Mailboxes[0] != "AAFolderClients=" {
		t.Errorf("mailboxes = %v", got.Mailboxes)
	}
}

func TestThreadSortsOldestFirstWithoutOrderBy(t *testing.T) {
	c, s := newGraph(t)
	late := s.SeedMessage(t, providertest.SeedMessage{Mailbox: provider.MailboxInbox, ThreadKey: "k", From: provider.Address{Email: "b@example.com"}, Subject: "late", Body: "b", Date: time.Now()})
	early := s.SeedMessage(t, providertest.SeedMessage{Mailbox: provider.MailboxSent, ThreadKey: "k", From: provider.Address{Email: "b@example.com"}, Subject: "early", Body: "b", Date: time.Now().Add(-time.Hour)})
	th, err := c.GetThread(context.Background(), late.ThreadID)
	if err != nil {
		t.Fatal(err)
	}
	if len(th.Messages) != 2 || th.Messages[0].ID != early.ID {
		t.Errorf("thread = %+v", th.Messages)
	}
	if last := s.Requests()[len(s.Requests())-1]; strings.Contains(last, "orderby") {
		t.Errorf("thread request %s sorts server side, which Exchange rejects with a conversationId filter", last)
	}
}

func TestThreadFollowsPages(t *testing.T) {
	c, s := newGraph(t)
	var ids []string
	for i := 0; i < 60; i++ { // more than one page of 50
		m := s.SeedMessage(t, providertest.SeedMessage{Mailbox: provider.MailboxInbox, ThreadKey: "big", From: provider.Address{Email: "b@example.com"}, Subject: "m", Body: "b", Date: time.Now().Add(time.Duration(i) * time.Minute)})
		ids = append(ids, m.ThreadID)
	}
	th, err := c.GetThread(context.Background(), ids[0])
	if err != nil {
		t.Fatal(err)
	}
	if len(th.Messages) != 60 {
		t.Errorf("thread has %d messages, want 60", len(th.Messages))
	}
}

func TestGetMessageReadsHTMLBodyReferencesAndAttachments(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/v1.0/me/mailFolders/"):
			id := "AAFolder-" + strings.TrimPrefix(r.URL.Path, "/v1.0/me/mailFolders/")
			if strings.HasSuffix(r.URL.Path, "/inbox") {
				id = "AAFolderInbox="
			}
			_, _ = w.Write([]byte(`{"id":"` + id + `"}`))
		default:
			_, _ = w.Write(fixture("message_html.json"))
		}
	}))
	defer ts.Close()
	g := graphapi.New(nil)
	g.BaseURL = ts.URL + "/v1.0"
	m, err := graph.New(g).GetMessage(context.Background(), "AAMkAGFixtureMessageOne=")
	if err != nil {
		t.Fatal(err)
	}
	if m.BodyText != "" || !strings.Contains(m.BodyHTML, "<p>Hello Me") {
		t.Errorf("body text %q html %q", m.BodyText, m.BodyHTML)
	}
	if m.From.Name != "Bob Example" || m.From.Email != "bob@example.com" || len(m.To) != 1 || len(m.Cc) != 1 {
		t.Errorf("addresses from=%+v to=%v cc=%v", m.From, m.To, m.Cc)
	}
	if m.References != "<a@example.com> <b@example.com>" {
		t.Errorf("references = %q", m.References)
	}
	if m.RFCMessageID != "<fixture-1@example.com>" || !m.Unread || m.ThreadID != "AAQkAGFixtureConversationOne=" {
		t.Errorf("message = %+v", m)
	}
	if len(m.Attachments) != 1 || m.Attachments[0].Filename != "plan.pdf" || m.Attachments[0].Size != 1234 {
		t.Errorf("attachments = %+v (an unnamed inline part is not listed)", m.Attachments)
	}
	if len(m.Mailboxes) != 1 || m.Mailboxes[0] != provider.MailboxInbox {
		t.Errorf("mailboxes = %v", m.Mailboxes)
	}
	if !m.Date.Equal(time.Date(2026, 3, 4, 9, 0, 0, 0, time.UTC)) {
		t.Errorf("date = %v", m.Date)
	}
}

func TestMalformedIDIsNotFound(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/v1.0/me/mailFolders/") {
			_, _ = w.Write([]byte(`{"id":"f"}`))
			return
		}
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write(fixture("error_id_malformed.json"))
	}))
	defer ts.Close()
	g := graphapi.New(nil)
	g.BaseURL = ts.URL + "/v1.0"
	if _, err := graph.New(g).GetMessage(context.Background(), "not-an-id"); !errors.Is(err, provider.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestAttachmentSizeCaps(t *testing.T) {
	var valueHits int
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/$value"):
			valueHits++
			// Lies about its size: the stream is longer than the metadata said.
			w.Header().Set("Transfer-Encoding", "chunked")
			chunk := make([]byte, 1<<20)
			for i := 0; i < 27; i++ {
				_, _ = w.Write(chunk)
				w.(http.Flusher).Flush()
			}
		case strings.Contains(r.URL.Path, "/attachments/big"):
			_, _ = w.Write([]byte(`{"@odata.type":"#microsoft.graph.fileAttachment","id":"big","name":"big.bin","size":` + "99999999" + `}`))
		case strings.Contains(r.URL.Path, "/attachments/ref"):
			_, _ = w.Write([]byte(`{"@odata.type":"#microsoft.graph.referenceAttachment","id":"ref","name":"link","size":1}`))
		case strings.Contains(r.URL.Path, "/attachments/liar"):
			_, _ = w.Write([]byte(`{"@odata.type":"#microsoft.graph.fileAttachment","id":"liar","name":"liar.bin","size":10}`))
		default:
			_, _ = w.Write([]byte(`{"id":"f"}`))
		}
	}))
	defer ts.Close()
	g := graphapi.New(nil)
	g.BaseURL = ts.URL + "/v1.0"
	c := graph.New(g)
	ctx := context.Background()

	if _, _, err := c.OpenAttachment(ctx, "m", "big"); !errors.Is(err, provider.ErrTooLarge) {
		t.Errorf("declared-big err = %v, want ErrTooLarge", err)
	}
	if valueHits != 0 {
		t.Errorf("a declared-too-large attachment was downloaded")
	}
	if _, _, err := c.OpenAttachment(ctx, "m", "ref"); !errors.Is(err, provider.ErrRejected) {
		t.Errorf("reference attachment err = %v, want ErrRejected", err)
	}
	rc, _, err := c.OpenAttachment(ctx, "m", "liar")
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	n, err := io.Copy(io.Discard, rc)
	if !errors.Is(err, provider.ErrTooLarge) || n != provider.MaxAttachment {
		t.Errorf("oversized stream: read %d bytes, err %v; want exactly MaxAttachment then ErrTooLarge", n, err)
	}
}

func TestRawMessageStreamsMIMEAndCapsSize(t *testing.T) {
	big := false
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if big {
			_, _ = w.Write([]byte(strings.Repeat("x", provider.MaxDraftRaw+10)))
			return
		}
		w.Header().Set("Content-Type", "message/rfc822")
		_, _ = w.Write([]byte("From: a@example.com\r\nX-MS-Exchange-Noise: 1\r\n\r\nhi"))
	}))
	defer ts.Close()
	g := graphapi.New(nil)
	g.BaseURL = ts.URL + "/v1.0"
	c := graph.New(g)
	raw, err := c.RawMessage(context.Background(), "m1")
	if err != nil || !strings.HasPrefix(string(raw), "From: a@example.com") {
		t.Fatalf("raw = %q, %v", raw, err)
	}
	big = true
	if _, err := c.RawMessage(context.Background(), "m1"); !errors.Is(err, provider.ErrDraftTooLarge) {
		t.Errorf("oversize err = %v, want ErrDraftTooLarge", err)
	}
}
