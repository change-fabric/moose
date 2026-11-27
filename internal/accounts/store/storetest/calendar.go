package storetest

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/change-fabric/moose/internal/accounts/store"
	"github.com/change-fabric/moose/internal/email/provider"
)

// runCalendar covers what the calendar service adds to the shared store: the
// per-service grants and invite opt-in on an account, the grant fields of a
// connect state, the calendar write ledger and the create idempotency records.
func runCalendar(t *testing.T, newStore func(t *testing.T) store.Store) {
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)

	newAcct := func(addr string) store.Account {
		return store.Account{
			Owner: "pat", ID: store.NewAccountID(), Provider: store.ProviderGoogle, ProviderUID: addr, Address: addr,
			Nickname: addr[:3], Status: store.StatusOK, HourlyCap: store.DefaultHourlyCap,
			Caps:         provider.Capabilities{Threads: true},
			RefreshToken: []byte("sealed-" + addr),
			Services:     map[string]store.ServiceGrant{store.ServiceMail: {Scopes: []string{"gmail.readonly"}, GrantedAt: now}},
		}
	}

	t.Run("ServiceGrantsRoundTripAndPatchMerges", func(t *testing.T) {
		s := newStore(t)
		a := newAcct("a@x.example")
		must(t, s.CreateAccount(ctx, a))
		got, _ := s.GetAccount(ctx, "pat", a.ID)
		if g, ok := got.Services[store.ServiceMail]; !ok || len(g.Scopes) != 1 {
			t.Fatalf("created services = %+v", got.Services)
		}
		if _, ok := got.Services[store.ServiceCalendar]; ok {
			t.Fatal("a new account has no calendar grant")
		}
		next := map[string]store.ServiceGrant{
			store.ServiceMail:     got.Services[store.ServiceMail],
			store.ServiceCalendar: {Scopes: []string{"calendar.readonly"}, GrantedAt: now.Add(time.Minute)},
		}
		must(t, s.UpdateAccount(ctx, "pat", a.ID, store.AccountPatch{Services: &next}))
		got, _ = s.GetAccount(ctx, "pat", a.ID)
		cal := got.Services[store.ServiceCalendar]
		if len(cal.Scopes) != 1 || cal.Scopes[0] != "calendar.readonly" || !cal.GrantedAt.Equal(now.Add(time.Minute)) || len(got.Services[store.ServiceMail].Scopes) != 1 {
			t.Fatalf("services after patch = %+v", got.Services)
		}
		if string(got.RefreshToken) != "sealed-a@x.example" || got.InviteEnabled {
			t.Fatalf("a services patch touched other fields: %+v", got)
		}
	})

	t.Run("DisconnectTurnsInvitesOffAndKeepsGrants", func(t *testing.T) {
		s := newStore(t)
		a := newAcct("b@x.example")
		must(t, s.CreateAccount(ctx, a))
		must(t, s.UpdateAccount(ctx, "pat", a.ID, store.AccountPatch{InviteEnabled: store.Ptr(true)}))
		must(t, s.DisconnectAccount(ctx, "pat", a.ID, now))
		got, _ := s.GetAccount(ctx, "pat", a.ID)
		if got.InviteEnabled || got.SendEnabled || len(got.RefreshToken) != 0 {
			t.Fatalf("disconnect left %+v", got)
		}
		if _, ok := got.Services[store.ServiceMail]; !ok {
			t.Fatal("disconnect dropped the recorded grants; a reconnect keeps the account's identity")
		}
	})

	t.Run("ConnectStateCarriesTheGrantBinding", func(t *testing.T) {
		s := newStore(t)
		c := store.ConnectState{Hash: "grant1", Owner: "pat", Verifier: "v", Provider: store.ProviderMicrosoft,
			GrantAccountID: "acc_1", Service: store.ServiceCalendar, Expires: now.Add(10 * time.Minute)}
		must(t, s.PutConnectState(ctx, c))
		got, err := s.TakeConnectState(ctx, "grant1", now)
		must(t, err)
		if got.GrantAccountID != "acc_1" || got.Service != store.ServiceCalendar || got.Provider != store.ProviderMicrosoft || got.ReconnectID != "" {
			t.Fatalf("state = %+v", got)
		}
	})

	row := func(id, acct, op string, at time.Time) store.CalendarRow {
		return store.CalendarRow{RequestID: id, Owner: "pat", AccountID: acct, Operation: op, Created: at}
	}

	t.Run("CalendarClaimOnceSharesTheHourlyCap", func(t *testing.T) {
		s := newStore(t)
		must(t, s.ClaimCalendarRequest(ctx, row("c1", "acc_1", "create", now), 2))
		if err := s.ClaimCalendarRequest(ctx, row("c1", "acc_1", "create", now), 2); !errors.Is(err, store.ErrAlreadyClaimed) {
			t.Fatalf("second claim err = %v, want ErrAlreadyClaimed", err)
		}
		if n, _ := s.HourlyCount(ctx, "acc_1", now); n != 1 {
			t.Fatalf("a rejected claim moved the cap counter to %d, want 1", n)
		}
		got, err := s.GetCalendarRequest(ctx, "c1")
		must(t, err)
		if got.State != store.CalendarClaimed || got.Hour != store.HourKey(now) || got.Operation != "create" || !got.TTL.Equal(now.Add(store.LedgerTTL)) {
			t.Fatalf("row = %+v", got)
		}
		// The counter is the send ledger's: a send claim takes the second slot,
		// and a calendar claim then hits the cap.
		must(t, s.ClaimSend(ctx, store.SendRow{RequestID: "s1", Owner: "pat", AccountID: "acc_1", Created: now}, 2))
		if err := s.ClaimCalendarRequest(ctx, row("c2", "acc_1", "update", now), 2); !errors.Is(err, store.ErrRateLimited) {
			t.Fatalf("claim at the shared cap err = %v, want ErrRateLimited", err)
		}
		if _, err := s.GetCalendarRequest(ctx, "c2"); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("a rate-limited claim left a row: %v", err)
		}
		if err := s.ClaimCalendarRequest(ctx, row("c3", "acc_1", "update", now), 0); !errors.Is(err, store.ErrRateLimited) {
			t.Fatalf("zero cap err = %v", err)
		}
		// A calendar request id never collides with a send request id.
		if _, err := s.GetSend(ctx, "c1"); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("calendar row visible as a send row: %v", err)
		}
		if _, err := s.GetCalendarRequest(ctx, "s1"); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("send row visible as a calendar row: %v", err)
		}
	})

	t.Run("CalendarStateMachineIsConditionalOnPreviousState", func(t *testing.T) {
		s := newStore(t)
		must(t, s.ClaimCalendarRequest(ctx, row("c1", "acc_1", "cancel", now), 5))
		got, err := s.AdvanceCalendarRequest(ctx, "c1", store.CalendarClaimed, store.CalendarDispatching, "", now)
		must(t, err)
		if got.State != store.CalendarDispatching || got.ProviderEventID != "" {
			t.Fatalf("after advance = %+v", got)
		}
		if _, err := s.AdvanceCalendarRequest(ctx, "c1", store.CalendarClaimed, store.CalendarApplied, "ev1", now); !errors.Is(err, store.ErrConflict) {
			t.Fatalf("advance from the wrong state err = %v, want ErrConflict", err)
		}
		got, err = s.AdvanceCalendarRequest(ctx, "c1", store.CalendarDispatching, store.CalendarApplied, "ev1", now)
		must(t, err)
		if got.State != store.CalendarApplied || got.ProviderEventID != "ev1" {
			t.Fatalf("after apply = %+v", got)
		}
		if _, err := s.AdvanceCalendarRequest(ctx, "nope", store.CalendarClaimed, store.CalendarApplied, "", now); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("missing row err = %v", err)
		}
	})

	t.Run("CalendarFailReleasesTheCapSlotAndRecordsTheReason", func(t *testing.T) {
		s := newStore(t)
		must(t, s.ClaimCalendarRequest(ctx, store.CalendarRow{RequestID: "f1", Owner: "pat", AccountID: "acc_f", Operation: "update", Created: now, Version: "v1", ProviderEventID: "ev9", DedupeKey: "dk"}, 1))
		if err := s.ClaimCalendarRequest(ctx, row("f2", "acc_f", "update", now), 1); !errors.Is(err, store.ErrRateLimited) {
			t.Fatalf("second claim at cap 1 err = %v", err)
		}
		if err := s.FailCalendarRequest(ctx, "f1", store.CalendarDispatching, "stale", now); !errors.Is(err, store.ErrConflict) {
			t.Fatalf("fail from the wrong state err = %v, want ErrConflict", err)
		}
		if n, _ := s.HourlyCount(ctx, "acc_f", now); n != 1 {
			t.Fatalf("a refused fail moved the counter to %d", n)
		}
		must(t, s.FailCalendarRequest(ctx, "f1", store.CalendarClaimed, "stale", now))
		got, err := s.GetCalendarRequest(ctx, "f1")
		must(t, err)
		if got.State != store.CalendarFailed || got.Reason != "stale" || got.Version != "v1" || got.ProviderEventID != "ev9" || got.DedupeKey != "dk" {
			t.Fatalf("failed row = %+v", got)
		}
		if n, _ := s.HourlyCount(ctx, "acc_f", now); n != 0 {
			t.Fatalf("cap counter after fail = %d, want the slot released", n)
		}
		must(t, s.ClaimCalendarRequest(ctx, row("f3", "acc_f", "create", now), 1))
		if err := s.FailCalendarRequest(ctx, "f1", store.CalendarClaimed, "x", now); !errors.Is(err, store.ErrConflict) {
			t.Fatalf("second fail of one row err = %v, want ErrConflict, and no second release", err)
		}
		if n, _ := s.HourlyCount(ctx, "acc_f", now); n != 1 {
			t.Fatalf("cap counter = %d, want f3's slot only", n)
		}
		if err := s.FailCalendarRequest(ctx, "nope", store.CalendarClaimed, "x", now); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("missing row err = %v", err)
		}
	})

	t.Run("ConcurrentCalendarClaimsOfOneRequestProduceOneClaim", func(t *testing.T) {
		s := newStore(t)
		var wins, dups atomic.Int32
		var wg sync.WaitGroup
		for range 8 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				switch err := s.ClaimCalendarRequest(ctx, row("c1", "acc_1", "create", now), 10); {
				case err == nil:
					wins.Add(1)
				case errors.Is(err, store.ErrAlreadyClaimed):
					dups.Add(1)
				default:
					t.Errorf("claim: %v", err)
				}
			}()
		}
		wg.Wait()
		if wins.Load() != 1 || dups.Load() != 7 {
			t.Fatalf("wins=%d dups=%d, want 1 and 7", wins.Load(), dups.Load())
		}
		if n, _ := s.HourlyCount(ctx, "acc_1", now); n != 1 {
			t.Fatalf("cap counter = %d, want 1", n)
		}
	})

	idem := func(acct, key, hash string) store.CalendarIdempotency {
		return store.CalendarIdempotency{AccountID: acct, Key: key, RequestHash: hash}
	}

	t.Run("CalendarIdempotencyReplaysTheFirstResult", func(t *testing.T) {
		s := newStore(t)
		got, created, err := s.ReserveCalendarIdempotency(ctx, idem("acc_1", "k1", "h1"))
		must(t, err)
		if !created || got.EventID != "" || got.Created.IsZero() {
			t.Fatalf("first reserve = %+v created=%v", got, created)
		}
		// A retry before the create finished sees the open reservation.
		got, created, err = s.ReserveCalendarIdempotency(ctx, idem("acc_1", "k1", "h1"))
		must(t, err)
		if created || got.EventID != "" {
			t.Fatalf("retry in flight = %+v created=%v", got, created)
		}
		done, err := s.CompleteCalendarIdempotency(ctx, "acc_1", "k1", "ev1")
		must(t, err)
		if done.EventID != "ev1" {
			t.Fatalf("complete = %+v", done)
		}
		got, created, err = s.ReserveCalendarIdempotency(ctx, idem("acc_1", "k1", "h1"))
		must(t, err)
		if created || got.EventID != "ev1" {
			t.Fatalf("replay = %+v created=%v, want the first event", got, created)
		}
		// Completing again with the same id is a no-op; a different id conflicts.
		if _, err := s.CompleteCalendarIdempotency(ctx, "acc_1", "k1", "ev1"); err != nil {
			t.Fatalf("repeat complete: %v", err)
		}
		if _, err := s.CompleteCalendarIdempotency(ctx, "acc_1", "k1", "ev2"); !errors.Is(err, store.ErrConflict) {
			t.Fatalf("complete with another id err = %v, want ErrConflict", err)
		}
		if _, err := s.CompleteCalendarIdempotency(ctx, "acc_1", "missing", "ev1"); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("complete of a missing key err = %v, want ErrNotFound", err)
		}
	})

	t.Run("CalendarIdempotencyKeyWithOtherInputIsRefused", func(t *testing.T) {
		s := newStore(t)
		_, _, err := s.ReserveCalendarIdempotency(ctx, idem("acc_1", "k1", "h1"))
		must(t, err)
		if _, created, err := s.ReserveCalendarIdempotency(ctx, idem("acc_1", "k1", "h2")); !errors.Is(err, store.ErrIdempotencyKeyReused) || created {
			t.Fatalf("same key, other input: created=%v err=%v, want ErrIdempotencyKeyReused", created, err)
		}
		// Keys are per account.
		if _, created, err := s.ReserveCalendarIdempotency(ctx, idem("acc_2", "k1", "h2")); err != nil || !created {
			t.Fatalf("another account's key: created=%v err=%v", created, err)
		}
		if _, _, err := s.ReserveCalendarIdempotency(ctx, idem("acc_1", "", "h")); err == nil {
			t.Fatal("an empty key was accepted")
		}
	})

	t.Run("CalendarIdempotencyReleaseOnlyBeforeAnEventExists", func(t *testing.T) {
		s := newStore(t)
		_, _, err := s.ReserveCalendarIdempotency(ctx, idem("acc_1", "k1", "h1"))
		must(t, err)
		must(t, s.ReleaseCalendarIdempotency(ctx, "acc_1", "k1"))
		if _, created, err := s.ReserveCalendarIdempotency(ctx, idem("acc_1", "k1", "h9")); err != nil || !created {
			t.Fatalf("reserve after release: created=%v err=%v", created, err)
		}
		_, err = s.CompleteCalendarIdempotency(ctx, "acc_1", "k1", "ev1")
		must(t, err)
		if err := s.ReleaseCalendarIdempotency(ctx, "acc_1", "k1"); !errors.Is(err, store.ErrConflict) {
			t.Fatalf("release of a completed key err = %v, want ErrConflict", err)
		}
		must(t, s.ReleaseCalendarIdempotency(ctx, "acc_1", "never-reserved"))
	})

	t.Run("ConcurrentCalendarReservationsHaveOneCreator", func(t *testing.T) {
		s := newStore(t)
		var created atomic.Int32
		var wg sync.WaitGroup
		for range 8 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, c, err := s.ReserveCalendarIdempotency(ctx, idem("acc_1", "k1", "h1"))
				if err != nil {
					t.Errorf("reserve: %v", err)
				}
				if c {
					created.Add(1)
				}
			}()
		}
		wg.Wait()
		if created.Load() != 1 {
			t.Fatalf("%d callers were told they created the key, want 1", created.Load())
		}
	})
}
