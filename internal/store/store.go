// Package store defines the persistence interface for users, PATs, connector
// config, and the audit log. Implementations live in sqlite and dynamo and must
// pass the shared storetest conformance suite.
package store

import (
	"context"
	"errors"
	"time"
)

// User is a moose user keyed by tailnet login.
type User struct {
	Login    string
	Role     string // "admin" | "member"
	Created  time.Time
	Disabled bool
}

// Token is a personal access token record. Hash is sha256 of the full token.
type Token struct {
	ID       string
	Owner    string // tailnet login or tag:xyz
	Name     string
	Hash     []byte
	Created  time.Time
	LastUsed *time.Time
	Expires  *time.Time
	Revoked  bool
}

// AuditEvent records one request against any surface.
type AuditEvent struct {
	At                                                        time.Time
	RequestID, Actor, TokenID, Surface, Action, Source, DocID string
	Status                                                    int
	LatencyMS                                                 int64
	Detail                                                    string
}

// Store is the state backend.
type Store interface {
	PutUser(ctx context.Context, u User) error
	GetUser(ctx context.Context, login string) (User, error)
	ListUsers(ctx context.Context) ([]User, error)
	CreateToken(ctx context.Context, t Token) error // ErrConflict on duplicate ID
	GetToken(ctx context.Context, id string) (Token, error)
	ListTokens(ctx context.Context, owner string) ([]Token, error) // owner "" = all
	RevokeToken(ctx context.Context, id string) error
	TouchToken(ctx context.Context, id string, at time.Time) error
	GetConnectorConfig(ctx context.Context, id string) (map[string]string, error)
	PutConnectorConfig(ctx context.Context, id string, cfg map[string]string) error
	AppendAudit(ctx context.Context, e AuditEvent) error
	ListAudit(ctx context.Context, since time.Time, limit int) ([]AuditEvent, error) // newest first
	Close() error
}

var (
	ErrNotFound = errors.New("store: not found")
	ErrConflict = errors.New("store: conflict")
)
