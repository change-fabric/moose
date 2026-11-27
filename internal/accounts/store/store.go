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
	// StatusProviderDisabled is never stored. GateProviders reports it for an
	// account whose provider is switched off in config (email.microsoft.enabled
	// false), so tools hide the account while its credentials stay untouched.
	StatusProviderDisabled = "provider_disabled"
)

// Canonical provider names. Records written before Phase 4 say "gmail"; the
// store reads that as ProviderGoogle (see Account.upgrade) and rewrites the
// record the next time it is patched.
const (
	ProviderGoogle      = "google"
	ProviderMicrosoft   = "microsoft"
	providerLegacyGmail = "gmail"
)

// Services an account can grant. A record with no Services is a legacy
// mail-only account.
const (
	ServiceMail     = "mail"
	ServiceCalendar = "calendar"
)

// ServiceGrant is what one service was actually granted: the scopes the
// provider reported, not the scopes that were asked for.
type ServiceGrant struct {
	Scopes    []string  `json:"scopes,omitempty"`
	GrantedAt time.Time `json:"granted_at"`
}

// Account is one connected mailbox. IDs are opaque ("acc_<base32>") and never
// the address. IsDefault is derived on read from the owner's default pointer.
type Account struct {
	Owner string `json:"owner"`
	ID    string `json:"id"`
	// Provider names the mailbox provider: ProviderGoogle or ProviderMicrosoft.
	Provider string `json:"provider"`
	// ProviderUID is the provider's stable identity for the mailbox, used to
	// recognise a reconnect: for Gmail it is the lowercased profile address.
	ProviderUID string `json:"provider_uid"`
	Address     string `json:"address"`
	DisplayName string `json:"display_name,omitempty"`
	// Nickname is the display form; its slug is unique per owner.
	Nickname    string     `json:"nickname"`
	Groups      []string   `json:"groups,omitempty"`
	IsDefault   bool       `json:"-"`
	ColorIndex  int        `json:"color_index"`
	Status      string     `json:"status"`
	LastSyncAt  *time.Time `json:"last_sync_at,omitempty"`
	LastError   string     `json:"last_error,omitempty"`
	SendEnabled bool       `json:"send_enabled"`
	// InviteEnabled is the per-account calendar invite opt-in, off by default.
	InviteEnabled bool                  `json:"invite_enabled,omitempty"`
	HourlyCap     int                   `json:"hourly_cap"`
	Caps          provider.Capabilities `json:"capabilities"`
	// Scopes is every scope the provider reports as granted, across services.
	Scopes []string `json:"scopes,omitempty"`
	// Services records what each service was granted. Absent on legacy records,
	// where the store defaults it to {mail}.
	Services map[string]ServiceGrant `json:"services,omitempty"`
	// RefreshToken is the envelope-sealed provider refresh token. Empty once
	// the account is disconnected. Only SetAccountCredential changes it after
	// creation.
	RefreshToken []byte `json:"refresh_token,omitempty"`
	// Version counts credential writes. SetAccountCredential is conditional on
	// it, so two refreshers that read the same version cannot both write back.
	Version int64     `json:"version,omitempty"`
	Created time.Time `json:"created"`
	Updated time.Time `json:"updated"`
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

// ConnectState is the server side of one connect attempt.
type ConnectState struct {
	Hash     string `json:"hash"`
	Owner    string `json:"owner"`
	Verifier string `json:"verifier"`
	// Provider names the provider the attempt started for (ProviderGoogle or
	// ProviderMicrosoft). A state written before the field existed reads as
	// empty, which means Google.
	Provider string `json:"provider,omitempty"`
	// ReconnectID names the account being reconnected, if any.
	ReconnectID string `json:"reconnect_id,omitempty"`
	// GrantAccountID and Service mark an incremental consent: the attempt adds
	// Service (ServiceCalendar) to that existing account and never creates or
	// reconnects one. The attempt is bound to the account, the provider (above)
	// and the service, and the callback accepts it only when the identity the
	// provider returns is the account's own.
	GrantAccountID string `json:"grant_account_id,omitempty"`
	Service        string `json:"service,omitempty"`
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
	Snapshot []byte `json:"snapshot,omitempty"`
	// ThreadID is the provider thread the approved message is sent into.
	ThreadID string    `json:"thread_id,omitempty"`
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

// Send ledger states (Phase 14 drives the transitions).
const (
	SendClaimed     = "claimed"
	SendDispatching = "dispatching"
	SendSent        = "sent"
	SendFailed      = "failed"
	SendUnknown     = "unknown"
	SendWouldSend   = "would_send"
	// SendStaging is a native draft-send whose staging draft is being created. It
	// is the mutex of that step: only the call that moved the row from claimed
	// creates the draft. Nothing has been issued in this state.
	SendStaging = "staging"
	// SendSubmitted is a native draft-send the provider accepted (Graph 202) but
	// whose Sent copy has not been seen: the send is asynchronous, so it is not a
	// failure and not yet sent. It resolves by correlating the staging id.
	SendSubmitted = "submitted"
	// SendSentMismatch is terminal: the correlated Sent copy differs from the
	// approved message. Mail may have been delivered; it is never retried.
	SendSentMismatch = "sent_mismatch"
)

// SendRow is the durable ledger entry for one send request.
type SendRow struct {
	RequestID         string `json:"request_id"`
	Owner             string `json:"owner"`
	AccountID         string `json:"account_id"`
	State             string `json:"state"`
	ProviderMessageID string `json:"provider_message_id,omitempty"`
	// StagingID is the id of the staging draft a native draft-send (Graph) sends,
	// and StagingMessageID its RFC822 Message-ID. Both are written before the send
	// is issued. SentID is the id of the correlated Sent copy once seen.
	StagingID        string `json:"staging_id,omitempty"`
	StagingMessageID string `json:"staging_message_id,omitempty"`
	SentID           string `json:"sent_id,omitempty"`
	// SendIssuedAt is when the send intent was persisted, immediately before the
	// send call. A row with it set may have been sent, whatever it says next.
	SendIssuedAt time.Time `json:"send_issued_at,omitempty"`
	// ReconcileAttempts counts looks for the Sent copy; LastReconcile is the
	// latest.
	ReconcileAttempts int       `json:"reconcile_attempts,omitempty"`
	LastReconcile     time.Time `json:"last_reconcile,omitempty"`
	// SweepDone is set once the sweeper has nothing more to do for the row, which
	// takes it out of the sweep index.
	SweepDone bool `json:"sweep_done,omitempty"`
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

// MaxContactBatch is the most addresses one contact-trust-batch may hold. The
// local claim and every contact change share one transaction, and DynamoDB
// allows 100 writes in one.
const MaxContactBatch = 50

// ContactBatchRow is the local ledger entry for one applied contact-trust-batch
// request. Its presence means the request's changes were applied: the row is
// written in the same transaction as the changes.
type ContactBatchRow struct {
	RequestID string `json:"request_id"`
	Owner     string `json:"owner"`
	AccountID string `json:"account_id"`
	// Applied lists the addresses now trusted.
	Applied []string `json:"applied"`
	// Skipped lists approved addresses that were left alone because they are
	// blocked: a block set after the request was made still wins.
	Skipped []string  `json:"skipped,omitempty"`
	Created time.Time `json:"created"`
	TTL     time.Time `json:"-"`
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
	CalendarLedger
	Contacts
	DraftMetas
	Settings
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
	// UpdateAccount applies p to the account: only the fields p sets change, on
	// a fresh read, so a concurrent credential write is never overwritten and
	// the credential is never written here at all. It moves the nickname row on
	// rename. ErrNotFound, ErrNicknameTaken.
	UpdateAccount(ctx context.Context, owner, id string, p AccountPatch) error
	// SetAccountCredential replaces the sealed credential and bumps Version, but
	// only if the stored Version equals expectVersion (ErrConflict otherwise:
	// reload, re-read the credential and retry). Nothing else on the record
	// changes. ErrNotFound if the account is gone. Disconnecting bumps
	// Version too, so a write-back that read the credential before a disconnect
	// cannot revive the account.
	SetAccountCredential(ctx context.Context, owner, id string, cred []byte, expectVersion int64) error
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

// AccountPatch is a partial update of an account's non-credential fields. A nil
// field is left alone. The sealed credential is deliberately not here.
type AccountPatch struct {
	Nickname      *string
	Groups        *[]string
	ColorIndex    *int
	HourlyCap     *int
	SendEnabled   *bool
	InviteEnabled *bool
	Status        *string
	LastError     *string
	LastSyncAt    *time.Time
	// Profile fields, refreshed when an account reconnects.
	DisplayName *string
	Address     *string
	ProviderUID *string
	Caps        *provider.Capabilities
	Scopes      *[]string
	Services    *map[string]ServiceGrant
}

// Ptr returns a pointer to v, for building an AccountPatch.
func Ptr[T any](v T) *T { return &v }

// upgrade normalises a record written by an older version. It is the one place
// legacy provider ids and a missing Services map are interpreted.
func (a *Account) upgrade() {
	if a.Provider == providerLegacyGmail {
		a.Provider = ProviderGoogle
	}
	if len(a.Services) == 0 {
		a.Services = map[string]ServiceGrant{ServiceMail: {Scopes: append([]string(nil), a.Scopes...), GrantedAt: a.Created}}
	}
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
	// FindApprovalByKey returns the approval created under an idempotency key,
	// or ErrNotFound.
	FindApprovalByKey(ctx context.Context, key string) (Approval, error)
	// TransitionApproval moves the status to `to` if it is currently one of
	// `from` (ErrInvalidTransition otherwise). clearSnapshot drops the sealed
	// RFC822 in the same write.
	TransitionApproval(ctx context.Context, id string, from []string, to string, clearSnapshot bool) (Approval, error)
	// FindApprovals returns the approvals of one kind for one account, newest
	// first. It reads a secondary index, which may lag a moment on DynamoDB.
	FindApprovals(ctx context.Context, kind, accountID string) ([]Approval, error)
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
	// PatchSend writes the staging, correlation and reconciliation fields of a
	// native draft-send row and moves it from `from` to `to` (they may be equal),
	// conditional on the current state (ErrConflict otherwise). Only the fields
	// set in patch are written. A row that has a StagingID enters the sweep index
	// until a patch sets SweepDone.
	PatchSend(ctx context.Context, requestID, from, to string, patch SendPatch, now time.Time) (SendRow, error)
	// ListSendSweep returns the rows with a staging draft the sweeper has not
	// finished with: those to reconcile (submitted, unknown) and those whose
	// staging draft may need cleaning (failed, sent). It reads the secondary
	// index, which may lag a moment on DynamoDB.
	ListSendSweep(ctx context.Context) ([]SendRow, error)
}

// SendPatch is a partial update of a SendRow. Zero fields are left alone.
type SendPatch struct {
	StagingID         string
	StagingMessageID  string
	SentID            string
	ProviderMessageID string
	SendIssuedAt      time.Time
	// ReconcileAttempts is written when ReconcileAttempts > 0, with LastReconcile.
	ReconcileAttempts int
	LastReconcile     time.Time
	SweepDone         bool
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
	// ApplyContactBatch promotes addresses to trusted and records row, in one
	// atomic write: either the claim and every change land, or nothing does.
	// Addresses that are blocked are skipped and listed in the row. It returns
	// ErrConflict, changing nothing, when row.RequestID was already applied.
	ApplyContactBatch(ctx context.Context, row ContactBatchRow, addresses []string) (ContactBatchRow, error)
	// GetContactBatch returns the ledger row of an applied batch, or ErrNotFound.
	GetContactBatch(ctx context.Context, requestID string) (ContactBatchRow, error)
}

// DraftMeta is what moose remembers about a draft it created: the thread the
// message belongs to and, for a reply, which account received the original.
// Gmail's raw draft carries neither in a form moose can rely on.
type DraftMeta struct {
	AccountID string `json:"account_id"`
	DraftID   string `json:"draft_id"`
	ThreadID  string `json:"thread_id,omitempty"`
	// ReplyAccountID is the account that received the message being replied
	// to. It is empty when the draft is not a reply.
	ReplyAccountID string    `json:"reply_account_id,omitempty"`
	Updated        time.Time `json:"updated"`
}

// DraftMetaTTL is how long a draft's notes are kept.
const DraftMetaTTL = 30 * 24 * time.Hour

// DraftMetas holds notes about drafts, keyed PK ACCT#<id>, SK DRAFT#<draft id>.
type DraftMetas interface {
	PutDraftMeta(ctx context.Context, m DraftMeta) error
	GetDraftMeta(ctx context.Context, accountID, draftID string) (DraftMeta, error) // ErrNotFound if none
}

// KillState is the send kill switch.
type KillState struct {
	On     bool      `json:"on"`
	Reason string    `json:"reason,omitempty"`
	At     time.Time `json:"at"`
}

// Settings holds deployment-wide switches that can change while the server runs.
type Settings interface {
	// SetKillSwitch turns the send kill switch on or off. While it is on, no
	// request dispatches a send; requests and reads are unaffected.
	SetKillSwitch(ctx context.Context, on bool, reason string, now time.Time) error
	// KillSwitch reads the switch. An unset switch is off.
	KillSwitch(ctx context.Context) (KillState, error)
	// SetProviderKillSwitch is the kill switch scoped to one provider
	// (ProviderGoogle, ProviderMicrosoft): while it is on no account of that
	// provider dispatches a send. The global switch still applies on top.
	SetProviderKillSwitch(ctx context.Context, provider string, on bool, reason string, now time.Time) error
	// ProviderKillSwitch reads one provider's switch. An unset switch is off.
	ProviderKillSwitch(ctx context.Context, provider string) (KillState, error)
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
