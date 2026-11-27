package providertest

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/change-fabric/moose/internal/email/mailmsg"
	"github.com/change-fabric/moose/internal/email/provider"
)

// approvedMessage builds the standard message the draft and send suites use.
func approvedMessage(t *testing.T, subject, body string) []byte {
	t.Helper()
	raw, err := mailmsg.Build(mailmsg.Message{
		From: "Me <me@example.com>", To: []string{"you@example.com"}, Cc: []string{"cc@example.com"}, Bcc: []string{"bcc@example.com"},
		Subject: subject, Body: body,
	}, time.Date(2026, 3, 4, 10, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func mustFields(t *testing.T, raw []byte) Fields {
	t.Helper()
	f, err := ParseFields(raw)
	if err != nil {
		t.Fatalf("unparseable message: %v\n%s", err, raw)
	}
	return f
}

// RunDraft runs the draft conformance suite: create, update keeps the id,
// snapshot determinism and delete.
//
// What GetDraftRaw returns is the approval snapshot, so it must read the same
// every time. A provider without Capabilities.NativeDraftSend (Gmail) must
// return exactly the stored bytes; one with it (Graph) may add headers or
// re-encode, but the From, recipients, subject and decoded body must match.
func RunDraft(t *testing.T, factory Factory) {
	ctx := context.Background()

	t.Run("CreateStoresTheApprovedMessage", func(t *testing.T) {
		p, _ := factory(t)
		raw := approvedMessage(t, "Quarterly plan", "Line one\n\nLine two\n")
		d, err := p.CreateDraft(ctx, provider.DraftInput{Raw: raw})
		if err != nil {
			t.Fatal(err)
		}
		if d.ID == "" {
			t.Fatal("CreateDraft returned an empty id")
		}
		got, err := p.GetDraftRaw(ctx, d.ID)
		if err != nil {
			t.Fatal(err)
		}
		if diff := SameFields(mustFields(t, raw), mustFields(t, got)); diff != "" {
			t.Errorf("stored draft differs from the approved message: %s", diff)
		}
		if !p.Capabilities().NativeDraftSend && !bytes.Equal(got, raw) {
			t.Errorf("a provider without NativeDraftSend must return the exact bytes\nwant %q\n got %q", raw, got)
		}
	})

	t.Run("UpdateKeepsTheID", func(t *testing.T) {
		p, _ := factory(t)
		d, err := p.CreateDraft(ctx, provider.DraftInput{Raw: approvedMessage(t, "First", "one")})
		if err != nil {
			t.Fatal(err)
		}
		next := approvedMessage(t, "Second subject", "second body")
		up, err := p.UpdateDraft(ctx, d.ID, provider.DraftInput{Raw: next})
		if err != nil {
			t.Fatal(err)
		}
		if up.ID != d.ID {
			t.Fatalf("UpdateDraft changed the draft id from %q to %q; email_update_draft relies on stable ids", d.ID, up.ID)
		}
		got, err := p.GetDraftRaw(ctx, d.ID)
		if err != nil {
			t.Fatal(err)
		}
		if diff := SameFields(mustFields(t, next), mustFields(t, got)); diff != "" {
			t.Errorf("updated draft differs: %s", diff)
		}
		if _, err := p.UpdateDraft(ctx, "no-such-draft", provider.DraftInput{Raw: next}); !errors.Is(err, provider.ErrNotFound) {
			t.Errorf("update of a missing draft err = %v, want ErrNotFound", err)
		}
	})

	t.Run("SnapshotIsDeterministic", func(t *testing.T) {
		p, _ := factory(t)
		d, err := p.CreateDraft(ctx, provider.DraftInput{Raw: approvedMessage(t, "Stable", "stable body")})
		if err != nil {
			t.Fatal(err)
		}
		first, err := p.GetDraftRaw(ctx, d.ID)
		if err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 3; i++ {
			again, err := p.GetDraftRaw(ctx, d.ID)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(first, again) {
				t.Fatalf("read %d of an unchanged draft differs from the first:\n%q\n%q", i+2, first, again)
			}
		}
		// The same holds after an update: two reads of the new content agree.
		if _, err := p.UpdateDraft(ctx, d.ID, provider.DraftInput{Raw: approvedMessage(t, "Stable 2", "changed")}); err != nil {
			t.Fatal(err)
		}
		a, _ := p.GetDraftRaw(ctx, d.ID)
		b, _ := p.GetDraftRaw(ctx, d.ID)
		if len(a) == 0 || !bytes.Equal(a, b) {
			t.Fatalf("reads after an update differ:\n%q\n%q", a, b)
		}
		if bytes.Equal(first, a) {
			t.Fatal("the update did not change the stored draft")
		}
	})

	t.Run("DeleteRemovesTheDraft", func(t *testing.T) {
		p, _ := factory(t)
		d, err := p.CreateDraft(ctx, provider.DraftInput{Raw: approvedMessage(t, "Doomed", "bye")})
		if err != nil {
			t.Fatal(err)
		}
		if err := p.DeleteDraft(ctx, d.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := p.GetDraftRaw(ctx, d.ID); !errors.Is(err, provider.ErrNotFound) {
			t.Errorf("read after delete err = %v, want ErrNotFound", err)
		}
		if err := p.DeleteDraft(ctx, d.ID); !errors.Is(err, provider.ErrNotFound) {
			t.Errorf("second delete err = %v, want ErrNotFound", err)
		}
		if _, err := p.GetDraftRaw(ctx, "never-existed"); !errors.Is(err, provider.ErrNotFound) {
			t.Errorf("read of a missing draft err = %v, want ErrNotFound", err)
		}
	})
}
