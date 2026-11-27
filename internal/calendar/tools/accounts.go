package tools

import (
	"context"
	"errors"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/change-fabric/moose/internal/accounts/fanout"
	"github.com/change-fabric/moose/internal/accounts/palette"
	"github.com/change-fabric/moose/internal/accounts/scopes"
	"github.com/change-fabric/moose/internal/accounts/store"
	"github.com/change-fabric/moose/internal/audit"
	"github.com/change-fabric/moose/internal/mcpkit"
)

// Calendar grant states.
const (
	// CalendarGranted: every permission calendar needs is held.
	CalendarGranted = "granted"
	// CalendarPartial: some are held and some are missing; see missing_scopes.
	CalendarPartial = "partial"
	// CalendarNotGranted: the owner has not granted calendar access to the account.
	CalendarNotGranted = "not_granted"
)

// DegradedOwnCalendarOnly marks a personal Microsoft account: free/busy and
// find-times work for its own calendar only.
const DegradedOwnCalendarOnly = "own_calendar_only"

// ListAccountsIn is the calendar_list_accounts input.
type ListAccountsIn struct {
	// Account narrows the listing; see fanout.Refs. Omitted or "all" lists every account.
	Account fanout.Refs `json:"account,omitempty"`
}

// CalendarOut is one account's calendar standing.
type CalendarOut struct {
	// Status is granted, partial or not_granted.
	Status string `json:"status"`
	// Scopes are the calendar permissions actually granted.
	Scopes []string `json:"scopes"`
	// MissingScopes are the permissions calendar needs that were not granted;
	// the owner finishes them with Grant calendar on the moose accounts page.
	MissingScopes []string   `json:"missing_scopes,omitempty"`
	GrantedAt     *time.Time `json:"granted_at,omitempty"`
	// InviteEnabled is whether the owner allowed events with attendees on this
	// account (off by default; turning it on needs a passkey approval).
	InviteEnabled bool `json:"invite_enabled"`
	// Degraded names a reduced capability, such as own_calendar_only.
	Degraded string `json:"degraded,omitempty"`
}

// AccountOut is one connected account as the calendar tools show it. It never
// carries a token.
type AccountOut struct {
	AccountID   string      `json:"account_id"`
	Provider    string      `json:"provider"`
	Address     string      `json:"address"`
	DisplayName string      `json:"display_name,omitempty"`
	Groups      []string    `json:"groups,omitempty"`
	IsDefault   bool        `json:"is_default"`
	ColorIndex  int         `json:"color_index"`
	ColorName   string      `json:"color_name"`
	Status      string      `json:"status"`
	LastError   string      `json:"last_error,omitempty"`
	Calendar    CalendarOut `json:"calendar"`
}

// ListAccountsOut is the calendar_list_accounts output.
type ListAccountsOut struct {
	Accounts []AccountOut `json:"accounts"`
}

func (d Deps) registerListAccounts(s *sdk.Server) {
	mcpkit.AddTool(s, &sdk.Tool{
		Name: ToolListAccounts,
		Description: "List the connected accounts and, for each, the state of its calendar access: granted, partial (with missing_scopes) or not_granted, " +
			"whether events with attendees are enabled, and whether it is limited to its own calendar (a personal Microsoft account). " +
			"These are the same accounts as the email tools. Calendar access is granted per account by the owner on the moose accounts page. " +
			"Pass account (an id or exact address, \"group:<tag>\", or a list) to look at some accounts.",
		InputSchema: &jsonschema.Schema{
			Type:       "object",
			Properties: map[string]*jsonschema.Schema{"account": fanout.RefsSchema(fanout.RefsHelp)},
		},
	}, mcpkit.ReadOnly(), func(ctx context.Context, req *sdk.CallToolRequest, in ListAccountsIn) (*sdk.CallToolResult, ListAccountsOut, error) {
		audit.FromContext(ctx).SetAction(ToolListAccounts, "", "")
		owner := d.owner(req)
		if owner == "" {
			return mcpkit.ErrorResult(ctx, "unauthorized", "no signed-in owner for this request"), ListAccountsOut{}, nil
		}
		accts, err := d.Store.ListAccounts(ctx, owner)
		if err != nil {
			return mcpkit.ErrorResult(ctx, "internal", "could not list accounts"), ListAccountsOut{}, nil
		}
		picked, err := fanout.Resolve(accts, in.Account)
		if err != nil {
			return resolveError(ctx, err), ListAccountsOut{}, nil
		}
		// An invite-enable approval that arrived since the last call is applied, so
		// an account the owner just approved shows as enabled.
		picked = d.applyPendingEnables(ctx, picked)
		out := ListAccountsOut{Accounts: make([]AccountOut, 0, len(picked))}
		for _, a := range picked {
			out.Accounts = append(out.Accounts, accountOut(a))
		}
		return nil, out, nil
	})
}

// resolveError maps a Resolve failure to a tool error.
func resolveError(ctx context.Context, err error) *sdk.CallToolResult {
	var nf *fanout.NotFoundError
	if errors.As(err, &nf) {
		return mcpkit.ErrorResult(ctx, "account_not_found", nf.Error())
	}
	return mcpkit.ErrorResult(ctx, "internal", "could not resolve the account")
}

func accountOut(a store.Account) AccountOut {
	c := palette.At(a.ColorIndex)
	groups := a.Groups
	if groups == nil {
		groups = []string{}
	}
	return AccountOut{
		AccountID: a.ID, Provider: a.Provider, Address: a.Address, DisplayName: a.DisplayName,
		Groups: groups, IsDefault: a.IsDefault, ColorIndex: a.ColorIndex, ColorName: c.Name, Status: a.Status,
		LastError: a.LastError, Calendar: calendarOut(a),
	}
}

// calendarOut reads an account's calendar grant. The store defaults a legacy
// account's services to mail only, so an account that never granted calendar
// has no calendar entry.
func calendarOut(a store.Account) CalendarOut {
	out := CalendarOut{Status: CalendarNotGranted, Scopes: []string{}, InviteEnabled: a.InviteEnabled}
	if a.Provider == store.ProviderMicrosoft && scopes.IsPersonalMicrosoft(a.ProviderUID) {
		out.Degraded = DegradedOwnCalendarOnly
	}
	g, ok := a.Services[store.ServiceCalendar]
	if !ok || len(g.Scopes) == 0 {
		out.MissingScopes = scopes.Missing(a.Provider, store.ServiceCalendar, a.ProviderUID, nil)
		return out
	}
	out.Scopes = append([]string(nil), g.Scopes...)
	if !g.GrantedAt.IsZero() {
		at := g.GrantedAt
		out.GrantedAt = &at
	}
	if missing := scopes.Missing(a.Provider, store.ServiceCalendar, a.ProviderUID, g.Scopes); len(missing) > 0 {
		out.Status, out.MissingScopes = CalendarPartial, missing
		return out
	}
	out.Status = CalendarGranted
	return out
}
