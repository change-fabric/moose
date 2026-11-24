package connect

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/change-fabric/moose/internal/email/store"
)

// sendEnableWindow is how long one account-send-enable request is reused for
// the same account: it matches the approver's 15 minute request lifetime, so a
// double click shares one request and a retry after expiry gets a fresh one.
const sendEnableWindow = 15 * time.Minute

// SendEnablePayload returns the canonical JSON of an account-send-enable
// payload, {account_id, from_address, owner}: keys sorted, no HTML escaping.
// The receipt's payload_sha256 covers it, so the approval is bound to this
// account and exact from address.
func SendEnablePayload(owner string, a store.Account) []byte {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	// A map marshals with sorted keys.
	_ = enc.Encode(map[string]string{"owner": owner, "account_id": a.ID, "from_address": a.Address})
	return bytes.TrimRight(buf.Bytes(), "\n")
}

// RequestSendEnable asks the approver for an `account-send-enable` approval for
// a and records the request locally. The connect page's Enable send button and
// the email_account_enable_send tool share it, so both create the same request.
// Repeating the call within sendEnableWindow returns the same request.
func RequestSendEnable(ctx context.Context, st store.Store, ap Approver, a store.Account, now time.Time) (ApprovalResponse, error) {
	payload := SendEnablePayload(a.Owner, a)
	sum := sha256.Sum256(payload)
	wantSHA := hex.EncodeToString(sum[:])
	window := now.Unix() / int64(sendEnableWindow/time.Second)
	keyHash := sha256.Sum256([]byte(fmt.Sprintf("%s|%s|%s|%s|%d", store.KindAccountSendEnable, a.Owner, a.ID, a.Address, window)))
	key := hex.EncodeToString(keyHash[:])

	resp, err := ap.CreateRequest(ctx, ApprovalRequest{
		IdempotencyKey: key,
		Kind:           store.KindAccountSendEnable,
		Payload:        payload,
		Preview: map[string]string{
			"action": "Enable sending", "nickname": a.Nickname, "from_address": a.Address, "account_id": a.ID,
		},
	})
	if err != nil {
		return ApprovalResponse{}, err
	}
	if resp.PayloadSHA256 != wantSHA {
		return ApprovalResponse{}, fmt.Errorf("approver hashed a different payload (%s, want %s)", resp.PayloadSHA256, wantSHA)
	}
	_, _, err = st.CreateApproval(ctx, store.Approval{
		ID: resp.RequestID, Owner: a.Owner, Kind: store.KindAccountSendEnable, AccountID: a.ID,
		IdempotencyKey: key, PayloadSHA256: wantSHA, Payload: payload, ApprovalURL: resp.ApprovalURL,
		Status: store.ApprovalPending, Created: now, Expires: resp.ExpiresAt,
	}, now)
	if err != nil && !isDuplicate(err) {
		return ApprovalResponse{}, err
	}
	return resp, nil
}

// isDuplicate reports a request id the store already holds, which happens when
// the approver returned an existing request for a repeated idempotency key.
func isDuplicate(err error) bool { return errors.Is(err, store.ErrConflict) }
