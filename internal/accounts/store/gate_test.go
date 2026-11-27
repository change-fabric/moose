package store_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/change-fabric/moose/internal/accounts/store"
	"github.com/change-fabric/moose/internal/accounts/store/sqlite"
)

func TestGateProvidersHidesAccountsOfASwitchedOffProvider(t *testing.T) {
	ctx := context.Background()
	st, err := sqlite.OpenStore(ctx, filepath.Join(t.TempDir(), "e.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	now := time.Now()
	for _, a := range []store.Account{
		{Owner: "pat", ID: "acc_g", Provider: store.ProviderGoogle, ProviderUID: "g@x.com", Address: "g@x.com", Nickname: "g", Status: store.StatusOK, RefreshToken: []byte("sealed-g")},
		{Owner: "pat", ID: "acc_m", Provider: store.ProviderMicrosoft, ProviderUID: "t.o", Address: "m@x.com", Nickname: "m", Status: store.StatusOK, RefreshToken: []byte("sealed-m")},
		{Owner: "pat", ID: "acc_d", Provider: store.ProviderMicrosoft, ProviderUID: "t.p", Address: "d@x.com", Nickname: "d", Status: store.StatusDisconnected},
	} {
		a.Created, a.Updated = now, now
		if err := st.CreateAccount(ctx, a); err != nil {
			t.Fatal(err)
		}
	}
	microsoftOn := false
	gated := store.GateProviders(st, func(p string) bool { return p == store.ProviderGoogle || (p == store.ProviderMicrosoft && microsoftOn) })

	status := func(s store.Store) map[string]string {
		accts, err := s.ListAccounts(ctx, "pat")
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]string{}
		for _, a := range accts {
			out[a.ID] = a.Status
		}
		return out
	}
	got := status(gated)
	if got["acc_g"] != store.StatusOK || got["acc_m"] != store.StatusProviderDisabled || got["acc_d"] != store.StatusDisconnected {
		t.Fatalf("gated statuses = %v", got)
	}
	a, err := gated.GetAccount(ctx, "pat", "acc_m")
	if err != nil || a.Status != store.StatusProviderDisabled {
		t.Fatalf("GetAccount = %+v, %v", a, err)
	}
	if a, err := gated.FindAccountByAddress(ctx, "pat", "m@x.com"); err != nil || a.Status != store.StatusProviderDisabled {
		t.Fatalf("FindAccountByAddress = %+v, %v", a, err)
	}

	// Nothing is stored: the real store and the credential are untouched, and
	// turning the provider on restores the account.
	raw, _ := st.GetAccount(ctx, "pat", "acc_m")
	if raw.Status != store.StatusOK || string(raw.RefreshToken) != "sealed-m" {
		t.Fatalf("stored account changed: %+v", raw)
	}
	microsoftOn = true
	if got := status(gated); got["acc_m"] != store.StatusOK {
		t.Fatalf("after enabling: %v", got)
	}
}
