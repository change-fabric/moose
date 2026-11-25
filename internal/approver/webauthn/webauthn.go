// Package webauthn runs the passkey ceremonies for moose-approver on top of
// github.com/go-webauthn/webauthn.
//
// The rules it enforces beyond the library's own checks:
//   - userVerification is required, and the UV flag is checked server-side on
//     every registration and assertion;
//   - a challenge is single use. It is stored with the (request_id,
//     payload_sha256) it was issued for, and consumed with a conditional delete
//     before the assertion is looked at, so a replay and a challenge presented
//     for another request both find nothing usable;
//   - the first passkey is enrolled only through a hashed, expiring,
//     single-use enrollment token.
package webauthn

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/go-webauthn/webauthn/protocol"
	gw "github.com/go-webauthn/webauthn/webauthn"

	"github.com/change-fabric/moose/internal/approver/store"
)

// Ceremony purposes stored with a challenge.
const (
	PurposeApprove    = "approve"
	PurposeDeny       = "deny"
	PurposeRegister   = "register"
	PurposeQueueLogin = "queue-login"
)

// Errors compared with errors.Is.
var (
	ErrNoCredential     = errors.New("webauthn: no passkey is enrolled")
	ErrChallenge        = errors.New("webauthn: challenge is unknown, used or expired")
	ErrBinding          = errors.New("webauthn: challenge was issued for another request")
	ErrUserVerification = errors.New("webauthn: user verification flag is not set")
	ErrCloned           = errors.New("webauthn: authenticator may be cloned")
	ErrEnrollToken      = errors.New("webauthn: enrollment token is unknown, used or expired")
)

// Config names the relying party. RPID is the registrable domain the passkeys
// are scoped to and Origin the exact origin the pages are served from.
type Config struct {
	RPID   string
	Name   string
	Origin string
}

// Service runs ceremonies against a store.
type Service struct {
	wa  *gw.WebAuthn
	st  *store.Store
	now func() time.Time
}

// New builds a Service.
func New(cfg Config, st *store.Store, now func() time.Time) (*Service, error) {
	if now == nil {
		now = time.Now
	}
	name := cfg.Name
	if name == "" {
		name = "moose approver"
	}
	wa, err := gw.New(&gw.Config{
		RPID: cfg.RPID, RPDisplayName: name, RPOrigins: []string{cfg.Origin},
		AttestationPreference: protocol.PreferNoAttestation,
		AuthenticatorSelection: protocol.AuthenticatorSelection{
			ResidentKey:      protocol.ResidentKeyRequirementPreferred,
			UserVerification: protocol.VerificationRequired,
		},
	})
	if err != nil {
		return nil, fmt.Errorf("webauthn: %w", err)
	}
	return &Service{wa: wa, st: st, now: now}, nil
}

// user adapts an owner and their stored credentials to the library.
type user struct {
	owner string
	creds []gw.Credential
}

func (u user) WebAuthnID() []byte {
	h := sha256.Sum256([]byte(u.owner))
	return h[:]
}
func (u user) WebAuthnName() string                 { return u.owner }
func (u user) WebAuthnDisplayName() string          { return u.owner }
func (u user) WebAuthnCredentials() []gw.Credential { return u.creds }

func (s *Service) user(ctx context.Context, owner string) (user, error) {
	stored, err := s.st.ListCredentials(ctx, owner)
	if err != nil {
		return user{}, err
	}
	u := user{owner: owner}
	for _, c := range stored {
		var cred gw.Credential
		if err := json.Unmarshal(c.Data, &cred); err != nil {
			return user{}, fmt.Errorf("webauthn: stored credential %s: %w", c.ID, err)
		}
		u.creds = append(u.creds, cred)
	}
	return u, nil
}

// HasCredentials reports whether owner has any passkey.
func (s *Service) HasCredentials(ctx context.Context, owner string) (bool, error) {
	u, err := s.user(ctx, owner)
	return len(u.creds) > 0, err
}

// Binding is what an assertion challenge is issued for.
type Binding struct {
	Purpose       string
	RequestID     string
	PayloadSHA256 string
	// Drops lists batch items excluded by this decision. They are fixed at
	// begin time, so the signed payload hash covers the final list.
	Drops []string
}

// BeginAssertion issues an assertion challenge for owner bound to b.
func (s *Service) BeginAssertion(ctx context.Context, owner string, b Binding) (*protocol.CredentialAssertion, error) {
	u, err := s.user(ctx, owner)
	if err != nil {
		return nil, err
	}
	if len(u.creds) == 0 {
		return nil, ErrNoCredential
	}
	assertion, sd, err := s.wa.BeginLogin(u, gw.WithUserVerification(protocol.VerificationRequired))
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(sd)
	if err != nil {
		return nil, err
	}
	err = s.st.PutChallenge(ctx, store.Challenge{
		Hash: store.Hash(sd.Challenge), Purpose: b.Purpose, Owner: owner, RequestID: b.RequestID,
		PayloadSHA256: b.PayloadSHA256, Drops: b.Drops, Session: raw, Expires: s.now().Add(store.ChallengeTTL),
	})
	if err != nil {
		return nil, err
	}
	return assertion, nil
}

// FinishAssertion verifies an assertion body (the PublicKeyCredential JSON the
// browser produced). It consumes the challenge first, then calls check with the
// stored binding so the caller can compare it with the request being decided,
// then verifies the signature, the UV flag and the sign counter. It returns the
// consumed challenge.
func (s *Service) FinishAssertion(ctx context.Context, owner string, body []byte, check func(store.Challenge) error) (store.Challenge, error) {
	parsed, err := protocol.ParseCredentialRequestResponseBytes(body)
	if err != nil {
		return store.Challenge{}, fmt.Errorf("webauthn: malformed assertion: %w", err)
	}
	c, err := s.st.TakeChallenge(ctx, store.Hash(parsed.Response.CollectedClientData.Challenge), s.now())
	if errors.Is(err, store.ErrNotFound) {
		return store.Challenge{}, ErrChallenge
	}
	if err != nil {
		return store.Challenge{}, err
	}
	if c.Owner != owner {
		return c, ErrBinding
	}
	if err := check(c); err != nil {
		return c, fmt.Errorf("%w: %v", ErrBinding, err)
	}
	if !parsed.Response.AuthenticatorData.Flags.HasUserVerified() {
		return c, ErrUserVerification
	}
	var sd gw.SessionData
	if err := json.Unmarshal(c.Session, &sd); err != nil {
		return c, err
	}
	u, err := s.user(ctx, owner)
	if err != nil {
		return c, err
	}
	cred, err := s.wa.ValidateLogin(u, sd, parsed)
	if err != nil {
		return c, fmt.Errorf("webauthn: assertion rejected: %w", err)
	}
	if cred.Authenticator.CloneWarning {
		return c, ErrCloned
	}
	data, err := json.Marshal(cred)
	if err != nil {
		return c, err
	}
	// The sign counter is advisory for passkeys; losing a race to another update is fine.
	if err := s.st.UpdateCredential(ctx, owner, CredentialID(cred.ID), data); err != nil && !errors.Is(err, store.ErrConflict) {
		return c, err
	}
	return c, nil
}

// CredentialID is the storage form of a credential id.
func CredentialID(id []byte) string { return base64.RawURLEncoding.EncodeToString(id) }

// BeginRegistration starts enrolling a passkey with an enrollment token. The
// token is only checked here, and consumed when the registration finishes.
func (s *Service) BeginRegistration(ctx context.Context, token string) (*protocol.CredentialCreation, error) {
	hash := store.Hash(token)
	t, err := s.st.PeekEnrollToken(ctx, hash, s.now())
	if errors.Is(err, store.ErrNotFound) {
		return nil, ErrEnrollToken
	}
	if err != nil {
		return nil, err
	}
	u, err := s.user(ctx, t.Owner)
	if err != nil {
		return nil, err
	}
	var exclude []protocol.CredentialDescriptor
	for _, c := range u.creds {
		exclude = append(exclude, c.Descriptor())
	}
	creation, sd, err := s.wa.BeginRegistration(u, gw.WithExclusions(exclude),
		gw.WithAuthenticatorSelection(protocol.AuthenticatorSelection{
			ResidentKey:      protocol.ResidentKeyRequirementPreferred,
			UserVerification: protocol.VerificationRequired,
		}))
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(sd)
	if err != nil {
		return nil, err
	}
	err = s.st.PutChallenge(ctx, store.Challenge{
		Hash: store.Hash(sd.Challenge), Purpose: PurposeRegister, Owner: t.Owner, RequestID: "enroll",
		PayloadSHA256: hash, Session: raw, Expires: s.now().Add(store.ChallengeTTL),
	})
	if err != nil {
		return nil, err
	}
	return creation, nil
}

// FinishRegistration verifies the attestation body and, in one transaction,
// consumes the enrollment token and stores the passkey. It returns the owner.
func (s *Service) FinishRegistration(ctx context.Context, token string, body []byte) (string, error) {
	hash := store.Hash(token)
	parsed, err := protocol.ParseCredentialCreationResponseBytes(body)
	if err != nil {
		return "", fmt.Errorf("webauthn: malformed attestation: %w", err)
	}
	c, err := s.st.TakeChallenge(ctx, store.Hash(parsed.Response.CollectedClientData.Challenge), s.now())
	if errors.Is(err, store.ErrNotFound) {
		return "", ErrChallenge
	}
	if err != nil {
		return "", err
	}
	if c.Purpose != PurposeRegister || c.PayloadSHA256 != hash {
		return "", ErrBinding
	}
	if !parsed.Response.AttestationObject.AuthData.Flags.HasUserVerified() {
		return "", ErrUserVerification
	}
	var sd gw.SessionData
	if err := json.Unmarshal(c.Session, &sd); err != nil {
		return "", err
	}
	u, err := s.user(ctx, c.Owner)
	if err != nil {
		return "", err
	}
	cred, err := s.wa.CreateCredential(u, sd, parsed)
	if err != nil {
		return "", fmt.Errorf("webauthn: attestation rejected: %w", err)
	}
	data, err := json.Marshal(cred)
	if err != nil {
		return "", err
	}
	err = s.st.RedeemEnrollToken(ctx, hash, s.now(), store.Credential{
		Owner: c.Owner, ID: CredentialID(cred.ID), Data: data, Created: s.now().UTC(),
	})
	if errors.Is(err, store.ErrNotFound) {
		return "", ErrEnrollToken
	}
	if err != nil {
		return "", err
	}
	return c.Owner, nil
}
