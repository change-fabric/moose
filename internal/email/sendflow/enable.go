package sendflow

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"time"

	"github.com/change-fabric/moose/internal/accounts/connect"
	"github.com/change-fabric/moose/internal/accounts/store"
	"github.com/change-fabric/moose/internal/approver/api"
	"github.com/change-fabric/moose/internal/approver/receipt"
)

// ConnectApprover adapts the approver client to connect.Approver, so the
// connect page's Enable send button and email_account_enable_send create the
// same account-send-enable request.
type ConnectApprover struct{ Approver Approver }

// CreateRequest implements connect.Approver.
func (c ConnectApprover) CreateRequest(ctx context.Context, r connect.ApprovalRequest) (connect.ApprovalResponse, error) {
	pv := api.Preview{
		Action: r.Preview["action"], AccountID: r.Preview["account_id"], Nickname: r.Preview["nickname"],
		FromAddress: r.Preview["from_address"], DisplayName: r.Preview["display_name"], AccountStatus: r.Preview["account_status"],
	}
	pv.ColorIndex, _ = strconv.Atoi(r.Preview["color_index"])
	if raw := r.Preview["siblings"]; raw != "" {
		_ = json.Unmarshal([]byte(raw), &pv.Siblings)
	}
	rawPV, _ := json.Marshal(pv)
	resp, _, err := c.Approver.Create(ctx, api.CreateBody{IdempotencyKey: r.IdempotencyKey, Kind: r.Kind, Payload: r.Payload, Preview: rawPV})
	if err != nil {
		return connect.ApprovalResponse{}, err
	}
	return connect.ApprovalResponse{RequestID: resp.RequestID, PayloadSHA256: resp.PayloadSHA256, ApprovalURL: resp.ApprovalURL, ExpiresAt: resp.ExpiresAt}, nil
}

// SendEnableResult is the outcome of an enable request or its application.
type SendEnableResult struct {
	// Status is pending (waiting for the passkey), enabled, or already_enabled.
	Status      string
	RequestID   string
	ApprovalURL string
	ExpiresAt   time.Time
}

// RequestEnable asks for the passkey approval that turns sending on for a. The
// request is the same one the connect page creates, and repeating the call
// inside its window returns it again.
func (s *Service) RequestEnable(ctx context.Context, a store.Account, from string) (SendEnableResult, error) {
	if !FromMatches(a, from) {
		return SendEnableResult{}, errf(CodeFromMismatch, "from_address %q is not the address of account %s (%s); restate the account's exact address", from, a.ID, a.Address)
	}
	if a.SendEnabled {
		return SendEnableResult{Status: "already_enabled"}, nil
	}
	if a.Status != store.StatusOK {
		return SendEnableResult{}, errf(CodeAccountNotReady, "%s is %s; reconnect it before enabling send", a.Address, a.Status)
	}
	resp, err := connect.RequestSendEnable(ctx, s.Store, ConnectApprover{s.Approver}, a, s.now())
	if err != nil {
		s.logf("request send enable", "err", err.Error())
		return SendEnableResult{}, errf(CodeApprover, "the approval service could not create the request")
	}
	return SendEnableResult{Status: StatusPending, RequestID: resp.RequestID, ApprovalURL: resp.ApprovalURL, ExpiresAt: resp.ExpiresAt}, nil
}

// Disable turns sending off at once; disabling needs no approval. Any enable
// request still waiting or approved but not applied is cancelled, so an old
// approval cannot turn sending back on.
func (s *Service) Disable(ctx context.Context, a store.Account) error {
	fresh, err := s.Store.GetAccount(ctx, a.Owner, a.ID)
	if err != nil {
		return errf(CodeInternal, "could not read the account")
	}
	if fresh.SendEnabled {
		if err := s.Store.UpdateAccount(ctx, fresh.Owner, fresh.ID, store.AccountPatch{SendEnabled: store.Ptr(false)}); err != nil {
			return errf(CodeInternal, "could not disable sending")
		}
	}
	aps, err := s.Store.FindApprovals(ctx, store.KindAccountSendEnable, a.ID)
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

// ApplyEnable looks for an approved account-send-enable request for a and, if
// its receipt verifies, turns sending on: verify, claim locally with a
// conditional write, consume (idempotent), set send_enabled. A claim that was
// interrupted resumes. It returns the account as it now stands.
func (s *Service) ApplyEnable(ctx context.Context, a store.Account) (store.Account, bool, error) {
	if a.SendEnabled {
		return a, false, nil
	}
	aps, err := s.Store.FindApprovals(ctx, store.KindAccountSendEnable, a.ID)
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
				return a, false, errf(CodeReceipt, "the approval's receipt did not verify against the stored request; sending stays off")
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
			return a, false, errf(CodeReceipt, "the approval was already consumed by another claimant; sending stays off")
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
	if err := s.Store.UpdateAccount(ctx, fresh.Owner, fresh.ID, store.AccountPatch{SendEnabled: store.Ptr(true)}); err != nil {
		return a, false, errf(CodeInternal, "could not enable sending")
	}
	fresh.SendEnabled = true
	if _, err := s.Store.TransitionApproval(ctx, ap.ID, []string{store.ApprovalApproved, store.ApprovalPending}, store.ApprovalConsumed, false); err != nil {
		s.logf("mark enable consumed", "err", err.Error())
	}
	s.audit(ctx, fresh, "email_account_enable_send", ap.ID, nil, "", "enabled")
	return fresh, true, nil
}

// verifyEnable checks the receipt against the stored request, and that the
// stored request is for this very account and address.
func (s *Service) verifyEnable(ap store.Approval, a store.Account, st api.StatusResponse) error {
	canon, err := api.Canonical(ap.Payload)
	if err != nil || api.SHA256(canon) != ap.PayloadSHA256 {
		return errors.New("the stored payload does not match its stored hash")
	}
	p, err := api.ParsePayload(api.KindAccountSendEnable, canon)
	if err != nil || p.SendEnable == nil {
		return errors.New("the stored payload is not an account-send-enable payload")
	}
	if p.SendEnable.Owner != a.Owner || p.SendEnable.AccountID != a.ID || p.SendEnable.FromAddress != a.Address {
		return errors.New("the stored request is for another account or address")
	}
	if st.RequestID != ap.ID || st.Receipt == nil {
		return errors.New("the approved request carries no matching receipt")
	}
	return receipt.Verify(s.ReceiptKey, *st.Receipt, receipt.Want{
		RequestID: ap.ID, Kind: api.KindAccountSendEnable, PayloadSHA256: ap.PayloadSHA256, RPID: s.RPID, Now: s.now(),
	})
}
