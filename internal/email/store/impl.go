package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// impl implements Store on any Backend.
type impl struct {
	b   Backend
	now func() time.Time
}

// New returns a Store over b.
func New(b Backend) Store { return NewWithClock(b, time.Now) }

// NewWithClock is New with an injectable clock, for tests.
func NewWithClock(b Backend, now func() time.Time) Store { return &impl{b: b, now: now} }

func (s *impl) Close() error { return s.b.Close() }

// ttlSetter is implemented by records that expose the retention deadline the
// store set on their row.
type ttlSetter interface{ setTTL(time.Time) }

func (c *AuthCode) setTTL(t time.Time)     { c.TTL = t }
func (c *Session) setTTL(t time.Time)      { c.TTL = t }
func (c *ConnectState) setTTL(t time.Time) { c.TTL = t }
func (c *AuthTxn) setTTL(t time.Time)      { c.TTL = t }
func (a *Approval) setTTL(t time.Time)     { a.TTL = t }
func (r *SendRow) setTTL(t time.Time)      { r.TTL = t }
func (e *AuditEvent) setTTL(t time.Time)   { e.ExpiresAt = t }

func decode[T any](it Item) (T, error) {
	var v T
	if err := json.Unmarshal(it.Data, &v); err != nil {
		return v, fmt.Errorf("emailstore: decode %s/%s: %w", it.PK, it.SK, err)
	}
	if t, ok := any(&v).(ttlSetter); ok {
		t.setTTL(it.TTL)
	}
	return v, nil
}

func encode(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		// Every stored record is a plain struct of marshalable fields.
		panic(fmt.Sprintf("emailstore: encode %T: %v", v, err))
	}
	return b
}

// load reads and decodes the row at pk/sk, or returns ErrNotFound.
func load[T any](ctx context.Context, b Backend, pk, sk string) (T, Item, error) {
	var zero T
	it, ok, err := b.Get(ctx, pk, sk)
	if err != nil {
		return zero, Item{}, err
	}
	if !ok {
		return zero, Item{}, ErrNotFound
	}
	v, err := decode[T](it)
	return v, it, err
}

// errSkip tells modify to return the record without writing it.
var errSkip = errors.New("emailstore: skip write")

// modify is a read-modify-write of one row, conditional on its version, retried
// when another writer got there first. If fn returns an error the row is not
// written and modify returns the record as fn left it with that error (errSkip
// means no error).
func modify[T any](ctx context.Context, b Backend, pk, sk string, fn func(*T) error) (T, error) {
	var zero T
	for range 10 {
		it, ok, err := b.Get(ctx, pk, sk)
		if err != nil {
			return zero, err
		}
		if !ok {
			return zero, ErrNotFound
		}
		v, err := decode[T](it)
		if err != nil {
			return zero, err
		}
		if err := fn(&v); err != nil {
			if errors.Is(err, errSkip) {
				return v, nil
			}
			return v, err
		}
		next := it
		next.Data = encode(v)
		next.Ver = it.Ver + 1
		err = b.Transact(ctx, []Op{{Kind: OpPut, Item: next, Cond: Cond{Kind: CondVer, Ver: it.Ver}}})
		if _, lost := condIndex(err); lost {
			continue
		}
		if err != nil {
			return zero, err
		}
		return decode[T](next)
	}
	return zero, fmt.Errorf("%w: too much contention on %s/%s", ErrConflict, pk, sk)
}

func secs(t time.Time) time.Time { return t.UTC().Truncate(time.Second) }

// ---- accounts ----

func ownerPK(owner string) string { return "OWNER#" + owner }
func acctSK(id string) string     { return "ACCT#" + id }
func nickSK(slug string) string   { return "NICK#" + slug }

const defaultSK = "DEFAULT"

func addrKey(owner, address string) string {
	return "ADDR#" + owner + "#" + strings.ToLower(strings.TrimSpace(address))
}

type pointer struct {
	AccountID string `json:"account_id"`
}

func (s *impl) CreateAccount(ctx context.Context, a Account) error {
	if a.Owner == "" || a.ID == "" || a.Address == "" {
		return errors.New("emailstore: account needs owner, id and address")
	}
	display, slug, err := NormalizeNickname(a.Nickname)
	if err != nil {
		return err
	}
	a.Nickname = display
	now := s.now().UTC()
	if a.Created.IsZero() {
		a.Created = now
	}
	a.Updated = now
	if a.Status == "" {
		a.Status = StatusOK
	}
	ops := []Op{
		{Kind: OpPut, Cond: Cond{Kind: CondAbsent}, Item: Item{
			PK: ownerPK(a.Owner), SK: acctSK(a.ID), G1: addrKey(a.Owner, a.Address), Data: encode(a)}},
		{Kind: OpPut, Cond: Cond{Kind: CondAbsent}, Item: Item{
			PK: ownerPK(a.Owner), SK: nickSK(slug), Data: encode(pointer{a.ID})}},
	}
	if a.IsDefault {
		ops = append(ops, Op{Kind: OpPut, Item: Item{PK: ownerPK(a.Owner), SK: defaultSK, Data: encode(pointer{a.ID})}})
	}
	err = s.b.Transact(ctx, ops)
	if i, failed := condIndex(err); failed {
		if i == 0 {
			return ErrConflict
		}
		return ErrNicknameTaken
	}
	return err
}

func (s *impl) UpdateAccount(ctx context.Context, a Account) error {
	display, slug, err := NormalizeNickname(a.Nickname)
	if err != nil {
		return err
	}
	for range 10 {
		old, it, err := load[Account](ctx, s.b, ownerPK(a.Owner), acctSK(a.ID))
		if err != nil {
			return err
		}
		next := a
		next.Owner, next.ID, next.Created = old.Owner, old.ID, old.Created
		next.Nickname = display
		next.Updated = s.now().UTC()
		it.Data = encode(next)
		it.G1 = addrKey(next.Owner, next.Address)
		it.Ver++
		ops := []Op{{Kind: OpPut, Item: it, Cond: Cond{Kind: CondVer, Ver: it.Ver - 1}}}
		_, oldSlug, _ := NormalizeNickname(old.Nickname)
		if oldSlug != slug {
			ops = append(ops,
				Op{Kind: OpPut, Cond: Cond{Kind: CondAbsent}, Item: Item{PK: ownerPK(a.Owner), SK: nickSK(slug), Data: encode(pointer{a.ID})}},
				Op{Kind: OpDelete, Item: Item{PK: ownerPK(a.Owner), SK: nickSK(oldSlug)}})
		}
		err = s.b.Transact(ctx, ops)
		if i, failed := condIndex(err); failed {
			if i == 1 {
				return ErrNicknameTaken
			}
			continue // lost the version race; reload
		}
		return err
	}
	return fmt.Errorf("%w: too much contention on account %s", ErrConflict, a.ID)
}

func (s *impl) defaultID(ctx context.Context, owner string) (string, error) {
	p, _, err := load[pointer](ctx, s.b, ownerPK(owner), defaultSK)
	if errors.Is(err, ErrNotFound) {
		return "", nil
	}
	return p.AccountID, err
}

func (s *impl) GetAccount(ctx context.Context, owner, id string) (Account, error) {
	a, _, err := load[Account](ctx, s.b, ownerPK(owner), acctSK(id))
	if err != nil {
		return Account{}, err
	}
	def, err := s.defaultID(ctx, owner)
	if err != nil {
		return Account{}, err
	}
	a.IsDefault = def == a.ID
	return a, nil
}

func (s *impl) ListAccounts(ctx context.Context, owner string) ([]Account, error) {
	items, err := s.b.Query(ctx, ownerPK(owner), "")
	if err != nil {
		return nil, err
	}
	var accts []Account
	var def string
	for _, it := range items {
		switch {
		case strings.HasPrefix(it.SK, "ACCT#"):
			a, err := decode[Account](it)
			if err != nil {
				return nil, err
			}
			accts = append(accts, a)
		case it.SK == defaultSK:
			p, err := decode[pointer](it)
			if err != nil {
				return nil, err
			}
			def = p.AccountID
		}
	}
	for i := range accts {
		accts[i].IsDefault = accts[i].ID == def
	}
	sort.Slice(accts, func(i, j int) bool {
		if !accts[i].Created.Equal(accts[j].Created) {
			return accts[i].Created.Before(accts[j].Created)
		}
		return accts[i].ID < accts[j].ID
	})
	return accts, nil
}

func (s *impl) FindAccountByAddress(ctx context.Context, owner, address string) (Account, error) {
	items, err := s.b.QueryIndex(ctx, addrKey(owner, address))
	if err != nil {
		return Account{}, err
	}
	for _, it := range items {
		if it.PK != ownerPK(owner) {
			continue
		}
		a, err := decode[Account](it)
		if err != nil {
			return Account{}, err
		}
		def, err := s.defaultID(ctx, owner)
		if err != nil {
			return Account{}, err
		}
		a.IsDefault = def == a.ID
		return a, nil
	}
	return Account{}, ErrNotFound
}

func (s *impl) SetDefaultAccount(ctx context.Context, owner, id string) error {
	if _, _, err := load[Account](ctx, s.b, ownerPK(owner), acctSK(id)); err != nil {
		return err
	}
	return s.b.Transact(ctx, []Op{{Kind: OpPut, Item: Item{PK: ownerPK(owner), SK: defaultSK, Data: encode(pointer{id})}}})
}

func (s *impl) DisconnectAccount(ctx context.Context, owner, id string, now time.Time) error {
	_, err := modify(ctx, s.b, ownerPK(owner), acctSK(id), func(a *Account) error {
		a.RefreshToken = nil
		a.SendEnabled = false
		a.Status = StatusDisconnected
		a.LastError = ""
		a.Updated = now.UTC()
		return nil
	})
	return err
}

// ---- OAuth clients, codes, sessions, refresh families ----

func (s *impl) CreateOAuthClient(ctx context.Context, c OAuthClient) error {
	err := s.b.Transact(ctx, []Op{{Kind: OpPut, Cond: Cond{Kind: CondAbsent},
		Item: Item{PK: "CLIENT#" + c.ID, SK: "META", Data: encode(c)}}})
	if _, failed := condIndex(err); failed {
		return ErrConflict
	}
	return err
}

func (s *impl) GetOAuthClient(ctx context.Context, id string) (OAuthClient, error) {
	c, _, err := load[OAuthClient](ctx, s.b, "CLIENT#"+id, "META")
	return c, err
}

func (s *impl) CreateAuthCode(ctx context.Context, c AuthCode) error {
	err := s.b.Transact(ctx, []Op{{Kind: OpPut, Cond: Cond{Kind: CondAbsent}, Item: Item{
		PK: "CODE#" + c.Hash, SK: "META", Data: encode(c), TTL: secs(c.Expires.Add(AuthCodeRetention))}}})
	if _, failed := condIndex(err); failed {
		return ErrConflict
	}
	return err
}

func (s *impl) RedeemAuthCode(ctx context.Context, hash string, now time.Time) (AuthCode, error) {
	return modify(ctx, s.b, "CODE#"+hash, "META", func(c *AuthCode) error {
		if c.RedeemedAt != nil {
			return ErrAlreadyRedeemed
		}
		if now.After(c.Expires) {
			return ErrExpired
		}
		t := now.UTC()
		c.RedeemedAt = &t
		return nil
	})
}

func famSessionSK(hash string) string { return "SESS#" + hash }

func (s *impl) CreateSession(ctx context.Context, sess Session) error {
	ops := []Op{{Kind: OpPut, Cond: Cond{Kind: CondAbsent}, Item: Item{
		PK: "SESS#" + sess.Hash, SK: "META", Data: encode(sess), TTL: secs(sess.Expires)}}}
	if sess.FamilyID != "" {
		ops = append(ops, Op{Kind: OpPut, Item: Item{
			PK: "FAM#" + sess.FamilyID, SK: famSessionSK(sess.Hash), Data: []byte("{}"), TTL: secs(sess.Expires)}})
	}
	err := s.b.Transact(ctx, ops)
	if _, failed := condIndex(err); failed {
		return ErrConflict
	}
	return err
}

func (s *impl) GetSession(ctx context.Context, hash string) (Session, error) {
	v, _, err := load[Session](ctx, s.b, "SESS#"+hash, "META")
	return v, err
}

func (s *impl) RevokeSession(ctx context.Context, hash string) error {
	_, err := modify(ctx, s.b, "SESS#"+hash, "META", func(v *Session) error {
		if v.Revoked {
			return errSkip
		}
		v.Revoked = true
		return nil
	})
	return err
}

func (s *impl) CreateRefreshFamily(ctx context.Context, f RefreshFamily) error {
	err := s.b.Transact(ctx, []Op{{Kind: OpPut, Cond: Cond{Kind: CondAbsent}, Item: Item{
		PK: "FAM#" + f.ID, SK: "META", Data: encode(f), TTL: secs(f.AbsExpires.Add(24 * time.Hour))}}})
	if _, failed := condIndex(err); failed {
		return ErrConflict
	}
	return err
}

func (s *impl) GetRefreshFamily(ctx context.Context, id string) (RefreshFamily, error) {
	f, _, err := load[RefreshFamily](ctx, s.b, "FAM#"+id, "META")
	return f, err
}

func (s *impl) RotateRefresh(ctx context.Context, familyID, presentedHash, newHash string, now time.Time) (RotateOutcome, error) {
	return s.RotateRefreshWithPair(ctx, familyID, presentedHash, newHash, nil, now)
}

func (s *impl) RotateRefreshWithPair(ctx context.Context, familyID, presentedHash, newHash string, pair []byte, now time.Time) (RotateOutcome, error) {
	if presentedHash == "" || newHash == "" {
		return "", ErrNotFound
	}
	var out RotateOutcome
	_, err := modify(ctx, s.b, "FAM#"+familyID, "META", func(f *RefreshFamily) error {
		out = rotate(f, presentedHash, newHash, pair, now.UTC())
		if out == OutcomeRevoked || out == OutcomeExpired {
			return errSkip
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	if out == OutcomeReused {
		if err := s.revokeSessions(ctx, familyID); err != nil {
			return out, err
		}
	}
	return out, nil
}

// rotate decides and applies one refresh presentation to f in memory.
func rotate(f *RefreshFamily, presented, newHash string, pair []byte, now time.Time) RotateOutcome {
	switch {
	case f.Revoked:
		return OutcomeRevoked
	case now.After(f.IdleExpires) || now.After(f.AbsExpires):
		return OutcomeExpired
	case presented == f.CurrentHash:
		until := now.Add(RefreshGrace)
		f.PrevHash, f.PrevUntil, f.GraceUsed = f.CurrentHash, &until, false
		f.CurrentHash, f.GracePair = newHash, pair
		f.LastRotated = now
		f.IdleExpires = now.Add(RefreshIdleTTL)
		if f.IdleExpires.After(f.AbsExpires) {
			f.IdleExpires = f.AbsExpires
		}
		return OutcomeRotated
	case presented == f.PrevHash && f.PrevUntil != nil && !now.After(*f.PrevUntil) && !f.GraceUsed:
		f.GraceUsed = true
		return OutcomeGraceReplay
	default:
		f.Revoked, f.GracePair = true, nil
		return OutcomeReused
	}
}

func (s *impl) RevokeFamily(ctx context.Context, familyID string) error {
	if _, err := modify(ctx, s.b, "FAM#"+familyID, "META", func(f *RefreshFamily) error {
		if f.Revoked {
			return errSkip
		}
		f.Revoked = true
		return nil
	}); err != nil && !errors.Is(err, ErrNotFound) {
		return err
	}
	return s.revokeSessions(ctx, familyID)
}

func (s *impl) revokeSessions(ctx context.Context, familyID string) error {
	idx, err := s.b.Query(ctx, "FAM#"+familyID, "SESS#")
	if err != nil {
		return err
	}
	for _, it := range idx {
		if err := s.RevokeSession(ctx, strings.TrimPrefix(it.SK, "SESS#")); err != nil && !errors.Is(err, ErrNotFound) {
			return err
		}
	}
	return nil
}

// ---- connect states ----

func (s *impl) PutConnectState(ctx context.Context, c ConnectState) error {
	err := s.b.Transact(ctx, []Op{{Kind: OpPut, Cond: Cond{Kind: CondAbsent}, Item: Item{
		PK: "CSTATE#" + c.Hash, SK: "META", Data: encode(c), TTL: secs(c.Expires.Add(time.Hour))}}})
	if _, failed := condIndex(err); failed {
		return ErrConflict
	}
	return err
}

func (s *impl) TakeConnectState(ctx context.Context, hash string, now time.Time) (ConnectState, error) {
	c, it, err := load[ConnectState](ctx, s.b, "CSTATE#"+hash, "META")
	if err != nil {
		return ConnectState{}, err
	}
	err = s.b.Transact(ctx, []Op{{Kind: OpDelete, Item: it, Cond: Cond{Kind: CondVer, Ver: it.Ver}}})
	if _, failed := condIndex(err); failed {
		return ConnectState{}, ErrNotFound
	}
	if err != nil {
		return ConnectState{}, err
	}
	if now.After(c.Expires) {
		return ConnectState{}, ErrNotFound
	}
	return c, nil
}

// ---- authorize transactions ----

func (s *impl) PutAuthTxn(ctx context.Context, t AuthTxn) error {
	err := s.b.Transact(ctx, []Op{{Kind: OpPut, Cond: Cond{Kind: CondAbsent}, Item: Item{
		PK: "ATXN#" + t.Hash, SK: "META", Data: encode(t), TTL: secs(t.Expires.Add(time.Hour))}}})
	if _, failed := condIndex(err); failed {
		return ErrConflict
	}
	return err
}

func (s *impl) GetAuthTxn(ctx context.Context, hash string, now time.Time) (AuthTxn, error) {
	t, _, err := load[AuthTxn](ctx, s.b, "ATXN#"+hash, "META")
	if err != nil {
		return AuthTxn{}, err
	}
	if now.After(t.Expires) {
		return AuthTxn{}, ErrNotFound
	}
	return t, nil
}

func (s *impl) TakeAuthTxn(ctx context.Context, hash string, now time.Time) (AuthTxn, error) {
	t, it, err := load[AuthTxn](ctx, s.b, "ATXN#"+hash, "META")
	if err != nil {
		return AuthTxn{}, err
	}
	err = s.b.Transact(ctx, []Op{{Kind: OpDelete, Item: it, Cond: Cond{Kind: CondVer, Ver: it.Ver}}})
	if _, failed := condIndex(err); failed {
		return AuthTxn{}, ErrNotFound
	}
	if err != nil {
		return AuthTxn{}, err
	}
	if now.After(t.Expires) {
		return AuthTxn{}, ErrNotFound
	}
	return t, nil
}

// ---- approvals ----

func approvalPK(id string) string { return "APPR#" + id }
func idemPK(key string) string    { return "IDEM#" + key }

func (s *impl) CreateApproval(ctx context.Context, a Approval, now time.Time) (Approval, bool, error) {
	if a.ID == "" || a.IdempotencyKey == "" {
		return Approval{}, false, errors.New("emailstore: approval needs id and idempotency key")
	}
	if a.Status == "" {
		a.Status = ApprovalPending
	}
	if a.Created.IsZero() {
		a.Created = now.UTC()
	}
	ttl := secs(now.Add(ApprovalTTL))
	err := s.b.Transact(ctx, []Op{
		{Kind: OpPut, Cond: Cond{Kind: CondAbsent}, Item: Item{PK: approvalPK(a.ID), SK: "META", G1: approvalIndex(a.Kind, a.AccountID), Data: encode(a), TTL: ttl}},
		{Kind: OpPut, Cond: Cond{Kind: CondAbsent}, Item: Item{PK: idemPK(a.IdempotencyKey), SK: "META", Data: encode(pointer{a.ID}), TTL: ttl}},
	})
	if i, failed := condIndex(err); failed {
		if i == 0 {
			return Approval{}, false, ErrConflict
		}
		p, _, err := load[pointer](ctx, s.b, idemPK(a.IdempotencyKey), "META")
		if err != nil {
			return Approval{}, false, err
		}
		got, err := s.GetApproval(ctx, p.AccountID)
		return got, false, err
	}
	if err != nil {
		return Approval{}, false, err
	}
	a.TTL = ttl
	return a, true, nil
}

// approvalIndex is the secondary-index key that finds an account's approvals of
// one kind. Approvals without an account are not indexed.
func approvalIndex(kind, accountID string) string {
	if accountID == "" {
		return ""
	}
	return "APPRACCT#" + accountID + "#" + kind
}

func (s *impl) FindApprovals(ctx context.Context, kind, accountID string) ([]Approval, error) {
	if accountID == "" {
		return nil, nil
	}
	items, err := s.b.QueryIndex(ctx, approvalIndex(kind, accountID))
	if err != nil {
		return nil, err
	}
	out := make([]Approval, 0, len(items))
	for _, it := range items {
		if !strings.HasPrefix(it.PK, "APPR#") || it.SK != "META" {
			continue
		}
		a, err := decode[Approval](it)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Created.After(out[j].Created) })
	return out, nil
}

func (s *impl) FindApprovalByKey(ctx context.Context, key string) (Approval, error) {
	p, _, err := load[pointer](ctx, s.b, idemPK(key), "META")
	if err != nil {
		return Approval{}, err
	}
	return s.GetApproval(ctx, p.AccountID)
}

func (s *impl) GetApproval(ctx context.Context, id string) (Approval, error) {
	a, _, err := load[Approval](ctx, s.b, approvalPK(id), "META")
	return a, err
}

func (s *impl) TransitionApproval(ctx context.Context, id string, from []string, to string, clearSnapshot bool) (Approval, error) {
	return modify(ctx, s.b, approvalPK(id), "META", func(a *Approval) error {
		ok := false
		for _, f := range from {
			ok = ok || a.Status == f
		}
		if !ok {
			return fmt.Errorf("%w: %s -> %s", ErrInvalidTransition, a.Status, to)
		}
		a.Status = to
		if clearSnapshot {
			a.Snapshot = nil
		}
		return nil
	})
}

// ---- send ledger and hourly cap ----

func sendPK(requestID string) string { return "SEND#" + requestID }
func capPK(accountID string) string  { return "ACCT#" + accountID }
func capSK(t time.Time) string       { return "CAP#" + HourKey(t) }

// capOp returns the counter op for the hour containing t. The counter row
// outlives its hour by CapRetention.
func capOp(accountID string, t time.Time, delta, max int64) Op {
	hourEnd := t.UTC().Truncate(time.Hour).Add(time.Hour)
	return Op{Kind: OpIncr, Delta: delta, Max: max,
		Item: Item{PK: capPK(accountID), SK: capSK(t), TTL: secs(hourEnd.Add(CapRetention))}}
}

func (s *impl) ClaimSend(ctx context.Context, row SendRow, capLimit int) error {
	if row.RequestID == "" || row.AccountID == "" {
		return errors.New("emailstore: send row needs request id and account id")
	}
	if capLimit <= 0 {
		return ErrRateLimited
	}
	now := s.now().UTC()
	if row.Created.IsZero() {
		row.Created = now
	}
	row.State, row.Updated = SendClaimed, row.Created
	row.Hour = HourKey(row.Created)
	err := s.b.Transact(ctx, []Op{
		{Kind: OpPut, Cond: Cond{Kind: CondAbsent}, Item: Item{
			PK: sendPK(row.RequestID), SK: "META", Data: encode(row), TTL: secs(row.Created.Add(LedgerTTL))}},
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

func (s *impl) GetSend(ctx context.Context, requestID string) (SendRow, error) {
	r, _, err := load[SendRow](ctx, s.b, sendPK(requestID), "META")
	return r, err
}

func (s *impl) AdvanceSend(ctx context.Context, requestID, from, to, providerMessageID string, now time.Time) (SendRow, error) {
	return modify(ctx, s.b, sendPK(requestID), "META", func(r *SendRow) error {
		if r.State != from {
			return fmt.Errorf("%w: ledger row is %s, not %s", ErrConflict, r.State, from)
		}
		r.State, r.Updated = to, now.UTC()
		if providerMessageID != "" {
			r.ProviderMessageID = providerMessageID
		}
		return nil
	})
}

func (s *impl) FailSend(ctx context.Context, requestID, from string, now time.Time) error {
	for range 10 {
		row, it, err := load[SendRow](ctx, s.b, sendPK(requestID), "META")
		if err != nil {
			return err
		}
		if row.State != from {
			return fmt.Errorf("%w: ledger row is %s, not %s", ErrConflict, row.State, from)
		}
		hour, err := time.Parse("2006010215", row.Hour)
		if err != nil {
			return fmt.Errorf("emailstore: ledger hour %q: %w", row.Hour, err)
		}
		row.State, row.Updated = SendFailed, now.UTC()
		next := it
		next.Data, next.Ver = encode(row), it.Ver+1
		err = s.b.Transact(ctx, []Op{
			{Kind: OpPut, Item: next, Cond: Cond{Kind: CondVer, Ver: it.Ver}},
			capOp(row.AccountID, hour, -1, 0),
		})
		if i, failed := condIndex(err); failed {
			if i == 0 {
				continue
			}
			return fmt.Errorf("%w: cap counter for %s was already empty", ErrConflict, row.Hour)
		}
		return err
	}
	return fmt.Errorf("%w: too much contention on send %s", ErrConflict, requestID)
}

func (s *impl) ReserveCap(ctx context.Context, accountID string, t time.Time, limit int) error {
	if limit <= 0 {
		return ErrRateLimited
	}
	err := s.b.Transact(ctx, []Op{capOp(accountID, t, 1, int64(limit))})
	if _, failed := condIndex(err); failed {
		return ErrRateLimited
	}
	return err
}

func (s *impl) ReleaseCap(ctx context.Context, accountID string, t time.Time) error {
	err := s.b.Transact(ctx, []Op{capOp(accountID, t, -1, 0)})
	if _, failed := condIndex(err); failed {
		return nil // already empty; releasing twice is harmless
	}
	return err
}

func (s *impl) HourlyCount(ctx context.Context, accountID string, t time.Time) (int, error) {
	it, ok, err := s.b.Get(ctx, capPK(accountID), capSK(t))
	if err != nil || !ok {
		return 0, err
	}
	return int(it.N), nil
}

// ---- contacts ----

func contactSK(address string) string {
	return "CONTACT#" + strings.ToLower(strings.TrimSpace(address))
}

func (s *impl) PutContact(ctx context.Context, c Contact) error {
	c.Address = strings.ToLower(strings.TrimSpace(c.Address))
	if c.AccountID == "" || c.Address == "" {
		return errors.New("emailstore: contact needs account id and address")
	}
	switch c.Trust {
	case "":
		c.Trust = TrustNew
	case TrustNew, TrustKnown, TrustTrusted, TrustBlocked:
	default:
		return fmt.Errorf("emailstore: unknown trust %q", c.Trust)
	}
	if c.Updated.IsZero() {
		c.Updated = s.now().UTC()
	}
	return s.b.Transact(ctx, []Op{{Kind: OpPut, Item: Item{PK: capPK(c.AccountID), SK: contactSK(c.Address), Data: encode(c)}}})
}

func (s *impl) GetContact(ctx context.Context, accountID, address string) (Contact, error) {
	c, _, err := load[Contact](ctx, s.b, capPK(accountID), contactSK(address))
	return c, err
}

func (s *impl) ListContacts(ctx context.Context, accountID string, f ContactFilter) ([]Contact, error) {
	items, err := s.b.Query(ctx, capPK(accountID), "CONTACT#")
	if err != nil {
		return nil, err
	}
	var out []Contact
	for _, it := range items {
		c, err := decode[Contact](it)
		if err != nil {
			return nil, err
		}
		if f.Trust != "" && c.Trust != f.Trust {
			continue
		}
		if f.Tag != "" && !hasTag(c.Tags, f.Tag) {
			continue
		}
		out = append(out, c)
	}
	return out, nil
}

func hasTag(tags []string, tag string) bool {
	for _, t := range tags {
		if t == tag {
			return true
		}
	}
	return false
}

// ---- audit ----

const (
	dayLayout = "2006-01-02"
	// auditSKTime is fixed width so sort keys order by time.
	auditSKTime = "2006-01-02T15:04:05.000000000Z"
)

func auditPK(owner string, t time.Time) string {
	return "AUDIT#" + owner + "#" + t.UTC().Format(dayLayout)
}

func (s *impl) AppendAudit(ctx context.Context, e AuditEvent) error {
	if e.At.IsZero() {
		e.At = s.now().UTC()
	}
	e.At = e.At.UTC()
	sk := e.At.Format(auditSKTime) + "#" + e.RequestID
	return s.b.Transact(ctx, []Op{{Kind: OpPut, Item: Item{
		PK: auditPK(e.Owner, e.At), SK: sk, Data: encode(e), TTL: secs(e.At.Add(AuditTTL))}}})
}

func (s *impl) ListAudit(ctx context.Context, owner string, since time.Time, limit int) ([]AuditEvent, error) {
	if limit <= 0 {
		limit = 100
	}
	now := s.now().UTC()
	if floor := now.Add(-AuditTTL); since.Before(floor) {
		since = floor
	}
	var out []AuditEvent
	for day := now.Truncate(24 * time.Hour); !day.Before(since.UTC().Truncate(24 * time.Hour)); day = day.Add(-24 * time.Hour) {
		items, err := s.b.Query(ctx, auditPK(owner, day), "")
		if err != nil {
			return nil, err
		}
		for i := len(items) - 1; i >= 0; i-- { // newest first within the day
			e, err := decode[AuditEvent](items[i])
			if err != nil {
				return nil, err
			}
			if e.At.Before(since) {
				continue
			}
			out = append(out, e)
			if len(out) >= limit {
				return out, nil
			}
		}
	}
	return out, nil
}

// ---- draft notes ----

func draftSK(draftID string) string { return "DRAFT#" + draftID }

func (s *impl) PutDraftMeta(ctx context.Context, m DraftMeta) error {
	if m.AccountID == "" || m.DraftID == "" {
		return errors.New("emailstore: draft meta needs account id and draft id")
	}
	if m.Updated.IsZero() {
		m.Updated = s.now().UTC()
	}
	return s.b.Transact(ctx, []Op{{Kind: OpPut, Item: Item{
		PK: capPK(m.AccountID), SK: draftSK(m.DraftID), Data: encode(m), TTL: secs(m.Updated.Add(DraftMetaTTL))}}})
}

func (s *impl) GetDraftMeta(ctx context.Context, accountID, draftID string) (DraftMeta, error) {
	m, _, err := load[DraftMeta](ctx, s.b, capPK(accountID), draftSK(draftID))
	return m, err
}

// ---- settings ----

const (
	settingsPK = "SETTINGS"
	killSK     = "KILL"
)

func (s *impl) SetKillSwitch(ctx context.Context, on bool, reason string, now time.Time) error {
	st := KillState{On: on, Reason: reason, At: now.UTC()}
	return s.b.Transact(ctx, []Op{{Kind: OpPut, Item: Item{PK: settingsPK, SK: killSK, Data: encode(st)}}})
}

func (s *impl) KillSwitch(ctx context.Context) (KillState, error) {
	st, _, err := load[KillState](ctx, s.b, settingsPK, killSK)
	if errors.Is(err, ErrNotFound) {
		return KillState{}, nil
	}
	return st, err
}
