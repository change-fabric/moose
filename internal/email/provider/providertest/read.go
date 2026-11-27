package providertest

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/change-fabric/moose/internal/email/provider"
)

// RunRead runs the read-side conformance suite.
func RunRead(t *testing.T, factory Factory) {
	ctx := context.Background()

	t.Run("ListMailboxesUsesNeutralIDs", func(t *testing.T) {
		p, _ := factory(t)
		boxes, err := p.ListMailboxes(ctx)
		if err != nil {
			t.Fatal(err)
		}
		byID := map[string]provider.Mailbox{}
		for _, b := range boxes {
			byID[b.ID] = b
		}
		for _, id := range []string{provider.MailboxInbox, provider.MailboxSent} {
			b, ok := byID[id]
			if !ok || !b.System {
				t.Errorf("mailbox %q missing or not a system mailbox in %+v", id, boxes)
			}
		}
	})

	t.Run("SearchPagesNewestFirstWithoutRepeats", func(t *testing.T) {
		p, s := factory(t)
		var want []string
		for i := 0; i < 5; i++ {
			m := s.SeedMessage(t, SeedMessage{Mailbox: provider.MailboxInbox, From: addr("bob@example.com"), To: []provider.Address{addr("me@example.com")},
				Subject: "message " + string(rune('A'+i)), Body: "body", Date: epoch.Add(time.Duration(i) * time.Hour)})
			want = append([]string{m.ID}, want...) // newest first
		}
		var got []string
		cursor := ""
		for page := 0; page < 10; page++ {
			res, err := p.Search(ctx, provider.Query{Mailbox: provider.MailboxInbox, Limit: 2, Cursor: cursor})
			if err != nil {
				t.Fatalf("page %d: %v", page, err)
			}
			if len(res.Messages) > 2 {
				t.Fatalf("page %d has %d messages, limit was 2", page, len(res.Messages))
			}
			for _, m := range res.Messages {
				got = append(got, m.ID)
			}
			if res.NextCursor == "" {
				break
			}
			cursor = res.NextCursor
		}
		if len(got) != len(want) {
			t.Fatalf("paged %v, want %v", got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("paged %v, want newest first %v", got, want)
			}
		}
	})

	t.Run("SearchFiltersByMailboxAndUnread", func(t *testing.T) {
		p, s := factory(t)
		in := s.SeedMessage(t, SeedMessage{Mailbox: provider.MailboxInbox, From: addr("bob@example.com"), To: []provider.Address{addr("me@example.com")},
			Subject: "inbox read", Body: "x", Date: epoch})
		unread := s.SeedMessage(t, SeedMessage{Mailbox: provider.MailboxInbox, From: addr("bob@example.com"), To: []provider.Address{addr("me@example.com")},
			Subject: "inbox unread", Body: "x", Date: epoch.Add(time.Hour), Unread: true})
		sent := s.SeedMessage(t, SeedMessage{Mailbox: provider.MailboxSent, From: addr("me@example.com"), To: []provider.Address{addr("bob@example.com")},
			Subject: "sent", Body: "x", Date: epoch.Add(2 * time.Hour)})
		ids := func(q provider.Query) map[string]bool {
			t.Helper()
			res, err := p.Search(ctx, q)
			if err != nil {
				t.Fatal(err)
			}
			out := map[string]bool{}
			for _, m := range res.Messages {
				out[m.ID] = true
			}
			return out
		}
		if got := ids(provider.Query{Mailbox: provider.MailboxSent}); len(got) != 1 || !got[sent.ID] {
			t.Errorf("SENT search = %v, want only %s", got, sent.ID)
		}
		if got := ids(provider.Query{Mailbox: provider.MailboxInbox}); len(got) != 2 || !got[in.ID] || !got[unread.ID] {
			t.Errorf("INBOX search = %v", got)
		}
		if got := ids(provider.Query{Mailbox: provider.MailboxInbox, Unread: true}); len(got) != 1 || !got[unread.ID] {
			t.Errorf("unread INBOX search = %v, want only %s", got, unread.ID)
		}
	})

	t.Run("GetMessage", func(t *testing.T) {
		p, s := factory(t)
		seeded := s.SeedMessage(t, SeedMessage{Mailbox: provider.MailboxInbox, ThreadKey: "k",
			From: provider.Address{Name: "Bob", Email: "bob@example.com"}, To: []provider.Address{addr("me@example.com"), addr("al@example.com")},
			Subject: "hello there", Body: "the body text", Date: epoch, Unread: true})
		m, err := p.GetMessage(ctx, seeded.ID)
		if err != nil {
			t.Fatal(err)
		}
		if m.ID != seeded.ID || m.From.Email != "bob@example.com" || len(m.To) != 2 || m.Subject != "hello there" || !m.Unread {
			t.Errorf("message = %+v", m)
		}
		if normalise(m.BodyText) != "the body text" {
			t.Errorf("body = %q", m.BodyText)
		}
		if m.ThreadID != seeded.ThreadID || m.ThreadID == "" {
			t.Errorf("thread id = %q, want %q", m.ThreadID, seeded.ThreadID)
		}
		found := false
		for _, b := range m.Mailboxes {
			found = found || b == provider.MailboxInbox
		}
		if !found {
			t.Errorf("mailboxes = %v, want to include the neutral id %s", m.Mailboxes, provider.MailboxInbox)
		}
		if _, err := p.GetMessage(ctx, "does-not-exist"); !errors.Is(err, provider.ErrNotFound) {
			t.Errorf("missing message err = %v, want ErrNotFound", err)
		}
	})

	t.Run("GetThreadOldestFirst", func(t *testing.T) {
		p, s := factory(t)
		a := s.SeedMessage(t, SeedMessage{Mailbox: provider.MailboxInbox, ThreadKey: "conv", From: addr("bob@example.com"), To: []provider.Address{addr("me@example.com")},
			Subject: "first", Body: "one", Date: epoch})
		b := s.SeedMessage(t, SeedMessage{Mailbox: provider.MailboxSent, ThreadKey: "conv", From: addr("me@example.com"), To: []provider.Address{addr("bob@example.com")},
			Subject: "Re: first", Body: "two", Date: epoch.Add(time.Hour)})
		s.SeedMessage(t, SeedMessage{Mailbox: provider.MailboxInbox, ThreadKey: "other", From: addr("x@example.com"), To: []provider.Address{addr("me@example.com")},
			Subject: "unrelated", Body: "three", Date: epoch.Add(2 * time.Hour)})
		if a.ThreadID != b.ThreadID {
			t.Fatalf("seeder gave one conversation two thread ids: %q vs %q", a.ThreadID, b.ThreadID)
		}
		th, err := p.GetThread(ctx, a.ThreadID)
		if err != nil {
			t.Fatal(err)
		}
		if len(th.Messages) != 2 || th.Messages[0].ID != a.ID || th.Messages[1].ID != b.ID {
			t.Fatalf("thread = %+v, want [%s %s]", th.Messages, a.ID, b.ID)
		}
		if _, err := p.GetThread(ctx, "no-such-thread"); !errors.Is(err, provider.ErrNotFound) {
			t.Errorf("missing thread err = %v, want ErrNotFound", err)
		}
	})

	t.Run("OpenAttachment", func(t *testing.T) {
		p, s := factory(t)
		data := []byte("%PDF-1.4 pretend attachment bytes")
		seeded := s.SeedMessage(t, SeedMessage{Mailbox: provider.MailboxInbox, From: addr("bob@example.com"), To: []provider.Address{addr("me@example.com")},
			Subject: "with file", Body: "see attached", Date: epoch,
			Attachment: &SeedAttachment{Filename: "plan.pdf", MIMEType: "application/pdf", Data: data}})
		m, err := p.GetMessage(ctx, seeded.ID)
		if err != nil {
			t.Fatal(err)
		}
		if len(m.Attachments) != 1 || m.Attachments[0].Filename != "plan.pdf" || m.Attachments[0].ID == "" {
			t.Fatalf("attachments = %+v", m.Attachments)
		}
		rc, meta, err := p.OpenAttachment(ctx, seeded.ID, m.Attachments[0].ID)
		if err != nil {
			t.Fatal(err)
		}
		defer rc.Close()
		got, _ := io.ReadAll(rc)
		if string(got) != string(data) || meta.Filename != "plan.pdf" {
			t.Errorf("attachment = %q %+v", got, meta)
		}
		if _, _, err := p.OpenAttachment(ctx, seeded.ID, "no-such-attachment"); !errors.Is(err, provider.ErrNotFound) {
			t.Errorf("missing attachment err = %v, want ErrNotFound", err)
		}
	})

	t.Run("RateLimitSurfacesAsRateLimitError", func(t *testing.T) {
		p, s := factory(t)
		seeded := s.SeedMessage(t, SeedMessage{Mailbox: provider.MailboxInbox, From: addr("bob@example.com"), To: []provider.Address{addr("me@example.com")},
			Subject: "x", Body: "x", Date: epoch})
		s.Fail(t, FailRateLimit)
		calls := map[string]error{
			"ListMailboxes": func() error { _, err := p.ListMailboxes(ctx); return err }(),
			"Search":        func() error { _, err := p.Search(ctx, provider.Query{Mailbox: provider.MailboxInbox}); return err }(),
			"GetMessage":    func() error { _, err := p.GetMessage(ctx, seeded.ID); return err }(),
		}
		for name, err := range calls {
			var rl *provider.RateLimitError
			if !errors.Is(err, provider.ErrRateLimited) || !errors.As(err, &rl) {
				t.Errorf("%s: err = %v, want a *RateLimitError wrapping ErrRateLimited", name, err)
				continue
			}
			if rl.RetryAfter != RetryAfter {
				t.Errorf("%s: RetryAfter = %v, want the provider's %v", name, rl.RetryAfter, RetryAfter)
			}
		}
	})

	t.Run("AuthFailureSurfacesAsErrAuth", func(t *testing.T) {
		p, s := factory(t)
		s.Fail(t, FailAuth)
		if _, err := p.ListMailboxes(ctx); !errors.Is(err, provider.ErrAuth) {
			t.Errorf("ListMailboxes err = %v, want ErrAuth", err)
		}
		if _, err := p.Search(ctx, provider.Query{}); !errors.Is(err, provider.ErrAuth) {
			t.Errorf("Search err = %v, want ErrAuth", err)
		}
		if _, err := p.GetMessage(ctx, "m1"); !errors.Is(err, provider.ErrAuth) {
			t.Errorf("GetMessage err = %v, want ErrAuth", err)
		}
	})
}
