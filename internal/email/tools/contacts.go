package tools

import (
	"context"
	"errors"
	"fmt"
	"net/mail"
	"sort"
	"strings"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/change-fabric/moose/internal/audit"
	"github.com/change-fabric/moose/internal/email/sendflow"
	"github.com/change-fabric/moose/internal/email/store"
	"github.com/change-fabric/moose/internal/mcpkit"
)

// Contact actions.
const (
	ActionTrust = "trust"
	ActionBlock = "block"
	ActionTag   = "tag"
)

const (
	maxContactChanges = 100
	maxContactTags    = 10
	defaultListLimit  = 100
	maxListLimit      = 500
)

// ---- contacts_list ----

// ContactsListIn is the contacts_list input.
type ContactsListIn struct {
	// Account narrows the listing; see Refs. Omitted or "all" covers every account.
	Account Refs   `json:"account,omitempty"`
	Trust   string `json:"trust,omitempty"`
	Tag     string `json:"tag,omitempty"`
	Limit   int    `json:"limit,omitempty"`
}

// ContactOut is one address as one account sees it.
type ContactOut struct {
	Account AccountRef `json:"account"`
	Address string     `json:"address"`
	Trust   string     `json:"trust"`
	Tags    []string   `json:"tags,omitempty"`
	Updated time.Time  `json:"updated"`
}

// ContactsListOut is the contacts_list output.
type ContactsListOut struct {
	Contacts  []ContactOut `json:"contacts"`
	Truncated bool         `json:"truncated"`
}

func (d Deps) registerContactsList(s *sdk.Server) {
	mcpkit.AddTool(s, &sdk.Tool{
		Name: ToolContactsList,
		Description: "List the contacts each account has: an address is new (not seen before), known (the account has written to it or read mail from it), " +
			"trusted (the owner approved it) or blocked. Contacts belong to one account, so the same address can be trusted in one and new in another. " +
			"Filter by trust (new, known, trusted, blocked) and by tag. Omit account to cover every account. Results are capped by limit (default " +
			fmt.Sprint(defaultListLimit) + ", at most " + fmt.Sprint(maxListLimit) + ").",
		InputSchema: &jsonschema.Schema{
			Type: "object",
			Properties: map[string]*jsonschema.Schema{
				"account": refsSchema(refsHelp),
				"trust":   {Type: "string", Enum: []any{store.TrustNew, store.TrustKnown, store.TrustTrusted, store.TrustBlocked}, Description: "Only contacts at this trust level."},
				"tag":     {Type: "string", Description: "Only contacts carrying this tag."},
				"limit":   {Type: "integer", Minimum: ptrF(1), Maximum: ptrF(maxListLimit), Description: "Most contacts to return."},
			},
		},
	}, mcpkit.ReadOnly(), d.contactsList)
}

func ptrF(v float64) *float64 { return &v }

func (d Deps) contactsList(ctx context.Context, req *sdk.CallToolRequest, in ContactsListIn) (*sdk.CallToolResult, ContactsListOut, error) {
	audit.FromContext(ctx).SetAction(ToolContactsList, "", "")
	owner := d.owner(req)
	if owner == "" {
		return mcpkit.ErrorResult(ctx, "unauthorized", "no signed-in owner for this request"), ContactsListOut{}, nil
	}
	f := store.ContactFilter{Trust: strings.ToLower(strings.TrimSpace(in.Trust)), Tag: strings.ToLower(strings.TrimSpace(in.Tag))}
	switch f.Trust {
	case "", store.TrustNew, store.TrustKnown, store.TrustTrusted, store.TrustBlocked:
	default:
		return mcpkit.ErrorResult(ctx, "invalid_input", "trust must be new, known, trusted or blocked"), ContactsListOut{}, nil
	}
	limit := in.Limit
	if limit <= 0 {
		limit = defaultListLimit
	}
	limit = min(limit, maxListLimit)
	accts, err := d.Store.ListAccounts(ctx, owner)
	if err != nil {
		return mcpkit.ErrorResult(ctx, "internal", "could not list accounts"), ContactsListOut{}, nil
	}
	picked, err := Resolve(accts, in.Account)
	if err != nil {
		return resolveError(ctx, err), ContactsListOut{}, nil
	}
	out := ContactsListOut{Contacts: []ContactOut{}}
	for _, a := range picked {
		cs, err := d.Store.ListContacts(ctx, a.ID, f)
		if err != nil {
			return mcpkit.ErrorResult(ctx, "internal", "could not read the contacts of "+a.Address), ContactsListOut{}, nil
		}
		for _, c := range cs {
			if len(out.Contacts) >= limit {
				out.Truncated = true
				return nil, out, nil
			}
			out.Contacts = append(out.Contacts, ContactOut{Account: refOf(a), Address: c.Address, Trust: c.Trust, Tags: c.Tags, Updated: c.Updated})
		}
	}
	return nil, out, nil
}

// ---- contacts_propose ----

// ContactChangeIn is one proposed change.
type ContactChangeIn struct {
	Address string   `json:"address"`
	Action  string   `json:"action"`
	Tags    []string `json:"tags,omitempty"`
}

// ContactsProposeIn is the contacts_propose input: changes for one account.
// address, action and tags together are shorthand for a single change.
type ContactsProposeIn struct {
	AccountID string            `json:"account_id"`
	Changes   []ContactChangeIn `json:"changes,omitempty"`
	Address   string            `json:"address,omitempty"`
	Action    string            `json:"action,omitempty"`
	Tags      []string          `json:"tags,omitempty"`
}

// ContactResult is what happened to one proposed change.
type ContactResult struct {
	Address string `json:"address"`
	Action  string `json:"action"`
	// Status is applied (block and tag take effect at once), pending_approval
	// (a trust change waits for the passkey), already_trusted, or refused.
	Status string   `json:"status"`
	Trust  string   `json:"trust,omitempty"`
	Tags   []string `json:"tags,omitempty"`
	Reason string   `json:"reason,omitempty"`
}

// TrustRequestOut is the one approval that carries every trust change.
type TrustRequestOut struct {
	Status      string     `json:"status"`
	RequestID   string     `json:"request_id"`
	ApprovalURL string     `json:"approval_url,omitempty"`
	ExpiresAt   *time.Time `json:"expires_at,omitempty"`
	Addresses   []string   `json:"addresses"`
}

// ContactsProposeOut is the contacts_propose output.
type ContactsProposeOut struct {
	Account      AccountRef       `json:"account"`
	Results      []ContactResult  `json:"results"`
	TrustRequest *TrustRequestOut `json:"trust_request,omitempty"`
	Note         string           `json:"note,omitempty"`
}

func (d Deps) registerContactsPropose(s *sdk.Server) {
	str := func(desc string) *jsonschema.Schema { return &jsonschema.Schema{Type: "string", Description: desc} }
	action := func() *jsonschema.Schema {
		return &jsonschema.Schema{Type: "string", Enum: []any{ActionTrust, ActionBlock, ActionTag},
			Description: "trust needs the owner's passkey approval; block and tag apply at once."}
	}
	mcpkit.AddTool(s, &sdk.Tool{
		Name: ToolContactsPropose,
		Description: "Change how one account treats some addresses. block marks an address blocked (mail from it is flagged and no draft to it can be sent) and tag adds tags (such as \"vendor\"); both apply at once. " +
			"trust asks the owner to trust addresses: every trust change in the call goes into ONE approval request, and you get its approval_url for the owner's phone. " +
			"After they approve, call contacts_status with the request_id to apply it. A blocked address is never trusted by a request; block always wins. " +
			"Contacts belong to one account, so name it. Pass `changes` for several addresses, or address, action and tags for one.",
		InputSchema: &jsonschema.Schema{
			Type:     "object",
			Required: []string{"account_id"},
			Properties: map[string]*jsonschema.Schema{
				"account_id": str("The account whose contacts change: its id or exact address."),
				"changes": {Type: "array", Description: "Changes to make, at most 100.", Items: &jsonschema.Schema{
					Type: "object", Required: []string{"address", "action"},
					Properties: map[string]*jsonschema.Schema{
						"address": str("The email address."),
						"action":  action(),
						"tags":    strList("Tags to add; only with action tag."),
					},
				}},
				"address": str("One address, as a shorthand for a single change."),
				"action":  action(),
				"tags":    strList("Tags to add; only with action tag."),
			},
		},
	}, mcpkit.Write(), d.contactsPropose)
}

type cleanChange struct {
	address, action string
	tags            []string
}

// cleanChanges validates every change before any is applied.
func cleanChanges(a store.Account, in ContactsProposeIn) ([]cleanChange, error) {
	raw := append([]ContactChangeIn(nil), in.Changes...)
	if in.Address != "" || in.Action != "" || len(in.Tags) > 0 {
		raw = append(raw, ContactChangeIn{Address: in.Address, Action: in.Action, Tags: in.Tags})
	}
	if len(raw) == 0 {
		return nil, errors.New("name at least one change: address and action, or changes")
	}
	if len(raw) > maxContactChanges {
		return nil, fmt.Errorf("at most %d changes per call", maxContactChanges)
	}
	seen := map[string]bool{}
	out := make([]cleanChange, 0, len(raw))
	for _, c := range raw {
		addr, err := cleanAddress(c.Address)
		if err != nil {
			return nil, err
		}
		if addr == a.Address {
			return nil, fmt.Errorf("%s is the account's own address; contacts are other people", addr)
		}
		if seen[addr] {
			return nil, fmt.Errorf("%s is listed twice; one change per address", addr)
		}
		seen[addr] = true
		act := strings.ToLower(strings.TrimSpace(c.Action))
		cc := cleanChange{address: addr, action: act}
		switch act {
		case ActionTrust, ActionBlock:
			if len(c.Tags) > 0 {
				return nil, fmt.Errorf("tags go with action tag, not %s (%s)", act, addr)
			}
		case ActionTag:
			if cc.tags, err = normalizeTags(c.Tags); err != nil {
				return nil, fmt.Errorf("%s: %w", addr, err)
			}
			if len(cc.tags) == 0 {
				return nil, fmt.Errorf("%s: action tag needs at least one tag", addr)
			}
		default:
			return nil, fmt.Errorf("action %q for %s must be trust, block or tag", clip(c.Action, 40), addr)
		}
		out = append(out, cc)
	}
	return out, nil
}

func cleanAddress(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	p, err := mail.ParseAddress(raw)
	if err != nil || p.Name != "" || !strings.EqualFold(p.Address, raw) || strings.ContainsAny(raw, " \t\r\n<>,;") {
		return "", fmt.Errorf("%q is not a plain email address like name@example.com", clip(raw, 80))
	}
	return strings.ToLower(p.Address), nil
}

func normalizeTags(in []string) ([]string, error) {
	out := make([]string, 0, len(in))
	seen := map[string]bool{}
	for _, t := range in {
		t = strings.ToLower(strings.TrimSpace(t))
		if !groupTag.MatchString(t) {
			return nil, fmt.Errorf("tag %q must be lowercase letters, digits, dash or underscore, 1 to 32 characters", clip(t, 40))
		}
		if !seen[t] {
			seen[t] = true
			out = append(out, t)
		}
	}
	sort.Strings(out)
	return out, nil
}

func (d Deps) contactsPropose(ctx context.Context, req *sdk.CallToolRequest, in ContactsProposeIn) (*sdk.CallToolResult, ContactsProposeOut, error) {
	audit.FromContext(ctx).SetAction(ToolContactsPropose, "", "")
	owner := d.owner(req)
	if owner == "" {
		return mcpkit.ErrorResult(ctx, "unauthorized", "no signed-in owner for this request"), ContactsProposeOut{}, nil
	}
	accts, err := d.Store.ListAccounts(ctx, owner)
	if err != nil {
		return mcpkit.ErrorResult(ctx, "internal", "could not list accounts"), ContactsProposeOut{}, nil
	}
	if strings.TrimSpace(in.AccountID) == "" {
		return mcpkit.ErrorResult(ctx, "needs_account", "name the account whose contacts change (account_id: an id or exact address); candidates: "+strings.Join(candidates(accts), ", ")), ContactsProposeOut{}, nil
	}
	a, err := singleAccount(accts, in.AccountID)
	if err != nil {
		var nf *NotFoundError
		if errors.As(err, &nf) {
			return resolveError(ctx, err), ContactsProposeOut{}, nil
		}
		return mcpkit.ErrorResult(ctx, "invalid_input", err.Error()), ContactsProposeOut{}, nil
	}
	if a.Status == store.StatusDisconnected {
		return mcpkit.ErrorResult(ctx, "account_unavailable", a.Address+" is disconnected"), ContactsProposeOut{}, nil
	}
	changes, err := cleanChanges(a, in)
	if err != nil {
		return mcpkit.ErrorResult(ctx, "invalid_input", err.Error()), ContactsProposeOut{}, nil
	}
	out := ContactsProposeOut{Account: refOf(a), Results: make([]ContactResult, 0, len(changes))}

	// Trust changes first: they need the approval service, and a failure there
	// must not leave the direct changes half done.
	var trust []string
	for _, c := range changes {
		if c.action == ActionTrust {
			trust = append(trust, c.address)
		}
	}
	var prop sendflow.TrustProposal
	if len(trust) > 0 {
		if bad := d.needSend(ctx); bad != nil {
			return bad, ContactsProposeOut{}, nil
		}
		cctx, cancel := context.WithTimeout(ctx, 25*time.Second)
		defer cancel()
		if prop, err = d.Send.ProposeTrust(cctx, a, trust); err != nil {
			return sendError(ctx, err), ContactsProposeOut{}, nil
		}
		if len(prop.Addresses) > 0 {
			out.TrustRequest = &TrustRequestOut{Status: prop.Status, RequestID: prop.RequestID, ApprovalURL: prop.ApprovalURL, ExpiresAt: prop.ExpiresAt, Addresses: prop.Addresses}
		}
	}
	inReq, already, blocked := set(prop.Addresses), set(prop.AlreadyTrusted), set(prop.Blocked)
	for _, c := range changes {
		r := ContactResult{Address: c.address, Action: c.action}
		switch c.action {
		case ActionTrust:
			switch {
			case already[c.address]:
				r.Status, r.Trust = "already_trusted", store.TrustTrusted
			case blocked[c.address]:
				r.Status, r.Trust, r.Reason = "refused", store.TrustBlocked, "the address is blocked, and a trust request never lifts a block"
			case inReq[c.address]:
				r.Status, r.Reason = "pending_approval", "waiting for the owner's passkey approval"
			}
		case ActionBlock, ActionTag:
			ct, err := d.applyDirect(ctx, a, c)
			switch {
			case errors.Is(err, errTooManyTags):
				r.Status, r.Reason = "refused", err.Error()
			case err != nil:
				return mcpkit.ErrorResult(ctx, "internal", "could not save the contact "+c.address), ContactsProposeOut{}, nil
			default:
				r.Status, r.Trust, r.Tags = "applied", ct.Trust, ct.Tags
			}
		}
		out.Results = append(out.Results, r)
	}
	if out.TrustRequest != nil && (out.TrustRequest.Status == "pending" || out.TrustRequest.Status == "approved") {
		out.Note = "Give the approval_url to the owner to open on their phone; they approve with their passkey. Then call contacts_status with the request_id to apply the trust changes."
	}
	return nil, out, nil
}

func set(l []string) map[string]bool {
	m := make(map[string]bool, len(l))
	for _, v := range l {
		m[v] = true
	}
	return m
}

var errTooManyTags = fmt.Errorf("a contact carries at most %d tags", maxContactTags)

// applyDirect applies a block or tag change, which needs no approval.
func (d Deps) applyDirect(ctx context.Context, a store.Account, c cleanChange) (store.Contact, error) {
	cur, err := d.Store.GetContact(ctx, a.ID, c.address)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return store.Contact{}, err
	}
	if errors.Is(err, store.ErrNotFound) {
		cur = store.Contact{AccountID: a.ID, Address: c.address, Trust: store.TrustNew}
	}
	switch c.action {
	case ActionBlock:
		cur.Trust = store.TrustBlocked
	case ActionTag:
		merged, _ := normalizeTags(append(append([]string(nil), cur.Tags...), c.tags...))
		if len(merged) > maxContactTags {
			return store.Contact{}, errTooManyTags
		}
		cur.Tags = merged
	}
	cur.Updated = d.Now().UTC()
	if err := d.Store.PutContact(ctx, cur); err != nil {
		return store.Contact{}, err
	}
	return cur, nil
}

// ---- contacts_status ----

// ContactsStatusIn is the contacts_status input.
type ContactsStatusIn struct {
	RequestID string `json:"request_id"`
}

func (d Deps) registerContactsStatus(s *sdk.Server) {
	mcpkit.AddTool(s, &sdk.Tool{
		Name: ToolContactsStatus,
		Description: "Check a trust request from contacts_propose and, once the owner has approved it, apply it. Every approved change lands together or none does. " +
			"Returns pending (still waiting for the passkey), applied (the addresses are now trusted), denied or expired. Calling it again after applied changes nothing.",
		InputSchema: &jsonschema.Schema{
			Type:     "object",
			Required: []string{"request_id"},
			Properties: map[string]*jsonschema.Schema{
				"request_id": {Type: "string", Description: "The request_id from contacts_propose."},
			},
		},
	}, mcpkit.Write(), d.contactsStatus)
}

func (d Deps) contactsStatus(ctx context.Context, req *sdk.CallToolRequest, in ContactsStatusIn) (*sdk.CallToolResult, sendflow.ContactsStatusOut, error) {
	audit.FromContext(ctx).SetAction(ToolContactsStatus, "", "")
	owner := d.owner(req)
	if owner == "" {
		return mcpkit.ErrorResult(ctx, "unauthorized", "no signed-in owner for this request"), sendflow.ContactsStatusOut{}, nil
	}
	if bad := d.needSend(ctx); bad != nil {
		return bad, sendflow.ContactsStatusOut{}, nil
	}
	if strings.TrimSpace(in.RequestID) == "" {
		return mcpkit.ErrorResult(ctx, "invalid_input", "request_id is required"), sendflow.ContactsStatusOut{}, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()
	out, err := d.Send.ContactsStatus(ctx, owner, strings.TrimSpace(in.RequestID))
	if err != nil {
		return sendError(ctx, err), sendflow.ContactsStatusOut{}, nil
	}
	return nil, out, nil
}
