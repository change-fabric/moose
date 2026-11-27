package tools_test

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/change-fabric/moose/internal/approver/api"
	"github.com/change-fabric/moose/internal/email/envelope"
	"github.com/change-fabric/moose/internal/email/fake"
	"github.com/change-fabric/moose/internal/email/mailmsg"
	"github.com/change-fabric/moose/internal/email/provider"
	"github.com/change-fabric/moose/internal/email/sendflow"
	"github.com/change-fabric/moose/internal/email/sendflow/flowtest"
	"github.com/change-fabric/moose/internal/email/store"
	"github.com/change-fabric/moose/internal/email/tools"
)

// sendEnv is an env with the send engine wired to the fake approval service.
type sendEnv struct {
	*env
	ap *flowtest.Approver
}

func newSendEnv(t *testing.T, live bool) *sendEnv {
	t.Helper()
	e := newEnv(t)
	ap := flowtest.New()
	ap.Now = func() time.Time { return now }
	seal, err := envelope.NewLocal(bytes.Repeat([]byte("k"), 32))
	if err != nil {
		t.Fatal(err)
	}
	e.d.Send = &sendflow.Service{Store: e.st, Seal: seal, Approver: ap, Providers: e.src,
		ReceiptKey: ap.PublicKeyPEM(), RPID: flowtest.RPID, Live: live, Now: func() time.Time { return now }}
	return &sendEnv{env: e, ap: ap}
}

// sender adds an ok account with sending switched on.
func (e *sendEnv) sender(id, addr string) *fake.Provider {
	e.t.Helper()
	p := e.account(id, addr, store.StatusOK)
	a, err := e.st.GetAccount(context.Background(), "pat", id)
	if err != nil {
		e.t.Fatal(err)
	}
	a.SendEnabled = true
	if err := e.st.UpdateAccount(context.Background(), a); err != nil {
		e.t.Fatal(err)
	}
	return p
}

func (e *sendEnv) draft(account, from string, extra map[string]any) tools.DraftOut {
	e.t.Helper()
	args := map[string]any{"account_id": account, "from_address": from, "to": []string{"ann@example.com"}, "subject": "Hello", "body": "Body text"}
	for k, v := range extra {
		args[k] = v
	}
	var out tools.DraftOut
	if res := e.call(tools.ToolCreateDraft, args, &out); res.IsError {
		e.t.Fatalf("create draft: %s", errText(res))
	}
	return out
}

// ---- email_create_draft and email_update_draft ----

func TestCreateDraftRequiresTheExactFromAddress(t *testing.T) {
	e := newSendEnv(t, true)
	p := e.sender("acc_a", "a@x.com")
	e.account("acc_b", "b@x.com", store.StatusOK)
	for name, from := range map[string]string{"another address": "b@x.com", "an account id": "acc_a", "a nickname": "n1", "empty": "", "suffixed": "a@x.com.evil.net"} {
		res := e.call(tools.ToolCreateDraft, map[string]any{"account_id": "acc_a", "from_address": from, "to": []string{"c@x.com"}, "subject": "s", "body": "b"}, nil)
		if !res.IsError || !strings.HasPrefix(errText(res), "from_mismatch:") {
			t.Errorf("%s: %q, want a from_mismatch error", name, errText(res))
		}
	}
	if p.DraftCount() != 0 {
		t.Fatal("a from_address mismatch must create no draft")
	}
	// The address may name the account too, case-insensitively.
	out := e.draft("A@X.com", "a@x.com", nil)
	if out.Status != "created" || out.Account.ID != "acc_a" || out.Account.Address != "a@x.com" || !strings.HasPrefix(out.DraftID, "acc_a:") {
		t.Fatalf("out = %+v", out)
	}
}

func TestCreateDraftRefusesUnknownAndUnavailableAccounts(t *testing.T) {
	e := newSendEnv(t, true)
	e.sender("acc_a", "a@x.com")
	e.account("acc_stale", "stale@x.com", store.StatusNeedsReauth)
	res := e.call(tools.ToolCreateDraft, map[string]any{"account_id": "acc_zzz", "from_address": "zzz@x.com", "to": []string{"c@x.com"}}, nil)
	if !res.IsError || !strings.HasPrefix(errText(res), "account_not_found:") || !strings.Contains(errText(res), "a@x.com") {
		t.Errorf("unknown account: %q", errText(res))
	}
	res = e.call(tools.ToolCreateDraft, map[string]any{"account_id": "stale@x.com", "from_address": "stale@x.com", "to": []string{"c@x.com"}}, nil)
	if !res.IsError || !strings.HasPrefix(errText(res), "account_unavailable:") {
		t.Errorf("needs_reauth account: %q", errText(res))
	}
	for _, args := range []map[string]any{
		{"account_id": "all", "from_address": "a@x.com", "to": []string{"c@x.com"}},
		{"account_id": "acc_a", "from_address": "a@x.com", "to": []string{"not an address"}},
		{"account_id": "acc_a", "from_address": "a@x.com"},
	} {
		if res := e.call(tools.ToolCreateDraft, args, nil); !res.IsError || !strings.HasPrefix(errText(res), "invalid_input:") {
			t.Errorf("%v: %q", args, errText(res))
		}
	}
}

func TestReplyWithoutAnAccountReturnsNeedsAccountAndCreatesNothing(t *testing.T) {
	e := newSendEnv(t, true)
	recv := e.sender("acc_recv", "recv@x.com")
	e.sender("acc_other", "other@x.com")
	e.account("acc_ro", "readonly@x.com", store.StatusOK) // not send-enabled: not a candidate
	other := e.src.m["acc_other"].(*fake.Provider)
	recv.AddMessage(msg("m1", "boss@corp.com", "Quarterly numbers", "please reply", time.Hour))

	var out tools.DraftOut
	res := e.call(tools.ToolCreateDraft, map[string]any{"in_reply_to": "acc_recv:m1", "from_address": "recv@x.com"}, &out)
	if res.IsError {
		t.Fatalf("needs_account is an answer, not a failure: %s", errText(res))
	}
	if out.Status != "needs_account" || out.DraftID != "" || out.Suggested == nil || out.Suggested.ID != "acc_recv" {
		t.Fatalf("out = %+v", out)
	}
	cand := map[string]bool{}
	for _, c := range out.Candidates {
		cand[c.ID] = true
	}
	if !cand["acc_recv"] || !cand["acc_other"] || cand["acc_ro"] || len(cand) != 2 {
		t.Fatalf("candidates = %+v, want the send-enabled accounts", out.Candidates)
	}
	if recv.DraftCount() != 0 || other.DraftCount() != 0 {
		t.Fatal("needs_account must create no draft anywhere")
	}
	// Without a reply, no account also means needs_account, never a guess.
	var plain tools.DraftOut
	e.call(tools.ToolCreateDraft, map[string]any{"from_address": "recv@x.com", "to": []string{"a@b.com"}}, &plain)
	if plain.Status != "needs_account" || plain.Suggested != nil || recv.DraftCount() != 0 {
		t.Fatalf("plain = %+v", plain)
	}
}

func TestReplyFromTheReceivingAccountThreadsAndDefaults(t *testing.T) {
	e := newSendEnv(t, true)
	recv := e.sender("acc_recv", "recv@x.com")
	m := msg("m1", "boss@corp.com", "Quarterly numbers", "please reply", time.Hour)
	m.RFCMessageID, m.References = "<orig@corp.com>", "<root@corp.com>"
	recv.AddMessage(m)
	var out tools.DraftOut
	res := e.call(tools.ToolCreateDraft, map[string]any{"in_reply_to": "acc_recv:m1", "account_id": "acc_recv", "from_address": "recv@x.com", "body": "On it."}, &out)
	if res.IsError || out.Status != "created" || len(out.Warnings) != 0 {
		t.Fatalf("res %s out %+v", errText(res), out)
	}
	if out.Subject != "Re: Quarterly numbers" || len(out.To) != 1 || out.To[0] != "boss@corp.com" {
		t.Fatalf("defaults: %+v", out)
	}
	draftID := strings.TrimPrefix(out.DraftID, "acc_recv:")
	d, raw, ok := recv.Draft(draftID)
	if !ok || d.ThreadID != "t-m1" {
		t.Fatalf("draft %+v ok=%v, want it in the original thread", d, ok)
	}
	if !bytes.Contains(raw, []byte("In-Reply-To: <orig@corp.com>")) || !bytes.Contains(raw, []byte("References: <root@corp.com> <orig@corp.com>")) {
		t.Fatalf("reply headers missing:\n%s", raw)
	}
}

func TestReplyFromADifferentAccountWarnsAndTheApprovalFlagsIt(t *testing.T) {
	e := newSendEnv(t, true)
	recv := e.sender("acc_recv", "recv@x.com")
	other := e.sender("acc_other", "other@x.com")
	m := msg("m1", "boss@corp.com", "Plans", "reply please", time.Hour)
	m.RFCMessageID = "<orig@corp.com>"
	recv.AddMessage(m)

	var out tools.DraftOut
	if res := e.call(tools.ToolCreateDraft, map[string]any{"in_reply_to": "acc_recv:m1", "account_id": "acc_other", "from_address": "other@x.com", "body": "hi"}, &out); res.IsError {
		t.Fatal(errText(res))
	}
	if out.Status != "created" || len(out.Warnings) != 1 || out.Warnings[0].Code != "reply_from_differs" {
		t.Fatalf("out = %+v", out)
	}
	// The thread belongs to another mailbox, so the draft is not placed in it.
	if d, _, _ := other.Draft(strings.TrimPrefix(out.DraftID, "acc_other:")); d.ThreadID != "" {
		t.Errorf("thread %q leaked across accounts", d.ThreadID)
	}
	var req tools.RequestSendOut
	if res := e.call(tools.ToolRequestSend, map[string]any{"account_id": "acc_other", "from_address": "other@x.com", "draft_id": out.DraftID}, &req); res.IsError {
		t.Fatal(errText(res))
	}
	var found bool
	for _, f := range req.Items[0].SoftFlags {
		found = found || f.Flag == "reply_account_differs"
	}
	if !found {
		t.Fatalf("soft flags = %+v, want reply_account_differs", req.Items[0].SoftFlags)
	}
	// A reply from the receiving account carries no such flag.
	var same tools.DraftOut
	e.call(tools.ToolCreateDraft, map[string]any{"in_reply_to": "acc_recv:m1", "account_id": "acc_recv", "from_address": "recv@x.com", "body": "hi"}, &same)
	var req2 tools.RequestSendOut
	e.call(tools.ToolRequestSend, map[string]any{"account_id": "acc_recv", "from_address": "recv@x.com", "draft_id": same.DraftID}, &req2)
	for _, f := range req2.Items[0].SoftFlags {
		if f.Flag == "reply_account_differs" {
			t.Errorf("unexpected flag on a same-account reply: %+v", req2.Items[0].SoftFlags)
		}
	}
}

func TestUpdateDraftChangesOnlyTheNamedFields(t *testing.T) {
	e := newSendEnv(t, true)
	p := e.sender("acc_a", "a@x.com")
	created := e.draft("acc_a", "a@x.com", map[string]any{"cc": []string{"cy@x.com"}})
	var out tools.DraftOut
	res := e.call(tools.ToolUpdateDraft, map[string]any{"draft_id": created.DraftID, "from_address": "a@x.com", "subject": "New subject", "bcc": []string{"dee@x.com"}}, &out)
	if res.IsError || out.Status != "updated" {
		t.Fatalf("%s %+v", errText(res), out)
	}
	if out.Subject != "New subject" || strings.Join(out.To, ",") != "ann@example.com" || strings.Join(out.Cc, ",") != "cy@x.com" || strings.Join(out.Bcc, ",") != "dee@x.com" {
		t.Fatalf("out = %+v", out)
	}
	_, raw, _ := p.Draft(strings.TrimPrefix(created.DraftID, "acc_a:"))
	parsed, err := mailmsg.Parse(raw)
	if err != nil || parsed.Body != "Body text" || parsed.Subject != "New subject" {
		t.Fatalf("stored draft = %+v, %v", parsed, err)
	}
	// Clearing a list with [] and a mismatched from_address.
	var cleared tools.DraftOut
	if res := e.call(tools.ToolUpdateDraft, map[string]any{"draft_id": created.DraftID, "from_address": "a@x.com", "cc": []string{}}, &cleared); res.IsError || len(cleared.Cc) != 0 || cleared.Subject != "New subject" {
		t.Fatalf("clear cc: %s %+v", errText(res), cleared)
	}
	if res := e.call(tools.ToolUpdateDraft, map[string]any{"draft_id": created.DraftID, "from_address": "n1"}, nil); !res.IsError || !strings.HasPrefix(errText(res), "from_mismatch:") {
		t.Errorf("from mismatch on update: %q", errText(res))
	}
	e.account("acc_b", "b@x.com", store.StatusOK)
	if res := e.call(tools.ToolUpdateDraft, map[string]any{"draft_id": created.DraftID, "account_id": "b@x.com", "from_address": "b@x.com"}, nil); !res.IsError || !strings.HasPrefix(errText(res), "account_mismatch:") {
		t.Errorf("another account's draft: %q", errText(res))
	}
	if res := e.call(tools.ToolUpdateDraft, map[string]any{"draft_id": "nonsense", "from_address": "a@x.com"}, nil); !res.IsError {
		t.Error("a draft id without an account must be refused")
	}
}

// ---- email_request_send and email_send_status ----

func TestRequestSendRefusals(t *testing.T) {
	e := newSendEnv(t, true)
	e.sender("acc_a", "a@x.com")
	ro := e.account("acc_ro", "ro@x.com", store.StatusOK) // send not enabled
	d := e.draft("acc_a", "a@x.com", nil)

	for name, args := range map[string]map[string]any{
		"nickname as from":  {"account_id": "acc_a", "from_address": "n1", "draft_id": d.DraftID},
		"another address":   {"account_id": "acc_a", "from_address": "ro@x.com", "draft_id": d.DraftID},
		"no draft":          {"account_id": "acc_a", "from_address": "a@x.com"},
		"both draft fields": {"account_id": "acc_a", "from_address": "a@x.com", "draft_id": d.DraftID, "drafts": []string{d.DraftID}},
		"bare draft id":     {"account_id": "acc_a", "from_address": "a@x.com", "draft_id": "d1"},
	} {
		res := e.call(tools.ToolRequestSend, args, nil)
		if !res.IsError {
			t.Errorf("%s: accepted", name)
		}
	}
	if res := e.call(tools.ToolRequestSend, map[string]any{"account_id": "acc_a", "from_address": "n1", "draft_id": d.DraftID}, nil); !strings.HasPrefix(errText(res), "from_mismatch:") {
		t.Errorf("from mismatch must be a hard error: %q", errText(res))
	}
	if e.ap.Creates != 0 {
		t.Fatal("a refusal created an approval request")
	}

	// Send refused while send_enabled is false, though a draft can be made.
	raw, _ := mailmsg.Build(mailmsg.Message{From: "ro@x.com", To: []string{"c@x.com"}, Subject: "s", Body: "b"}, now)
	rd, _ := ro.CreateDraft(context.Background(), provider.DraftInput{Raw: raw})
	res := e.call(tools.ToolRequestSend, map[string]any{"account_id": "acc_ro", "from_address": "ro@x.com", "draft_id": "acc_ro:" + rd.ID}, nil)
	if !res.IsError || !strings.HasPrefix(errText(res), "send_disabled:") || !strings.Contains(errText(res), "email_account_enable_send") {
		t.Errorf("send while disabled: %q", errText(res))
	}
}

func TestRequestSendThenStatusEndToEnd(t *testing.T) {
	e := newSendEnv(t, false) // email.send.live is false: the run ends at would_send
	p := e.sender("acc_a", "a@x.com")
	d := e.draft("acc_a", "a@x.com", nil)
	var req tools.RequestSendOut
	if res := e.call(tools.ToolRequestSend, map[string]any{"account_id": "acc_a", "from_address": "a@x.com", "draft_id": d.DraftID}, &req); res.IsError {
		t.Fatal(errText(res))
	}
	if req.Status != "pending" || req.ApprovalURL == "" || req.RequestID == "" || req.Batch || req.Items[0].DraftID != d.DraftID {
		t.Fatalf("request = %+v", req)
	}
	var st sendflow.StatusOut
	e.call(tools.ToolSendStatus, map[string]any{"request_id": req.RequestID}, &st)
	if st.Status != "pending" || st.ApprovalURL == "" {
		t.Fatalf("before approval: %+v", st)
	}
	e.ap.Approve(req.RequestID)
	e.call(tools.ToolSendStatus, map[string]any{"request_id": req.RequestID}, &st)
	if st.Status != store.SendWouldSend || !st.DryRun || len(p.SentMessages()) != 0 {
		t.Fatalf("after approval with live off: %+v, sent %d", st, len(p.SentMessages()))
	}
	if res := e.call(tools.ToolSendStatus, map[string]any{"request_id": "nope"}, nil); !res.IsError || !strings.HasPrefix(errText(res), "not_found:") {
		t.Errorf("unknown request: %q", errText(res))
	}
}

func TestSendThroughTheToolsDiscardsTheSourceDraft(t *testing.T) {
	e := newSendEnv(t, true)
	p := e.sender("acc_a", "a@x.com")
	d := e.draft("acc_a", "a@x.com", nil)
	var req tools.RequestSendOut
	e.call(tools.ToolRequestSend, map[string]any{"account_id": "acc_a", "from_address": "a@x.com", "draft_id": d.DraftID}, &req)
	e.ap.Approve(req.RequestID)
	var st sendflow.StatusOut
	e.call(tools.ToolSendStatus, map[string]any{"request_id": req.RequestID}, &st)
	if st.Status != store.SendSent || st.SourceDraftRetained || st.SourceDraftID != "" || len(p.SentMessages()) != 1 {
		t.Fatalf("%+v", st)
	}
}

func TestCrossAccountBatchIsRejected(t *testing.T) {
	e := newSendEnv(t, true)
	e.sender("acc_a", "a@x.com")
	e.sender("acc_b", "b@x.com")
	da := e.draft("acc_a", "a@x.com", nil)
	db := e.draft("acc_b", "b@x.com", nil)
	res := e.call(tools.ToolRequestSend, map[string]any{"account_id": "acc_a", "from_address": "a@x.com", "drafts": []string{da.DraftID, db.DraftID}}, nil)
	if !res.IsError || !strings.HasPrefix(errText(res), "cross_account_batch:") {
		t.Fatalf("cross-account batch: %q", errText(res))
	}
	if e.ap.Creates != 0 {
		t.Fatal("a cross-account batch created approval requests")
	}
}

func TestBatchRequestThroughTheTool(t *testing.T) {
	e := newSendEnv(t, true)
	p := e.sender("acc_a", "a@x.com")
	d1 := e.draft("acc_a", "a@x.com", map[string]any{"subject": "one"})
	d2 := e.draft("acc_a", "a@x.com", map[string]any{"subject": "two", "to": []string{"bob@example.com"}})
	var req tools.RequestSendOut
	if res := e.call(tools.ToolRequestSend, map[string]any{"account_id": "acc_a", "from_address": "a@x.com", "drafts": []string{d1.DraftID, d2.DraftID}}, &req); res.IsError {
		t.Fatal(errText(res))
	}
	if !req.Batch || len(req.Items) != 2 || req.Items[0].DraftID != d1.DraftID {
		t.Fatalf("batch = %+v", req)
	}
	kind, _, _ := e.ap.Request(req.RequestID)
	if kind != api.KindEmailSendBatch {
		t.Fatalf("kind = %s", kind)
	}
	e.ap.ApproveBatch(req.RequestID, req.Items[1].RequestID) // drop the second
	var st sendflow.StatusOut
	e.call(tools.ToolSendStatus, map[string]any{"request_id": req.RequestID}, &st)
	if st.Status != "done" || len(st.Items) != 2 || st.Items[0].Status != store.SendSent || st.Items[1].Status != sendflow.StatusDenied || len(p.SentMessages()) != 1 {
		t.Fatalf("%+v sent %d", st, len(p.SentMessages()))
	}
	if st.Items[0].SourceDraftRetained {
		t.Errorf("the sent item's unchanged draft must be discarded: %+v", st.Items[0])
	}
}

// ---- R10: contacts are per account ----

func TestAContactTrustedInOneAccountIsStillNewInAnother(t *testing.T) {
	e := newSendEnv(t, true)
	e.sender("acc_a", "a@x.com")
	e.sender("acc_b", "b@x.com")
	send := func(acct, addr string) tools.RequestSendOut {
		var d tools.DraftOut
		e.call(tools.ToolCreateDraft, map[string]any{"account_id": acct, "from_address": addr, "to": []string{"stranger@elsewhere.org"}, "subject": "s-" + acct, "body": "b"}, &d)
		var req tools.RequestSendOut
		if res := e.call(tools.ToolRequestSend, map[string]any{"account_id": acct, "from_address": addr, "draft_id": d.DraftID}, &req); res.IsError {
			t.Fatal(errText(res))
		}
		return req
	}
	hasNew := func(req tools.RequestSendOut) bool {
		for _, f := range req.Items[0].SoftFlags {
			if f.Flag == "new_recipient" && f.Address == "stranger@elsewhere.org" {
				return true
			}
		}
		return false
	}
	first := send("acc_a", "a@x.com")
	if !hasNew(first) {
		t.Fatalf("a first-time recipient must be flagged: %+v", first.Items[0].SoftFlags)
	}
	e.ap.Approve(first.RequestID)
	var st sendflow.StatusOut
	e.call(tools.ToolSendStatus, map[string]any{"request_id": first.RequestID}, &st)
	if st.Status != store.SendSent {
		t.Fatalf("%+v", st)
	}
	ctx := context.Background()
	if c, err := e.st.GetContact(ctx, "acc_a", "stranger@elsewhere.org"); err != nil || c.Trust != store.TrustTrusted {
		t.Fatalf("account A's contact = %+v, %v", c, err)
	}
	if _, err := e.st.GetContact(ctx, "acc_b", "stranger@elsewhere.org"); err == nil {
		t.Fatal("trust leaked into account B")
	}
	if second := send("acc_a", "a@x.com"); hasNew(second) {
		t.Error("account A has trusted the recipient: no new_recipient flag")
	}
	if third := send("acc_b", "b@x.com"); !hasNew(third) {
		t.Errorf("account B has never written to them: %+v", third.Items[0].SoftFlags)
	}
}

// ---- email_account_enable_send ----

func TestEnableSendThroughTheTools(t *testing.T) {
	e := newSendEnv(t, true)
	e.account("acc_a", "a@x.com", store.StatusOK) // connected: read-only
	var out tools.EnableSendOut
	if res := e.call(tools.ToolEnableSend, map[string]any{"account_id": "acc_a", "from_address": "a@x.com", "enable": true}, &out); res.IsError {
		t.Fatal(errText(res))
	}
	if out.Status != "pending" || out.ApprovalURL == "" || out.SendEnabled || out.RequestID == "" {
		t.Fatalf("out = %+v", out)
	}
	kind, _, _ := e.ap.Request(out.RequestID)
	if kind != api.KindAccountSendEnable {
		t.Fatalf("kind = %s", kind)
	}
	// Still off until the passkey approval arrives.
	var again tools.EnableSendOut
	e.call(tools.ToolEnableSend, map[string]any{"account_id": "acc_a", "from_address": "a@x.com", "enable": true}, &again)
	if again.Status != "pending" || again.RequestID != out.RequestID {
		t.Fatalf("second call = %+v", again)
	}
	a, _ := e.st.GetAccount(context.Background(), "pat", "acc_a")
	if a.SendEnabled {
		t.Fatal("send was enabled without an approval")
	}

	e.ap.Approve(out.RequestID)
	var done tools.EnableSendOut
	e.call(tools.ToolEnableSend, map[string]any{"account_id": "acc_a", "from_address": "a@x.com", "enable": true}, &done)
	if done.Status != "enabled" || !done.SendEnabled {
		t.Fatalf("after approval: %+v", done)
	}
	if a, _ = e.st.GetAccount(context.Background(), "pat", "acc_a"); !a.SendEnabled {
		t.Fatal("send_enabled was not stored")
	}

	// Disable is direct.
	var off tools.EnableSendOut
	e.call(tools.ToolEnableSend, map[string]any{"account_id": "acc_a", "from_address": "a@x.com", "enable": false}, &off)
	if off.Status != "disabled" || off.SendEnabled {
		t.Fatalf("disable: %+v", off)
	}
	if a, _ = e.st.GetAccount(context.Background(), "pat", "acc_a"); a.SendEnabled {
		t.Fatal("disable did not apply")
	}
}

func TestEnableSendRefusals(t *testing.T) {
	e := newSendEnv(t, true)
	e.account("acc_a", "a@x.com", store.StatusOK)
	e.account("acc_b", "b@x.com", store.StatusOK)
	for name, args := range map[string]map[string]any{
		"nickname as from": {"account_id": "acc_a", "from_address": "n1", "enable": true},
		"other from":       {"account_id": "acc_a", "from_address": "b@x.com", "enable": true},
		"disable mismatch": {"account_id": "acc_a", "from_address": "b@x.com", "enable": false},
		"unknown account":  {"account_id": "zzz@x.com", "from_address": "zzz@x.com", "enable": true},
		"all accounts":     {"account_id": "all", "from_address": "a@x.com", "enable": true},
		"missing account":  {"from_address": "a@x.com", "enable": true},
	} {
		if res := e.call(tools.ToolEnableSend, args, nil); !res.IsError {
			t.Errorf("%s: accepted", name)
		}
	}
	if e.ap.Creates != 0 {
		t.Fatal("a refusal created an approval request")
	}
}

func TestEnableSendReceiptForAnotherAccountIsRejectedThroughTheTool(t *testing.T) {
	e := newSendEnv(t, true)
	e.account("acc_a", "a@x.com", store.StatusOK)
	e.account("acc_b", "b@x.com", store.StatusOK)
	var ra, rb tools.EnableSendOut
	e.call(tools.ToolEnableSend, map[string]any{"account_id": "acc_a", "from_address": "a@x.com", "enable": true}, &ra)
	e.call(tools.ToolEnableSend, map[string]any{"account_id": "acc_b", "from_address": "b@x.com", "enable": true}, &rb)
	e.ap.Approve(ra.RequestID)
	e.ap.Approve(rb.RequestID)
	e.ap.Tamper(ra.RequestID, e.ap.Receipt(rb.RequestID))
	res := e.call(tools.ToolEnableSend, map[string]any{"account_id": "acc_a", "from_address": "a@x.com", "enable": true}, nil)
	if !res.IsError || !strings.HasPrefix(errText(res), "receipt_invalid:") {
		t.Fatalf("substituted receipt: %q", errText(res))
	}
	if a, _ := e.st.GetAccount(context.Background(), "pat", "acc_a"); a.SendEnabled {
		t.Fatal("a receipt for another account enabled sending")
	}
}

func TestListAccountsAppliesAnApprovedEnable(t *testing.T) {
	e := newSendEnv(t, true)
	e.account("acc_a", "a@x.com", store.StatusOK)
	var out tools.EnableSendOut
	e.call(tools.ToolEnableSend, map[string]any{"account_id": "acc_a", "from_address": "a@x.com", "enable": true}, &out)
	var list tools.ListAccountsOut
	e.call(tools.ToolListAccounts, nil, &list)
	if list.Accounts[0].SendEnabled {
		t.Fatal("listed as enabled before approval")
	}
	e.ap.Approve(out.RequestID)
	e.call(tools.ToolListAccounts, nil, &list)
	if !list.Accounts[0].SendEnabled || list.Accounts[0].HourlyCap == nil {
		t.Fatalf("after approval: %+v", list.Accounts[0])
	}
	if a, _ := e.st.GetAccount(context.Background(), "pat", "acc_a"); !a.SendEnabled {
		t.Fatal("the listing did not store send_enabled")
	}
}

func TestSendToolsFailCleanlyWithoutAnEngine(t *testing.T) {
	e := newEnv(t) // no Send
	e.account("acc_a", "a@x.com", store.StatusOK)
	for name, args := range map[string]map[string]any{
		tools.ToolRequestSend: {"account_id": "acc_a", "from_address": "a@x.com", "draft_id": "acc_a:d1"},
		tools.ToolSendStatus:  {"request_id": "r"},
		tools.ToolEnableSend:  {"account_id": "acc_a", "from_address": "a@x.com", "enable": true},
	} {
		if res := e.call(name, args, nil); !res.IsError || !strings.HasPrefix(errText(res), "internal:") {
			t.Errorf("%s: %q", name, errText(res))
		}
	}
}

func TestWriteToolAnnotations(t *testing.T) {
	// Draft and send tools are writes that reach outside but never destroy.
	e := newSendEnv(t, true)
	names := []string{tools.ToolCreateDraft, tools.ToolUpdateDraft, tools.ToolRequestSend, tools.ToolSendStatus, tools.ToolEnableSend}
	got := e.toolAnnotations(names)
	for _, n := range names {
		a := got[n]
		if a == nil || a.ReadOnlyHint || a.DestructiveHint == nil || *a.DestructiveHint || a.OpenWorldHint == nil || !*a.OpenWorldHint {
			t.Errorf("%s annotations = %+v, want readOnly=false destructive=false openWorld=true", n, a)
		}
		if desc := e.toolDescription(n); !strings.Contains(desc, tools.WriteGuidance) {
			t.Errorf("%s description lacks the write guidance", n)
		}
	}
}

// listTools returns the registered tools by name.
func (e *env) listTools() map[string]*sdk.Tool {
	e.t.Helper()
	srv := sdk.NewServer(&sdk.Implementation{Name: "t", Version: "1"}, tools.ServerOptions())
	tools.Register(srv, e.d)
	ct, stp := sdk.NewInMemoryTransports()
	ctx := context.Background()
	ss, err := srv.Connect(ctx, stp, nil)
	if err != nil {
		e.t.Fatal(err)
	}
	defer ss.Close()
	cs, err := sdk.NewClient(&sdk.Implementation{Name: "c", Version: "1"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		e.t.Fatal(err)
	}
	defer cs.Close()
	list, err := cs.ListTools(ctx, nil)
	if err != nil {
		e.t.Fatal(err)
	}
	out := map[string]*sdk.Tool{}
	for _, tl := range list.Tools {
		out[tl.Name] = tl
	}
	return out
}

func (e *env) toolAnnotations(names []string) map[string]*sdk.ToolAnnotations {
	all := e.listTools()
	out := map[string]*sdk.ToolAnnotations{}
	for _, n := range names {
		if tl := all[n]; tl != nil {
			out[n] = tl.Annotations
		}
	}
	return out
}

func (e *env) toolDescription(name string) string {
	if tl := e.listTools()[name]; tl != nil {
		return tl.Description
	}
	return ""
}
