package store

import (
	"context"
	"errors"
	"strconv"
	"time"
)

// Item is one row of the single-table layout shared by every backend.
//
// PK and SK form the primary key. G1 is the key of the sparse secondary index
// (empty means not indexed). Data holds the JSON record. Ver is an optimistic
// version used by CondVer writes. N is a counter used by OpIncr. TTL is when
// the backend may delete the row; the zero time means never. Backends keep TTL
// at whole-second precision.
type Item struct {
	PK, SK, G1 string
	Data       []byte
	Ver        int64
	N          int64
	TTL        time.Time
}

// CondKind selects a write condition.
type CondKind int

const (
	// CondNone writes unconditionally.
	CondNone CondKind = iota
	// CondAbsent requires that no row exists at the key.
	CondAbsent
	// CondVer requires that the row exists with Ver equal to Cond.Ver.
	CondVer
)

// Cond is a write condition on the existing row at the op's key.
type Cond struct {
	Kind CondKind
	Ver  int64
}

// OpKind selects what an Op does.
type OpKind int

const (
	// OpPut writes Item, subject to Cond.
	OpPut OpKind = iota
	// OpDelete removes the row at Item.PK/Item.SK, subject to Cond.
	OpDelete
	// OpIncr adds Delta to the counter N of the row at Item.PK/Item.SK,
	// creating it at zero if absent, and sets its TTL from Item.TTL. A positive
	// Delta with Max > 0 requires the result to stay <= Max. A negative Delta
	// requires the result to stay >= 0.
	OpIncr
)

// Op is one write in a transaction.
type Op struct {
	Kind  OpKind
	Item  Item
	Cond  Cond
	Delta int64
	Max   int64
}

// CondError reports that a transaction aborted because the condition of the op
// at Index failed. No op of the transaction was applied.
type CondError struct{ Index int }

func (e *CondError) Error() string {
	return "emailstore: condition failed on op " + strconv.Itoa(e.Index)
}

// condIndex returns the failing op index when err is a *CondError.
func condIndex(err error) (int, bool) {
	var ce *CondError
	if errors.As(err, &ce) {
		return ce.Index, true
	}
	return 0, false
}

// Backend is the storage engine contract. Every method is strongly consistent
// except QueryIndex, which may lag on DynamoDB.
type Backend interface {
	// Get returns the row at the key; ok is false if absent.
	Get(ctx context.Context, pk, sk string) (item Item, ok bool, err error)
	// Query returns the rows of pk whose sk starts with skPrefix, ascending by sk.
	Query(ctx context.Context, pk, skPrefix string) ([]Item, error)
	// QueryIndex returns the rows whose G1 equals g1.
	QueryIndex(ctx context.Context, g1 string) ([]Item, error)
	// Transact applies ops atomically: all or none. A failed condition returns
	// *CondError naming the first failing op.
	Transact(ctx context.Context, ops []Op) error
	Close() error
}
