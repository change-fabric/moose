package store

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// Calendar request ledger states. The calendar-invite flow (a later phase)
// drives the transitions; the ledger here is the durable claim, keyed apart
// from the send ledger so the two never collide in the one table.
const (
	CalendarClaimed     = "claimed"
	CalendarDispatching = "dispatching"
	CalendarApplied     = "applied"
	CalendarFailed      = "failed"
	CalendarUnknown     = "unknown"
	CalendarWouldApply  = "would_apply"
)

// CalendarIdempotencyTTL is how long a create's idempotency key is remembered.
const CalendarIdempotencyTTL = 7 * 24 * time.Hour

// CalendarRow is the durable ledger entry for one calendar write that needed an
// approval (create, update or delete of an event with attendees). Keys:
// PK CALREQ#<request id>, SK META.
type CalendarRow struct {
	RequestID string `json:"request_id"`
	Owner     string `json:"owner"`
	AccountID string `json:"account_id"`
	// Operation is create, update or cancel.
	Operation string `json:"operation"`
	State     string `json:"state"`
	// DedupeKey is the provider-side create dedupe key (a client-supplied event
	// id, or a transaction id), written before the provider call so an unknown
	// outcome can be reconciled by looking the event up.
	DedupeKey string `json:"dedupe_key,omitempty"`
	// ProviderEventID is the event the write touched, once known.
	ProviderEventID string `json:"provider_event_id,omitempty"`
	// Version is the provider event version the request was approved against.
	Version string `json:"version,omitempty"`
	// Hour is the cap bucket the claim was counted in (HourKey). The hourly cap
	// is shared with the send ledger: one counter per account.
	Hour    string    `json:"hour"`
	Created time.Time `json:"created"`
	Updated time.Time `json:"updated"`
	TTL     time.Time `json:"-"`
}

func (r *CalendarRow) setTTL(t time.Time) { r.TTL = t }

// CalendarIdempotency backs calendar_create_event: a moose-side record that a
// create with this key was already made for this account, so a retry returns
// the first result instead of a second event. Keys: PK CALIDEM#<account id>,
// SK KEY#<key>.
type CalendarIdempotency struct {
	AccountID string `json:"account_id"`
	Key       string `json:"key"`
	// RequestHash is the sha256 of the normalised create input. The same key
	// with different input is a caller mistake and is refused, not replayed.
	RequestHash string `json:"request_hash"`
	// EventID is the provider event id once the create succeeded; empty while
	// the first call is in flight or if it failed before creating.
	EventID string    `json:"event_id,omitempty"`
	Created time.Time `json:"created"`
	Updated time.Time `json:"updated"`
	TTL     time.Time `json:"-"`
}

func (r *CalendarIdempotency) setTTL(t time.Time) { r.TTL = t }

// ErrIdempotencyKeyReused means an idempotency key was seen with different input.
var ErrIdempotencyKeyReused = errors.New("emailstore: idempotency key reused with different input")

// CalendarLedger holds the calendar write ledger and the create idempotency
// records.
type CalendarLedger interface {
	// ClaimCalendarRequest atomically writes a `claimed` row (ErrAlreadyClaimed
	// if the request already has one) and reserves one slot of the account's
	// hourly cap, the same counter the send ledger uses (ErrRateLimited at
	// capLimit). Either failure aborts both writes.
	ClaimCalendarRequest(ctx context.Context, row CalendarRow, capLimit int) error
	// GetCalendarRequest returns the row, or ErrNotFound.
	GetCalendarRequest(ctx context.Context, requestID string) (CalendarRow, error)
	// AdvanceCalendarRequest moves the row from `from` to `to`, conditional on
	// the current state (ErrConflict otherwise). A non-empty eventID is recorded.
	AdvanceCalendarRequest(ctx context.Context, requestID, from, to, eventID string, now time.Time) (CalendarRow, error)
	// ReserveCalendarIdempotency stores rec if its key is unused for the
	// account and returns it with created=true. If the key exists it returns the
	// stored record with created=false, or ErrIdempotencyKeyReused when the
	// stored RequestHash differs.
	ReserveCalendarIdempotency(ctx context.Context, rec CalendarIdempotency) (got CalendarIdempotency, created bool, err error)
	// CompleteCalendarIdempotency records the created event id. It is
	// idempotent for the same id; a different id already recorded is ErrConflict.
	CompleteCalendarIdempotency(ctx context.Context, accountID, key, eventID string) (CalendarIdempotency, error)
	// ReleaseCalendarIdempotency removes a key whose create failed before any
	// event existed, so the caller can retry. It never removes a key with an event id.
	ReleaseCalendarIdempotency(ctx context.Context, accountID, key string) error
}

func calReqPK(requestID string) string  { return "CALREQ#" + requestID }
func calIdemPK(accountID string) string { return "CALIDEM#" + accountID }
func calIdemSK(key string) string       { return "KEY#" + key }

func (s *impl) ClaimCalendarRequest(ctx context.Context, row CalendarRow, capLimit int) error {
	if row.RequestID == "" || row.AccountID == "" {
		return errors.New("emailstore: calendar row needs request id and account id")
	}
	if capLimit <= 0 {
		return ErrRateLimited
	}
	if row.Created.IsZero() {
		row.Created = s.now().UTC()
	}
	row.State, row.Updated = CalendarClaimed, row.Created
	row.Hour = HourKey(row.Created)
	err := s.b.Transact(ctx, []Op{
		{Kind: OpPut, Cond: Cond{Kind: CondAbsent}, Item: Item{
			PK: calReqPK(row.RequestID), SK: "META", Data: encode(row), TTL: secs(row.Created.Add(LedgerTTL))}},
		capOp(row.AccountID, row.Created, 1, int64(capLimit)),
	})
	if i, failed := condIndex(err); failed {
		if i == 0 {
			return ErrAlreadyClaimed
		}
		return ErrRateLimited
	}
	return err
}

func (s *impl) GetCalendarRequest(ctx context.Context, requestID string) (CalendarRow, error) {
	r, _, err := load[CalendarRow](ctx, s.b, calReqPK(requestID), "META")
	return r, err
}

func (s *impl) AdvanceCalendarRequest(ctx context.Context, requestID, from, to, eventID string, now time.Time) (CalendarRow, error) {
	return modify(ctx, s.b, calReqPK(requestID), "META", func(r *CalendarRow) error {
		if r.State != from {
			return fmt.Errorf("%w: calendar row is %s, not %s", ErrConflict, r.State, from)
		}
		r.State, r.Updated = to, now.UTC()
		if eventID != "" {
			r.ProviderEventID = eventID
		}
		return nil
	})
}

func (s *impl) ReserveCalendarIdempotency(ctx context.Context, rec CalendarIdempotency) (CalendarIdempotency, bool, error) {
	if rec.AccountID == "" || rec.Key == "" || rec.RequestHash == "" {
		return CalendarIdempotency{}, false, errors.New("emailstore: idempotency record needs account id, key and request hash")
	}
	now := s.now().UTC()
	rec.Created, rec.Updated, rec.EventID = now, now, ""
	err := s.b.Transact(ctx, []Op{{Kind: OpPut, Cond: Cond{Kind: CondAbsent}, Item: Item{
		PK: calIdemPK(rec.AccountID), SK: calIdemSK(rec.Key), Data: encode(rec), TTL: secs(now.Add(CalendarIdempotencyTTL))}}})
	if _, exists := condIndex(err); exists {
		cur, _, gerr := load[CalendarIdempotency](ctx, s.b, calIdemPK(rec.AccountID), calIdemSK(rec.Key))
		if gerr != nil {
			return CalendarIdempotency{}, false, gerr
		}
		if cur.RequestHash != rec.RequestHash {
			return cur, false, ErrIdempotencyKeyReused
		}
		return cur, false, nil
	}
	if err != nil {
		return CalendarIdempotency{}, false, err
	}
	return rec, true, nil
}

func (s *impl) CompleteCalendarIdempotency(ctx context.Context, accountID, key, eventID string) (CalendarIdempotency, error) {
	if eventID == "" {
		return CalendarIdempotency{}, errors.New("emailstore: event id is required")
	}
	return modify(ctx, s.b, calIdemPK(accountID), calIdemSK(key), func(r *CalendarIdempotency) error {
		switch r.EventID {
		case eventID:
			return errSkip
		case "":
			r.EventID, r.Updated = eventID, s.now().UTC()
			return nil
		}
		return fmt.Errorf("%w: key already holds event %s", ErrConflict, r.EventID)
	})
}

func (s *impl) ReleaseCalendarIdempotency(ctx context.Context, accountID, key string) error {
	cur, it, err := load[CalendarIdempotency](ctx, s.b, calIdemPK(accountID), calIdemSK(key))
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if cur.EventID != "" {
		return fmt.Errorf("%w: key already holds event %s", ErrConflict, cur.EventID)
	}
	err = s.b.Transact(ctx, []Op{{Kind: OpDelete, Cond: Cond{Kind: CondVer, Ver: it.Ver}, Item: Item{PK: calIdemPK(accountID), SK: calIdemSK(key)}}})
	if _, lost := condIndex(err); lost {
		return fmt.Errorf("%w: key changed while releasing", ErrConflict)
	}
	return err
}
