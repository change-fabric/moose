package tools

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/change-fabric/moose/internal/accounts/fanout"
	"github.com/change-fabric/moose/internal/audit"
	"github.com/change-fabric/moose/internal/mcpkit"
)

const maxGroups = 10

var groupTag = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)

// AccountUpdateIn is the email_account_update input. It has no field for
// send_enabled, the colour or anything else that affects what an account can
// do: those change only on the moose accounts page.
type AccountUpdateIn struct {
	Account   string    `json:"account"`
	Groups    *[]string `json:"groups,omitempty"`
	IsDefault *bool     `json:"is_default,omitempty"`
}

func (d Deps) registerAccountUpdate(s *sdk.Server) {
	mcpkit.AddTool(s, &sdk.Tool{
		Name: ToolAccountUpdate,
		Description: "Organize one account: set its groups (tags such as \"work\"; a tool's account argument can then say \"group:work\") " +
			"and whether it is the default account. This needs no approval because it cannot change what the account may do: " +
			"it never touches send_enabled or the colour. Name the account by id or exact address.",
		InputSchema: &jsonschema.Schema{
			Type:                 "object",
			Required:             []string{"account"},
			AdditionalProperties: &jsonschema.Schema{Not: &jsonschema.Schema{}},
			Properties: map[string]*jsonschema.Schema{
				"account": {Type: "string", Description: "Account id or exact address."},
				"groups": {Type: "array", Items: &jsonschema.Schema{Type: "string"},
					Description: "Replace the account's groups. Lowercase letters, digits, dash and underscore, up to 32 characters each. [] clears them."},
				"is_default": {Type: "boolean", Description: "true makes this the default account. The default cannot be unset; make another account the default instead."},
			},
		},
	}, mcpkit.Write(), func(ctx context.Context, req *sdk.CallToolRequest, in AccountUpdateIn) (*sdk.CallToolResult, AccountOut, error) {
		audit.FromContext(ctx).SetAction(ToolAccountUpdate, "", "")
		owner := d.owner(req)
		if owner == "" {
			return mcpkit.ErrorResult(ctx, "unauthorized", "no signed-in owner for this request"), AccountOut{}, nil
		}
		if in.Groups == nil && in.IsDefault == nil {
			return mcpkit.ErrorResult(ctx, "invalid_input", "pass groups, is_default or both"), AccountOut{}, nil
		}
		accts, err := d.Store.ListAccounts(ctx, owner)
		if err != nil {
			return mcpkit.ErrorResult(ctx, "internal", "could not list accounts"), AccountOut{}, nil
		}
		ref := strings.TrimSpace(in.Account)
		if ref == "" || strings.EqualFold(ref, "all") || strings.HasPrefix(strings.ToLower(ref), "group:") {
			return mcpkit.ErrorResult(ctx, "invalid_input", "name exactly one account by id or address"), AccountOut{}, nil
		}
		picked, err := fanout.Resolve(accts, []string{ref})
		if err != nil {
			return resolveError(ctx, err), AccountOut{}, nil
		}
		a := picked[0]
		if in.IsDefault != nil && !*in.IsDefault {
			if a.IsDefault {
				return mcpkit.ErrorResult(ctx, "invalid_input", a.Address+" is the default account; make another account the default instead"), AccountOut{}, nil
			}
		}
		if in.Groups != nil {
			groups, err := normalizeGroups(*in.Groups)
			if err != nil {
				return mcpkit.ErrorResult(ctx, "invalid_input", err.Error()), AccountOut{}, nil
			}
			a.Groups, a.Updated = groups, d.Now().UTC()
			if err := d.Store.UpdateAccount(ctx, a); err != nil {
				return mcpkit.ErrorResult(ctx, "internal", "could not save the groups"), AccountOut{}, nil
			}
		}
		if in.IsDefault != nil && *in.IsDefault && !a.IsDefault {
			if err := d.Store.SetDefaultAccount(ctx, owner, a.ID); err != nil {
				return mcpkit.ErrorResult(ctx, "internal", "could not set the default account"), AccountOut{}, nil
			}
		}
		fresh, err := d.Store.GetAccount(ctx, owner, a.ID)
		if err != nil {
			return mcpkit.ErrorResult(ctx, "internal", "could not read the account back"), AccountOut{}, nil
		}
		out, err := d.accountOut(ctx, fresh)
		if err != nil {
			return mcpkit.ErrorResult(ctx, "internal", "could not read the send counter"), AccountOut{}, nil
		}
		return nil, out, nil
	})
}

// normalizeGroups lowercases, validates and deduplicates tags.
func normalizeGroups(in []string) ([]string, error) {
	out := make([]string, 0, len(in))
	seen := map[string]bool{}
	for _, g := range in {
		g = strings.ToLower(strings.TrimSpace(g))
		if !groupTag.MatchString(g) {
			return nil, fmt.Errorf("group %q must be lowercase letters, digits, dash or underscore, 1 to 32 characters", g)
		}
		if !seen[g] {
			seen[g] = true
			out = append(out, g)
		}
	}
	if len(out) > maxGroups {
		return nil, fmt.Errorf("at most %d groups per account", maxGroups)
	}
	return out, nil
}
