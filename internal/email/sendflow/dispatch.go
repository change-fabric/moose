package sendflow

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/change-fabric/moose/internal/approver/api"
	"github.com/change-fabric/moose/internal/approver/receipt"
	"github.com/change-fabric/moose/internal/email/envelope"
	"github.com/change-fabric/moose/internal/email/mailmsg"
	"github.com/change-fabric/moose/internal/email/provider"
	"github.com/change-fabric/moose/internal/email/store"
)

// Status values StatusOut.Status takes, beyond the ledger states (claimed is
// internal; sent, failed, unknown and would_send come straight from the ledger).
const (
	StatusPending        = "pending"
	StatusDispatching    = "dispatching"
	StatusBlocked        = "blocked"
	StatusRateLimited    = "rate_limited"
	StatusAlreadyClaimed = "already_claimed"
	StatusDenied         = "denied"
	StatusExpired        = "expired"
	StatusReceiptInvalid = "receipt_invalid"
	StatusClaimed        = "claimed"
	StatusQueued         = "queued"
)

// ItemStatus is the state of one send request.
type ItemStatus struct {
	RequestID string `json:"request_id"`
	Status    string `json:"status"`
	// State is the send ledger state when a row exists.
	State       string `json:"state,omitempty"`
	AccountID   string `json:"account_id,omitempty"`
	FromAddress string `json:"from_address,omitempty"`
	ApprovalURL string `json:"approval_url,omitempty"`
	Note        string `json:"note,omitempty"`
	// ProviderMessageID is the id the provider gave the sent message.
	ProviderMessageID string `json:"provider_message_id,omitempty"`
	// After a send, the source draft is deleted when it still matches the
	// approved snapshot. SourceDraftRetained is true when it was kept (edited
	// since the approval, or the delete failed); SourceDraftID names it.
	SourceDraftRetained bool   `json:"source_draft_retained,omitempty"`
	SourceDraftID       string `json:"source_draft_id,omitempty"`
	// DryRun is true for would_send: email.send.live is off, so nothing left moose.
	DryRun bool `json:"dry_run,omitempty"`
}

// StatusOut is the answer to Status. For a batch, Items holds one entry per
// message, each dispatched individually.
type StatusOut struct {
	ItemStatus
	Items []ItemStatus `json:"items,omitempty"`
}

func (s *Service) out(ap store.Approval, status, note string) StatusOut {
	return StatusOut{ItemStatus: ItemStatus{RequestID: ap.ID, Status: status, Note: note, AccountID: ap.AccountID}}
}

func terminal(state string) bool {
	switch state {
	case store.SendSent, store.SendFailed, store.SendUnknown, store.SendWouldSend:
		return true
	}
	return false
}

// Status reports a send request and, when its approval has arrived, dispatches
// it. It is safe to call any number of times, concurrently: the ledger decides
// who sends, and a message goes out at most once.
func (s *Service) Status(ctx context.Context, owner, requestID string) (StatusOut, error) {
	ap, err := s.Store.GetApproval(ctx, requestID)
	if errors.Is(err, store.ErrNotFound) || (err == nil && ap.Owner != owner) {
		return StatusOut{}, errf(CodeNotFound, "no send request %q", requestID)
	}
	if err != nil {
		return StatusOut{}, errf(CodeInternal, "could not read the request")
	}
	switch ap.Kind {
	case store.KindEmailSend:
		return s.statusItem(ctx, ap)
	case store.KindEmailSendBatch:
		return s.statusBatch(ctx, ap)
	}
	return StatusOut{}, errf(CodeNotFound, "%q is not a send request", requestID)
}

func (s *Service) account(ctx context.Context, ap store.Approval) (store.Account, error) {
	a, err := s.Store.GetAccount(ctx, ap.Owner, ap.AccountID)
	if err != nil {
		return store.Account{}, errf(CodeInternal, "could not read the account for this request")
	}
	return a, nil
}

// fromRow renders a ledger row.
func (s *Service) fromRow(ap store.Approval, a store.Account, row store.SendRow) StatusOut {
	o := StatusOut{ItemStatus: ItemStatus{RequestID: ap.ID, Status: row.State, State: row.State, AccountID: ap.AccountID, FromAddress: a.Address,
		ProviderMessageID: row.ProviderMessageID}}
	switch row.State {
	case store.SendSent:
		o.Note = "The message was sent."
	case store.SendWouldSend:
		o.DryRun = true
		o.Note = "Approved and ready, but live sending is switched off (email.send.live is false), so nothing was sent."
	case store.SendFailed:
		o.Note = "The send was refused before anything was delivered. The hourly cap slot was released."
	case store.SendUnknown:
		o.Note = "It is not known whether the message left. Check the Sent folder before sending again; moose will not retry it."
	case store.SendDispatching:
		o.Note = "The send is in progress."
	}
	return o
}

func (s *Service) statusItem(ctx context.Context, ap store.Approval) (StatusOut, error) {
	a, err := s.account(ctx, ap)
	if err != nil {
		return StatusOut{}, err
	}
	// 1. The local ledger first: a terminal row is the answer, and the
	// approver is not asked.
	row, err := s.Store.GetSend(ctx, ap.ID)
	switch {
	case err == nil:
		switch {
		case terminal(row.State):
			return s.fromRow(ap, a, row), nil
		case row.State == store.SendDispatching:
			return s.dispatching(ctx, ap, a, row)
		case row.State == store.SendClaimed:
			return s.resume(ctx, ap, a, row)
		}
		return StatusOut{}, errf(CodeInternal, "the send ledger holds an unknown state %q", row.State)
	case !errors.Is(err, store.ErrNotFound):
		return StatusOut{}, errf(CodeInternal, "could not read the send ledger")
	}

	// 2. Nothing claimed yet. Ask the approver.
	switch ap.Status {
	case store.ApprovalDenied:
		return s.out(ap, StatusDenied, "The approval was denied. Nothing was sent."), nil
	case store.ApprovalExpired:
		return s.out(ap, StatusExpired, "The approval expired before it was decided. Request the send again."), nil
	}
	st, err := s.Approver.Status(ctx, ap.ID)
	if err != nil {
		s.logf("approver status", "err", err.Error())
		o := s.out(ap, StatusPending, "The approval service could not be reached; nothing was claimed. Try again shortly.")
		o.ApprovalURL = ap.ApprovalURL
		return o, nil
	}
	switch st.Status {
	case "pending":
		o := s.out(ap, StatusPending, "Waiting for the passkey approval.")
		o.ApprovalURL = ap.ApprovalURL
		return o, nil
	case "denied", "expired":
		s.closeRequest(ctx, ap, st.Status)
		if st.Status == "denied" {
			return s.out(ap, StatusDenied, "The approval was denied. Nothing was sent."), nil
		}
		return s.out(ap, StatusExpired, "The approval expired before it was decided. Request the send again."), nil
	case "approved":
	default: // consumed by someone else, or anything unknown
		return s.out(ap, StatusAlreadyClaimed, "This approval was already used."), nil
	}

	// 3. Verify the receipt against what moose stored when it made the request.
	if err := s.verify(ctx, ap, st); err != nil {
		s.logf("receipt rejected", "request", ap.ID, "err", err.Error())
		return s.out(ap, StatusReceiptInvalid, "The approval's receipt did not verify against the stored request, so nothing was sent."), nil
	}

	// 4. Recheck the kill switch, send_enabled and the account status.
	if e := s.blocked(ctx, a); e != nil {
		o := s.out(ap, StatusBlocked, e.Msg)
		o.FromAddress = a.Address
		return o, nil
	}

	// 5. Claim the ledger row and an hourly cap slot in one transaction.
	claim := store.SendRow{RequestID: ap.ID, Owner: ap.Owner, AccountID: ap.AccountID, Created: s.now()}
	switch err := s.Store.ClaimSend(ctx, claim, a.HourlyCap); {
	case errors.Is(err, store.ErrAlreadyClaimed):
		cur, gerr := s.Store.GetSend(ctx, ap.ID)
		if gerr == nil && terminal(cur.State) {
			return s.fromRow(ap, a, cur), nil
		}
		o := s.out(ap, StatusAlreadyClaimed, "Another call is already sending this request.")
		if gerr == nil {
			o.State = cur.State
		}
		return o, nil
	case errors.Is(err, store.ErrRateLimited):
		return s.out(ap, StatusRateLimited, "The account's hourly send cap is reached; nothing was claimed. Call again once the hour has turned. An approval stays valid for 15 minutes, so if the cap does not clear in time, request the send again."), nil
	case err != nil:
		return StatusOut{}, errf(CodeInternal, "could not claim the send")
	}
	claim.State = store.SendClaimed
	return s.afterClaim(ctx, ap, a, claim)
}

// resume continues a row found in claimed: the claim and cap slot are held, so
// it picks up at the consume step. The kill switch and the account are checked
// again first; if they block, the row stays claimed and a later call resumes.
func (s *Service) resume(ctx context.Context, ap store.Approval, a store.Account, row store.SendRow) (StatusOut, error) {
	if e := s.blocked(ctx, a); e != nil {
		o := s.out(ap, StatusBlocked, e.Msg)
		o.State, o.FromAddress = store.SendClaimed, a.Address
		return o, nil
	}
	return s.afterClaim(ctx, ap, a, row)
}

// afterClaim runs steps 6 to 9: consume, then would_send or messages.send.
func (s *Service) afterClaim(ctx context.Context, ap store.Approval, a store.Account, row store.SendRow) (StatusOut, error) {
	// 6. Consume as this request's own claimant. Idempotent.
	if _, err := s.Approver.Consume(ctx, ap.ID, ap.ID); err != nil {
		var ae *api.Error
		switch {
		case errors.Is(err, api.ErrAlreadyConsumed):
			return s.failClaimed(ctx, ap, a, "The approval was already consumed by another claimant.")
		case errors.As(err, &ae) && (ae.Status == http.StatusGone || ae.Status == http.StatusConflict):
			return s.failClaimed(ctx, ap, a, "The approval could no longer be consumed ("+ae.Code+").")
		}
		s.logf("consume", "request", ap.ID, "err", err.Error())
		o := s.out(ap, StatusClaimed, "The send is claimed but the approval service could not be reached to consume it. Call again to continue; it will not be sent twice.")
		o.State, o.FromAddress = store.SendClaimed, a.Address
		return o, nil
	}
	if _, err := s.Store.TransitionApproval(ctx, ap.ID, []string{store.ApprovalPending}, store.ApprovalConsumed, false); err != nil && !errors.Is(err, store.ErrInvalidTransition) {
		s.logf("mark consumed", "request", ap.ID, "err", err.Error())
	}

	snapshot, perr := s.openSnapshot(ctx, ap, a)
	if perr != "" {
		return s.failClaimed(ctx, ap, a, perr)
	}

	// 7. Without live sending, stop at would_send.
	if !s.Live {
		next, err := s.Store.AdvanceSend(ctx, ap.ID, store.SendClaimed, store.SendWouldSend, "", s.now())
		if err != nil {
			return s.lost(ctx, ap, a, err)
		}
		s.finish(ctx, ap, a, next)
		return s.fromRow(ap, a, next), nil
	}
	if e := s.blocked(ctx, a); e != nil { // the switch can flip while we consumed
		o := s.out(ap, StatusBlocked, e.Msg)
		o.State, o.FromAddress = store.SendClaimed, a.Address
		return o, nil
	}
	if _, err := s.Store.AdvanceSend(ctx, ap.ID, store.SendClaimed, store.SendDispatching, "", s.now()); err != nil {
		return s.lost(ctx, ap, a, err)
	}
	p, err := s.Providers.Provider(ctx, a)
	if err != nil {
		// Nothing was sent: the provider could not even be opened.
		return s.failDispatching(ctx, ap, a, "The mailbox could not be opened.")
	}
	sctx, cancel := context.WithTimeout(ctx, s.dispatchTimeout())
	sent, err := p.SendDraft(sctx, snapshot, ap.ThreadID)
	cancel()
	if err != nil {
		if definite(err) {
			return s.failDispatching(ctx, ap, a, "The provider refused the message: "+refusalReason(err))
		}
		next, aerr := s.Store.AdvanceSend(ctx, ap.ID, store.SendDispatching, store.SendUnknown, "", s.now())
		if aerr != nil {
			return s.lost(ctx, ap, a, aerr)
		}
		s.audit(ctx, a, toolSendStatus, ap.ID, nil, ap.RFC822SHA256, store.SendUnknown)
		return s.fromRow(ap, a, next), nil
	}
	// 8. Persist the provider message id with the move to sent, before any cleanup.
	var next store.SendRow
	for range 3 {
		if next, err = s.Store.AdvanceSend(ctx, ap.ID, store.SendDispatching, store.SendSent, sent.MessageID, s.now()); err == nil {
			break
		}
		if errors.Is(err, store.ErrConflict) {
			break
		}
	}
	if err != nil {
		// The provider accepted it but the ledger could not say so. The row
		// stays dispatching and resolves to unknown, never to a second send.
		s.logf("record sent", "request", ap.ID, "err", err.Error())
		o := s.out(ap, StatusDispatching, "The message was handed to the provider but could not be recorded. It will be reported as unknown; check the Sent folder.")
		o.State = store.SendDispatching
		return o, nil
	}
	// 9. After sent: enroll the approved trust changes and drop the snapshot,
	// then discard the source draft if it is still what was approved.
	s.finish(ctx, ap, a, next)
	o := s.fromRow(ap, a, next)
	if !s.discardDraft(ctx, p, ap) {
		o.SourceDraftRetained, o.SourceDraftID = true, ap.DraftID
		o.Note = "The message was sent. The source draft was kept because it changed after approval or could not be deleted; discard it if you do not need it."
	}
	return o, nil
}

// discardDraft deletes the source draft after a send, but only when its bytes
// still hash to the approved snapshot, so later edits are never lost. It
// reports whether the draft is gone.
func (s *Service) discardDraft(ctx context.Context, p provider.Provider, ap store.Approval) bool {
	raw, err := p.GetDraftRaw(ctx, ap.DraftID)
	if errors.Is(err, provider.ErrNotFound) {
		return true
	}
	if err != nil {
		s.logf("read sent draft", "request", ap.ID, "err", err.Error())
		return false
	}
	if sum := sha256.Sum256(raw); hex.EncodeToString(sum[:]) != ap.RFC822SHA256 {
		return false
	}
	if err := p.DeleteDraft(ctx, ap.DraftID); err != nil && !errors.Is(err, provider.ErrNotFound) {
		s.logf("delete sent draft", "request", ap.ID, "err", err.Error())
		return false
	}
	return true
}

// dispatching handles a row found in dispatching. A call in flight keeps it
// fresh; one that is stale belonged to a crashed call and becomes unknown.
func (s *Service) dispatching(ctx context.Context, ap store.Approval, a store.Account, row store.SendRow) (StatusOut, error) {
	if s.now().Sub(row.Updated) < s.staleAfter() {
		o := s.fromRow(ap, a, row)
		o.Status = StatusDispatching
		return o, nil
	}
	next, err := s.Store.AdvanceSend(ctx, ap.ID, store.SendDispatching, store.SendUnknown, "", s.now())
	if err != nil {
		return s.lost(ctx, ap, a, err)
	}
	s.audit(ctx, a, toolSendStatus, ap.ID, nil, ap.RFC822SHA256, store.SendUnknown)
	return s.fromRow(ap, a, next), nil
}

// lost handles a conditional write that found the ledger already moved: another
// call won, so report what it left.
func (s *Service) lost(ctx context.Context, ap store.Approval, a store.Account, err error) (StatusOut, error) {
	if !errors.Is(err, store.ErrConflict) {
		return StatusOut{}, errf(CodeInternal, "could not update the send ledger")
	}
	cur, gerr := s.Store.GetSend(ctx, ap.ID)
	if gerr != nil {
		return StatusOut{}, errf(CodeInternal, "could not read the send ledger")
	}
	if terminal(cur.State) {
		return s.fromRow(ap, a, cur), nil
	}
	o := s.out(ap, StatusAlreadyClaimed, "Another call is already sending this request.")
	o.State = cur.State
	return o, nil
}

// openSnapshot opens the sealed bytes and checks them against the hashes in
// the stored request, so only the approved bytes can go out. The returned
// string is a failure reason, empty on success.
func (s *Service) openSnapshot(ctx context.Context, ap store.Approval, a store.Account) ([]byte, string) {
	if len(ap.Snapshot) == 0 {
		return nil, "The stored message snapshot is gone."
	}
	raw, err := s.Seal.Open(ctx, ap.Snapshot, envelope.Context{Owner: a.Owner, AccountID: a.ID})
	if err != nil {
		return nil, "The stored message snapshot could not be opened."
	}
	sum := sha256.Sum256(raw)
	if hex.EncodeToString(sum[:]) != ap.RFC822SHA256 {
		return nil, "The stored message snapshot does not match its approved hash."
	}
	var pl api.EmailSendPayload
	if err := json.Unmarshal(ap.Payload, &pl); err != nil || pl.RFC822SHA256 != ap.RFC822SHA256 || pl.AccountID != a.ID {
		return nil, "The stored request does not match its snapshot."
	}
	m, err := mailmsg.Parse(raw)
	if err != nil || !strings.EqualFold(m.From.Address, a.Address) {
		return nil, "The stored message is not sendable from this account."
	}
	return raw, ""
}

// definite reports a provider error that means the message was not accepted.
func definite(err error) bool {
	for _, target := range []error{provider.ErrAuth, provider.ErrNotFound, provider.ErrInvalidQuery, provider.ErrRejected, provider.ErrRateLimited} {
		if errors.Is(err, target) {
			return true
		}
	}
	return false
}

func refusalReason(err error) string {
	switch {
	case errors.Is(err, provider.ErrAuth):
		return "the account needs to be reconnected"
	case errors.Is(err, provider.ErrRateLimited):
		return "the provider is rate limiting this account"
	}
	return "it was rejected"
}

// failClaimed moves a claimed row to failed (releasing the cap slot) and ends
// the request.
func (s *Service) failClaimed(ctx context.Context, ap store.Approval, a store.Account, why string) (StatusOut, error) {
	return s.failFrom(ctx, ap, a, store.SendClaimed, why)
}

func (s *Service) failDispatching(ctx context.Context, ap store.Approval, a store.Account, why string) (StatusOut, error) {
	return s.failFrom(ctx, ap, a, store.SendDispatching, why)
}

func (s *Service) failFrom(ctx context.Context, ap store.Approval, a store.Account, from, why string) (StatusOut, error) {
	if err := s.Store.FailSend(ctx, ap.ID, from, s.now()); err != nil {
		return s.lost(ctx, ap, a, err)
	}
	cur, err := s.Store.GetSend(ctx, ap.ID)
	if err != nil {
		return StatusOut{}, errf(CodeInternal, "could not read the send ledger")
	}
	s.finish(ctx, ap, a, cur)
	o := s.fromRow(ap, a, cur)
	o.Note = why + " " + o.Note
	return o, nil
}

// finish runs the cleanup that follows a terminal outcome: audit, enrollment of
// the approved trust changes (only after a real send) and deletion of the
// message snapshot. Only unknown keeps the snapshot, until its 24 hour backstop.
func (s *Service) finish(ctx context.Context, ap store.Approval, a store.Account, row store.SendRow) {
	var pl api.EmailSendPayload
	_ = json.Unmarshal(ap.Payload, &pl)
	if row.State == store.SendSent {
		s.enroll(ctx, a, pl)
	}
	s.audit(ctx, a, toolSendStatus, ap.ID, pl.Recipients, ap.RFC822SHA256, row.State)
	if _, err := s.Store.TransitionApproval(ctx, ap.ID, []string{store.ApprovalPending, store.ApprovalConsumed}, store.ApprovalConsumed, true); err != nil {
		s.logf("delete snapshot", "request", ap.ID, "err", err.Error())
	}
}

// enroll applies the trust changes the approval carried and records every other
// recipient as known (the account has now written to them).
func (s *Service) enroll(ctx context.Context, a store.Account, pl api.EmailSendPayload) {
	changed := map[string]bool{}
	for _, c := range pl.TrustChanges {
		addr := strings.ToLower(c.Address)
		changed[addr] = true
		if cur, err := s.Store.GetContact(ctx, a.ID, addr); err == nil && cur.Trust == store.TrustBlocked {
			continue // a block set since the request still wins
		}
		if err := s.Store.PutContact(ctx, store.Contact{AccountID: a.ID, Address: addr, Trust: store.TrustTrusted, Updated: s.now()}); err != nil {
			s.logf("enroll contact", "err", err.Error())
		}
	}
	for _, r := range pl.Recipients {
		addr := strings.ToLower(r)
		if changed[addr] || strings.EqualFold(addr, a.Address) {
			continue
		}
		if _, err := s.Store.GetContact(ctx, a.ID, addr); errors.Is(err, store.ErrNotFound) {
			_ = s.Store.PutContact(ctx, store.Contact{AccountID: a.ID, Address: addr, Trust: store.TrustKnown, Updated: s.now()})
		}
	}
}

// closeRequest records a denied or expired request locally and drops its
// snapshot.
func (s *Service) closeRequest(ctx context.Context, ap store.Approval, status string) {
	to := store.ApprovalDenied
	if status == "expired" {
		to = store.ApprovalExpired
	}
	if _, err := s.Store.TransitionApproval(ctx, ap.ID, []string{store.ApprovalPending}, to, true); err != nil && !errors.Is(err, store.ErrInvalidTransition) {
		s.logf("close request", "request", ap.ID, "err", err.Error())
	}
}

// verify checks an approved status against the request moose stored. A receipt
// for another request, another payload or another kind, an expired one, and
// one that a batch did not list this request in, all fail.
func (s *Service) verify(ctx context.Context, ap store.Approval, st api.StatusResponse) error {
	canon, err := api.Canonical(ap.Payload)
	if err != nil || api.SHA256(canon) != ap.PayloadSHA256 {
		return errors.New("the stored payload does not match its stored hash")
	}
	if st.RequestID != ap.ID {
		return errors.New("the status answer is for another request")
	}
	if st.BatchID == "" {
		if st.Receipt == nil {
			return errors.New("the approved request carries no receipt")
		}
		return receipt.Verify(s.ReceiptKey, *st.Receipt, receipt.Want{
			RequestID: ap.ID, Kind: api.KindEmailSend, PayloadSHA256: ap.PayloadSHA256, RPID: s.RPID, Now: s.now(),
		})
	}
	return s.verifyViaBatch(ctx, ap, st.BatchID)
}

// verifyViaBatch verifies a request that was approved inside a batch: the
// batch's receipt must verify against the batch's stored request and the list
// it finally approved, and that list must contain this request with its stored
// payload hash.
func (s *Service) verifyViaBatch(ctx context.Context, ap store.Approval, batchID string) error {
	local, err := s.Store.GetApproval(ctx, batchID)
	if err != nil || local.Owner != ap.Owner || local.Kind != store.KindEmailSendBatch || local.AccountID != ap.AccountID {
		return errors.New("the request names a batch moose did not create for this account")
	}
	bst, err := s.Approver.Status(ctx, batchID)
	if err != nil {
		return err
	}
	if bst.RequestID != batchID || (bst.Status != "approved" && bst.Status != "consumed") || bst.Receipt == nil {
		return errors.New("the batch is not approved")
	}
	canon, err := api.Canonical(bst.Payload)
	if err != nil {
		return err
	}
	sha := api.SHA256(canon)
	if err := receipt.Verify(s.ReceiptKey, *bst.Receipt, receipt.Want{
		RequestID: batchID, Kind: api.KindEmailSendBatch, PayloadSHA256: sha, RPID: s.RPID, Now: s.now(),
	}); err != nil {
		return err
	}
	approved, err := api.ParsePayload(api.KindEmailSendBatch, canon)
	if err != nil || approved.Batch == nil {
		return errors.New("the approved batch payload is not valid")
	}
	orig, err := api.ParsePayload(api.KindEmailSendBatch, local.Payload)
	if err != nil || orig.Batch == nil {
		return errors.New("the stored batch payload is not valid")
	}
	b := approved.Batch
	if b.Owner != orig.Batch.Owner || b.AccountID != orig.Batch.AccountID || b.FromAddress != orig.Batch.FromAddress {
		return errors.New("the approved batch is for another account")
	}
	originally := map[string]string{}
	for _, it := range orig.Batch.Items {
		originally[it.RequestID] = it.PayloadSHA256
	}
	in := false
	for _, it := range b.Items {
		if originally[it.RequestID] != it.PayloadSHA256 {
			return errors.New("the approved batch lists an item moose did not request")
		}
		if it.RequestID == ap.ID {
			in = it.PayloadSHA256 == ap.PayloadSHA256
		}
	}
	if !in {
		return errors.New("the approved batch does not include this request")
	}
	return nil
}

// statusBatch reports a batch and dispatches each of its messages
// individually, under the hourly cap and the kill switch, within a time budget.
func (s *Service) statusBatch(ctx context.Context, ap store.Approval) (StatusOut, error) {
	orig, err := api.ParsePayload(api.KindEmailSendBatch, ap.Payload)
	if err != nil || orig.Batch == nil {
		return StatusOut{}, errf(CodeInternal, "the stored batch is not valid")
	}
	out := StatusOut{ItemStatus: ItemStatus{RequestID: ap.ID, AccountID: ap.AccountID, Status: StatusPending}}
	deadline := s.now().Add(s.batchBudget())
	allDone := true
	for _, it := range orig.Batch.Items {
		if !s.now().Before(deadline) {
			out.Items = append(out.Items, ItemStatus{RequestID: it.RequestID, Status: StatusQueued, Note: "Call email_send_status again to continue."})
			allDone = false
			continue
		}
		full, err := s.Status(ctx, ap.Owner, it.RequestID)
		item := full.ItemStatus
		if err != nil {
			item = ItemStatus{RequestID: it.RequestID, Status: "error", Note: err.Error()}
		}
		out.Items = append(out.Items, item)
		if !terminal(item.Status) && item.Status != StatusDenied && item.Status != StatusExpired {
			allDone = false
		}
	}
	switch {
	case allDone:
		out.Status = "done"
		out.Note = "Every message in the batch has been resolved."
		_, _ = s.Store.TransitionApproval(ctx, ap.ID, []string{store.ApprovalPending}, store.ApprovalConsumed, false)
		_, _ = s.Approver.Consume(ctx, ap.ID, ap.ID)
	default:
		out.Status = "in_progress"
		allPending := true
		for _, it := range out.Items {
			if it.Status != StatusPending {
				allPending = false
			}
		}
		if allPending {
			out.Status = StatusPending
			out.ApprovalURL = ap.ApprovalURL
			out.Note = "Waiting for the passkey approval of the batch."
		}
	}
	return out, nil
}
