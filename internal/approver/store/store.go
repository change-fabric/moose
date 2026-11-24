// Package store is moose-approver's persistence: approval requests and their
// encrypted previews, single-use WebAuthn challenges, passkey credentials and
// hashed single-use enrollment tokens. It runs on the same Backend contract as
// the email store (DynamoDB table `moose-approver` in production, SQLite for
// local runs), so every conditional write here is atomic on both.
//
// Retention rules the tests pin down:
//   - a preview (headers, body text) lives only in its own sealed row, with a
//     ttl of at most 24 hours from creation, and is deleted on deny, on expiry
//     and on consume;
//   - request rows keep hashes and metadata only;
//   - challenges and enrollment tokens are consumed by a conditional delete
//     before they are honoured, so a replay finds nothing.
package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/change-fabric/moose/internal/approver/receipt"
	"github.com/change-fabric/moose/internal/email/envelope"
	estore "github.com/change-fabric/moose/internal/email/store"
)

// Errors compared with errors.Is.
var (
	ErrNotFound          = errors.New("approver store: not found")
	ErrConflict          = errors.New("approver store: conflict")
	ErrIdempotencyReuse  = errors.New("approver store: idempotency key reused with a different payload")
	ErrInvalidTransition = errors.New("approver store: state does not allow this")
	ErrExpired           = errors.New("approver store: expired")
	ErrNeedsFromConfirm  = errors.New("approver store: the FROM account has not been confirmed")
	ErrAlreadyConsumed   = errors.New("approver store: already consumed")
)

// Request statuses.
const (
	StatusPending  = "pending"
	StatusDenied   = "denied"
	StatusExpired  = "expired"
	StatusApproved = "approved"
	StatusConsumed = "consumed"
)

// Retention.
const (
	RequestLifetime = 15 * time.Minute // a request can be decided for this long
	ReceiptLifetime = 15 * time.Minute // a receipt is valid for this long after approval
	PreviewMaxTTL   = 24 * time.Hour
	RequestKeep     = 7 * 24 * time.Hour // request rows (hashes only) outlive previews
	ChallengeTTL    = 5 * time.Minute
	EnrollTTL       = 15 * time.Minute
)

// Request is one approval request. It holds no message content.
type Request struct {
	ID             string `json:"id"`
	Owner          string `json:"owner"`
	Kind           string `json:"kind"`
	IdempotencyKey string `json:"idempotency_key"`
	AccountID      string `json:"account_id,omitempty"`
	FromAddress    string `json:"from_address,omitempty"`
	// PayloadSHA256 covers Payload. A batch whose items were dropped at
	// approval has its payload and hash rewritten to the approved list;
	// OrigPayloadSHA256 keeps the hash it was created with.
	PayloadSHA256     string          `json:"payload_sha256"`
	OrigPayloadSHA256 string          `json:"orig_payload_sha256,omitempty"`
	Payload           json.RawMessage `json:"payload"`
	Status            string          `json:"status"`
	Created           time.Time       `json:"created"`
	Expires           time.Time       `json:"expires"`
	FromConfirmedAt   *time.Time      `json:"from_confirmed_at,omitempty"`
	ApprovedAt        *time.Time      `json:"approved_at,omitempty"`
	DeniedAt          *time.Time      `json:"denied_at,omitempty"`
	ConsumedAt        *time.Time      `json:"consumed_at,omitempty"`
	Claimant          string          `json:"claimant,omitempty"`
	// BatchID names the batch request that approved or dropped this item.
	BatchID string `json:"batch_id,omitempty"`
	// Receipt is set once approved. An item approved through a batch carries
	// no receipt of its own: the batch's receipt covers it.
	Receipt *receipt.Receipt `json:"receipt,omitempty"`
	// ApprovalExpires is when the approval stops being consumable: the
	// receipt's expiry, also for an item approved through a batch.
	ApprovalExpires *time.Time `json:"approval_expires,omitempty"`
}

// Open reports whether the request still counts as outstanding.
func (r Request) Open() bool { return r.Status == StatusPending || r.Status == StatusApproved }

// Challenge is one single-use WebAuthn ceremony, keyed by the hash of its challenge.
type Challenge struct {
	Hash string `json:"hash"`
	// Purpose is "approve", "deny", "register" or "queue-login".
	Purpose       string          `json:"purpose"`
	Owner         string          `json:"owner"`
	RequestID     string          `json:"request_id"`
	PayloadSHA256 string          `json:"payload_sha256"`
	Drops         []string        `json:"drops,omitempty"`
	Session       json.RawMessage `json:"session"`
	Expires       time.Time       `json:"expires"`
}

// Credential is a stored passkey. Data is the go-webauthn credential as JSON.
type Credential struct {
	Owner   string          `json:"owner"`
	ID      string          `json:"id"`
	Data    json.RawMessage `json:"data"`
	Created time.Time       `json:"created"`
}

// EnrollToken is a first-passkey enrollment token, kept only as its hash.
type EnrollToken struct {
	Hash    string    `json:"hash"`
	Owner   string    `json:"owner"`
	Expires time.Time `json:"expires"`
}

// Store is the approver's store.
type Store struct {
	b    estore.Backend
	seal envelope.Sealer
	now  func() time.Time
}

// New returns a Store over b. seal encrypts previews.
func New(b estore.Backend, seal envelope.Sealer) *Store { return NewWithClock(b, seal, time.Now) }

// NewWithClock is New with an injectable clock.
func NewWithClock(b estore.Backend, seal envelope.Sealer, now func() time.Time) *Store {
	return &Store{b: b, seal: seal, now: now}
}

// Close closes the backend.
func (s *Store) Close() error { return s.b.Close() }

// ---- keys ----

func reqPK(id string) string { return "REQ#" + id }

const (
	metaSK    = "META"
	previewSK = "PREVIEW"
)

func openKey(owner string) string { return "OPEN#" + owner }

func idemPK(owner string) string { return "IDEM#" + owner }

// Hash returns the hex sha256 of a secret value, the key form for tokens and challenges.
func Hash(v string) string {
	h := sha256.Sum256([]byte(v))
	return hex.EncodeToString(h[:])
}

var idEnc = base32.StdEncoding.WithPadding(base32.NoPadding)

// NewRequestID returns "apr_" and 26 lowercase base32 characters (128 bits).
func NewRequestID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(fmt.Sprintf("approver store: random: %v", err))
	}
	return "apr_" + strings.ToLower(idEnc.EncodeToString(b[:]))
}

// NewToken returns a 256-bit URL-safe secret.
func NewToken() string {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(fmt.Sprintf("approver store: random: %v", err))
	}
	return base64.RawURLEncoding.EncodeToString(b[:])
}

func encode(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(fmt.Sprintf("approver store: encode %T: %v", v, err))
	}
	return b
}

func previewCtx(owner, id string) envelope.Context {
	// The envelope context needs {owner, account_id}; the request id takes the
	// second slot so a sealed preview opens only for its own request.
	return envelope.Context{Owner: owner, AccountID: id}
}

type loaded struct {
	req  Request
	item estore.Item
}

func (s *Store) load(ctx context.Context, id string) (loaded, error) {
	it, ok, err := s.b.Get(ctx, reqPK(id), metaSK)
	if err != nil {
		return loaded{}, err
	}
	if !ok {
		return loaded{}, ErrNotFound
	}
	var r Request
	if err := json.Unmarshal(it.Data, &r); err != nil {
		return loaded{}, fmt.Errorf("approver store: decode request %s: %w", id, err)
	}
	return loaded{r, it}, nil
}

// putOp writes r back at version l.item.Ver+1, conditional on that version.
func (l loaded) putOp(r Request) estore.Op {
	it := l.item
	it.Data = encode(r)
	it.Ver = l.item.Ver + 1
	it.G1 = ""
	if r.Open() {
		it.G1 = openKey(r.Owner)
	}
	return estore.Op{Kind: estore.OpPut, Item: it, Cond: estore.Cond{Kind: estore.CondVer, Ver: l.item.Ver}}
}

func previewDelete(id string) estore.Op {
	return estore.Op{Kind: estore.OpDelete, Item: estore.Item{PK: reqPK(id), SK: previewSK}}
}

func lost(err error) bool {
	var ce *estore.CondError
	return errors.As(err, &ce)
}

// retry runs fn until it commits or fails for a reason other than a lost race.
func retry(fn func() (stop bool, err error)) error {
	for range 10 {
		stop, err := fn()
		if stop || err != nil {
			return err
		}
	}
	return fmt.Errorf("%w: too much contention", ErrConflict)
}

// ---- requests ----

// CreateRequest stores r (status pending) with its preview, sealed. Calling it
// again with the same owner and idempotency key returns the stored request and
// created=false, or ErrIdempotencyReuse if the payload hash differs.
func (s *Store) CreateRequest(ctx context.Context, r Request, preview []byte) (Request, bool, error) {
	if r.ID == "" || r.Owner == "" || r.Kind == "" || r.IdempotencyKey == "" || r.PayloadSHA256 == "" {
		return Request{}, false, errors.New("approver store: request needs id, owner, kind, idempotency key and payload hash")
	}
	r.Status = StatusPending
	r.OrigPayloadSHA256 = r.PayloadSHA256
	sealed, err := s.seal.Seal(ctx, preview, previewCtx(r.Owner, r.ID))
	if err != nil {
		return Request{}, false, err
	}
	keep := r.Created.Add(RequestKeep)
	ptr, _ := json.Marshal(map[string]string{"request_id": r.ID, "payload_sha256": r.PayloadSHA256})
	err = s.b.Transact(ctx, []estore.Op{
		{Kind: estore.OpPut, Item: estore.Item{PK: reqPK(r.ID), SK: metaSK, G1: openKey(r.Owner), Data: encode(r), TTL: keep}, Cond: estore.Cond{Kind: estore.CondAbsent}},
		{Kind: estore.OpPut, Item: estore.Item{PK: reqPK(r.ID), SK: previewSK, Data: sealed, TTL: r.Created.Add(PreviewMaxTTL)}},
		{Kind: estore.OpPut, Item: estore.Item{PK: idemPK(r.Owner), SK: Hash(r.IdempotencyKey), Data: ptr, TTL: keep}, Cond: estore.Cond{Kind: estore.CondAbsent}},
	})
	var ce *estore.CondError
	switch {
	case err == nil:
		return r, true, nil
	case errors.As(err, &ce) && ce.Index == 2:
		it, ok, gerr := s.b.Get(ctx, idemPK(r.Owner), Hash(r.IdempotencyKey))
		if gerr != nil || !ok {
			return Request{}, false, fmt.Errorf("approver store: idempotency row vanished: %v", gerr)
		}
		var p struct {
			RequestID     string `json:"request_id"`
			PayloadSHA256 string `json:"payload_sha256"`
		}
		if err := json.Unmarshal(it.Data, &p); err != nil {
			return Request{}, false, err
		}
		if p.PayloadSHA256 != r.PayloadSHA256 {
			return Request{}, false, ErrIdempotencyReuse
		}
		existing, err := s.GetRequest(ctx, p.RequestID)
		return existing, false, err
	case errors.As(err, &ce):
		return Request{}, false, ErrConflict
	}
	return Request{}, false, err
}

// GetRequest loads a request. It does not apply expiry; see Fresh.
func (s *Store) GetRequest(ctx context.Context, id string) (Request, error) {
	l, err := s.load(ctx, id)
	return l.req, err
}

// Fresh loads a request and, if it is pending past its expiry, moves it to
// expired and deletes its preview first. Reads and decisions call it, so an
// expired request is never decided even if no sweep has run.
func (s *Store) Fresh(ctx context.Context, id string, now time.Time) (Request, error) {
	var out Request
	err := retry(func() (bool, error) {
		l, err := s.load(ctx, id)
		if err != nil {
			return false, err
		}
		out = l.req
		if l.req.Status != StatusPending || now.Before(l.req.Expires) {
			return true, nil
		}
		next := l.req
		next.Status = StatusExpired
		err = s.b.Transact(ctx, []estore.Op{l.putOp(next), previewDelete(id)})
		if lost(err) {
			return false, nil
		}
		if err == nil {
			out = next
		}
		return true, err
	})
	return out, err
}

// Preview opens the request's sealed preview. ErrNotFound once it is deleted.
func (s *Store) Preview(ctx context.Context, r Request) ([]byte, error) {
	it, ok, err := s.b.Get(ctx, reqPK(r.ID), previewSK)
	if err != nil {
		return nil, err
	}
	if !ok || (!it.TTL.IsZero() && !s.now().Before(it.TTL)) {
		return nil, ErrNotFound
	}
	return s.seal.Open(ctx, it.Data, previewCtx(r.Owner, r.ID))
}

// HasPreview reports whether the preview row still exists.
func (s *Store) HasPreview(ctx context.Context, id string) (bool, error) {
	_, ok, err := s.b.Get(ctx, reqPK(id), previewSK)
	return ok, err
}

// mutate applies fn to the fresh request in a version-checked write. fn
// returns the next state and any extra ops (they must not touch the request
// row); a returned error aborts without writing.
func (s *Store) mutate(ctx context.Context, id string, fn func(Request) (Request, []estore.Op, error)) (Request, error) {
	var out Request
	err := retry(func() (bool, error) {
		l, err := s.load(ctx, id)
		if err != nil {
			return false, err
		}
		next, extra, err := fn(l.req)
		if err != nil {
			out = l.req
			return true, err
		}
		err = s.b.Transact(ctx, append([]estore.Op{l.putOp(next)}, extra...))
		if lost(err) {
			return false, nil
		}
		out = next
		return true, err
	})
	if errors.Is(err, errNoWrite) {
		return out, nil
	}
	return out, err
}

func pendingOnly(r Request, now time.Time) error {
	if r.Status == StatusPending && !now.Before(r.Expires) {
		return ErrExpired
	}
	if r.Status != StatusPending {
		return fmt.Errorf("%w: request is %s", ErrInvalidTransition, r.Status)
	}
	return nil
}

// ConfirmFrom records the "Send from this account" tap with a conditional
// write. The first tap wins; a repeat returns the stored request unchanged.
func (s *Store) ConfirmFrom(ctx context.Context, id string, now time.Time) (Request, error) {
	return s.mutate(ctx, id, func(r Request) (Request, []estore.Op, error) {
		if err := pendingOnly(r, now); err != nil {
			return r, nil, err
		}
		if r.FromConfirmedAt == nil {
			t := now.UTC()
			r.FromConfirmedAt = &t
		}
		return r, nil, nil
	})
}

// Approve moves pending to approved. needFrom requires the recorded FROM tap
// (checked inside the same conditional write that approves). sign builds the
// receipt for the request as it will be stored.
func (s *Store) Approve(ctx context.Context, id string, now time.Time, needFrom bool, sign func(Request) (receipt.Receipt, error)) (Request, error) {
	return s.mutate(ctx, id, func(r Request) (Request, []estore.Op, error) {
		if err := pendingOnly(r, now); err != nil {
			return r, nil, err
		}
		if needFrom && r.FromConfirmedAt == nil {
			return r, nil, ErrNeedsFromConfirm
		}
		t := now.UTC()
		r.Status, r.ApprovedAt = StatusApproved, &t
		rc, err := sign(r)
		if err != nil {
			return r, nil, err
		}
		r.Receipt, r.ApprovalExpires = &rc, &rc.Body.ExpiresAt
		return r, nil, nil
	})
}

// Deny moves pending to denied and deletes the preview in the same write.
func (s *Store) Deny(ctx context.Context, id string, now time.Time) (Request, error) {
	return s.mutate(ctx, id, func(r Request) (Request, []estore.Op, error) {
		if err := pendingOnly(r, now); err != nil {
			return r, nil, err
		}
		t := now.UTC()
		r.Status, r.DeniedAt = StatusDenied, &t
		return r, []estore.Op{previewDelete(id)}, nil
	})
}

// Consume moves approved to consumed and deletes the preview. The same
// non-empty claimant calling again gets the consumed request back (idempotent);
// any other second call is ErrAlreadyConsumed. A receipt past its expiry cannot
// be consumed.
func (s *Store) Consume(ctx context.Context, id, claimant string, now time.Time) (Request, error) {
	return s.mutate(ctx, id, func(r Request) (Request, []estore.Op, error) {
		switch r.Status {
		case StatusConsumed:
			if claimant != "" && r.Claimant == claimant {
				return r, nil, errNoWrite
			}
			return r, nil, ErrAlreadyConsumed
		case StatusApproved:
		default:
			return r, nil, fmt.Errorf("%w: request is %s", ErrInvalidTransition, r.Status)
		}
		if r.ApprovalExpires != nil && !now.Before(*r.ApprovalExpires) {
			return r, nil, ErrExpired
		}
		if r.Receipt == nil && r.BatchID == "" {
			return r, nil, fmt.Errorf("%w: no receipt", ErrInvalidTransition)
		}
		t := now.UTC()
		r.Status, r.ConsumedAt, r.Claimant = StatusConsumed, &t, claimant
		return r, []estore.Op{previewDelete(id)}, nil
	})
}

// errNoWrite makes mutate return the loaded request without error or write.
var errNoWrite = errors.New("no write")

// ApproveBatch approves a batch and its kept items, and denies its dropped
// items, in one transaction. final is the batch request as it will be stored
// (rewritten payload and hash); sign receives it. Dropped items must be pending
// and are denied with their previews deleted; kept items become approved and
// carry no receipt of their own.
func (s *Store) ApproveBatch(ctx context.Context, id string, now time.Time, kept, dropped []string, rewrite func(Request) (payload []byte, sha string, err error), sign func(Request) (receipt.Receipt, error)) (Request, error) {
	var out Request
	err := retry(func() (bool, error) {
		l, err := s.load(ctx, id)
		if err != nil {
			return false, err
		}
		b := l.req
		if err := pendingOnly(b, now); err != nil {
			out = b
			return true, err
		}
		if b.FromConfirmedAt == nil {
			out = b
			return true, ErrNeedsFromConfirm
		}
		t := now.UTC()
		b.Status, b.ApprovedAt = StatusApproved, &t
		payload, sha, err := rewrite(b)
		if err != nil {
			return true, err
		}
		b.Payload, b.PayloadSHA256 = payload, sha
		rc, err := sign(b)
		if err != nil {
			return true, err
		}
		b.Receipt, b.ApprovalExpires = &rc, &rc.Body.ExpiresAt
		ops := []estore.Op{l.putOp(b)}
		for _, group := range []struct {
			ids  []string
			keep bool
		}{{kept, true}, {dropped, false}} {
			for _, cid := range group.ids {
				c, err := s.load(ctx, cid)
				if err != nil {
					return true, fmt.Errorf("batch item %s: %w", cid, err)
				}
				if err := pendingOnly(c.req, now); err != nil {
					return true, fmt.Errorf("batch item %s: %w", cid, err)
				}
				next := c.req
				next.BatchID = id
				if group.keep {
					next.Status, next.ApprovedAt, next.ApprovalExpires = StatusApproved, &t, &rc.Body.ExpiresAt
					ops = append(ops, c.putOp(next))
				} else {
					next.Status, next.DeniedAt = StatusDenied, &t
					ops = append(ops, c.putOp(next), previewDelete(cid))
				}
			}
		}
		err = s.b.Transact(ctx, ops)
		if lost(err) {
			return false, nil
		}
		out = b
		return true, err
	})
	return out, err
}

// ListOpen returns the owner's pending and approved requests, oldest first.
func (s *Store) ListOpen(ctx context.Context, owner string) ([]Request, error) {
	items, err := s.b.QueryIndex(ctx, openKey(owner))
	if err != nil {
		return nil, err
	}
	var out []Request
	for _, it := range items {
		if it.SK != metaSK {
			continue
		}
		var r Request
		if err := json.Unmarshal(it.Data, &r); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	slices.SortStableFunc(out, func(a, b Request) int { return a.Created.Compare(b.Created) })
	return out, nil
}

// SweepResult counts what Sweep did.
type SweepResult struct {
	Expired         int // pending requests moved to expired
	PreviewsDeleted int // previews removed from approved requests whose receipt lapsed
}

// Sweep expires overdue pending requests and deletes their previews. It also
// deletes the preview of an approved request whose receipt has lapsed, since
// nothing can consume it any more.
func (s *Store) Sweep(ctx context.Context, owner string, now time.Time) (SweepResult, error) {
	open, err := s.ListOpen(ctx, owner)
	if err != nil {
		return SweepResult{}, err
	}
	var res SweepResult
	for _, r := range open {
		switch {
		case r.Status == StatusPending && !now.Before(r.Expires):
			got, err := s.Fresh(ctx, r.ID, now)
			if err != nil {
				return res, err
			}
			if got.Status == StatusExpired {
				res.Expired++
			}
		case r.Status == StatusApproved && r.ApprovalExpires != nil && !now.Before(*r.ApprovalExpires):
			if ok, err := s.HasPreview(ctx, r.ID); err != nil {
				return res, err
			} else if ok {
				if err := s.b.Transact(ctx, []estore.Op{previewDelete(r.ID)}); err != nil {
					return res, err
				}
				res.PreviewsDeleted++
			}
		}
	}
	return res, nil
}

// ---- challenges ----

func chalPK(hash string) string { return "CHAL#" + hash }

// PutChallenge stores a ceremony challenge under the hash of its value.
func (s *Store) PutChallenge(ctx context.Context, c Challenge) error {
	err := s.b.Transact(ctx, []estore.Op{{
		Kind: estore.OpPut, Item: estore.Item{PK: chalPK(c.Hash), SK: metaSK, Data: encode(c), TTL: c.Expires.Add(time.Minute)},
		Cond: estore.Cond{Kind: estore.CondAbsent},
	}})
	if lost(err) {
		return ErrConflict
	}
	return err
}

// TakeChallenge deletes the challenge with a conditional delete and returns
// it. Of two concurrent takes exactly one succeeds; the other, a replay, and an
// expired challenge all get ErrNotFound (an expired one is deleted too).
func (s *Store) TakeChallenge(ctx context.Context, hash string, now time.Time) (Challenge, error) {
	it, ok, err := s.b.Get(ctx, chalPK(hash), metaSK)
	if err != nil {
		return Challenge{}, err
	}
	if !ok {
		return Challenge{}, ErrNotFound
	}
	err = s.b.Transact(ctx, []estore.Op{{Kind: estore.OpDelete, Item: estore.Item{PK: it.PK, SK: it.SK}, Cond: estore.Cond{Kind: estore.CondVer, Ver: it.Ver}}})
	if lost(err) {
		return Challenge{}, ErrNotFound
	}
	if err != nil {
		return Challenge{}, err
	}
	var c Challenge
	if err := json.Unmarshal(it.Data, &c); err != nil {
		return Challenge{}, err
	}
	if !now.Before(c.Expires) {
		return Challenge{}, ErrNotFound
	}
	return c, nil
}

// ---- credentials ----

func credPK(owner string) string { return "CRED#" + owner }

// ListCredentials returns the owner's passkeys.
func (s *Store) ListCredentials(ctx context.Context, owner string) ([]Credential, error) {
	items, err := s.b.Query(ctx, credPK(owner), "")
	if err != nil {
		return nil, err
	}
	out := make([]Credential, 0, len(items))
	for _, it := range items {
		var c Credential
		if err := json.Unmarshal(it.Data, &c); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, nil
}

// UpdateCredential rewrites a stored credential's data (sign counter).
func (s *Store) UpdateCredential(ctx context.Context, owner, id string, data json.RawMessage) error {
	it, ok, err := s.b.Get(ctx, credPK(owner), id)
	if err != nil {
		return err
	}
	if !ok {
		return ErrNotFound
	}
	var c Credential
	if err := json.Unmarshal(it.Data, &c); err != nil {
		return err
	}
	c.Data = data
	it.Data, it.Ver = encode(c), it.Ver+1
	err = s.b.Transact(ctx, []estore.Op{{Kind: estore.OpPut, Item: it, Cond: estore.Cond{Kind: estore.CondVer, Ver: it.Ver - 1}}})
	if lost(err) {
		return ErrConflict
	}
	return err
}

// ---- enrollment tokens ----

func enrollPK(hash string) string { return "ENROLL#" + hash }

// CreateEnrollToken stores a token's hash with its owner and expiry.
func (s *Store) CreateEnrollToken(ctx context.Context, t EnrollToken) error {
	return s.b.Transact(ctx, []estore.Op{{
		Kind: estore.OpPut, Item: estore.Item{PK: enrollPK(t.Hash), SK: metaSK, Data: encode(t), TTL: t.Expires.Add(time.Minute)},
		Cond: estore.Cond{Kind: estore.CondAbsent},
	}})
}

// PeekEnrollToken returns a live token without consuming it. ErrNotFound if
// absent or expired.
func (s *Store) PeekEnrollToken(ctx context.Context, hash string, now time.Time) (EnrollToken, error) {
	t, _, err := s.enroll(ctx, hash, now)
	return t, err
}

func (s *Store) enroll(ctx context.Context, hash string, now time.Time) (EnrollToken, estore.Item, error) {
	it, ok, err := s.b.Get(ctx, enrollPK(hash), metaSK)
	if err != nil {
		return EnrollToken{}, it, err
	}
	if !ok {
		return EnrollToken{}, it, ErrNotFound
	}
	var t EnrollToken
	if err := json.Unmarshal(it.Data, &t); err != nil {
		return EnrollToken{}, it, err
	}
	if !now.Before(t.Expires) {
		return EnrollToken{}, it, ErrNotFound
	}
	return t, it, nil
}

// RedeemEnrollToken consumes the token and stores the credential in one
// transaction: the delete is conditional on the token's version and the
// credential write on its absence, so a token works exactly once.
func (s *Store) RedeemEnrollToken(ctx context.Context, hash string, now time.Time, cred Credential) error {
	t, it, err := s.enroll(ctx, hash, now)
	if err != nil {
		return err
	}
	if cred.Owner != t.Owner {
		return ErrInvalidTransition
	}
	err = s.b.Transact(ctx, []estore.Op{
		{Kind: estore.OpDelete, Item: estore.Item{PK: it.PK, SK: it.SK}, Cond: estore.Cond{Kind: estore.CondVer, Ver: it.Ver}},
		{Kind: estore.OpPut, Item: estore.Item{PK: credPK(cred.Owner), SK: cred.ID, Data: encode(cred)}, Cond: estore.Cond{Kind: estore.CondAbsent}},
	})
	if lost(err) {
		return ErrNotFound
	}
	return err
}
