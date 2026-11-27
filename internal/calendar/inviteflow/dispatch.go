package inviteflow

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/change-fabric/moose/internal/accounts/envelope"
	"github.com/change-fabric/moose/internal/accounts/store"
	"github.com/change-fabric/moose/internal/approver/api"
	"github.com/change-fabric/moose/internal/approver/receipt"
	"github.com/change-fabric/moose/internal/calendar/provider"
)

// Status values StatusOut.Status takes, beyond the ledger states (claimed,
// dispatching, applied, failed, unknown and would_apply come from the ledger).
const (
	StatusPending        = "pending"
	StatusBlocked        = "blocked"
	StatusRateLimited    = "rate_limited"
	StatusAlreadyClaimed = "already_claimed"
	StatusDenied         = "denied"
	StatusExpired        = "expired"
	StatusReceiptInvalid = "receipt_invalid"
	StatusClaimed        = "claimed"
	StatusDispatching    = "dispatching"
	// StatusStale is a failed row whose approval was rejected because the event
	// changed between the approval and the dispatch. Nothing was written.
	StatusStale = "stale"
)

// StatusOut is the state of one invite request.
type StatusOut struct {
	RequestID string `json:"request_id"`
	// Status is pending, claimed, dispatching, applied, failed, unknown, stale,
	// would_apply, or a refusal (blocked, rate_limited, denied, expired,
	// receipt_invalid, already_claimed).
	Status      string `json:"status"`
	State       string `json:"state,omitempty"`
	Operation   string `json:"operation,omitempty"`
	AccountID   string `json:"account_id,omitempty"`
	FromAddress string `json:"from_address,omitempty"`
	ApprovalURL string `json:"approval_url,omitempty"`
	Note        string `json:"note,omitempty"`
	// CalendarID and EventID name the event the change touched, once known.
	CalendarID string `json:"calendar_id,omitempty"`
	EventID    string `json:"event_id,omitempty"`
	// Reconciliation is what looking at the provider concluded about an unknown
	// outcome.
	Reconciliation string `json:"reconciliation,omitempty"`
	// DryRun is true for would_apply: calendar.invite.live is off, so nothing
	// reached the provider.
	DryRun bool `json:"dry_run,omitempty"`
}

func (s *Service) out(ap store.Approval, status, note string) StatusOut {
	return StatusOut{RequestID: ap.ID, Status: status, Note: note, AccountID: ap.AccountID}
}

func terminal(state string) bool {
	switch state {
	case store.CalendarApplied, store.CalendarFailed, store.CalendarWouldApply:
		return true
	}
	return false
}

// Status reports an invite request and, when its approval has arrived,
// dispatches it. It is safe to call any number of times, concurrently: the
// ledger decides who writes, and a change is made at most once.
func (s *Service) Status(ctx context.Context, owner, requestID string) (StatusOut, error) {
	ap, err := s.Store.GetApproval(ctx, requestID)
	if errors.Is(err, store.ErrNotFound) || (err == nil && (ap.Owner != owner || ap.Kind != store.KindCalendarInvite)) {
		return StatusOut{}, errf(CodeNotFound, "no invite request %q", requestID)
	}
	if err != nil {
		return StatusOut{}, errf(CodeInternal, "could not read the request")
	}
	a, err := s.Store.GetAccount(ctx, ap.Owner, ap.AccountID)
	if err != nil {
		return StatusOut{}, errf(CodeInternal, "could not read the account for this request")
	}
	var pl api.CalendarInvitePayload
	if err := json.Unmarshal(ap.Payload, &pl); err != nil {
		return StatusOut{}, errf(CodeInternal, "the stored request is not valid")
	}

	// 1. The local ledger first: a terminal row is the answer, and the approver
	// is not asked.
	row, err := s.Store.GetCalendarRequest(ctx, ap.ID)
	switch {
	case err == nil:
		switch {
		case terminal(row.State):
			return s.fromRow(ap, a, row), nil
		case row.State == store.CalendarUnknown:
			return s.reconcile(ctx, ap, a, pl, row)
		case row.State == store.CalendarDispatching:
			return s.dispatching(ctx, ap, a, pl, row)
		case row.State == store.CalendarClaimed:
			return s.resume(ctx, ap, a, pl, row)
		}
		return StatusOut{}, errf(CodeInternal, "the calendar ledger holds an unknown state %q", row.State)
	case !errors.Is(err, store.ErrNotFound):
		return StatusOut{}, errf(CodeInternal, "could not read the calendar ledger")
	}

	// 2. Nothing claimed yet. Ask the approver.
	switch ap.Status {
	case store.ApprovalDenied:
		return s.out(ap, StatusDenied, "The approval was denied. Nothing was changed."), nil
	case store.ApprovalExpired:
		return s.out(ap, StatusExpired, "The approval expired before it was decided. Request the change again."), nil
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
			return s.out(ap, StatusDenied, "The approval was denied. Nothing was changed."), nil
		}
		return s.out(ap, StatusExpired, "The approval expired before it was decided. Request the change again."), nil
	case "approved":
	default:
		return s.out(ap, StatusAlreadyClaimed, "This approval was already used."), nil
	}

	// 3. Verify the receipt against what moose stored when it made the request.
	if err := s.verify(ap, st); err != nil {
		s.logf("receipt rejected", "request", ap.ID, "err", err.Error())
		return s.out(ap, StatusReceiptInvalid, "The approval's receipt did not verify against the stored request, so nothing was changed."), nil
	}

	// 4. Recheck the kill switch, invites_enabled, the grant and the account status.
	if e := s.Blocked(ctx, a); e != nil {
		o := s.out(ap, StatusBlocked, e.Msg)
		o.FromAddress = a.Address
		return o, nil
	}

	// 5. Claim the ledger row and an hourly cap slot in one transaction. The
	// dedupe key, event id and pre-call version are written with it, before any
	// call to the provider.
	claim := store.CalendarRow{
		RequestID: ap.ID, Owner: ap.Owner, AccountID: ap.AccountID, Operation: pl.Operation, Created: s.now(),
		ProviderEventID: pl.Target.EventID, Version: pl.Version,
	}
	if pl.Operation == api.CalOpCreate {
		claim.DedupeKey = ap.ID
	}
	switch err := s.Store.ClaimCalendarRequest(ctx, claim, a.HourlyCap); {
	case errors.Is(err, store.ErrAlreadyClaimed):
		cur, gerr := s.Store.GetCalendarRequest(ctx, ap.ID)
		if gerr == nil && terminal(cur.State) {
			return s.fromRow(ap, a, cur), nil
		}
		o := s.out(ap, StatusAlreadyClaimed, "Another call is already applying this request.")
		if gerr == nil {
			o.State = cur.State
		}
		return o, nil
	case errors.Is(err, store.ErrRateLimited):
		return s.out(ap, StatusRateLimited, "The account's hourly cap on sends and invites is reached; nothing was claimed. Call again once the hour has turned. An approval stays valid for 15 minutes, so if the cap does not clear in time, request the change again."), nil
	case err != nil:
		return StatusOut{}, errf(CodeInternal, "could not claim the change")
	}
	claim.State = store.CalendarClaimed
	return s.afterClaim(ctx, ap, a, pl, claim)
}

// fromRow renders a ledger row.
func (s *Service) fromRow(ap store.Approval, a store.Account, row store.CalendarRow) StatusOut {
	pl := payloadOf(ap)
	o := StatusOut{RequestID: ap.ID, Status: row.State, State: row.State, Operation: row.Operation, AccountID: ap.AccountID,
		FromAddress: a.Address, CalendarID: pl.Target.CalendarID, EventID: row.ProviderEventID}
	switch row.State {
	case store.CalendarApplied:
		o.Note = map[string]string{
			api.CalOpCreate: "The event was created and its attendees were invited.",
			api.CalOpUpdate: "The event was updated and its attendees were notified.",
			api.CalOpCancel: "The event was cancelled and its attendees were notified.",
		}[row.Operation]
	case store.CalendarWouldApply:
		o.DryRun = true
		o.Note = "Approved and ready, but live invites are switched off (calendar.invite.live is false), so nothing reached the calendar."
	case store.CalendarFailed:
		if row.Reason == "stale" {
			o.Status = StatusStale
			o.Note = "The event changed after you approved this, so the approval was rejected and nothing was written. Read the event again and request the change again. The hourly cap slot was released."
			break
		}
		o.Note = "The change was refused before anything was applied. The hourly cap slot was released."
	case store.CalendarUnknown:
		o.Note = "It is not known whether the change reached the calendar. moose will not replay it; it keeps checking the calendar. Look at the event before changing it again."
	case store.CalendarDispatching:
		o.Note = "The change is being applied."
	}
	return o
}

func payloadOf(ap store.Approval) api.CalendarInvitePayload {
	var pl api.CalendarInvitePayload
	_ = json.Unmarshal(ap.Payload, &pl)
	return pl
}

// resume continues a row found in claimed: the claim and cap slot are held, so
// it picks up at the consume step. The kill switch and the account are checked
// again first; if they block, the row stays claimed and a later call resumes.
func (s *Service) resume(ctx context.Context, ap store.Approval, a store.Account, pl api.CalendarInvitePayload, row store.CalendarRow) (StatusOut, error) {
	if e := s.Blocked(ctx, a); e != nil {
		o := s.out(ap, StatusBlocked, e.Msg)
		o.State, o.FromAddress = store.CalendarClaimed, a.Address
		return o, nil
	}
	return s.afterClaim(ctx, ap, a, pl, row)
}

// afterClaim consumes the approval, then ends in would_apply or applies the change.
func (s *Service) afterClaim(ctx context.Context, ap store.Approval, a store.Account, pl api.CalendarInvitePayload, row store.CalendarRow) (StatusOut, error) {
	if _, err := s.Approver.Consume(ctx, ap.ID, ap.ID); err != nil {
		var ae *api.Error
		switch {
		case errors.Is(err, api.ErrAlreadyConsumed):
			return s.failClaimed(ctx, ap, a, "The approval was already consumed by another claimant.")
		case errors.As(err, &ae) && (ae.Status == http.StatusGone || ae.Status == http.StatusConflict):
			return s.failClaimed(ctx, ap, a, "The approval could no longer be consumed ("+ae.Code+").")
		}
		s.logf("consume", "request", ap.ID, "err", err.Error())
		o := s.out(ap, StatusClaimed, "The change is claimed but the approval service could not be reached to consume it. Call again to continue; it will not be applied twice.")
		o.State, o.FromAddress = store.CalendarClaimed, a.Address
		return o, nil
	}
	if _, err := s.Store.TransitionApproval(ctx, ap.ID, []string{store.ApprovalPending}, store.ApprovalConsumed, false); err != nil && !errors.Is(err, store.ErrInvalidTransition) {
		s.logf("mark consumed", "request", ap.ID, "err", err.Error())
	}

	snap, reason := s.openSnapshot(ctx, ap, a, pl)
	if reason != "" {
		return s.failClaimed(ctx, ap, a, reason)
	}

	// Without live invites, stop at would_apply.
	if !s.Live {
		next, err := s.Store.AdvanceCalendarRequest(ctx, ap.ID, store.CalendarClaimed, store.CalendarWouldApply, "", s.now())
		if err != nil {
			return s.lost(ctx, ap, a, err)
		}
		s.finish(ctx, ap, a, pl, next)
		return s.fromRow(ap, a, next), nil
	}
	if e := s.Blocked(ctx, a); e != nil { // the switch can flip while we consumed
		o := s.out(ap, StatusBlocked, e.Msg)
		o.State, o.FromAddress = store.CalendarClaimed, a.Address
		return o, nil
	}
	p, err := s.Providers.CalendarProvider(ctx, a)
	if err != nil {
		// Nothing was written: the calendar could not even be opened.
		return s.failClaimed(ctx, ap, a, "The calendar could not be opened.")
	}
	if _, err := s.Store.AdvanceCalendarRequest(ctx, ap.ID, store.CalendarClaimed, store.CalendarDispatching, "", s.now()); err != nil {
		return s.lost(ctx, ap, a, err)
	}
	return s.dispatch(ctx, ap, a, pl, p, snap)
}

// dispatch applies the snapshot. The row is in dispatching.
func (s *Service) dispatch(ctx context.Context, ap store.Approval, a store.Account, pl api.CalendarInvitePayload, p provider.Provider, snap snapshot) (StatusOut, error) {
	sctx, cancel := context.WithTimeout(ctx, s.dispatchTimeout())
	defer cancel()
	ref := provider.EventRef{CalendarID: snap.CalendarID, EventID: snap.EventID}

	if pl.Operation != api.CalOpCreate {
		// Re-read: the approval is for the event at the version it was approved
		// against, with the people it named.
		cur, err := p.GetEvent(sctx, ref)
		switch {
		case errors.Is(err, provider.ErrNotFound):
			return s.failDispatching(ctx, ap, a, "", "The event no longer exists, so nothing was changed.")
		case err != nil && definite(err):
			return s.failDispatching(ctx, ap, a, "", "The calendar refused the read: "+refusalReason(err))
		case err != nil:
			// Nothing has been written yet, so a failed read is a plain failure.
			return s.failDispatching(ctx, ap, a, "", "The event could not be read before the change, so nothing was changed.")
		}
		if reason := s.staleReason(a, pl, snap, cur); reason != "" {
			return s.failDispatching(ctx, ap, a, "stale", reason)
		}
	}

	var (
		eventID string
		err     error
	)
	switch pl.Operation {
	case api.CalOpCreate:
		var made provider.Event
		made, err = p.CreateEvent(sctx, snap.CalendarID, *snap.Create, provider.CreateOptions{DedupeKey: ap.ID})
		eventID = made.ID
	case api.CalOpUpdate:
		var upd provider.Event
		upd, err = p.UpdateEvent(sctx, ref, snap.Scope, *snap.Patch, snap.Version)
		eventID = upd.ID
	case api.CalOpCancel:
		err = p.DeleteEvent(sctx, ref, snap.Scope, snap.Version)
		eventID = snap.EventID
	}
	if err != nil {
		switch {
		case errors.Is(err, provider.ErrPreconditionFailed):
			return s.failDispatching(ctx, ap, a, "stale", "The event changed at the moment of writing, so nothing was written.")
		case definite(err):
			return s.failDispatching(ctx, ap, a, "", "The calendar refused the change: "+refusalReason(err))
		}
		next, aerr := s.Store.AdvanceCalendarRequest(ctx, ap.ID, store.CalendarDispatching, store.CalendarUnknown, "", s.now())
		if aerr != nil {
			return s.lost(ctx, ap, a, aerr)
		}
		s.audit(ctx, a, toolInviteStatus, ap.ID, nil, pl.SnapshotSHA256, store.CalendarUnknown)
		return s.fromRow(ap, a, next), nil
	}
	var next store.CalendarRow
	for range 3 {
		if next, err = s.Store.AdvanceCalendarRequest(ctx, ap.ID, store.CalendarDispatching, store.CalendarApplied, eventID, s.now()); err == nil {
			break
		}
		if errors.Is(err, store.ErrConflict) {
			break
		}
	}
	if err != nil {
		// The calendar accepted it but the ledger could not say so. The row stays
		// dispatching and resolves to unknown, then to applied by reconciliation,
		// never to a second write.
		s.logf("record applied", "request", ap.ID, "err", err.Error())
		o := s.out(ap, StatusDispatching, "The change reached the calendar but could not be recorded. It will be reported as unknown, then settled by looking at the event.")
		o.State = store.CalendarDispatching
		return o, nil
	}
	s.finish(ctx, ap, a, pl, next)
	return s.fromRow(ap, a, next), nil
}

// staleReason says why a re-read event no longer matches the approval, or "".
// The version must be the one approved, the account must still organize the
// event, and the people the change would notify must be the ones approved.
func (s *Service) staleReason(a store.Account, pl api.CalendarInvitePayload, snap snapshot, cur provider.Event) string {
	if cur.Version != snap.Version {
		return "The event changed after you approved this (its version moved), so the approval was rejected and nothing was written."
	}
	if !isOrganizer(a, cur) {
		return "The account no longer organizes this event, so the approval was rejected and nothing was written."
	}
	now := eventOf(a, cur)
	var proposed *api.CalEvent
	if pl.Operation == api.CalOpUpdate && snap.Patch != nil {
		p := patched(a, now, *snap.Patch)
		proposed = &p
	}
	if !sameRecipients(api.RecipientsFor(a.Address, pl.Operation, &now, proposed), pl.Recipients) {
		return "The people this change would email are not the ones you approved, so the approval was rejected and nothing was written."
	}
	return ""
}

// definite reports a provider error that means nothing was written.
func definite(err error) bool {
	for _, target := range []error{
		provider.ErrAuth, provider.ErrNotFound, provider.ErrInvalidQuery, provider.ErrRejected, provider.ErrRateLimited,
		provider.ErrUnsupportedHere, provider.ErrInvalidRecurrence, provider.ErrInvalidTime,
	} {
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
	case errors.Is(err, provider.ErrUnsupportedHere):
		return "this account's provider cannot do that"
	}
	return "it was rejected"
}

// dispatching handles a row found in dispatching. A call in flight keeps it
// fresh; one that is stale belonged to a crashed call and becomes unknown, then
// is reconciled.
func (s *Service) dispatching(ctx context.Context, ap store.Approval, a store.Account, pl api.CalendarInvitePayload, row store.CalendarRow) (StatusOut, error) {
	if s.now().Sub(row.Updated) < s.staleAfter() {
		o := s.fromRow(ap, a, row)
		o.Status = StatusDispatching
		return o, nil
	}
	next, err := s.Store.AdvanceCalendarRequest(ctx, ap.ID, store.CalendarDispatching, store.CalendarUnknown, "", s.now())
	if err != nil {
		return s.lost(ctx, ap, a, err)
	}
	s.audit(ctx, a, toolInviteStatus, ap.ID, nil, pl.SnapshotSHA256, store.CalendarUnknown)
	return s.reconcile(ctx, ap, a, pl, next)
}

// reconcile settles an unknown row by looking at the calendar. It never writes
// to the event. A create is looked up by its dedupe key (the request id): found
// means applied, and not found for long enough means failed. An update or
// cancel is settled by the event itself: a new version carrying what was
// proposed (or, for a cancel, an event that is gone) means applied; anything
// else is inconclusive and the row stays unknown.
func (s *Service) reconcile(ctx context.Context, ap store.Approval, a store.Account, pl api.CalendarInvitePayload, row store.CalendarRow) (StatusOut, error) {
	o := s.fromRow(ap, a, row)
	p, err := s.Providers.CalendarProvider(ctx, a)
	if err != nil {
		o.Reconciliation = "The calendar could not be opened to check."
		return o, nil
	}
	cctx, cancel := context.WithTimeout(ctx, s.dispatchTimeout())
	defer cancel()
	settle := func(to, eventID, why string) (StatusOut, error) {
		var next store.CalendarRow
		var aerr error
		if to == store.CalendarFailed {
			if aerr = s.Store.FailCalendarRequest(ctx, ap.ID, store.CalendarUnknown, "", s.now()); aerr == nil {
				next, aerr = s.Store.GetCalendarRequest(ctx, ap.ID)
			}
		} else {
			next, aerr = s.Store.AdvanceCalendarRequest(ctx, ap.ID, store.CalendarUnknown, to, eventID, s.now())
		}
		if aerr != nil {
			return s.lost(ctx, ap, a, aerr)
		}
		s.finish(ctx, ap, a, pl, next)
		out := s.fromRow(ap, a, next)
		out.Reconciliation = why
		return out, nil
	}
	ref := provider.EventRef{CalendarID: pl.Target.CalendarID, EventID: pl.Target.EventID}
	switch pl.Operation {
	case api.CalOpCreate:
		f, ok := p.(provider.DedupeFinder)
		if !ok {
			o.Reconciliation = "This provider cannot look an event up by its dedupe key, so the outcome stays unknown."
			return o, nil
		}
		ev, found, err := f.FindByDedupeKey(cctx, pl.Target.CalendarID, row.DedupeKey)
		switch {
		case err != nil:
			o.Reconciliation = "The calendar could not be checked now; the outcome stays unknown."
			return o, nil
		case found:
			return settle(store.CalendarApplied, ev.ID, "An event carrying this request's dedupe key exists, so the create landed.")
		case s.now().Sub(row.Updated) < s.reconcileGap():
			o.Reconciliation = "No event carries this request's dedupe key yet; checking again shortly in case the provider is still completing the call."
			return o, nil
		}
		return settle(store.CalendarFailed, "", "No event carries this request's dedupe key, so the create did not land.")
	case api.CalOpUpdate:
		cur, err := p.GetEvent(cctx, ref)
		switch {
		case errors.Is(err, provider.ErrNotFound):
			o.Reconciliation = "The event no longer exists, so it cannot be told whether the update landed; the outcome stays unknown."
			return o, nil
		case err != nil:
			o.Reconciliation = "The event could not be read now; the outcome stays unknown."
			return o, nil
		}
		if pl.Proposed != nil && cur.Version != row.Version && matches(a, cur, *pl.Proposed) {
			return settle(store.CalendarApplied, cur.ID, "The event is at a new version and carries the approved changes, so the update landed.")
		}
		if cur.Version == row.Version {
			o.Reconciliation = "The event is still at the version it was approved against: the update has not landed, but it may still be in flight. It will not be replayed."
		} else {
			o.Reconciliation = "The event has changed since but does not carry what was approved, so it cannot be told whether the update landed. It will not be replayed."
		}
		return o, nil
	case api.CalOpCancel:
		cur, err := p.GetEvent(cctx, ref)
		switch {
		case errors.Is(err, provider.ErrNotFound):
			return settle(store.CalendarApplied, pl.Target.EventID, "The event no longer exists (cancelled), so the cancel landed.")
		case err != nil:
			o.Reconciliation = "The event could not be read now; the outcome stays unknown."
			return o, nil
		}
		if cur.Status == provider.StatusCancelled {
			return settle(store.CalendarApplied, pl.Target.EventID, "The event is cancelled, so the cancel landed.")
		}
		o.Reconciliation = "The event still exists, so the cancel has not landed, but it may still be in flight. It will not be replayed."
		return o, nil
	}
	return o, nil
}

// lost handles a conditional write that found the ledger already moved: another
// call won, so report what it left.
func (s *Service) lost(ctx context.Context, ap store.Approval, a store.Account, err error) (StatusOut, error) {
	if !errors.Is(err, store.ErrConflict) {
		return StatusOut{}, errf(CodeInternal, "could not update the calendar ledger")
	}
	cur, gerr := s.Store.GetCalendarRequest(ctx, ap.ID)
	if gerr != nil {
		return StatusOut{}, errf(CodeInternal, "could not read the calendar ledger")
	}
	if terminal(cur.State) {
		return s.fromRow(ap, a, cur), nil
	}
	o := s.out(ap, StatusAlreadyClaimed, "Another call is already applying this request.")
	o.State = cur.State
	return o, nil
}

// openSnapshot opens the sealed change and checks it against the signed
// payload, so only the approved change can be applied. The returned string is a
// failure reason, empty on success.
func (s *Service) openSnapshot(ctx context.Context, ap store.Approval, a store.Account, pl api.CalendarInvitePayload) (snapshot, string) {
	var snap snapshot
	if len(ap.Snapshot) == 0 {
		return snap, "The stored change snapshot is gone."
	}
	raw, err := s.Seal.Open(ctx, ap.Snapshot, envelope.Context{Owner: a.Owner, AccountID: a.ID})
	if err != nil {
		return snap, "The stored change snapshot could not be opened."
	}
	sum := sha256.Sum256(raw)
	if hex.EncodeToString(sum[:]) != pl.SnapshotSHA256 {
		return snap, "The stored change snapshot does not match its approved hash."
	}
	if err := json.Unmarshal(raw, &snap); err != nil {
		return snap, "The stored change snapshot is not valid."
	}
	if pl.AccountID != a.ID || !strings.EqualFold(pl.FromAddress, a.Address) || snap.Operation != pl.Operation ||
		snap.CalendarID != pl.Target.CalendarID || snap.EventID != pl.Target.EventID || snap.Version != pl.Version ||
		(pl.Operation != api.CalOpCreate && string(snap.Scope) != pl.Target.Scope) {
		return snap, "The stored change does not match its request."
	}
	switch pl.Operation {
	case api.CalOpCreate:
		if snap.Create == nil {
			return snap, "The stored change has no event to create."
		}
	case api.CalOpUpdate:
		if snap.Patch == nil {
			return snap, "The stored change has nothing to apply."
		}
	}
	return snap, ""
}

// failClaimed moves a claimed row to failed (releasing the cap slot) and ends
// the request.
func (s *Service) failClaimed(ctx context.Context, ap store.Approval, a store.Account, why string) (StatusOut, error) {
	return s.failFrom(ctx, ap, a, store.CalendarClaimed, "", why)
}

func (s *Service) failDispatching(ctx context.Context, ap store.Approval, a store.Account, reason, why string) (StatusOut, error) {
	return s.failFrom(ctx, ap, a, store.CalendarDispatching, reason, why)
}

func (s *Service) failFrom(ctx context.Context, ap store.Approval, a store.Account, from, reason, why string) (StatusOut, error) {
	if err := s.Store.FailCalendarRequest(ctx, ap.ID, from, reason, s.now()); err != nil {
		return s.lost(ctx, ap, a, err)
	}
	cur, err := s.Store.GetCalendarRequest(ctx, ap.ID)
	if err != nil {
		return StatusOut{}, errf(CodeInternal, "could not read the calendar ledger")
	}
	s.finish(ctx, ap, a, payloadOf(ap), cur)
	o := s.fromRow(ap, a, cur)
	o.Note = why + " " + o.Note
	return o, nil
}

// finish runs the cleanup that follows a terminal outcome: audit and deletion
// of the sealed snapshot. Only unknown keeps the snapshot.
func (s *Service) finish(ctx context.Context, ap store.Approval, a store.Account, pl api.CalendarInvitePayload, row store.CalendarRow) {
	s.audit(ctx, a, toolInviteStatus, ap.ID, recipientAddrs(pl.Recipients), pl.SnapshotSHA256, row.State)
	if _, err := s.Store.TransitionApproval(ctx, ap.ID, []string{store.ApprovalPending, store.ApprovalConsumed}, store.ApprovalConsumed, true); err != nil {
		s.logf("delete snapshot", "request", ap.ID, "err", err.Error())
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

// verify checks an approved status against the request moose stored: the stored
// payload still hashes to what moose recorded, and the receipt is for this
// request, this kind and this payload, unexpired, from the pinned key.
func (s *Service) verify(ap store.Approval, st api.StatusResponse) error {
	canon, err := api.Canonical(ap.Payload)
	if err != nil || api.SHA256(canon) != ap.PayloadSHA256 {
		return errors.New("the stored payload does not match its stored hash")
	}
	if st.RequestID != ap.ID {
		return errors.New("the status answer is for another request")
	}
	if st.Receipt == nil {
		return errors.New("the approved request carries no receipt")
	}
	if _, err := api.ParsePayload(api.KindCalendarInvite, canon); err != nil {
		return err
	}
	return receipt.Verify(s.ReceiptKey, *st.Receipt, receipt.Want{
		RequestID: ap.ID, Kind: api.KindCalendarInvite, PayloadSHA256: ap.PayloadSHA256, RPID: s.RPID, Now: s.now(),
	})
}
