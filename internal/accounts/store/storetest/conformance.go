// Package storetest holds the conformance suites for the email store: RunBackend
// for a store.Backend and Run for the Store built on it. Both the sqlite and
// dynamo backends must pass both, which is what keeps every conditional write
// (code redeem, refresh rotation, send claim, cap reservation) and the ttl
// attribute behaving the same everywhere.
package storetest

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/change-fabric/moose/internal/accounts/store"
	"github.com/change-fabric/moose/internal/email/provider"
)

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func isCond(err error) bool {
	var ce *store.CondError
	return errors.As(err, &ce)
}

func condIndex(err error) int {
	var ce *store.CondError
	if errors.As(err, &ce) {
		return ce.Index
	}
	return -1
}

// RunBackend exercises the Backend contract. newBackend must return a fresh, empty backend.
func RunBackend(t *testing.T, newBackend func(t *testing.T) store.Backend) {
	ctx := context.Background()
	put := func(it store.Item, c store.Cond) store.Op { return store.Op{Kind: store.OpPut, Item: it, Cond: c} }

	t.Run("PutGetRoundTripAndTTL", func(t *testing.T) {
		b := newBackend(t)
		ttl := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
		it := store.Item{PK: "p", SK: "s", G1: "g", Data: []byte(`{"a":1}`), Ver: 3, N: 7, TTL: ttl.Add(400 * time.Millisecond)}
		must(t, b.Transact(ctx, []store.Op{put(it, store.Cond{})}))
		got, ok, err := b.Get(ctx, "p", "s")
		must(t, err)
		if !ok || got.PK != "p" || got.SK != "s" || got.G1 != "g" || !bytes.Equal(got.Data, it.Data) || got.Ver != 3 || got.N != 7 {
			t.Fatalf("Get = %+v ok=%v", got, ok)
		}
		if !got.TTL.Equal(ttl) {
			t.Fatalf("ttl = %v, want %v (whole seconds)", got.TTL, ttl)
		}
		if _, ok, err := b.Get(ctx, "p", "missing"); err != nil || ok {
			t.Fatalf("missing Get ok=%v err=%v", ok, err)
		}
		must(t, b.Transact(ctx, []store.Op{put(store.Item{PK: "p", SK: "nottl", Data: []byte("{}")}, store.Cond{})}))
		got, _, _ = b.Get(ctx, "p", "nottl")
		if !got.TTL.IsZero() {
			t.Fatalf("row without ttl read back ttl %v", got.TTL)
		}
	})

	t.Run("CondAbsentAndCondVer", func(t *testing.T) {
		b := newBackend(t)
		it := store.Item{PK: "p", SK: "s", Data: []byte("1"), Ver: 1}
		must(t, b.Transact(ctx, []store.Op{put(it, store.Cond{Kind: store.CondAbsent})}))
		if err := b.Transact(ctx, []store.Op{put(it, store.Cond{Kind: store.CondAbsent})}); condIndex(err) != 0 {
			t.Fatalf("second CondAbsent err = %v, want CondError{0}", err)
		}
		next := it
		next.Ver, next.Data = 2, []byte("2")
		if err := b.Transact(ctx, []store.Op{put(next, store.Cond{Kind: store.CondVer, Ver: 9})}); condIndex(err) != 0 {
			t.Fatalf("wrong version err = %v, want CondError{0}", err)
		}
		must(t, b.Transact(ctx, []store.Op{put(next, store.Cond{Kind: store.CondVer, Ver: 1})}))
		got, _, _ := b.Get(ctx, "p", "s")
		if got.Ver != 2 || string(got.Data) != "2" {
			t.Fatalf("after versioned write: %+v", got)
		}
		if err := b.Transact(ctx, []store.Op{put(store.Item{PK: "p", SK: "gone"}, store.Cond{Kind: store.CondVer, Ver: 0})}); condIndex(err) != 0 {
			t.Fatalf("CondVer on a missing row err = %v, want CondError{0}", err)
		}
	})

	t.Run("TransactionIsAtomic", func(t *testing.T) {
		b := newBackend(t)
		must(t, b.Transact(ctx, []store.Op{put(store.Item{PK: "p", SK: "taken", Data: []byte("x")}, store.Cond{})}))
		err := b.Transact(ctx, []store.Op{
			put(store.Item{PK: "p", SK: "first", Data: []byte("x")}, store.Cond{Kind: store.CondAbsent}),
			put(store.Item{PK: "p", SK: "taken", Data: []byte("y")}, store.Cond{Kind: store.CondAbsent}),
		})
		if condIndex(err) != 1 {
			t.Fatalf("err = %v, want CondError{1}", err)
		}
		if _, ok, _ := b.Get(ctx, "p", "first"); ok {
			t.Fatal("first op of an aborted transaction was applied")
		}
	})

	t.Run("Delete", func(t *testing.T) {
		b := newBackend(t)
		must(t, b.Transact(ctx, []store.Op{put(store.Item{PK: "p", SK: "s", Data: []byte("x"), Ver: 4}, store.Cond{})}))
		del := func(c store.Cond) store.Op {
			return store.Op{Kind: store.OpDelete, Item: store.Item{PK: "p", SK: "s"}, Cond: c}
		}
		if err := b.Transact(ctx, []store.Op{del(store.Cond{Kind: store.CondVer, Ver: 1})}); condIndex(err) != 0 {
			t.Fatalf("delete with wrong version err = %v", err)
		}
		must(t, b.Transact(ctx, []store.Op{del(store.Cond{Kind: store.CondVer, Ver: 4})}))
		if _, ok, _ := b.Get(ctx, "p", "s"); ok {
			t.Fatal("row still present after delete")
		}
	})

	t.Run("IncrBoundsAndTTL", func(t *testing.T) {
		b := newBackend(t)
		ttl := time.Now().Add(2 * time.Hour).UTC().Truncate(time.Second)
		incr := func(delta, max int64) error {
			return b.Transact(ctx, []store.Op{{Kind: store.OpIncr, Delta: delta, Max: max, Item: store.Item{PK: "c", SK: "n", TTL: ttl}}})
		}
		must(t, incr(1, 2))
		must(t, incr(1, 2))
		if err := incr(1, 2); condIndex(err) != 0 {
			t.Fatalf("increment past max err = %v, want CondError{0}", err)
		}
		got, _, _ := b.Get(ctx, "c", "n")
		if got.N != 2 || !got.TTL.Equal(ttl) {
			t.Fatalf("counter = %+v, want n=2 ttl=%v", got, ttl)
		}
		must(t, incr(-1, 0))
		must(t, incr(-1, 0))
		if err := incr(-1, 0); condIndex(err) != 0 {
			t.Fatalf("decrement below zero err = %v, want CondError{0}", err)
		}
		if err := b.Transact(ctx, []store.Op{{Kind: store.OpIncr, Delta: -1, Item: store.Item{PK: "c", SK: "never"}}}); condIndex(err) != 0 {
			t.Fatalf("decrement of a missing counter err = %v, want CondError{0}", err)
		}
	})

	t.Run("ConcurrentIncrRespectsMax", func(t *testing.T) {
		b := newBackend(t)
		var wins atomic.Int32
		var wg sync.WaitGroup
		for range 12 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				err := b.Transact(ctx, []store.Op{{Kind: store.OpIncr, Delta: 1, Max: 3, Item: store.Item{PK: "c", SK: "n"}}})
				if err == nil {
					wins.Add(1)
				} else if !isCond(err) {
					t.Errorf("unexpected error %v", err)
				}
			}()
		}
		wg.Wait()
		if wins.Load() != 3 {
			t.Fatalf("%d increments won, want exactly 3", wins.Load())
		}
	})

	t.Run("QueryAndIndex", func(t *testing.T) {
		b := newBackend(t)
		for _, sk := range []string{"ACCT#b", "NICK#x", "ACCT#a", "ACCT#c"} {
			must(t, b.Transact(ctx, []store.Op{put(store.Item{PK: "o", SK: sk, Data: []byte("{}")}, store.Cond{})}))
		}
		must(t, b.Transact(ctx, []store.Op{put(store.Item{PK: "other", SK: "ACCT#z", Data: []byte("{}")}, store.Cond{})}))
		all, err := b.Query(ctx, "o", "")
		must(t, err)
		if len(all) != 4 {
			t.Fatalf("Query all = %d rows, want 4", len(all))
		}
		accts, err := b.Query(ctx, "o", "ACCT#")
		must(t, err)
		if len(accts) != 3 || accts[0].SK != "ACCT#a" || accts[1].SK != "ACCT#b" || accts[2].SK != "ACCT#c" {
			t.Fatalf("Query prefix = %+v, want ACCT#a..c in order", accts)
		}
		must(t, b.Transact(ctx, []store.Op{
			put(store.Item{PK: "o", SK: "ACCT#a", G1: "ADDR#o#a@x.com", Data: []byte("{}")}, store.Cond{}),
		}))
		idx, err := b.QueryIndex(ctx, "ADDR#o#a@x.com")
		must(t, err)
		if len(idx) != 1 || idx[0].SK != "ACCT#a" {
			t.Fatalf("QueryIndex = %+v", idx)
		}
		if none, err := b.QueryIndex(ctx, "ADDR#o#nobody"); err != nil || len(none) != 0 {
			t.Fatalf("QueryIndex miss = %v, %v", none, err)
		}
	})
}

// Run executes the Store conformance suite. newStore must return a fresh, empty store.
func Run(t *testing.T, newStore func(t *testing.T) store.Store) {
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	sec := func(t time.Time) time.Time { return t.UTC().Truncate(time.Second) }

	newAcct := func(owner, nick, addr string, color int) store.Account {
		return store.Account{
			Owner: owner, ID: store.NewAccountID(), Provider: store.ProviderGoogle, ProviderUID: addr, Address: addr,
			Nickname: nick, ColorIndex: color, Status: store.StatusOK, HourlyCap: store.DefaultHourlyCap,
			Caps:         provider.Capabilities{Threads: true, Labels: true, SearchDialect: provider.DialectGmail},
			RefreshToken: []byte("sealed-" + addr),
		}
	}

	t.Run("AccountRoundTrip", func(t *testing.T) {
		s := newStore(t)
		a := newAcct("pat", "Work", "Pat@Work.example", 0)
		a.Groups = []string{"work", "client"}
		must(t, s.CreateAccount(ctx, a))
		got, err := s.GetAccount(ctx, "pat", a.ID)
		must(t, err)
		if got.Nickname != "Work" || got.Address != "Pat@Work.example" || got.SendEnabled || got.Status != store.StatusOK ||
			got.HourlyCap != store.DefaultHourlyCap || len(got.Groups) != 2 || !got.Caps.Threads || got.IsDefault ||
			!bytes.Equal(got.RefreshToken, a.RefreshToken) || got.Created.IsZero() {
			t.Fatalf("GetAccount = %+v", got)
		}
		if _, err := s.GetAccount(ctx, "pat", "acc_missing"); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("missing account err = %v", err)
		}
		if _, err := s.GetAccount(ctx, "someone-else", a.ID); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("another owner's account err = %v, want ErrNotFound", err)
		}
		if err := s.CreateAccount(ctx, a); !errors.Is(err, store.ErrConflict) {
			t.Fatalf("duplicate id err = %v, want ErrConflict", err)
		}
	})

	t.Run("AccountIDShape", func(t *testing.T) {
		id := store.NewAccountID()
		if len(id) != len("acc_")+16 || id[:4] != "acc_" || id == store.NewAccountID() {
			t.Fatalf("id %q is not acc_<16 base32> and unique", id)
		}
	})

	t.Run("DuplicateNicknameRejectedAtomically", func(t *testing.T) {
		s := newStore(t)
		a := newAcct("pat", "Work", "a@x.example", 0)
		must(t, s.CreateAccount(ctx, a))
		dup := newAcct("pat", "  work ", "b@x.example", 1)
		if err := s.CreateAccount(ctx, dup); !errors.Is(err, store.ErrNicknameTaken) {
			t.Fatalf("duplicate nickname err = %v, want ErrNicknameTaken", err)
		}
		if _, err := s.GetAccount(ctx, "pat", dup.ID); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("account of a rejected nickname was written: %v", err)
		}
		other := newAcct("conner", "Work", "c@x.example", 0)
		must(t, s.CreateAccount(ctx, other)) // nicknames are unique per owner
		for _, bad := range []string{"", "   ", "this nickname is far too long", "tab\there"} {
			if err := s.CreateAccount(ctx, newAcct("pat", bad, "d@x.example", 2)); !errors.Is(err, store.ErrInvalidNickname) {
				t.Fatalf("nickname %q err = %v, want ErrInvalidNickname", bad, err)
			}
		}
	})

	t.Run("ListAccountsOneOwnerOldestFirst", func(t *testing.T) {
		s := newStore(t)
		for i, nick := range []string{"One", "Two", "Three"} {
			a := newAcct("pat", nick, nick+"@x.example", i)
			a.Created = now.Add(time.Duration(i) * time.Minute)
			must(t, s.CreateAccount(ctx, a))
		}
		must(t, s.CreateAccount(ctx, newAcct("conner", "Mine", "m@x.example", 0)))
		list, err := s.ListAccounts(ctx, "pat")
		must(t, err)
		if len(list) != 3 || list[0].Nickname != "One" || list[2].Nickname != "Three" {
			t.Fatalf("ListAccounts = %+v", list)
		}
		if none, err := s.ListAccounts(ctx, "nobody"); err != nil || len(none) != 0 {
			t.Fatalf("ListAccounts(nobody) = %v, %v", none, err)
		}
	})

	t.Run("ReconnectKeepsIdentity", func(t *testing.T) {
		s := newStore(t)
		a := newAcct("pat", "Work", "a@x.example", 3)
		must(t, s.CreateAccount(ctx, a))
		cur, err := s.FindAccountByAddress(ctx, "pat", "A@X.example")
		must(t, err)
		if cur.ID != a.ID {
			t.Fatalf("FindAccountByAddress id = %s, want %s", cur.ID, a.ID)
		}
		must(t, s.SetAccountCredential(ctx, "pat", cur.ID, []byte("new-token"), cur.Version))
		must(t, s.UpdateAccount(ctx, "pat", cur.ID, store.AccountPatch{Status: store.Ptr(store.StatusNeedsReauth)}))
		must(t, s.UpdateAccount(ctx, "pat", cur.ID, store.AccountPatch{Status: store.Ptr(store.StatusOK)}))
		got, err := s.GetAccount(ctx, "pat", a.ID)
		must(t, err)
		if got.ID != a.ID || got.ColorIndex != 3 || got.Nickname != "Work" || string(got.RefreshToken) != "new-token" || got.Status != store.StatusOK {
			t.Fatalf("after reconnect: %+v", got)
		}
		if _, err := s.FindAccountByAddress(ctx, "pat", "nobody@x.example"); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("unknown address err = %v", err)
		}
		if _, err := s.FindAccountByAddress(ctx, "conner", "a@x.example"); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("another owner's address err = %v", err)
		}
	})

	t.Run("CredentialWriteIsVersioned", func(t *testing.T) {
		s := newStore(t)
		a := newAcct("pat", "Work", "a@x.example", 0)
		must(t, s.CreateAccount(ctx, a))
		cur, err := s.GetAccount(ctx, "pat", a.ID)
		must(t, err)
		must(t, s.SetAccountCredential(ctx, "pat", a.ID, []byte("rt-2"), cur.Version))
		got, _ := s.GetAccount(ctx, "pat", a.ID)
		if string(got.RefreshToken) != "rt-2" || got.Version != cur.Version+1 {
			t.Fatalf("after write: token %q version %d (was %d)", got.RefreshToken, got.Version, cur.Version)
		}
		// A second writer that read the same version loses and changes nothing.
		if err := s.SetAccountCredential(ctx, "pat", a.ID, []byte("rt-stale"), cur.Version); !errors.Is(err, store.ErrConflict) {
			t.Fatalf("stale credential write err = %v, want ErrConflict", err)
		}
		got, _ = s.GetAccount(ctx, "pat", a.ID)
		if string(got.RefreshToken) != "rt-2" {
			t.Fatalf("a conflicting write changed the credential to %q", got.RefreshToken)
		}
		if err := s.SetAccountCredential(ctx, "pat", "acc_missing", []byte("x"), 0); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("missing account err = %v, want ErrNotFound", err)
		}
		if err := s.SetAccountCredential(ctx, "other", a.ID, []byte("x"), got.Version); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("another owner's account err = %v, want ErrNotFound", err)
		}
		if err := s.SetAccountCredential(ctx, "pat", a.ID, nil, got.Version); err == nil {
			t.Fatal("an empty credential must be refused")
		}
		// Disconnecting bumps the version, so a write-back that read the old
		// credential cannot bring the account back.
		must(t, s.DisconnectAccount(ctx, "pat", a.ID, now))
		if err := s.SetAccountCredential(ctx, "pat", a.ID, []byte("late"), got.Version); !errors.Is(err, store.ErrConflict) {
			t.Fatalf("write-back after disconnect err = %v, want ErrConflict", err)
		}
		after, _ := s.GetAccount(ctx, "pat", a.ID)
		if len(after.RefreshToken) != 0 || after.Status != store.StatusDisconnected {
			t.Fatalf("disconnected account was revived: %+v", after)
		}
	})

	// Both orderings of a display patch and a credential write end with the new
	// credential and the patched field. The stale reader is the point: it holds
	// a copy from before the other writer ran.
	t.Run("PatchThenCredentialWrite", func(t *testing.T) {
		s := newStore(t)
		a := newAcct("pat", "Work", "a@x.example", 0)
		must(t, s.CreateAccount(ctx, a))
		stale, err := s.GetAccount(ctx, "pat", a.ID) // the refresher's read
		must(t, err)
		must(t, s.UpdateAccount(ctx, "pat", a.ID, store.AccountPatch{Nickname: store.Ptr("Renamed"), SendEnabled: store.Ptr(true)}))
		must(t, s.SetAccountCredential(ctx, "pat", a.ID, []byte("rt-new"), stale.Version))
		got, _ := s.GetAccount(ctx, "pat", a.ID)
		if string(got.RefreshToken) != "rt-new" || got.Nickname != "Renamed" || !got.SendEnabled {
			t.Fatalf("final record = token %q nickname %q send %v", got.RefreshToken, got.Nickname, got.SendEnabled)
		}
	})

	t.Run("CredentialWriteThenPatchFromStaleRead", func(t *testing.T) {
		s := newStore(t)
		a := newAcct("pat", "Work", "a@x.example", 0)
		must(t, s.CreateAccount(ctx, a))
		stale, err := s.GetAccount(ctx, "pat", a.ID) // the patcher's read, before the rotation
		must(t, err)
		must(t, s.SetAccountCredential(ctx, "pat", a.ID, []byte("rt-new"), stale.Version))
		// The patch is built from the stale read but carries only display
		// fields, so it cannot put the old credential back.
		must(t, s.UpdateAccount(ctx, "pat", stale.ID, store.AccountPatch{ColorIndex: store.Ptr(7), SendEnabled: store.Ptr(true)}))
		got, _ := s.GetAccount(ctx, "pat", a.ID)
		if string(got.RefreshToken) != "rt-new" || got.ColorIndex != 7 || !got.SendEnabled || got.Version != stale.Version+1 {
			t.Fatalf("final record = token %q color %d send %v version %d", got.RefreshToken, got.ColorIndex, got.SendEnabled, got.Version)
		}
	})

	t.Run("PatchLeavesUnsetFieldsAlone", func(t *testing.T) {
		s := newStore(t)
		a := newAcct("pat", "Work", "a@x.example", 4)
		a.Groups = []string{"g"}
		must(t, s.CreateAccount(ctx, a))
		must(t, s.UpdateAccount(ctx, "pat", a.ID, store.AccountPatch{InviteEnabled: store.Ptr(true)}))
		got, _ := s.GetAccount(ctx, "pat", a.ID)
		if !got.InviteEnabled || got.Nickname != "Work" || got.ColorIndex != 4 || len(got.Groups) != 1 || got.Status != store.StatusOK ||
			string(got.RefreshToken) != "sealed-a@x.example" {
			t.Fatalf("patch touched unset fields: %+v", got)
		}
	})

	t.Run("ConcurrentCredentialWritersOneWins", func(t *testing.T) {
		s := newStore(t)
		a := newAcct("pat", "Work", "a@x.example", 0)
		must(t, s.CreateAccount(ctx, a))
		cur, _ := s.GetAccount(ctx, "pat", a.ID)
		const writers = 8
		var wg sync.WaitGroup
		var won, lost atomic.Int32
		for i := 0; i < writers; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				switch err := s.SetAccountCredential(ctx, "pat", a.ID, []byte(fmt.Sprintf("rt-%d", i)), cur.Version); {
				case err == nil:
					won.Add(1)
				case errors.Is(err, store.ErrConflict):
					lost.Add(1)
				default:
					t.Errorf("unexpected error: %v", err)
				}
			}()
		}
		wg.Wait()
		if won.Load() != 1 || lost.Load() != writers-1 {
			t.Fatalf("won %d lost %d, want exactly one winner", won.Load(), lost.Load())
		}
	})

	t.Run("RenameMovesNicknameRow", func(t *testing.T) {
		s := newStore(t)
		a, b := newAcct("pat", "Work", "a@x.example", 0), newAcct("pat", "Home", "b@x.example", 1)
		must(t, s.CreateAccount(ctx, a))
		must(t, s.CreateAccount(ctx, b))
		if err := s.UpdateAccount(ctx, "pat", a.ID, store.AccountPatch{Nickname: store.Ptr("Home")}); !errors.Is(err, store.ErrNicknameTaken) {
			t.Fatalf("rename onto a taken nickname err = %v, want ErrNicknameTaken", err)
		}
		must(t, s.UpdateAccount(ctx, "pat", a.ID, store.AccountPatch{Nickname: store.Ptr("Office")}))
		must(t, s.CreateAccount(ctx, newAcct("pat", "Work", "c@x.example", 2))) // old slug is free again
		if err := s.CreateAccount(ctx, newAcct("pat", "OFFICE", "d@x.example", 3)); !errors.Is(err, store.ErrNicknameTaken) {
			t.Fatalf("new slug not reserved: %v", err)
		}
		must(t, s.UpdateAccount(ctx, "pat", a.ID, store.AccountPatch{Nickname: store.Ptr("OFFICE")})) // case-only change keeps the slug
		got, _ := s.GetAccount(ctx, "pat", a.ID)
		if got.Nickname != "OFFICE" {
			t.Fatalf("nickname = %q", got.Nickname)
		}
		if err := s.UpdateAccount(ctx, "pat", "acc_ghost", store.AccountPatch{Nickname: store.Ptr("Ghost")}); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("update of a missing account err = %v", err)
		}
	})

	t.Run("SecondDefaultClearsFirst", func(t *testing.T) {
		s := newStore(t)
		a, b := newAcct("pat", "A", "a@x.example", 0), newAcct("pat", "B", "b@x.example", 1)
		a.IsDefault = true
		must(t, s.CreateAccount(ctx, a))
		must(t, s.CreateAccount(ctx, b))
		defaults := func() []string {
			list, err := s.ListAccounts(ctx, "pat")
			must(t, err)
			var ids []string
			for _, x := range list {
				if x.IsDefault {
					ids = append(ids, x.Nickname)
				}
			}
			return ids
		}
		if d := defaults(); len(d) != 1 || d[0] != "A" {
			t.Fatalf("defaults = %v, want [A]", d)
		}
		must(t, s.SetDefaultAccount(ctx, "pat", b.ID))
		if d := defaults(); len(d) != 1 || d[0] != "B" {
			t.Fatalf("defaults after switch = %v, want [B]", d)
		}
		got, _ := s.GetAccount(ctx, "pat", a.ID)
		if got.IsDefault {
			t.Fatal("first default still set on GetAccount")
		}
		if err := s.SetDefaultAccount(ctx, "pat", "acc_missing"); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("default of a missing account err = %v", err)
		}
	})

	t.Run("DisconnectDeletesTokens", func(t *testing.T) {
		s := newStore(t)
		a := newAcct("pat", "Work", "a@x.example", 0)
		a.SendEnabled = true
		must(t, s.CreateAccount(ctx, a))
		must(t, s.DisconnectAccount(ctx, "pat", a.ID, now))
		got, err := s.GetAccount(ctx, "pat", a.ID)
		must(t, err)
		if len(got.RefreshToken) != 0 || got.SendEnabled || got.Status != store.StatusDisconnected || got.Nickname != "Work" {
			t.Fatalf("after disconnect: %+v", got)
		}
		if err := s.DisconnectAccount(ctx, "pat", "acc_missing", now); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("disconnect of a missing account err = %v", err)
		}
	})

	t.Run("OAuthClient", func(t *testing.T) {
		s := newStore(t)
		c := store.OAuthClient{ID: "c1", Name: "Claude", RedirectURIs: []string{"https://claude.ai/api/mcp/auth_callback"}, Created: now}
		must(t, s.CreateOAuthClient(ctx, c))
		if err := s.CreateOAuthClient(ctx, c); !errors.Is(err, store.ErrConflict) {
			t.Fatalf("duplicate client err = %v", err)
		}
		got, err := s.GetOAuthClient(ctx, "c1")
		must(t, err)
		if got.Name != "Claude" || len(got.RedirectURIs) != 1 {
			t.Fatalf("client = %+v", got)
		}
		if _, err := s.GetOAuthClient(ctx, "nope"); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("missing client err = %v", err)
		}
	})

	newCode := func(hash string) store.AuthCode {
		return store.AuthCode{Hash: hash, Owner: "pat", ClientID: "c1", RedirectURI: "https://x/cb", CodeChallenge: "chal",
			Resource: "https://moose.example/mcp", FamilyID: "fam1", Created: now, Expires: now.Add(60 * time.Second)}
	}

	t.Run("AuthCodeRedeemOnce", func(t *testing.T) {
		s := newStore(t)
		must(t, s.CreateAuthCode(ctx, newCode("h1")))
		if err := s.CreateAuthCode(ctx, newCode("h1")); !errors.Is(err, store.ErrConflict) {
			t.Fatalf("duplicate code err = %v", err)
		}
		got, err := s.RedeemAuthCode(ctx, "h1", now.Add(time.Second))
		must(t, err)
		if got.ClientID != "c1" || got.FamilyID != "fam1" || got.RedeemedAt == nil {
			t.Fatalf("redeemed = %+v", got)
		}
		again, err := s.RedeemAuthCode(ctx, "h1", now.Add(2*time.Second))
		if !errors.Is(err, store.ErrAlreadyRedeemed) || again.FamilyID != "fam1" {
			t.Fatalf("second redeem = %+v, %v; want ErrAlreadyRedeemed carrying the family", again, err)
		}
		if _, err := s.RedeemAuthCode(ctx, "nope", now); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("unknown code err = %v", err)
		}
	})

	t.Run("AuthCodeExpiredCannotRedeem", func(t *testing.T) {
		s := newStore(t)
		must(t, s.CreateAuthCode(ctx, newCode("h1")))
		if _, err := s.RedeemAuthCode(ctx, "h1", now.Add(61*time.Second)); !errors.Is(err, store.ErrExpired) {
			t.Fatalf("expired redeem err = %v, want ErrExpired", err)
		}
	})

	t.Run("AuthCodeTTLOutlivesExpiry", func(t *testing.T) {
		s := newStore(t)
		c := newCode("h1")
		must(t, s.CreateAuthCode(ctx, c))
		got, err := s.RedeemAuthCode(ctx, "h1", now)
		must(t, err)
		if want := sec(c.Expires.Add(store.AuthCodeRetention)); !got.TTL.Equal(want) {
			t.Fatalf("code ttl = %v, want %v", got.TTL, want)
		}
	})

	t.Run("AuthCodeConcurrentRedeemHasOneWinner", func(t *testing.T) {
		s := newStore(t)
		must(t, s.CreateAuthCode(ctx, newCode("h1")))
		var won, replayed atomic.Int32
		var wg sync.WaitGroup
		for range 10 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, err := s.RedeemAuthCode(ctx, "h1", now)
				switch {
				case err == nil:
					won.Add(1)
				case errors.Is(err, store.ErrAlreadyRedeemed):
					replayed.Add(1)
				default:
					t.Errorf("unexpected error %v", err)
				}
			}()
		}
		wg.Wait()
		if won.Load() != 1 || replayed.Load() != 9 {
			t.Fatalf("won=%d replayed=%d, want 1 and 9", won.Load(), replayed.Load())
		}
	})

	t.Run("SessionsAndFamilyRevocation", func(t *testing.T) {
		s := newStore(t)
		sess := store.Session{Hash: "s1", Kind: store.SessionAccess, Owner: "pat", ClientID: "c1", FamilyID: "fam1",
			Audience: "https://moose.example/mcp", Created: now, Expires: now.Add(time.Hour)}
		must(t, s.CreateSession(ctx, sess))
		if err := s.CreateSession(ctx, sess); !errors.Is(err, store.ErrConflict) {
			t.Fatalf("duplicate session err = %v", err)
		}
		other := sess
		other.Hash, other.FamilyID = "s2", "fam2"
		must(t, s.CreateSession(ctx, other))
		dash := store.Session{Hash: "d1", Kind: store.SessionDashboard, Owner: "pat", Created: now, Expires: now.Add(time.Hour)}
		must(t, s.CreateSession(ctx, dash))
		got, err := s.GetSession(ctx, "s1")
		must(t, err)
		if got.Revoked || got.Audience != sess.Audience || !got.TTL.Equal(sec(sess.Expires)) {
			t.Fatalf("session = %+v", got)
		}
		must(t, s.RevokeFamily(ctx, "fam1"))
		for hash, want := range map[string]bool{"s1": true, "s2": false, "d1": false} {
			got, err := s.GetSession(ctx, hash)
			must(t, err)
			if got.Revoked != want {
				t.Errorf("session %s revoked = %v, want %v", hash, got.Revoked, want)
			}
		}
		must(t, s.RevokeSession(ctx, "d1"))
		must(t, s.RevokeSession(ctx, "d1")) // idempotent
		if err := s.RevokeSession(ctx, "nope"); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("revoke of a missing session err = %v", err)
		}
		if _, err := s.GetSession(ctx, "nope"); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("missing session err = %v", err)
		}
	})

	newFamily := func(id string) store.RefreshFamily {
		return store.RefreshFamily{ID: id, Owner: "pat", ClientID: "c1", CurrentHash: "r0", Created: now, LastRotated: now,
			IdleExpires: now.Add(store.RefreshIdleTTL), AbsExpires: now.Add(store.RefreshAbsoluteTTL)}
	}

	t.Run("RefreshRotation", func(t *testing.T) {
		s := newStore(t)
		must(t, s.CreateRefreshFamily(ctx, newFamily("fam1")))
		if err := s.CreateRefreshFamily(ctx, newFamily("fam1")); !errors.Is(err, store.ErrConflict) {
			t.Fatalf("duplicate family err = %v", err)
		}
		out, err := s.RotateRefresh(ctx, "fam1", "r0", "r1", now.Add(time.Minute))
		must(t, err)
		if out != store.OutcomeRotated {
			t.Fatalf("outcome = %s", out)
		}
		out, err = s.RotateRefresh(ctx, "fam1", "r1", "r2", now.Add(2*time.Minute))
		must(t, err)
		if out != store.OutcomeRotated {
			t.Fatalf("second rotation outcome = %s", out)
		}
		f, err := s.GetRefreshFamily(ctx, "fam1")
		must(t, err)
		if f.CurrentHash != "r2" || f.PrevHash != "r1" || f.Revoked {
			t.Fatalf("family = %+v", f)
		}
		if _, err := s.RotateRefresh(ctx, "nope", "x", "y", now); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("unknown family err = %v", err)
		}
		if _, err := s.RotateRefresh(ctx, "fam1", "", "y", now); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("empty token err = %v, want ErrNotFound", err)
		}
	})

	t.Run("RefreshConcurrentRotationOneWinner", func(t *testing.T) {
		s := newStore(t)
		must(t, s.CreateRefreshFamily(ctx, newFamily("fam1")))
		var mu sync.Mutex
		outcomes := map[store.RotateOutcome]int{}
		var wg sync.WaitGroup
		for i := range 2 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				out, err := s.RotateRefresh(ctx, "fam1", "r0", fmt.Sprintf("new-%d", i), now.Add(time.Minute))
				if err != nil {
					t.Errorf("rotate: %v", err)
					return
				}
				mu.Lock()
				outcomes[out]++
				mu.Unlock()
			}()
		}
		wg.Wait()
		if outcomes[store.OutcomeRotated] != 1 || outcomes[store.OutcomeGraceReplay] != 1 {
			t.Fatalf("outcomes = %v, want one rotated and one grace replay", outcomes)
		}
		f, _ := s.GetRefreshFamily(ctx, "fam1")
		if f.Revoked {
			t.Fatal("family revoked by a legitimate concurrent refresh")
		}
	})

	t.Run("RefreshGraceReplayOnce", func(t *testing.T) {
		s := newStore(t)
		must(t, s.CreateRefreshFamily(ctx, newFamily("fam1")))
		t0 := now.Add(time.Minute)
		_, err := s.RotateRefresh(ctx, "fam1", "r0", "r1", t0)
		must(t, err)
		out, err := s.RotateRefresh(ctx, "fam1", "r0", "ignored", t0.Add(5*time.Second))
		must(t, err)
		if out != store.OutcomeGraceReplay {
			t.Fatalf("replay within grace = %s, want grace_replay", out)
		}
		out, err = s.RotateRefresh(ctx, "fam1", "r0", "ignored", t0.Add(6*time.Second))
		must(t, err)
		if out != store.OutcomeReused {
			t.Fatalf("second replay = %s, want reused", out)
		}
		if f, _ := s.GetRefreshFamily(ctx, "fam1"); !f.Revoked {
			t.Fatal("family not revoked after the second replay")
		}
	})

	t.Run("RefreshReuseAfterGraceRevokesFamilyAndSessions", func(t *testing.T) {
		s := newStore(t)
		must(t, s.CreateRefreshFamily(ctx, newFamily("fam1")))
		must(t, s.CreateSession(ctx, store.Session{Hash: "acc1", Kind: store.SessionAccess, Owner: "pat", FamilyID: "fam1",
			Created: now, Expires: now.Add(time.Hour)}))
		t0 := now.Add(time.Minute)
		_, err := s.RotateRefresh(ctx, "fam1", "r0", "r1", t0)
		must(t, err)
		out, err := s.RotateRefresh(ctx, "fam1", "r0", "x", t0.Add(store.RefreshGrace+time.Second))
		must(t, err)
		if out != store.OutcomeReused {
			t.Fatalf("reuse after grace = %s, want reused", out)
		}
		if f, _ := s.GetRefreshFamily(ctx, "fam1"); !f.Revoked {
			t.Fatal("family not revoked")
		}
		if got, _ := s.GetSession(ctx, "acc1"); !got.Revoked {
			t.Fatal("family's access token not revoked")
		}
		out, err = s.RotateRefresh(ctx, "fam1", "r1", "r2", t0.Add(time.Minute))
		must(t, err)
		if out != store.OutcomeRevoked {
			t.Fatalf("rotation of a revoked family = %s, want revoked", out)
		}
		if out, _ := s.RotateRefresh(ctx, "fam1", "garbage", "r3", t0); out != store.OutcomeRevoked {
			t.Fatalf("garbage on a revoked family = %s", out)
		}
	})

	t.Run("RefreshUnknownTokenRevokesFamily", func(t *testing.T) {
		s := newStore(t)
		must(t, s.CreateRefreshFamily(ctx, newFamily("fam1")))
		out, err := s.RotateRefresh(ctx, "fam1", "never-issued", "x", now.Add(time.Minute))
		must(t, err)
		if out != store.OutcomeReused {
			t.Fatalf("outcome = %s, want reused", out)
		}
	})

	t.Run("RefreshExpiry", func(t *testing.T) {
		s := newStore(t)
		must(t, s.CreateRefreshFamily(ctx, newFamily("fam1")))
		out, err := s.RotateRefresh(ctx, "fam1", "r0", "r1", now.Add(store.RefreshIdleTTL+time.Hour))
		must(t, err)
		if out != store.OutcomeExpired {
			t.Fatalf("idle expiry outcome = %s", out)
		}
		must(t, s.CreateRefreshFamily(ctx, newFamily("fam2")))
		cur, t0 := "r0", now
		for i := range 5 { // keep it warm past the absolute limit
			t0 = t0.Add(25 * 24 * time.Hour)
			next := fmt.Sprintf("r%d", i+1)
			out, err := s.RotateRefresh(ctx, "fam2", cur, next, t0)
			must(t, err)
			if t0.After(now.Add(store.RefreshAbsoluteTTL)) {
				if out != store.OutcomeExpired {
					t.Fatalf("rotation at day %d = %s, want expired", (i+1)*25, out)
				}
				return
			}
			if out != store.OutcomeRotated {
				t.Fatalf("rotation %d = %s", i, out)
			}
			cur = next
		}
		t.Fatal("absolute expiry never hit")
	})

	t.Run("RefreshGracePairHandedToTheLoser", func(t *testing.T) {
		s := newStore(t)
		must(t, s.CreateRefreshFamily(ctx, newFamily("fam1")))
		t0 := now.Add(time.Minute)
		out, err := s.RotateRefreshWithPair(ctx, "fam1", "r0", "r1", []byte("sealed-pair"), t0)
		must(t, err)
		if out != store.OutcomeRotated {
			t.Fatalf("rotation = %s", out)
		}
		out, err = s.RotateRefreshWithPair(ctx, "fam1", "r0", "ignored", []byte("loser"), t0.Add(2*time.Second))
		must(t, err)
		if out != store.OutcomeGraceReplay {
			t.Fatalf("replay = %s, want grace_replay", out)
		}
		f, err := s.GetRefreshFamily(ctx, "fam1")
		must(t, err)
		if string(f.GracePair) != "sealed-pair" || f.CurrentHash != "r1" {
			t.Fatalf("family after replay = pair %q current %q; the winner's pair must survive", f.GracePair, f.CurrentHash)
		}
		if _, err := s.RotateRefreshWithPair(ctx, "fam1", "r0", "x", nil, t0.Add(3*time.Second)); err != nil {
			t.Fatal(err)
		}
		if f, _ = s.GetRefreshFamily(ctx, "fam1"); len(f.GracePair) != 0 || !f.Revoked {
			t.Fatalf("a second replay must revoke the family and drop the pair: %+v", f)
		}
	})

	t.Run("AuthTxnTakenOnce", func(t *testing.T) {
		s := newStore(t)
		tx := store.AuthTxn{Hash: "tx1", Owner: "pat", ClientID: "c", RedirectURI: "https://x/cb", CodeChallenge: "ch",
			Resource: "https://m/mcp", State: "st", RequestID: "req1", PayloadSHA256: "sha", Expires: now.Add(10 * time.Minute)}
		must(t, s.PutAuthTxn(ctx, tx))
		if err := s.PutAuthTxn(ctx, tx); !errors.Is(err, store.ErrConflict) {
			t.Fatalf("duplicate txn err = %v", err)
		}
		got, err := s.GetAuthTxn(ctx, "tx1", now)
		must(t, err)
		if got.RequestID != "req1" || got.State != "st" || got.Resource != "https://m/mcp" {
			t.Fatalf("txn = %+v", got)
		}
		if _, err := s.GetAuthTxn(ctx, "tx1", now.Add(11*time.Minute)); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("expired get err = %v, want ErrNotFound", err)
		}
		if _, err := s.TakeAuthTxn(ctx, "tx1", now); err != nil {
			t.Fatal(err)
		}
		if _, err := s.TakeAuthTxn(ctx, "tx1", now); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("second take err = %v, want ErrNotFound", err)
		}
		if _, err := s.GetAuthTxn(ctx, "tx1", now); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("get after take err = %v", err)
		}
	})

	t.Run("AuthTxnConcurrentTake", func(t *testing.T) {
		s := newStore(t)
		must(t, s.PutAuthTxn(ctx, store.AuthTxn{Hash: "tx1", Owner: "pat", Expires: now.Add(time.Minute)}))
		var won atomic.Int32
		var wg sync.WaitGroup
		for range 8 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if _, err := s.TakeAuthTxn(ctx, "tx1", now); err == nil {
					won.Add(1)
				}
			}()
		}
		wg.Wait()
		if won.Load() != 1 {
			t.Fatalf("%d takes won, want 1", won.Load())
		}
	})

	t.Run("ConnectStateTakenOnce", func(t *testing.T) {
		s := newStore(t)
		c := store.ConnectState{Hash: "st1", Owner: "pat", Verifier: "v", Provider: store.ProviderMicrosoft, ReconnectID: "acc_1", Expires: now.Add(10 * time.Minute)}
		must(t, s.PutConnectState(ctx, c))
		if err := s.PutConnectState(ctx, c); !errors.Is(err, store.ErrConflict) {
			t.Fatalf("duplicate state err = %v", err)
		}
		got, err := s.TakeConnectState(ctx, "st1", now)
		must(t, err)
		if got.Verifier != "v" || got.ReconnectID != "acc_1" || got.Provider != store.ProviderMicrosoft {
			t.Fatalf("state = %+v", got)
		}
		if _, err := s.TakeConnectState(ctx, "st1", now); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("second take err = %v, want ErrNotFound", err)
		}
		must(t, s.PutConnectState(ctx, store.ConnectState{Hash: "st2", Owner: "pat", Expires: now.Add(time.Minute)}))
		if _, err := s.TakeConnectState(ctx, "st2", now.Add(2*time.Minute)); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("expired state err = %v, want ErrNotFound", err)
		}
	})

	t.Run("ConnectStateConcurrentTake", func(t *testing.T) {
		s := newStore(t)
		must(t, s.PutConnectState(ctx, store.ConnectState{Hash: "st1", Owner: "pat", Expires: now.Add(time.Minute)}))
		var won atomic.Int32
		var wg sync.WaitGroup
		for range 8 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if _, err := s.TakeConnectState(ctx, "st1", now); err == nil {
					won.Add(1)
				}
			}()
		}
		wg.Wait()
		if won.Load() != 1 {
			t.Fatalf("%d takes won, want 1", won.Load())
		}
	})

	t.Run("ApprovalIdempotentCreateAndTransitions", func(t *testing.T) {
		s := newStore(t)
		a := store.Approval{ID: "req1", Owner: "pat", Kind: store.KindEmailSend, AccountID: "acc_1", IdempotencyKey: "k1",
			PayloadSHA256: "abc", Snapshot: []byte("sealed"), Created: now, Expires: now.Add(15 * time.Minute)}
		got, created, err := s.CreateApproval(ctx, a, now)
		must(t, err)
		if !created || got.Status != store.ApprovalPending || !got.TTL.Equal(sec(now.Add(store.ApprovalTTL))) {
			t.Fatalf("created = %v %+v", created, got)
		}
		dup := a
		dup.ID = "req2"
		again, created, err := s.CreateApproval(ctx, dup, now)
		must(t, err)
		if created || again.ID != "req1" {
			t.Fatalf("same idempotency key returned created=%v id=%s, want existing req1", created, again.ID)
		}
		if _, err := s.GetApproval(ctx, "req2"); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("a duplicate key left request req2 behind: %v", err)
		}
		same := a
		same.IdempotencyKey = "k2"
		if _, _, err := s.CreateApproval(ctx, same, now); !errors.Is(err, store.ErrConflict) {
			t.Fatalf("duplicate id err = %v, want ErrConflict", err)
		}
		if _, err := s.TransitionApproval(ctx, "req1", []string{store.ApprovalApproved}, store.ApprovalConsumed, false); !errors.Is(err, store.ErrInvalidTransition) {
			t.Fatalf("transition from the wrong state err = %v", err)
		}
		ap, err := s.TransitionApproval(ctx, "req1", []string{store.ApprovalPending}, store.ApprovalApproved, false)
		must(t, err)
		if ap.Status != store.ApprovalApproved || string(ap.Snapshot) != "sealed" {
			t.Fatalf("approved = %+v", ap)
		}
		ap, err = s.TransitionApproval(ctx, "req1", []string{store.ApprovalApproved}, store.ApprovalConsumed, true)
		must(t, err)
		if ap.Status != store.ApprovalConsumed || len(ap.Snapshot) != 0 {
			t.Fatalf("consumed = %+v, want snapshot cleared", ap)
		}
		if _, err := s.TransitionApproval(ctx, "nope", []string{store.ApprovalPending}, store.ApprovalDenied, true); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("missing approval err = %v", err)
		}
	})

	t.Run("ApprovalConcurrentTransitionOneWinner", func(t *testing.T) {
		s := newStore(t)
		_, _, err := s.CreateApproval(ctx, store.Approval{ID: "req1", Owner: "pat", Kind: store.KindEmailSend, IdempotencyKey: "k1"}, now)
		must(t, err)
		var won atomic.Int32
		var wg sync.WaitGroup
		for range 8 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if _, err := s.TransitionApproval(ctx, "req1", []string{store.ApprovalPending}, store.ApprovalApproved, false); err == nil {
					won.Add(1)
				}
			}()
		}
		wg.Wait()
		if won.Load() != 1 {
			t.Fatalf("%d transitions won, want 1", won.Load())
		}
	})

	t.Run("ApprovalLookupByKeyAndByAccountKind", func(t *testing.T) {
		s := newStore(t)
		mk := func(id, key, kind, acct string, at time.Time) {
			_, _, err := s.CreateApproval(ctx, store.Approval{ID: id, Owner: "pat", Kind: kind, AccountID: acct, IdempotencyKey: key, ThreadID: "th-" + id, Created: at}, at)
			must(t, err)
		}
		mk("e1", "k1", store.KindAccountSendEnable, "acc_1", now)
		mk("e2", "k2", store.KindAccountSendEnable, "acc_1", now.Add(time.Minute))
		mk("e3", "k3", store.KindAccountSendEnable, "acc_2", now)
		mk("m1", "k4", store.KindEmailSend, "acc_1", now)
		mk("d1", "k5", store.KindDashboardSession, "", now)
		got, err := s.FindApprovals(ctx, store.KindAccountSendEnable, "acc_1")
		must(t, err)
		if len(got) != 2 || got[0].ID != "e2" || got[1].ID != "e1" {
			t.Fatalf("FindApprovals = %+v, want e2 then e1 (newest first, one account, one kind)", got)
		}
		// The index follows status changes: it holds the row, not a copy.
		_, err = s.TransitionApproval(ctx, "e2", []string{store.ApprovalPending}, store.ApprovalDenied, false)
		must(t, err)
		if got, _ = s.FindApprovals(ctx, store.KindAccountSendEnable, "acc_1"); got[0].Status != store.ApprovalDenied {
			t.Fatalf("index row is stale: %+v", got[0])
		}
		if got, err = s.FindApprovals(ctx, store.KindAccountSendEnable, "acc_none"); err != nil || len(got) != 0 {
			t.Fatalf("unknown account = %+v, %v", got, err)
		}
		if got, err = s.FindApprovals(ctx, store.KindEmailSend, ""); err != nil || len(got) != 0 {
			t.Fatalf("no account = %+v, %v", got, err)
		}
		byKey, err := s.FindApprovalByKey(ctx, "k4")
		must(t, err)
		if byKey.ID != "m1" || byKey.ThreadID != "th-m1" {
			t.Fatalf("FindApprovalByKey = %+v", byKey)
		}
		if _, err := s.FindApprovalByKey(ctx, "nope"); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("unknown key err = %v", err)
		}
	})

	t.Run("DraftMetaRoundTripAndTTL", func(t *testing.T) {
		s := newStore(t)
		if _, err := s.GetDraftMeta(ctx, "acc_1", "d1"); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("missing draft meta err = %v", err)
		}
		must(t, s.PutDraftMeta(ctx, store.DraftMeta{AccountID: "acc_1", DraftID: "d1", ThreadID: "t1", ReplyAccountID: "acc_9", Updated: now}))
		got, err := s.GetDraftMeta(ctx, "acc_1", "d1")
		must(t, err)
		if got.ThreadID != "t1" || got.ReplyAccountID != "acc_9" {
			t.Fatalf("meta = %+v", got)
		}
		if _, err := s.GetDraftMeta(ctx, "acc_2", "d1"); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("draft notes crossed accounts: %v", err)
		}
		// Notes share the account partition with contacts without disturbing them.
		must(t, s.PutContact(ctx, store.Contact{AccountID: "acc_1", Address: "a@x.example", Trust: store.TrustKnown}))
		cs, err := s.ListContacts(ctx, "acc_1", store.ContactFilter{})
		must(t, err)
		if len(cs) != 1 {
			t.Fatalf("contacts = %+v, draft notes must not appear", cs)
		}
		if err := s.PutDraftMeta(ctx, store.DraftMeta{AccountID: "acc_1"}); err == nil {
			t.Fatal("a draft meta without a draft id must be refused")
		}
	})

	t.Run("KillSwitch", func(t *testing.T) {
		s := newStore(t)
		ks, err := s.KillSwitch(ctx)
		must(t, err)
		if ks.On {
			t.Fatal("an unset kill switch must be off")
		}
		must(t, s.SetKillSwitch(ctx, true, "incident", now))
		ks, err = s.KillSwitch(ctx)
		must(t, err)
		if !ks.On || ks.Reason != "incident" || !ks.At.Equal(now) {
			t.Fatalf("kill switch = %+v", ks)
		}
		must(t, s.SetKillSwitch(ctx, false, "", now.Add(time.Minute)))
		if ks, _ = s.KillSwitch(ctx); ks.On {
			t.Fatal("the kill switch did not turn off")
		}
	})

	claim := func(id, acct string, at time.Time) store.SendRow {
		return store.SendRow{RequestID: id, Owner: "pat", AccountID: acct, Created: at}
	}

	t.Run("SendClaimOnce", func(t *testing.T) {
		s := newStore(t)
		must(t, s.ClaimSend(ctx, claim("r1", "acc_1", now), 5))
		if err := s.ClaimSend(ctx, claim("r1", "acc_1", now), 5); !errors.Is(err, store.ErrAlreadyClaimed) {
			t.Fatalf("second claim err = %v, want ErrAlreadyClaimed", err)
		}
		if n, _ := s.HourlyCount(ctx, "acc_1", now); n != 1 {
			t.Fatalf("a rejected claim moved the cap counter to %d, want 1", n)
		}
		row, err := s.GetSend(ctx, "r1")
		must(t, err)
		if row.State != store.SendClaimed || row.Hour != store.HourKey(now) || !row.TTL.Equal(sec(now.Add(store.LedgerTTL))) {
			t.Fatalf("ledger row = %+v", row)
		}
		if _, err := s.GetSend(ctx, "nope"); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("missing ledger row err = %v", err)
		}
	})

	t.Run("SendClaimRateLimitedLeavesNoRow", func(t *testing.T) {
		s := newStore(t)
		must(t, s.ClaimSend(ctx, claim("r1", "acc_1", now), 1))
		if err := s.ClaimSend(ctx, claim("r2", "acc_1", now), 1); !errors.Is(err, store.ErrRateLimited) {
			t.Fatalf("claim at cap err = %v, want ErrRateLimited", err)
		}
		if _, err := s.GetSend(ctx, "r2"); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("rate-limited claim left a ledger row: %v", err)
		}
		if err := s.ClaimSend(ctx, claim("r3", "acc_1", now), 0); !errors.Is(err, store.ErrRateLimited) {
			t.Fatalf("zero cap err = %v", err)
		}
		if err := s.ClaimSend(ctx, claim("r4", "acc_2", now), 1); err != nil {
			t.Fatalf("another account's cap is independent: %v", err)
		}
		if err := s.ClaimSend(ctx, claim("r5", "acc_1", now.Add(time.Hour)), 1); err != nil {
			t.Fatalf("next hour starts a new counter: %v", err)
		}
	})

	t.Run("ConcurrentClaimsAtCapMinusOneProduceOneClaim", func(t *testing.T) {
		s := newStore(t)
		const limit = 3
		for i := range limit - 1 {
			must(t, s.ClaimSend(ctx, claim(fmt.Sprintf("seed%d", i), "acc_1", now), limit))
		}
		var won, limited atomic.Int32
		var wg sync.WaitGroup
		for i := range 10 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				switch err := s.ClaimSend(ctx, claim(fmt.Sprintf("race%d", i), "acc_1", now), limit); {
				case err == nil:
					won.Add(1)
				case errors.Is(err, store.ErrRateLimited):
					limited.Add(1)
				default:
					t.Errorf("unexpected error %v", err)
				}
			}()
		}
		wg.Wait()
		if won.Load() != 1 || limited.Load() != 9 {
			t.Fatalf("won=%d limited=%d, want 1 and 9", won.Load(), limited.Load())
		}
		if n, _ := s.HourlyCount(ctx, "acc_1", now); n != limit {
			t.Fatalf("count = %d, want %d", n, limit)
		}
	})

	t.Run("ConcurrentClaimsOfOneRequestProduceOneClaim", func(t *testing.T) {
		s := newStore(t)
		var won, dup atomic.Int32
		var wg sync.WaitGroup
		for range 10 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				switch err := s.ClaimSend(ctx, claim("same", "acc_1", now), 100); {
				case err == nil:
					won.Add(1)
				case errors.Is(err, store.ErrAlreadyClaimed):
					dup.Add(1)
				default:
					t.Errorf("unexpected error %v", err)
				}
			}()
		}
		wg.Wait()
		if won.Load() != 1 || dup.Load() != 9 {
			t.Fatalf("won=%d dup=%d, want 1 and 9", won.Load(), dup.Load())
		}
		if n, _ := s.HourlyCount(ctx, "acc_1", now); n != 1 {
			t.Fatalf("count = %d, want 1: losers must not reserve cap", n)
		}
	})

	t.Run("SendStateMachineConditionalOnPreviousState", func(t *testing.T) {
		s := newStore(t)
		must(t, s.ClaimSend(ctx, claim("r1", "acc_1", now), 5))
		if _, err := s.AdvanceSend(ctx, "r1", store.SendDispatching, store.SendSent, "m1", now); !errors.Is(err, store.ErrConflict) {
			t.Fatalf("advance from the wrong state err = %v, want ErrConflict", err)
		}
		row, err := s.AdvanceSend(ctx, "r1", store.SendClaimed, store.SendDispatching, "", now)
		must(t, err)
		if row.State != store.SendDispatching {
			t.Fatalf("row = %+v", row)
		}
		row, err = s.AdvanceSend(ctx, "r1", store.SendDispatching, store.SendSent, "gmail-123", now)
		must(t, err)
		if row.State != store.SendSent || row.ProviderMessageID != "gmail-123" {
			t.Fatalf("row = %+v", row)
		}
		if _, err := s.AdvanceSend(ctx, "nope", store.SendClaimed, store.SendDispatching, "", now); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("missing row err = %v", err)
		}
	})

	t.Run("PatchSendWritesStagingAndCorrelationFieldsConditionally", func(t *testing.T) {
		s := newStore(t)
		must(t, s.ClaimSend(ctx, claim("p1", "acc_1", now), 5))
		row, err := s.PatchSend(ctx, "p1", store.SendClaimed, store.SendClaimed, store.SendPatch{StagingID: "stage-1", StagingMessageID: "<m1@example.com>"}, now)
		must(t, err)
		if row.State != store.SendClaimed || row.StagingID != "stage-1" || row.StagingMessageID != "<m1@example.com>" || !row.SendIssuedAt.IsZero() {
			t.Fatalf("row = %+v", row)
		}
		issued := now.Add(time.Second)
		row, err = s.PatchSend(ctx, "p1", store.SendClaimed, store.SendDispatching, store.SendPatch{SendIssuedAt: issued}, issued)
		must(t, err)
		if row.State != store.SendDispatching || !row.SendIssuedAt.Equal(issued) || row.StagingID != "stage-1" {
			t.Fatalf("a patch must keep the fields it does not set: %+v", row)
		}
		if _, err := s.PatchSend(ctx, "p1", store.SendClaimed, store.SendSubmitted, store.SendPatch{}, now); !errors.Is(err, store.ErrConflict) {
			t.Fatalf("patch from the wrong state err = %v, want ErrConflict", err)
		}
		if _, err := s.PatchSend(ctx, "nope", store.SendClaimed, store.SendClaimed, store.SendPatch{}, now); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("missing row err = %v", err)
		}
		row, err = s.PatchSend(ctx, "p1", store.SendDispatching, store.SendSubmitted, store.SendPatch{}, now)
		must(t, err)
		row, err = s.PatchSend(ctx, "p1", store.SendSubmitted, store.SendSent, store.SendPatch{SentID: "sent-1", ProviderMessageID: "sent-1", ReconcileAttempts: 2, LastReconcile: now}, now)
		must(t, err)
		got, err := s.GetSend(ctx, "p1")
		must(t, err)
		if got.State != store.SendSent || got.SentID != "sent-1" || got.ProviderMessageID != "sent-1" || got.StagingID != "stage-1" || got.ReconcileAttempts != 2 || !got.SendIssuedAt.Equal(issued) {
			t.Fatalf("row = %+v", got)
		}
		// The new states are reachable from AdvanceSend too, and legacy rows
		// (no staging fields) keep working.
		must(t, s.ClaimSend(ctx, claim("p2", "acc_1", now), 5))
		_, err = s.AdvanceSend(ctx, "p2", store.SendClaimed, store.SendDispatching, "", now)
		must(t, err)
		_, err = s.AdvanceSend(ctx, "p2", store.SendDispatching, store.SendSubmitted, "", now)
		must(t, err)
		legacy, err := s.AdvanceSend(ctx, "p2", store.SendSubmitted, store.SendSentMismatch, "", now)
		must(t, err)
		if legacy.State != store.SendSentMismatch || legacy.StagingID != "" {
			t.Fatalf("row = %+v", legacy)
		}
	})

	t.Run("SendSweepIndexHoldsStagedRowsUntilSweepDone", func(t *testing.T) {
		s := newStore(t)
		for _, id := range []string{"w1", "w2", "w3"} {
			must(t, s.ClaimSend(ctx, claim(id, "acc_1", now), 9))
		}
		if rows, err := s.ListSendSweep(ctx); err != nil || len(rows) != 0 {
			t.Fatalf("rows with no staging draft must not be listed: %v %v", rows, err)
		}
		_, err := s.PatchSend(ctx, "w1", store.SendClaimed, store.SendClaimed, store.SendPatch{StagingID: "s1"}, now)
		must(t, err)
		_, err = s.PatchSend(ctx, "w2", store.SendClaimed, store.SendClaimed, store.SendPatch{StagingID: "s2"}, now)
		must(t, err)
		// A later transition through AdvanceSend or FailSend keeps the row indexed.
		must(t, s.FailSend(ctx, "w2", store.SendClaimed, now))
		rows, err := s.ListSendSweep(ctx)
		must(t, err)
		if len(rows) != 2 || rows[0].RequestID != "w1" || rows[1].RequestID != "w2" || rows[1].State != store.SendFailed {
			t.Fatalf("rows = %+v", rows)
		}
		_, err = s.PatchSend(ctx, "w2", store.SendFailed, store.SendFailed, store.SendPatch{SweepDone: true}, now)
		must(t, err)
		rows, err = s.ListSendSweep(ctx)
		must(t, err)
		if len(rows) != 1 || rows[0].RequestID != "w1" {
			t.Fatalf("a row with sweep_done must leave the index: %+v", rows)
		}
	})

	t.Run("ProviderKillSwitchIsScopedToOneProvider", func(t *testing.T) {
		s := newStore(t)
		if ks, err := s.ProviderKillSwitch(ctx, store.ProviderMicrosoft); err != nil || ks.On {
			t.Fatalf("an unset provider switch must be off: %+v %v", ks, err)
		}
		must(t, s.SetProviderKillSwitch(ctx, store.ProviderMicrosoft, true, "sent_mismatch", now))
		ks, err := s.ProviderKillSwitch(ctx, store.ProviderMicrosoft)
		must(t, err)
		if !ks.On || ks.Reason != "sent_mismatch" || !ks.At.Equal(now) {
			t.Fatalf("switch = %+v", ks)
		}
		if g, _ := s.ProviderKillSwitch(ctx, store.ProviderGoogle); g.On {
			t.Fatal("the Microsoft switch must not turn Google off")
		}
		if g, _ := s.KillSwitch(ctx); g.On {
			t.Fatal("a provider switch must not set the global switch")
		}
		must(t, s.SetProviderKillSwitch(ctx, store.ProviderMicrosoft, false, "", now.Add(time.Minute)))
		if ks, _ = s.ProviderKillSwitch(ctx, store.ProviderMicrosoft); ks.On {
			t.Fatal("the provider switch did not turn off")
		}
	})

	t.Run("FailSendReleasesCapSlot", func(t *testing.T) {
		s := newStore(t)
		must(t, s.ClaimSend(ctx, claim("r1", "acc_1", now), 1))
		_, err := s.AdvanceSend(ctx, "r1", store.SendClaimed, store.SendDispatching, "", now)
		must(t, err)
		if err := s.FailSend(ctx, "r1", store.SendClaimed, now); !errors.Is(err, store.ErrConflict) {
			t.Fatalf("fail from the wrong state err = %v", err)
		}
		must(t, s.FailSend(ctx, "r1", store.SendDispatching, now))
		row, _ := s.GetSend(ctx, "r1")
		if row.State != store.SendFailed {
			t.Fatalf("state = %s", row.State)
		}
		if n, _ := s.HourlyCount(ctx, "acc_1", now); n != 0 {
			t.Fatalf("count = %d after a failed send, want 0", n)
		}
		must(t, s.ClaimSend(ctx, claim("r2", "acc_1", now), 1)) // the slot is reusable
		if err := s.FailSend(ctx, "r1", store.SendDispatching, now); !errors.Is(err, store.ErrConflict) {
			t.Fatalf("failing twice err = %v: the slot must be released once", err)
		}
	})

	t.Run("ReserveAndReleaseCap", func(t *testing.T) {
		s := newStore(t)
		must(t, s.ReserveCap(ctx, "acc_1", now, 2))
		must(t, s.ReserveCap(ctx, "acc_1", now, 2))
		if err := s.ReserveCap(ctx, "acc_1", now, 2); !errors.Is(err, store.ErrRateLimited) {
			t.Fatalf("reserve at cap err = %v", err)
		}
		must(t, s.ReleaseCap(ctx, "acc_1", now))
		must(t, s.ReserveCap(ctx, "acc_1", now, 2))
		must(t, s.ReleaseCap(ctx, "acc_1", now))
		must(t, s.ReleaseCap(ctx, "acc_1", now))
		must(t, s.ReleaseCap(ctx, "acc_1", now)) // below zero is a no-op
		if n, _ := s.HourlyCount(ctx, "acc_1", now); n != 0 {
			t.Fatalf("count = %d, want 0", n)
		}
		if n, err := s.HourlyCount(ctx, "acc_never", now); err != nil || n != 0 {
			t.Fatalf("untouched counter = %d, %v", n, err)
		}
	})

	t.Run("ContactsArePerAccount", func(t *testing.T) {
		s := newStore(t)
		must(t, s.PutContact(ctx, store.Contact{AccountID: "acc_1", Address: "Bob@Example.com", Trust: store.TrustTrusted, Tags: []string{"vendor"}}))
		must(t, s.PutContact(ctx, store.Contact{AccountID: "acc_1", Address: "eve@example.com", Trust: store.TrustBlocked}))
		must(t, s.PutContact(ctx, store.Contact{AccountID: "acc_1", Address: "amy@example.com", Trust: store.TrustKnown, Tags: []string{"vendor", "friend"}}))
		must(t, s.PutContact(ctx, store.Contact{AccountID: "acc_2", Address: "bob@example.com", Trust: store.TrustBlocked}))
		got, err := s.GetContact(ctx, "acc_1", "BOB@example.com")
		must(t, err)
		if got.Trust != store.TrustTrusted || got.Address != "bob@example.com" {
			t.Fatalf("contact = %+v", got)
		}
		other, err := s.GetContact(ctx, "acc_2", "bob@example.com")
		must(t, err)
		if other.Trust != store.TrustBlocked {
			t.Fatalf("acc_2 contact = %+v: trust leaked across accounts", other)
		}
		if _, err := s.GetContact(ctx, "acc_3", "bob@example.com"); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("a contact trusted in acc_1 must be new in acc_3: %v", err)
		}
		all, err := s.ListContacts(ctx, "acc_1", store.ContactFilter{})
		must(t, err)
		if len(all) != 3 || all[0].Address != "amy@example.com" {
			t.Fatalf("ListContacts = %+v", all)
		}
		trusted, _ := s.ListContacts(ctx, "acc_1", store.ContactFilter{Trust: store.TrustTrusted})
		if len(trusted) != 1 || trusted[0].Address != "bob@example.com" {
			t.Fatalf("trusted = %+v", trusted)
		}
		vendors, _ := s.ListContacts(ctx, "acc_1", store.ContactFilter{Tag: "vendor"})
		if len(vendors) != 2 {
			t.Fatalf("vendor tag = %+v", vendors)
		}
		if err := s.PutContact(ctx, store.Contact{AccountID: "acc_1", Address: "x@y.z", Trust: "bogus"}); err == nil {
			t.Fatal("unknown trust level accepted")
		}
	})

	t.Run("ApplyContactBatchIsAtomicAndOnce", func(t *testing.T) {
		s := newStore(t)
		must(t, s.PutContact(ctx, store.Contact{AccountID: "acc_1", Address: "a@x.com", Trust: store.TrustKnown, Tags: []string{"vendor"}}))
		must(t, s.PutContact(ctx, store.Contact{AccountID: "acc_1", Address: "bad@x.com", Trust: store.TrustBlocked}))
		row, err := s.ApplyContactBatch(ctx, store.ContactBatchRow{RequestID: "req1", Owner: "pat", AccountID: "acc_1", Created: now},
			[]string{"B@x.com", "a@x.com", "a@x.com", "bad@x.com"})
		must(t, err)
		if len(row.Applied) != 2 || row.Applied[0] != "a@x.com" || row.Applied[1] != "b@x.com" || len(row.Skipped) != 1 || row.Skipped[0] != "bad@x.com" {
			t.Fatalf("row = %+v", row)
		}
		a, _ := s.GetContact(ctx, "acc_1", "a@x.com")
		if a.Trust != store.TrustTrusted || len(a.Tags) != 1 {
			t.Fatalf("a = %+v: trust changes and tags are kept", a)
		}
		if b, err := s.GetContact(ctx, "acc_1", "b@x.com"); err != nil || b.Trust != store.TrustTrusted {
			t.Fatalf("b = %+v, %v", b, err)
		}
		if bad, _ := s.GetContact(ctx, "acc_1", "bad@x.com"); bad.Trust != store.TrustBlocked {
			t.Fatalf("a blocked contact was changed: %+v", bad)
		}
		got, err := s.GetContactBatch(ctx, "req1")
		must(t, err)
		if got.AccountID != "acc_1" || len(got.Applied) != 2 {
			t.Fatalf("ledger = %+v", got)
		}
		// A second apply of the same request changes nothing.
		must(t, s.PutContact(ctx, store.Contact{AccountID: "acc_1", Address: "a@x.com", Trust: store.TrustBlocked}))
		if _, err := s.ApplyContactBatch(ctx, store.ContactBatchRow{RequestID: "req1", Owner: "pat", AccountID: "acc_1"}, []string{"a@x.com", "c@x.com"}); !errors.Is(err, store.ErrConflict) {
			t.Fatalf("second apply err = %v, want ErrConflict", err)
		}
		if a, _ := s.GetContact(ctx, "acc_1", "a@x.com"); a.Trust != store.TrustBlocked {
			t.Fatalf("a replay changed a contact: %+v", a)
		}
		if _, err := s.GetContact(ctx, "acc_1", "c@x.com"); !errors.Is(err, store.ErrNotFound) {
			t.Fatal("a replay created a contact")
		}
		if _, err := s.GetContactBatch(ctx, "nope"); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("missing ledger err = %v", err)
		}
		var many []string
		for i := range store.MaxContactBatch + 1 {
			many = append(many, fmt.Sprintf("u%d@x.com", i))
		}
		if _, err := s.ApplyContactBatch(ctx, store.ContactBatchRow{RequestID: "req2", Owner: "pat", AccountID: "acc_1"}, many); err == nil {
			t.Fatal("an oversize batch was accepted")
		}
		if _, err := s.ApplyContactBatch(ctx, store.ContactBatchRow{RequestID: "req3", Owner: "pat", AccountID: "acc_1"}, nil); err == nil {
			t.Fatal("an empty batch was accepted")
		}
	})

	t.Run("AuditNewestFirstSinceAndTTL", func(t *testing.T) {
		s := newStore(t)
		base := time.Now().UTC().Add(-48 * time.Hour)
		for i := range 5 {
			must(t, s.AppendAudit(ctx, store.AuditEvent{
				At: base.Add(time.Duration(i) * 12 * time.Hour), Owner: "pat", RequestID: fmt.Sprintf("r%d", i),
				AccountID: "acc_1", Tool: "email_search", Recipients: []string{"a@x.example"}, BodySHA256: "deadbeef", ApprovalID: "ap1",
			}))
		}
		must(t, s.AppendAudit(ctx, store.AuditEvent{At: base, Owner: "conner", RequestID: "other", Tool: "email_search"}))
		got, err := s.ListAudit(ctx, "pat", base.Add(-time.Hour), 100)
		must(t, err)
		if len(got) != 5 || got[0].RequestID != "r4" || got[4].RequestID != "r0" {
			t.Fatalf("ListAudit = %v", ids(got))
		}
		if want := sec(got[0].At.Add(store.AuditTTL)); !got[0].ExpiresAt.Equal(want) {
			t.Fatalf("audit ttl = %v, want At+90d = %v", got[0].ExpiresAt, want)
		}
		if got[0].Tool != "email_search" || got[0].BodySHA256 != "deadbeef" || len(got[0].Recipients) != 1 {
			t.Fatalf("audit fields lost: %+v", got[0])
		}
		recent, _ := s.ListAudit(ctx, "pat", base.Add(30*time.Hour), 100)
		if len(recent) != 2 {
			t.Fatalf("since filter = %v", ids(recent))
		}
		limited, _ := s.ListAudit(ctx, "pat", base.Add(-time.Hour), 2)
		if len(limited) != 2 || limited[0].RequestID != "r4" {
			t.Fatalf("limit = %v", ids(limited))
		}
		if none, err := s.ListAudit(ctx, "nobody", time.Time{}, 10); err != nil || len(none) != 0 {
			t.Fatalf("other owner = %v, %v", none, err)
		}
	})
}

func ids(es []store.AuditEvent) []string {
	out := make([]string, len(es))
	for i, e := range es {
		out[i] = e.RequestID
	}
	return out
}
