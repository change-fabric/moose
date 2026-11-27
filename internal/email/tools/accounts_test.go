package tools_test

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/change-fabric/moose/internal/accounts/store"
	"github.com/change-fabric/moose/internal/accounts/store/sqlite"
	"github.com/change-fabric/moose/internal/email/provider"
	"github.com/change-fabric/moose/internal/email/tools"
)

var now = time.Date(2026, 11, 24, 15, 30, 0, 0, time.UTC)

func seed(t *testing.T) store.Store {
	t.Helper()
	ctx := context.Background()
	st, err := sqlite.OpenStore(ctx, filepath.Join(t.TempDir(), "e.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	sync := now.Add(-time.Hour)
	for i, a := range []store.Account{
		{Owner: "pat", ID: "acc_one", Provider: store.ProviderGoogle, ProviderUID: "work@example.com", Address: "work@example.com", DisplayName: "Pat Work",
			Nickname: "Work", Groups: []string{"jobs"}, ColorIndex: 0, Status: store.StatusOK, LastSyncAt: &sync, HourlyCap: 10, SendEnabled: true,
			Caps: provider.Capabilities{Threads: true, Labels: true, SearchDialect: "gmail"}, RefreshToken: []byte("sealed-secret")},
		{Owner: "pat", ID: "acc_two", Provider: store.ProviderGoogle, ProviderUID: "home@example.com", Address: "home@example.com",
			Nickname: "Home Base", ColorIndex: 1, Status: store.StatusNeedsReauth, LastError: "invalid_grant", HourlyCap: 10,
			Caps: provider.Capabilities{Threads: true}},
		{Owner: "other", ID: "acc_three", Provider: store.ProviderGoogle, ProviderUID: "x@example.com", Address: "x@example.com", Nickname: "Theirs", Status: store.StatusOK, HourlyCap: 10},
	} {
		a.Created, a.Updated = now.Add(time.Duration(i)*time.Second), now
		if err := st.CreateAccount(ctx, a); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.ReserveCap(ctx, "acc_one", now, 10); err != nil {
		t.Fatal(err)
	}
	return st
}

func call(t *testing.T, d tools.Deps, args map[string]any) (*sdk.CallToolResult, tools.ListAccountsOut) {
	t.Helper()
	srv := sdk.NewServer(&sdk.Implementation{Name: "t", Version: "1"}, nil)
	d.Now = func() time.Time { return now }
	tools.Register(srv, d)
	ct, stp := sdk.NewInMemoryTransports()
	ctx := context.Background()
	ss, err := srv.Connect(ctx, stp, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ss.Close()
	cs, err := sdk.NewClient(&sdk.Implementation{Name: "c", Version: "1"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	res, err := cs.CallTool(ctx, &sdk.CallToolParams{Name: tools.ToolListAccounts, Arguments: args})
	if err != nil {
		t.Fatal(err)
	}
	var out tools.ListAccountsOut
	if !res.IsError {
		b, _ := json.Marshal(res.StructuredContent)
		if err := json.Unmarshal(b, &out); err != nil {
			t.Fatal(err)
		}
	}
	return res, out
}

func TestListAccountsReturnsEveryR1AndR6Field(t *testing.T) {
	res, out := call(t, tools.Deps{Store: seed(t), FallbackOwner: "pat"}, nil)
	if res.IsError || len(out.Accounts) != 2 {
		t.Fatalf("res = %+v, accounts = %+v", res, out.Accounts)
	}
	a := out.Accounts[0]
	if a.AccountID != "acc_one" || a.Address != "work@example.com" || a.DisplayName != "Pat Work" ||
		len(a.Groups) != 1 || a.ColorName != "Blue" || a.Status != "ok" || !a.SendEnabled || a.HourlyCap == nil || *a.HourlyCap != 10 ||
		a.SentThisHour == nil || *a.SentThisHour != 1 || !a.Capabilities.Threads || a.LastSyncAt == nil || a.Provider != store.ProviderGoogle {
		t.Fatalf("account = %+v", a)
	}
	b := out.Accounts[1]
	if b.Status != "needs_reauth" || b.LastError != "invalid_grant" || b.SendEnabled || b.ColorName != "Crimson" || b.SentThisHour != nil || b.HourlyCap != nil || b.Groups != nil {
		t.Fatalf("second account = %+v", b)
	}
	raw, _ := json.Marshal(res.StructuredContent)
	for _, banned := range []string{"sealed-secret", "refresh_token", "c2VhbGVkLXNlY3JldA"} {
		if strings.Contains(string(raw), banned) {
			t.Fatalf("output leaks %q: %s", banned, raw)
		}
	}
}

func TestListAccountsOnlyShowsTheOwnersAccounts(t *testing.T) {
	_, out := call(t, tools.Deps{Store: seed(t), FallbackOwner: "other"}, nil)
	if len(out.Accounts) != 1 || out.Accounts[0].AccountID != "acc_three" {
		t.Fatalf("accounts = %+v", out.Accounts)
	}
}

func TestListAccountsAcceptsIDAndAddressNotNickname(t *testing.T) {
	d := tools.Deps{Store: seed(t), FallbackOwner: "pat"}
	for _, ref := range []string{"acc_two", "HOME@example.com", " home@example.com "} {
		_, out := call(t, d, map[string]any{"account": ref})
		if len(out.Accounts) != 1 || out.Accounts[0].AccountID != "acc_two" {
			t.Errorf("ref %q = %+v", ref, out.Accounts)
		}
	}
	_, out := call(t, d, map[string]any{"account": "all"})
	if len(out.Accounts) != 2 {
		t.Errorf("all = %+v", out.Accounts)
	}
	// Accounts are labelled by address: a nickname no longer names one.
	if res, _ := call(t, d, map[string]any{"account": "Home Base"}); !res.IsError {
		t.Error("a nickname must not resolve")
	}
	// A list is the union of its items.
	_, out = call(t, d, map[string]any{"account": []string{"acc_two", "work@example.com"}})
	if len(out.Accounts) != 2 {
		t.Errorf("list = %+v", out.Accounts)
	}
	_, out = call(t, d, map[string]any{"account": "group:jobs"})
	if len(out.Accounts) != 1 || out.Accounts[0].AccountID != "acc_one" {
		t.Errorf("group = %+v", out.Accounts)
	}
}

func TestListAccountsNeverGuesses(t *testing.T) {
	res, _ := call(t, tools.Deps{Store: seed(t), FallbackOwner: "pat"}, map[string]any{"account": "wrk"})
	if !res.IsError {
		t.Fatal("a near miss must not resolve")
	}
	text := res.Content[0].(*sdk.TextContent).Text
	for _, want := range []string{"account_not_found", "work@example.com (acc_one)", "home@example.com (acc_two)"} {
		if !strings.Contains(text, want) {
			t.Errorf("error %q lacks %q", text, want)
		}
	}
	if strings.Contains(text, "acc_three") {
		t.Errorf("candidates leak another owner's account: %s", text)
	}
}

func TestListAccountsNeedsAnOwner(t *testing.T) {
	res, _ := call(t, tools.Deps{Store: seed(t)}, nil)
	if !res.IsError || !strings.Contains(res.Content[0].(*sdk.TextContent).Text, "unauthorized") {
		t.Fatalf("res = %+v", res)
	}
}

func TestListAccountsIsAnnotatedReadOnly(t *testing.T) {
	srv := sdk.NewServer(&sdk.Implementation{Name: "t", Version: "1"}, nil)
	tools.Register(srv, tools.Deps{Store: seed(t), FallbackOwner: "pat"})
	ct, stp := sdk.NewInMemoryTransports()
	ctx := context.Background()
	ss, err := srv.Connect(ctx, stp, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ss.Close()
	cs, err := sdk.NewClient(&sdk.Implementation{Name: "c", Version: "1"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	list, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	readOnly := map[string]bool{"email_list_accounts": true, "email_list_mailboxes": true, "email_search": true, "email_get_message": true, "email_get_thread": true, "email_get_attachment": true, "email_account_update": false,
		"email_create_draft": false, "email_update_draft": false, "email_request_send": false, "email_send_status": false, "email_account_enable_send": false,
		"contacts_list": true, "contacts_propose": false, "contacts_status": false}
	if len(list.Tools) != len(readOnly) {
		t.Fatalf("registered %d tools, want %d: %+v", len(list.Tools), len(readOnly), list.Tools)
	}
	for _, tl := range list.Tools {
		want, ok := readOnly[tl.Name]
		if !ok {
			t.Errorf("unexpected tool %s", tl.Name)
			continue
		}
		if tl.Annotations == nil || tl.Annotations.ReadOnlyHint != want {
			t.Errorf("%s annotations = %+v, readOnly want %v", tl.Name, tl.Annotations, want)
		}
	}
}
