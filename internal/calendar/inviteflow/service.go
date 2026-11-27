// Package inviteflow is moose-calendar's approved-invite engine: it snapshots a
// create, update or cancel of an event that notifies someone other than the
// account, asks the approver for a passkey approval of exactly that change, and
// later applies the stored change once, and only once, after the approval's
// receipt verifies. It mirrors internal/email/sendflow.
//
// A request runs in this order:
//
//	request  read the event, work out the proposed event and everyone the
//	         provider will email (added, kept, removed, cancelled), refuse an
//	         organizer-only operation on an event the account is only invited
//	         to, check invites_enabled, the calendar grant, the kill switch and
//	         the hourly cap, seal the exact provider input, and create the
//	         approver request keyed by the snapshot.
//	dispatch verify the receipt against the stored request, recheck the kill
//	         switch and the account, claim the calendar ledger row and an hourly
//	         cap slot in one transaction (the cap is the send ledger's: one
//	         counter per account), consume the approval, and, only when
//	         calendar.invite.live is true, re-read the event, reject the approval
//	         as stale if its version changed or its recipient set differs, and
//	         write with the version as the precondition.
//
// The ledger row moves claimed, dispatching, then applied, failed, unknown or
// would_apply, each step a conditional write on the previous state. A create
// carries a provider dedupe key (the request id) written to the ledger before
// the call, so an unknown outcome is settled by looking the event up by that
// key. An update or cancel records the event id and the pre-call version, and an
// unknown outcome is settled by comparing the event now with what was proposed;
// when that is inconclusive the row stays unknown and nothing is ever replayed.
package inviteflow

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/change-fabric/moose/internal/accounts/envelope"
	"github.com/change-fabric/moose/internal/accounts/fanout"
	"github.com/change-fabric/moose/internal/accounts/scopes"
	"github.com/change-fabric/moose/internal/accounts/store"
	"github.com/change-fabric/moose/internal/approver/api"
	"github.com/change-fabric/moose/internal/calendar/provider"
)

// Approver is the part of the approver's service API the engine uses. The HMAC
// client in internal/approver/api implements it.
type Approver interface {
	Create(ctx context.Context, in api.CreateBody) (resp api.CreateResponse, created bool, err error)
	Status(ctx context.Context, id string) (api.StatusResponse, error)
	Consume(ctx context.Context, id, claimant string) (api.StatusResponse, error)
}

// Providers builds the calendar provider for a stored account.
type Providers interface {
	CalendarProvider(ctx context.Context, a store.Account) (provider.Provider, error)
}

// Defaults for the timing knobs on Service.
const (
	DefaultDispatchTimeout = 20 * time.Second
	DefaultStaleAfter      = 90 * time.Second
	DefaultReconcileGap    = 2 * time.Minute
	maxRequestAttempts     = 5
	toolInviteStatus       = "calendar_invite_status"
	toolInviteRequest      = "calendar_invite_request"
)

// Service is the invite engine. Its zero fields take defaults except Store,
// Seal, Approver and Providers, which are required.
type Service struct {
	Store     store.Store
	Seal      envelope.Sealer
	Approver  Approver
	Providers Providers
	// ReceiptKey is the pinned PEM public key receipts are verified against.
	ReceiptKey []byte
	// RPID is the relying party id every receipt must carry.
	RPID string
	// Live is calendar.invite.live. While false, a fully approved change ends in
	// would_apply and nothing reaches the provider.
	Live bool
	Now  func() time.Time
	Log  *slog.Logger
	// Sleep waits between 429 retries of the reads made while requesting; nil
	// uses a context-aware timer.
	Sleep func(ctx context.Context, d time.Duration) error
	// DispatchTimeout bounds the provider call. A ledger row left in
	// dispatching for StaleAfter is judged to belong to a crashed call and
	// becomes unknown.
	DispatchTimeout time.Duration
	StaleAfter      time.Duration
	// ReconcileGap is how long a create must have been unknown before "no event
	// carries the dedupe key" is taken to mean it never happened (default 2
	// minutes): a provider may still be completing a call it accepted.
	ReconcileGap time.Duration
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

func (s *Service) dispatchTimeout() time.Duration {
	if s.DispatchTimeout > 0 {
		return s.DispatchTimeout
	}
	return DefaultDispatchTimeout
}

func (s *Service) staleAfter() time.Duration {
	if s.StaleAfter > 0 {
		return s.StaleAfter
	}
	return DefaultStaleAfter
}

func (s *Service) reconcileGap() time.Duration {
	if s.ReconcileGap > 0 {
		return s.ReconcileGap
	}
	return DefaultReconcileGap
}

func (s *Service) logf(msg string, args ...any) {
	if s.Log != nil {
		s.Log.Warn("inviteflow: "+msg, args...)
	}
}

func (s *Service) retry429(ctx context.Context, fn func() error) error {
	sleep := s.Sleep
	if sleep == nil {
		sleep = func(ctx context.Context, d time.Duration) error {
			t := time.NewTimer(d)
			defer t.Stop()
			select {
			case <-t.C:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}
	return fanout.Retry429(ctx, sleep, fn)
}

// Error is a refusal with a stable code and a message written for the caller.
type Error struct {
	Code string
	Msg  string
}

func (e *Error) Error() string { return e.Code + ": " + e.Msg }

func errf(code, format string, a ...any) *Error {
	return &Error{Code: code, Msg: fmt.Sprintf(format, a...)}
}

// Error codes.
const (
	CodeInvalid          = "invalid_input"
	CodeInvitesNotOn     = "invites_not_enabled"
	CodeAccountNotReady  = "account_unavailable"
	CodeNotGranted       = "calendar_not_granted"
	CodeKillSwitch       = "kill_switch"
	CodeRateLimited      = "rate_limited"
	CodeOrganizerOnly    = "organizer_only"
	CodeNoInviteNeeded   = "no_invite_needed"
	CodeEventChanged     = "event_changed"
	CodeApprover         = "approver_unavailable"
	CodeNotFound         = "not_found"
	CodeInternal         = "internal"
	CodeReceipt          = "receipt_invalid"
	CodeProvider         = "provider_error"
	CodeUnsupportedHere  = provider.CodeUnsupportedHere
	CodeInvalidRecurring = "invalid_recurrence"
)

// snapshot is the exact provider input an approved request executes. It is
// sealed with the account's envelope and its hash is in the signed payload, so
// only what was shown for approval can be written.
type snapshot struct {
	Operation  string               `json:"operation"`
	CalendarID string               `json:"calendar_id"`
	EventID    string               `json:"event_id,omitempty"`
	Scope      provider.Scope       `json:"scope,omitempty"`
	Version    string               `json:"version,omitempty"`
	Create     *provider.EventInput `json:"create,omitempty"`
	Patch      *provider.EventPatch `json:"patch,omitempty"`
}

// RequestIn is what RequestInvite needs. For a create give CalendarID and
// Event; for an update give Ref, Scope and Patch; for a cancel give Ref and
// Scope. Ref is the event as the caller named it (an occurrence's own id for
// scope instance).
type RequestIn struct {
	Account    store.Account
	Operation  string
	CalendarID string
	Event      *provider.EventInput
	Ref        provider.EventRef
	Scope      provider.Scope
	Patch      *provider.EventPatch
}

// RequestOut is the answer to RequestInvite.
type RequestOut struct {
	// Status is pending (waiting for the passkey), or the earlier request's state
	// when this exact change was requested before.
	Status      string             `json:"status"`
	RequestID   string             `json:"request_id"`
	Operation   string             `json:"operation"`
	ApprovalURL string             `json:"approval_url,omitempty"`
	ExpiresAt   *time.Time         `json:"expires_at,omitempty"`
	Recipients  []api.CalRecipient `json:"recipients"`
	Proposed    *api.CalEvent      `json:"proposed,omitempty"`
	Original    *api.CalEvent      `json:"original,omitempty"`
	Target      api.CalTarget      `json:"target"`
}

// Granted reports whether the account holds every permission calendar needs.
func Granted(a store.Account) bool {
	g, ok := a.Services[store.ServiceCalendar]
	if !ok || len(g.Scopes) == 0 {
		return false
	}
	return len(scopes.Missing(a.Provider, store.ServiceCalendar, a.ProviderUID, g.Scopes)) == 0
}

// Blocked refuses work for an account that is not ready: status ok, invites
// enabled, calendar granted, and neither the global nor the provider's kill
// switch on. It is used when a request is made and again at dispatch.
func (s *Service) Blocked(ctx context.Context, a store.Account) *Error {
	switch {
	case a.Status != store.StatusOK:
		return errf(CodeAccountNotReady, "%s is %s; it cannot invite anyone until it is reconnected", a.Address, strings.ReplaceAll(a.Status, "_", " "))
	case !Granted(a):
		return errf(CodeNotGranted, "%s has not granted calendar access", a.Address)
	case !a.InviteEnabled:
		return errf(CodeInvitesNotOn, "inviting others is not enabled for %s; call calendar_account_enable_invites and approve it with your passkey first", a.Address)
	}
	ks, err := s.Store.KillSwitch(ctx)
	if err != nil {
		return errf(CodeInternal, "could not read the kill switch")
	}
	if ks.On {
		msg := "the kill switch is on, so nothing is sent or invited"
		if ks.Reason != "" {
			msg += " (" + ks.Reason + ")"
		}
		return errf(CodeKillSwitch, "%s", msg)
	}
	prov := a.Provider
	if prov == "" {
		prov = store.ProviderGoogle
	}
	pks, err := s.Store.ProviderKillSwitch(ctx, prov)
	if err != nil {
		return errf(CodeInternal, "could not read the kill switch")
	}
	if pks.On {
		msg := "the " + prov + " kill switch is on, so nothing is sent or invited from " + prov + " accounts"
		if pks.Reason != "" {
			msg += " (" + pks.Reason + ")"
		}
		return errf(CodeKillSwitch, "%s", msg)
	}
	return nil
}

func isOrganizer(a store.Account, ev provider.Event) bool {
	org := strings.TrimSpace(ev.Organizer.Email)
	return ev.Organizer.Self || org == "" || strings.EqualFold(org, a.Address)
}

func isOccurrence(ev provider.Event) bool { return ev.SeriesID != "" && ev.ID != ev.SeriesID }
func isMaster(ev provider.Event) bool     { return ev.SeriesID != "" && ev.ID == ev.SeriesID }

// aimed is the event a write is aimed at: its ref, the version the write
// presents, and the event as it stands. A series written through one of its
// occurrences is aimed at the master with the master's version.
type aimed struct {
	ref     provider.EventRef
	version string
	cur     provider.Event
}

func (s *Service) aim(ctx context.Context, p provider.Provider, ref provider.EventRef, scope provider.Scope) (aimed, error) {
	var cur provider.Event
	if err := s.retry429(ctx, func() (e error) { cur, e = p.GetEvent(ctx, ref); return }); err != nil {
		return aimed{}, err
	}
	if cur.CalendarID == "" {
		cur.CalendarID = ref.CalendarID
	}
	out := aimed{ref: ref, version: cur.Version, cur: cur}
	if scope == provider.ScopeSeries && isOccurrence(cur) {
		mref := provider.EventRef{CalendarID: ref.CalendarID, EventID: cur.SeriesID}
		var m provider.Event
		if err := s.retry429(ctx, func() (e error) { m, e = p.GetEvent(ctx, mref); return }); err != nil {
			return aimed{}, err
		}
		if m.CalendarID == "" {
			m.CalendarID = ref.CalendarID
		}
		out = aimed{ref: mref, version: m.Version, cur: m}
	}
	return out, nil
}

func (s *Service) providerErr(a store.Account, err error) *Error {
	switch {
	case errors.Is(err, provider.ErrNotFound):
		return errf(CodeNotFound, "the provider has no such calendar or event in %s", a.Address)
	case errors.Is(err, provider.ErrAuth):
		return errf("needs_reauth", "%s needs to be reconnected or its calendar access granted again", a.Address)
	case errors.Is(err, provider.ErrUnsupportedHere):
		return errf(CodeUnsupportedHere, "this account's provider or account type cannot do that")
	case errors.Is(err, provider.ErrInvalidRecurrence):
		return errf(CodeInvalidRecurring, "the recurrence is outside what moose can write")
	case errors.Is(err, provider.ErrRateLimited):
		return errf(CodeRateLimited, "the provider is limiting requests for this account; try again shortly")
	}
	return errf(CodeProvider, "the event could not be read from the calendar")
}

// RequestInvite creates the approver request for one create, update or cancel.
func (s *Service) RequestInvite(ctx context.Context, in RequestIn) (RequestOut, error) {
	a := in.Account
	if e := s.Blocked(ctx, a); e != nil {
		return RequestOut{}, e
	}
	if n, err := s.Store.HourlyCount(ctx, a.ID, s.now()); err != nil {
		return RequestOut{}, errf(CodeInternal, "could not read the hourly counter")
	} else if n >= a.HourlyCap {
		return RequestOut{}, errf(CodeRateLimited, "%s has reached its hourly cap of %d sends and invites", a.Address, a.HourlyCap)
	}
	p, err := s.Providers.CalendarProvider(ctx, a)
	if err != nil {
		return RequestOut{}, errf(CodeProvider, "could not open the calendar for %s", a.Address)
	}

	payload := api.CalendarInvitePayload{
		Owner: a.Owner, AccountID: a.ID, FromAddress: a.Address, Operation: in.Operation, OrganizerRole: api.CalRoleOrganizer,
	}
	snap := snapshot{Operation: in.Operation}
	switch in.Operation {
	case api.CalOpCreate:
		if in.Event == nil || in.CalendarID == "" {
			return RequestOut{}, errf(CodeInvalid, "a create needs a calendar and an event")
		}
		snap.CalendarID, snap.Create = in.CalendarID, in.Event
		proposed := eventOfInput(a, *in.Event)
		payload.Target = api.CalTarget{CalendarID: in.CalendarID}
		payload.Proposed = &proposed
	case api.CalOpUpdate, api.CalOpCancel:
		if in.Operation == api.CalOpUpdate && in.Patch == nil {
			return RequestOut{}, errf(CodeInvalid, "an update needs a change")
		}
		if !in.Scope.Valid() {
			return RequestOut{}, errf(CodeInvalid, "scope must be instance or series")
		}
		am, err := s.aim(ctx, p, in.Ref, in.Scope)
		if err != nil {
			return RequestOut{}, s.providerErr(a, err)
		}
		if in.Scope == provider.ScopeInstance && isMaster(am.cur) {
			return RequestOut{}, errf(CodeInvalid, "that id is a repeating series; use scope series to change the whole series, or an occurrence's id for one occurrence")
		}
		if !isOrganizer(a, am.cur) {
			return RequestOut{}, errf(CodeOrganizerOnly, "%s is only invited to this event; only the organizer (%s) can change or cancel it. To decline use calendar_respond_to_invite", a.Address, am.cur.Organizer.Email)
		}
		original := eventOf(a, am.cur)
		payload.Original = &original
		payload.Version = am.version
		payload.Target = api.CalTarget{CalendarID: am.ref.CalendarID, EventID: am.ref.EventID, Scope: string(in.Scope)}
		if in.Scope == provider.ScopeInstance && am.cur.OriginalStart != nil {
			ot := calTime(*am.cur.OriginalStart)
			payload.Target.OriginalStart = &ot
		}
		snap.CalendarID, snap.EventID, snap.Scope, snap.Version = am.ref.CalendarID, am.ref.EventID, in.Scope, am.version
		if in.Operation == api.CalOpUpdate {
			snap.Patch = in.Patch
			proposed := patched(a, original, *in.Patch)
			payload.Proposed = &proposed
		}
	default:
		return RequestOut{}, errf(CodeInvalid, "operation must be create, update or cancel")
	}
	payload.Recipients = api.RecipientsFor(a.Address, in.Operation, payload.Original, payload.Proposed)
	if len(payload.Recipients) == 0 {
		return RequestOut{}, errf(CodeNoInviteNeeded, "this change involves nobody but %s, so it needs no invite approval", a.Address)
	}
	rawSnap, err := json.Marshal(snap)
	if err != nil {
		return RequestOut{}, errf(CodeInternal, "could not encode the change")
	}
	sum := sha256.Sum256(rawSnap)
	snapSHA := hex.EncodeToString(sum[:])
	payload.SnapshotSHA256 = snapSHA
	raw, err := json.Marshal(payload)
	if err != nil {
		return RequestOut{}, errf(CodeInternal, "could not encode the request")
	}
	canon, err := api.Canonical(raw)
	if err != nil {
		return RequestOut{}, errf(CodeInternal, "could not encode the request")
	}
	// The approver parses the payload strictly; moose checks it the same way so
	// a malformed request fails here with a reason, not at the approver.
	if _, err := api.ParsePayload(api.KindCalendarInvite, canon); err != nil {
		return RequestOut{}, errf(CodeInvalid, "this change cannot be approved: %v", err)
	}
	wantSHA := api.SHA256(canon)
	preview, err := s.preview(ctx, a, previewAction(in.Operation))
	if err != nil {
		return RequestOut{}, err
	}
	rawPreview, _ := json.Marshal(preview)

	out := RequestOut{Operation: in.Operation, Recipients: payload.Recipients, Proposed: payload.Proposed, Original: payload.Original, Target: payload.Target}
	for attempt := 0; attempt < maxRequestAttempts; attempt++ {
		key := idemKey(a.ID, in.Operation, payload.Target, snapSHA, attempt)
		if prior, err := s.Store.FindApprovalByKey(ctx, key); err == nil {
			state, dead := s.earlier(ctx, prior)
			if dead {
				continue
			}
			out.RequestID, out.Status = prior.ID, state
			if state == "pending" || state == "approved" {
				out.ApprovalURL = prior.ApprovalURL
				exp := prior.Expires
				out.ExpiresAt = &exp
			}
			return out, nil
		} else if !errors.Is(err, store.ErrNotFound) {
			return RequestOut{}, errf(CodeInternal, "could not look up earlier requests")
		}
		resp, created, err := s.Approver.Create(ctx, api.CreateBody{IdempotencyKey: key, Kind: api.KindCalendarInvite, Payload: canon, Preview: rawPreview})
		var ae *api.Error
		if errors.As(err, &ae) && ae.Code == "idempotency_conflict" {
			continue
		}
		if err != nil {
			s.logf("create request", "err", err.Error())
			return RequestOut{}, errf(CodeApprover, "the approval service could not create the request: %s", describe(err))
		}
		if resp.PayloadSHA256 != wantSHA {
			return RequestOut{}, errf(CodeInternal, "the approver hashed a different payload than moose sent")
		}
		sealed, err := s.Seal.Seal(ctx, rawSnap, envelope.Context{Owner: a.Owner, AccountID: a.ID})
		if err != nil {
			return RequestOut{}, errf(CodeInternal, "could not seal the change snapshot")
		}
		_, _, err = s.Store.CreateApproval(ctx, store.Approval{
			ID: resp.RequestID, Owner: a.Owner, Kind: store.KindCalendarInvite, AccountID: a.ID, IdempotencyKey: key,
			PayloadSHA256: wantSHA, Payload: canon, ApprovalURL: resp.ApprovalURL, Snapshot: sealed,
			Status: store.ApprovalPending, Created: s.now(), Expires: resp.ExpiresAt,
		}, s.now())
		if err != nil && !errors.Is(err, store.ErrConflict) {
			return RequestOut{}, errf(CodeInternal, "could not record the request")
		}
		if created {
			s.audit(ctx, a, toolInviteRequest, resp.RequestID, recipientAddrs(payload.Recipients), snapSHA, in.Operation+" requested")
		}
		out.RequestID, out.Status, out.ApprovalURL = resp.RequestID, "pending", resp.ApprovalURL
		exp := resp.ExpiresAt
		out.ExpiresAt = &exp
		return out, nil
	}
	return RequestOut{}, errf(CodeInternal, "too many earlier requests for this change were denied or expired; change it and try again")
}

func previewAction(op string) string {
	switch op {
	case api.CalOpCreate:
		return "Invite people to an event"
	case api.CalOpUpdate:
		return "Change an event with attendees"
	}
	return "Cancel an event with attendees"
}

func recipientAddrs(rs []api.CalRecipient) []string {
	out := make([]string, 0, len(rs))
	for _, r := range rs {
		out = append(out, r.Email)
	}
	return out
}

func idemKey(accountID, op string, t api.CalTarget, snapSHA string, attempt int) string {
	s := strings.Join([]string{"calendar-invite", accountID, op, t.CalendarID, t.EventID, t.Scope, snapSHA}, "\x00")
	if attempt > 0 {
		s += fmt.Sprintf("\x00attempt-%d", attempt)
	}
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

func describe(err error) string {
	var ae *api.Error
	if errors.As(err, &ae) && ae.Message != "" {
		return ae.Message
	}
	return "it did not answer"
}

// earlier classifies a request moose already holds for this key: its current
// state, and whether it is dead (denied or expired) so a new request is needed.
func (s *Service) earlier(ctx context.Context, prior store.Approval) (state string, dead bool) {
	if row, err := s.Store.GetCalendarRequest(ctx, prior.ID); err == nil {
		return row.State, false
	}
	switch prior.Status {
	case store.ApprovalDenied, store.ApprovalExpired:
		return prior.Status, true
	}
	st, err := s.Approver.Status(ctx, prior.ID)
	if err != nil {
		return "pending", false
	}
	switch st.Status {
	case "denied", "expired":
		return st.Status, true
	case "pending":
		if !s.now().Before(prior.Expires) {
			return "expired", true
		}
	}
	return st.Status, false
}

// preview builds the banner the approver shows: the account, its colour, the
// other accounts for the look-alike warning. The event itself is never in the
// preview; the page renders it from the signed payload.
func (s *Service) preview(ctx context.Context, a store.Account, action string) (api.Preview, error) {
	nick := a.Nickname
	if strings.TrimSpace(nick) == "" {
		nick = a.Address
	}
	inv, cal := a.InviteEnabled, Granted(a)
	pv := api.Preview{
		Action: action, AccountID: a.ID, Nickname: nick, FromAddress: a.Address, DisplayName: a.DisplayName,
		ColorIndex: a.ColorIndex, AccountStatus: a.Status, InviteEnabled: &inv, CalendarGranted: &cal,
	}
	accts, err := s.Store.ListAccounts(ctx, a.Owner)
	if err != nil {
		return pv, errf(CodeInternal, "could not list accounts")
	}
	for _, o := range accts {
		if o.ID == a.ID {
			continue
		}
		on := o.Nickname
		if strings.TrimSpace(on) == "" {
			on = o.Address
		}
		pv.Siblings = append(pv.Siblings, api.Sibling{AccountID: o.ID, Nickname: on, Address: o.Address, DisplayName: o.DisplayName})
	}
	return pv, nil
}

func (s *Service) audit(ctx context.Context, a store.Account, tool, requestID string, recips []string, bodySHA, detail string) {
	err := s.Store.AppendAudit(ctx, store.AuditEvent{
		At: s.now(), Owner: a.Owner, RequestID: requestID, AccountID: a.ID, Tool: tool,
		Recipients: recips, BodySHA256: bodySHA, ApprovalID: requestID, Detail: detail,
	})
	if err != nil {
		s.logf("audit", "err", err.Error())
	}
}
