// Package sqlite is the SQLite storage backend for the email store, used for
// local development and tests. It holds the same single-table layout as the
// DynamoDB backend. Writes are serialized by a mutex, so every transaction is
// all-or-nothing and conditions are evaluated against a stable view.
package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite" // registers the "sqlite" driver

	"github.com/change-fabric/moose/internal/email/store"
)

const schema = `
CREATE TABLE IF NOT EXISTS email_items (
  pk   TEXT NOT NULL,
  sk   TEXT NOT NULL,
  g1   TEXT,
  data BLOB,
  ver  INTEGER NOT NULL DEFAULT 0,
  n    INTEGER NOT NULL DEFAULT 0,
  ttl  INTEGER,
  PRIMARY KEY (pk, sk)
) WITHOUT ROWID;
CREATE INDEX IF NOT EXISTS email_items_g1 ON email_items(g1) WHERE g1 IS NOT NULL;
CREATE INDEX IF NOT EXISTS email_items_ttl ON email_items(ttl) WHERE ttl IS NOT NULL;
`

const pragmas = "_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)"

// Backend is a SQLite store.Backend.
type Backend struct {
	db *sql.DB
	mu sync.Mutex
}

var _ store.Backend = (*Backend)(nil)

// Open opens (creating if needed) the database at path and applies the schema.
func Open(ctx context.Context, path string) (*Backend, error) {
	sep := "?"
	if strings.Contains(path, "?") {
		sep = "&"
	}
	db, err := sql.Open("sqlite", "file:"+path+sep+pragmas)
	if err != nil {
		return nil, fmt.Errorf("emailstore sqlite: open: %w", err)
	}
	if _, err := db.ExecContext(ctx, schema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("emailstore sqlite: apply schema: %w", err)
	}
	return &Backend{db: db}, nil
}

// OpenStore opens path and returns the email Store on it.
func OpenStore(ctx context.Context, path string) (store.Store, error) {
	b, err := Open(ctx, path)
	if err != nil {
		return nil, err
	}
	return store.New(b), nil
}

func (b *Backend) Close() error { return b.db.Close() }

const cols = "pk, sk, g1, data, ver, n, ttl"

type scanner interface{ Scan(...any) error }

func scanItem(sc scanner) (store.Item, error) {
	var it store.Item
	var g1 sql.NullString
	var ttl sql.NullInt64
	if err := sc.Scan(&it.PK, &it.SK, &g1, &it.Data, &it.Ver, &it.N, &ttl); err != nil {
		return store.Item{}, err
	}
	it.G1 = g1.String
	if ttl.Valid {
		it.TTL = time.Unix(ttl.Int64, 0).UTC()
	}
	return it, nil
}

func nullTTL(t time.Time) sql.NullInt64 {
	if t.IsZero() {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: t.Unix(), Valid: true}
}

func (b *Backend) Get(ctx context.Context, pk, sk string) (store.Item, bool, error) {
	it, err := scanItem(b.db.QueryRowContext(ctx, `SELECT `+cols+` FROM email_items WHERE pk = ? AND sk = ?`, pk, sk))
	if errors.Is(err, sql.ErrNoRows) {
		return store.Item{}, false, nil
	}
	return it, err == nil, err
}

func (b *Backend) query(ctx context.Context, q string, args ...any) ([]store.Item, error) {
	rows, err := b.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []store.Item
	for rows.Next() {
		it, err := scanItem(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

func (b *Backend) Query(ctx context.Context, pk, skPrefix string) ([]store.Item, error) {
	return b.query(ctx, `SELECT `+cols+` FROM email_items
		WHERE pk = ?1 AND substr(sk, 1, length(?2)) = ?2 ORDER BY sk`, pk, skPrefix)
}

func (b *Backend) QueryIndex(ctx context.Context, g1 string) ([]store.Item, error) {
	return b.query(ctx, `SELECT `+cols+` FROM email_items WHERE g1 = ? ORDER BY pk, sk`, g1)
}

// Transact applies ops in one SQL transaction, checking each op's condition
// against the state left by the ops before it.
func (b *Backend) Transact(ctx context.Context, ops []store.Op) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	tx, err := b.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for i, op := range ops {
		if err := apply(ctx, tx, op); err != nil {
			if errors.Is(err, errCond) {
				return &store.CondError{Index: i}
			}
			return err
		}
	}
	return tx.Commit()
}

var errCond = errors.New("condition failed")

func apply(ctx context.Context, tx *sql.Tx, op store.Op) error {
	var ver, n int64
	err := tx.QueryRowContext(ctx, `SELECT ver, n FROM email_items WHERE pk = ? AND sk = ?`, op.Item.PK, op.Item.SK).Scan(&ver, &n)
	exists := err == nil
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	switch op.Cond.Kind {
	case store.CondAbsent:
		if exists {
			return errCond
		}
	case store.CondVer:
		if !exists || ver != op.Cond.Ver {
			return errCond
		}
	}
	switch op.Kind {
	case store.OpPut:
		it := op.Item
		_, err = tx.ExecContext(ctx,
			`INSERT OR REPLACE INTO email_items (pk, sk, g1, data, ver, n, ttl) VALUES (?, ?, ?, ?, ?, ?, ?)`,
			it.PK, it.SK, sql.NullString{String: it.G1, Valid: it.G1 != ""}, it.Data, it.Ver, it.N, nullTTL(it.TTL))
		return err
	case store.OpDelete:
		_, err = tx.ExecContext(ctx, `DELETE FROM email_items WHERE pk = ? AND sk = ?`, op.Item.PK, op.Item.SK)
		return err
	case store.OpIncr:
		next := n + op.Delta
		if op.Delta > 0 && op.Max > 0 && next > op.Max {
			return errCond
		}
		if op.Delta < 0 && next < 0 {
			return errCond
		}
		_, err = tx.ExecContext(ctx,
			`INSERT INTO email_items (pk, sk, n, ttl) VALUES (?, ?, ?, ?)
			 ON CONFLICT(pk, sk) DO UPDATE SET n = excluded.n, ttl = excluded.ttl`,
			op.Item.PK, op.Item.SK, next, nullTTL(op.Item.TTL))
		return err
	}
	return fmt.Errorf("emailstore sqlite: unknown op kind %d", op.Kind)
}

// PurgeExpired deletes rows whose ttl is at or before now, emulating what
// DynamoDB's ttl sweeper does. It returns the number of rows removed.
func (b *Backend) PurgeExpired(ctx context.Context, now time.Time) (int64, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	res, err := b.db.ExecContext(ctx, `DELETE FROM email_items WHERE ttl IS NOT NULL AND ttl <= ?`, now.Unix())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
