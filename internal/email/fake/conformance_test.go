package fake_test

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/change-fabric/moose/internal/email/fake"
	"github.com/change-fabric/moose/internal/email/provider"
	"github.com/change-fabric/moose/internal/email/provider/providertest"
)

// seeder arranges a fake.Provider for the conformance suites.
type seeder struct {
	p    *fake.Provider
	atts int
}

func (s *seeder) SeedMessage(t *testing.T, m providertest.SeedMessage) providertest.Seeded {
	t.Helper()
	msg := provider.Message{
		Mailboxes: []string{m.Mailbox}, From: m.From, To: m.To, Subject: m.Subject, BodyText: m.Body, Date: m.Date, Unread: m.Unread,
	}
	if m.ThreadKey != "" {
		msg.ThreadID = "thread-" + m.ThreadKey
	}
	added := s.p.AddMessage(msg)
	out := providertest.Seeded{ID: added.ID, ThreadID: added.ThreadID}
	if a := m.Attachment; a != nil {
		s.atts++
		out.AttachmentID = "att" + strconv.Itoa(s.atts)
		s.p.AddAttachment(added.ID, provider.AttachmentMeta{ID: out.AttachmentID, Filename: a.Filename, MIMEType: a.MIMEType}, a.Data)
	}
	return out
}

func (s *seeder) Fail(t *testing.T, f providertest.Failure) {
	t.Helper()
	s.p.Err, s.p.SendErr, s.p.LoseSendResponse = nil, nil, false
	switch f {
	case providertest.FailAuth:
		s.p.Err = fmt.Errorf("%w: fake credential revoked", provider.ErrAuth)
	case providertest.FailRateLimit:
		s.p.Err = &provider.RateLimitError{RetryAfter: providertest.RetryAfter}
	case providertest.FailSendRejected:
		s.p.SendErr = fmt.Errorf("%w: fake refused the recipient", provider.ErrRejected)
	case providertest.FailSendResponseLost:
		s.p.LoseSendResponse = true
	case providertest.FailNone:
	default:
		t.Fatalf("unknown failure %d", f)
	}
}

func (s *seeder) Delivered(t *testing.T) [][]byte {
	t.Helper()
	var out [][]byte
	for _, m := range s.p.SentMessages() {
		out = append(out, m.Raw)
	}
	return out
}

// factory returns the fake in Gmail-like (byte-exact) or Exchange-like mode.
func factory(exchange bool) providertest.Factory {
	return func(t *testing.T) (provider.Provider, providertest.Seeder) {
		p := fake.New()
		if exchange {
			p.ExchangeLikeMIME = true
			p.SetCapabilities(provider.Capabilities{Threads: true, SearchDialect: provider.DialectGraph, NativeDraftSend: true})
		}
		return p, &seeder{p: p}
	}
}

func TestConformanceGmailLike(t *testing.T) {
	f := factory(false)
	t.Run("Read", func(t *testing.T) { providertest.RunRead(t, f) })
	t.Run("Draft", func(t *testing.T) { providertest.RunDraft(t, f) })
	t.Run("Send", func(t *testing.T) { providertest.RunSend(t, f) })
}

func TestConformanceExchangeLikeMIME(t *testing.T) {
	f := factory(true)
	t.Run("Read", func(t *testing.T) { providertest.RunRead(t, f) })
	t.Run("Draft", func(t *testing.T) { providertest.RunDraft(t, f) })
	t.Run("Send", func(t *testing.T) { providertest.RunSend(t, f) })
}

func TestExchangeLikeMIMEAddsNoiseTheSenderNeverWrote(t *testing.T) {
	ctx := context.Background()
	p := fake.New()
	p.ExchangeLikeMIME = true
	raw := []byte("From: me@example.com\r\nTo: you@example.com\r\nSubject: s\r\n\r\nbody\r\n")
	d, err := p.CreateDraft(ctx, provider.DraftInput{Raw: raw})
	if err != nil {
		t.Fatal(err)
	}
	got, err := p.GetDraftRaw(ctx, d.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range []string{"X-MS-Has-Attach:", "Thread-Index:", "X-Unsent: 1"} {
		if !strings.Contains(string(got), h) {
			t.Errorf("draft lacks the %s noise header:\n%s", h, got)
		}
	}
	if !strings.HasSuffix(string(got), string(raw)) {
		t.Errorf("the sender's own message must follow the noise unchanged:\n%s", got)
	}
}
