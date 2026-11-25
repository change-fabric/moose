package webauthn_test

import (
	"context"
	"crypto/rand"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-webauthn/webauthn/protocol"

	"github.com/change-fabric/moose/internal/approver/store"
	"github.com/change-fabric/moose/internal/approver/webauthn"
	"github.com/change-fabric/moose/internal/approver/webauthn/webauthntest"
	"github.com/change-fabric/moose/internal/email/envelope"
	"github.com/change-fabric/moose/internal/email/store/sqlite"
)

const (
	rpID   = "moose.example.org"
	origin = "https://moose.example.org"
)

var t0 = time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)

type rig struct {
	t   *testing.T
	st  *store.Store
	svc *webauthn.Service
	now time.Time
	a   *webauthntest.Authenticator
}

func newRig(t *testing.T) *rig {
	t.Helper()
	b, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "a.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = b.Close() })
	key := make([]byte, 32)
	_, _ = rand.Read(key)
	seal, _ := envelope.NewLocal(key)
	r := &rig{t: t, now: t0, a: webauthntest.New(rpID, origin)}
	r.st = store.NewWithClock(b, seal, func() time.Time { return r.now })
	r.svc, err = webauthn.New(webauthn.Config{RPID: rpID, Origin: origin}, r.st, func() time.Time { return r.now })
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func (r *rig) enroll() {
	r.t.Helper()
	ctx := context.Background()
	tok := store.NewToken()
	if err := r.st.CreateEnrollToken(ctx, store.EnrollToken{Hash: store.Hash(tok), Owner: "pat", Expires: r.now.Add(store.EnrollTTL)}); err != nil {
		r.t.Fatal(err)
	}
	creation, err := r.svc.BeginRegistration(ctx, tok)
	if err != nil {
		r.t.Fatal(err)
	}
	if got := creation.Response.AuthenticatorSelection.UserVerification; got != protocol.VerificationRequired {
		r.t.Fatalf("registration userVerification = %q", got)
	}
	if _, err := r.svc.FinishRegistration(ctx, tok, r.a.Register(creation)); err != nil {
		r.t.Fatal(err)
	}
}

func bind(id string) webauthn.Binding {
	return webauthn.Binding{Purpose: webauthn.PurposeApprove, RequestID: id, PayloadSHA256: "sha-" + id}
}

func accept(store.Challenge) error { return nil }

func TestAssertionRoundTrip(t *testing.T) {
	r := newRig(t)
	r.enroll()
	ctx := context.Background()
	ca, err := r.svc.BeginAssertion(ctx, "pat", bind("apr_1"))
	if err != nil {
		t.Fatal(err)
	}
	if ca.Response.UserVerification != protocol.VerificationRequired {
		t.Fatalf("assertion userVerification = %q, want required", ca.Response.UserVerification)
	}
	if len(ca.Response.AllowedCredentials) != 1 {
		t.Fatalf("%d allowed credentials", len(ca.Response.AllowedCredentials))
	}
	c, err := r.svc.FinishAssertion(ctx, "pat", r.a.Assert(ca), accept)
	if err != nil {
		t.Fatal(err)
	}
	if c.RequestID != "apr_1" || c.PayloadSHA256 != "sha-apr_1" || c.Purpose != webauthn.PurposeApprove {
		t.Fatalf("returned binding: %+v", c)
	}
}

func TestAssertionNeedsAnEnrolledPasskey(t *testing.T) {
	r := newRig(t)
	if _, err := r.svc.BeginAssertion(context.Background(), "pat", bind("apr_1")); !errors.Is(err, webauthn.ErrNoCredential) {
		t.Fatalf("no passkey: %v", err)
	}
	if ok, _ := r.svc.HasCredentials(context.Background(), "pat"); ok {
		t.Fatal("HasCredentials with none enrolled")
	}
}

func TestMissingUserVerificationFlagIsRejected(t *testing.T) {
	r := newRig(t)
	r.enroll()
	ctx := context.Background()
	ca, _ := r.svc.BeginAssertion(ctx, "pat", bind("apr_1"))
	r.a.UserVerified = false
	if _, err := r.svc.FinishAssertion(ctx, "pat", r.a.Assert(ca), accept); !errors.Is(err, webauthn.ErrUserVerification) {
		t.Fatalf("an assertion with UV off: %v, want ErrUserVerification", err)
	}
	// The challenge was consumed, so retrying the same assertion cannot get past it either.
	r.a.UserVerified = true
	if _, err := r.svc.FinishAssertion(ctx, "pat", r.a.Assert(ca), accept); !errors.Is(err, webauthn.ErrChallenge) {
		t.Fatalf("reusing the burned challenge: %v", err)
	}
}

func TestRegistrationWithoutUserVerificationIsRejected(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	tok := store.NewToken()
	_ = r.st.CreateEnrollToken(ctx, store.EnrollToken{Hash: store.Hash(tok), Owner: "pat", Expires: r.now.Add(store.EnrollTTL)})
	creation, _ := r.svc.BeginRegistration(ctx, tok)
	r.a.UserVerified = false
	if _, err := r.svc.FinishRegistration(ctx, tok, r.a.Register(creation)); !errors.Is(err, webauthn.ErrUserVerification) {
		t.Fatalf("a passkey created without UV: %v", err)
	}
	if creds, _ := r.st.ListCredentials(ctx, "pat"); len(creds) != 0 {
		t.Fatal("a passkey without user verification was stored")
	}
}

func TestChallengeIsSingleUse(t *testing.T) {
	r := newRig(t)
	r.enroll()
	ctx := context.Background()
	ca, _ := r.svc.BeginAssertion(ctx, "pat", bind("apr_1"))
	body := r.a.Assert(ca)
	if _, err := r.svc.FinishAssertion(ctx, "pat", body, accept); err != nil {
		t.Fatal(err)
	}
	if _, err := r.svc.FinishAssertion(ctx, "pat", body, accept); !errors.Is(err, webauthn.ErrChallenge) {
		t.Fatalf("replay: %v, want ErrChallenge", err)
	}
}

func TestChallengeExpires(t *testing.T) {
	r := newRig(t)
	r.enroll()
	ctx := context.Background()
	ca, _ := r.svc.BeginAssertion(ctx, "pat", bind("apr_1"))
	body := r.a.Assert(ca)
	r.now = r.now.Add(store.ChallengeTTL)
	if _, err := r.svc.FinishAssertion(ctx, "pat", body, accept); !errors.Is(err, webauthn.ErrChallenge) {
		t.Fatalf("an expired challenge: %v", err)
	}
}

func TestChallengeBindingIsCheckedAgainstTheRequest(t *testing.T) {
	r := newRig(t)
	r.enroll()
	ctx := context.Background()
	ca, _ := r.svc.BeginAssertion(ctx, "pat", bind("apr_A"))
	body := r.a.Assert(ca)
	// The caller is deciding apr_B: its check refuses A's binding.
	_, err := r.svc.FinishAssertion(ctx, "pat", body, func(c store.Challenge) error {
		if c.RequestID != "apr_B" {
			return errors.New("issued for another request")
		}
		return nil
	})
	if !errors.Is(err, webauthn.ErrBinding) {
		t.Fatalf("A's challenge on B: %v, want ErrBinding", err)
	}
	// The mismatch burned it.
	if _, err := r.svc.FinishAssertion(ctx, "pat", body, accept); !errors.Is(err, webauthn.ErrChallenge) {
		t.Fatalf("after the mismatch: %v", err)
	}
	// The payload digest is part of the binding too.
	ca, _ = r.svc.BeginAssertion(ctx, "pat", bind("apr_A"))
	_, err = r.svc.FinishAssertion(ctx, "pat", r.a.Assert(ca), func(c store.Challenge) error {
		if c.PayloadSHA256 != "another-digest" {
			return errors.New("issued for another payload")
		}
		return nil
	})
	if !errors.Is(err, webauthn.ErrBinding) {
		t.Fatalf("another payload digest: %v", err)
	}
}

func TestAssertionFromAnotherAuthenticatorIsRejected(t *testing.T) {
	r := newRig(t)
	r.enroll()
	ctx := context.Background()
	ca, _ := r.svc.BeginAssertion(ctx, "pat", bind("apr_1"))
	stranger := webauthntest.New(rpID, origin)
	if _, err := r.svc.FinishAssertion(ctx, "pat", stranger.Assert(ca), accept); err == nil {
		t.Fatal("an assertion from an unenrolled authenticator was accepted")
	}
}

func TestAssertionForAnotherOriginIsRejected(t *testing.T) {
	r := newRig(t)
	r.enroll()
	ctx := context.Background()
	ca, _ := r.svc.BeginAssertion(ctx, "pat", bind("apr_1"))
	r.a.Origin = "https://evil.example"
	if _, err := r.svc.FinishAssertion(ctx, "pat", r.a.Assert(ca), accept); err == nil {
		t.Fatal("an assertion made on another origin was accepted")
	}
}

func TestAssertionOwnerMustMatchTheChallenge(t *testing.T) {
	r := newRig(t)
	r.enroll()
	ctx := context.Background()
	ca, _ := r.svc.BeginAssertion(ctx, "pat", bind("apr_1"))
	if _, err := r.svc.FinishAssertion(ctx, "someone-else", r.a.Assert(ca), accept); !errors.Is(err, webauthn.ErrBinding) {
		t.Fatalf("another owner: %v", err)
	}
}

func TestEnrollmentTokenIsSingleUseAndExpires(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	tok := store.NewToken()
	_ = r.st.CreateEnrollToken(ctx, store.EnrollToken{Hash: store.Hash(tok), Owner: "pat", Expires: r.now.Add(store.EnrollTTL)})
	c1, err := r.svc.BeginRegistration(ctx, tok)
	if err != nil {
		t.Fatal(err)
	}
	// A second ceremony may begin before the first finishes, but only one can finish.
	c2, err := r.svc.BeginRegistration(ctx, tok)
	if err != nil {
		t.Fatal(err)
	}
	second := webauthntest.New(rpID, origin)
	if _, err := r.svc.FinishRegistration(ctx, tok, r.a.Register(c1)); err != nil {
		t.Fatal(err)
	}
	if _, err := r.svc.FinishRegistration(ctx, tok, second.Register(c2)); !errors.Is(err, webauthn.ErrEnrollToken) {
		t.Fatalf("second registration with the same token: %v, want ErrEnrollToken", err)
	}
	if _, err := r.svc.BeginRegistration(ctx, tok); !errors.Is(err, webauthn.ErrEnrollToken) {
		t.Fatalf("begin with a used token: %v", err)
	}
	if creds, _ := r.st.ListCredentials(ctx, "pat"); len(creds) != 1 {
		t.Fatalf("%d passkeys, want 1", len(creds))
	}

	tok2 := store.NewToken()
	_ = r.st.CreateEnrollToken(ctx, store.EnrollToken{Hash: store.Hash(tok2), Owner: "pat", Expires: r.now.Add(store.EnrollTTL)})
	r.now = r.now.Add(store.EnrollTTL)
	if _, err := r.svc.BeginRegistration(ctx, tok2); !errors.Is(err, webauthn.ErrEnrollToken) {
		t.Fatalf("begin with an expired token: %v", err)
	}
	if _, err := r.svc.BeginRegistration(ctx, "not-a-token"); !errors.Is(err, webauthn.ErrEnrollToken) {
		t.Fatalf("begin with an unknown token: %v", err)
	}
}

func TestRegistrationChallengeIsBoundToItsToken(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	tokA, tokB := store.NewToken(), store.NewToken()
	for _, tk := range []string{tokA, tokB} {
		_ = r.st.CreateEnrollToken(ctx, store.EnrollToken{Hash: store.Hash(tk), Owner: "pat", Expires: r.now.Add(store.EnrollTTL)})
	}
	cA, _ := r.svc.BeginRegistration(ctx, tokA)
	if _, err := r.svc.FinishRegistration(ctx, tokB, r.a.Register(cA)); !errors.Is(err, webauthn.ErrBinding) {
		t.Fatalf("A's registration challenge under B's token: %v, want ErrBinding", err)
	}
}

func TestAssertionChallengeCannotRegister(t *testing.T) {
	r := newRig(t)
	r.enroll()
	ctx := context.Background()
	tok := store.NewToken()
	_ = r.st.CreateEnrollToken(ctx, store.EnrollToken{Hash: store.Hash(tok), Owner: "pat", Expires: r.now.Add(store.EnrollTTL)})
	ca, _ := r.svc.BeginAssertion(ctx, "pat", bind("apr_1"))
	// Hand the assertion challenge to the registration flow.
	other := webauthntest.New(rpID, origin)
	fake := &protocol.CredentialCreation{Response: protocol.PublicKeyCredentialCreationOptions{Challenge: ca.Response.Challenge}}
	if _, err := r.svc.FinishRegistration(ctx, tok, other.Register(fake)); err == nil {
		t.Fatal("an assertion challenge was accepted for registration")
	}
}
