package tools

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/change-fabric/moose/internal/audit"
	"github.com/change-fabric/moose/internal/email/attach"
	"github.com/change-fabric/moose/internal/email/provider"
	"github.com/change-fabric/moose/internal/email/store"
	"github.com/change-fabric/moose/internal/mcpkit"
)

const (
	// MaxAttachmentContent caps the wrapped attachment text. The cap applies to
	// the wrapped output, so the wrapper is never cut off.
	MaxAttachmentContent = 64 << 10
	// attachmentTimeout bounds one call: download, upload, parse and presign
	// must fit under the API's 30 second limit.
	attachmentTimeout = 25 * time.Second
)

// GetAttachmentIn is the email_get_attachment input.
type GetAttachmentIn struct {
	MessageID    string `json:"message_id"`
	AttachmentID string `json:"attachment_id"`
}

// AttachmentResult is the email_get_attachment output.
type AttachmentResult struct {
	Account AccountRef `json:"account"`
	// MessageID is the composite id of the message that carries the attachment.
	MessageID    string `json:"message_id"`
	AttachmentID string `json:"attachment_id"`
	MIMEType     string `json:"mime_type,omitempty"`
	Size         int64  `json:"size"`
	SenderTrust  string `json:"sender_trust"`
	// Status says how the text extraction went: extracted, no_text,
	// unsupported, limit_exceeded, unreadable, timeout or busy.
	Status string `json:"status"`
	// Note is moose's own explanation of an empty or partial result.
	Note string `json:"note,omitempty"`
	// Content is the extracted text inside the untrusted wrapper, capped at
	// 64 KiB. It is empty when no text could be read.
	Content   string `json:"content,omitempty"`
	Truncated bool   `json:"truncated,omitempty"`
	// DownloadURL fetches the original file until ExpiresAt (15 minutes). It
	// is a bearer link: anyone who has it can download the file.
	DownloadURL string    `json:"download_url"`
	ExpiresAt   time.Time `json:"expires_at"`
}

func (d Deps) registerGetAttachment(s *sdk.Server) {
	mcpkit.AddTool(s, &sdk.Tool{
		Name: ToolGetAttachment,
		Description: "Read one attachment of a message: PDF, docx and plain text are converted to text (capped at 64 KiB) and come back inside an untrusted wrapper, " +
			"together with a download link for the original file that works for 15 minutes. Other file types return only the link. " +
			"Find attachment ids in the attachments of email_get_message. " + contentWrapperNote,
		InputSchema: &jsonschema.Schema{
			Type:     "object",
			Required: []string{"message_id", "attachment_id"},
			Properties: map[string]*jsonschema.Schema{
				"message_id":    {Type: "string", Description: "Composite message id (acc_...:message) from email_search or email_get_message."},
				"attachment_id": {Type: "string", Description: "An attachment id from that message's attachments."},
			},
		},
	}, mcpkit.ReadOnly(), d.getAttachment)
}

func (d Deps) getAttachment(ctx context.Context, req *sdk.CallToolRequest, in GetAttachmentIn) (*sdk.CallToolResult, AttachmentResult, error) {
	audit.FromContext(ctx).SetAction(ToolGetAttachment, "", "")
	if in.AttachmentID == "" {
		return mcpkit.ErrorResult(ctx, "invalid_input", "attachment_id is required"), AttachmentResult{}, nil
	}
	a, p, rawMsg, bad := d.single(ctx, req, in.MessageID)
	if bad != nil {
		return bad, AttachmentResult{}, nil
	}
	if d.Attach == nil {
		return mcpkit.ErrorResult(ctx, "internal", "attachments are not configured on this server"), AttachmentResult{}, nil
	}
	ctx, cancel := context.WithTimeout(ctx, attachmentTimeout)
	defer cancel()

	var m provider.Message
	if err := d.retry429(ctx, func() (e error) { m, e = p.GetMessage(ctx, rawMsg); return }); err != nil {
		return d.providerError(ctx, err), AttachmentResult{}, nil
	}
	found := false
	for _, at := range m.Attachments {
		if at.ID == in.AttachmentID {
			found = true
		}
	}
	if !found {
		return mcpkit.ErrorResult(ctx, "not_found", "that message has no attachment with that id"), AttachmentResult{}, nil
	}
	trust := d.newTrust(a, p).of(ctx, m.From.Email)

	res, err := d.Attach.Fetch(ctx, p, rawMsg, in.AttachmentID)
	switch {
	case errors.Is(err, attach.ErrTooLarge), errors.Is(err, provider.ErrTooLarge):
		return mcpkit.ErrorResult(ctx, "attachment_too_large", fmt.Sprintf("attachments over %d MiB are not fetched", provider.MaxAttachment>>20)), AttachmentResult{}, nil
	case errors.Is(err, provider.ErrNotFound), errors.Is(err, provider.ErrAuth), errors.Is(err, provider.ErrRateLimited), errors.Is(err, context.DeadlineExceeded):
		return d.providerError(ctx, err), AttachmentResult{}, nil
	case err != nil:
		return mcpkit.ErrorResult(ctx, "attachment_error", "the attachment could not be fetched"), AttachmentResult{}, nil
	}
	return nil, buildAttachment(a, m, trust, in.AttachmentID, res), nil
}

// buildAttachment wraps the extracted text. The caps apply after wrapping.
func buildAttachment(a store.Account, m provider.Message, trust, attachmentID string, res attach.Result) AttachmentResult {
	out := AttachmentResult{
		Account: refOf(a), MessageID: compositeID(a.ID, m.ID), AttachmentID: attachmentID,
		MIMEType: res.Meta.MIMEType, Size: res.Meta.Size, SenderTrust: trust,
		Status: res.Status, Note: res.Note, DownloadURL: res.URL, ExpiresAt: res.Expires,
	}
	if res.Text == "" {
		out.Truncated = res.Truncated
		return out
	}
	prov := func(truncated bool) string {
		return mcpkit.Provenance(
			"sender_trust", trust,
			"account", a.Address,
			"account_id", a.ID,
			"message_id", compositeID(a.ID, m.ID),
			"attachment_id", attachmentID,
			"truncated", fmt.Sprint(truncated),
			"from", m.From.Email,
			"filename", res.Meta.Filename,
		)
	}
	content, cut := mcpkit.UntrustedCapped(prov(res.Truncated), res.Text, MaxAttachmentContent)
	if cut && !res.Truncated {
		content, _ = mcpkit.UntrustedCapped(prov(true), res.Text, MaxAttachmentContent)
	}
	out.Content, out.Truncated = content, res.Truncated || cut
	return out
}
