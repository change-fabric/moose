package tools_test

import (
	"context"
	"strings"
	"testing"

	"github.com/change-fabric/moose/internal/email/sendflow"
	"github.com/change-fabric/moose/internal/email/store"
	"github.com/change-fabric/moose/internal/email/tools"
)

func TestContactsListFiltersByTrustTagAndAccount(t *testing.T) {
	e := newEnv(t)
	e.account("acc_a", "a@x.com", store.StatusOK)
	e.account("acc_b", "b@x.com", store.StatusOK)
	ctx := context.Background()
	for _, c := range []store.Contact{
		{AccountID: "acc_a", Address: "t1@x.com", Trust: store.TrustTrusted, Tags: []string{"vendor"}},
		{AccountID: "acc_a", Address: "k1@x.com", Trust: store.TrustKnown},
		{AccountID: "acc_a", Address: "n1@x.com", Trust: store.TrustNew, Tags: []string{"vendor"}},
		{AccountID: "acc_a", Address: "bad@x.com", Trust: store.TrustBlocked},
		{AccountID: "acc_b", Address: "t1@x.com", Trust: store.TrustBlocked},
	} {
		if err := e.st.PutContact(ctx, c); err != nil {
			t.Fatal(err)
		}
	}
	list := func(args map[string]any) tools.ContactsListOut {
		t.Helper()
		var out tools.ContactsListOut
		if res := e.call(tools.ToolContactsList, args, &out); res.IsError {
			t.Fatalf("%v: %s", args, errText(res))
		}
		return out
	}
	if out := list(nil); len(out.Contacts) != 5 || out.Truncated {
		t.Fatalf("all = %+v", out)
	}
	for trust, want := range map[string]int{"new": 1, "known": 1, "trusted": 1, "blocked": 2} {
		if out := list(map[string]any{"trust": trust}); len(out.Contacts) != want {
			t.Errorf("trust %s: %d contacts, want %d", trust, len(out.Contacts), want)
		}
	}
	if out := list(map[string]any{"tag": "vendor"}); len(out.Contacts) != 2 {
		t.Errorf("tag vendor = %+v", out)
	}
	if out := list(map[string]any{"account": "b@x.com"}); len(out.Contacts) != 1 || out.Contacts[0].Trust != store.TrustBlocked || out.Contacts[0].Account.Address != "b@x.com" {
		t.Errorf("account b = %+v", out)
	}
	if out := list(map[string]any{"limit": 2}); len(out.Contacts) != 2 || !out.Truncated {
		t.Errorf("limit 2 = %+v", out)
	}
	if res := e.call(tools.ToolContactsList, map[string]any{"trust": "bogus"}, nil); !res.IsError {
		t.Errorf("bogus trust: %q", errText(res))
	}
	if res := e.call(tools.ToolContactsList, map[string]any{"account": "zzz"}, nil); !res.IsError || !strings.HasPrefix(errText(res), "account_not_found:") {
		t.Errorf("unknown account: %q", errText(res))
	}
}

func TestContactsProposeBlockAndTagApplyAtOnce(t *testing.T) {
	e := newEnv(t) // no send engine: block and tag need none
	e.account("acc_a", "a@x.com", store.StatusOK)
	var out tools.ContactsProposeOut
	res := e.call(tools.ToolContactsPropose, map[string]any{"account_id": "acc_a", "changes": []map[string]any{
		{"address": "Eve@Evil.com", "action": "block"},
		{"address": "bob@x.com", "action": "tag", "tags": []string{"Vendor", "vendor", "net-30"}},
	}}, &out)
	if res.IsError {
		t.Fatal(errText(res))
	}
	if out.TrustRequest != nil || len(out.Results) != 2 || out.Results[0].Status != "applied" || out.Results[0].Trust != store.TrustBlocked ||
		strings.Join(out.Results[1].Tags, ",") != "net-30,vendor" {
		t.Fatalf("out = %+v", out)
	}
	ctx := context.Background()
	if c, err := e.st.GetContact(ctx, "acc_a", "eve@evil.com"); err != nil || c.Trust != store.TrustBlocked {
		t.Fatalf("eve = %+v, %v", c, err)
	}
	if c, err := e.st.GetContact(ctx, "acc_a", "bob@x.com"); err != nil || c.Trust != store.TrustNew || len(c.Tags) != 2 {
		t.Fatalf("bob = %+v, %v", c, err)
	}
	// Tags merge, and a tag never changes trust.
	e.call(tools.ToolContactsPropose, map[string]any{"account_id": "acc_a", "address": "eve@evil.com", "action": "tag", "tags": []string{"spam"}}, &out)
	if c, _ := e.st.GetContact(ctx, "acc_a", "eve@evil.com"); c.Trust != store.TrustBlocked || strings.Join(c.Tags, ",") != "spam" {
		t.Fatalf("eve after tag = %+v", c)
	}
	if _, err := e.st.GetContact(ctx, "acc_b", "eve@evil.com"); err == nil {
		t.Error("a block leaked across accounts")
	}
}

func TestContactsProposeValidatesBeforeChangingAnything(t *testing.T) {
	e := newSendEnv(t, true)
	e.sender("acc_a", "a@x.com")
	cases := map[string]map[string]any{
		"no account":      {"address": "c@x.com", "action": "block"},
		"no changes":      {"account_id": "acc_a"},
		"bad address":     {"account_id": "acc_a", "changes": []map[string]any{{"address": "c@x.com", "action": "block"}, {"address": "not an address", "action": "block"}}},
		"display name":    {"account_id": "acc_a", "changes": []map[string]any{{"address": "Eve <eve@x.com>", "action": "block"}}},
		"bad action":      {"account_id": "acc_a", "changes": []map[string]any{{"address": "c@x.com", "action": "delete"}}},
		"tag no tags":     {"account_id": "acc_a", "changes": []map[string]any{{"address": "c@x.com", "action": "tag"}}},
		"tags on block":   {"account_id": "acc_a", "changes": []map[string]any{{"address": "c@x.com", "action": "block", "tags": []string{"x"}}}},
		"bad tag":         {"account_id": "acc_a", "changes": []map[string]any{{"address": "c@x.com", "action": "tag", "tags": []string{"Bad Tag!"}}}},
		"duplicate":       {"account_id": "acc_a", "changes": []map[string]any{{"address": "c@x.com", "action": "block"}, {"address": "C@x.com", "action": "trust"}}},
		"own address":     {"account_id": "acc_a", "changes": []map[string]any{{"address": "a@x.com", "action": "trust"}}},
		"all accounts":    {"account_id": "all", "address": "c@x.com", "action": "block"},
		"unknown account": {"account_id": "zzz", "address": "c@x.com", "action": "block"},
	}
	for name, args := range cases {
		res := e.call(tools.ToolContactsPropose, args, nil)
		if !res.IsError {
			t.Errorf("%s: accepted", name)
		}
	}
	if cs, _ := e.st.ListContacts(context.Background(), "acc_a", store.ContactFilter{}); len(cs) != 0 {
		t.Fatalf("a rejected call changed contacts: %+v", cs)
	}
}

func TestContactsProposeTagLimit(t *testing.T) {
	e := newEnv(t)
	e.account("acc_a", "a@x.com", store.StatusOK)
	var out tools.ContactsProposeOut
	tags := []string{"t0", "t1", "t2", "t3", "t4", "t5", "t6", "t7", "t8", "t9"}
	e.call(tools.ToolContactsPropose, map[string]any{"account_id": "acc_a", "address": "c@x.com", "action": "tag", "tags": tags}, &out)
	if out.Results[0].Status != "applied" {
		t.Fatalf("ten tags: %+v", out)
	}
	e.call(tools.ToolContactsPropose, map[string]any{"account_id": "acc_a", "address": "c@x.com", "action": "tag", "tags": []string{"eleven"}}, &out)
	if out.Results[0].Status != "refused" {
		t.Fatalf("eleventh tag: %+v", out)
	}
}

func TestContactsTrustEndToEndThroughTheTools(t *testing.T) {
	e := newSendEnv(t, true)
	e.sender("acc_a", "a@x.com")
	e.sender("acc_b", "b@x.com")
	ctx := context.Background()
	_ = e.st.PutContact(ctx, store.Contact{AccountID: "acc_a", Address: "bad@x.com", Trust: store.TrustBlocked})
	var prop tools.ContactsProposeOut
	res := e.call(tools.ToolContactsPropose, map[string]any{"account_id": "acc_a", "changes": []map[string]any{
		{"address": "ann@x.com", "action": "trust"},
		{"address": "bob@x.com", "action": "trust"},
		{"address": "bad@x.com", "action": "trust"},
		{"address": "cy@x.com", "action": "block"},
	}}, &prop)
	if res.IsError {
		t.Fatal(errText(res))
	}
	if prop.TrustRequest == nil || prop.TrustRequest.Status != "pending" || prop.TrustRequest.ApprovalURL == "" || strings.Join(prop.TrustRequest.Addresses, ",") != "ann@x.com,bob@x.com" {
		t.Fatalf("trust request = %+v", prop.TrustRequest)
	}
	if e.ap.Creates != 1 {
		t.Fatalf("approver creates = %d, want exactly one", e.ap.Creates)
	}
	status := map[string]string{}
	for _, r := range prop.Results {
		status[r.Address] = r.Status
	}
	if status["ann@x.com"] != "pending_approval" || status["bad@x.com"] != "refused" || status["cy@x.com"] != "applied" {
		t.Fatalf("results = %+v", prop.Results)
	}
	if c, _ := e.st.GetContact(ctx, "acc_a", "ann@x.com"); c.Trust == store.TrustTrusted {
		t.Fatal("trusted before approval")
	}

	var st sendflow.ContactsStatusOut
	if res := e.call(tools.ToolContactsStatus, map[string]any{"request_id": prop.TrustRequest.RequestID}, &st); res.IsError || st.Status != "pending" {
		t.Fatalf("pending: %+v %s", st, errText(res))
	}
	e.ap.Approve(prop.TrustRequest.RequestID)
	if res := e.call(tools.ToolContactsStatus, map[string]any{"request_id": prop.TrustRequest.RequestID}, &st); res.IsError || st.Status != "applied" || len(st.Applied) != 2 {
		t.Fatalf("applied: %+v %s", st, errText(res))
	}
	var list tools.ContactsListOut
	e.call(tools.ToolContactsList, map[string]any{"account": "acc_a", "trust": "trusted"}, &list)
	if len(list.Contacts) != 2 {
		t.Fatalf("trusted list = %+v", list)
	}
	e.call(tools.ToolContactsList, map[string]any{"account": "acc_b", "trust": "trusted"}, &list)
	if len(list.Contacts) != 0 {
		t.Fatalf("account b trusts %+v", list)
	}
	if c, _ := e.st.GetContact(ctx, "acc_a", "bad@x.com"); c.Trust != store.TrustBlocked {
		t.Error("the block was lifted")
	}
}

func TestContactsToolsNeedTheEngineOnlyForTrust(t *testing.T) {
	e := newEnv(t)
	e.account("acc_a", "a@x.com", store.StatusOK)
	if res := e.call(tools.ToolContactsPropose, map[string]any{"account_id": "acc_a", "address": "c@x.com", "action": "trust"}, nil); !res.IsError || !strings.HasPrefix(errText(res), "internal:") {
		t.Errorf("trust without an engine: %q", errText(res))
	}
	if res := e.call(tools.ToolContactsStatus, map[string]any{"request_id": "r"}, nil); !res.IsError || !strings.HasPrefix(errText(res), "internal:") {
		t.Errorf("status without an engine: %q", errText(res))
	}
	if res := e.call(tools.ToolContactsStatus, map[string]any{"request_id": " "}, nil); !res.IsError {
		t.Error("empty request id accepted")
	}
}

func TestContactsStatusRefusesAnotherRequestKind(t *testing.T) {
	e := newSendEnv(t, true)
	e.sender("acc_a", "a@x.com")
	e.call(tools.ToolContactsStatus, map[string]any{"request_id": "nope"}, nil)
	if res := e.call(tools.ToolContactsStatus, map[string]any{"request_id": "nope"}, nil); !res.IsError || !strings.HasPrefix(errText(res), "not_found:") {
		t.Errorf("unknown id: %q", errText(res))
	}
}
