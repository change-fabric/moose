package gmail_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/api/option"

	"github.com/change-fabric/moose/internal/email/gmail"
	"github.com/change-fabric/moose/internal/email/provider"
	"github.com/change-fabric/moose/internal/email/provider/providertest"
)

// stub is a small in-memory Gmail REST server: enough of labels, messages,
// threads, attachments, drafts and send for the conformance suites. It speaks
// Gmail's JSON and error shapes, so the real client code runs against it.
type stub struct {
	mu        sync.Mutex
	msgs      []*stubMsg
	drafts    map[string]*stubDraft
	delivered [][]byte
	seq       int
	fail      providertest.Failure
}

type stubMsg struct {
	id, thread, mailbox, subject, body string
	from                               provider.Address
	to                                 []provider.Address
	date                               time.Time
	unread                             bool
	att                                *providertest.SeedAttachment
	attID                              string
}

type stubDraft struct {
	id, msgID, thread string
	raw               []byte
}

func newStub() *stub { return &stub{drafts: map[string]*stubDraft{}} }

func (s *stub) SeedMessage(t *testing.T, m providertest.SeedMessage) providertest.Seeded {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seq++
	sm := &stubMsg{id: "m" + strconv.Itoa(s.seq), mailbox: m.Mailbox, subject: m.Subject, body: m.Body, from: m.From, to: m.To,
		date: m.Date, unread: m.Unread, att: m.Attachment}
	sm.thread = "t" + sm.id
	if m.ThreadKey != "" {
		sm.thread = "thread-" + m.ThreadKey
	}
	if m.Attachment != nil {
		sm.attID = "att" + strconv.Itoa(s.seq)
	}
	s.msgs = append(s.msgs, sm)
	return providertest.Seeded{ID: sm.id, ThreadID: sm.thread, AttachmentID: sm.attID}
}

func (s *stub) Fail(t *testing.T, f providertest.Failure) {
	s.mu.Lock()
	s.fail = f
	s.mu.Unlock()
}

func (s *stub) Delivered(t *testing.T) [][]byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([][]byte(nil), s.delivered...)
}

func gerr(w http.ResponseWriter, code int, reason string, hdr map[string]string) {
	for k, v := range hdr {
		w.Header().Set(k, v)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{
		"code": code, "message": reason, "errors": []map[string]string{{"reason": reason, "message": reason}},
	}})
}

func writeOne(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

var systemLabels = map[string]string{"in:inbox": "INBOX", "in:sent": "SENT", "in:drafts": "DRAFT", "in:trash": "TRASH", "in:spam": "SPAM"}

func (s *stub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	path := strings.TrimPrefix(r.URL.Path, "/gmail/v1/users/me/")
	switch s.fail {
	case providertest.FailAuth:
		gerr(w, http.StatusUnauthorized, "authError", nil)
		return
	case providertest.FailRateLimit:
		gerr(w, http.StatusTooManyRequests, "rateLimitExceeded", map[string]string{"Retry-After": strconv.Itoa(int(providertest.RetryAfter / time.Second))})
		return
	}
	switch {
	case path == "labels" && r.Method == http.MethodGet:
		writeOne(w, map[string]any{"labels": []map[string]string{
			{"id": "INBOX", "name": "INBOX", "type": "system"}, {"id": "SENT", "name": "SENT", "type": "system"},
			{"id": "DRAFT", "name": "DRAFT", "type": "system"}, {"id": "TRASH", "name": "TRASH", "type": "system"},
			{"id": "SPAM", "name": "SPAM", "type": "system"}, {"id": "Label_1", "name": "Clients", "type": "user"},
		}})
	case path == "messages" && r.Method == http.MethodGet:
		s.list(w, r)
	case path == "messages/send" && r.Method == http.MethodPost:
		s.send(w, r)
	case strings.HasPrefix(path, "messages/") && strings.Contains(path, "/attachments/"):
		s.attachment(w, path)
	case strings.HasPrefix(path, "messages/") && r.Method == http.MethodGet:
		if m := s.find(strings.TrimPrefix(path, "messages/")); m != nil {
			writeOne(w, s.render(m))
			return
		}
		gerr(w, http.StatusNotFound, "notFound", nil)
	case strings.HasPrefix(path, "threads/") && r.Method == http.MethodGet:
		s.thread(w, strings.TrimPrefix(path, "threads/"))
	case path == "drafts" && r.Method == http.MethodPost:
		s.putDraft(w, r, "")
	case strings.HasPrefix(path, "drafts/"):
		id := strings.TrimPrefix(path, "drafts/")
		switch r.Method {
		case http.MethodPut:
			s.putDraft(w, r, id)
		case http.MethodGet:
			s.getDraft(w, r, id)
		case http.MethodDelete:
			if _, ok := s.drafts[id]; !ok {
				gerr(w, http.StatusNotFound, "notFound", nil)
				return
			}
			delete(s.drafts, id)
			w.WriteHeader(http.StatusNoContent)
		}
	default:
		gerr(w, http.StatusNotFound, "notFound", nil)
	}
}

func (s *stub) find(id string) *stubMsg {
	for _, m := range s.msgs {
		if m.id == id {
			return m
		}
	}
	return nil
}

func b64u(b []byte) string { return base64.URLEncoding.EncodeToString(b) }

func (s *stub) render(m *stubMsg) map[string]any {
	labels := []string{m.mailbox}
	if m.unread {
		labels = append(labels, "UNREAD")
	}
	addrs := func(l []provider.Address) string {
		parts := make([]string, len(l))
		for i, a := range l {
			parts[i] = a.Email
		}
		return strings.Join(parts, ", ")
	}
	from := m.from.Email
	if m.from.Name != "" {
		from = m.from.Name + " <" + m.from.Email + ">"
	}
	parts := []map[string]any{{"mimeType": "text/plain", "body": map[string]any{"data": b64u([]byte(m.body)), "size": len(m.body)}}}
	if m.att != nil {
		parts = append(parts, map[string]any{"mimeType": m.att.MIMEType, "filename": m.att.Filename,
			"body": map[string]any{"attachmentId": m.attID, "size": len(m.att.Data)}})
	}
	return map[string]any{
		"id": m.id, "threadId": m.thread, "labelIds": labels, "snippet": m.body,
		"internalDate": strconv.FormatInt(m.date.UnixMilli(), 10),
		"payload": map[string]any{"mimeType": "multipart/mixed", "headers": []map[string]string{
			{"name": "From", "value": from}, {"name": "To", "value": addrs(m.to)}, {"name": "Subject", "value": m.subject},
			{"name": "Date", "value": m.date.Format(time.RFC1123Z)},
		}, "parts": parts},
	}
}

// list understands the terms CompileQuery emits that the suites use: in:, is:unread, and quoted from:/to:/subject:.
func (s *stub) list(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query().Get("q")
	var box string
	unread := false
	for _, term := range strings.Fields(q) {
		switch {
		case term == "is:unread":
			unread = true
		case systemLabels[term] != "":
			box = systemLabels[term]
		}
	}
	var hits []*stubMsg
	for _, m := range s.msgs {
		if (box == "" || m.mailbox == box) && (!unread || m.unread) {
			hits = append(hits, m)
		}
	}
	sort.SliceStable(hits, func(i, j int) bool { return hits[i].date.After(hits[j].date) })
	max, _ := strconv.Atoi(r.URL.Query().Get("maxResults"))
	if max <= 0 {
		max = 100
	}
	offset, _ := strconv.Atoi(r.URL.Query().Get("pageToken"))
	if offset > len(hits) {
		offset = len(hits)
	}
	end := offset + max
	resp := map[string]any{}
	if end < len(hits) {
		resp["nextPageToken"] = strconv.Itoa(end)
	} else {
		end = len(hits)
	}
	refs := []map[string]string{}
	for _, m := range hits[offset:end] {
		refs = append(refs, map[string]string{"id": m.id, "threadId": m.thread})
	}
	resp["messages"] = refs
	writeOne(w, resp)
}

func (s *stub) attachment(w http.ResponseWriter, path string) {
	segs := strings.Split(path, "/") // messages/{id}/attachments/{aid}
	if len(segs) == 4 {
		if m := s.find(segs[1]); m != nil && m.att != nil && m.attID == segs[3] {
			writeOne(w, map[string]any{"data": b64u(m.att.Data), "size": len(m.att.Data)})
			return
		}
	}
	gerr(w, http.StatusNotFound, "notFound", nil)
}

func (s *stub) thread(w http.ResponseWriter, id string) {
	var in []*stubMsg
	for _, m := range s.msgs {
		if m.thread == id {
			in = append(in, m)
		}
	}
	if len(in) == 0 {
		gerr(w, http.StatusNotFound, "notFound", nil)
		return
	}
	sort.SliceStable(in, func(i, j int) bool { return in[i].date.Before(in[j].date) })
	out := make([]map[string]any, len(in))
	for i, m := range in {
		out[i] = s.render(m)
	}
	writeOne(w, map[string]any{"id": id, "messages": out})
}

type rawBody struct {
	Raw      string `json:"raw"`
	ThreadID string `json:"threadId"`
}

func (s *stub) send(w http.ResponseWriter, r *http.Request) {
	switch s.fail {
	case providertest.FailSendRejected:
		gerr(w, http.StatusForbidden, "forbidden", nil)
		return
	}
	var in rawBody
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		gerr(w, http.StatusBadRequest, "invalidArgument", nil)
		return
	}
	raw, err := base64.URLEncoding.DecodeString(in.Raw)
	if err != nil {
		gerr(w, http.StatusBadRequest, "invalidArgument", nil)
		return
	}
	s.delivered = append(s.delivered, raw)
	s.seq++
	if s.fail == providertest.FailSendResponseLost {
		// The message is out; drop the connection before answering.
		if hj, ok := w.(http.Hijacker); ok {
			conn, _, err := hj.Hijack()
			if err == nil {
				_ = conn.Close()
			}
		}
		return
	}
	writeOne(w, map[string]any{"id": "s" + strconv.Itoa(s.seq), "threadId": in.ThreadID})
}

func (s *stub) putDraft(w http.ResponseWriter, r *http.Request, id string) {
	var in struct {
		Message rawBody `json:"message"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		gerr(w, http.StatusBadRequest, "invalidArgument", nil)
		return
	}
	raw, err := base64.URLEncoding.DecodeString(in.Message.Raw)
	if err != nil {
		gerr(w, http.StatusBadRequest, "invalidArgument", nil)
		return
	}
	var d *stubDraft
	if id == "" {
		s.seq++
		d = &stubDraft{id: "d" + strconv.Itoa(s.seq), msgID: "dm" + strconv.Itoa(s.seq)}
		s.drafts[d.id] = d
	} else if d = s.drafts[id]; d == nil {
		gerr(w, http.StatusNotFound, "notFound", nil)
		return
	}
	d.raw, d.thread = raw, in.Message.ThreadID
	writeOne(w, map[string]any{"id": d.id, "message": map[string]any{"id": d.msgID, "threadId": d.thread}})
}

func (s *stub) getDraft(w http.ResponseWriter, r *http.Request, id string) {
	d := s.drafts[id]
	if d == nil {
		gerr(w, http.StatusNotFound, "notFound", nil)
		return
	}
	msg := map[string]any{"id": d.msgID, "sizeEstimate": len(d.raw)}
	if r.URL.Query().Get("format") == "raw" {
		// Gmail prepends a trace header to a stored draft; the client strips it.
		trace := "Received: from localhost by gmailapi.google.com with HTTPREST;\r\n Thu, 04 Mar 2026 10:00:00 -0500\r\n"
		msg["raw"] = b64u(append([]byte(trace), d.raw...))
	}
	writeOne(w, map[string]any{"id": d.id, "message": msg})
}

func gmailFactory(t *testing.T) (provider.Provider, providertest.Seeder) {
	t.Helper()
	s := newStub()
	ts := httptest.NewServer(s)
	t.Cleanup(ts.Close)
	c, err := gmail.New(context.Background(), option.WithEndpoint(ts.URL), option.WithoutAuthentication(), option.WithHTTPClient(ts.Client()))
	if err != nil {
		t.Fatal(err)
	}
	return c, s
}

func TestConformanceAgainstAGmailStub(t *testing.T) {
	t.Run("Read", func(t *testing.T) { providertest.RunRead(t, gmailFactory) })
	t.Run("Draft", func(t *testing.T) { providertest.RunDraft(t, gmailFactory) })
	t.Run("Send", func(t *testing.T) { providertest.RunSend(t, gmailFactory) })
}

func TestGmailRateLimitCarriesRetryAfter(t *testing.T) {
	for name, tc := range map[string]struct {
		header string
		want   time.Duration
	}{
		"seconds":               {"3", 3 * time.Second},
		"absent":                {"", 0},
		"unparseable":           {"soon", 0},
		"negative":              {"-5", 0},
		"http date in the past": {time.Now().Add(-time.Hour).UTC().Format(http.TimeFormat), 0},
	} {
		t.Run(name, func(t *testing.T) {
			c := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				hdr := map[string]string{}
				if tc.header != "" {
					hdr["Retry-After"] = tc.header
				}
				gerr(w, http.StatusTooManyRequests, "rateLimitExceeded", hdr)
			}))
			_, err := c.ListMailboxes(context.Background())
			var rl *provider.RateLimitError
			if !errors.As(err, &rl) {
				t.Fatalf("err = %v, want *RateLimitError", err)
			}
			if rl.RetryAfter != tc.want {
				t.Fatalf("RetryAfter = %v, want %v", rl.RetryAfter, tc.want)
			}
		})
	}
}

func TestGmailRateLimitFromAnHTTPDate(t *testing.T) {
	c := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gerr(w, http.StatusTooManyRequests, "rateLimitExceeded", map[string]string{
			"Retry-After": time.Now().Add(30 * time.Second).UTC().Format(http.TimeFormat)})
	}))
	_, err := c.ListMailboxes(context.Background())
	var rl *provider.RateLimitError
	if !errors.As(err, &rl) || rl.RetryAfter < 25*time.Second || rl.RetryAfter > 31*time.Second {
		t.Fatalf("err = %v (%+v), want about 30s", err, rl)
	}
}

func TestGmailQuotaForbiddenIsAlsoRateLimited(t *testing.T) {
	c := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gerr(w, http.StatusForbidden, "userRateLimitExceeded", map[string]string{"Retry-After": "2"})
	}))
	_, err := c.ListMailboxes(context.Background())
	var rl *provider.RateLimitError
	if !errors.As(err, &rl) || rl.RetryAfter != 2*time.Second {
		t.Fatalf("err = %v, want a RateLimitError of 2s", err)
	}
}
