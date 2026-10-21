// Package storetest holds the conformance suite every store.Store must pass.
package storetest

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/change-fabric/moose/internal/store"
)

// Run executes the conformance suite. newStore must return a fresh, empty store.
func Run(t *testing.T, newStore func(t *testing.T) store.Store) {
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Millisecond)

	t.Run("UserRoundTrip", func(t *testing.T) {
		s := newStore(t)
		u := store.User{Login: "alice@example.com", Role: "admin", Created: now}
		must(t, s.PutUser(ctx, u))
		must(t, s.PutUser(ctx, store.User{Login: "bob@example.com", Role: "member", Created: now, Disabled: true}))
		got, err := s.GetUser(ctx, u.Login)
		must(t, err)
		if got.Login != u.Login || got.Role != u.Role || !got.Created.Equal(now) || got.Disabled {
			t.Fatalf("GetUser = %+v, want %+v", got, u)
		}
		if _, err := s.GetUser(ctx, "nobody"); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("GetUser missing err = %v, want ErrNotFound", err)
		}
		users, err := s.ListUsers(ctx)
		must(t, err)
		if len(users) != 2 {
			t.Fatalf("ListUsers len = %d, want 2", len(users))
		}
	})

	t.Run("DuplicateTokenConflict", func(t *testing.T) {
		s := newStore(t)
		tok := token("tok1", "alice@example.com", now)
		must(t, s.CreateToken(ctx, tok))
		if err := s.CreateToken(ctx, tok); !errors.Is(err, store.ErrConflict) {
			t.Fatalf("duplicate CreateToken err = %v, want ErrConflict", err)
		}
		got, err := s.GetToken(ctx, "tok1")
		must(t, err)
		if got.Owner != tok.Owner || got.Name != tok.Name || !bytes.Equal(got.Hash, tok.Hash) ||
			!got.Created.Equal(now) || got.LastUsed != nil || got.Expires == nil || !got.Expires.Equal(*tok.Expires) || got.Revoked {
			t.Fatalf("GetToken = %+v, want %+v", got, tok)
		}
	})

	t.Run("GetTokenMissing", func(t *testing.T) {
		s := newStore(t)
		if _, err := s.GetToken(ctx, "missing"); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("err = %v, want ErrNotFound", err)
		}
		if err := s.RevokeToken(ctx, "missing"); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("RevokeToken err = %v, want ErrNotFound", err)
		}
		if err := s.TouchToken(ctx, "missing", now); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("TouchToken err = %v, want ErrNotFound", err)
		}
	})

	t.Run("RevokeVisible", func(t *testing.T) {
		s := newStore(t)
		must(t, s.CreateToken(ctx, token("tok1", "alice@example.com", now)))
		must(t, s.RevokeToken(ctx, "tok1"))
		got, err := s.GetToken(ctx, "tok1")
		must(t, err)
		if !got.Revoked {
			t.Fatal("token not revoked")
		}
		list, err := s.ListTokens(ctx, "alice@example.com")
		must(t, err)
		if len(list) != 1 || !list[0].Revoked {
			t.Fatalf("ListTokens after revoke = %+v", list)
		}
	})

	t.Run("ListTokensByOwner", func(t *testing.T) {
		s := newStore(t)
		must(t, s.CreateToken(ctx, token("a1", "alice@example.com", now)))
		must(t, s.CreateToken(ctx, token("a2", "alice@example.com", now)))
		must(t, s.CreateToken(ctx, token("b1", "bob@example.com", now)))
		alice, err := s.ListTokens(ctx, "alice@example.com")
		must(t, err)
		if len(alice) != 2 {
			t.Fatalf("alice tokens = %d, want 2", len(alice))
		}
		for _, tk := range alice {
			if tk.Owner != "alice@example.com" {
				t.Fatalf("wrong owner %q", tk.Owner)
			}
		}
		all, err := s.ListTokens(ctx, "")
		must(t, err)
		if len(all) != 3 {
			t.Fatalf("all tokens = %d, want 3", len(all))
		}
		none, err := s.ListTokens(ctx, "carol@example.com")
		must(t, err)
		if len(none) != 0 {
			t.Fatalf("carol tokens = %d, want 0", len(none))
		}
	})

	t.Run("TouchTokenUpdatesLastUsed", func(t *testing.T) {
		s := newStore(t)
		must(t, s.CreateToken(ctx, token("tok1", "alice@example.com", now)))
		at := now.Add(time.Minute)
		must(t, s.TouchToken(ctx, "tok1", at))
		got, err := s.GetToken(ctx, "tok1")
		must(t, err)
		if got.LastUsed == nil || !got.LastUsed.Equal(at) {
			t.Fatalf("LastUsed = %v, want %v", got.LastUsed, at)
		}
	})

	t.Run("ConnectorConfig", func(t *testing.T) {
		s := newStore(t)
		if _, err := s.GetConnectorConfig(ctx, "gdrive"); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("missing config err = %v, want ErrNotFound", err)
		}
		must(t, s.PutConnectorConfig(ctx, "gdrive", map[string]string{"root": "abc", "x": "1"}))
		got, err := s.GetConnectorConfig(ctx, "gdrive")
		must(t, err)
		if len(got) != 2 || got["root"] != "abc" || got["x"] != "1" {
			t.Fatalf("config = %v", got)
		}
		must(t, s.PutConnectorConfig(ctx, "gdrive", map[string]string{"root": "def"}))
		got, err = s.GetConnectorConfig(ctx, "gdrive")
		must(t, err)
		if len(got) != 1 || got["root"] != "def" {
			t.Fatalf("overwritten config = %v", got)
		}
	})

	t.Run("AuditNewestFirstAndSince", func(t *testing.T) {
		s := newStore(t)
		base := now.Add(-48 * time.Hour)
		for i := 0; i < 5; i++ {
			must(t, s.AppendAudit(ctx, store.AuditEvent{
				At: base.Add(time.Duration(i) * 12 * time.Hour), RequestID: string(rune('a' + i)),
				Actor: "alice@example.com", TokenID: "tok1", Surface: "rest", Action: "search",
				Source: "gdrive", DocID: "d", Status: 200, LatencyMS: int64(i), Detail: "ok",
			}))
		}
		all, err := s.ListAudit(ctx, base.Add(-time.Hour), 100)
		must(t, err)
		if len(all) != 5 {
			t.Fatalf("audit len = %d, want 5", len(all))
		}
		for i := 1; i < len(all); i++ {
			if !all[i-1].At.After(all[i].At) {
				t.Fatalf("audit not newest first: %v then %v", all[i-1].At, all[i].At)
			}
		}
		first := all[0]
		if first.RequestID != "e" || first.Status != 200 || first.LatencyMS != 4 || first.Actor != "alice@example.com" ||
			first.Surface != "rest" || first.Detail != "ok" || !first.At.Equal(base.Add(48*time.Hour)) {
			t.Fatalf("newest event = %+v", first)
		}
		recent, err := s.ListAudit(ctx, base.Add(30*time.Hour), 100)
		must(t, err)
		if len(recent) != 2 {
			t.Fatalf("since filter len = %d, want 2", len(recent))
		}
		limited, err := s.ListAudit(ctx, base.Add(-time.Hour), 2)
		must(t, err)
		if len(limited) != 2 || limited[0].RequestID != "e" {
			t.Fatalf("limited = %+v", limited)
		}
	})

	t.Run("CloseIdempotent", func(t *testing.T) {
		s := newStore(t)
		must(t, s.Close())
		must(t, s.Close())
	})
}

func token(id, owner string, created time.Time) store.Token {
	exp := created.Add(30 * 24 * time.Hour)
	return store.Token{ID: id, Owner: owner, Name: id + "-name", Hash: []byte("hash-" + id), Created: created, Expires: &exp}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
