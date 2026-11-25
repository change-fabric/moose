package tools

import (
	"context"
	"errors"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/change-fabric/moose/internal/audit"
	"github.com/change-fabric/moose/internal/email/palette"
	"github.com/change-fabric/moose/internal/email/provider"
	"github.com/change-fabric/moose/internal/email/store"
	"github.com/change-fabric/moose/internal/mcpkit"
)

// ListAccountsIn is the email_list_accounts input.
type ListAccountsIn struct {
	// Account narrows the listing; see Refs. Omitted or "all" lists every account.
	Account Refs `json:"account,omitempty"`
}

// AccountOut is one connected account (R1 and R6 fields). It never carries a
// token.
type AccountOut struct {
	AccountID   string     `json:"account_id"`
	Provider    string     `json:"provider"`
	Address     string     `json:"address"`
	DisplayName string     `json:"display_name,omitempty"`
	Groups      []string   `json:"groups,omitempty"`
	IsDefault   bool       `json:"is_default"`
	ColorIndex  int        `json:"color_index"`
	ColorName   string     `json:"color_name"`
	Status      string     `json:"status"`
	LastSyncAt  *time.Time `json:"last_sync_at,omitempty"`
	LastError   string     `json:"last_error,omitempty"`
	SendEnabled bool       `json:"send_enabled"`
	// The cap fields appear only while sending is enabled; they mean nothing
	// otherwise.
	HourlyCap    *int                  `json:"hourly_cap,omitempty"`
	SentThisHour *int                  `json:"sent_this_hour,omitempty"`
	Capabilities provider.Capabilities `json:"capabilities"`
}

// ListAccountsOut is the email_list_accounts output.
type ListAccountsOut struct {
	Accounts []AccountOut `json:"accounts"`
}

func (d Deps) registerListAccounts(s *sdk.Server) {
	mcpkit.AddTool(s, &sdk.Tool{
		Name: ToolListAccounts,
		Description: "List the connected email accounts: address, status, whether sending is enabled and, when it is, " +
			"the hourly send cap and sends this hour, plus what the provider supports. " +
			"Sending is off until the owner enables it per account on the moose accounts page. " +
			"Pass account (an id or exact address, \"group:<tag>\", or a list) to look at some accounts.",
		InputSchema: &jsonschema.Schema{
			Type:       "object",
			Properties: map[string]*jsonschema.Schema{"account": refsSchema(refsHelp)},
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
		picked, err := Resolve(accts, in.Account)
		if err != nil {
			return resolveError(ctx, err), ListAccountsOut{}, nil
		}
		out := ListAccountsOut{Accounts: make([]AccountOut, 0, len(picked))}
		for _, a := range picked {
			ao, err := d.accountOut(ctx, a)
			if err != nil {
				return mcpkit.ErrorResult(ctx, "internal", "could not read the send counter"), ListAccountsOut{}, nil
			}
			out.Accounts = append(out.Accounts, ao)
		}
		return nil, out, nil
	})
}

// resolveError maps a Resolve failure to a tool error.
func resolveError(ctx context.Context, err error) *sdk.CallToolResult {
	var nf *NotFoundError
	if errors.As(err, &nf) {
		return mcpkit.ErrorResult(ctx, "account_not_found", nf.Error())
	}
	return mcpkit.ErrorResult(ctx, "internal", "could not resolve the account")
}

func (d Deps) accountOut(ctx context.Context, a store.Account) (AccountOut, error) {
	sent, err := d.Store.HourlyCount(ctx, a.ID, d.Now())
	if err != nil {
		return AccountOut{}, err
	}
	return accountOut(a, sent), nil
}

func accountOut(a store.Account, sent int) AccountOut {
	c := palette.At(a.ColorIndex)
	groups := a.Groups
	if groups == nil {
		groups = []string{}
	}
	out := AccountOut{
		AccountID: a.ID, Provider: a.Provider, Address: a.Address, DisplayName: a.DisplayName,
		Groups: groups, IsDefault: a.IsDefault, ColorIndex: a.ColorIndex, ColorName: c.Name, Status: a.Status,
		LastSyncAt: a.LastSyncAt, LastError: a.LastError, SendEnabled: a.SendEnabled, Capabilities: a.Caps,
	}
	if a.SendEnabled {
		cap := a.HourlyCap
		out.HourlyCap, out.SentThisHour = &cap, &sent
	}
	return out
}
