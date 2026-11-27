package tools_test

import (
	"context"
	"encoding/json"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/change-fabric/moose/internal/accounts/scopes"
	"github.com/change-fabric/moose/internal/accounts/store"
	"github.com/change-fabric/moose/internal/accounts/store/sqlite"
	"github.com/change-fabric/moose/internal/calendar/tools"
)

var now = time.Date(2026, 11, 24, 15, 30, 0, 0, time.UTC)

const (
	workTenant = "11111111-aaaa-bbbb-cccc-222222222222"
	calEvents  = "https://www.googleapis.com/auth/calendar.events"
	calRead    = "https://www.googleapis.com/auth/calendar.readonly"
)

func seed(t *testing.T) store.Store {
	t.Helper()
	ctx := context.Background()
	st, err := sqlite.OpenStore(ctx, filepath.Join(t.TempDir(), "c.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	mail := map[string]store.ServiceGrant{store.ServiceMail: {Scopes: []string{"gmail.readonly"}, GrantedAt: now}}
	with := func(extra map[string]store.ServiceGrant) map[string]store.ServiceGrant {
		out := map[string]store.ServiceGrant{}
		for k, v := range mail {
			out[k] = v
		}
		for k, v := range extra {
			out[k] = v
		}
		return out
	}
	for i, a := range []store.Account{
		{Owner: "pat", ID: "acc_full", Provider: store.ProviderGoogle, ProviderUID: "full@example.com", Address: "full@example.com", DisplayName: "Pat Full",
			Nickname: "Full", Groups: []string{"jobs"}, Status: store.StatusOK, HourlyCap: 10, InviteEnabled: true, RefreshToken: []byte("sealed-secret"),
			Services: with(map[string]store.ServiceGrant{store.ServiceCalendar: {Scopes: []string{calEvents, calRead}, GrantedAt: now}})},
		{Owner: "pat", ID: "acc_part", Provider: store.ProviderGoogle, ProviderUID: "part@example.com", Address: "part@example.com",
			Nickname: "Part", ColorIndex: 1, Status: store.StatusOK, HourlyCap: 10,
			Services: with(map[string]store.ServiceGrant{store.ServiceCalendar: {Scopes: []string{calRead}, GrantedAt: now}})},
		{Owner: "pat", ID: "acc_mail", Provider: store.ProviderGoogle, ProviderUID: "mail@example.com", Address: "mail@example.com",
			Nickname: "Mail", ColorIndex: 2, Status: store.StatusNeedsReauth, LastError: "invalid_grant", HourlyCap: 10, Services: mail},
		{Owner: "pat", ID: "acc_msp", Provider: store.ProviderMicrosoft, ProviderUID: scopes.MicrosoftPersonalTenant + ".oid1", Address: "me@outlook.example",
			Nickname: "Outlook", ColorIndex: 3, Status: store.StatusOK, HourlyCap: 10,
			Services: with(map[string]store.ServiceGrant{store.ServiceCalendar: {Scopes: []string{"https://graph.microsoft.com/Calendars.ReadWrite"}, GrantedAt: now}})},
		{Owner: "pat", ID: "acc_msw", Provider: store.ProviderMicrosoft, ProviderUID: workTenant + ".oid2", Address: "me@contoso.example",
			Nickname: "Contoso", ColorIndex: 4, Status: store.StatusOK, HourlyCap: 10,
			Services: with(map[string]store.ServiceGrant{store.ServiceCalendar: {Scopes: []string{"Calendars.ReadWrite", "Calendars.Read.Shared"}, GrantedAt: now}})},
		{Owner: "other", ID: "acc_theirs", Provider: store.ProviderGoogle, ProviderUID: "x@example.com", Address: "x@example.com", Nickname: "Theirs", Status: store.StatusOK, HourlyCap: 10},
	} {
		a.Created, a.Updated = now.Add(time.Duration(i)*time.Second), now
		if err := st.CreateAccount(ctx, a); err != nil {
			t.Fatal(err)
		}
	}
	return st
}

func connect(t *testing.T, d tools.Deps) *sdk.ClientSession {
	t.Helper()
	srv := sdk.NewServer(&sdk.Implementation{Name: "t", Version: "1"}, tools.ServerOptions())
	d.Now = func() time.Time { return now }
	tools.Register(srv, d)
	ct, stp := sdk.NewInMemoryTransports()
	ctx := context.Background()
	ss, err := srv.Connect(ctx, stp, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ss.Close() })
	cs, err := sdk.NewClient(&sdk.Implementation{Name: "c", Version: "1"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close() })
	return cs
}

func call(t *testing.T, d tools.Deps, args map[string]any) (*sdk.CallToolResult, tools.ListAccountsOut) {
	t.Helper()
	res, err := connect(t, d).CallTool(context.Background(), &sdk.CallToolParams{Name: tools.ToolListAccounts, Arguments: args})
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

func byID(out tools.ListAccountsOut) map[string]tools.AccountOut {
	m := map[string]tools.AccountOut{}
	for _, a := range out.Accounts {
		m[a.AccountID] = a
	}
	return m
}

func TestListAccountsReportsEachAccountsCalendarGrant(t *testing.T) {
	res, out := call(t, tools.Deps{Store: seed(t), FallbackOwner: "pat"}, nil)
	if res.IsError || len(out.Accounts) != 5 {
		t.Fatalf("res = %+v, accounts = %+v", res, out.Accounts)
	}
	got := byID(out)

	full := got["acc_full"]
	if full.Calendar.Status != tools.CalendarGranted || len(full.Calendar.Scopes) != 2 || len(full.Calendar.MissingScopes) != 0 ||
		!full.Calendar.InviteEnabled || full.Calendar.GrantedAt == nil || full.Status != "ok" || full.DisplayName != "Pat Full" ||
		full.ColorName != "Blue" || len(full.Groups) != 1 || full.Provider != store.ProviderGoogle {
		t.Errorf("full = %+v", full)
	}
	part := got["acc_part"]
	if part.Calendar.Status != tools.CalendarPartial || len(part.Calendar.MissingScopes) != 1 || part.Calendar.MissingScopes[0] != "calendar.events" || part.Calendar.InviteEnabled {
		t.Errorf("partial = %+v, want missing calendar.events", part.Calendar)
	}
	mail := got["acc_mail"]
	if mail.Calendar.Status != tools.CalendarNotGranted || len(mail.Calendar.Scopes) != 0 || len(mail.Calendar.MissingScopes) != 2 || mail.Status != "needs_reauth" || mail.LastError != "invalid_grant" {
		t.Errorf("mail only = %+v", mail)
	}
	msp := got["acc_msp"]
	if msp.Calendar.Status != tools.CalendarGranted || msp.Calendar.Degraded != tools.DegradedOwnCalendarOnly || len(msp.Calendar.MissingScopes) != 0 {
		t.Errorf("personal microsoft = %+v, want granted with ReadWrite alone and degraded own_calendar_only", msp.Calendar)
	}
	msw := got["acc_msw"]
	if msw.Calendar.Status != tools.CalendarGranted || msw.Calendar.Degraded != "" {
		t.Errorf("work microsoft = %+v", msw.Calendar)
	}
}

func TestListAccountsNeverLeaksCredentials(t *testing.T) {
	res, _ := call(t, tools.Deps{Store: seed(t), FallbackOwner: "pat"}, nil)
	raw, _ := json.Marshal(res.StructuredContent)
	for _, banned := range []string{"sealed-secret", "refresh_token", "c2VhbGVkLXNlY3JldA", "provider_uid", "ProviderUID"} {
		if strings.Contains(string(raw), banned) {
			t.Fatalf("output leaks %q: %s", banned, raw)
		}
	}
}

func TestListAccountsOnlyShowsTheOwnersAccounts(t *testing.T) {
	_, out := call(t, tools.Deps{Store: seed(t), FallbackOwner: "other"}, nil)
	if len(out.Accounts) != 1 || out.Accounts[0].AccountID != "acc_theirs" {
		t.Fatalf("accounts = %+v", out.Accounts)
	}
}

func TestListAccountsResolvesIDsAddressesAndGroupsAndNeverGuesses(t *testing.T) {
	d := tools.Deps{Store: seed(t), FallbackOwner: "pat"}
	for _, ref := range []string{"acc_part", "PART@example.com", " part@example.com "} {
		_, out := call(t, d, map[string]any{"account": ref})
		if len(out.Accounts) != 1 || out.Accounts[0].AccountID != "acc_part" {
			t.Errorf("ref %q = %+v", ref, out.Accounts)
		}
	}
	_, out := call(t, d, map[string]any{"account": "group:jobs"})
	if len(out.Accounts) != 1 || out.Accounts[0].AccountID != "acc_full" {
		t.Errorf("group = %+v", out.Accounts)
	}
	res, _ := call(t, d, map[string]any{"account": "nobody@example.com"})
	if !res.IsError {
		t.Fatalf("an unknown address resolved: %+v", res)
	}
	var text string
	for _, c := range res.Content {
		if tc, ok := c.(*sdk.TextContent); ok {
			text += tc.Text
		}
	}
	if !strings.Contains(text, "account_not_found") || !strings.Contains(text, "full@example.com") {
		t.Errorf("error = %q, want account_not_found with candidates", text)
	}
}

func TestListAccountsWithoutAnOwnerIsUnauthorized(t *testing.T) {
	res, _ := call(t, tools.Deps{Store: seed(t)}, nil)
	if !res.IsError {
		t.Fatal("a request with no owner listed accounts")
	}
}

func TestServerCarriesInstructionsAndOnlyTheWriteToolsAreNotReadOnly(t *testing.T) {
	cs := connect(t, tools.Deps{Store: seed(t), FallbackOwner: "pat"})
	if init := cs.InitializeResult(); init == nil || !strings.Contains(init.Instructions, "address") || !strings.Contains(init.Instructions, "not_granted") {
		t.Errorf("instructions = %+v", init)
	}
	list, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"calendar_create_event", "calendar_delete_event", "calendar_find_meeting_times", "calendar_free_busy", "calendar_get_event",
		"calendar_list_accounts", "calendar_list_calendars", "calendar_list_events", "calendar_respond_to_invite", "calendar_update_event",
	}
	writes := map[string]bool{
		"calendar_create_event": true, "calendar_update_event": true, "calendar_delete_event": true, "calendar_respond_to_invite": true,
	}
	var got []string
	for _, tl := range list.Tools {
		got = append(got, tl.Name)
		a := tl.Annotations
		if a == nil || a.ReadOnlyHint == writes[tl.Name] {
			t.Errorf("%s annotations = %+v, want read-only exactly for the read tools", tl.Name, a)
		}
		if tl.Name == "calendar_delete_event" && (a == nil || a.DestructiveHint == nil || !*a.DestructiveHint) {
			t.Errorf("delete is not marked destructive: %+v", a)
		}
	}
	sort.Strings(got)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("tools = %v, want exactly %v", got, want)
	}
}
