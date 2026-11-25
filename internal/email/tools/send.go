package tools

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/change-fabric/moose/internal/audit"
	"github.com/change-fabric/moose/internal/email/sendflow"
	"github.com/change-fabric/moose/internal/email/store"
	"github.com/change-fabric/moose/internal/mcpkit"
)

// sendError maps an engine refusal to a tool error.
func sendError(ctx context.Context, err error) *sdk.CallToolResult {
	var e *sendflow.Error
	if errors.As(err, &e) {
		return mcpkit.ErrorResult(ctx, e.Code, e.Msg)
	}
	return mcpkit.ErrorResult(ctx, "internal", "the send request failed")
}

func (d Deps) needSend(ctx context.Context) *sdk.CallToolResult {
	if d.Send == nil {
		return mcpkit.ErrorResult(ctx, "internal", "sending is not configured on this server")
	}
	return nil
}

// ---- email_request_send ----

// RequestSendIn is the email_request_send input: one draft, or a batch of
// drafts from the same account.
type RequestSendIn struct {
	AccountID   string   `json:"account_id"`
	FromAddress string   `json:"from_address"`
	DraftID     string   `json:"draft_id,omitempty"`
	Drafts      []string `json:"drafts,omitempty"`
}

// RequestSendOut is the email_request_send output.
type RequestSendOut struct {
	// Status is pending: the owner has to approve with a passkey. Open
	// ApprovalURL on a phone, then call email_send_status with RequestID.
	Status      string          `json:"status"`
	RequestID   string          `json:"request_id"`
	ApprovalURL string          `json:"approval_url,omitempty"`
	Batch       bool            `json:"batch"`
	Account     AccountRef      `json:"account"`
	Items       []sendflow.Item `json:"items"`
	Note        string          `json:"note"`
}

func (d Deps) registerRequestSend(s *sdk.Server) {
	str := func(desc string) *jsonschema.Schema { return &jsonschema.Schema{Type: "string", Description: desc} }
	mcpkit.AddTool(s, &sdk.Tool{
		Name: ToolRequestSend,
		Description: "Ask the owner to approve sending a draft. moose snapshots the draft's exact bytes (plain text only, at most 256 KB), shows them on the owner's phone " +
			"and waits for a passkey approval; you get status pending and an approval_url to hand to the owner. The send is refused unless sending is enabled for the " +
			"account (email_account_enable_send). Pass one draft_id, or `drafts` to send several from the same account with one approval. " +
			"After the owner approves, call email_send_status to carry it out. " + WriteGuidance,
		InputSchema: &jsonschema.Schema{
			Type:     "object",
			Required: []string{"account_id", "from_address"},
			Properties: map[string]*jsonschema.Schema{
				"account_id":   str("The sending account: its id or exact address."),
				"from_address": str("The account's exact address, restated."),
				"draft_id":     str("Composite draft id (acc_...:draft) from email_create_draft."),
				"drafts":       strList("Several composite draft ids, all from this one account, approved together."),
			},
		},
	}, mcpkit.Write(), d.requestSend)
}

func (d Deps) requestSend(ctx context.Context, req *sdk.CallToolRequest, in RequestSendIn) (*sdk.CallToolResult, RequestSendOut, error) {
	audit.FromContext(ctx).SetAction(ToolRequestSend, "", "")
	owner := d.owner(req)
	if owner == "" {
		return mcpkit.ErrorResult(ctx, "unauthorized", "no signed-in owner for this request"), RequestSendOut{}, nil
	}
	if bad := d.needSend(ctx); bad != nil {
		return bad, RequestSendOut{}, nil
	}
	ids := in.Drafts
	switch {
	case in.DraftID != "" && len(in.Drafts) > 0:
		return mcpkit.ErrorResult(ctx, "invalid_input", "pass draft_id or drafts, not both"), RequestSendOut{}, nil
	case in.DraftID != "":
		ids = []string{in.DraftID}
	case len(ids) == 0:
		return mcpkit.ErrorResult(ctx, "invalid_input", "name the draft to send: draft_id, or drafts for a batch"), RequestSendOut{}, nil
	}
	a, bad := d.writeAccount(ctx, owner, in.AccountID, in.FromAddress)
	if bad != nil {
		return bad, RequestSendOut{}, nil
	}
	raw := make([]string, 0, len(ids))
	for _, id := range ids {
		accID, rawID, ok := splitID(id)
		switch {
		case !ok:
			return mcpkit.ErrorResult(ctx, "invalid_input", fmt.Sprintf("draft id %q must be a composite id like acc_xxxx:draft_id, as returned by email_create_draft", clip(id, 60))), RequestSendOut{}, nil
		case accID != a.ID:
			return mcpkit.ErrorResult(ctx, "cross_account_batch",
				fmt.Sprintf("draft %s belongs to another account than %s; a request holds drafts from one account only", id, a.Address)), RequestSendOut{}, nil
		}
		raw = append(raw, rawID)
	}
	ctx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()
	out, err := d.Send.RequestSend(ctx, sendflow.RequestIn{Account: a, FromAddress: in.FromAddress, DraftIDs: raw})
	if err != nil {
		return sendError(ctx, err), RequestSendOut{}, nil
	}
	for i := range out.Items {
		out.Items[i].DraftID = compositeID(a.ID, out.Items[i].DraftID)
	}
	note := "Give the approval_url to the owner to open on their phone; they approve with their passkey. Then call email_send_status with this request_id."
	if out.Status != "pending" {
		note = "These exact bytes were requested before; call email_send_status with the request_id for where it stands."
	}
	return nil, RequestSendOut{Status: out.Status, RequestID: out.RequestID, ApprovalURL: out.ApprovalURL, Batch: out.Batch,
		Account: refOf(a), Items: out.Items, Note: note}, nil
}

// ---- email_send_status ----

// SendStatusIn is the email_send_status input.
type SendStatusIn struct {
	RequestID string `json:"request_id"`
}

func (d Deps) registerSendStatus(s *sdk.Server) {
	mcpkit.AddTool(s, &sdk.Tool{
		Name: ToolSendStatus,
		Description: "Check a send request and, once the owner has approved it, carry it out. Call it again until it reaches a final state: " +
			"sent, failed, unknown (it is not known whether the message left; check the Sent folder, it is never retried), would_send (approved, but live sending is switched off), " +
			"denied or expired. pending means the approval is still waiting; blocked, rate_limited and already_claimed explain why nothing was sent yet. " +
			"The message sent is the exact snapshot the owner approved, never the draft as it is now, and it is sent at most once. " +
			"The source draft stays in the mailbox after a send. Pass a batch's request_id to carry out every message in it. " + WriteGuidance,
		InputSchema: &jsonschema.Schema{
			Type:     "object",
			Required: []string{"request_id"},
			Properties: map[string]*jsonschema.Schema{
				"request_id": {Type: "string", Description: "The request_id from email_request_send."},
			},
		},
	}, mcpkit.Write(), d.sendStatus)
}

func (d Deps) sendStatus(ctx context.Context, req *sdk.CallToolRequest, in SendStatusIn) (*sdk.CallToolResult, sendflow.StatusOut, error) {
	audit.FromContext(ctx).SetAction(ToolSendStatus, "", "")
	owner := d.owner(req)
	if owner == "" {
		return mcpkit.ErrorResult(ctx, "unauthorized", "no signed-in owner for this request"), sendflow.StatusOut{}, nil
	}
	if bad := d.needSend(ctx); bad != nil {
		return bad, sendflow.StatusOut{}, nil
	}
	if strings.TrimSpace(in.RequestID) == "" {
		return mcpkit.ErrorResult(ctx, "invalid_input", "request_id is required"), sendflow.StatusOut{}, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 28*time.Second)
	defer cancel()
	out, err := d.Send.Status(ctx, owner, strings.TrimSpace(in.RequestID))
	if err != nil {
		return sendError(ctx, err), sendflow.StatusOut{}, nil
	}
	if out.SourceDraftID != "" {
		out.SourceDraftID = compositeID(out.AccountID, out.SourceDraftID)
	}
	for i := range out.Items {
		if out.Items[i].SourceDraftID != "" {
			out.Items[i].SourceDraftID = compositeID(out.Items[i].AccountID, out.Items[i].SourceDraftID)
		}
	}
	return nil, out, nil
}

// ---- email_account_enable_send ----

// EnableSendIn is the email_account_enable_send input.
type EnableSendIn struct {
	AccountID   string `json:"account_id"`
	FromAddress string `json:"from_address"`
	Enable      bool   `json:"enable"`
}

// EnableSendOut is the email_account_enable_send output.
type EnableSendOut struct {
	// Status is pending (waiting for the passkey approval), enabled,
	// already_enabled or disabled.
	Status      string     `json:"status"`
	Account     AccountRef `json:"account"`
	SendEnabled bool       `json:"send_enabled"`
	RequestID   string     `json:"request_id,omitempty"`
	ApprovalURL string     `json:"approval_url,omitempty"`
	ExpiresAt   *time.Time `json:"expires_at,omitempty"`
	Note        string     `json:"note,omitempty"`
}

func (d Deps) registerEnableSend(s *sdk.Server) {
	mcpkit.AddTool(s, &sdk.Tool{
		Name: ToolEnableSend,
		Description: "Turn sending on or off for one account. A connected account is read-only until sending is enabled. " +
			"enable=true needs the owner's passkey approval: you get status pending and an approval_url for the owner's phone; after they approve, call this tool again " +
			"(or email_list_accounts) and it switches sending on. enable=false switches it off at once. " + WriteGuidance,
		InputSchema: &jsonschema.Schema{
			Type:     "object",
			Required: []string{"account_id", "from_address", "enable"},
			Properties: map[string]*jsonschema.Schema{
				"account_id":   {Type: "string", Description: "The account's id or exact address."},
				"from_address": {Type: "string", Description: "The account's exact address, restated."},
				"enable":       {Type: "boolean", Description: "true asks for approval to enable sending; false disables it directly."},
			},
		},
	}, mcpkit.Write(), d.enableSend)
}

func (d Deps) enableSend(ctx context.Context, req *sdk.CallToolRequest, in EnableSendIn) (*sdk.CallToolResult, EnableSendOut, error) {
	audit.FromContext(ctx).SetAction(ToolEnableSend, "", "")
	owner := d.owner(req)
	if owner == "" {
		return mcpkit.ErrorResult(ctx, "unauthorized", "no signed-in owner for this request"), EnableSendOut{}, nil
	}
	if bad := d.needSend(ctx); bad != nil {
		return bad, EnableSendOut{}, nil
	}
	accts, err := d.Store.ListAccounts(ctx, owner)
	if err != nil {
		return mcpkit.ErrorResult(ctx, "internal", "could not list accounts"), EnableSendOut{}, nil
	}
	if strings.TrimSpace(in.AccountID) == "" {
		return mcpkit.ErrorResult(ctx, "needs_account", "name the account (account_id: an id or exact address); candidates: "+strings.Join(candidates(accts), ", ")), EnableSendOut{}, nil
	}
	a, err := singleAccount(accts, in.AccountID)
	if err != nil {
		var nf *NotFoundError
		if errors.As(err, &nf) {
			return resolveError(ctx, err), EnableSendOut{}, nil
		}
		return mcpkit.ErrorResult(ctx, "invalid_input", err.Error()), EnableSendOut{}, nil
	}
	if !sendflow.FromMatches(a, in.FromAddress) {
		return mcpkit.ErrorResult(ctx, sendflow.CodeFromMismatch,
			"from_address "+quote(in.FromAddress)+" is not the address of account "+a.ID+" ("+a.Address+"); restate the account's exact address"), EnableSendOut{}, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()

	if !in.Enable {
		if err := d.Send.Disable(ctx, a); err != nil {
			return sendError(ctx, err), EnableSendOut{}, nil
		}
		return nil, EnableSendOut{Status: "disabled", Account: refOf(a), SendEnabled: false, Note: "Sending is off for " + a.Address + "."}, nil
	}
	if a.SendEnabled {
		return nil, EnableSendOut{Status: "already_enabled", Account: refOf(a), SendEnabled: true}, nil
	}
	// An approval that arrived since the last call is applied first.
	if fresh, applied, err := d.Send.ApplyEnable(ctx, a); err != nil {
		return sendError(ctx, err), EnableSendOut{}, nil
	} else if applied {
		return nil, EnableSendOut{Status: "enabled", Account: refOf(fresh), SendEnabled: true, Note: "Sending is on for " + fresh.Address + "."}, nil
	}
	res, err := d.Send.RequestEnable(ctx, a, in.FromAddress)
	if err != nil {
		return sendError(ctx, err), EnableSendOut{}, nil
	}
	exp := res.ExpiresAt
	return nil, EnableSendOut{
		Status: res.Status, Account: refOf(a), RequestID: res.RequestID, ApprovalURL: res.ApprovalURL, ExpiresAt: &exp,
		Note: "Give the approval_url to the owner to open on their phone and approve with their passkey; then call email_account_enable_send again.",
	}, nil
}

// applyPendingEnables applies enable approvals that have arrived, so
// email_list_accounts shows an account the owner just approved as enabled. A
// failure leaves the account as it was.
func (d Deps) applyPendingEnables(ctx context.Context, accts []store.Account) []store.Account {
	if d.Send == nil {
		return accts
	}
	out := make([]store.Account, len(accts))
	for i, a := range accts {
		out[i] = a
		if a.SendEnabled || a.Status != store.StatusOK {
			continue
		}
		if fresh, applied, err := d.Send.ApplyEnable(ctx, a); err == nil && applied {
			fresh.IsDefault = a.IsDefault
			out[i] = fresh
		}
	}
	return out
}
