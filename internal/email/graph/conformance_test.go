package graph_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/change-fabric/moose/internal/email/graph"
	"github.com/change-fabric/moose/internal/email/provider"
	"github.com/change-fabric/moose/internal/email/provider/providertest"
	"github.com/change-fabric/moose/internal/graphapi"
)

// stub is a small in-memory Graph REST server: enough of mailFolders, messages,
// attachments and $value for RunRead. It speaks Graph's JSON and error shapes
// (the error bodies and the folder list are the synthetic fixtures), and it
// enforces the request rules the client must follow: the ImmutableId Prefer
// header, and Exchange's InefficientFilter restriction on $orderby.
type stub struct {
	mu   sync.Mutex
	msgs []*stubMsg
	seq  int
	fail providertest.Failure
	base string // server URL plus /v1.0, for nextLink
	// requests records "METHOD path?query" for assertions.
	requests []string
	// delivered is what the recipient side holds for each accepted send.
	delivered [][]byte
	// sendAccepts counts POST .../send calls the stub accepted.
	sendAccepts int
	// mutateSent, when set, rewrites a message as it is delivered, so a test can
	// make the Sent copy differ from what was approved.
	mutateSent func(*stubMsg)
}

type stubMsg struct {
	id, conv, folder, subject, body string
	from                            provider.Address
	to                              []provider.Address
	date                            time.Time
	unread                          bool
	att                             *providertest.SeedAttachment
	attID                           string
	// Draft and send state (draftstub_test.go).
	cc, bcc  []provider.Address
	isDraft  bool
	modified time.Time
	sent     time.Time
	msgID    string
	hasAtt   bool
	htmlBody bool
}

// folderIDs are the ids in testdata/synthetic/mailfolders.json.
var folderIDs = map[string]string{
	provider.MailboxInbox: "AAFolderInbox=", provider.MailboxSent: "AAFolderSent=", provider.MailboxDraft: "AAFolderDrafts=",
	provider.MailboxTrash: "AAFolderDeleted=", provider.MailboxSpam: "AAFolderJunk=",
}

var aliases = map[string]string{
	"inbox": provider.MailboxInbox, "sentitems": provider.MailboxSent, "drafts": provider.MailboxDraft,
	"deleteditems": provider.MailboxTrash, "junkemail": provider.MailboxSpam,
}

// fixture reads a synthetic fixture. A missing one panics, which fails the
// test loudly whether it is read from a test or from the stub's handler.
func fixture(name string) []byte {
	b, err := os.ReadFile(filepath.Join("testdata", "synthetic", name))
	if err != nil {
		panic(err)
	}
	return b
}

func (s *stub) SeedMessage(t *testing.T, m providertest.SeedMessage) providertest.Seeded {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seq++
	sm := &stubMsg{id: "AAMkMsg" + strconv.Itoa(s.seq) + "=", folder: folderIDs[m.Mailbox], subject: m.Subject, body: m.Body,
		from: m.From, to: m.To, date: m.Date, unread: m.Unread, att: m.Attachment}
	sm.conv = "AAQkConvSolo" + strconv.Itoa(s.seq) + "="
	if m.ThreadKey != "" {
		sm.conv = "AAQkConv-" + m.ThreadKey + "="
	}
	if m.Attachment != nil {
		sm.attID = "AAAtt" + strconv.Itoa(s.seq) + "="
	}
	s.msgs = append(s.msgs, sm)
	return providertest.Seeded{ID: sm.id, ThreadID: sm.conv, AttachmentID: sm.attID}
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

func (s *stub) Requests() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.requests...)
}

func gerr(w http.ResponseWriter, code int, fixtureName string, hdr map[string]string) {
	for k, v := range hdr {
		w.Header().Set(k, v)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_, _ = w.Write(fixture(fixtureName))
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func (s *stub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requests = append(s.requests, r.Method+" "+r.URL.RequestURI())
	if !strings.Contains(r.Header.Get("Prefer"), `IdType="ImmutableId"`) {
		http.Error(w, `{"error":{"code":"test","message":"missing Prefer IdType ImmutableId"}}`, http.StatusTeapot)
		return
	}
	switch s.fail {
	case providertest.FailAuth:
		gerr(w, http.StatusUnauthorized, "error_claims_challenge.json", map[string]string{
			"WWW-Authenticate": `Bearer realm="", authorization_uri="https://login.microsoftonline.com/common/oauth2/authorize", error="insufficient_claims", claims="eyJhY2Nlc3NfdG9rZW4iOnt9fQ=="`})
		return
	case providertest.FailRateLimit:
		gerr(w, http.StatusTooManyRequests, "error_throttled.json", map[string]string{"Retry-After": strconv.Itoa(int(providertest.RetryAfter / time.Second))})
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/v1.0/me/")
	segs := strings.Split(path, "/")
	if s.draftRoutes(w, r, path) {
		return
	}
	switch {
	case path == "mailFolders":
		_, _ = w.Write(fixture("mailfolders.json"))
	case len(segs) == 2 && segs[0] == "mailFolders" && aliases[segs[1]] != "":
		writeJSON(w, map[string]string{"id": folderIDs[aliases[segs[1]]]})
	case path == "messages":
		s.list(w, r, "")
	case len(segs) == 3 && segs[0] == "mailFolders" && segs[2] == "messages":
		folder := segs[1]
		if n, ok := aliases[folder]; ok {
			folder = folderIDs[n]
		}
		s.list(w, r, folder)
	case len(segs) == 2 && segs[0] == "messages":
		if m := s.find(segs[1]); m != nil {
			writeJSON(w, s.render(m, true))
			return
		}
		gerr(w, http.StatusNotFound, "error_item_not_found.json", nil)
	case len(segs) >= 4 && segs[0] == "messages" && segs[2] == "attachments":
		m := s.find(segs[1])
		if m == nil || m.att == nil || m.attID != segs[3] {
			gerr(w, http.StatusNotFound, "error_item_not_found.json", nil)
			return
		}
		if len(segs) == 5 && segs[4] == "$value" {
			w.Header().Set("Content-Type", m.att.MIMEType)
			_, _ = w.Write(m.att.Data)
			return
		}
		writeJSON(w, s.renderAtt(m))
	default:
		gerr(w, http.StatusNotFound, "error_item_not_found.json", nil)
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

func rcpts(l []provider.Address) []map[string]any {
	out := []map[string]any{}
	for _, a := range l {
		out = append(out, map[string]any{"emailAddress": map[string]string{"name": a.Name, "address": a.Email}})
	}
	return out
}

func (m *stubMsg) modifiedAt() time.Time {
	if m.modified.IsZero() {
		return m.date
	}
	return m.modified
}

func (s *stub) renderAtt(m *stubMsg) map[string]any {
	return map[string]any{"@odata.type": "#microsoft.graph.fileAttachment", "id": m.attID, "name": m.att.Filename,
		"contentType": m.att.MIMEType, "size": len(m.att.Data), "isInline": false}
}

func (s *stub) render(m *stubMsg, full bool) map[string]any {
	out := map[string]any{
		"id": m.id, "conversationId": m.conv, "parentFolderId": m.folder,
		"from":         map[string]any{"emailAddress": map[string]string{"name": m.from.Name, "address": m.from.Email}},
		"toRecipients": rcpts(m.to), "ccRecipients": []any{},
		"subject": m.subject, "receivedDateTime": m.date.UTC().Format(time.RFC3339), "sentDateTime": m.date.UTC().Format(time.RFC3339),
		"bodyPreview": m.body, "internetMessageId": "<" + m.id + "@example.com>", "isRead": !m.unread, "hasAttachments": m.att != nil || m.hasAtt,
		"isDraft": m.isDraft, "bccRecipients": rcpts(m.bcc), "lastModifiedDateTime": m.modifiedAt().UTC().Format(time.RFC3339),
	}
	if m.msgID != "" {
		out["internetMessageId"] = m.msgID
	}
	if !m.sent.IsZero() {
		out["sentDateTime"] = m.sent.UTC().Format(time.RFC3339)
	}
	if m.cc != nil {
		out["ccRecipients"] = rcpts(m.cc)
	}
	if full {
		ct := "text"
		if m.htmlBody {
			ct = "html"
		}
		out["body"] = map[string]string{"contentType": ct, "content": m.body}
		if m.att != nil {
			out["attachments"] = []any{s.renderAtt(m)}
		}
	}
	return out
}

// list answers a messages collection. It applies the filters CompileQuery and
// GetThread emit, rejects the filter and order combinations Exchange rejects,
// pages with $skip, and puts the whole original query in nextLink like Graph.
func (s *stub) list(w http.ResponseWriter, r *http.Request, folder string) {
	q := r.URL.Query()
	filter, search, orderby := q.Get("$filter"), q.Get("$search"), q.Get("$orderby")
	if search != "" && (filter != "" || orderby != "") {
		gerr(w, http.StatusBadRequest, "error_inefficient_filter.json", nil)
		return
	}
	if filter != "" && orderby != "" && !strings.HasPrefix(filter, "receivedDateTime") {
		gerr(w, http.StatusBadRequest, "error_inefficient_filter.json", nil)
		return
	}
	if strings.Contains(filter, "conversationId") && orderby != "" {
		gerr(w, http.StatusBadRequest, "error_inefficient_filter.json", nil)
		return
	}
	conv := ""
	if i := strings.Index(filter, "conversationId eq '"); i >= 0 {
		rest := filter[i+len("conversationId eq '"):]
		conv = rest[:strings.Index(rest, "'")]
	}
	unread := strings.Contains(filter, "isRead eq false")
	needle := ""
	if search != "" {
		needle = strings.ToLower(strings.ReplaceAll(strings.TrimSuffix(strings.TrimPrefix(search, `"`), `"`), `\"`, ""))
	}
	var hits []*stubMsg
	for _, m := range s.msgs {
		switch {
		case folder != "" && m.folder != folder, conv != "" && m.conv != conv, unread && !m.unread:
			continue
		case needle != "" && !strings.Contains(strings.ToLower(m.subject+" "+m.body), needle):
			continue
		}
		hits = append(hits, m)
	}
	sort.SliceStable(hits, func(i, j int) bool { return hits[i].date.After(hits[j].date) })
	top, _ := strconv.Atoi(q.Get("$top"))
	if top <= 0 {
		top = 10
	}
	skip, _ := strconv.Atoi(q.Get("$skip"))
	if skip > len(hits) {
		skip = len(hits)
	}
	end := skip + top
	resp := map[string]any{}
	if end < len(hits) {
		next := url.Values{}
		for k, v := range q {
			next[k] = v
		}
		next.Set("$skip", strconv.Itoa(end))
		resp["@odata.nextLink"] = s.base + r.URL.Path[len("/v1.0"):] + "?" + next.Encode()
	} else {
		end = len(hits)
	}
	full := strings.Contains(q.Get("$select"), "body")
	out := []map[string]any{}
	for _, m := range hits[skip:end] {
		out = append(out, s.render(m, full))
	}
	resp["value"] = out
	writeJSON(w, resp)
}

func newGraph(t *testing.T) (*graph.Client, *stub) {
	t.Helper()
	s := &stub{}
	ts := httptest.NewServer(s)
	t.Cleanup(ts.Close)
	s.base = ts.URL + "/v1.0"
	g := graphapi.New(nil)
	g.BaseURL = s.base
	return graph.New(g), s
}

func graphFactory(t *testing.T) (provider.Provider, providertest.Seeder) {
	t.Helper()
	c, s := newGraph(t)
	return c, s
}

func TestConformanceAgainstAGraphStub(t *testing.T) {
	t.Run("Read", func(t *testing.T) { providertest.RunRead(t, graphFactory) })
	t.Run("Draft", func(t *testing.T) { providertest.RunDraft(t, graphFactory) })
	t.Run("Send", func(t *testing.T) {
		providertest.RunSend(t, func(t *testing.T) (provider.Provider, providertest.Seeder) {
			c, s := newGraph(t)
			c.PollInterval = time.Millisecond
			return c, s
		})
	})
}
