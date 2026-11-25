package sendflow_test

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/change-fabric/moose/internal/approver/api"
	"github.com/change-fabric/moose/internal/email/envelope"
	"github.com/change-fabric/moose/internal/email/sendflow"
	"github.com/change-fabric/moose/internal/email/sendflow/flowtest"
	"github.com/change-fabric/moose/internal/email/store"
	"github.com/change-fabric/moose/internal/email/store/sqlite"
)

func (r *rig) propose(addrs ...string) sendflow.TrustProposal {
	r.t.Helper()
	p, err := r.svc.ProposeTrust(ctx, r.account(), addrs)
	if err != nil {
		r.t.Fatalf("ProposeTrust: %v", err)
	}
	return p
}

func (r *rig) trustOf(accountID, addr string) string {
	r.t.Helper()
	c, err := r.st.GetContact(ctx, accountID, addr)
	if errors.Is(err, store.ErrNotFound) {
		return store.TrustNew
	}
	if err != nil {
		r.t.Fatal(err)
	}
	return c.Trust
}

func TestProposeTrustCollectsEverythingIntoOneRequest(t *testing.T) {
	r := newRig(t, true)
	_ = r.st.PutContact(ctx, store.Contact{AccountID: r.acct.ID, Address: "done@x.com", Trust: store.TrustTrusted})
	_ = r.st.PutContact(ctx, store.Contact{AccountID: r.acct.ID, Address: "bad@x.com", Trust: store.TrustBlocked})
	p := r.propose("B@x.com", "a@x.com", "a@x.com", "done@x.com", "bad@x.com")
	if p.Status != sendflow.StatusPending || p.RequestID == "" || p.ApprovalURL == "" || p.ExpiresAt == nil {
		t.Fatalf("proposal = %+v", p)
	}
	if strings.Join(p.Addresses, ",") != "a@x.com,b@x.com" || strings.Join(p.AlreadyTrusted, ",") != "done@x.com" || strings.Join(p.Blocked, ",") != "bad@x.com" {
		t.Fatalf("proposal lists = %+v", p)
	}
	if r.ap.Creates != 1 {
		t.Fatalf("approver creates = %d, want one request for the whole batch", r.ap.Creates)
	}
	kind, _, _ := r.ap.Request(p.RequestID)
	if kind != api.KindContactTrustBatch {
		t.Fatalf("kind = %s", kind)
	}
	ap := r.approval(p.RequestID)
	pp, err := api.ParsePayload(api.KindContactTrustBatch, ap.Payload)
	if err != nil || pp.Trust.AccountID != r.acct.ID || pp.Trust.Owner != "pat" || len(pp.Trust.Changes) != 2 ||
		pp.Trust.Changes[0].Address != "a@x.com" || pp.Trust.Changes[1].Address != "b@x.com" || pp.Trust.Changes[0].To != "trusted" {
		t.Fatalf("payload = %+v, %v", pp.Trust, err)
	}
	if pv := r.ap.Preview(p.RequestID); pv.AccountID != r.acct.ID {
		t.Errorf("preview = %+v", pv)
	}
	// Nothing is trusted before the approval.
	if r.trustOf(r.acct.ID, "a@x.com") != store.TrustNew {
		t.Fatal("a proposal changed a contact")
	}
	// The same set in another order and case is the same request.
	if again := r.propose("a@x.com", "b@x.com"); again.RequestID != p.RequestID || r.ap.Creates != 1 {
		t.Fatalf("repeat = %+v, creates %d", again, r.ap.Creates)
	}
	// A different set is its own request.
	if other := r.propose("c@x.com"); other.RequestID == p.RequestID {
		t.Fatal("a different set reused the request")
	}
	// Nothing left to ask.
	if none := r.propose("done@x.com", "bad@x.com"); none.Status != sendflow.ContactsNone || none.RequestID != "" {
		t.Fatalf("empty proposal = %+v", none)
	}
}

func TestProposeTrustAfterDenialMakesAFreshRequest(t *testing.T) {
	r := newRig(t, true)
	p := r.propose("a@x.com")
	r.ap.Deny(p.RequestID)
	again := r.propose("a@x.com")
	if again.RequestID == p.RequestID || again.Status != sendflow.StatusPending {
		t.Fatalf("after denial: %+v", again)
	}
}

func TestContactsStatusAppliesAnApprovedBatchOnce(t *testing.T) {
	r := newRig(t, true)
	p := r.propose("a@x.com", "b@x.com")
	// Pending first.
	out, err := r.svc.ContactsStatus(ctx, "pat", p.RequestID)
	if err != nil || out.Status != sendflow.StatusPending || out.ApprovalURL == "" {
		t.Fatalf("pending: %+v, %v", out, err)
	}
	if r.trustOf(r.acct.ID, "a@x.com") != store.TrustNew {
		t.Fatal("applied before approval")
	}
	r.ap.Approve(p.RequestID)
	out, err = r.svc.ContactsStatus(ctx, "pat", p.RequestID)
	if err != nil || out.Status != sendflow.ContactsApplied || strings.Join(out.Applied, ",") != "a@x.com,b@x.com" {
		t.Fatalf("applied: %+v, %v", out, err)
	}
	for _, a := range []string{"a@x.com", "b@x.com"} {
		if r.trustOf(r.acct.ID, a) != store.TrustTrusted {
			t.Errorf("%s not trusted", a)
		}
	}
	if r.ap.Claimant(p.RequestID) != p.RequestID {
		t.Errorf("claimant = %q", r.ap.Claimant(p.RequestID))
	}
	if ap := r.approval(p.RequestID); ap.Status != store.ApprovalConsumed {
		t.Errorf("approval status = %s", ap.Status)
	}
	// Replay: a second call returns the ledger and changes nothing, even if
	// the owner has since changed a contact.
	if err := r.st.PutContact(ctx, store.Contact{AccountID: r.acct.ID, Address: "a@x.com", Trust: store.TrustBlocked}); err != nil {
		t.Fatal(err)
	}
	consumes := r.ap.ConsumeCalls
	again, err := r.svc.ContactsStatus(ctx, "pat", p.RequestID)
	if err != nil || again.Status != sendflow.ContactsApplied || strings.Join(again.Applied, ",") != "a@x.com,b@x.com" {
		t.Fatalf("replay: %+v, %v", again, err)
	}
	if r.trustOf(r.acct.ID, "a@x.com") != store.TrustBlocked {
		t.Fatal("a replay re-applied the batch over a later block")
	}
	if r.ap.ConsumeCalls != consumes {
		t.Error("a replay of a consumed batch called consume again")
	}
}

func TestContactsStatusSkipsAddressesBlockedAfterTheRequest(t *testing.T) {
	r := newRig(t, true)
	p := r.propose("a@x.com", "b@x.com")
	r.ap.Approve(p.RequestID)
	if err := r.st.PutContact(ctx, store.Contact{AccountID: r.acct.ID, Address: "b@x.com", Trust: store.TrustBlocked}); err != nil {
		t.Fatal(err)
	}
	out, err := r.svc.ContactsStatus(ctx, "pat", p.RequestID)
	if err != nil || out.Status != sendflow.ContactsApplied || strings.Join(out.Applied, ",") != "a@x.com" || strings.Join(out.Skipped, ",") != "b@x.com" {
		t.Fatalf("out = %+v, %v", out, err)
	}
	if r.trustOf(r.acct.ID, "b@x.com") != store.TrustBlocked || r.trustOf(r.acct.ID, "a@x.com") != store.TrustTrusted {
		t.Fatal("a block must win over an approved trust change")
	}
}

func TestContactsStatusDeniedAndExpired(t *testing.T) {
	r := newRig(t, true)
	p := r.propose("a@x.com")
	r.ap.Deny(p.RequestID)
	out, err := r.svc.ContactsStatus(ctx, "pat", p.RequestID)
	if err != nil || out.Status != "denied" {
		t.Fatalf("denied: %+v, %v", out, err)
	}
	if again, _ := r.svc.ContactsStatus(ctx, "pat", p.RequestID); again.Status != "denied" {
		t.Fatalf("denied again: %+v", again)
	}
	q := r.propose("b@x.com")
	r.clk.Advance(time.Hour)
	out, err = r.svc.ContactsStatus(ctx, "pat", q.RequestID)
	if err != nil || out.Status != "expired" {
		t.Fatalf("expired: %+v, %v", out, err)
	}
	if r.trustOf(r.acct.ID, "a@x.com") != store.TrustNew || r.trustOf(r.acct.ID, "b@x.com") != store.TrustNew {
		t.Fatal("a denied or expired request changed a contact")
	}
}

func TestContactsStatusRejectsSubstitutedReceipts(t *testing.T) {
	r := newRig(t, true)
	p := r.propose("a@x.com")
	other := r.propose("evil@x.com")
	r.ap.Approve(p.RequestID)
	r.ap.Approve(other.RequestID)
	_, sha, _ := r.ap.Request(p.RequestID)
	now := r.clk.Now()
	exp := now.Add(15 * time.Minute)
	check := func(label string) {
		t.Helper()
		_, err := r.svc.ContactsStatus(ctx, "pat", p.RequestID)
		var e *sendflow.Error
		if err == nil || !asErr(err, &e) || e.Code != sendflow.CodeReceipt {
			t.Errorf("%s: err = %v, want receipt_invalid", label, err)
		}
		if r.trustOf(r.acct.ID, "a@x.com") != store.TrustNew || r.trustOf(r.acct.ID, "evil@x.com") != store.TrustNew {
			t.Fatalf("%s: a contact changed", label)
		}
		if _, err := r.st.GetContactBatch(ctx, p.RequestID); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("%s: a ledger row exists", label)
		}
	}
	r.ap.Tamper(p.RequestID, r.ap.Receipt(other.RequestID))
	check("another request's receipt")
	r.ap.Tamper(p.RequestID, r.ap.SignFor(p.RequestID, api.KindEmailSend, sha, now, exp))
	check("another kind")
	r.ap.Tamper(p.RequestID, r.ap.SignFor(p.RequestID, api.KindContactTrustBatch, strings.Repeat("b", 64), now, exp))
	check("another payload hash")
	r.ap.Tamper(p.RequestID, r.ap.SignFor(p.RequestID, api.KindContactTrustBatch, sha, now.Add(-40*time.Minute), now.Add(-25*time.Minute)))
	check("expired receipt")
	if r.ap.ConsumeCalls != 0 {
		t.Error("a rejected receipt reached consume")
	}
	// The genuine receipt works.
	r.ap.Tamper(p.RequestID, r.ap.SignFor(p.RequestID, api.KindContactTrustBatch, sha, now, exp))
	if out, err := r.svc.ContactsStatus(ctx, "pat", p.RequestID); err != nil || out.Status != sendflow.ContactsApplied {
		t.Fatalf("genuine: %+v, %v", out, err)
	}
	if r.trustOf(r.acct.ID, "evil@x.com") != store.TrustNew {
		t.Error("another request's addresses were trusted")
	}
}

func TestContactsStatusIsPerOwnerAndKind(t *testing.T) {
	r := newRig(t, true)
	p := r.propose("a@x.com")
	var e *sendflow.Error
	if _, err := r.svc.ContactsStatus(ctx, "mallory", p.RequestID); err == nil || !asErr(err, &e) || e.Code != sendflow.CodeNotFound {
		t.Errorf("another owner: %v", err)
	}
	if _, err := r.svc.ContactsStatus(ctx, "pat", "nope"); err == nil || !asErr(err, &e) || e.Code != sendflow.CodeNotFound {
		t.Errorf("unknown request: %v", err)
	}
	a := r.offAccount()
	en, err := r.svc.RequestEnable(ctx, a, a.Address)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.svc.ContactsStatus(ctx, "pat", en.RequestID); err == nil || !asErr(err, &e) || e.Code != sendflow.CodeNotFound {
		t.Errorf("another kind of request: %v", err)
	}
}

func TestContactsStatusApproverDownChangesNothing(t *testing.T) {
	r := newRig(t, true)
	p := r.propose("a@x.com")
	r.ap.Approve(p.RequestID)
	r.ap.Down = true
	_, err := r.svc.ContactsStatus(ctx, "pat", p.RequestID)
	var e *sendflow.Error
	if err == nil || !asErr(err, &e) || e.Code != sendflow.CodeApprover {
		t.Fatalf("err = %v", err)
	}
	if r.trustOf(r.acct.ID, "a@x.com") != store.TrustNew {
		t.Fatal("changed while the approver was down")
	}
	r.ap.Down = false
	if out, err := r.svc.ContactsStatus(ctx, "pat", p.RequestID); err != nil || out.Status != sendflow.ContactsApplied {
		t.Fatalf("retry: %+v, %v", out, err)
	}
}

func TestContactsStatusConsumeFailureResumes(t *testing.T) {
	r := newRig(t, true)
	p := r.propose("a@x.com")
	r.ap.Approve(p.RequestID)
	// The approval service goes away between the apply and the consume.
	r.svc.Approver = &downAfterStatus{Approver: r.ap}
	out, err := r.svc.ContactsStatus(ctx, "pat", p.RequestID)
	if err != nil || out.Status != sendflow.ContactsApplied || !strings.Contains(out.Note, "call again") {
		t.Fatalf("first call: %+v, %v", out, err)
	}
	if r.trustOf(r.acct.ID, "a@x.com") != store.TrustTrusted {
		t.Fatal("the change was not applied")
	}
	if r.ap.ConsumeCalls != 0 {
		t.Fatal("consume reached the approver")
	}
	r.svc.Approver = r.ap
	out, err = r.svc.ContactsStatus(ctx, "pat", p.RequestID)
	if err != nil || out.Status != sendflow.ContactsApplied || strings.Contains(out.Note, "call again") {
		t.Fatalf("resume: %+v, %v", out, err)
	}
	if r.ap.Claimant(p.RequestID) != p.RequestID || r.approval(p.RequestID).Status != store.ApprovalConsumed {
		t.Error("the resumed call did not consume")
	}
}

// downAfterStatus answers status calls and fails consume, as an approver that
// went away right after the receipt was read would.
type downAfterStatus struct{ *flowtest.Approver }

func (d *downAfterStatus) Consume(context.Context, string, string) (api.StatusResponse, error) {
	return api.StatusResponse{}, &api.Error{Status: 502, Code: "down"}
}

func TestConcurrentContactsStatusAppliesOnce(t *testing.T) {
	r := newRig(t, true)
	p := r.propose("a@x.com", "b@x.com")
	r.ap.Approve(p.RequestID)
	var wg sync.WaitGroup
	results := make([]sendflow.ContactsStatusOut, 8)
	errs := make([]error, 8)
	for i := range results {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i], errs[i] = r.svc.ContactsStatus(ctx, "pat", p.RequestID)
		}()
	}
	wg.Wait()
	for i := range results {
		if errs[i] != nil || results[i].Status != sendflow.ContactsApplied || len(results[i].Applied) != 2 {
			t.Errorf("call %d: %+v, %v", i, results[i], errs[i])
		}
	}
	if r.approval(p.RequestID).Status != store.ApprovalConsumed {
		t.Error("not consumed")
	}
}

func TestTrustIsPerAccount(t *testing.T) {
	r := newRig(t, true)
	two, _ := r.addAccount("acc_two", "two@example.com")
	p := r.propose("a@x.com")
	r.ap.Approve(p.RequestID)
	if _, err := r.svc.ContactsStatus(ctx, "pat", p.RequestID); err != nil {
		t.Fatal(err)
	}
	if r.trustOf(r.acct.ID, "a@x.com") != store.TrustTrusted || r.trustOf(two.ID, "a@x.com") != store.TrustNew {
		t.Fatal("trust leaked across accounts")
	}
	// The same address proposed for the other account is its own request.
	q, err := r.svc.ProposeTrust(ctx, two, []string{"a@x.com"})
	if err != nil || q.RequestID == p.RequestID || q.Status != sendflow.StatusPending {
		t.Fatalf("second account: %+v, %v", q, err)
	}
}

func TestProposeTrustRefusesTooManyAddresses(t *testing.T) {
	r := newRig(t, true)
	var addrs []string
	for i := range store.MaxContactBatch + 1 {
		addrs = append(addrs, "u"+string(rune('a'+i%26))+string(rune('a'+i/26))+"@x.com")
	}
	if _, err := r.svc.ProposeTrust(ctx, r.account(), addrs); err == nil || r.refusal(err).Code != sendflow.CodeInvalid {
		t.Fatalf("err = %v", err)
	}
	if r.ap.Creates != 0 {
		t.Fatal("an oversize request reached the approver")
	}
}

// ---- partial failure ----

// racing is a Backend that fails the contact transaction partway: before
// delegating a Transact that touches contacts it lets a "concurrent writer"
// change one contact, so that op's condition fails and the whole transaction
// must abort.
type racing struct {
	store.Backend
	mu     sync.Mutex
	always bool
	hit    int
	race   func()
}

func (b *racing) Transact(ctx context.Context, ops []store.Op) error {
	if len(ops) > 2 && strings.HasPrefix(ops[0].Item.PK, "CBATCH#") {
		b.mu.Lock()
		b.hit++
		fire := b.always || b.hit == 1
		b.mu.Unlock()
		if fire && b.race != nil {
			b.race()
		}
	}
	return b.Backend.Transact(ctx, ops)
}

func newRacingRig(t *testing.T, always bool) (*rig, *racing) {
	t.Helper()
	r := newRig(t, true)
	be, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "race.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { be.Close() })
	rb := &racing{Backend: be, always: always}
	st := store.NewWithClock(rb, r.clk.Now)
	if err := st.CreateAccount(ctx, r.acct); err != nil {
		t.Fatal(err)
	}
	seal, _ := envelope.NewLocal(bytes.Repeat([]byte("k"), 32))
	r.st = st
	r.svc = &sendflow.Service{Store: st, Seal: seal, Approver: r.ap, Providers: providers{r.acct.ID: r.prov},
		ReceiptKey: r.ap.PublicKeyPEM(), RPID: flowtest.RPID, Live: true, Now: r.clk.Now}
	return r, rb
}

func TestAPartialFailureAppliesNoChange(t *testing.T) {
	r, rb := newRacingRig(t, true)
	for _, a := range []string{"a@x.com", "b@x.com", "c@x.com"} {
		if err := r.st.PutContact(ctx, store.Contact{AccountID: r.acct.ID, Address: a, Trust: store.TrustKnown}); err != nil {
			t.Fatal(err)
		}
	}
	p := r.propose("a@x.com", "b@x.com", "c@x.com")
	r.ap.Approve(p.RequestID)
	// A writer touches the last contact between every read and write, so the
	// transaction fails after the first two changes were already staged.
	n := 0
	rb.race = func() {
		n++
		it, ok, err := rb.Backend.Get(ctx, "ACCT#"+r.acct.ID, "CONTACT#c@x.com")
		if err != nil || !ok {
			t.Errorf("race read: %v %v", ok, err)
			return
		}
		it.Ver++
		if err := rb.Backend.Transact(ctx, []store.Op{{Kind: store.OpPut, Item: it}}); err != nil {
			t.Errorf("race write: %v", err)
		}
	}
	_, err := r.svc.ContactsStatus(ctx, "pat", p.RequestID)
	var e *sendflow.Error
	if err == nil || !asErr(err, &e) || e.Code != sendflow.CodeInternal {
		t.Fatalf("err = %v, want an internal refusal", err)
	}
	if n < 2 {
		t.Fatalf("the writer raced %d times; the apply never retried", n)
	}
	for _, a := range []string{"a@x.com", "b@x.com", "c@x.com"} {
		if got := r.trustOf(r.acct.ID, a); got != store.TrustKnown {
			t.Errorf("%s = %s after a failed apply, want known: no change may land", a, got)
		}
	}
	if _, err := r.st.GetContactBatch(ctx, p.RequestID); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("a failed apply left a claim behind")
	}
	if r.ap.ConsumeCalls != 0 || r.approval(p.RequestID).Status == store.ApprovalConsumed {
		t.Error("a failed apply consumed the approval")
	}

	// The writer stops; the same request now applies in full.
	rb.mu.Lock()
	rb.always, rb.hit = false, 1
	rb.mu.Unlock()
	out, err := r.svc.ContactsStatus(ctx, "pat", p.RequestID)
	if err != nil || out.Status != sendflow.ContactsApplied || len(out.Applied) != 3 {
		t.Fatalf("retry: %+v, %v", out, err)
	}
	for _, a := range []string{"a@x.com", "b@x.com", "c@x.com"} {
		if r.trustOf(r.acct.ID, a) != store.TrustTrusted {
			t.Errorf("%s not trusted after the retry", a)
		}
	}
}

func TestAContactChangedMidApplyIsReadAgainAndStillBlockWins(t *testing.T) {
	r, rb := newRacingRig(t, false)
	p := r.propose("a@x.com", "b@x.com")
	r.ap.Approve(p.RequestID)
	// Once, just before the first attempt writes, b is blocked.
	rb.race = func() {
		if err := r.st.PutContact(ctx, store.Contact{AccountID: r.acct.ID, Address: "b@x.com", Trust: store.TrustBlocked}); err != nil {
			t.Error(err)
		}
	}
	out, err := r.svc.ContactsStatus(ctx, "pat", p.RequestID)
	if err != nil || out.Status != sendflow.ContactsApplied || strings.Join(out.Applied, ",") != "a@x.com" || strings.Join(out.Skipped, ",") != "b@x.com" {
		t.Fatalf("out = %+v, %v", out, err)
	}
	if r.trustOf(r.acct.ID, "b@x.com") != store.TrustBlocked {
		t.Fatal("a block set during the apply was overwritten")
	}
}
