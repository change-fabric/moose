package sendflow_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/change-fabric/moose/internal/accounts/envelope"
	"github.com/change-fabric/moose/internal/accounts/store"
	"github.com/change-fabric/moose/internal/accounts/store/sqlite"
	"github.com/change-fabric/moose/internal/email/fake"
	"github.com/change-fabric/moose/internal/email/mailmsg"
	"github.com/change-fabric/moose/internal/email/provider"
	"github.com/change-fabric/moose/internal/email/sendflow"
	"github.com/change-fabric/moose/internal/email/sendflow/flowtest"
)

// native is a mailbox with Graph's draft-send behaviour, for the dispatch
// tests: a staging draft that keeps its id when it is sent, an asynchronous
// Sent copy that appears after a number of looks, and the failures the engine
// must classify. Drafts, reads and the rest come from the embedded fake.
type native struct {
	*fake.Provider
	mu     sync.Mutex
	staged map[string]*stagedMsg
	order  []string
	seq    int

	StageErr error
	// IssueErr is returned by IssueSend without accepting the message. Wrap
	// provider.ErrSendRejected for a provable rejection.
	IssueErr error
	// LoseResponse accepts the message and then fails IssueSend with a transport
	// error.
	LoseResponse bool
	// HideNext gives, per staging draft in creation order, how many looks the
	// Sent copy stays invisible after the send was accepted.
	HideNext []int
	// Transform rewrites the Sent copy, to make it differ from the approval.
	Transform func([]byte) []byte
	// OnIssue runs inside IssueSend before the message is accepted.
	OnIssue func(stagingID string)

	StageCalls, IssueCalls, LookupCalls int
	Deleted                             []string
}

type stagedMsg struct {
	id, msgID string
	raw       []byte
	hide      int
	accepted  bool
	looks     int
}

func newNative() *native {
	f := fake.New()
	f.SetCapabilities(provider.Capabilities{Threads: true, SearchDialect: provider.DialectGraph, NativeDraftSend: true})
	return &native{Provider: f, staged: map[string]*stagedMsg{}}
}

var _ provider.NativeSender = (*native)(nil)

func (n *native) SendDraft(context.Context, []byte, string) (provider.SentMessage, error) {
	panic("the engine must drive a NativeDraftSend provider through NativeSender, not SendDraft")
}

func (n *native) StageDraft(_ context.Context, raw []byte) (provider.Staged, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.StageCalls++
	if n.StageErr != nil {
		return provider.Staged{}, n.StageErr
	}
	n.seq++
	m := &stagedMsg{id: fmt.Sprintf("AAStage%d=", n.seq), msgID: fmt.Sprintf("<stage%d@example.com>", n.seq), raw: append([]byte(nil), raw...)}
	if len(n.HideNext) > 0 {
		m.hide, n.HideNext = n.HideNext[0], n.HideNext[1:]
	}
	n.staged[m.id] = m
	n.order = append(n.order, m.id)
	return provider.Staged{ID: m.id, InternetMessageID: m.msgID}, nil
}

func (n *native) IssueSend(_ context.Context, id string) error {
	n.mu.Lock()
	hook := n.OnIssue
	n.mu.Unlock()
	if hook != nil {
		hook(id)
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	n.IssueCalls++
	if n.IssueErr != nil {
		return n.IssueErr
	}
	m := n.staged[id]
	if m == nil {
		return fmt.Errorf("%w: %s", provider.ErrNotFound, id)
	}
	m.accepted = true
	if n.LoseResponse {
		return errors.New("native: connection reset by peer after the send was accepted")
	}
	return nil
}

func (n *native) LookupSent(_ context.Context, id string) (provider.SentCopy, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.LookupCalls++
	m := n.staged[id]
	if m == nil {
		return provider.SentCopy{State: provider.SentMissing}, nil
	}
	if !m.accepted {
		return provider.SentCopy{State: provider.SentPending, ID: m.id, InternetMessageID: m.msgID}, nil
	}
	m.looks++
	if m.looks <= m.hide {
		return provider.SentCopy{State: provider.SentPending, ID: m.id, InternetMessageID: m.msgID}, nil
	}
	raw := m.raw
	if n.Transform != nil {
		raw = n.Transform(raw)
	}
	return provider.SentCopy{State: provider.SentFound, ID: m.id, InternetMessageID: m.msgID, Raw: raw}, nil
}

func (n *native) DeleteStaging(_ context.Context, id string) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	m := n.staged[id]
	if m == nil || m.accepted {
		return fmt.Errorf("%w: %s is not a draft", provider.ErrNotFound, id)
	}
	delete(n.staged, id)
	n.Deleted = append(n.Deleted, id)
	return nil
}

func (n *native) counts() (stage, issue int) {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.StageCalls, n.IssueCalls
}

// newNativeRig is newRig for a Microsoft account served by a native provider.
func newNativeRig(t *testing.T, live bool) (*rig, *native, *bytes.Buffer) {
	t.Helper()
	st, err := sqlite.OpenStore(ctx, filepath.Join(t.TempDir(), "e.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	seal, err := envelope.NewLocal(bytes.Repeat([]byte("k"), 32))
	if err != nil {
		t.Fatal(err)
	}
	clk := &clock{t: time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)}
	ap := flowtest.New()
	ap.Now = clk.Now
	nat := newNative()
	acct := store.Account{Owner: "pat", ID: "acc_ms", Provider: store.ProviderMicrosoft, ProviderUID: "tid.oid", Address: "me@example.com",
		Nickname: "Me", Status: store.StatusOK, SendEnabled: true, HourlyCap: 10, Created: clk.Now(), Updated: clk.Now()}
	if err := st.CreateAccount(ctx, acct); err != nil {
		t.Fatal(err)
	}
	logs := &bytes.Buffer{}
	r := &rig{t: t, st: st, ap: ap, prov: nat.Provider, clk: clk, acct: acct}
	r.svc = &sendflow.Service{Store: st, Seal: seal, Approver: ap, Providers: providers{acct.ID: nat},
		ReceiptKey: ap.PublicKeyPEM(), RPID: flowtest.RPID, Live: live, Now: clk.Now,
		Log: slog.New(slog.NewTextHandler(io.MultiWriter(logs), nil))}
	return r, nat, logs
}

func (r *rig) row(id string) store.SendRow {
	r.t.Helper()
	row, err := r.st.GetSend(ctx, id)
	if err != nil {
		r.t.Fatal(err)
	}
	return row
}

// advanceToNextLook moves the clock past the reconciliation gap.
func (r *rig) advanceToNextLook() { r.clk.Advance(sendflow.DefaultReconcileGap + time.Second) }

func TestNativeSendPersistsIntentBeforeIssueThenSubmittedThenSent(t *testing.T) {
	r, nat, _ := newNativeRig(t, true)
	id := r.approved(r.draft("ann@example.com", "Plans", "approved body"))
	var atIssue store.SendRow
	nat.OnIssue = func(stagingID string) {
		atIssue, _ = r.st.GetSend(ctx, id)
	}
	out := r.status(id)
	if atIssue.State != store.SendDispatching || atIssue.SendIssuedAt.IsZero() || atIssue.StagingID == "" || atIssue.StagingMessageID == "" {
		t.Fatalf("at the moment of the send call the ledger must already hold the staging id, its Message-ID and the send intent: %+v", atIssue)
	}
	if out.Status != store.SendSent || out.SentID == "" {
		t.Fatalf("the Sent copy was visible at once, so the first call should end sent: %+v", out)
	}
	row := r.row(id)
	if row.State != store.SendSent || row.SentID != row.StagingID || row.ProviderMessageID != row.SentID {
		t.Fatalf("row = %+v", row)
	}
	if stage, issue := nat.counts(); stage != 1 || issue != 1 {
		t.Fatalf("stage %d, issue %d, want 1 and 1", stage, issue)
	}
}

func TestNativeSendFailedOnlyWhenNeverIssued(t *testing.T) {
	r, nat, _ := newNativeRig(t, true)
	nat.StageErr = errors.New("503 from the draft create")
	id := r.approved(r.draft("ann@example.com", "s", "b"))
	out := r.status(id)
	if out.Status != store.SendFailed {
		t.Fatalf("a failure before the send was issued is a definite failure: %+v", out)
	}
	if _, issue := nat.counts(); issue != 0 {
		t.Fatalf("the send was issued %d times", issue)
	}
	if n, _ := r.st.HourlyCount(ctx, r.acct.ID, r.clk.Now()); n != 0 {
		t.Errorf("the cap slot must be released for a send that never went out: %d", n)
	}
}

func TestNativeEnumeratedRejectionFailsTheSendAndTheStagingDraftIsSwept(t *testing.T) {
	r, nat, _ := newNativeRig(t, true)
	nat.IssueErr = fmt.Errorf("%w: %w: graph 400 ErrorInvalidRecipients", provider.ErrSendRejected, provider.ErrRejected)
	id := r.approved(r.draft("ann@example.com", "s", "b"))
	out := r.status(id)
	if out.Status != store.SendFailed {
		t.Fatalf("a provable rejection is failed: %+v", out)
	}
	if n, _ := r.st.HourlyCount(ctx, r.acct.ID, r.clk.Now()); n != 0 {
		t.Errorf("cap = %d, want the slot released", n)
	}
	row := r.row(id)
	if row.StagingID == "" || row.SendIssuedAt.IsZero() {
		t.Fatalf("the staging id and intent stay on the row for the sweeper: %+v", row)
	}
	rep, err := r.svc.SweepSends(ctx)
	if err != nil || rep.Cleaned != 1 {
		t.Fatalf("sweep = %+v, %v", rep, err)
	}
	if len(nat.Deleted) != 1 || nat.Deleted[0] != row.StagingID {
		t.Fatalf("deleted = %v, want the failed row's staging draft", nat.Deleted)
	}
	rows, _ := r.st.ListSendSweep(ctx)
	if len(rows) != 0 {
		t.Fatalf("the swept row must leave the index: %+v", rows)
	}
}

func TestNativeAnUnenumeratedErrorIsUnknownAndNeverResent(t *testing.T) {
	r, nat, _ := newNativeRig(t, true)
	nat.IssueErr = errors.New("graph 503 ErrorServerBusy")
	id := r.approved(r.draft("ann@example.com", "s", "b"))
	if out := r.status(id); out.Status != store.SendUnknown {
		t.Fatalf("a 5xx on the send may mean it went out: %+v", out)
	}
	for range 3 {
		r.advanceToNextLook()
		if out := r.status(id); out.Status != store.SendUnknown {
			t.Fatalf("unknown must stay unknown while the draft is still a draft: %+v", out)
		}
	}
	if _, issue := nat.counts(); issue != 1 {
		t.Fatalf("send issued %d times; unknown is never retried", issue)
	}
	if n, _ := r.st.HourlyCount(ctx, r.acct.ID, r.clk.Now()); n != 1 {
		t.Errorf("an unknown send keeps its cap slot: %d", n)
	}
}

// The provider accepts the send and the response is lost: unknown, then the
// reconciliation finds the Sent copy and moves it to sent.
func TestNativeLostResponseIsUnknownThenReconcilesToSent(t *testing.T) {
	r, nat, _ := newNativeRig(t, true)
	nat.LoseResponse = true
	nat.HideNext = []int{0}
	id := r.approved(r.draft("ann@example.com", "s", "b"))
	out := r.status(id)
	if out.Status != store.SendUnknown {
		t.Fatalf("a lost response is unknown, never failed: %+v", out)
	}
	if _, issue := nat.counts(); issue != 1 {
		t.Fatalf("issue = %d", issue)
	}
	// Within the gap no look is made.
	looks := nat.LookupCalls
	if out := r.status(id); out.Status != store.SendUnknown || nat.LookupCalls != looks {
		t.Fatalf("a status call inside the gap must not poll: %+v, lookups %d -> %d", out, looks, nat.LookupCalls)
	}
	r.advanceToNextLook()
	out = r.status(id)
	if out.Status != store.SendSent || out.SentID == "" {
		t.Fatalf("reconciliation should have found the sent copy: %+v", out)
	}
	if _, issue := nat.counts(); issue != 1 {
		t.Fatalf("issue = %d: reconciliation must never resend", issue)
	}
}

func TestNativeCrashAfterStagingWriteReusesTheStagingDraft(t *testing.T) {
	r, nat, _ := newNativeRig(t, true)
	id := r.approved(r.draft("ann@example.com", "s", "b"))
	// The process dies right after writing the staging id: the intent write fails.
	r.svc.Store = &failingPatch{Store: r.st, to: store.SendDispatching}
	out, err := r.svc.Status(ctx, "pat", id)
	if err == nil && out.Status == store.SendSent {
		t.Fatalf("status = %+v, the crash must stop the send", out)
	}
	row := r.row(id)
	if row.State != store.SendStaging || row.StagingID == "" || !row.SendIssuedAt.IsZero() {
		t.Fatalf("row after the crash = %+v, want staging with a staging id and no intent", row)
	}
	if _, issue := nat.counts(); issue != 0 {
		t.Fatalf("nothing was issued before the crash, got %d", issue)
	}
	r.svc.Store = r.st // restart
	if out := r.status(id); out.Status != sendflow.StatusDispatching {
		t.Fatalf("a fresh staging row belongs to a call in flight: %+v", out)
	}
	r.clk.Advance(sendflow.DefaultStaleAfter + time.Second)
	out = r.status(id)
	if out.Status != store.SendSent {
		t.Fatalf("resume: %+v", out)
	}
	if stage, issue := nat.counts(); stage != 1 || issue != 1 {
		t.Fatalf("stage %d, issue %d: the resumed call must reuse the staging draft and send once", stage, issue)
	}
	if got := r.row(id).StagingID; got != row.StagingID {
		t.Errorf("staging id changed from %s to %s", row.StagingID, got)
	}
}

type failingPatch struct {
	store.Store
	to string
	// once makes only the first matching call fail.
	once bool
	n    int
}

func (f *failingPatch) PatchSend(ctx context.Context, id, from, to string, p store.SendPatch, now time.Time) (store.SendRow, error) {
	if to == f.to && (!f.once || f.n == 0) {
		f.n++
		return store.SendRow{}, errors.New("disk on fire")
	}
	return f.Store.PatchSend(ctx, id, from, to, p, now)
}

func TestNativeCrashAfterSendBeforeVerifyReconcilesWithoutResending(t *testing.T) {
	r, nat, _ := newNativeRig(t, true)
	id := r.approved(r.draft("ann@example.com", "s", "b"))
	// The send was accepted but the ledger write that records submitted fails.
	r.svc.Store = &failingAdvance{Store: r.st, to: store.SendSubmitted}
	out, err := r.svc.Status(ctx, "pat", id)
	if err != nil || out.Status != sendflow.StatusDispatching {
		t.Fatalf("status = %+v, %v", out, err)
	}
	r.svc.Store = r.st
	if row := r.row(id); row.State != store.SendDispatching || row.StagingID == "" || row.SendIssuedAt.IsZero() {
		t.Fatalf("row = %+v", row)
	}
	r.clk.Advance(sendflow.DefaultStaleAfter + time.Second)
	if out := r.status(id); out.Status != store.SendUnknown {
		t.Fatalf("stale dispatching must become unknown: %+v", out)
	}
	r.advanceToNextLook()
	out = r.status(id)
	if out.Status != store.SendSent {
		t.Fatalf("reconciliation must find the message that did go out: %+v", out)
	}
	if _, issue := nat.counts(); issue != 1 {
		t.Fatalf("issue = %d, want 1", issue)
	}
}

func TestNativeDelayedSentCopyAppearsOnAttemptFour(t *testing.T) {
	r, nat, _ := newNativeRig(t, true)
	nat.HideNext = []int{3} // looks 1 to 3 see nothing, look 4 finds the copy
	id := r.approved(r.draft("ann@example.com", "s", "b"))
	out := r.status(id) // attempt 1, made straight after the send
	if out.Status != store.SendSubmitted || out.ReconcileAttempts != 1 {
		t.Fatalf("attempt 1: %+v", out)
	}
	for attempt := 2; attempt <= 3; attempt++ {
		r.advanceToNextLook()
		out = r.status(id)
		if out.Status != store.SendSubmitted || out.ReconcileAttempts != attempt {
			t.Fatalf("attempt %d: %+v (a staging draft that is still visible is never a failure)", attempt, out)
		}
	}
	r.advanceToNextLook()
	out = r.status(id)
	if out.Status != store.SendSent || out.ReconcileAttempts != 4 || out.SentID == "" {
		t.Fatalf("attempt 4: %+v", out)
	}
	if _, issue := nat.counts(); issue != 1 {
		t.Fatalf("issue = %d", issue)
	}
}

func TestNativeInlineReconciliationIsBoundedAndTheSweeperTakesOver(t *testing.T) {
	r, nat, _ := newNativeRig(t, true)
	nat.HideNext = []int{100}
	id := r.approved(r.draft("ann@example.com", "s", "b"))
	r.status(id)
	for range 10 {
		r.advanceToNextLook()
		r.status(id)
	}
	if row := r.row(id); row.ReconcileAttempts != sendflow.MaxInlineReconcile || row.State != store.SendSubmitted {
		t.Fatalf("status calls must stop after %d looks: %+v", sendflow.MaxInlineReconcile, row)
	}
	// The sweeper carries on, and never deletes a submitted row's staging draft.
	r.advanceToNextLook()
	rep, err := r.svc.SweepSends(ctx)
	if err != nil || rep.Reconciled != 1 || len(nat.Deleted) != 0 {
		t.Fatalf("sweep = %+v, %v, deleted %v", rep, err, nat.Deleted)
	}
	if row := r.row(id); row.ReconcileAttempts != sendflow.MaxInlineReconcile+1 {
		t.Fatalf("attempts = %d", row.ReconcileAttempts)
	}
}

func TestNativeUnresolvedAfter24HoursIsSurfacedForOperatorResolution(t *testing.T) {
	r, nat, logs := newNativeRig(t, true)
	nat.HideNext = []int{10_000}
	id := r.approved(r.draft("ann@example.com", "s", "b"))
	r.status(id)
	r.clk.Advance(sendflow.DefaultReconcileWindow + time.Minute)
	rep, err := r.svc.SweepSends(ctx)
	if err != nil || rep.Escalated != 1 {
		t.Fatalf("sweep = %+v, %v", rep, err)
	}
	if !strings.Contains(logs.String(), "ALARM send_unresolved") {
		t.Errorf("the owner must be alerted:\n%s", logs.String())
	}
	out := r.status(id)
	if out.Status != store.SendUnknown || !out.NeedsOperator {
		t.Fatalf("status = %+v, want unknown with needs_operator", out)
	}
	if len(nat.Deleted) != 0 {
		t.Fatalf("a staging draft whose ledger is unknown is never deleted: %v", nat.Deleted)
	}
	evs0, _ := r.st.ListAudit(ctx, "pat", r.clk.Now().Add(-48*time.Hour), 50)
	var esc bool
	for _, e := range evs0 {
		esc = esc || (e.RequestID == id && e.Detail == "unresolved_24h")
	}
	if !esc {
		t.Errorf("the escalation must be audited: %+v", evs0)
	}

	// Operator: mark sent.
	r.clk.Advance(time.Minute)
	res, err := r.svc.ResolveUnknown(ctx, "pat", id, true)
	if err != nil || res.Status != store.SendSent {
		t.Fatalf("mark sent = %+v, %v", res, err)
	}
	evs, _ := r.st.ListAudit(ctx, "pat", r.clk.Now().Add(-48*time.Hour), 50)
	var found bool
	for _, e := range evs {
		found = found || (e.RequestID == id && e.Detail == "operator_marked_sent")
	}
	if !found {
		t.Errorf("the operator decision must be audited: %+v", evs)
	}
	if _, err := r.svc.ResolveUnknown(ctx, "pat", id, false); err == nil {
		t.Error("only an unknown send can be resolved by hand")
	}
}

func TestNativeOperatorMarkNotSentFailsItAndReleasesTheSlot(t *testing.T) {
	r, nat, _ := newNativeRig(t, true)
	nat.IssueErr = errors.New("graph 500")
	id := r.approved(r.draft("ann@example.com", "s", "b"))
	r.status(id)
	res, err := r.svc.ResolveUnknown(ctx, "pat", id, false)
	if err != nil || res.Status != store.SendFailed {
		t.Fatalf("mark not sent = %+v, %v", res, err)
	}
	if n, _ := r.st.HourlyCount(ctx, r.acct.ID, r.clk.Now()); n != 0 {
		t.Errorf("cap = %d", n)
	}
	if _, err := r.svc.ResolveUnknown(ctx, "someone-else", id, true); err == nil {
		t.Error("another owner must not resolve the request")
	}
}

func TestNativeTwoIdenticalMessagesCorrelateToTheirOwnLedgers(t *testing.T) {
	r, nat, _ := newNativeRig(t, true)
	nat.HideNext = []int{2, 0} // the first message's copy shows up later than the second's
	d1 := r.draft("ann@example.com", "Same subject", "same body")
	d2 := r.draft("ann@example.com", "Same subject", "same body")
	id1, id2 := r.approved(d1), r.approved(d2)
	o1 := r.status(id1)
	o2 := r.status(id2)
	if o1.Status != store.SendSubmitted || o2.Status != store.SendSent {
		t.Fatalf("first %+v, second %+v: each must resolve on its own staging id, not on subject or time", o1, o2)
	}
	r1, r2 := r.row(id1), r.row(id2)
	if r1.StagingID == r2.StagingID || r1.StagingID == "" {
		t.Fatalf("staging ids %q and %q", r1.StagingID, r2.StagingID)
	}
	if r2.SentID != r2.StagingID {
		t.Fatalf("second ledger got sent id %q, want its own staging id %q", r2.SentID, r2.StagingID)
	}
	for range 3 {
		r.advanceToNextLook()
		r.status(id1)
	}
	r1 = r.row(id1)
	if r1.State != store.SendSent || r1.SentID != r1.StagingID || r1.SentID == r2.SentID {
		t.Fatalf("first ledger = %+v", r1)
	}
}

func TestNativeSentMismatchIsTerminalRaisesTheAlarmAndSwitchesOffMicrosoft(t *testing.T) {
	r, nat, logs := newNativeRig(t, true)
	nat.Transform = func(raw []byte) []byte {
		return bytes.Replace(raw, []byte("approved body"), []byte("tampered body!"), 1)
	}
	id := r.approved(r.draft("ann@example.com", "Plans", "approved body"))
	id2 := r.approved(r.draft("ann@example.com", "Again", "again"))
	out := r.status(id)
	if out.Status != store.SendSentMismatch {
		t.Fatalf("status = %+v, want sent_mismatch", out)
	}
	row := r.row(id)
	if row.State != store.SendSentMismatch || row.SentID == "" {
		t.Fatalf("row = %+v", row)
	}
	if !strings.Contains(logs.String(), "ALARM sent_mismatch") {
		t.Errorf("no alarm raised:\n%s", logs.String())
	}
	if ks, _ := r.st.ProviderKillSwitch(ctx, store.ProviderMicrosoft); !ks.On || !strings.Contains(ks.Reason, id) {
		t.Fatalf("microsoft kill switch = %+v", ks)
	}
	if ks, _ := r.st.KillSwitch(ctx); ks.On {
		t.Fatal("the global switch must stay off")
	}
	// Terminal: more status calls change nothing and never resend.
	for range 2 {
		r.advanceToNextLook()
		if o := r.status(id); o.Status != store.SendSentMismatch {
			t.Fatalf("sent_mismatch must be terminal: %+v", o)
		}
	}
	if _, issue := nat.counts(); issue != 1 {
		t.Fatalf("issue = %d", issue)
	}
	// Dispatch for Microsoft is off: a second send approved earlier is blocked.
	if o := r.status(id2); o.Status != sendflow.StatusBlocked || !strings.Contains(o.Note, "microsoft") {
		t.Fatalf("a Microsoft send after a mismatch must be blocked: %+v", o)
	}
	req := sendflow.RequestIn{Account: r.acct, FromAddress: r.acct.Address, DraftIDs: []string{r.draft("ann@example.com", "x", "y")}}
	if _, err := r.svc.RequestSend(ctx, req); r.refusal(err).Code != sendflow.CodeKillSwitch {
		t.Fatalf("a new Microsoft request must be refused by the kill switch: %v", err)
	}
	// A mismatch row's staging draft is never swept.
	if _, err := r.svc.SweepSends(ctx); err != nil || len(nat.Deleted) != 0 {
		t.Fatalf("sweep err %v deleted %v", err, nat.Deleted)
	}
}

func TestNativeAMessageIDThatDiffersFromTheStagedOneIsAMismatch(t *testing.T) {
	r, _, _ := newNativeRig(t, true)
	id := r.approved(r.draft("ann@example.com", "s", "b"))
	// Rewrite the recorded Message-ID so the Sent copy's no longer agrees.
	r.svc.Store = &rewriteStaging{Store: r.st}
	out := r.status(id)
	if out.Status != store.SendSentMismatch {
		t.Fatalf("status = %+v", out)
	}
}

type rewriteStaging struct{ store.Store }

func (f *rewriteStaging) PatchSend(ctx context.Context, id, from, to string, p store.SendPatch, now time.Time) (store.SendRow, error) {
	if p.StagingMessageID != "" {
		p.StagingMessageID = "<someone-elses@example.com>"
	}
	return f.Store.PatchSend(ctx, id, from, to, p, now)
}

func TestNativeConcurrentDispatchStagesAndSendsOnce(t *testing.T) {
	r, nat, _ := newNativeRig(t, true)
	id := r.approved(r.draft("ann@example.com", "s", "b"))
	var wg sync.WaitGroup
	for range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = r.svc.Status(ctx, "pat", id)
		}()
	}
	wg.Wait()
	if stage, issue := nat.counts(); stage != 1 || issue != 1 {
		t.Fatalf("stage %d, issue %d, want exactly one of each", stage, issue)
	}
	if row := r.row(id); row.State != store.SendSent && row.State != store.SendSubmitted {
		t.Fatalf("row = %+v", row)
	}
}

func TestNativeLiveOffYieldsWouldSendWithoutTouchingTheProvider(t *testing.T) {
	r, nat, _ := newNativeRig(t, false)
	id := r.approved(r.draft("ann@example.com", "s", "b"))
	out := r.status(id)
	if out.Status != store.SendWouldSend || !out.DryRun {
		t.Fatalf("status = %+v", out)
	}
	if stage, issue := nat.counts(); stage != 0 || issue != 0 || nat.LookupCalls != 0 {
		t.Fatalf("live=false touched the provider: stage %d issue %d lookup %d", stage, issue, nat.LookupCalls)
	}
	if row := r.row(id); row.StagingID != "" {
		t.Fatalf("no staging draft may exist: %+v", row)
	}
}

func TestNativeSweeperDeletesStagingOnlyForFailedOrSentLedgers(t *testing.T) {
	r, nat, _ := newNativeRig(t, true)
	// Five ledgers, each with a staging draft, in five states.
	mk := func(name string) (string, string) {
		raw, _ := r.prov.GetDraftRaw(ctx, r.draft("ann@example.com", name, name))
		st, err := nat.StageDraft(ctx, raw)
		if err != nil {
			t.Fatal(err)
		}
		id := "req-" + name
		if err := r.st.ClaimSend(ctx, store.SendRow{RequestID: id, Owner: "pat", AccountID: r.acct.ID, Created: r.clk.Now()}, 50); err != nil {
			t.Fatal(err)
		}
		if _, err := r.st.PatchSend(ctx, id, store.SendClaimed, store.SendClaimed, store.SendPatch{StagingID: st.ID, StagingMessageID: st.InternetMessageID}, r.clk.Now()); err != nil {
			t.Fatal(err)
		}
		return id, st.ID
	}
	idFailed, stFailed := mk("failed")
	idSent, stSent := mk("sent")
	idSubmitted, stSubmitted := mk("submitted")
	idUnknown, stUnknown := mk("unknown")
	idMismatch, stMismatch := mk("mismatch")
	adv := func(id string, to ...string) {
		from := store.SendClaimed
		for _, s := range to {
			if s == store.SendFailed {
				if err := r.st.FailSend(ctx, id, from, r.clk.Now()); err != nil {
					t.Fatal(err)
				}
			} else if _, err := r.st.AdvanceSend(ctx, id, from, s, "", r.clk.Now()); err != nil {
				t.Fatal(err)
			}
			from = s
		}
	}
	adv(idFailed, store.SendFailed)
	adv(idSent, store.SendDispatching, store.SendSent)
	adv(idSubmitted, store.SendDispatching, store.SendSubmitted)
	adv(idUnknown, store.SendDispatching, store.SendUnknown)
	adv(idMismatch, store.SendDispatching, store.SendSubmitted, store.SendSentMismatch)
	// The sent row's staging message has moved to Sent Items on the provider side.
	nat.staged[stSent].accepted = true
	// Reconciliation of the submitted and unknown rows needs approvals; without
	// them they are skipped, which is fine for this test: only deletion is asserted.
	if _, err := r.svc.SweepSends(ctx); err != nil {
		t.Fatal(err)
	}
	deleted := map[string]bool{}
	for _, d := range nat.Deleted {
		deleted[d] = true
	}
	if !deleted[stFailed] || len(deleted) != 1 {
		t.Fatalf("deleted = %v, want only the failed ledger's staging draft %s", nat.Deleted, stFailed)
	}
	for name, id := range map[string]string{"submitted": stSubmitted, "unknown": stUnknown, "mismatch": stMismatch, "sent": stSent} {
		if nat.staged[id] == nil {
			t.Errorf("the %s staging message %s was deleted", name, id)
		}
	}
	// failed and sent rows leave the index once handled; the others stay or are done.
	rows, _ := r.st.ListSendSweep(ctx)
	left := map[string]bool{}
	for _, row := range rows {
		left[row.RequestID] = true
	}
	if left[idFailed] || left[idSent] || left[idMismatch] {
		t.Errorf("handled rows still indexed: %v", left)
	}
	if !left[idSubmitted] && !left[idUnknown] {
		t.Errorf("submitted and unknown rows must stay in the index until they resolve: %v", left)
	}
}

func TestNativeSourceDraftDiscardedOnlyWhileUnchanged(t *testing.T) {
	r, _, _ := newNativeRig(t, true)
	d := r.draft("ann@example.com", "s", "b")
	id := r.approved(d)
	out := r.status(id)
	if out.Status != store.SendSent || out.SourceDraftRetained {
		t.Fatalf("an unchanged source draft is discarded: %+v", out)
	}
	if _, _, ok := r.prov.Draft(d); ok {
		t.Fatal("the source draft is still there")
	}

	d2 := r.draft("bob@example.com", "s2", "b2")
	id2 := r.approved(d2)
	edited, _ := mailmsg.Build(mailmsg.Message{From: r.acct.Address, To: []string{"bob@example.com"}, Subject: "edited", Body: "edited"}, r.clk.Now())
	r.prov.SetDraftRaw(d2, edited)
	out = r.status(id2)
	if out.Status != store.SendSent || !out.SourceDraftRetained {
		t.Fatalf("an edited source draft must be kept: %+v", out)
	}
}

func TestNativePreviewSaysSentContentWillMatch(t *testing.T) {
	r, _, _ := newNativeRig(t, true)
	it := r.request(r.draft("ann@example.com", "s", "b"))
	pv := r.ap.Preview(it.RequestID)
	if !pv.SemanticMatch {
		t.Fatalf("the approver preview for a Microsoft account must carry semantic_match: %+v", pv)
	}
	g := newRig(t, true)
	git := g.request(g.draft("ann@example.com", "s", "b"))
	if g.ap.Preview(git.RequestID).SemanticMatch {
		t.Fatal("a Gmail preview must not claim a semantic match")
	}
}
