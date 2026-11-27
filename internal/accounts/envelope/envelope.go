// Package envelope seals small secrets (Google refresh tokens, approved RFC822
// snapshots) with envelope encryption: a fresh AES-256 data key per blob,
// AES-GCM for the payload, and the data key wrapped by AWS KMS under an
// encryption context of {owner, account_id}. KMS therefore refuses to unwrap a
// blob for any other owner or account, and so does the local sealer.
//
// Blob layout: mode(1) | wrappedKeyLen(2, big endian) | wrappedKey | nonce(12) | ciphertext.
package envelope

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/kms"
	kmstypes "github.com/aws/aws-sdk-go-v2/service/kms/types"
)

// Context is the encryption context every blob is bound to.
type Context struct {
	Owner     string
	AccountID string
}

func (c Context) valid() error {
	if c.Owner == "" || c.AccountID == "" {
		return errors.New("envelope: owner and account_id are required in the encryption context")
	}
	return nil
}

func (c Context) kms() map[string]string {
	return map[string]string{"owner": c.Owner, "account_id": c.AccountID}
}

// aad is the context in a fixed, unambiguous encoding, bound into AES-GCM so a
// blob also fails to open under a different context in local mode.
func (c Context) aad() []byte {
	var b []byte
	for _, f := range []string{"account_id", c.AccountID, "owner", c.Owner} {
		b = binary.BigEndian.AppendUint32(b, uint32(len(f)))
		b = append(b, f...)
	}
	return b
}

// Sealer seals and opens blobs under an encryption context.
type Sealer interface {
	Seal(ctx context.Context, plaintext []byte, ec Context) ([]byte, error)
	Open(ctx context.Context, blob []byte, ec Context) ([]byte, error)
}

const (
	modeKMS   byte = 1
	modeLocal byte = 2
	dekSize        = 32
)

// ErrFormat means the blob is not a sealed blob of the expected mode.
var ErrFormat = errors.New("envelope: malformed or wrong-mode blob")

// wrapper recovers a data key from its wrapped form.
type wrapper interface {
	mode() byte
	unwrap(ctx context.Context, wrapped []byte, ec Context) ([]byte, error)
}

type sealer struct {
	w wrapper
	// generate returns a fresh data key and its wrapped form. KMS does both in
	// one call; local mode generates then wraps.
	generate func(ctx context.Context, ec Context) (dek, wrapped []byte, err error)
}

func (s *sealer) Seal(ctx context.Context, plaintext []byte, ec Context) ([]byte, error) {
	if err := ec.valid(); err != nil {
		return nil, err
	}
	dek, wrapped, err := s.generate(ctx, ec)
	if err != nil {
		return nil, err
	}
	defer zero(dek)
	if len(wrapped) > 0xffff {
		return nil, errors.New("envelope: wrapped key too large")
	}
	gcm, err := newGCM(dek)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, err
	}
	out := []byte{s.w.mode()}
	out = binary.BigEndian.AppendUint16(out, uint16(len(wrapped)))
	out = append(out, wrapped...)
	out = append(out, nonce...)
	return gcm.Seal(out, nonce, plaintext, ec.aad()), nil
}

func (s *sealer) Open(ctx context.Context, blob []byte, ec Context) ([]byte, error) {
	if err := ec.valid(); err != nil {
		return nil, err
	}
	if len(blob) < 3 || blob[0] != s.w.mode() {
		return nil, ErrFormat
	}
	n := int(binary.BigEndian.Uint16(blob[1:3]))
	rest := blob[3:]
	if len(rest) < n+12 {
		return nil, ErrFormat
	}
	wrapped, rest := rest[:n], rest[n:]
	dek, err := s.w.unwrap(ctx, wrapped, ec)
	if err != nil {
		return nil, err
	}
	defer zero(dek)
	gcm, err := newGCM(dek)
	if err != nil {
		return nil, err
	}
	nonce, ct := rest[:gcm.NonceSize()], rest[gcm.NonceSize():]
	pt, err := gcm.Open(nil, nonce, ct, ec.aad())
	if err != nil {
		return nil, fmt.Errorf("envelope: open: %w", err)
	}
	return pt, nil
}

func newGCM(key []byte) (cipher.AEAD, error) {
	blk, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(blk)
}

func zero(b []byte) {
	for i := range b {
		b[i] = 0
	}
}

// ---- KMS ----

// KMSAPI is the subset of the KMS client the sealer uses.
type KMSAPI interface {
	GenerateDataKey(context.Context, *kms.GenerateDataKeyInput, ...func(*kms.Options)) (*kms.GenerateDataKeyOutput, error)
	Decrypt(context.Context, *kms.DecryptInput, ...func(*kms.Options)) (*kms.DecryptOutput, error)
}

type kmsWrapper struct {
	api   KMSAPI
	keyID string
}

func (kmsWrapper) mode() byte { return modeKMS }

func (k kmsWrapper) unwrap(ctx context.Context, wrapped []byte, ec Context) ([]byte, error) {
	out, err := k.api.Decrypt(ctx, &kms.DecryptInput{
		CiphertextBlob:    wrapped,
		EncryptionContext: ec.kms(),
		KeyId:             aws.String(k.keyID),
	})
	if err != nil {
		return nil, fmt.Errorf("envelope: kms decrypt: %w", err)
	}
	return out.Plaintext, nil
}

// NewKMS returns a Sealer that wraps data keys with the KMS key keyID. The key
// must be a symmetric key; moose grants the Lambda role GenerateDataKey and
// Decrypt on it only.
func NewKMS(api KMSAPI, keyID string) Sealer {
	w := kmsWrapper{api: api, keyID: keyID}
	return &sealer{w: w, generate: func(ctx context.Context, ec Context) ([]byte, []byte, error) {
		out, err := api.GenerateDataKey(ctx, &kms.GenerateDataKeyInput{
			KeyId:             aws.String(keyID),
			KeySpec:           kmstypes.DataKeySpecAes256,
			EncryptionContext: ec.kms(),
		})
		if err != nil {
			return nil, nil, fmt.Errorf("envelope: kms generate data key: %w", err)
		}
		return out.Plaintext, out.CiphertextBlob, nil
	}}
}

// ---- local ----

type localWrapper struct{ aead cipher.AEAD }

func (localWrapper) mode() byte { return modeLocal }

func (l localWrapper) wrap(_ context.Context, dek []byte, ec Context) ([]byte, error) {
	nonce := make([]byte, l.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, err
	}
	return l.aead.Seal(nonce, nonce, dek, ec.aad()), nil
}

func (l localWrapper) unwrap(_ context.Context, wrapped []byte, ec Context) ([]byte, error) {
	ns := l.aead.NonceSize()
	if len(wrapped) < ns {
		return nil, ErrFormat
	}
	dek, err := l.aead.Open(nil, wrapped[:ns], wrapped[ns:], ec.aad())
	if err != nil {
		return nil, fmt.Errorf("envelope: unwrap: %w", err)
	}
	return dek, nil
}

// NewLocal returns a Sealer for local development that wraps data keys with a
// 32-byte dev key from config instead of KMS. Blobs it produces cannot be
// opened by the KMS sealer, and the other way round.
func NewLocal(devKey []byte) (Sealer, error) {
	if len(devKey) != dekSize {
		return nil, fmt.Errorf("envelope: dev key must be %d bytes, got %d", dekSize, len(devKey))
	}
	aead, err := newGCM(devKey)
	if err != nil {
		return nil, err
	}
	w := localWrapper{aead: aead}
	return &sealer{w: w, generate: func(ctx context.Context, ec Context) ([]byte, []byte, error) {
		dek := make([]byte, dekSize)
		if _, err := io.ReadFull(rand.Reader, dek); err != nil {
			return nil, nil, err
		}
		wrapped, err := w.wrap(ctx, dek, ec)
		if err != nil {
			zero(dek)
			return nil, nil, err
		}
		return dek, wrapped, nil
	}}, nil
}
