package inviteflow

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/change-fabric/moose/internal/accounts/store"
	"github.com/change-fabric/moose/internal/approver/api"
	"github.com/change-fabric/moose/internal/approver/receipt"
)

// enableWindow is how long one account-invite-enable request is reused for the
// same account: it matches the approver's 15 minute request lifetime, so a
// repeated call shares one request and a call after expiry gets a fresh one.
const enableWindow = 15 * time.Minute

// EnableResult is the outcome of an enable request or its application.
type EnableResult struct {
	// Status is pending (waiting for the passkey), or already_enabled.
	Status      string
	RequestID   string
	ApprovalURL string
	ExpiresAt   time.Time
}

// FromMatches reports whether from is exactly the account's address. Case is
// ignored; a nickname or display name never matches.
func FromMatches(a store.Account, from string) bool {
	return strings.TrimSpace(from) != "" && strings.EqualFold(strings.TrimSpace(from), a.Address)
}

// RequestEnable asks for the passkey approval that turns invites on for a.
// Repeating the call inside its window returns the same request.
func (s *Service) RequestEnable(ctx context.Context, a store.Account, from string) (EnableResult, error) {
	if !FromMatches(a, from) {
		return EnableResult{}, errf(CodeInvalid, "from_address %q is not the address of account %s (%s); restate the account's exact address", from, a.ID, a.Address)
	}
	if a.InviteEnabled {
		return EnableResult{Status: "already_enabled"}, nil
	}
	if a.Status != store.StatusOK {
		return EnableResult{}, errf(CodeAccountNotReady, "%s is %s; reconnect it before enabling invites", a.Address, a.Status)
	}
	if !Granted(a) {
		return EnableResult{}, errf(CodeNotGranted, "%s has not granted calendar access; the owner grants it on the moose accounts page first", a.Address)
	}
	raw, err := json.Marshal(api.InviteEnablePayload{Owner: a.Owner, AccountID: a.ID, FromAddress: a.Address})
	if err != nil {
		return EnableResult{}, errf(CodeInternal, "could not encode the request")
	}
	canon, err := api.Canonical(raw)
	if err != nil {
		return EnableResult{}, errf(CodeInternal, "could not encode the request")
	}
	wantSHA := api.SHA256(canon)
	window := s.now().Unix() / int64(enableWindow/time.Second)
	kh := sha256.Sum256([]byte(fmt.Sprintf("%s|%s|%s|%s|%d", store.KindAccountInviteEnable, a.Owner, a.ID, a.Address, window)))
	key := hex.EncodeToString(kh[:])
	pv, err := s.preview(ctx, a, "Enable invitations")
	if err != nil {
		return EnableResult{}, err
	}
	pv.InviteEnabled = nil // the switch is what is being asked for
	rawPV, _ := json.Marshal(pv)
	resp, _, err := s.Approver.Create(ctx, api.CreateBody{IdempotencyKey: key, Kind: api.KindAccountInviteEnable, Payload: canon, Preview: rawPV})
	if err != nil {
		s.logf("request invite enable", "err", err.Error())
		return EnableResult{}, errf(CodeApprover, "the approval service could not create the request: %s", describe(err))
	}
	if resp.PayloadSHA256 != wantSHA {
		return EnableResult{}, errf(CodeInternal, "the approver hashed a different payload than moose sent")
	}
	_, _, err = s.Store.CreateApproval(ctx, store.Approval{
		ID: resp.RequestID, Owner: a.Owner, Kind: store.KindAccountInviteEnable, AccountID: a.ID, IdempotencyKey: key,
		PayloadSHA256: wantSHA, Payload: canon, ApprovalURL: resp.ApprovalURL, Status: store.ApprovalPending,
		Created: s.now(), Expires: resp.ExpiresAt,
	}, s.now())
	if err != nil && !errors.Is(err, store.ErrConflict) {
		return EnableResult{}, errf(CodeInternal, "could not record the request")
	}
	return EnableResult{Status: StatusPending, RequestID: resp.RequestID, ApprovalURL: resp.ApprovalURL, ExpiresAt: resp.ExpiresAt}, nil
}

// Disable turns invites off at once; disabling needs no approval. Any enable
// request still waiting or approved but not applied is cancelled, so an old
// approval cannot turn invites back on.
func (s *Service) Disable(ctx context.Context, a store.Account) error {
	fresh, err := s.Store.GetAccount(ctx, a.Owner, a.ID)
	if err != nil {
		return errf(CodeInternal, "could not read the account")
	}
	if fresh.InviteEnabled {
		if err := s.Store.UpdateAccount(ctx, fresh.Owner, fresh.ID, store.AccountPatch{InviteEnabled: store.Ptr(false)}); err != nil {
			return errf(CodeInternal, "could not disable invites")
		}
	}
	aps, err := s.Store.FindApprovals(ctx, store.KindAccountInviteEnable, a.ID)
	if err != nil {
		return nil // the account is already off; cancelling is best effort
	}
	for _, ap := range aps {
		if ap.Status == store.ApprovalPending || ap.Status == store.ApprovalApproved {
			_, _ = s.Store.TransitionApproval(ctx, ap.ID, []string{store.ApprovalPending, store.ApprovalApproved}, store.ApprovalDenied, false)
		}
	}
	return nil
}

// ApplyEnable looks for an approved account-invite-enable request for a and, if
// its receipt verifies, turns invites on: verify, claim locally with a
// conditional write, consume (idempotent), set invite_enabled. A claim that was
// interrupted resumes. It returns the account as it now stands.
func (s *Service) ApplyEnable(ctx context.Context, a store.Account) (store.Account, bool, error) {
	if a.InviteEnabled {
		return a, false, nil
	}
	aps, err := s.Store.FindApprovals(ctx, store.KindAccountInviteEnable, a.ID)
	if err != nil {
		return a, false, errf(CodeInternal, "could not look up enable requests")
	}
	for _, ap := range aps {
		if ap.Owner != a.Owner {
			continue
		}
		switch ap.Status {
		case store.ApprovalPending:
			st, err := s.Approver.Status(ctx, ap.ID)
			if err != nil {
				return a, false, errf(CodeApprover, "the approval service could not be reached; nothing was changed")
			}
			switch st.Status {
			case "denied", "expired":
				s.closeRequest(ctx, ap, st.Status)
				continue
			case "approved":
			default:
				continue // still pending, or consumed elsewhere
			}
			if err := s.verifyEnable(ap, a, st); err != nil {
				s.logf("enable receipt rejected", "request", ap.ID, "err", err.Error())
				return a, false, errf(CodeReceipt, "the approval's receipt did not verify against the stored request; invites stay off")
			}
			if _, err := s.Store.TransitionApproval(ctx, ap.ID, []string{store.ApprovalPending}, store.ApprovalApproved, false); err != nil && !errors.Is(err, store.ErrInvalidTransition) {
				return a, false, errf(CodeInternal, "could not claim the approval")
			}
		case store.ApprovalApproved:
			// Claimed by an earlier call that did not finish: resume.
		default:
			continue
		}
		return s.finishEnable(ctx, ap, a)
	}
	return a, false, nil
}

func (s *Service) finishEnable(ctx context.Context, ap store.Approval, a store.Account) (store.Account, bool, error) {
	if _, err := s.Approver.Consume(ctx, ap.ID, ap.ID); err != nil {
		if errors.Is(err, api.ErrAlreadyConsumed) {
			_, _ = s.Store.TransitionApproval(ctx, ap.ID, []string{store.ApprovalApproved}, store.ApprovalDenied, false)
			return a, false, errf(CodeReceipt, "the approval was already consumed by another claimant; invites stay off")
		}
		return a, false, errf(CodeApprover, "the approval service could not be reached to consume the approval; call again to finish")
	}
	fresh, err := s.Store.GetAccount(ctx, a.Owner, a.ID)
	if err != nil {
		return a, false, errf(CodeInternal, "could not read the account")
	}
	if fresh.Status != store.StatusOK {
		return a, false, errf(CodeAccountNotReady, "%s is %s; reconnect it, then request the enable again", fresh.Address, fresh.Status)
	}
	if !Granted(fresh) {
		return a, false, errf(CodeNotGranted, "%s no longer holds its calendar grant; grant it again, then request the enable again", fresh.Address)
	}
	if err := s.Store.UpdateAccount(ctx, fresh.Owner, fresh.ID, store.AccountPatch{InviteEnabled: store.Ptr(true)}); err != nil {
		return a, false, errf(CodeInternal, "could not enable invites")
	}
	fresh.InviteEnabled = true
	if _, err := s.Store.TransitionApproval(ctx, ap.ID, []string{store.ApprovalApproved, store.ApprovalPending}, store.ApprovalConsumed, false); err != nil {
		s.logf("mark enable consumed", "err", err.Error())
	}
	s.audit(ctx, fresh, "calendar_account_enable_invites", ap.ID, nil, "", "enabled")
	return fresh, true, nil
}

// verifyEnable checks the receipt against the stored request, and that the
// stored request is for this very account and address.
func (s *Service) verifyEnable(ap store.Approval, a store.Account, st api.StatusResponse) error {
	canon, err := api.Canonical(ap.Payload)
	if err != nil || api.SHA256(canon) != ap.PayloadSHA256 {
		return errors.New("the stored payload does not match its stored hash")
	}
	p, err := api.ParsePayload(api.KindAccountInviteEnable, canon)
	if err != nil || p.InviteEnable == nil {
		return errors.New("the stored payload is not an account-invite-enable payload")
	}
	if p.InviteEnable.Owner != a.Owner || p.InviteEnable.AccountID != a.ID || p.InviteEnable.FromAddress != a.Address {
		return errors.New("the stored request is for another account or address")
	}
	if st.RequestID != ap.ID || st.Receipt == nil {
		return errors.New("the approved request carries no matching receipt")
	}
	return receipt.Verify(s.ReceiptKey, *st.Receipt, receipt.Want{
		RequestID: ap.ID, Kind: api.KindAccountInviteEnable, PayloadSHA256: ap.PayloadSHA256, RPID: s.RPID, Now: s.now(),
	})
}
