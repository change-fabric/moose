package inviteflow_test

import (
	"strings"
	"testing"
	"time"

	"github.com/change-fabric/moose/internal/accounts/store"
	"github.com/change-fabric/moose/internal/approver/api"
	"github.com/change-fabric/moose/internal/calendar/inviteflow"
)

func (r *rig) account() store.Account {
	r.t.Helper()
	a, err := r.st.GetAccount(ctx, "pat", r.acct.ID)
	if err != nil {
		r.t.Fatal(err)
	}
	return a
}

func TestEnableNeedsAnApprovedReceiptBoundToThisAccount(t *testing.T) {
	r := newRig(t, true)
	a := r.disabledAccount()
	res, err := r.svc.RequestEnable(ctx, a, a.Address)
	if err != nil || res.Status != inviteflow.StatusPending || res.ApprovalURL == "" {
		t.Fatalf("RequestEnable = %+v, %v", res, err)
	}
	if kind, _, _ := r.ap.Request(res.RequestID); kind != api.KindAccountInviteEnable {
		t.Fatalf("kind = %s", kind)
	}
	ap, err := r.st.GetApproval(ctx, res.RequestID)
	if err != nil {
		t.Fatal(err)
	}
	p, err := api.ParsePayload(api.KindAccountInviteEnable, ap.Payload)
	if err != nil || p.InviteEnable.AccountID != a.ID || p.InviteEnable.FromAddress != a.Address || p.InviteEnable.Owner != "pat" {
		t.Fatalf("payload = %+v, %v", p.InviteEnable, err)
	}
	if pv := r.ap.Preview(res.RequestID); pv.AccountID != a.ID || pv.FromAddress != a.Address || pv.Nickname == "" || pv.CalendarGranted == nil || !*pv.CalendarGranted {
		t.Errorf("preview = %+v", pv)
	}
	// Repeating the call reuses the request.
	if again, _ := r.svc.RequestEnable(ctx, a, a.Address); again.RequestID != res.RequestID {
		t.Errorf("second request %s, want %s", again.RequestID, res.RequestID)
	}
	// Before the approval, nothing changes.
	if got, applied, err := r.svc.ApplyEnable(ctx, a); err != nil || applied || got.InviteEnabled {
		t.Fatalf("before approval: %v %v %+v", err, applied, got)
	}
	r.ap.Approve(res.RequestID)
	got, applied, err := r.svc.ApplyEnable(ctx, a)
	if err != nil || !applied || !got.InviteEnabled || !r.account().InviteEnabled {
		t.Fatalf("after approval: %v %v %+v", err, applied, got)
	}
	// An approval turns it on once: after a direct disable, the used approval does nothing.
	if err := r.svc.Disable(ctx, r.account()); err != nil {
		t.Fatal(err)
	}
	if _, applied, _ := r.svc.ApplyEnable(ctx, r.account()); applied || r.account().InviteEnabled {
		t.Fatal("a used approval turned invites back on")
	}
}

func TestEnableReceiptForAnotherKindOrAccountIsRejected(t *testing.T) {
	r := newRig(t, true)
	a := r.disabledAccount()
	res, err := r.svc.RequestEnable(ctx, a, a.Address)
	if err != nil {
		t.Fatal(err)
	}
	r.ap.Approve(res.RequestID)
	_, sha, _ := r.ap.Request(res.RequestID)
	now := r.clk.Now()
	r.ap.Tamper(res.RequestID, r.ap.SignFor(res.RequestID, api.KindAccountSendEnable, sha, now, now.Add(15*time.Minute)))
	if _, applied, err := r.svc.ApplyEnable(ctx, a); applied || err == nil {
		t.Fatalf("a send-enable receipt turned invites on: applied=%v err=%v", applied, err)
	}
	r.ap.Tamper(res.RequestID, r.ap.SignFor(res.RequestID, api.KindCalendarInvite, sha, now, now.Add(15*time.Minute)))
	if _, applied, err := r.svc.ApplyEnable(ctx, a); applied || err == nil {
		t.Fatalf("a calendar-invite receipt turned invites on: applied=%v err=%v", applied, err)
	}
	if r.account().InviteEnabled {
		t.Fatal("invites were enabled by a receipt for something else")
	}
	r.ap.Tamper(res.RequestID, r.ap.SignFor(res.RequestID, api.KindAccountInviteEnable, sha, now, now.Add(15*time.Minute)))
	if _, applied, err := r.svc.ApplyEnable(ctx, a); err != nil || !applied {
		t.Fatalf("the genuine receipt: applied=%v err=%v", applied, err)
	}
}

func TestEnableRequestRefusals(t *testing.T) {
	r := newRig(t, true)
	a := r.disabledAccount()
	if _, err := r.svc.RequestEnable(ctx, a, "someone@example.com"); r.refusal(err).Code != inviteflow.CodeInvalid {
		t.Errorf("a wrong from_address: %v", err)
	}
	if _, err := r.svc.RequestEnable(ctx, a, "Me <me@example.com>"); r.refusal(err).Code != inviteflow.CodeInvalid {
		t.Errorf("a display-name form: %v", err)
	}
	// Case alone does not make an address different.
	if _, err := r.svc.RequestEnable(ctx, a, "ME@Example.com"); err != nil {
		t.Errorf("case: %v", err)
	}
	partial := a
	partial.Services = map[string]store.ServiceGrant{store.ServiceCalendar: {Scopes: []string{calRead}, GrantedAt: r.clk.Now()}}
	if _, err := r.svc.RequestEnable(ctx, partial, a.Address); r.refusal(err).Code != inviteflow.CodeNotGranted {
		t.Errorf("a partial grant: %v", err)
	}
	broken := a
	broken.Status = store.StatusNeedsReauth
	if _, err := r.svc.RequestEnable(ctx, broken, a.Address); r.refusal(err).Code != inviteflow.CodeAccountNotReady {
		t.Errorf("an account needing reauth: %v", err)
	}
	on := r.acct
	on.InviteEnabled = true
	if res, err := r.svc.RequestEnable(ctx, on, a.Address); err != nil || res.Status != "already_enabled" {
		t.Errorf("already enabled: %+v %v", res, err)
	}
}

func TestDisableIsDirectAndCancelsAnOutstandingEnableRequest(t *testing.T) {
	r := newRig(t, true)
	a := r.disabledAccount()
	res, err := r.svc.RequestEnable(ctx, a, a.Address)
	if err != nil {
		t.Fatal(err)
	}
	r.ap.Approve(res.RequestID)
	// Disabling an already-off account cancels the approved but unapplied request.
	if err := r.svc.Disable(ctx, a); err != nil {
		t.Fatal(err)
	}
	if _, applied, _ := r.svc.ApplyEnable(ctx, r.account()); applied || r.account().InviteEnabled {
		t.Fatal("an approval that was cancelled by a disable turned invites on")
	}
	// A fresh request is needed.
	r.clk.Advance(16 * time.Minute)
	again, err := r.svc.RequestEnable(ctx, r.account(), a.Address)
	if err != nil || again.RequestID == res.RequestID {
		t.Fatalf("after a disable: %+v %v", again, err)
	}
	// Disabling an enabled account is immediate and needs no approver.
	must(t, r.st.UpdateAccount(ctx, "pat", r.acct.ID, store.AccountPatch{InviteEnabled: store.Ptr(true)}))
	r.ap.Down = true
	if err := r.svc.Disable(ctx, r.account()); err != nil || r.account().InviteEnabled {
		t.Fatalf("disable with the approver down: %v enabled=%v", err, r.account().InviteEnabled)
	}
}

func TestADeniedEnableRequestIsClosed(t *testing.T) {
	r := newRig(t, true)
	a := r.disabledAccount()
	res, err := r.svc.RequestEnable(ctx, a, a.Address)
	if err != nil {
		t.Fatal(err)
	}
	r.ap.Deny(res.RequestID)
	if _, applied, err := r.svc.ApplyEnable(ctx, a); applied || err != nil {
		t.Fatalf("denied: applied=%v err=%v", applied, err)
	}
	if ap, _ := r.st.GetApproval(ctx, res.RequestID); ap.Status != store.ApprovalDenied {
		t.Errorf("local status = %s", ap.Status)
	}
	if r.account().InviteEnabled {
		t.Error("a denied request enabled invites")
	}
}

func TestEnableIsRefusedAtApplyWhenTheCalendarGrantWasWithdrawn(t *testing.T) {
	r := newRig(t, true)
	a := r.disabledAccount()
	res, err := r.svc.RequestEnable(ctx, a, a.Address)
	if err != nil {
		t.Fatal(err)
	}
	r.ap.Approve(res.RequestID)
	g := map[string]store.ServiceGrant{store.ServiceMail: {Scopes: []string{"gmail.readonly"}, GrantedAt: r.clk.Now()}}
	must(t, r.st.UpdateAccount(ctx, "pat", r.acct.ID, store.AccountPatch{Services: &g}))
	_, applied, err := r.svc.ApplyEnable(ctx, a)
	if applied || err == nil || !strings.Contains(err.Error(), inviteflow.CodeNotGranted) {
		t.Fatalf("applied=%v err=%v", applied, err)
	}
}
