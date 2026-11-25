package sendflow

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/change-fabric/moose/internal/approver/api"
	"github.com/change-fabric/moose/internal/approver/receipt"
	"github.com/change-fabric/moose/internal/email/store"
)

// Contact request statuses, besides the approver's own (denied, expired).
const (
	// ContactsNone means there was nothing left to ask the owner about.
	ContactsNone = "none"
	// ContactsApplied means the approved changes are in force.
	ContactsApplied = "applied"
)

// Tool names recorded in the audit trail.
const (
	toolContactsPropose = "contacts_propose"
	toolContactsStatus  = "contacts_status"
)

// TrustProposal is the outcome of ProposeTrust.
type TrustProposal struct {
	// Status is pending (waiting for the passkey), approved (the owner has
	// approved; contacts_status applies it), none (nothing to ask), or the
	// state of an earlier identical request.
	Status      string
	RequestID   string
	ApprovalURL string
	ExpiresAt   *time.Time
	// Addresses are the addresses the request asks to trust, sorted.
	Addresses []string
	// AlreadyTrusted and Blocked are requested addresses left out of the
	// request: they are trusted already, or blocked (a block is never lifted
	// by a trust request).
	AlreadyTrusted []string
	Blocked        []string
}

// ProposeTrust collects trust promotions for account a into one
// contact-trust-batch approver request. The idempotency key is the hash of the
// account and the sorted addresses, so asking again for the same set returns
// the request that is still alive.
func (s *Service) ProposeTrust(ctx context.Context, a store.Account, addresses []string) (TrustProposal, error) {
	var out TrustProposal
	seen := map[string]bool{}
	for _, addr := range addresses {
		addr = strings.ToLower(strings.TrimSpace(addr))
		if addr == "" || seen[addr] {
			continue
		}
		seen[addr] = true
		c, err := s.Store.GetContact(ctx, a.ID, addr)
		switch {
		case errors.Is(err, store.ErrNotFound):
		case err != nil:
			return TrustProposal{}, errf(CodeInternal, "could not read the contact list")
		case c.Trust == store.TrustBlocked:
			out.Blocked = append(out.Blocked, addr)
			continue
		case c.Trust == store.TrustTrusted:
			out.AlreadyTrusted = append(out.AlreadyTrusted, addr)
			continue
		}
		out.Addresses = append(out.Addresses, addr)
	}
	sort.Strings(out.Addresses)
	sort.Strings(out.Blocked)
	sort.Strings(out.AlreadyTrusted)
	if len(out.Addresses) == 0 {
		out.Status = ContactsNone
		return out, nil
	}
	if len(out.Addresses) > store.MaxContactBatch {
		return TrustProposal{}, errf(CodeInvalid, "a trust request holds at most %d addresses; split it", store.MaxContactBatch)
	}
	payload := api.ContactTrustPayload{Owner: a.Owner, AccountID: a.ID, Changes: make([]api.TrustChange, len(out.Addresses))}
	for i, addr := range out.Addresses {
		payload.Changes[i] = api.TrustChange{Address: addr, To: "trusted"}
	}
	canon, err := canonicalOf(payload)
	if err != nil {
		return TrustProposal{}, errf(CodeInternal, "could not encode the request")
	}
	wantSHA := api.SHA256(canon)
	pv, err := s.accountPreview(ctx, a, "Trust these contacts")
	if err != nil {
		return TrustProposal{}, err
	}
	rawPreview, _ := json.Marshal(pv)

	for attempt := 0; attempt < maxRequestAttempts; attempt++ {
		key := trustKey(a, out.Addresses, attempt)
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
			return TrustProposal{}, errf(CodeInternal, "could not look up earlier requests")
		}
		resp, created, err := s.Approver.Create(ctx, api.CreateBody{IdempotencyKey: key, Kind: api.KindContactTrustBatch, Payload: canon, Preview: rawPreview})
		var ae *api.Error
		if errors.As(err, &ae) && ae.Code == "idempotency_conflict" {
			continue // the approver holds this key for a request moose never recorded
		}
		if err != nil {
			s.logf("create trust request", "err", err.Error())
			return TrustProposal{}, errf(CodeApprover, "the approval service could not create the request: %s", describe(err))
		}
		if resp.PayloadSHA256 != wantSHA {
			return TrustProposal{}, errf(CodeInternal, "the approver hashed a different payload than moose sent")
		}
		_, _, err = s.Store.CreateApproval(ctx, store.Approval{
			ID: resp.RequestID, Owner: a.Owner, Kind: store.KindContactTrustBatch, AccountID: a.ID, IdempotencyKey: key,
			PayloadSHA256: wantSHA, Payload: canon, ApprovalURL: resp.ApprovalURL, Status: store.ApprovalPending,
			Created: s.now(), Expires: resp.ExpiresAt,
		}, s.now())
		if err != nil && !errors.Is(err, store.ErrConflict) {
			return TrustProposal{}, errf(CodeInternal, "could not record the request")
		}
		if created {
			s.audit(ctx, a, toolContactsPropose, resp.RequestID, out.Addresses, "", "requested")
		}
		out.RequestID, out.Status, out.ApprovalURL, out.ExpiresAt = resp.RequestID, StatusPending, resp.ApprovalURL, &resp.ExpiresAt
		return out, nil
	}
	return TrustProposal{}, errf(CodeInternal, "too many earlier requests for these addresses were denied or expired; try again later")
}

func trustKey(a store.Account, sorted []string, attempt int) string {
	s := "contact-trust-batch\x00" + a.Owner + "\x00" + a.ID + "\x00" + strings.Join(sorted, "\x00")
	if attempt > 0 {
		s += fmt.Sprintf("\x00attempt-%d", attempt)
	}
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

// ContactsStatusOut is the answer to ContactsStatus.
type ContactsStatusOut struct {
	// Status is pending, applied, denied or expired.
	Status      string     `json:"status"`
	RequestID   string     `json:"request_id"`
	AccountID   string     `json:"account_id"`
	Address     string     `json:"address"`
	ApprovalURL string     `json:"approval_url,omitempty"`
	ExpiresAt   *time.Time `json:"expires_at,omitempty"`
	// Requested is every address the request asks to trust.
	Requested []string `json:"requested"`
	// Applied lists the addresses now trusted. Skipped lists approved
	// addresses left alone because they are blocked.
	Applied []string `json:"applied,omitempty"`
	Skipped []string `json:"skipped,omitempty"`
	Note    string   `json:"note,omitempty"`
}

// ContactsStatus reports a contact-trust-batch request and, once the owner has
// approved it, applies it: read the local ledger, ask the approver, verify the
// receipt against the stored request, claim and apply every change in one
// transaction, then consume (idempotent). A batch that is applied stays
// applied, and asking again returns the ledger row.
func (s *Service) ContactsStatus(ctx context.Context, owner, requestID string) (ContactsStatusOut, error) {
	ap, err := s.Store.GetApproval(ctx, requestID)
	if err != nil || ap.Owner != owner || ap.Kind != store.KindContactTrustBatch {
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return ContactsStatusOut{}, errf(CodeInternal, "could not read the request")
		}
		return ContactsStatusOut{}, errf(CodeNotFound, "no contact trust request %q; use the request_id contacts_propose returned", clipStr(requestID, 60))
	}
	a, err := s.Store.GetAccount(ctx, owner, ap.AccountID)
	if err != nil {
		return ContactsStatusOut{}, errf(CodeInternal, "could not read the account")
	}
	trust, err := s.storedTrust(ap, a)
	if err != nil {
		s.logf("contact request payload", "request", ap.ID, "err", err.Error())
		return ContactsStatusOut{}, errf(CodeReceipt, "the stored request does not match its hash; nothing was changed")
	}
	out := ContactsStatusOut{RequestID: ap.ID, AccountID: a.ID, Address: a.Address}
	for _, c := range trust.Changes {
		out.Requested = append(out.Requested, c.Address)
	}

	// 1. The local ledger: an applied batch is final.
	switch row, err := s.Store.GetContactBatch(ctx, ap.ID); {
	case err == nil:
		return s.finishContacts(ctx, ap, a, out, row), nil
	case !errors.Is(err, store.ErrNotFound):
		return ContactsStatusOut{}, errf(CodeInternal, "could not read the contact ledger")
	}
	if ap.Status == store.ApprovalDenied || ap.Status == store.ApprovalExpired {
		out.Status = ap.Status
		out.Note = "This request is closed; nothing was changed. Propose the contacts again to ask once more."
		return out, nil
	}

	// 2. The approver's word.
	st, err := s.Approver.Status(ctx, ap.ID)
	if err != nil {
		return ContactsStatusOut{}, errf(CodeApprover, "the approval service could not be reached; nothing was changed")
	}
	switch st.Status {
	case "pending":
		out.Status, out.ApprovalURL = StatusPending, ap.ApprovalURL
		exp := ap.Expires
		out.ExpiresAt = &exp
		if !s.now().Before(ap.Expires) {
			out.Status, out.ApprovalURL, out.ExpiresAt = "expired", "", nil
			s.closeRequest(ctx, ap, "expired")
		}
		out.Note = noteFor(out.Status)
		return out, nil
	case "denied", "expired":
		s.closeRequest(ctx, ap, st.Status)
		out.Status = st.Status
		out.Note = noteFor(st.Status)
		return out, nil
	case "approved":
	default:
		// consumed, and moose holds no ledger row: another claimant used it.
		return ContactsStatusOut{}, errf(CodeReceipt, "the approval is %s but moose never applied it; it will not be applied now", st.Status)
	}

	// 3. Verify the receipt against the stored request.
	if st.RequestID != ap.ID || st.Receipt == nil {
		return ContactsStatusOut{}, errf(CodeReceipt, "the approved request carries no matching receipt; nothing was changed")
	}
	if err := receipt.Verify(s.ReceiptKey, *st.Receipt, receipt.Want{
		RequestID: ap.ID, Kind: api.KindContactTrustBatch, PayloadSHA256: ap.PayloadSHA256, RPID: s.RPID, Now: s.now(),
	}); err != nil {
		s.logf("contact receipt rejected", "request", ap.ID, "err", err.Error())
		return ContactsStatusOut{}, errf(CodeReceipt, "the approval's receipt did not verify against the stored request; nothing was changed")
	}

	// 4. Claim and apply in one transaction.
	row, err := s.Store.ApplyContactBatch(ctx, store.ContactBatchRow{RequestID: ap.ID, Owner: owner, AccountID: a.ID, Created: s.now()}, out.Requested)
	if errors.Is(err, store.ErrConflict) {
		// Another call claimed it first; its row is the answer.
		if row, err = s.Store.GetContactBatch(ctx, ap.ID); err == nil {
			return s.finishContacts(ctx, ap, a, out, row), nil
		}
	}
	if err != nil {
		s.logf("apply contact batch", "request", ap.ID, "err", err.Error())
		return ContactsStatusOut{}, errf(CodeInternal, "could not apply the changes; none were applied, call again to retry")
	}
	s.audit(ctx, a, toolContactsStatus, ap.ID, row.Applied, "", "applied")
	return s.finishContacts(ctx, ap, a, out, row), nil
}

func noteFor(status string) string {
	if status == "expired" {
		return "This request expired before it was approved; nothing was changed."
	}
	if status == "denied" {
		return "The owner did not approve this request; nothing was changed."
	}
	return "Waiting for the owner's passkey approval. Give them the approval_url, then call contacts_status again."
}

// finishContacts reports an applied batch and completes its bookkeeping: the
// approval is consumed (the same claimant may consume again, so a call that
// died after the apply resumes here) and marked consumed locally.
func (s *Service) finishContacts(ctx context.Context, ap store.Approval, a store.Account, out ContactsStatusOut, row store.ContactBatchRow) ContactsStatusOut {
	out.Status, out.Applied, out.Skipped = ContactsApplied, row.Applied, row.Skipped
	out.Note = "The approved changes are applied."
	if ap.Status != store.ApprovalConsumed {
		if _, err := s.Approver.Consume(ctx, ap.ID, ap.ID); err != nil {
			if !errors.Is(err, api.ErrAlreadyConsumed) {
				s.logf("consume contact batch", "request", ap.ID, "err", err.Error())
				out.Note += " The approval service could not be reached to consume the approval; call again to finish."
				return out
			}
			// The changes were approved and are in force; log the oddity.
			s.logf("contact batch already consumed by another claimant", "request", ap.ID)
		}
		if _, err := s.Store.TransitionApproval(ctx, ap.ID, []string{store.ApprovalPending, store.ApprovalApproved}, store.ApprovalConsumed, false); err != nil && !errors.Is(err, store.ErrInvalidTransition) {
			s.logf("mark contact batch consumed", "request", ap.ID, "err", err.Error())
		}
	}
	if len(row.Skipped) > 0 {
		out.Note += " Blocked addresses were skipped: a block always wins."
	}
	return out
}

// storedTrust checks the stored payload against its stored hash and against
// the account it was made for, and returns it.
func (s *Service) storedTrust(ap store.Approval, a store.Account) (*api.ContactTrustPayload, error) {
	canon, err := api.Canonical(ap.Payload)
	if err != nil || api.SHA256(canon) != ap.PayloadSHA256 {
		return nil, errors.New("the stored payload does not match its stored hash")
	}
	p, err := api.ParsePayload(api.KindContactTrustBatch, canon)
	if err != nil || p.Trust == nil {
		return nil, errors.New("the stored payload is not a contact-trust-batch payload")
	}
	if p.Trust.Owner != a.Owner || p.Trust.AccountID != a.ID {
		return nil, errors.New("the stored request is for another account")
	}
	if len(p.Trust.Changes) > store.MaxContactBatch {
		return nil, errors.New("the stored request holds too many addresses")
	}
	return p.Trust, nil
}

func clipStr(s string, n int) string {
	if len(s) > n {
		return s[:n] + "..."
	}
	return s
}
