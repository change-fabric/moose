package graph_test

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/change-fabric/moose/internal/email/mailmsg"
	"github.com/change-fabric/moose/internal/email/provider"
	"github.com/change-fabric/moose/internal/email/provider/providertest"
)

// The draft and send half of the Graph stub: POST /me/messages, PATCH,
// permanentDelete and /send, with the behaviours the client must cope with.
// A sent draft keeps its id and moves to Sent Items, as the spike's
// send-correlation row assumes Exchange does (UNVERIFIED until the spike runs).

var stubClock = time.Date(2026, 3, 4, 10, 0, 0, 0, time.UTC)

func (s *stub) tick() time.Time {
	s.seq++
	return stubClock.Add(time.Duration(s.seq) * time.Second)
}

type inRecipient struct {
	EmailAddress struct {
		Name    string `json:"name"`
		Address string `json:"address"`
	} `json:"emailAddress"`
}

type inMessage struct {
	Subject string `json:"subject"`
	Body    *struct {
		ContentType string `json:"contentType"`
		Content     string `json:"content"`
	} `json:"body"`
	From          *inRecipient  `json:"from"`
	ToRecipients  []inRecipient `json:"toRecipients"`
	CcRecipients  []inRecipient `json:"ccRecipients"`
	BccRecipients []inRecipient `json:"bccRecipients"`
}

func addrsOf(in []inRecipient) []provider.Address {
	out := []provider.Address{}
	for _, r := range in {
		out = append(out, provider.Address{Name: r.EmailAddress.Name, Email: r.EmailAddress.Address})
	}
	return out
}

func (s *stub) apply(m *stubMsg, in inMessage) {
	m.subject = in.Subject
	if in.Body != nil {
		m.body = in.Body.Content
	}
	if in.From != nil {
		m.from = provider.Address{Name: in.From.EmailAddress.Name, Email: in.From.EmailAddress.Address}
	}
	m.to, m.cc, m.bcc = addrsOf(in.ToRecipients), addrsOf(in.CcRecipients), addrsOf(in.BccRecipients)
	m.modified = s.tick()
}

// draftRoutes serves the draft and send routes. It reports whether it handled
// the request. The caller holds s.mu.
func (s *stub) draftRoutes(w http.ResponseWriter, r *http.Request, path string) bool {
	segs := strings.Split(path, "/")
	switch {
	case r.Method == http.MethodPost && path == "messages":
		var in inMessage
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return true
		}
		s.seq++
		m := &stubMsg{id: "AAMkDraft" + strconv.Itoa(s.seq) + "=", conv: "AAQkConvDraft" + strconv.Itoa(s.seq) + "=",
			folder: folderIDs[provider.MailboxDraft], isDraft: true, date: s.tick(), msgID: "<draft" + strconv.Itoa(s.seq) + "@example.com>"}
		s.apply(m, in)
		s.msgs = append(s.msgs, m)
		w.WriteHeader(http.StatusCreated)
		writeJSON(w, s.render(m, true))
		return true
	case r.Method == http.MethodPatch && len(segs) == 2 && segs[0] == "messages":
		m := s.find(segs[1])
		if m == nil {
			gerr(w, http.StatusNotFound, "error_item_not_found.json", nil)
			return true
		}
		var in inMessage
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return true
		}
		s.apply(m, in)
		writeJSON(w, s.render(m, true))
		return true
	case r.Method == http.MethodPost && len(segs) == 3 && segs[0] == "messages" && segs[2] == "permanentDelete":
		for i, m := range s.msgs {
			if m.id == segs[1] {
				s.msgs = append(s.msgs[:i], s.msgs[i+1:]...)
				w.WriteHeader(http.StatusNoContent)
				return true
			}
		}
		gerr(w, http.StatusNotFound, "error_item_not_found.json", nil)
		return true
	case r.Method == http.MethodPost && len(segs) == 3 && segs[0] == "messages" && segs[2] == "send":
		m := s.find(segs[1])
		if m == nil || !m.isDraft {
			gerr(w, http.StatusNotFound, "error_item_not_found.json", nil)
			return true
		}
		if s.fail == providertest.FailSendRejected {
			gerr(w, http.StatusBadRequest, "error_invalid_recipients.json", nil)
			return true
		}
		s.deliver(m)
		if s.fail == providertest.FailSendResponseLost {
			if hj, ok := w.(http.Hijacker); ok {
				conn, _, err := hj.Hijack()
				if err == nil {
					_ = conn.Close()
					return true
				}
			}
		}
		w.WriteHeader(http.StatusAccepted)
		return true
	}
	return false
}

// deliver moves a draft to Sent Items under the same id, as Exchange does, and
// records what the recipient side holds (with the headers Exchange adds).
func (s *stub) deliver(m *stubMsg) {
	m.isDraft = false
	m.folder = folderIDs[provider.MailboxSent]
	m.sent = s.tick()
	m.modified = m.sent
	s.sendAccepts++
	body := m.body
	if s.mutateSent != nil {
		s.mutateSent(m)
		body = m.body
	}
	raw, err := mailmsg.Build(mailmsg.Message{
		From: addrStr(m.from), To: addrStrs(m.to), Cc: addrStrs(m.cc), Bcc: addrStrs(m.bcc), Subject: m.subject, Body: body,
	}, m.sent)
	if err != nil {
		panic(err)
	}
	noise := "X-MS-Exchange-Organization-AuthAs: Internal\r\nThread-Index: AdkQ3k0q0c1Vx0hQnE2m5l0Zr1rM0A==\r\n"
	s.delivered = append(s.delivered, append([]byte(noise), raw...))
}

// msg returns the stored message under the stub's lock, or nil.
func (s *stub) msg(id string) *stubMsg {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.find(id)
}

func addrStr(a provider.Address) string {
	if a.Name == "" {
		return a.Email
	}
	return a.Name + " <" + a.Email + ">"
}

func addrStrs(l []provider.Address) []string {
	var out []string
	for _, a := range l {
		out = append(out, addrStr(a))
	}
	return out
}
