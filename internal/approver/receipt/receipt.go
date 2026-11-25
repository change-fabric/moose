// Package receipt signs and verifies approval receipts.
//
// An approved request gets a receipt: {v, request_id, kind, payload_sha256,
// approved_at, expires_at, rp_id} signed with an ECDSA P-256 key (KMS
// ECDSA_SHA_256 in production). moose-email holds only the public key and
// verifies offline. Verify checks the signature, rp_id, kind and expiry, and
// that request_id and payload_sha256 equal the values moose-email stored when
// it created the request, so a receipt for one request or payload can never
// authorise another.
package receipt

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/kms"
	kmstypes "github.com/aws/aws-sdk-go-v2/service/kms/types"
)

// Version is the receipt format version.
const Version = 1

// domain separates receipt signatures from any other use of the key.
const domain = "moose-approval-receipt-v1\n"

// Body is the signed content.
type Body struct {
	V             int       `json:"v"`
	RequestID     string    `json:"request_id"`
	Kind          string    `json:"kind"`
	PayloadSHA256 string    `json:"payload_sha256"`
	ApprovedAt    time.Time `json:"approved_at"`
	ExpiresAt     time.Time `json:"expires_at"`
	RPID          string    `json:"rp_id"`
}

// message is the exact byte string that is signed: the domain tag followed by
// the body as JSON with times as second-precision UTC RFC 3339, so the bytes do
// not depend on how a time round-tripped.
func (b Body) message() []byte {
	type wire struct {
		V             int    `json:"v"`
		RequestID     string `json:"request_id"`
		Kind          string `json:"kind"`
		PayloadSHA256 string `json:"payload_sha256"`
		ApprovedAt    string `json:"approved_at"`
		ExpiresAt     string `json:"expires_at"`
		RPID          string `json:"rp_id"`
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(wire{b.V, b.RequestID, b.Kind, b.PayloadSHA256,
		b.ApprovedAt.UTC().Format(time.RFC3339), b.ExpiresAt.UTC().Format(time.RFC3339), b.RPID})
	return append([]byte(domain), bytes.TrimRight(buf.Bytes(), "\n")...)
}

// Receipt is a signed Body. Signature is the base64 ASN.1 DER ECDSA signature.
type Receipt struct {
	Body      Body   `json:"receipt"`
	Signature string `json:"signature"`
}

// Signer signs a message with ECDSA P-256 over SHA-256 and returns an ASN.1 DER signature.
type Signer interface {
	Sign(ctx context.Context, message []byte) ([]byte, error)
}

// Sign signs body. Times are truncated to whole seconds first.
func Sign(ctx context.Context, s Signer, body Body) (Receipt, error) {
	body.V = Version
	body.ApprovedAt = body.ApprovedAt.UTC().Truncate(time.Second)
	body.ExpiresAt = body.ExpiresAt.UTC().Truncate(time.Second)
	if body.RequestID == "" || body.Kind == "" || body.PayloadSHA256 == "" || body.RPID == "" {
		return Receipt{}, errors.New("receipt: request_id, kind, payload_sha256 and rp_id are required")
	}
	sig, err := s.Sign(ctx, body.message())
	if err != nil {
		return Receipt{}, fmt.Errorf("receipt: sign: %w", err)
	}
	return Receipt{Body: body, Signature: base64.StdEncoding.EncodeToString(sig)}, nil
}

// Want is what the verifier expects, taken from its own stored request and not
// from the receipt.
type Want struct {
	RequestID     string
	Kind          string
	PayloadSHA256 string
	RPID          string
	Now           time.Time
}

// Verification failures. Callers compare with errors.Is.
var (
	ErrSignature = errors.New("receipt: bad signature")
	ErrMismatch  = errors.New("receipt: does not match the request")
	ErrExpired   = errors.New("receipt: expired")
)

// Verify checks r against the pinned public key (PEM, PKIX) and want. It
// rejects a bad signature, a different request_id, kind, payload_sha256 or
// rp_id, an unknown version, and a receipt whose expires_at is not after
// want.Now.
func Verify(pubkeyPEM []byte, r Receipt, want Want) error {
	pub, err := ParsePublicKey(pubkeyPEM)
	if err != nil {
		return err
	}
	sig, err := base64.StdEncoding.DecodeString(r.Signature)
	if err != nil {
		return fmt.Errorf("%w: undecodable", ErrSignature)
	}
	digest := sha256.Sum256(r.Body.message())
	if !ecdsa.VerifyASN1(pub, digest[:], sig) {
		return ErrSignature
	}
	if want.RequestID == "" || want.Kind == "" || want.PayloadSHA256 == "" || want.RPID == "" || want.Now.IsZero() {
		return errors.New("receipt: want needs request_id, kind, payload_sha256, rp_id and now")
	}
	b := r.Body
	switch {
	case b.V != Version:
		return fmt.Errorf("%w: version %d", ErrMismatch, b.V)
	case b.RequestID != want.RequestID:
		return fmt.Errorf("%w: request_id", ErrMismatch)
	case b.Kind != want.Kind:
		return fmt.Errorf("%w: kind", ErrMismatch)
	case b.PayloadSHA256 != want.PayloadSHA256:
		return fmt.Errorf("%w: payload_sha256", ErrMismatch)
	case b.RPID != want.RPID:
		return fmt.Errorf("%w: rp_id", ErrMismatch)
	case !want.Now.Before(b.ExpiresAt):
		return ErrExpired
	}
	return nil
}

// ParsePublicKey reads a PKIX PEM P-256 public key.
func ParsePublicKey(pemBytes []byte) (*ecdsa.PublicKey, error) {
	blk, _ := pem.Decode(pemBytes)
	if blk == nil {
		return nil, errors.New("receipt: public key is not PEM")
	}
	k, err := x509.ParsePKIXPublicKey(blk.Bytes)
	if err != nil {
		return nil, fmt.Errorf("receipt: public key: %w", err)
	}
	pub, ok := k.(*ecdsa.PublicKey)
	if !ok || pub.Curve != elliptic.P256() {
		return nil, errors.New("receipt: public key is not ECDSA P-256")
	}
	return pub, nil
}

// PublicKeyPEM encodes a PKIX DER public key (as KMS returns it) as PEM.
func PublicKeyPEM(der []byte) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})
}

// ---- local signer (dev and tests) ----

// LocalSigner signs with an in-process key.
type LocalSigner struct{ key *ecdsa.PrivateKey }

// NewLocalSigner wraps key, which must be P-256.
func NewLocalSigner(key *ecdsa.PrivateKey) (*LocalSigner, error) {
	if key == nil || key.Curve != elliptic.P256() {
		return nil, errors.New("receipt: signing key must be ECDSA P-256")
	}
	return &LocalSigner{key: key}, nil
}

// GenerateLocalSigner creates a fresh P-256 key.
func GenerateLocalSigner() (*LocalSigner, error) {
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	return &LocalSigner{key: k}, nil
}

// ParseLocalSigner reads a PEM EC private key (SEC1 or PKCS8).
func ParseLocalSigner(pemBytes []byte) (*LocalSigner, error) {
	blk, _ := pem.Decode(pemBytes)
	if blk == nil {
		return nil, errors.New("receipt: signing key is not PEM")
	}
	if k, err := x509.ParseECPrivateKey(blk.Bytes); err == nil {
		return NewLocalSigner(k)
	}
	k, err := x509.ParsePKCS8PrivateKey(blk.Bytes)
	if err != nil {
		return nil, fmt.Errorf("receipt: signing key: %w", err)
	}
	ek, ok := k.(*ecdsa.PrivateKey)
	if !ok {
		return nil, errors.New("receipt: signing key is not ECDSA")
	}
	return NewLocalSigner(ek)
}

// Sign implements Signer.
func (l *LocalSigner) Sign(_ context.Context, message []byte) ([]byte, error) {
	d := sha256.Sum256(message)
	return ecdsa.SignASN1(rand.Reader, l.key, d[:])
}

// PublicKeyPEM returns the matching public key.
func (l *LocalSigner) PublicKeyPEM() ([]byte, error) {
	der, err := x509.MarshalPKIXPublicKey(&l.key.PublicKey)
	if err != nil {
		return nil, err
	}
	return PublicKeyPEM(der), nil
}

// PrivateKeyPEM returns the key as PKCS8 PEM, for dev key files.
func (l *LocalSigner) PrivateKeyPEM() ([]byte, error) {
	der, err := x509.MarshalPKCS8PrivateKey(l.key)
	if err != nil {
		return nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), nil
}

// ---- KMS signer ----

// KMSAPI is the subset of the KMS client the signer uses.
type KMSAPI interface {
	Sign(context.Context, *kms.SignInput, ...func(*kms.Options)) (*kms.SignOutput, error)
	GetPublicKey(context.Context, *kms.GetPublicKeyInput, ...func(*kms.Options)) (*kms.GetPublicKeyOutput, error)
}

// KMSSigner signs with an asymmetric ECC_NIST_P256 KMS key. Only the approver's
// role holds kms:Sign on it.
type KMSSigner struct {
	api   KMSAPI
	keyID string
}

// NewKMSSigner returns a signer for keyID.
func NewKMSSigner(api KMSAPI, keyID string) *KMSSigner { return &KMSSigner{api: api, keyID: keyID} }

// Sign implements Signer. KMS hashes the raw message with SHA-256.
func (k *KMSSigner) Sign(ctx context.Context, message []byte) ([]byte, error) {
	out, err := k.api.Sign(ctx, &kms.SignInput{
		KeyId: aws.String(k.keyID), Message: message,
		MessageType: kmstypes.MessageTypeRaw, SigningAlgorithm: kmstypes.SigningAlgorithmSpecEcdsaSha256,
	})
	if err != nil {
		return nil, err
	}
	return out.Signature, nil
}

// PublicKeyPEM fetches the key's public half, for pinning in moose-email's config.
func (k *KMSSigner) PublicKeyPEM(ctx context.Context) ([]byte, error) {
	out, err := k.api.GetPublicKey(ctx, &kms.GetPublicKeyInput{KeyId: aws.String(k.keyID)})
	if err != nil {
		return nil, err
	}
	return PublicKeyPEM(out.PublicKey), nil
}
