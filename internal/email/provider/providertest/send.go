package providertest

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/change-fabric/moose/internal/email/provider"
)

// RunSend runs the send conformance suite: send returns an id, the delivered
// message verifies against the approved one, a definite refusal is reported as
// one, and a send whose response was lost is never reported as a definite
// failure and is never repeated.
//
// The delivered message must match the approved fields on every provider. On a
// provider without Capabilities.NativeDraftSend it must also be byte-exact.
func RunSend(t *testing.T, factory Factory) {
	ctx := context.Background()

	t.Run("SendReturnsAnIDAndTheDeliveredCopyVerifies", func(t *testing.T) {
		p, s := factory(t)
		raw := approvedMessage(t, "Approved subject", "Approved body.\n\nSecond paragraph.\n")
		sent, err := p.SendDraft(ctx, raw, "")
		if err != nil {
			t.Fatal(err)
		}
		if sent.MessageID == "" {
			t.Fatal("SendDraft returned no message id")
		}
		delivered := s.Delivered(t)
		if len(delivered) != 1 {
			t.Fatalf("delivered %d messages, want 1", len(delivered))
		}
		// Verify: the delivered copy equals the approved parse.
		if diff := SameFields(mustFields(t, raw), mustFields(t, delivered[0])); diff != "" {
			t.Errorf("delivered message does not match the approved one: %s", diff)
		}
		if !p.Capabilities().NativeDraftSend && !bytes.Equal(delivered[0], raw) {
			t.Errorf("a provider without NativeDraftSend must deliver exactly the approved bytes\nwant %q\n got %q", raw, delivered[0])
		}
	})

	t.Run("DefiniteRefusalDeliversNothing", func(t *testing.T) {
		p, s := factory(t)
		s.Fail(t, FailSendRejected)
		_, err := p.SendDraft(ctx, approvedMessage(t, "x", "y"), "")
		if !errors.Is(err, provider.ErrRejected) {
			t.Fatalf("err = %v, want ErrRejected", err)
		}
		if n := len(s.Delivered(t)); n != 0 {
			t.Fatalf("a refused send delivered %d messages", n)
		}
	})

	t.Run("LostResponseIsNeverADefiniteFailureAndIsNotRepeated", func(t *testing.T) {
		p, s := factory(t)
		s.Fail(t, FailSendResponseLost)
		_, err := p.SendDraft(ctx, approvedMessage(t, "x", "y"), "")
		if err == nil {
			t.Fatal("a lost response must surface as an error")
		}
		for name, definite := range map[string]error{
			"ErrRejected": provider.ErrRejected, "ErrAuth": provider.ErrAuth, "ErrNotFound": provider.ErrNotFound,
			"ErrInvalidQuery": provider.ErrInvalidQuery, "ErrRateLimited": provider.ErrRateLimited,
		} {
			if errors.Is(err, definite) {
				t.Errorf("err %v is classified as %s, but the message was delivered: the caller must treat it as unknown", err, name)
			}
		}
		if n := len(s.Delivered(t)); n != 1 {
			t.Fatalf("delivered %d copies, want exactly 1: a provider must never resend on its own", n)
		}
	})

	t.Run("AuthFailureOnSend", func(t *testing.T) {
		p, s := factory(t)
		s.Fail(t, FailAuth)
		if _, err := p.SendDraft(ctx, approvedMessage(t, "x", "y"), ""); !errors.Is(err, provider.ErrAuth) {
			t.Fatalf("err = %v, want ErrAuth", err)
		}
		if n := len(s.Delivered(t)); n != 0 {
			t.Fatalf("delivered %d messages", n)
		}
	})

	t.Run("RateLimitOnSendCarriesRetryAfter", func(t *testing.T) {
		p, s := factory(t)
		s.Fail(t, FailRateLimit)
		_, err := p.SendDraft(ctx, approvedMessage(t, "x", "y"), "")
		var rl *provider.RateLimitError
		if !errors.Is(err, provider.ErrRateLimited) || !errors.As(err, &rl) || rl.RetryAfter != RetryAfter {
			t.Fatalf("err = %v, want RateLimitError with RetryAfter %v", err, RetryAfter)
		}
		if n := len(s.Delivered(t)); n != 0 {
			t.Fatalf("delivered %d messages", n)
		}
	})
}
