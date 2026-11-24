package envelope_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"maps"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/kms"

	"github.com/change-fabric/moose/internal/email/envelope"
)

// fakeKMS behaves like KMS for the two calls the sealer makes: it wraps data
// keys under an encryption context and refuses to unwrap under a different one.
type fakeKMS struct {
	keyID     string
	generated int
	lastCtx   map[string]string
	wrapped   map[string]fakeEntry
}

type fakeEntry struct {
	dek []byte
	ec  map[string]string
}

func newFakeKMS(keyID string) *fakeKMS {
	return &fakeKMS{keyID: keyID, wrapped: map[string]fakeEntry{}}
}

func (f *fakeKMS) GenerateDataKey(_ context.Context, in *kms.GenerateDataKeyInput, _ ...func(*kms.Options)) (*kms.GenerateDataKeyOutput, error) {
	if *in.KeyId != f.keyID {
		return nil, errors.New("fake kms: wrong key id")
	}
	f.generated++
	f.lastCtx = in.EncryptionContext
	dek := make([]byte, 32)
	_, _ = rand.Read(dek)
	blob := make([]byte, 16)
	_, _ = rand.Read(blob)
	f.wrapped[string(blob)] = fakeEntry{dek: dek, ec: maps.Clone(in.EncryptionContext)}
	return &kms.GenerateDataKeyOutput{Plaintext: bytes.Clone(dek), CiphertextBlob: blob}, nil
}

func (f *fakeKMS) Decrypt(_ context.Context, in *kms.DecryptInput, _ ...func(*kms.Options)) (*kms.DecryptOutput, error) {
	e, ok := f.wrapped[string(in.CiphertextBlob)]
	if !ok {
		return nil, errors.New("fake kms: invalid ciphertext")
	}
	if !maps.Equal(e.ec, in.EncryptionContext) {
		return nil, errors.New("fake kms: InvalidCiphertextException, encryption context mismatch")
	}
	return &kms.DecryptOutput{Plaintext: bytes.Clone(e.dek)}, nil
}

var ec = envelope.Context{Owner: "pat", AccountID: "acc_aaaa"}

func sealers(t *testing.T) map[string]envelope.Sealer {
	t.Helper()
	key := bytes.Repeat([]byte{7}, 32)
	local, err := envelope.NewLocal(key)
	if err != nil {
		t.Fatal(err)
	}
	return map[string]envelope.Sealer{"kms": envelope.NewKMS(newFakeKMS("key-1"), "key-1"), "local": local}
}

func TestRoundTripAndNoPlaintextInBlob(t *testing.T) {
	ctx := context.Background()
	secret := []byte("1//refresh-token-value")
	for name, s := range sealers(t) {
		t.Run(name, func(t *testing.T) {
			blob, err := s.Seal(ctx, secret, ec)
			if err != nil {
				t.Fatal(err)
			}
			if bytes.Contains(blob, secret) {
				t.Fatal("plaintext appears in the sealed blob")
			}
			got, err := s.Open(ctx, blob, ec)
			if err != nil || !bytes.Equal(got, secret) {
				t.Fatalf("Open = %q, %v", got, err)
			}
			again, _ := s.Seal(ctx, secret, ec)
			if bytes.Equal(blob, again) {
				t.Fatal("two seals of the same plaintext are identical: nonce or data key reused")
			}
		})
	}
}

func TestOpenRefusesAnotherOwnerOrAccount(t *testing.T) {
	ctx := context.Background()
	for name, s := range sealers(t) {
		t.Run(name, func(t *testing.T) {
			blob, err := s.Seal(ctx, []byte("secret"), ec)
			if err != nil {
				t.Fatal(err)
			}
			for _, other := range []envelope.Context{
				{Owner: "conner", AccountID: ec.AccountID},
				{Owner: ec.Owner, AccountID: "acc_bbbb"},
			} {
				if got, err := s.Open(ctx, blob, other); err == nil {
					t.Fatalf("blob opened under %+v: %q", other, got)
				}
			}
		})
	}
}

func TestContextIsRequired(t *testing.T) {
	ctx := context.Background()
	for name, s := range sealers(t) {
		t.Run(name, func(t *testing.T) {
			for _, bad := range []envelope.Context{{}, {Owner: "pat"}, {AccountID: "acc_aaaa"}} {
				if _, err := s.Seal(ctx, []byte("x"), bad); err == nil {
					t.Errorf("Seal accepted context %+v", bad)
				}
				if _, err := s.Open(ctx, []byte("x"), bad); err == nil {
					t.Errorf("Open accepted context %+v", bad)
				}
			}
		})
	}
}

func TestTamperedAndTruncatedBlobsFail(t *testing.T) {
	ctx := context.Background()
	for name, s := range sealers(t) {
		t.Run(name, func(t *testing.T) {
			blob, _ := s.Seal(ctx, []byte("secret"), ec)
			flipped := bytes.Clone(blob)
			flipped[len(flipped)-1] ^= 1
			if _, err := s.Open(ctx, flipped, ec); err == nil {
				t.Error("tampered ciphertext opened")
			}
			for _, n := range []int{0, 1, 2, 5, len(blob) / 2} {
				if _, err := s.Open(ctx, blob[:n], ec); err == nil {
					t.Errorf("blob truncated to %d bytes opened", n)
				}
			}
		})
	}
}

func TestModesDoNotCross(t *testing.T) {
	ctx := context.Background()
	s := sealers(t)
	kmsBlob, _ := s["kms"].Seal(ctx, []byte("x"), ec)
	localBlob, _ := s["local"].Seal(ctx, []byte("x"), ec)
	if _, err := s["local"].Open(ctx, kmsBlob, ec); !errors.Is(err, envelope.ErrFormat) {
		t.Errorf("local opened a kms blob: %v", err)
	}
	if _, err := s["kms"].Open(ctx, localBlob, ec); !errors.Is(err, envelope.ErrFormat) {
		t.Errorf("kms opened a local blob: %v", err)
	}
}

func TestKMSReceivesOwnerAndAccountContextAndFreshKeys(t *testing.T) {
	ctx := context.Background()
	f := newFakeKMS("key-1")
	s := envelope.NewKMS(f, "key-1")
	for range 3 {
		if _, err := s.Seal(ctx, []byte("x"), ec); err != nil {
			t.Fatal(err)
		}
	}
	if f.generated != 3 {
		t.Fatalf("GenerateDataKey called %d times for 3 seals, want one data key per blob", f.generated)
	}
	want := map[string]string{"owner": "pat", "account_id": "acc_aaaa"}
	if !maps.Equal(f.lastCtx, want) {
		t.Fatalf("encryption context = %v, want exactly %v", f.lastCtx, want)
	}
	wrongKey := envelope.NewKMS(f, "other-key")
	if _, err := wrongKey.Seal(ctx, []byte("x"), ec); err == nil {
		t.Fatal("sealing under an unknown key id succeeded")
	}
}

func TestNewLocalRejectsBadKey(t *testing.T) {
	if _, err := envelope.NewLocal([]byte("short")); err == nil {
		t.Fatal("short dev key accepted")
	}
}
