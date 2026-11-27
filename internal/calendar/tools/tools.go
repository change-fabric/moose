// Package tools holds moose-calendar's MCP tool handlers. Tools are provider
// neutral: they work on accounts and the neutral calendar types, never on
// Google Calendar or Microsoft Graph directly. The accounts are the ones
// moose-email's connect page stores; calendar access is a separate grant on each.
package tools

import (
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/change-fabric/moose/internal/accounts/store"
)

// Tool names.
const (
	ToolListAccounts = "calendar_list_accounts"
)

// Instructions is the MCP server's instruction text. Accounts are named by
// their address, the same way moose-email names them.
const Instructions = "Many accounts may be connected. Always name the account by its address when you show or act on calendar data. " +
	"Never guess an account for a write; if needs_account or account_not_found is returned, ask the user and show the candidates. " +
	"Calendar access is granted per account on the moose accounts page; an account whose calendar status is not_granted or partial " +
	"cannot be read until the owner grants it."

// ServerOptions returns the MCP server options for moose-calendar, which carry
// the instructions.
func ServerOptions() *sdk.ServerOptions { return &sdk.ServerOptions{Instructions: Instructions} }

// Deps is what the tools need.
type Deps struct {
	Store store.Store
	// FallbackOwner names the owner when a request carries no bearer token. It
	// is set only for local development without the OAuth server; with OAuth on,
	// a request without a token never reaches a tool.
	FallbackOwner string
	Now           func() time.Time
}

// Register adds the calendar tools to s.
func Register(s *sdk.Server, d Deps) {
	if d.Now == nil {
		d.Now = time.Now
	}
	d.registerListAccounts(s)
}

func (d Deps) owner(req *sdk.CallToolRequest) string {
	if req != nil {
		if ex := req.GetExtra(); ex != nil && ex.TokenInfo != nil && ex.TokenInfo.UserID != "" {
			return ex.TokenInfo.UserID
		}
	}
	return d.FallbackOwner
}
