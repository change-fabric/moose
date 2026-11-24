// Package tools holds moose-email's MCP tool handlers. Tools are provider
// neutral: they work on accounts and the neutral provider types, never on
// Gmail directly.
package tools

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/change-fabric/moose/internal/audit"
	"github.com/change-fabric/moose/internal/email/palette"
	"github.com/change-fabric/moose/internal/email/provider"
	"github.com/change-fabric/moose/internal/email/store"
	"github.com/change-fabric/moose/internal/mcpkit"
)

// ToolListAccounts is the name of the account listing tool.
const ToolListAccounts = "email_list_accounts"

// Deps is what the tools need.
type Deps struct {
	Store store.Store
	// FallbackOwner names the owner when a request carries no bearer token. It
	// is set only for local development without the OAuth server; with OAuth on,
	// a request without a token never reaches a tool.
	FallbackOwner string
	Now           func() time.Time
}

// ListAccountsIn is the email_list_accounts input.
type ListAccountsIn struct {
	// Account narrows the listing to one account, named by id, nickname or
	// exact address. Omitted or "all" lists every account.
	Account string `json:"account,omitempty"`
}

// AccountOut is one connected account (R1 and R6 fields). It never carries a
// token.
type AccountOut struct {
	AccountID    string                `json:"account_id"`
	Provider     string                `json:"provider"`
	Nickname     string                `json:"nickname"`
	Address      string                `json:"address"`
	DisplayName  string                `json:"display_name,omitempty"`
	Groups       []string              `json:"groups"`
	IsDefault    bool                  `json:"is_default"`
	ColorIndex   int                   `json:"color_index"`
	ColorName    string                `json:"color_name"`
	Status       string                `json:"status"`
	LastSyncAt   *time.Time            `json:"last_sync_at,omitempty"`
	LastError    string                `json:"last_error,omitempty"`
	SendEnabled  bool                  `json:"send_enabled"`
	HourlyCap    int                   `json:"hourly_cap"`
	SentThisHour int                   `json:"sent_this_hour"`
	Capabilities provider.Capabilities `json:"capabilities"`
}

// ListAccountsOut is the email_list_accounts output.
type ListAccountsOut struct {
	Accounts []AccountOut `json:"accounts"`
}

// Register adds the slice's tools to s. Only email_list_accounts exists so
// far, and it is read-only.
func Register(s *sdk.Server, d Deps) {
	if d.Now == nil {
		d.Now = time.Now
	}
	mcpkit.AddTool(s, &sdk.Tool{
		Name: ToolListAccounts,
		Description: "List the connected email accounts with their nickname, address, status, whether sending is enabled, " +
			"the hourly send cap and sends this hour, and what the provider supports. " +
			"Pass account (an id, nickname or exact address) to look at one account.",
		InputSchema: &jsonschema.Schema{
			Type: "object",
			Properties: map[string]*jsonschema.Schema{
				"account": {Type: "string", Description: "Account id, nickname or exact address. Omit for every account."},
			},
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
			var nf *NotFoundError
			if errors.As(err, &nf) {
				return mcpkit.ErrorResult(ctx, "account_not_found", nf.Error()), ListAccountsOut{}, nil
			}
			return mcpkit.ErrorResult(ctx, "internal", "could not resolve the account"), ListAccountsOut{}, nil
		}
		now := d.Now()
		out := ListAccountsOut{Accounts: make([]AccountOut, 0, len(picked))}
		for _, a := range picked {
			sent, err := d.Store.HourlyCount(ctx, a.ID, now)
			if err != nil {
				return mcpkit.ErrorResult(ctx, "internal", "could not read the send counter"), ListAccountsOut{}, nil
			}
			out.Accounts = append(out.Accounts, accountOut(a, sent))
		}
		return nil, out, nil
	})
}

func (d Deps) owner(req *sdk.CallToolRequest) string {
	if req != nil {
		if ex := req.GetExtra(); ex != nil && ex.TokenInfo != nil && ex.TokenInfo.UserID != "" {
			return ex.TokenInfo.UserID
		}
	}
	return d.FallbackOwner
}

func accountOut(a store.Account, sent int) AccountOut {
	c := palette.At(a.ColorIndex)
	groups := a.Groups
	if groups == nil {
		groups = []string{}
	}
	return AccountOut{
		AccountID: a.ID, Provider: a.Provider, Nickname: a.Nickname, Address: a.Address, DisplayName: a.DisplayName,
		Groups: groups, IsDefault: a.IsDefault, ColorIndex: a.ColorIndex, ColorName: c.Name, Status: a.Status,
		LastSyncAt: a.LastSyncAt, LastError: a.LastError, SendEnabled: a.SendEnabled, HourlyCap: a.HourlyCap,
		SentThisHour: sent, Capabilities: a.Caps,
	}
}

// NotFoundError says an account reference matched nothing. Candidates lists
// the accounts the caller could have meant; the resolver never guesses.
type NotFoundError struct {
	Ref        string
	Candidates []string
}

func (e *NotFoundError) Error() string {
	msg := fmt.Sprintf("no account matches %q", e.Ref)
	if len(e.Candidates) > 0 {
		msg += "; candidates: " + strings.Join(e.Candidates, ", ")
	}
	return msg
}

// Resolve picks accounts by reference: empty or "all" is every account, and
// anything else must be exactly one account's id, nickname (case-insensitive)
// or address (case-insensitive). It never guesses: a reference that matches
// nothing returns a NotFoundError listing every account as a candidate.
func Resolve(all []store.Account, ref string) ([]store.Account, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" || strings.EqualFold(ref, "all") {
		return all, nil
	}
	_, slug, nickErr := store.NormalizeNickname(ref)
	for _, a := range all {
		if a.ID == ref || strings.EqualFold(a.Address, ref) {
			return []store.Account{a}, nil
		}
		if nickErr == nil {
			if _, s, err := store.NormalizeNickname(a.Nickname); err == nil && s == slug {
				return []store.Account{a}, nil
			}
		}
	}
	cands := make([]string, 0, len(all))
	for _, a := range all {
		cands = append(cands, fmt.Sprintf("%s <%s> (%s)", a.Nickname, a.Address, a.ID))
	}
	sort.Strings(cands)
	return nil, &NotFoundError{Ref: ref, Candidates: cands}
}
