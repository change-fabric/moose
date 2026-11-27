package graph

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/change-fabric/moose/internal/email/provider"
	"github.com/change-fabric/moose/internal/graphapi"
)

var _ provider.NativeSender = (*Client)(nil)

// provablyRejected lists the answers to POST /me/messages/{id}/send that prove
// Exchange refused the message, so nothing was delivered. It is deliberately
// short. Any other answer (a transport error, a timeout, a 5xx, a 429, a 401, a
// 404, an unlisted 4xx) leaves the outcome unknown, because the send may have
// been accepted before the answer was lost.
var provablyRejected = map[int][]string{
	http.StatusBadRequest:            {"ErrorInvalidRecipients", "ErrorInvalidRequest"},
	http.StatusForbidden:             {"ErrorAccessDenied", "ErrorSendAsDenied", "ErrorSubmissionQuotaExceeded"},
	http.StatusRequestEntityTooLarge: {"ErrorMessageSizeExceeded"},
	http.StatusUnprocessableEntity:   {"ErrorQuotaExceeded"},
}

// SendProvablyRejected reports whether err is one of the enumerated Graph
// answers that prove a send was refused.
func SendProvablyRejected(err error) bool {
	var ge *graphapi.Error
	if !errors.As(err, &ge) {
		return false
	}
	for _, code := range provablyRejected[ge.Status] {
		if ge.Code == code {
			return true
		}
	}
	return false
}

// StageDraft creates a draft from the approved snapshot. Nothing is sent. The
// draft's internetMessageId is read back when the create answer lacks it.
func (c *Client) StageDraft(ctx context.Context, raw []byte) (provider.Staged, error) {
	d, err := c.CreateDraft(ctx, provider.DraftInput{Raw: raw})
	if err != nil {
		return provider.Staged{}, err
	}
	st := provider.Staged{ID: d.ID, InternetMessageID: d.MessageID}
	if st.InternetMessageID == "" {
		if m, err := c.getDraftMsg(ctx, d.ID); err == nil {
			st.InternetMessageID = m.InternetMessageID
		}
	}
	return st, nil
}

// IssueSend submits the staging draft with POST /me/messages/{id}/send. A 2xx
// answer (Graph sends 202) means accepted, not delivered. An enumerated
// rejection wraps provider.ErrSendRejected and provider.ErrRejected; every
// other error is returned unclassified for the caller to treat as unknown.
func (c *Client) IssueSend(ctx context.Context, stagingID string) error {
	resp, err := c.g.Do(ctx, graphapi.Request{Method: http.MethodPost, Path: messagePath(stagingID) + "/send"})
	if err != nil {
		if SendProvablyRejected(err) {
			return fmt.Errorf("%w: %w: %w", provider.ErrSendRejected, provider.ErrRejected, err)
		}
		return err
	}
	resp.Body.Close()
	return nil
}

// LookupSent reads the staged message by its ImmutableId. In Sent Items it is
// SentFound with the canonical rebuild of the Sent copy; still a draft (or in
// any other folder) it is SentPending; unknown to Graph it is SentMissing.
// Nothing is matched by subject, recipients or time.
func (c *Client) LookupSent(ctx context.Context, stagingID string) (provider.SentCopy, error) {
	m, err := c.getDraftMsg(ctx, stagingID)
	if errors.Is(err, provider.ErrNotFound) {
		return provider.SentCopy{State: provider.SentMissing}, nil
	}
	if err != nil {
		return provider.SentCopy{}, err
	}
	sys, err := c.systemIDs(ctx)
	if err != nil {
		return provider.SentCopy{}, err
	}
	if m.IsDraft || sys[m.ParentFolderID] != provider.MailboxSent {
		return provider.SentCopy{State: provider.SentPending, ID: m.ID, InternetMessageID: m.InternetMessageID}, nil
	}
	date := m.SentDateTime
	if date.IsZero() {
		date = m.LastModifiedDateTime
	}
	raw, err := canonical(m, date)
	if err != nil {
		// The Sent copy exists but cannot be rebuilt as plain text: report it
		// found with no bytes, so the caller records a mismatch, not a failure.
		return provider.SentCopy{State: provider.SentFound, ID: m.ID, InternetMessageID: m.InternetMessageID}, nil
	}
	return provider.SentCopy{State: provider.SentFound, ID: m.ID, InternetMessageID: m.InternetMessageID, Raw: raw}, nil
}

// DeleteStaging permanently deletes the staging draft, but only while it is
// still a draft. After a send the same id names the Sent Items copy, and that
// is never deleted. A missing message is ErrNotFound; a message that is no
// longer a draft is left alone and also reported as ErrNotFound.
func (c *Client) DeleteStaging(ctx context.Context, stagingID string) error {
	return c.DeleteDraft(ctx, stagingID)
}

// SendDraft stages raw, issues the send, and looks briefly for the Sent copy,
// returning its id (the staging id when the copy has not appeared yet; that id
// stays valid after the move). It exists so Graph satisfies provider.Provider
// and the conformance suite; the send engine drives the steps itself through
// NativeSender so the ledger sees each one.
func (c *Client) SendDraft(ctx context.Context, raw []byte, _ string) (provider.SentMessage, error) {
	st, err := c.StageDraft(ctx, raw)
	if err != nil {
		return provider.SentMessage{}, err
	}
	if err := c.IssueSend(ctx, st.ID); err != nil {
		if errors.Is(err, provider.ErrSendRejected) {
			_ = c.DeleteStaging(ctx, st.ID)
		}
		return provider.SentMessage{}, err
	}
	interval := c.PollInterval
	if interval <= 0 {
		interval = 750 * time.Millisecond
	}
	for attempt := 0; attempt < 5; attempt++ {
		cp, err := c.LookupSent(ctx, st.ID)
		if err == nil && cp.State == provider.SentFound {
			return provider.SentMessage{MessageID: cp.ID}, nil
		}
		select {
		case <-ctx.Done():
			return provider.SentMessage{MessageID: st.ID}, nil
		case <-time.After(interval):
		}
	}
	return provider.SentMessage{MessageID: st.ID}, nil
}
