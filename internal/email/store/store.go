// Package store is moose-email's persistence layer: accounts, OAuth state,
// approvals, the send ledger, per-account contacts and audit metadata.
//
// All business rules live here, once, on top of the small Backend interface in
// backend.go. The dynamo and sqlite packages implement Backend only, and the
// storetest suite runs against both, so every conditional write (code redeem,
// refresh rotation, send claim, cap reservation) behaves the same on each.
//
// Secrets are never stored in the clear: codes, sessions and refresh tokens are
// keyed by the hex sha256 of the value (HashSecret), and an account's Google
// refresh token is held as an envelope-sealed blob.
package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/change-fabric/moose/internal/email/provider"
)

// Errors the Store returns. Callers compare with errors.Is.
var (
	ErrNotFound          = errors.New("emailstore: not found")
	ErrConflict          = errors.New("emailstore: conflict")
	ErrNicknameTaken     = errors.New("emailstore: nickname already used by another account")
	ErrExpired           = errors.New("emailstore: expired")
	ErrAlreadyRedeemed   = errors.New("emailstore: already redeemed")
	ErrAlreadyClaimed    = errors.New("emailstore: send already claimed")
	ErrRateLimited       = errors.New("emailstore: hourly cap reached")
	ErrInvalidNickname   = errors.New("emailstore: nickname must be 1 to 24 characters with no control characters")
	ErrInvalidTransition = errors.New("emailstore: state does not allow this transition")
)

// Retention windows. Dynamo deletes lazily (up to 48h after the ttl), so
// readers still check the explicit Expires fields.
const (
	AuditTTL           = 90 * 24 * time.Hour
	ApprovalTTL        = 24 * time.Hour
	LedgerTTL          = 90 * 24 * time.Hour
	AuthCodeRetention  = 10 * time.Minute // kept past expiry so a second redeem is detected
	CapRetention       = 2 * time.Hour    // after the end of the counted hour
	ConnectStateTTL    = 15 * time.Minute
	RefreshIdleTTL     = 30 * 24 * time.Hour
	RefreshAbsoluteTTL = 90 * 24 * time.Hour
	RefreshGrace       = 10 * time.Second
	DefaultHourlyCap   = 10
)

// Account statuses (R1).
const (
	StatusOK           = "ok"
	StatusNeedsReauth  = "needs_reauth"
	StatusError        = "error"
	StatusDisconnected = "disconnected"
)

// Account is one connected mailbox. IDs are opaque ("acc_<base32>") and never
// the address. IsDefault is derived on read from the owner's default pointer.
type Account struct {
	Owner string `json:"owner"`
	ID    string `json:"id"`
	// Provider names the mailbox provider ("gmail").
	Provider string `json:"provider"`
	// ProviderUID is the provider's stable identity for the mailbox, used to
	// recognise a reconnect: for Gmail it is the lowercased profile address.
	ProviderUID string `json:"provider_uid"`
	Address     string `json:"address"`
	DisplayName string `json:"display_name,omitempty"`
	// Nickname is the display form; its slug is unique per owner.
	Nickname    string                `json:"nickname"`
	Groups      []string              `json:"groups,omitempty"`
	IsDefault   bool                  `json:"-"`
	ColorIndex  int                   `json:"color_index"`
	Status      string                `json:"status"`
	LastSyncAt  *time.Time            `json:"last_sync_at,omitempty"`
	LastError   string                `json:"last_error,omitempty"`
	SendEnabled bool                  `json:"send_enabled"`
	HourlyCap   int                   `json:"hourly_cap"`
	Caps        provider.Capabilities `json:"capabilities"`
	Scopes      []string              `json:"scopes,omitempty"`
	// RefreshToken is the envelope-sealed Google refresh token. Empty once
	// the account is disconnected.
	RefreshToken []byte    `json:"refresh_token,omitempty"`
	Created      time.Time `json:"created"`
	Updated      time.Time `json:"updated"`
}

// OAuthClient is a dynamically registered MCP client.
type OAuthClient struct {
	ID           string    `json:"id"`
	Name         string    `json:"name"`
	RedirectURIs []string  `json:"redirect_uris"`
	Created      time.Time `json:"created"`
}

// AuthCode is a single-use authorization code, keyed by its hash.
type AuthCode struct {
	Hash          string `json:"hash"`
	Owner         string `json:"owner"`
	ClientID      string `json:"client_id"`
	RedirectURI   string `json:"redirect_uri"`
	CodeChallenge string `json:"code_challenge"`
	Resource      string `json:"resource"`
	// FamilyID names the token family minted on redeem; a replay revokes it.
	FamilyID   string     `json:"family_id"`
	Created    time.Time  `json:"created"`
	Expires    time.Time  `json:"expires"`
	RedeemedAt *time.Time `json:"redeemed_at,omitempty"`
	// TTL is the retention deadline set by the store.
	TTL time.Time `json:"-"`
}

// Session kinds.
const (
	SessionDashboard = "dashboard"
	SessionAccess    = "access"
)

// Session is a dashboard login or an MCP access token, keyed by hash.
type Session struct {
	Hash     string    `json:"hash"`
	Kind     string    `json:"kind"`
	Owner    string    `json:"owner"`
	ClientID string    `json:"client_id,omitempty"`
	FamilyID string    `json:"family_id,omitempty"`
	Audience string    `json:"audience,omitempty"`
	Scope    string    `json:"scope,omitempty"`
	Created  time.Time `json:"created"`
	Expires  time.Time `json:"expires"`
	Revoked  bool      `json:"revoked"`
	TTL      time.Time `json:"-"`
}

// RefreshFamily is one chain of rotating refresh tokens.
type RefreshFamily struct {
	ID          string     `json:"id"`
	Owner       string     `json:"owner"`
	ClientID    string     `json:"client_id"`
	Audience    string     `json:"audience,omitempty"`
	CurrentHash string     `json:"current_hash"`
	PrevHash    string     `json:"prev_hash,omitempty"`
	PrevUntil   *time.Time `json:"prev_until,omitempty"`
	// GraceUsed is set once the previous token has been replayed within the grace window.
	GraceUsed bool `json:"grace_used,omitempty"`
	// GracePair is the sealed token pair the winning rotation handed out. A
	// concurrent loser replaying the previous token inside the grace window
	// gets this pair once, so it ends up holding the family's current token.
	GracePair   []byte    `json:"grace_pair,omitempty"`
	Created     time.Time `json:"created"`
	LastRotated time.Time `json:"last_rotated"`
	IdleExpires time.Time `json:"idle_expires"`
	AbsExpires  time.Time `json:"abs_expires"`
	Revoked     bool      `json:"revoked"`
}

// RotateOutcome says what RotateRefresh did with the presented token.
type RotateOutcome string

const (
	// OutcomeRotated: the presented token was current; the family now holds the new hash.
	OutcomeRotated RotateOutcome = "rotated"
	// OutcomeGraceReplay: the presented token was the previous one, replayed
	// within the grace window for the first time. The caller may hand out the
	// winner's pair once.
	OutcomeGraceReplay RotateOutcome = "grace_replay"
	// OutcomeReused: any other reuse. The whole family is now revoked.
	OutcomeReused RotateOutcome = "reused"
	// OutcomeRevoked and OutcomeExpired: the family cannot rotate.
	OutcomeRevoked RotateOutcome = "revoked"
	OutcomeExpired RotateOutcome = "expired"
)

// AuthTxn is one in-flight /authorize browser transaction, keyed by the hash
// of the secret polling handle the browser holds in its cookie. The dashboard
// sign-in it waits on is bound to the same hash through the request payload.
type AuthTxn struct {
	Hash          string `json:"hash"`
	Owner         string `json:"owner"`
	ClientID      string `json:"client_id"`
	RedirectURI   string `json:"redirect_uri"`
	CodeChallenge string `json:"code_challenge"`
	Resource      string `json:"resource"`
	Scope         string `json:"scope,omitempty"`
	State         string `json:"state,omitempty"`
	// RequestID and PayloadSHA256 identify the approver request the poll
	// verifies a receipt against.
	RequestID     string    `json:"request_id"`
	PayloadSHA256 string    `json:"payload_sha256"`
	Expires       time.Time `json:"expires"`
	TTL           time.Time `json:"-"`
}

// ConnectState is the server side of one Google connect attempt.
type ConnectState struct {
	Hash     string `json:"hash"`
	Owner    string `json:"owner"`
	Verifier string `json:"verifier"`
	// ReconnectID names the account being reconnected, if any.
	ReconnectID string `json:"reconnect_id,omitempty"`
	// Nickname is the requested nickname for a new account, if any.
	Nickname string    `json:"nickname,omitempty"`
	Expires  time.Time `json:"expires"`
	TTL      time.Time `json:"-"`
}

// Approval is moose-email's local record of one approver request.
type Approval struct {
	ID             string `json:"id"`
	Owner          string `json:"owner"`
	Kind           string `json:"kind"`
	AccountID      string `json:"account_id,omitempty"`
	IdempotencyKey string `json:"idempotency_key"`
	PayloadSHA256  string `json:"payload_sha256"`
	Payload        []byte `json:"payload,omitempty"`
	ApprovalURL    string `json:"approval_url,omitempty"`
	DraftID        string `json:"draft_id,omitempty"`
	RFC822SHA256   string `json:"rfc822_sha256,omitempty"`
	// Snapshot is the envelope-sealed approved RFC822 (send requests only).
	Snapshot []byte    `json:"snapshot,omitempty"`
	Status   string    `json:"status"`
	Created  time.Time `json:"created"`
	Expires  time.Time `json:"expires"`
	TTL      time.Time `json:"-"`
}

// Approval statuses held locally.
const (
	ApprovalPending  = "pending"
	ApprovalApproved = "approved"
	ApprovalConsumed = "consumed"
	ApprovalDenied   = "denied"
	ApprovalExpired  = "expired"
)

// Approval kinds (the approver's payload kinds, Phase 9).
const (
	KindAccountSendEnable = "account-send-enable"
	KindEmailSend         = "email-send"
	KindEmailSendBatch    = "email-send-batch"
	KindContactTrustBatch = "contact-trust-batch"
	KindDashboardSession  = "dashboard-session"
)

// Send ledger states (Phase 14 drives the transitions).
const (
	SendClaimed     = "claimed"
	SendDispatching = "dispatching"
	SendSent        = "sent"
	SendFailed      = "failed"
	SendUnknown     = "unknown"
	SendWouldSend   = "would_send"
)

// SendRow is the durable ledger entry for one send request.
type SendRow struct {
	RequestID         string `json:"request_id"`
	Owner             string `json:"owner"`
	AccountID         string `json:"account_id"`
	State             string `json:"state"`
	ProviderMessageID string `json:"provider_message_id,omitempty"`
	// Hour is the cap bucket the claim was counted in (HourKey).
	Hour    string    `json:"hour"`
	Created time.Time `json:"created"`
	Updated time.Time `json:"updated"`
	TTL     time.Time `json:"-"`
}

// Contact trust levels.
const (
	TrustNew     = "new"
	TrustKnown   = "known"
	TrustTrusted = "trusted"
	TrustBlocked = "blocked"
)

// Contact is one address as seen by one account. Contacts never cross accounts.
type Contact struct {
	AccountID string    `json:"account_id"`
	Address   string    `json:"address"`
	Trust     string    `json:"trust"`
	Tags      []string  `json:"tags,omitempty"`
	Updated   time.Time `json:"updated"`
}

// ContactFilter narrows ListContacts. Empty fields match everything.
type ContactFilter struct {
	Trust string
	Tag   string
}

// AuditEvent is metadata only: never a body, token or approval URL.
type AuditEvent struct {
	At         time.Time `json:"at"`
	Owner      string    `json:"owner"`
	RequestID  string    `json:"request_id"`
	AccountID  string    `json:"account_id,omitempty"`
	Tool       string    `json:"tool"`
	Recipients []string  `json:"recipients,omitempty"`
	BodySHA256 string    `json:"body_sha256,omitempty"`
	ApprovalID string    `json:"approval_id,omitempty"`
	Detail     string    `json:"detail,omitempty"`
	// ExpiresAt is the retention deadline set by the store (At + AuditTTL).
	ExpiresAt time.Time `json:"-"`
}

// Store is the email persistence interface.
type Store interface {
	Accounts
	OAuthClients
	AuthCodes
	Sessions
	RefreshFamilies
	ConnectStates
	AuthTxns
	Approvals
	SendLedger
	HourlyCap
	Contacts
	Audit
	Close() error
}

// Accounts holds connected mailboxes. Keys: PK OWNER#<owner>, SK ACCT#<id>, so
// listing an owner's accounts is one query; a NICK#<slug> row enforces nickname
// uniqueness in the same transaction as the account; an address index serves
// dedupe lookups.
type Accounts interface {
	// CreateAccount writes the account and its nickname row atomically.
	// ErrConflict if the id exists, ErrNicknameTaken if the slug is used.
	CreateAccount(ctx context.Context, a Account) error
	// UpdateAccount replaces the account's mutable fields (everything but
	// Owner, ID, Created and IsDefault) and moves the nickname row on rename.
	// ErrNotFound, ErrNicknameTaken.
	UpdateAccount(ctx context.Context, a Account) error
	GetAccount(ctx context.Context, owner, id string) (Account, error)
	// ListAccounts returns every account of owner, oldest first.
	ListAccounts(ctx context.Context, owner string) ([]Account, error)
	// FindAccountByAddress looks an account up by its lowercased address.
	FindAccountByAddress(ctx context.Context, owner, address string) (Account, error)
	// SetDefaultAccount makes id the owner's only default account, atomically.
	SetDefaultAccount(ctx context.Context, owner, id string) error
	// DisconnectAccount deletes the sealed tokens, turns send off and sets the
	// status to disconnected. The nickname row and id are kept, so a later
	// reconnect keeps the same identity.
	DisconnectAccount(ctx context.Context, owner, id string, now time.Time) error
}

// OAuthClients holds DCR registrations.
type OAuthClients interface {
	CreateOAuthClient(ctx context.Context, c OAuthClient) error // ErrConflict on duplicate id
	GetOAuthClient(ctx context.Context, id string) (OAuthClient, error)
}

// AuthCodes holds single-use authorization codes.
type AuthCodes interface {
	CreateAuthCode(ctx context.Context, c AuthCode) error
	// RedeemAuthCode marks the code redeemed with a conditional write. The
	// first call returns the code. Any later call returns the code with
	// ErrAlreadyRedeemed, so the caller can revoke the tokens issued from it.
	// An unredeemed code past its expiry returns ErrExpired and cannot be redeemed.
	RedeemAuthCode(ctx context.Context, hash string, now time.Time) (AuthCode, error)
}

// Sessions holds dashboard sessions and access tokens.
type Sessions interface {
	CreateSession(ctx context.Context, s Session) error
	GetSession(ctx context.Context, hash string) (Session, error)
	RevokeSession(ctx context.Context, hash string) error
}

// RefreshFamilies holds rotating refresh token chains.
type RefreshFamilies interface {
	CreateRefreshFamily(ctx context.Context, f RefreshFamily) error
	GetRefreshFamily(ctx context.Context, id string) (RefreshFamily, error)
	// RotateRefresh presents a refresh token hash and, if it is the current one,
	// atomically swaps in newHash. Of two concurrent presentations exactly one
	// rotates; the other sees the previous token (grace replay or reuse).
	RotateRefresh(ctx context.Context, familyID, presentedHash, newHash string, now time.Time) (RotateOutcome, error)
	// RotateRefreshWithPair is RotateRefresh that also records pair (the sealed
	// tokens being handed out) on the family when the rotation wins, in the
	// same write. A grace replay reads it back with GetRefreshFamily.
	RotateRefreshWithPair(ctx context.Context, familyID, presentedHash, newHash string, pair []byte, now time.Time) (RotateOutcome, error)
	// RevokeFamily revokes the family and every session minted in it.
	RevokeFamily(ctx context.Context, familyID string) error
}

// ConnectStates holds in-flight Google connect attempts.
type ConnectStates interface {
	PutConnectState(ctx context.Context, c ConnectState) error
	// TakeConnectState deletes and returns the state; ErrNotFound if absent,
	// already taken, or expired.
	TakeConnectState(ctx context.Context, hash string, now time.Time) (ConnectState, error)
}

// AuthTxns holds in-flight /authorize transactions.
type AuthTxns interface {
	// PutAuthTxn stores a transaction; ErrConflict if the hash exists.
	PutAuthTxn(ctx context.Context, t AuthTxn) error
	// GetAuthTxn reads a transaction without consuming it; ErrNotFound if
	// absent, taken or expired.
	GetAuthTxn(ctx context.Context, hash string, now time.Time) (AuthTxn, error)
	// TakeAuthTxn deletes and returns the transaction with a conditional
	// delete, so of two concurrent callers exactly one wins; the other gets
	// ErrNotFound.
	TakeAuthTxn(ctx context.Context, hash string, now time.Time) (AuthTxn, error)
}

// Approvals holds moose-email's view of approver requests.
type Approvals interface {
	// CreateApproval stores a new request. If the idempotency key already
	// exists it returns the stored request and created=false.
	CreateApproval(ctx context.Context, a Approval, now time.Time) (got Approval, created bool, err error)
	GetApproval(ctx context.Context, id string) (Approval, error)
	// TransitionApproval moves the status to `to` if it is currently one of
	// `from` (ErrInvalidTransition otherwise). clearSnapshot drops the sealed
	// RFC822 in the same write.
	TransitionApproval(ctx context.Context, id string, from []string, to string, clearSnapshot bool) (Approval, error)
}

// SendLedger is the durable double-send guard.
type SendLedger interface {
	// ClaimSend atomically writes a `claimed` row (ErrAlreadyClaimed if the
	// request already has one) and reserves one slot of the account's hourly
	// cap (ErrRateLimited at capLimit). Either failure aborts both writes.
	ClaimSend(ctx context.Context, row SendRow, capLimit int) error
	GetSend(ctx context.Context, requestID string) (SendRow, error)
	// AdvanceSend moves the row from `from` to `to`, conditional on the
	// current state (ErrConflict otherwise).
	AdvanceSend(ctx context.Context, requestID, from, to, providerMessageID string, now time.Time) (SendRow, error)
	// FailSend moves the row from `from` to `failed` and releases its cap slot
	// in one transaction.
	FailSend(ctx context.Context, requestID, from string, now time.Time) error
}

// HourlyCap counts sends per account per UTC hour.
type HourlyCap interface {
	// ReserveCap adds one to the account's counter for the hour containing t,
	// conditional on count < limit (ErrRateLimited otherwise).
	ReserveCap(ctx context.Context, accountID string, t time.Time, limit int) error
	ReleaseCap(ctx context.Context, accountID string, t time.Time) error
	HourlyCount(ctx context.Context, accountID string, t time.Time) (int, error)
}

// Contacts holds per-account contact trust. Keys: PK ACCT#<id>, SK CONTACT#<address>.
type Contacts interface {
	PutContact(ctx context.Context, c Contact) error
	GetContact(ctx context.Context, accountID, address string) (Contact, error) // ErrNotFound means "new"
	ListContacts(ctx context.Context, accountID string, f ContactFilter) ([]Contact, error)
}

// Audit holds metadata-only audit rows with a 90-day ttl.
type Audit interface {
	AppendAudit(ctx context.Context, e AuditEvent) error
	// ListAudit returns the owner's events at or after since, newest first.
	ListAudit(ctx context.Context, owner string, since time.Time, limit int) ([]AuditEvent, error)
}

// HashSecret returns the hex sha256 used as the storage key for codes, session
// tokens and refresh tokens.
func HashSecret(secret string) string {
	h := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(h[:])
}

// HourKey names the cap bucket for t: the UTC hour as YYYYMMDDHH.
func HourKey(t time.Time) string { return t.UTC().Format("2006010215") }

var idEnc = base32.StdEncoding.WithPadding(base32.NoPadding)

// NewAccountID returns a fresh opaque id, "acc_" plus 16 lowercase base32 chars.
func NewAccountID() string {
	var b [10]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(fmt.Sprintf("emailstore: random: %v", err))
	}
	return "acc_" + strings.ToLower(idEnc.EncodeToString(b[:]))
}

// NormalizeNickname validates a nickname (1 to 24 characters, no control
// characters) and returns its trimmed display form and its slug. The slug is
// lowercase with runs of whitespace collapsed to one dash.
func NormalizeNickname(nick string) (display, slug string, err error) {
	display = strings.TrimSpace(nick)
	if n := utf8.RuneCountInString(display); n < 1 || n > 24 {
		return "", "", ErrInvalidNickname
	}
	for _, r := range display {
		if unicode.IsControl(r) {
			return "", "", ErrInvalidNickname
		}
	}
	slug = strings.Join(strings.Fields(strings.ToLower(display)), "-")
	return display, slug, nil
}
