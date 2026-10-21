// Package sqlite implements store.Store on a local SQLite file using the pure-Go
// modernc.org/sqlite driver. Times are stored as Unix milliseconds UTC.
package sqlite

import (
	"context"
	"database/sql"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite" // registers the "sqlite" driver

	"github.com/change-fabric/moose/internal/store"
)

//go:embed schema.sql
var schema string

const pragmas = "_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)"

// Store is a SQLite-backed store.Store.
type Store struct {
	db        *sql.DB
	closeOnce sync.Once
	closeErr  error
}

var _ store.Store = (*Store)(nil)

// Open opens (creating if needed) the database at path and applies the schema.
func Open(ctx context.Context, path string) (*Store, error) {
	sep := "?"
	if strings.Contains(path, "?") {
		sep = "&"
	}
	db, err := sql.Open("sqlite", "file:"+path+sep+pragmas)
	if err != nil {
		return nil, fmt.Errorf("sqlite: open: %w", err)
	}
	if _, err := db.ExecContext(ctx, schema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("sqlite: apply schema: %w", err)
	}
	return &Store{db: db}, nil
}

func ms(t time.Time) int64 { return t.UTC().UnixMilli() }

func fromMS(v int64) time.Time { return time.UnixMilli(v).UTC() }

func nullMS(t *time.Time) sql.NullInt64 {
	if t == nil {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: ms(*t), Valid: true}
}

func ptrMS(v sql.NullInt64) *time.Time {
	if !v.Valid {
		return nil
	}
	t := fromMS(v.Int64)
	return &t
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

func (s *Store) PutUser(ctx context.Context, u store.User) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO users (login, role, created_at, disabled) VALUES (?, ?, ?, ?)
		 ON CONFLICT(login) DO UPDATE SET role = excluded.role, created_at = excluded.created_at, disabled = excluded.disabled`,
		u.Login, u.Role, ms(u.Created), b2i(u.Disabled))
	return err
}

func scanUser(sc interface{ Scan(...any) error }) (store.User, error) {
	var u store.User
	var created int64
	var disabled int
	if err := sc.Scan(&u.Login, &u.Role, &created, &disabled); err != nil {
		return store.User{}, err
	}
	u.Created, u.Disabled = fromMS(created), disabled != 0
	return u, nil
}

func (s *Store) GetUser(ctx context.Context, login string) (store.User, error) {
	u, err := scanUser(s.db.QueryRowContext(ctx,
		`SELECT login, role, created_at, disabled FROM users WHERE login = ?`, login))
	if errors.Is(err, sql.ErrNoRows) {
		return store.User{}, store.ErrNotFound
	}
	return u, err
}

func (s *Store) ListUsers(ctx context.Context) ([]store.User, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT login, role, created_at, disabled FROM users ORDER BY login`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []store.User
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

func (s *Store) CreateToken(ctx context.Context, t store.Token) error {
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO tokens (id, owner, name, hash, created_at, last_used_at, expires_at, revoked)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT(id) DO NOTHING`,
		t.ID, t.Owner, t.Name, t.Hash, ms(t.Created), nullMS(t.LastUsed), nullMS(t.Expires), b2i(t.Revoked))
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n == 0 {
		return store.ErrConflict
	}
	return nil
}

const tokenCols = `id, owner, name, hash, created_at, last_used_at, expires_at, revoked`

func scanToken(sc interface{ Scan(...any) error }) (store.Token, error) {
	var t store.Token
	var created int64
	var lastUsed, expires sql.NullInt64
	var revoked int
	if err := sc.Scan(&t.ID, &t.Owner, &t.Name, &t.Hash, &created, &lastUsed, &expires, &revoked); err != nil {
		return store.Token{}, err
	}
	t.Created, t.LastUsed, t.Expires, t.Revoked = fromMS(created), ptrMS(lastUsed), ptrMS(expires), revoked != 0
	return t, nil
}

func (s *Store) GetToken(ctx context.Context, id string) (store.Token, error) {
	t, err := scanToken(s.db.QueryRowContext(ctx, `SELECT `+tokenCols+` FROM tokens WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return store.Token{}, store.ErrNotFound
	}
	return t, err
}

func (s *Store) ListTokens(ctx context.Context, owner string) ([]store.Token, error) {
	q, args := `SELECT `+tokenCols+` FROM tokens`, []any{}
	if owner != "" {
		q, args = q+` WHERE owner = ?`, append(args, owner)
	}
	rows, err := s.db.QueryContext(ctx, q+` ORDER BY created_at, id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []store.Token
	for rows.Next() {
		t, err := scanToken(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *Store) execOne(ctx context.Context, q string, args ...any) error {
	res, err := s.db.ExecContext(ctx, q, args...)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return store.ErrNotFound
	}
	return nil
}

func (s *Store) RevokeToken(ctx context.Context, id string) error {
	return s.execOne(ctx, `UPDATE tokens SET revoked = 1 WHERE id = ?`, id)
}

func (s *Store) TouchToken(ctx context.Context, id string, at time.Time) error {
	return s.execOne(ctx, `UPDATE tokens SET last_used_at = ? WHERE id = ?`, ms(at), id)
}

func (s *Store) GetConnectorConfig(ctx context.Context, id string) (map[string]string, error) {
	var raw string
	err := s.db.QueryRowContext(ctx, `SELECT json FROM connector_config WHERE id = ?`, id).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	cfg := map[string]string{}
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		return nil, fmt.Errorf("sqlite: decode connector config %q: %w", id, err)
	}
	return cfg, nil
}

func (s *Store) PutConnectorConfig(ctx context.Context, id string, cfg map[string]string) error {
	raw, err := json.Marshal(cfg)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO connector_config (id, json, updated_at) VALUES (?, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET json = excluded.json, updated_at = excluded.updated_at`,
		id, string(raw), ms(time.Now()))
	return err
}

func (s *Store) AppendAudit(ctx context.Context, e store.AuditEvent) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO audit (at, request_id, actor, token_id, surface, action, source, doc_id, status, latency_ms, detail)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		ms(e.At), e.RequestID, e.Actor, e.TokenID, e.Surface, e.Action, e.Source, e.DocID, e.Status, e.LatencyMS, e.Detail)
	return err
}

func (s *Store) ListAudit(ctx context.Context, since time.Time, limit int) ([]store.AuditEvent, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT at, request_id, COALESCE(actor,''), COALESCE(token_id,''), COALESCE(surface,''), COALESCE(action,''),
		        COALESCE(source,''), COALESCE(doc_id,''), COALESCE(status,0), COALESCE(latency_ms,0), COALESCE(detail,'')
		 FROM audit WHERE at >= ? ORDER BY at DESC, seq DESC LIMIT ?`, ms(since), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []store.AuditEvent
	for rows.Next() {
		var e store.AuditEvent
		var at int64
		if err := rows.Scan(&at, &e.RequestID, &e.Actor, &e.TokenID, &e.Surface, &e.Action,
			&e.Source, &e.DocID, &e.Status, &e.LatencyMS, &e.Detail); err != nil {
			return nil, err
		}
		e.At = fromMS(at)
		out = append(out, e)
	}
	return out, rows.Err()
}

// Close closes the database. Calling it more than once is safe.
func (s *Store) Close() error {
	s.closeOnce.Do(func() { s.closeErr = s.db.Close() })
	return s.closeErr
}
