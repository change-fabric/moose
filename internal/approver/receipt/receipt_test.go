package receipt

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/kms"
)

var t0 = time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)

func fixture(t *testing.T) (*LocalSigner, []byte, Receipt, Want) {
	t.Helper()
	s, err := GenerateLocalSigner()
	if err != nil {
		t.Fatal(err)
	}
	pub, err := s.PublicKeyPEM()
	if err != nil {
		t.Fatal(err)
	}
	r, err := Sign(context.Background(), s, Body{
		RequestID: "req_1", Kind: "email-send", PayloadSHA256: strings.Repeat("a", 64),
		ApprovedAt: t0, ExpiresAt: t0.Add(15 * time.Minute), RPID: "moose.example.org",
	})
	if err != nil {
		t.Fatal(err)
	}
	return s, pub, r, Want{RequestID: "req_1", Kind: "email-send", PayloadSHA256: strings.Repeat("a", 64), RPID: "moose.example.org", Now: t0.Add(time.Minute)}
}

func TestVerifyAccepts(t *testing.T) {
	_, pub, r, want := fixture(t)
	if err := Verify(pub, r, want); err != nil {
		t.Fatal(err)
	}
}

func TestVerifySurvivesJSONRoundTrip(t *testing.T) {
	_, pub, r, want := fixture(t)
	// A receipt is stored and returned as JSON; times come back in other zones.
	r.Body.ApprovedAt = r.Body.ApprovedAt.In(time.FixedZone("x", 3600))
	r.Body.ExpiresAt = r.Body.ExpiresAt.In(time.FixedZone("x", 3600))
	if err := Verify(pub, r, want); err != nil {
		t.Fatal(err)
	}
}

func TestVerifyRejectsSubstitution(t *testing.T) {
	_, pub, r, want := fixture(t)
	cases := map[string]func(w *Want){
		"another request_id":     func(w *Want) { w.RequestID = "req_2" },
		"another payload_sha256": func(w *Want) { w.PayloadSHA256 = strings.Repeat("b", 64) },
		"another kind":           func(w *Want) { w.Kind = "account-send-enable" },
		"another rp_id":          func(w *Want) { w.RPID = "evil.example.org" },
	}
	for name, mut := range cases {
		w := want
		mut(&w)
		if err := Verify(pub, r, w); !errors.Is(err, ErrMismatch) {
			t.Errorf("%s: err = %v, want ErrMismatch", name, err)
		}
	}
}

func TestVerifyRejectsExpired(t *testing.T) {
	_, pub, r, want := fixture(t)
	want.Now = r.Body.ExpiresAt
	if err := Verify(pub, r, want); !errors.Is(err, ErrExpired) {
		t.Fatalf("at expires_at: err = %v, want ErrExpired", err)
	}
	want.Now = r.Body.ExpiresAt.Add(time.Hour)
	if err := Verify(pub, r, want); !errors.Is(err, ErrExpired) {
		t.Fatalf("after expires_at: err = %v, want ErrExpired", err)
	}
}

func TestVerifyRejectsTamperedBody(t *testing.T) {
	_, pub, r, want := fixture(t)
	// A forger edits the receipt to match what the verifier wants; the signature no longer fits.
	forged := r
	forged.Body.RequestID = "req_2"
	want.RequestID = "req_2"
	if err := Verify(pub, forged, want); !errors.Is(err, ErrSignature) {
		t.Fatalf("tampered request_id: err = %v, want ErrSignature", err)
	}
	forged = r
	forged.Body.ExpiresAt = forged.Body.ExpiresAt.Add(24 * time.Hour)
	if err := Verify(pub, forged, Want{RequestID: "req_1", Kind: "email-send", PayloadSHA256: strings.Repeat("a", 64), RPID: "moose.example.org", Now: t0}); !errors.Is(err, ErrSignature) {
		t.Fatalf("extended expiry: err = %v, want ErrSignature", err)
	}
}

func TestVerifyRejectsOtherKeyAndGarbage(t *testing.T) {
	_, _, r, want := fixture(t)
	other, _ := GenerateLocalSigner()
	otherPub, _ := other.PublicKeyPEM()
	if err := Verify(otherPub, r, want); !errors.Is(err, ErrSignature) {
		t.Fatalf("other key: err = %v", err)
	}
	r2 := r
	r2.Signature = "!!!"
	_, pub, _, _ := fixture(t)
	if err := Verify(pub, r2, want); !errors.Is(err, ErrSignature) {
		t.Fatalf("garbage signature: err = %v", err)
	}
	if err := Verify([]byte("nope"), r, want); err == nil {
		t.Fatal("accepted a non-PEM key")
	}
}

func TestVerifyRejectsWrongCurveKey(t *testing.T) {
	k, _ := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	if _, err := NewLocalSigner(k); err == nil {
		t.Fatal("P-384 signer accepted")
	}
}

func TestVerifyNeedsCompleteWant(t *testing.T) {
	_, pub, r, want := fixture(t)
	want.RPID = ""
	if err := Verify(pub, r, want); err == nil {
		t.Fatal("an empty want matched")
	}
}

func TestSignRequiresFields(t *testing.T) {
	s, _ := GenerateLocalSigner()
	if _, err := Sign(context.Background(), s, Body{Kind: "x"}); err == nil {
		t.Fatal("signed an incomplete body")
	}
}

func TestPrivateKeyRoundTrip(t *testing.T) {
	s, _ := GenerateLocalSigner()
	pem, err := s.PrivateKeyPEM()
	if err != nil {
		t.Fatal(err)
	}
	back, err := ParseLocalSigner(pem)
	if err != nil {
		t.Fatal(err)
	}
	pub, _ := s.PublicKeyPEM()
	r, err := Sign(context.Background(), back, Body{RequestID: "r", Kind: "k", PayloadSHA256: "p", ApprovedAt: t0, ExpiresAt: t0.Add(time.Hour), RPID: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if err := Verify(pub, r, Want{RequestID: "r", Kind: "k", PayloadSHA256: "p", RPID: "x", Now: t0}); err != nil {
		t.Fatal(err)
	}
}

type fakeKMS struct{ signer *LocalSigner }

func (f fakeKMS) Sign(ctx context.Context, in *kms.SignInput, _ ...func(*kms.Options)) (*kms.SignOutput, error) {
	sig, err := f.signer.Sign(ctx, in.Message)
	return &kms.SignOutput{Signature: sig}, err
}

func (f fakeKMS) GetPublicKey(context.Context, *kms.GetPublicKeyInput, ...func(*kms.Options)) (*kms.GetPublicKeyOutput, error) {
	return &kms.GetPublicKeyOutput{}, nil
}

func TestKMSSignerProducesVerifiableReceipt(t *testing.T) {
	local, _ := GenerateLocalSigner()
	pub, _ := local.PublicKeyPEM()
	ks := NewKMSSigner(fakeKMS{local}, "alias/receipts")
	r, err := Sign(context.Background(), ks, Body{RequestID: "r", Kind: "k", PayloadSHA256: "p", ApprovedAt: t0, ExpiresAt: t0.Add(time.Hour), RPID: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if err := Verify(pub, r, Want{RequestID: "r", Kind: "k", PayloadSHA256: "p", RPID: "x", Now: t0}); err != nil {
		t.Fatal(err)
	}
}
