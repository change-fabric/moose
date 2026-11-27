package storetest

import (
	"context"
	"embed"
	"encoding/json"
	"strings"
	"testing"

	"github.com/change-fabric/moose/internal/accounts/store"
)

//go:embed testdata/legacy/*.json
var legacyFS embed.FS

// RunLegacy checks that records written in the format before canonical provider
// ids and per-service grants still read correctly. The fixtures under
// testdata/legacy are real old-format account rows: provider "gmail", no
// services, no version. newBackend must return a fresh, empty backend, and the
// rows are written to it raw, bypassing the Store, exactly as an old binary
// left them.
func RunLegacy(t *testing.T, newBackend func(t *testing.T) store.Backend) {
	ctx := context.Background()
	entries, err := legacyFS.ReadDir("testdata/legacy")
	if err != nil || len(entries) == 0 {
		t.Fatalf("legacy fixtures: %v (%d files)", err, len(entries))
	}
	for _, e := range entries {
		t.Run(strings.TrimSuffix(e.Name(), ".json"), func(t *testing.T) {
			raw, err := legacyFS.ReadFile("testdata/legacy/" + e.Name())
			if err != nil {
				t.Fatal(err)
			}
			var old store.Account
			if err := json.Unmarshal(raw, &old); err != nil {
				t.Fatalf("fixture does not parse: %v", err)
			}
			if old.Provider != "gmail" || len(old.Services) != 0 {
				t.Fatalf("fixture is not in the legacy format: provider %q services %v", old.Provider, old.Services)
			}
			b := newBackend(t)
			pk, sk := "OWNER#"+old.Owner, "ACCT#"+old.ID
			g1 := "ADDR#" + old.Owner + "#" + strings.ToLower(old.Address)
			if err := b.Transact(ctx, []store.Op{
				{Kind: store.OpPut, Cond: store.Cond{Kind: store.CondAbsent}, Item: store.Item{PK: pk, SK: sk, G1: g1, Data: raw}},
				{Kind: store.OpPut, Cond: store.Cond{Kind: store.CondAbsent}, Item: store.Item{PK: pk, SK: "NICK#" + strings.ToLower(old.Nickname), Data: []byte(`{"account_id":"` + old.ID + `"}`)}},
			}); err != nil {
				t.Fatal(err)
			}
			s := store.New(b)

			check := func(label string, got store.Account) {
				t.Helper()
				if got.Provider != store.ProviderGoogle {
					t.Errorf("%s: provider = %q, want %q", label, got.Provider, store.ProviderGoogle)
				}
				mail, ok := got.Services[store.ServiceMail]
				if !ok || len(got.Services) != 1 {
					t.Errorf("%s: services = %v, want exactly {mail}", label, got.Services)
				}
				if len(mail.Scopes) != len(old.Scopes) {
					t.Errorf("%s: mail scopes = %v, want the legacy scopes %v", label, mail.Scopes, old.Scopes)
				}
				if _, cal := got.Services[store.ServiceCalendar]; cal {
					t.Errorf("%s: a legacy account must have no calendar grant", label)
				}
				if got.Version != 0 || string(got.RefreshToken) != string(old.RefreshToken) || got.Status != old.Status {
					t.Errorf("%s: credential or status changed: version %d token %q status %q", label, got.Version, got.RefreshToken, got.Status)
				}
			}
			got, err := s.GetAccount(ctx, old.Owner, old.ID)
			if err != nil {
				t.Fatal(err)
			}
			check("get", got)
			byAddr, err := s.FindAccountByAddress(ctx, old.Owner, old.Address)
			if err != nil {
				t.Fatal(err)
			}
			check("find", byAddr)
			list, err := s.ListAccounts(ctx, old.Owner)
			if err != nil || len(list) != 1 {
				t.Fatalf("list = %v, %v", list, err)
			}
			check("list", list[0])

			// Reading leaves the stored row as it was; the next patch rewrites it
			// in the canonical form.
			row, ok, err := b.Get(ctx, pk, sk)
			if err != nil || !ok {
				t.Fatalf("row: %v %v", ok, err)
			}
			if !strings.Contains(string(row.Data), `"provider": "gmail"`) && !strings.Contains(string(row.Data), `"provider":"gmail"`) {
				t.Fatalf("a read must not rewrite the row: %s", row.Data)
			}
			if err := s.UpdateAccount(ctx, old.Owner, old.ID, store.AccountPatch{ColorIndex: store.Ptr(5)}); err != nil {
				t.Fatal(err)
			}
			row, _, _ = b.Get(ctx, pk, sk)
			var stored store.Account
			if err := json.Unmarshal(row.Data, &stored); err != nil {
				t.Fatal(err)
			}
			if stored.Provider != store.ProviderGoogle || len(stored.Services) != 1 || stored.ColorIndex != 5 {
				t.Fatalf("patched row was not rewritten canonically: provider %q services %v color %d", stored.Provider, stored.Services, stored.ColorIndex)
			}
			// The credential survives the rewrite and is still versioned from zero.
			after, err := s.GetAccount(ctx, old.Owner, old.ID)
			if err != nil {
				t.Fatal(err)
			}
			if string(after.RefreshToken) != string(old.RefreshToken) || after.Version != 0 {
				t.Fatalf("rewrite lost the credential: %q version %d", after.RefreshToken, after.Version)
			}
			// The first credential write on a legacy record expects version 0.
			if err := s.SetAccountCredential(ctx, old.Owner, old.ID, []byte("rotated"), 0); err != nil {
				t.Fatal(err)
			}
		})
	}
}
