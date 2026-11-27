package sendflow

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/change-fabric/moose/internal/accounts/store"
	"github.com/change-fabric/moose/internal/approver/api"
	"github.com/change-fabric/moose/internal/email/mailmsg"
	"github.com/change-fabric/moose/internal/email/provider"
)

// Defaults for the native draft-send reconciliation knobs on Service.
const (
	// DefaultReconcileGap is the wait between looks for the Sent copy. With
	// MaxInlineReconcile it gives five attempts over about ten minutes.
	DefaultReconcileGap = 2 * time.Minute
	// MaxInlineReconcile is how many looks status calls make; after that the
	// sweeper takes over.
	MaxInlineReconcile = 5
	// DefaultReconcileWindow is how long an unresolved send is looked for before
	// it is surfaced to the owner for a manual decision.
	DefaultReconcileWindow = 24 * time.Hour
)

func (s *Service) reconcileGap() time.Duration {
	if s.ReconcileGap > 0 {
		return s.ReconcileGap
	}
	return DefaultReconcileGap
}

func (s *Service) reconcileWindow() time.Duration {
	if s.ReconcileWindow > 0 {
		return s.ReconcileWindow
	}
	return DefaultReconcileWindow
}

// reconcilable reports a row whose outcome is settled by looking for the Sent
// copy by staging id: a submitted native send, or an unknown one.
func reconcilable(row store.SendRow) bool {
	return row.StagingID != "" && (row.State == store.SendSubmitted || row.State == store.SendUnknown)
}

func issuedAt(row store.SendRow) time.Time {
	if !row.SendIssuedAt.IsZero() {
		return row.SendIssuedAt
	}
	return row.Updated
}

// needsOperator reports an unknown send that has stayed unresolved for the
// whole reconciliation window.
func (s *Service) needsOperator(row store.SendRow) bool {
	return row.State == store.SendUnknown && s.now().Sub(issuedAt(row)) >= s.reconcileWindow()
}

// dispatchNative sends through a provider that stores and re-serialises the
// message (Graph). The order is what makes it safe:
//
//  1. move the row from claimed to staging (the mutex: one call stages), create a
//     staging draft from the approved snapshot and write its id and Message-ID to
//     the ledger;
//  2. persist the send intent (dispatching, send_issued_at) before the send call;
//  3. issue the send. A 2xx moves the row to submitted. Only an enumerated
//     rejection moves it to failed. Anything else (transport error, timeout,
//     5xx, a crash from here on) leaves the outcome unknown and is never
//     retried or resent;
//  4. look for the Sent copy by staging id and compare it to the approval.
//
// A row left in staging by a crash is resumed once it is stale: with a staging
// id it continues at step 2 (nothing was issued), without one it fails, because
// nothing was ever issued.
func (s *Service) dispatchNative(ctx context.Context, ap store.Approval, a store.Account, row store.SendRow, p provider.Provider, ns provider.NativeSender, snapshot []byte) (StatusOut, error) {
	staging, err := s.Store.AdvanceSend(ctx, ap.ID, store.SendClaimed, store.SendStaging, "", s.now())
	if err != nil {
		return s.lost(ctx, ap, a, err)
	}
	row = staging
	sctx, cancel := context.WithTimeout(ctx, s.dispatchTimeout())
	staged, err := ns.StageDraft(sctx, snapshot)
	cancel()
	if err != nil {
		// Creating a draft sends nothing, so this is a definite failure.
		return s.failFrom(ctx, ap, a, store.SendStaging, "The staging draft could not be created, so nothing was sent.")
	}
	row, err = s.Store.PatchSend(ctx, ap.ID, store.SendStaging, store.SendStaging,
		store.SendPatch{StagingID: staged.ID, StagingMessageID: staged.InternetMessageID}, s.now())
	if err != nil {
		// Never issued, so deleting the draft cannot lose a send.
		dctx, dcancel := context.WithTimeout(ctx, s.dispatchTimeout())
		_ = ns.DeleteStaging(dctx, staged.ID)
		dcancel()
		return s.lost(ctx, ap, a, err)
	}
	return s.issueNative(ctx, ap, a, row, p, ns)
}

// stagingRow handles a status call that finds a row in staging. A call in
// flight keeps it fresh and is left alone. A stale one belonged to a crashed
// call: without a staging id nothing was issued and it fails; with one it
// resumes at the send intent, reusing the staging draft.
func (s *Service) stagingRow(ctx context.Context, ap store.Approval, a store.Account, row store.SendRow) (StatusOut, error) {
	if s.now().Sub(row.Updated) < s.staleAfter() {
		o := s.fromRow(ap, a, row)
		o.Status = StatusDispatching
		return o, nil
	}
	if row.StagingID == "" {
		return s.failFrom(ctx, ap, a, store.SendStaging, "The staging draft was never recorded, so nothing was sent.")
	}
	if e := s.blocked(ctx, a); e != nil {
		o := s.out(ap, StatusBlocked, e.Msg)
		o.State, o.FromAddress = store.SendStaging, a.Address
		return o, nil
	}
	p, err := s.Providers.Provider(ctx, a)
	if err != nil {
		return s.failFrom(ctx, ap, a, store.SendStaging, "The mailbox could not be opened.")
	}
	ns, ok := p.(provider.NativeSender)
	if !ok {
		return s.failFrom(ctx, ap, a, store.SendStaging, "The mailbox cannot send a staged draft.")
	}
	return s.issueNative(ctx, ap, a, row, p, ns)
}

// issueNative runs steps 2 to 4 for a row that is in staging with a staging id.
func (s *Service) issueNative(ctx context.Context, ap store.Approval, a store.Account, row store.SendRow, p provider.Provider, ns provider.NativeSender) (StatusOut, error) {
	// Send intent first: from here on the message may be on its way.
	issued := s.now()
	next, err := s.Store.PatchSend(ctx, ap.ID, store.SendStaging, store.SendDispatching, store.SendPatch{SendIssuedAt: issued}, issued)
	if err != nil {
		return s.lost(ctx, ap, a, err)
	}
	row = next

	sctx, cancel := context.WithTimeout(ctx, s.dispatchTimeout())
	err = ns.IssueSend(sctx, row.StagingID)
	cancel()
	if err != nil {
		if errors.Is(err, provider.ErrSendRejected) {
			return s.failDispatching(ctx, ap, a, "The provider refused the message: it was rejected.")
		}
		unknown, aerr := s.Store.AdvanceSend(ctx, ap.ID, store.SendDispatching, store.SendUnknown, "", s.now())
		if aerr != nil {
			return s.lost(ctx, ap, a, aerr)
		}
		s.audit(ctx, a, toolSendStatus, ap.ID, nil, ap.RFC822SHA256, store.SendUnknown)
		return s.fromRow(ap, a, unknown), nil
	}

	var submitted store.SendRow
	for range 3 {
		if submitted, err = s.Store.AdvanceSend(ctx, ap.ID, store.SendDispatching, store.SendSubmitted, "", s.now()); err == nil || errors.Is(err, store.ErrConflict) {
			break
		}
	}
	if err != nil {
		// Accepted, but the ledger could not say so. The row stays dispatching and
		// resolves to unknown (and then reconciles); it is never sent again.
		s.logf("record submitted", "request", ap.ID, "err", err.Error())
		o := s.out(ap, StatusDispatching, "The message was handed to the provider but could not be recorded. It will be reported as unknown and moose will look for the sent copy.")
		o.State = store.SendDispatching
		return o, nil
	}
	s.audit(ctx, a, toolSendStatus, ap.ID, nil, ap.RFC822SHA256, store.SendSubmitted)
	// One look straight away: Exchange usually has the Sent copy within seconds.
	return s.reconcileOnce(ctx, ap, a, submitted, p, ns)
}

// reconcile handles a status call for a submitted or unknown native send. It
// looks for the Sent copy once when a look is due (the gap since the last one
// has passed and fewer than MaxInlineReconcile were made), and otherwise just
// reports the row.
func (s *Service) reconcile(ctx context.Context, ap store.Approval, a store.Account, row store.SendRow) (StatusOut, error) {
	last := row.LastReconcile
	if last.IsZero() {
		last = issuedAt(row)
	}
	if row.ReconcileAttempts >= MaxInlineReconcile || s.now().Sub(last) < s.reconcileGap() {
		return s.fromRow(ap, a, row), nil
	}
	p, err := s.Providers.Provider(ctx, a)
	if err != nil {
		return s.fromRow(ap, a, row), nil
	}
	ns, ok := p.(provider.NativeSender)
	if !ok {
		return s.fromRow(ap, a, row), nil
	}
	return s.reconcileOnce(ctx, ap, a, row, p, ns)
}

// reconcileOnce looks for the Sent copy of row by staging id and settles the
// row when it finds one. A copy that is not there yet, or a failed look, only
// counts as an attempt: a staging draft still visible is never evidence of
// failure, and nothing is matched by subject or time.
func (s *Service) reconcileOnce(ctx context.Context, ap store.Approval, a store.Account, row store.SendRow, p provider.Provider, ns provider.NativeSender) (StatusOut, error) {
	sctx, cancel := context.WithTimeout(ctx, s.dispatchTimeout())
	cp, err := ns.LookupSent(sctx, row.StagingID)
	cancel()
	attempt := store.SendPatch{ReconcileAttempts: row.ReconcileAttempts + 1, LastReconcile: s.now()}
	if err != nil || cp.State != provider.SentFound {
		if err != nil {
			s.logf("look for sent copy", "request", ap.ID, "err", err.Error())
		}
		next, perr := s.Store.PatchSend(ctx, ap.ID, row.State, row.State, attempt, s.now())
		if perr != nil {
			return s.lost(ctx, ap, a, perr)
		}
		return s.fromRow(ap, a, next), nil
	}
	return s.settle(ctx, ap, a, row, p, cp, attempt)
}

// settle compares the correlated Sent copy to the approved message and moves
// the row to sent or sent_mismatch.
func (s *Service) settle(ctx context.Context, ap store.Approval, a store.Account, row store.SendRow, p provider.Provider, cp provider.SentCopy, attempt store.SendPatch) (StatusOut, error) {
	snapshot, why := s.openSnapshot(ctx, ap, a)
	if why != "" {
		// Without the approved bytes there is nothing to verify against.
		s.logf("cannot verify sent copy", "request", ap.ID, "why", why)
		next, perr := s.Store.PatchSend(ctx, ap.ID, row.State, row.State, attempt, s.now())
		if perr != nil {
			return s.lost(ctx, ap, a, perr)
		}
		o := s.fromRow(ap, a, next)
		o.Note = "The sent copy was found but could not be verified: " + why
		return o, nil
	}
	diff := s.verifySent(snapshot, row, cp)
	patch := attempt
	patch.SentID, patch.ProviderMessageID = cp.ID, cp.ID
	if diff != "" {
		next, perr := s.Store.PatchSend(ctx, ap.ID, row.State, store.SendSentMismatch, patch, s.now())
		if perr != nil {
			return s.lost(ctx, ap, a, perr)
		}
		s.mismatch(ctx, ap, a, diff)
		return s.fromRow(ap, a, next), nil
	}
	next, perr := s.Store.PatchSend(ctx, ap.ID, row.State, store.SendSent, patch, s.now())
	if perr != nil {
		return s.lost(ctx, ap, a, perr)
	}
	s.finish(ctx, ap, a, next)
	o := s.fromRow(ap, a, next)
	if !s.discardDraft(ctx, p, ap) {
		o.SourceDraftRetained, o.SourceDraftID = true, ap.DraftID
		o.Note = "The message was sent and the sent copy matches the approval. The source draft was kept because it changed after approval or could not be deleted; discard it if you do not need it."
	}
	return o, nil
}

// verifySent compares From, to, cc, bcc, subject and decoded text body of the
// Sent copy with the approved parse. It returns "" when they agree.
func (s *Service) verifySent(snapshot []byte, row store.SendRow, cp provider.SentCopy) string {
	want, err := mailmsg.Parse(snapshot)
	if err != nil {
		return "the approved message could not be read"
	}
	if len(cp.Raw) == 0 {
		return "the sent copy could not be read as plain text"
	}
	got, err := mailmsg.Parse(cp.Raw)
	if err != nil {
		return "the sent copy could not be read as plain text"
	}
	if diff := mailmsg.SemanticDiff(want, got); diff != "" {
		return diff
	}
	if row.StagingMessageID != "" && cp.InternetMessageID != "" && row.StagingMessageID != cp.InternetMessageID {
		return "message id differs"
	}
	return ""
}

// mismatch raises the alarm for a sent copy that differs from the approval:
// mail may have been delivered, so it is never retried, the owner is alerted,
// and dispatch for the account's provider is switched off until someone
// investigates. The approval's snapshot is kept for that investigation.
func (s *Service) mismatch(ctx context.Context, ap store.Approval, a store.Account, diff string) {
	prov := providerOf(a)
	reason := "sent_mismatch on request " + ap.ID + ": " + diff
	if err := s.Store.SetProviderKillSwitch(ctx, prov, true, reason, s.now()); err != nil {
		s.logf("ALARM could not switch off provider after sent_mismatch", "provider", prov, "request", ap.ID, "err", err.Error())
	}
	s.alarm("sent_mismatch", "request", ap.ID, "account", a.ID, "provider", prov, "diff", diff)
	s.audit(ctx, a, toolSendStatus, ap.ID, nil, ap.RFC822SHA256, store.SendSentMismatch)
	if _, err := s.Store.TransitionApproval(ctx, ap.ID, []string{store.ApprovalPending, store.ApprovalConsumed}, store.ApprovalConsumed, false); err != nil && !errors.Is(err, store.ErrInvalidTransition) {
		s.logf("mark consumed", "request", ap.ID, "err", err.Error())
	}
}

// alarm records an owner-facing alert. It is an error-level log line with a
// stable alarm key, which the deployment's log metric filter turns into an
// alarm.
func (s *Service) alarm(kind string, args ...any) {
	if s.Log != nil {
		s.Log.Error("sendflow: ALARM "+kind, append([]any{"alarm", kind}, args...)...)
	}
}

// ResolveUnknown is the operator's decision on a send that stayed unknown after
// the reconciliation window: the owner checked the Sent folder and marks it
// sent or not sent. It is audited. Only an unknown row can be resolved, and a
// message marked not sent is not sent by moose either: the ledger moves to
// failed and the cap slot is released, and the staging draft is cleaned up by
// the sweeper.
func (s *Service) ResolveUnknown(ctx context.Context, owner, requestID string, sent bool) (StatusOut, error) {
	ap, err := s.Store.GetApproval(ctx, requestID)
	if errors.Is(err, store.ErrNotFound) || (err == nil && ap.Owner != owner) {
		return StatusOut{}, errf(CodeNotFound, "no send request %q", requestID)
	}
	if err != nil || ap.Kind != store.KindEmailSend {
		return StatusOut{}, errf(CodeNotFound, "%q is not a send request", requestID)
	}
	a, err := s.account(ctx, ap)
	if err != nil {
		return StatusOut{}, err
	}
	row, err := s.Store.GetSend(ctx, requestID)
	if err != nil {
		return StatusOut{}, errf(CodeNotFound, "no send ledger entry for %q", requestID)
	}
	if row.State != store.SendUnknown {
		return StatusOut{}, errf(CodeInvalid, "only an unknown send can be resolved by hand; this one is %s", row.State)
	}
	if sent {
		next, err := s.Store.PatchSend(ctx, requestID, store.SendUnknown, store.SendSent, store.SendPatch{}, s.now())
		if err != nil {
			return s.lost(ctx, ap, a, err)
		}
		s.finish(ctx, ap, a, next)
		s.auditOperator(ctx, a, requestID, ap.RFC822SHA256, "operator_marked_sent")
		return s.fromRow(ap, a, next), nil
	}
	if err := s.Store.FailSend(ctx, requestID, store.SendUnknown, s.now()); err != nil {
		return s.lost(ctx, ap, a, err)
	}
	cur, err := s.Store.GetSend(ctx, requestID)
	if err != nil {
		return StatusOut{}, errf(CodeInternal, "could not read the send ledger")
	}
	s.finish(ctx, ap, a, cur)
	s.auditOperator(ctx, a, requestID, ap.RFC822SHA256, "operator_marked_not_sent")
	return s.fromRow(ap, a, cur), nil
}

// auditOperator records an operator's decision. Audit rows are keyed by time
// and request, so it is stamped a tick after the outcome row finish wrote.
func (s *Service) auditOperator(ctx context.Context, a store.Account, requestID, bodySHA, detail string) {
	err := s.Store.AppendAudit(ctx, store.AuditEvent{
		At: s.now().Add(time.Nanosecond), Owner: a.Owner, RequestID: requestID, AccountID: a.ID, Tool: toolSendStatus,
		BodySHA256: bodySHA, ApprovalID: requestID, Detail: detail,
	})
	if err != nil {
		s.logf("audit", "err", err.Error())
	}
}

// SweepReport counts what one SweepSends pass did.
type SweepReport struct {
	Examined   int `json:"examined"`
	Reconciled int `json:"reconciled"`
	Cleaned    int `json:"cleaned"`
	Escalated  int `json:"escalated"`
	Skipped    int `json:"skipped"`
	Errors     int `json:"errors"`
}

const maxSweepRows = 200

// SweepSends is the sweeper's step for native draft-sends. For each ledger row
// that has a staging draft it
//
//   - reconciles submitted and unknown sends (looks for the Sent copy), until
//     the reconciliation window has passed, then surfaces the row to the owner;
//   - moves a dispatching row whose call died to unknown, as a status call does;
//   - deletes the staging draft of a failed or sent row, but only while it is
//     still a draft, so a Sent Items copy is never deleted. A staging draft whose
//     ledger is submitted or unknown is never deleted;
//   - leaves sent_mismatch rows and their messages alone.
func (s *Service) SweepSends(ctx context.Context) (SweepReport, error) {
	var rep SweepReport
	rows, err := s.Store.ListSendSweep(ctx)
	if err != nil {
		return rep, errors.Join(errors.New("sendflow: list sweep rows"), err)
	}
	if len(rows) > maxSweepRows {
		rows = rows[:maxSweepRows]
	}
	for _, row := range rows {
		rep.Examined++
		if err := s.sweepRow(ctx, row, &rep); err != nil {
			rep.Errors++
			s.logf("sweep", "request", row.RequestID, "err", err.Error())
		}
	}
	if rep.Errors > 0 {
		return rep, errors.New("sendflow: sweep incomplete")
	}
	return rep, nil
}

func (s *Service) sweepRow(ctx context.Context, row store.SendRow, rep *SweepReport) error {
	ap, aerr := s.Store.GetApproval(ctx, row.RequestID)
	a, err := s.Store.GetAccount(ctx, row.Owner, row.AccountID)
	if err != nil {
		rep.Skipped++
		return nil
	}
	if a.Status == store.StatusProviderDisabled {
		rep.Skipped++
		return nil
	}
	done := func(state string) error {
		_, err := s.Store.PatchSend(ctx, row.RequestID, state, state, store.SendPatch{SweepDone: true}, s.now())
		if errors.Is(err, store.ErrConflict) {
			return nil // another call moved it; the next pass looks again
		}
		return err
	}
	switch row.State {
	case store.SendClaimed:
		rep.Skipped++
	case store.SendStaging:
		if aerr != nil {
			rep.Skipped++
			return nil
		}
		if _, err := s.stagingRow(ctx, ap, a, row); err != nil {
			return err
		}
	case store.SendDispatching:
		if aerr != nil {
			rep.Skipped++
			return nil
		}
		if _, err := s.dispatching(ctx, ap, a, row); err != nil {
			return err
		}
	case store.SendSubmitted, store.SendUnknown:
		if s.now().Sub(issuedAt(row)) >= s.reconcileWindow() {
			return s.escalate(ctx, row, a, ap, aerr, rep)
		}
		if aerr != nil {
			rep.Skipped++
			return nil
		}
		last := row.LastReconcile
		if last.IsZero() {
			last = issuedAt(row)
		}
		if s.now().Sub(last) < s.reconcileGap() {
			rep.Skipped++
			return nil
		}
		p, err := s.Providers.Provider(ctx, a)
		if err != nil {
			return err
		}
		ns, ok := p.(provider.NativeSender)
		if !ok {
			rep.Skipped++
			return nil
		}
		if _, err := s.reconcileOnce(ctx, ap, a, row, p, ns); err != nil {
			return err
		}
		rep.Reconciled++
	case store.SendFailed, store.SendSent:
		p, err := s.Providers.Provider(ctx, a)
		if err != nil {
			return err
		}
		ns, ok := p.(provider.NativeSender)
		if !ok {
			rep.Skipped++
			return nil
		}
		dctx, cancel := context.WithTimeout(ctx, s.dispatchTimeout())
		err = ns.DeleteStaging(dctx, row.StagingID)
		cancel()
		if err != nil && !errors.Is(err, provider.ErrNotFound) {
			return err
		}
		if err := done(row.State); err != nil {
			return err
		}
		rep.Cleaned++
	default: // sent_mismatch, would_send: nothing to delete, nothing more to do
		return done(row.State)
	}
	return nil
}

// escalate surfaces a send that stayed unresolved through the whole window: a
// submitted row becomes unknown, the owner is alerted, and the sweeper stops
// looking. Status then reports needs_operator.
func (s *Service) escalate(ctx context.Context, row store.SendRow, a store.Account, ap store.Approval, aerr error, rep *SweepReport) error {
	cur := row
	if row.State == store.SendSubmitted {
		next, err := s.Store.PatchSend(ctx, row.RequestID, store.SendSubmitted, store.SendUnknown, store.SendPatch{}, s.now())
		if err != nil {
			if errors.Is(err, store.ErrConflict) {
				return nil
			}
			return err
		}
		cur = next
	}
	if _, err := s.Store.PatchSend(ctx, row.RequestID, cur.State, cur.State, store.SendPatch{SweepDone: true}, s.now()); err != nil {
		if errors.Is(err, store.ErrConflict) {
			return nil
		}
		return err
	}
	var recips []string
	if aerr == nil {
		var pl api.EmailSendPayload
		if json.Unmarshal(ap.Payload, &pl) == nil {
			recips = pl.Recipients
		}
	}
	s.alarm("send_unresolved", "request", row.RequestID, "account", a.ID, "issued_at", issuedAt(row))
	s.audit(ctx, a, toolSendStatus, row.RequestID, recips, ap.RFC822SHA256, "unresolved_24h")
	rep.Escalated++
	return nil
}
