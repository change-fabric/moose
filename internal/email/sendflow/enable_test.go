package sendflow_test

import (
	"strings"
	"testing"
	"time"

	"github.com/change-fabric/moose/internal/accounts/store"
	"github.com/change-fabric/moose/internal/approver/api"
	"github.com/change-fabric/moose/internal/email/sendflow"
)

// offAccount returns the rig's account with sending off.
func (r *rig) offAccount() store.Account {
	r.t.Helper()
	a, _ := r.st.GetAccount(ctx, "pat", r.acct.ID)
	a.SendEnabled = false
	if err := r.st.UpdateAccount(ctx, a); err != nil {
		r.t.Fatal(err)
	}
	r.acct = a
	return a
}

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
	a := r.offAccount()
	res, err := r.svc.RequestEnable(ctx, a, a.Address)
	if err != nil || res.Status != sendflow.StatusPending || res.ApprovalURL == "" {
		t.Fatalf("RequestEnable = %+v, %v", res, err)
	}
	kind, _, _ := r.ap.Request(res.RequestID)
	if kind != api.KindAccountSendEnable {
		t.Fatalf("kind = %s", kind)
	}
	ap := r.approval(res.RequestID)
	p, err := api.ParsePayload(api.KindAccountSendEnable, ap.Payload)
	if err != nil || p.SendEnable.AccountID != a.ID || p.SendEnable.FromAddress != a.Address || p.SendEnable.Owner != "pat" {
		t.Fatalf("payload = %+v, %v", p.SendEnable, err)
	}
	if pv := r.ap.Preview(res.RequestID); pv.AccountID != a.ID || pv.FromAddress != a.Address || pv.Nickname == "" {
		t.Errorf("preview = %+v", pv)
	}
	// Repeating the call reuses the request.
	if again, _ := r.svc.RequestEnable(ctx, a, a.Address); again.RequestID != res.RequestID {
		t.Errorf("second request %s, want %s", again.RequestID, res.RequestID)
	}

	// Before the approval, nothing changes.
	if got, applied, err := r.svc.ApplyEnable(ctx, a); err != nil || applied || got.SendEnabled {
		t.Fatalf("before approval: %v %v %+v", err, applied, got)
	}
	r.ap.Approve(res.RequestID)
	got, applied, err := r.svc.ApplyEnable(ctx, a)
	if err != nil || !applied || !got.SendEnabled {
		t.Fatalf("after approval: %v %v %+v", err, applied, got)
	}
	if !r.account().SendEnabled {
		t.Fatal("send_enabled was not stored")
	}
	if r.ap.Claimant(res.RequestID) != res.RequestID {
		t.Errorf("claimant = %q, want the request's own id", r.ap.Claimant(res.RequestID))
	}
	if ap := r.approval(res.RequestID); ap.Status != store.ApprovalConsumed {
		t.Errorf("approval status = %s", ap.Status)
	}
	// Idempotent: nothing more to apply.
	if _, applied, err := r.svc.ApplyEnable(ctx, r.account()); err != nil || applied {
		t.Errorf("second apply: %v %v", err, applied)
	}
}

func TestEnableRejectsAReceiptForAnotherAccountOrKindOrExpiry(t *testing.T) {
	r := newRig(t, true)
	a := r.offAccount()
	b, _ := r.addAccount("acc_two", "two@example.com")
	b.SendEnabled = false
	if err := r.st.UpdateAccount(ctx, b); err != nil {
		t.Fatal(err)
	}
	ra, err := r.svc.RequestEnable(ctx, a, a.Address)
	if err != nil {
		t.Fatal(err)
	}
	rb, err := r.svc.RequestEnable(ctx, b, b.Address)
	if err != nil {
		t.Fatal(err)
	}
	r.ap.Approve(ra.RequestID)
	r.ap.Approve(rb.RequestID)
	_, aSHA, _ := r.ap.Request(ra.RequestID)
	now := r.clk.Now()

	// Account B's genuine receipt is presented for account A's request.
	r.ap.Tamper(ra.RequestID, r.ap.Receipt(rb.RequestID))
	check := func(label string) {
		t.Helper()
		_, applied, err := r.svc.ApplyEnable(ctx, r.account())
		var e *sendflow.Error
		if applied || err == nil || !asErr(err, &e) || e.Code != sendflow.CodeReceipt {
			t.Errorf("%s: applied=%v err=%v, want receipt_invalid", label, applied, err)
		}
		if r.account().SendEnabled {
			t.Fatalf("%s: send was enabled by a bad receipt", label)
		}
	}
	check("another account's receipt")
	r.ap.Tamper(ra.RequestID, r.ap.SignFor(ra.RequestID, api.KindEmailSend, aSHA, now, now.Add(15*time.Minute)))
	check("another kind")
	r.ap.Tamper(ra.RequestID, r.ap.SignFor(ra.RequestID, api.KindAccountSendEnable, strings.Repeat("a", 64), now, now.Add(15*time.Minute)))
	check("another payload")
	r.ap.Tamper(ra.RequestID, r.ap.SignFor(ra.RequestID, api.KindAccountSendEnable, aSHA, now.Add(-40*time.Minute), now.Add(-25*time.Minute)))
	check("expired")

	// The genuine receipt for A's own request works, and B stays off.
	r.ap.Tamper(ra.RequestID, r.ap.SignFor(ra.RequestID, api.KindAccountSendEnable, aSHA, now, now.Add(15*time.Minute)))
	if _, applied, err := r.svc.ApplyEnable(ctx, r.account()); err != nil || !applied {
		t.Fatalf("genuine: %v %v", applied, err)
	}
	if got, _ := r.st.GetAccount(ctx, "pat", b.ID); got.SendEnabled {
		t.Error("account B was enabled by account A's approval")
	}
}

func TestEnableRefusalsAndDisable(t *testing.T) {
	r := newRig(t, true)
	a := r.offAccount()
	for _, from := range []string{"", "Me", "acc_one", "OTHER@example.com"} {
		if _, err := r.svc.RequestEnable(ctx, a, from); err == nil || r.refusal(err).Code != sendflow.CodeFromMismatch {
			t.Errorf("from %q: %v", from, err)
		}
	}
	if r.ap.Creates != 0 {
		t.Error("a from mismatch must not create a request")
	}
	bad := a
	bad.Status = store.StatusNeedsReauth
	if _, err := r.svc.RequestEnable(ctx, bad, a.Address); err == nil || r.refusal(err).Code != sendflow.CodeAccountNotReady {
		t.Errorf("needs_reauth: %v", err)
	}
	// Already on: nothing to ask.
	on := r.acct
	on.SendEnabled = true
	if res, err := r.svc.RequestEnable(ctx, on, on.Address); err != nil || res.Status != "already_enabled" {
		t.Errorf("already enabled: %+v %v", res, err)
	}

	// Disable applies directly and also cancels an approval that is waiting.
	res, _ := r.svc.RequestEnable(ctx, a, a.Address)
	r.ap.Approve(res.RequestID)
	if err := r.svc.Disable(ctx, a); err != nil {
		t.Fatal(err)
	}
	if _, applied, err := r.svc.ApplyEnable(ctx, r.account()); err != nil || applied || r.account().SendEnabled {
		t.Fatalf("an approval approved before the disable must not turn sending back on: %v %v", err, applied)
	}
	// Disabling an enabled account is immediate.
	r2 := newRig(t, true)
	if err := r2.svc.Disable(ctx, r2.acct); err != nil {
		t.Fatal(err)
	}
	if r2.account().SendEnabled {
		t.Error("Disable did not turn sending off")
	}
}

func TestEnableResumesAnInterruptedClaim(t *testing.T) {
	r := newRig(t, true)
	a := r.offAccount()
	res, _ := r.svc.RequestEnable(ctx, a, a.Address)
	r.ap.Approve(res.RequestID)
	// An earlier call claimed the approval locally and died before consuming.
	if _, err := r.st.TransitionApproval(ctx, res.RequestID, []string{store.ApprovalPending}, store.ApprovalApproved, false); err != nil {
		t.Fatal(err)
	}
	got, applied, err := r.svc.ApplyEnable(ctx, a)
	if err != nil || !applied || !got.SendEnabled || r.ap.Claimant(res.RequestID) != res.RequestID {
		t.Fatalf("resume: %v %v %+v claimant %q", err, applied, got, r.ap.Claimant(res.RequestID))
	}
}

func TestEnableIsRefusedWhileTheApproverIsDownAndWhenDenied(t *testing.T) {
	r := newRig(t, true)
	a := r.offAccount()
	res, _ := r.svc.RequestEnable(ctx, a, a.Address)
	r.ap.Approve(res.RequestID)
	r.ap.Down = true
	if _, applied, err := r.svc.ApplyEnable(ctx, a); err == nil || applied {
		t.Fatalf("approver down: %v %v", err, applied)
	}
	r.ap.Down = false

	r2 := newRig(t, true)
	a2 := r2.offAccount()
	res2, _ := r2.svc.RequestEnable(ctx, a2, a2.Address)
	r2.ap.Deny(res2.RequestID)
	if _, applied, err := r2.svc.ApplyEnable(ctx, a2); err != nil || applied || r2.account().SendEnabled {
		t.Fatalf("denied: %v %v", err, applied)
	}
}

// The enable request carries the banner fields the page needs: the account's
// colour, display name and status, and the other accounts for the look-alike
// warning, all as typed preview fields.
func TestEnableRequestCarriesTheBannerFields(t *testing.T) {
	r := newRig(t, true)
	a := r.offAccount()
	a.ColorIndex, a.DisplayName = 3, "Pat Work"
	if err := r.st.UpdateAccount(ctx, a); err != nil {
		t.Fatal(err)
	}
	r.addAccount("acc_two", "me@examp1e.com") // a look-alike address
	res, err := r.svc.RequestEnable(ctx, r.account(), a.Address)
	if err != nil {
		t.Fatal(err)
	}
	pv := r.ap.Preview(res.RequestID)
	if pv.ColorIndex != 3 || pv.DisplayName != "Pat Work" || pv.AccountStatus != "ok" || pv.AccountID != a.ID || pv.FromAddress != a.Address {
		t.Fatalf("preview = %+v", pv)
	}
	if len(pv.Siblings) != 1 || pv.Siblings[0].Address != "me@examp1e.com" {
		t.Fatalf("siblings = %+v", pv.Siblings)
	}
}

func asErr(err error, target **sendflow.Error) bool {
	e, ok := err.(*sendflow.Error)
	if ok {
		*target = e
	}
	return ok
}
