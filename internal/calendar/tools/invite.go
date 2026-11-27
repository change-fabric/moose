package tools

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/change-fabric/moose/internal/accounts/fanout"
	"github.com/change-fabric/moose/internal/accounts/store"
	"github.com/change-fabric/moose/internal/audit"
	"github.com/change-fabric/moose/internal/calendar/inviteflow"
	"github.com/change-fabric/moose/internal/mcpkit"
)

// Tool names of the invite tools.
const (
	ToolEnableInvites = "calendar_account_enable_invites"
	ToolInviteStatus  = "calendar_invite_status"
)

// StatusNeedsApproval is the status of a write that is waiting for the owner's
// passkey approval.
const StatusNeedsApproval = "needs_approval"

// Recipient is one person the provider will email because of a change that
// needs approval.
type Recipient struct {
	Email    string `json:"email"`
	Name     string `json:"name,omitempty"`
	External bool   `json:"external,omitempty"`
	// Change is added (invited), kept (told of the change), removed (told they
	// were removed) or cancelled.
	Change string `json:"change"`
}

// ApprovalOut says a write is waiting for approval and who it will notify.
type ApprovalOut struct {
	RequestID   string     `json:"request_id"`
	ApprovalURL string     `json:"approval_url"`
	ExpiresAt   *time.Time `json:"expires_at,omitempty"`
	// Operation is create, update or cancel.
	Operation string `json:"operation"`
	// Notifies is everyone who will be emailed when it is applied.
	Notifies []Recipient `json:"notifies"`
	Note     string      `json:"note"`
}

// invitesOn reports whether a write that notifies others may go to approval.
func (d Deps) invitesOn(a store.Account) bool { return d.Invites != nil && a.InviteEnabled }

// inviteError maps an inviteflow refusal to a tool error.
func inviteError(ctx context.Context, err error) *sdk.CallToolResult {
	var e *inviteflow.Error
	if errors.As(err, &e) {
		return mcpkit.ErrorResult(ctx, e.Code, e.Msg)
	}
	return mcpkit.ErrorResult(ctx, "internal", "the invite request could not be completed")
}

func approvalOut(out inviteflow.RequestOut) *ApprovalOut {
	o := &ApprovalOut{RequestID: out.RequestID, ApprovalURL: out.ApprovalURL, ExpiresAt: out.ExpiresAt, Operation: out.Operation}
	for _, r := range out.Recipients {
		o.Notifies = append(o.Notifies, Recipient{Email: r.Email, Name: r.Name, External: r.External, Change: r.Change})
	}
	o.Note = "Nothing has been written. Give the approval_url to the owner to open on their phone; the page shows the full title and description and everyone who will be emailed, and they approve with their passkey. " +
		"Then call calendar_invite_status with this request_id to apply it."
	if out.Status != "pending" {
		o.Note = "This exact change was requested before; call calendar_invite_status with the request_id for where it stands (" + out.Status + ")."
	}
	return o
}

// requestInvite sends one write that notifies others to the approval flow.
func (d Deps) requestInvite(ctx context.Context, in inviteflow.RequestIn) (*sdk.CallToolResult, WriteOut) {
	out, err := d.Invites.RequestInvite(ctx, in)
	if err != nil {
		return inviteError(ctx, err), WriteOut{}
	}
	return nil, WriteOut{Status: StatusNeedsApproval, Approval: approvalOut(out)}
}

// ---- calendar_account_enable_invites ----

// EnableInvitesIn is the calendar_account_enable_invites input.
type EnableInvitesIn struct {
	Account     string `json:"account"`
	FromAddress string `json:"from_address"`
	Enable      bool   `json:"enable"`
}

// EnableInvitesOut is the calendar_account_enable_invites output.
type EnableInvitesOut struct {
	// Status is pending (waiting for the passkey), enabled, already_enabled or disabled.
	Status        string     `json:"status"`
	Account       AccountRef `json:"account"`
	InviteEnabled bool       `json:"invite_enabled"`
	RequestID     string     `json:"request_id,omitempty"`
	ApprovalURL   string     `json:"approval_url,omitempty"`
	ExpiresAt     *time.Time `json:"expires_at,omitempty"`
	Note          string     `json:"note,omitempty"`
}

func (d Deps) registerEnableInvites(s *sdk.Server) {
	mcpkit.AddTool(s, &sdk.Tool{
		Name: ToolEnableInvites,
		Description: "Turn on or off creating, changing and cancelling events that have other attendees, for one account. It is off until the owner turns it on. " +
			"enable=true needs the owner's passkey approval: you get status pending and an approval_url for the owner's phone; after they approve, call this tool again " +
			"(or calendar_list_accounts) and it switches invites on. enable=false switches them off at once, no approval. " +
			"Even with invites on, each event write with attendees still needs its own passkey approval. Answering an invitation never needs this.",
		InputSchema: &jsonschema.Schema{
			Type:     "object",
			Required: []string{"account", "from_address", "enable"},
			Properties: map[string]*jsonschema.Schema{
				"account":      str("The account's id or exact address."),
				"from_address": str("The account's exact address, restated."),
				"enable":       {Type: "boolean", Description: "true asks for approval to enable invites; false disables them directly."},
			},
		},
	}, mcpkit.Write(), d.enableInvites)
}

func (d Deps) needInvites(ctx context.Context) *sdk.CallToolResult {
	if d.Invites == nil {
		return mcpkit.ErrorResult(ctx, "invites_unavailable", "inviting others is not configured on this server: the approval service is not set up here, so only events with nobody but the account can be written")
	}
	return nil
}

func (d Deps) enableInvites(ctx context.Context, req *sdk.CallToolRequest, in EnableInvitesIn) (*sdk.CallToolResult, EnableInvitesOut, error) {
	audit.FromContext(ctx).SetAction(ToolEnableInvites, "", "")
	owner := d.owner(req)
	if owner == "" {
		return mcpkit.ErrorResult(ctx, "unauthorized", "no signed-in owner for this request"), EnableInvitesOut{}, nil
	}
	if bad := d.needInvites(ctx); bad != nil {
		return bad, EnableInvitesOut{}, nil
	}
	accts, err := d.Store.ListAccounts(ctx, owner)
	if err != nil {
		return mcpkit.ErrorResult(ctx, "internal", "could not list accounts"), EnableInvitesOut{}, nil
	}
	ref := strings.TrimSpace(in.Account)
	if ref == "" {
		return mcpkit.ErrorResult(ctx, "needs_account", "name the account (account: an id or exact address); candidates: "+strings.Join(fanout.Candidates(accts), ", ")), EnableInvitesOut{}, nil
	}
	picked, err := fanout.Resolve(accts, []string{ref})
	if err != nil {
		return resolveError(ctx, err), EnableInvitesOut{}, nil
	}
	if len(picked) != 1 {
		return mcpkit.ErrorResult(ctx, "invalid_input", "name exactly one account by id or address"), EnableInvitesOut{}, nil
	}
	a := picked[0]
	if !inviteflow.FromMatches(a, in.FromAddress) {
		return mcpkit.ErrorResult(ctx, "from_mismatch",
			"from_address "+quoted(in.FromAddress)+" is not the address of account "+a.ID+" ("+a.Address+"); restate the account's exact address"), EnableInvitesOut{}, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()

	if !in.Enable {
		if err := d.Invites.Disable(ctx, a); err != nil {
			return inviteError(ctx, err), EnableInvitesOut{}, nil
		}
		return nil, EnableInvitesOut{Status: "disabled", Account: refOf(a), Note: "Inviting others is off for " + a.Address + "."}, nil
	}
	if bad := d.usable(ctx, a); bad != nil {
		return bad, EnableInvitesOut{}, nil
	}
	if a.InviteEnabled {
		return nil, EnableInvitesOut{Status: "already_enabled", Account: refOf(a), InviteEnabled: true}, nil
	}
	// An approval that arrived since the last call is applied first.
	if fresh, applied, err := d.Invites.ApplyEnable(ctx, a); err != nil {
		return inviteError(ctx, err), EnableInvitesOut{}, nil
	} else if applied {
		return nil, EnableInvitesOut{Status: "enabled", Account: refOf(fresh), InviteEnabled: true, Note: "Inviting others is on for " + fresh.Address + ". Each event with attendees still needs its own approval."}, nil
	}
	res, err := d.Invites.RequestEnable(ctx, a, in.FromAddress)
	if err != nil {
		return inviteError(ctx, err), EnableInvitesOut{}, nil
	}
	if res.Status == "already_enabled" {
		return nil, EnableInvitesOut{Status: "already_enabled", Account: refOf(a), InviteEnabled: true}, nil
	}
	exp := res.ExpiresAt
	return nil, EnableInvitesOut{
		Status: res.Status, Account: refOf(a), RequestID: res.RequestID, ApprovalURL: res.ApprovalURL, ExpiresAt: &exp,
		Note: "Give the approval_url to the owner to open on their phone and approve with their passkey; then call calendar_account_enable_invites again.",
	}, nil
}

func quoted(s string) string {
	if len(s) > 80 {
		s = s[:80] + "..."
	}
	return `"` + s + `"`
}

// applyPendingEnables applies enable approvals that have arrived, so
// calendar_list_accounts shows an account the owner just approved as enabled. A
// failure leaves the account as it was.
func (d Deps) applyPendingEnables(ctx context.Context, accts []store.Account) []store.Account {
	if d.Invites == nil {
		return accts
	}
	out := make([]store.Account, len(accts))
	for i, a := range accts {
		out[i] = a
		if a.InviteEnabled || a.Status != store.StatusOK {
			continue
		}
		if fresh, applied, err := d.Invites.ApplyEnable(ctx, a); err == nil && applied {
			fresh.IsDefault = a.IsDefault
			out[i] = fresh
		}
	}
	return out
}

// ---- calendar_invite_status ----

// InviteStatusIn is the calendar_invite_status input.
type InviteStatusIn struct {
	RequestID string `json:"request_id"`
}

// InviteStatusOut is the calendar_invite_status output.
type InviteStatusOut struct {
	RequestID string `json:"request_id"`
	// Status is pending, applied, failed, unknown, stale, would_apply, denied,
	// expired, or what is holding it up (blocked, rate_limited, claimed,
	// dispatching, already_claimed, receipt_invalid).
	Status    string     `json:"status"`
	Operation string     `json:"operation,omitempty"`
	Account   AccountRef `json:"account"`
	// EventID and CalendarID are the composite ids of the event the change touched.
	EventID     string `json:"event_id,omitempty"`
	CalendarID  string `json:"calendar_id,omitempty"`
	ApprovalURL string `json:"approval_url,omitempty"`
	// Reconciliation is what looking at the calendar concluded about an unknown outcome.
	Reconciliation string `json:"reconciliation,omitempty"`
	DryRun         bool   `json:"dry_run,omitempty"`
	Note           string `json:"note,omitempty"`
}

func (d Deps) registerInviteStatus(s *sdk.Server) {
	mcpkit.AddTool(s, &sdk.Tool{
		Name: ToolInviteStatus,
		Description: "Check a calendar change that was waiting for approval and, once the owner has approved it, carry it out. Call it again until it reaches a final state: " +
			"applied (done, and the attendees were emailed), failed (refused, nothing changed), stale (the event changed after the approval, so it was rejected and nothing was written; ask for the change again), " +
			"unknown (it is not known whether it reached the calendar; moose keeps checking, reports what it found in reconciliation, and never replays it), would_apply (approved, but live invites are switched off), " +
			"denied or expired. pending means the approval is still waiting; blocked, rate_limited and already_claimed explain why nothing was applied yet. " +
			"What is applied is the change the owner approved, never anything else, and at most once. " + WriteGuidance,
		InputSchema: &jsonschema.Schema{
			Type:     "object",
			Required: []string{"request_id"},
			Properties: map[string]*jsonschema.Schema{
				"request_id": str("The request_id from the needs_approval answer."),
			},
		},
	}, mcpkit.Write(), d.inviteStatus)
}

func (d Deps) inviteStatus(ctx context.Context, req *sdk.CallToolRequest, in InviteStatusIn) (*sdk.CallToolResult, InviteStatusOut, error) {
	audit.FromContext(ctx).SetAction(ToolInviteStatus, "", "")
	owner := d.owner(req)
	if owner == "" {
		return mcpkit.ErrorResult(ctx, "unauthorized", "no signed-in owner for this request"), InviteStatusOut{}, nil
	}
	if bad := d.needInvites(ctx); bad != nil {
		return bad, InviteStatusOut{}, nil
	}
	if strings.TrimSpace(in.RequestID) == "" {
		return mcpkit.ErrorResult(ctx, "invalid_input", "request_id is required"), InviteStatusOut{}, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 28*time.Second)
	defer cancel()
	st, err := d.Invites.Status(ctx, owner, strings.TrimSpace(in.RequestID))
	if err != nil {
		return inviteError(ctx, err), InviteStatusOut{}, nil
	}
	out := InviteStatusOut{
		RequestID: st.RequestID, Status: st.Status, Operation: st.Operation, ApprovalURL: st.ApprovalURL,
		Reconciliation: st.Reconciliation, DryRun: st.DryRun, Note: st.Note,
	}
	if a, err := d.Store.GetAccount(ctx, owner, st.AccountID); err == nil {
		out.Account = refOf(a)
		if st.EventID != "" && st.CalendarID != "" {
			out.EventID, out.CalendarID = eventID(a, st.CalendarID, st.EventID), calendarID(a, st.CalendarID)
		}
	}
	return nil, out, nil
}
