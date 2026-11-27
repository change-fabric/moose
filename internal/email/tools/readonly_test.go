package tools_test

import (
	"strings"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/change-fabric/moose/internal/accounts/store"
	"github.com/change-fabric/moose/internal/email/fake"
	"github.com/change-fabric/moose/internal/email/provider"
	"github.com/change-fabric/moose/internal/email/tools"
)

// msAccount adds a send-enabled Microsoft account whose provider reports
// read-only capabilities, as the Graph client does until drafts and send exist.
func (e *sendEnv) msAccount(id, addr string) *fake.Provider {
	e.t.Helper()
	p := e.sender(id, addr)
	p.SetCapabilities(provider.Capabilities{Threads: true, SearchDialect: provider.DialectGraph, NativeDraftSend: true, ReadOnly: true})
	return p
}

// A read-only account refuses every write tool with the stable
// unsupported_for_provider code before anything is created or approved.
func TestReadOnlyProviderRefusesDraftsAndSend(t *testing.T) {
	e := newSendEnv(t, true)
	m := e.msAccount("acc_m", "m@contoso.example")
	e.sender("acc_g", "g@x.com") // a writable account beside it

	const from = "m@contoso.example"
	calls := map[string]*sdk.CallToolResult{
		"create": e.call(tools.ToolCreateDraft, map[string]any{"account_id": "acc_m", "from_address": from, "to": []string{"c@x.com"}, "subject": "s", "body": "b"}, nil),
		"update": e.call(tools.ToolUpdateDraft, map[string]any{"draft_id": "acc_m:d1", "from_address": from, "body": "b"}, nil),
		"send":   e.call(tools.ToolRequestSend, map[string]any{"account_id": "acc_m", "from_address": from, "draft_id": "acc_m:d1"}, nil),
		"enable": e.call(tools.ToolEnableSend, map[string]any{"account_id": "acc_m", "from_address": from, "enable": true}, nil),
	}
	for name, res := range calls {
		if !res.IsError || !strings.HasPrefix(errText(res), provider.CodeUnsupportedForProvider+":") {
			t.Errorf("%s: %q, want an %s error", name, errText(res), provider.CodeUnsupportedForProvider)
		}
	}
	if e.ap.Creates != 0 {
		t.Errorf("%d approval requests created for a read-only account", e.ap.Creates)
	}
	if n := m.DraftCount(); n != 0 {
		t.Errorf("%d drafts created on a read-only account", n)
	}

	// The writable account is unaffected.
	if res := e.call(tools.ToolCreateDraft, map[string]any{"account_id": "acc_g", "from_address": "g@x.com", "to": []string{"c@x.com"}, "subject": "s", "body": "b"}, nil); res.IsError {
		t.Errorf("writable account refused: %s", errText(res))
	}
	// Turning sending off is never refused.
	if res := e.call(tools.ToolEnableSend, map[string]any{"account_id": "acc_m", "from_address": from, "enable": false}, nil); res.IsError {
		t.Errorf("disable refused: %s", errText(res))
	}
}

// An account of a switched-off provider is hidden from every tool: it reports
// provider_disabled, is never called, and a write names the reason.
func TestProviderDisabledAccountsAreHiddenFromTools(t *testing.T) {
	e := newSendEnv(t, true)
	live := e.sender("acc_g", "g@x.com")
	live.AddMessage(msg("g1", "x@y.com", "hello", "hi", 1))
	off := e.account("acc_m", "m@contoso.example", store.StatusProviderDisabled)
	off.AddMessage(msg("m1", "x@y.com", "secret", "hi", 1))

	var out tools.SearchOut
	if res := e.call(tools.ToolSearch, nil, &out); res.IsError {
		t.Fatal(errText(res))
	}
	if len(out.Results) != 1 || out.Results[0].ID != "acc_g:g1" {
		t.Fatalf("results = %+v, want only the enabled provider's mail", out.Results)
	}
	got := map[string]string{}
	for _, r := range out.Accounts {
		got[r.AccountID] = r.Status + "/" + r.Reason
	}
	if got["acc_m"] != "provider_disabled/provider_not_enabled" || !strings.HasPrefix(got["acc_g"], "ok") {
		t.Errorf("accounts = %v", got)
	}

	if text := errText(e.call(tools.ToolGetMessage, map[string]any{"id": "acc_m:m1"}, nil)); !strings.HasPrefix(text, "provider_disabled:") {
		t.Errorf("get_message = %q", text)
	}
	draft := map[string]any{"account_id": "acc_m", "from_address": "m@contoso.example", "to": []string{"c@x.com"}, "subject": "s", "body": "b"}
	if text := errText(e.call(tools.ToolCreateDraft, draft, nil)); !strings.HasPrefix(text, "provider_disabled:") {
		t.Errorf("create_draft = %q", text)
	}

	var accts tools.ListAccountsOut
	if res := e.call(tools.ToolListAccounts, nil, &accts); res.IsError {
		t.Fatal(errText(res))
	}
	for _, a := range accts.Accounts {
		if a.AccountID == "acc_m" && a.Status != store.StatusProviderDisabled {
			t.Errorf("list_accounts reports %q for the disabled account", a.Status)
		}
	}
}
