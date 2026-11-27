package tools

import (
	"context"
	"errors"
	"net/mail"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/change-fabric/moose/internal/accounts/fanout"
	"github.com/change-fabric/moose/internal/accounts/store"
	"github.com/change-fabric/moose/internal/audit"
	"github.com/change-fabric/moose/internal/email/mailmsg"
	"github.com/change-fabric/moose/internal/email/provider"
	"github.com/change-fabric/moose/internal/email/sendflow"
	"github.com/change-fabric/moose/internal/mcpkit"
)

// CreateDraftIn is the email_create_draft input. There is no attachments field:
// the PoC sends plain text only.
type CreateDraftIn struct {
	// AccountID is the sending account, by id or exact address. It may be
	// omitted only to be told which accounts could send a reply.
	AccountID   string   `json:"account_id,omitempty"`
	FromAddress string   `json:"from_address"`
	To          []string `json:"to,omitempty"`
	Cc          []string `json:"cc,omitempty"`
	Bcc         []string `json:"bcc,omitempty"`
	Subject     string   `json:"subject,omitempty"`
	Body        string   `json:"body,omitempty"`
	// InReplyTo is the composite id of the message being answered.
	InReplyTo string `json:"in_reply_to,omitempty"`
}

// Warning is a notice that does not stop the call.
type Warning struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// DraftOut is the email_create_draft and email_update_draft output.
type DraftOut struct {
	// Status is created, updated or needs_account. needs_account means nothing
	// was created: pick an account from Candidates and call again.
	Status string `json:"status"`
	// DraftID is the composite id (acc_...:draft) to pass to email_update_draft
	// and email_request_send.
	DraftID string      `json:"draft_id,omitempty"`
	Account *AccountRef `json:"account,omitempty"`
	From    string      `json:"from,omitempty"`
	To      []string    `json:"to,omitempty"`
	Cc      []string    `json:"cc,omitempty"`
	Bcc     []string    `json:"bcc,omitempty"`
	Subject string      `json:"subject,omitempty"`
	// Suggested is the account that received the message being answered.
	Suggested *AccountRef `json:"suggested,omitempty"`
	// Candidates are the accounts that could send it: ok and send-enabled.
	Candidates []AccountRef `json:"candidates,omitempty"`
	Warnings   []Warning    `json:"warnings,omitempty"`
	Note       string       `json:"note,omitempty"`
}

const draftNote = "The draft is saved in the mailbox only; nothing is sent. Call email_request_send to ask for approval."

func strList(desc string) *jsonschema.Schema {
	return &jsonschema.Schema{Type: "array", Items: &jsonschema.Schema{Type: "string"}, Description: desc}
}

func (d Deps) registerCreateDraft(s *sdk.Server) {
	str := func(desc string) *jsonschema.Schema { return &jsonschema.Schema{Type: "string", Description: desc} }
	mcpkit.AddTool(s, &sdk.Tool{
		Name: ToolCreateDraft,
		Description: "Create a plain-text draft in one account's mailbox. Nothing is sent. Name the account (account_id: its id or exact address) and restate " +
			"its address exactly as from_address; a mismatch is an error. Plain text only: no attachments, no HTML. " +
			"To answer a message pass in_reply_to (its composite id); without account_id you get needs_account with the receiving account " +
			"as `suggested` and the send-enabled accounts as `candidates`, and nothing is created. A reply from a different account than the one that " +
			"received the message works but returns the warning reply_from_differs, and the approval page flags it. " + WriteGuidance,
		InputSchema: &jsonschema.Schema{
			Type:     "object",
			Required: []string{"from_address"},
			Properties: map[string]*jsonschema.Schema{
				"account_id":   str("The sending account: its id or its exact address."),
				"from_address": str("The account's exact address, restated. A nickname or any other text is refused."),
				"to":           strList("Recipient addresses."),
				"cc":           strList("Cc addresses."),
				"bcc":          strList("Bcc addresses."),
				"subject":      str("Subject. For a reply it defaults to \"Re: \" and the original subject."),
				"body":         str("Plain-text body."),
				"in_reply_to":  str("Composite id (acc_...:message) of the message being answered. To defaults to its sender."),
			},
		},
	}, mcpkit.Write(), d.createDraft)
}

// singleAccount resolves one account by id or exact address.
func singleAccount(accts []store.Account, ref string) (store.Account, error) {
	ref = strings.TrimSpace(ref)
	if strings.EqualFold(ref, "all") || strings.HasPrefix(strings.ToLower(ref), "group:") {
		return store.Account{}, errors.New("name exactly one account by id or address")
	}
	picked, err := fanout.Resolve(accts, []string{ref})
	if err != nil {
		return store.Account{}, err
	}
	if len(picked) != 1 {
		return store.Account{}, errors.New("name exactly one account by id or address")
	}
	return picked[0], nil
}

// writeAccount resolves the account of a write tool and checks the restated
// from_address and the account's status. A missing account is a needs_account
// error that lists the accounts a send could come from.
func (d Deps) writeAccount(ctx context.Context, owner, ref, from string) (store.Account, *sdk.CallToolResult) {
	accts, err := d.Store.ListAccounts(ctx, owner)
	if err != nil {
		return store.Account{}, mcpkit.ErrorResult(ctx, "internal", "could not list accounts")
	}
	if strings.TrimSpace(ref) == "" {
		return store.Account{}, mcpkit.ErrorResult(ctx, "needs_account", "name the account (account_id: an id or exact address); candidates: "+strings.Join(fanout.Candidates(sendable(accts)), ", "))
	}
	a, err := singleAccount(accts, ref)
	if err != nil {
		var nf *fanout.NotFoundError
		if errors.As(err, &nf) {
			return store.Account{}, resolveError(ctx, err)
		}
		return store.Account{}, mcpkit.ErrorResult(ctx, "invalid_input", err.Error())
	}
	if !sendflow.FromMatches(a, from) {
		return store.Account{}, mcpkit.ErrorResult(ctx, sendflow.CodeFromMismatch,
			"from_address "+quote(from)+" is not the address of account "+a.ID+" ("+a.Address+"); restate the account's exact address")
	}
	if a.Status != store.StatusOK {
		return store.Account{}, mcpkit.ErrorResult(ctx, "account_unavailable", a.Address+" is "+strings.ReplaceAll(a.Status, "_", " ")+"; reconnect it on the moose accounts page")
	}
	return a, nil
}

func quote(s string) string {
	return "\"" + strings.NewReplacer("\"", "'", "\n", " ", "\r", " ").Replace(clip(s, 80)) + "\""
}

// sendable lists the accounts a message could go out from: ok and send-enabled.
func sendable(accts []store.Account) []store.Account {
	var out []store.Account
	for _, a := range accts {
		if a.Status == store.StatusOK && a.SendEnabled {
			out = append(out, a)
		}
	}
	return out
}

func refs(accts []store.Account) []AccountRef {
	out := make([]AccountRef, 0, len(accts))
	for _, a := range accts {
		out = append(out, refOf(a))
	}
	return out
}

func (d Deps) createDraft(ctx context.Context, req *sdk.CallToolRequest, in CreateDraftIn) (*sdk.CallToolResult, DraftOut, error) {
	audit.FromContext(ctx).SetAction(ToolCreateDraft, "", "")
	owner := d.owner(req)
	if owner == "" {
		return mcpkit.ErrorResult(ctx, "unauthorized", "no signed-in owner for this request"), DraftOut{}, nil
	}
	if d.Providers == nil {
		return mcpkit.ErrorResult(ctx, "internal", "mail access is not configured on this server"), DraftOut{}, nil
	}
	accts, err := d.Store.ListAccounts(ctx, owner)
	if err != nil {
		return mcpkit.ErrorResult(ctx, "internal", "could not list accounts"), DraftOut{}, nil
	}

	// A reply names the message it answers, which fixes the receiving account.
	var (
		orig     provider.Message
		origAcct store.Account
		isReply  bool
	)
	if strings.TrimSpace(in.InReplyTo) != "" {
		isReply = true
		a, p, rawID, bad := d.single(ctx, req, in.InReplyTo)
		if bad != nil {
			return bad, DraftOut{}, nil
		}
		origAcct = a
		if err := d.retry429(ctx, func() (e error) { orig, e = p.GetMessage(ctx, rawID); return }); err != nil {
			return d.providerError(ctx, err), DraftOut{}, nil
		}
		if strings.TrimSpace(in.AccountID) == "" {
			// Nothing is created: the caller must choose the sending account.
			return nil, DraftOut{
				Status: "needs_account", Suggested: ptr(refOf(a)), Candidates: refs(sendable(accts)),
				Note: "Choose the sending account and call again with account_id and from_address. The account that received the message is suggested; it must have sending enabled to be sent from.",
			}, nil
		}
	} else if strings.TrimSpace(in.AccountID) == "" {
		return nil, DraftOut{
			Status: "needs_account", Candidates: refs(sendable(accts)),
			Note: "Name the sending account (account_id) and restate its address as from_address. Nothing was created.",
		}, nil
	}

	a, bad := d.writeAccount(ctx, owner, in.AccountID, in.FromAddress)
	if bad != nil {
		return bad, DraftOut{}, nil
	}
	p, err := d.provider(ctx, a)
	if err != nil {
		return d.providerError(ctx, err), DraftOut{}, nil
	}

	m := mailmsg.Message{From: fromHeader(a), To: in.To, Cc: in.Cc, Bcc: in.Bcc, Subject: in.Subject, Body: in.Body}
	var threadID string
	if isReply {
		if len(m.To) == 0 && len(m.Cc) == 0 && len(m.Bcc) == 0 {
			m.To = []string{orig.From.Email}
		}
		if strings.TrimSpace(m.Subject) == "" {
			m.Subject = replySubject(orig.Subject)
		}
		m.InReplyTo = orig.RFCMessageID
		m.References = strings.TrimSpace(orig.References + " " + orig.RFCMessageID)
		if origAcct.ID == a.ID {
			threadID = orig.ThreadID
		}
	}
	raw, err := mailmsg.Build(m, d.Now())
	if err != nil {
		return draftProblem(ctx, err), DraftOut{}, nil
	}
	var draft provider.Draft
	if err := d.retry429(ctx, func() (e error) {
		draft, e = p.CreateDraft(ctx, provider.DraftInput{Raw: raw, ThreadID: threadID})
		return
	}); err != nil {
		return d.providerError(ctx, err), DraftOut{}, nil
	}
	meta := store.DraftMeta{AccountID: a.ID, DraftID: draft.ID, ThreadID: firstNonEmpty(draft.ThreadID, threadID), Updated: d.Now().UTC()}
	if isReply {
		meta.ReplyAccountID = origAcct.ID
	}
	if err := d.Store.PutDraftMeta(ctx, meta); err != nil {
		return mcpkit.ErrorResult(ctx, "internal", "the draft was created but its notes could not be saved"), DraftOut{}, nil
	}
	out, bad2 := d.draftOut(ctx, "created", a, draft.ID, raw)
	if bad2 != nil {
		return bad2, DraftOut{}, nil
	}
	if isReply && origAcct.ID != a.ID {
		out.Warnings = append(out.Warnings, Warning{Code: "reply_from_differs",
			Message: "This reply goes out from " + a.Address + " but the message was received by " + origAcct.Address + ". The approval page will flag it."})
	}
	return nil, out, nil
}

func ptr[T any](v T) *T { return &v }

func firstNonEmpty(s ...string) string {
	for _, v := range s {
		if v != "" {
			return v
		}
	}
	return ""
}

func fromHeader(a store.Account) string {
	if strings.TrimSpace(a.DisplayName) == "" {
		return a.Address
	}
	return (&mail.Address{Name: a.DisplayName, Address: a.Address}).String()
}

func replySubject(s string) string {
	s = oneLine(s)
	if strings.HasPrefix(strings.ToLower(s), "re:") {
		return s
	}
	return "Re: " + s
}

func draftProblem(ctx context.Context, err error) *sdk.CallToolResult {
	var pr *mailmsg.Problem
	if errors.As(err, &pr) {
		return mcpkit.ErrorResult(ctx, "invalid_input", pr.Msg)
	}
	return mcpkit.ErrorResult(ctx, "internal", "the message could not be built")
}

// draftOut describes a saved draft by re-reading the bytes that were stored.
func (d Deps) draftOut(ctx context.Context, status string, a store.Account, draftID string, raw []byte) (DraftOut, *sdk.CallToolResult) {
	p, err := mailmsg.Parse(raw)
	if err != nil {
		return DraftOut{}, draftProblem(ctx, err)
	}
	addrs := func(l []mail.Address) []string {
		var out []string
		for _, x := range l {
			out = append(out, x.Address)
		}
		return out
	}
	return DraftOut{
		Status: status, DraftID: compositeID(a.ID, draftID), Account: ptr(refOf(a)), From: p.From.Address,
		To: addrs(p.To), Cc: addrs(p.Cc), Bcc: addrs(p.Bcc), Subject: p.Subject, Note: draftNote,
	}, nil
}

// ---- email_update_draft ----

// UpdateDraftIn is the email_update_draft input. Fields left out keep their
// current value; an empty list clears a recipient list.
type UpdateDraftIn struct {
	DraftID     string    `json:"draft_id"`
	AccountID   string    `json:"account_id,omitempty"`
	FromAddress string    `json:"from_address"`
	To          *[]string `json:"to,omitempty"`
	Cc          *[]string `json:"cc,omitempty"`
	Bcc         *[]string `json:"bcc,omitempty"`
	Subject     *string   `json:"subject,omitempty"`
	Body        *string   `json:"body,omitempty"`
}

func (d Deps) registerUpdateDraft(s *sdk.Server) {
	str := func(desc string) *jsonschema.Schema { return &jsonschema.Schema{Type: "string", Description: desc} }
	mcpkit.AddTool(s, &sdk.Tool{
		Name: ToolUpdateDraft,
		Description: "Change a plain-text draft made with email_create_draft: only the fields you pass change. Nothing is sent. " +
			"A send that was already requested for the old text stays tied to that old text; request the send again for the new text. " + WriteGuidance,
		InputSchema: &jsonschema.Schema{
			Type:     "object",
			Required: []string{"draft_id", "from_address"},
			Properties: map[string]*jsonschema.Schema{
				"draft_id":     str("The composite draft id (acc_...:draft) from email_create_draft."),
				"account_id":   str("Optional: the account's id or exact address; it must match the draft's account."),
				"from_address": str("The account's exact address, restated."),
				"to":           strList("Replace the To list. [] clears it."),
				"cc":           strList("Replace the Cc list. [] clears it."),
				"bcc":          strList("Replace the Bcc list. [] clears it."),
				"subject":      str("New subject."),
				"body":         str("New plain-text body."),
			},
		},
	}, mcpkit.Write(), d.updateDraft)
}

func (d Deps) updateDraft(ctx context.Context, req *sdk.CallToolRequest, in UpdateDraftIn) (*sdk.CallToolResult, DraftOut, error) {
	audit.FromContext(ctx).SetAction(ToolUpdateDraft, "", "")
	owner := d.owner(req)
	if owner == "" {
		return mcpkit.ErrorResult(ctx, "unauthorized", "no signed-in owner for this request"), DraftOut{}, nil
	}
	accID, rawID, ok := splitID(in.DraftID)
	if !ok {
		return mcpkit.ErrorResult(ctx, "invalid_input", "draft_id must be a composite id like acc_xxxx:draft_id, as returned by email_create_draft"), DraftOut{}, nil
	}
	if ref := strings.TrimSpace(in.AccountID); ref != "" {
		accts, err := d.Store.ListAccounts(ctx, owner)
		if err != nil {
			return mcpkit.ErrorResult(ctx, "internal", "could not list accounts"), DraftOut{}, nil
		}
		named, err := singleAccount(accts, ref)
		if err != nil {
			var nf *fanout.NotFoundError
			if errors.As(err, &nf) {
				return resolveError(ctx, err), DraftOut{}, nil
			}
			return mcpkit.ErrorResult(ctx, "invalid_input", err.Error()), DraftOut{}, nil
		}
		if named.ID != accID {
			return mcpkit.ErrorResult(ctx, "account_mismatch", "draft "+in.DraftID+" belongs to another account than "+named.Address), DraftOut{}, nil
		}
	}
	a, bad := d.writeAccount(ctx, owner, accID, in.FromAddress)
	if bad != nil {
		return bad, DraftOut{}, nil
	}
	if d.Providers == nil {
		return mcpkit.ErrorResult(ctx, "internal", "mail access is not configured on this server"), DraftOut{}, nil
	}
	p, err := d.provider(ctx, a)
	if err != nil {
		return d.providerError(ctx, err), DraftOut{}, nil
	}
	var raw []byte
	if err := d.retry429(ctx, func() (e error) { raw, e = p.GetDraftRaw(ctx, rawID); return }); err != nil {
		if errors.Is(err, provider.ErrDraftTooLarge) {
			return mcpkit.ErrorResult(ctx, "draft_too_large", "that draft is larger than 256 KB; create a shorter one"), DraftOut{}, nil
		}
		return d.providerError(ctx, err), DraftOut{}, nil
	}
	cur, err := mailmsg.Parse(raw)
	if err != nil {
		return draftProblem(ctx, err), DraftOut{}, nil
	}
	m := cur.Compose()
	m.From = fromHeader(a)
	if in.To != nil {
		m.To = *in.To
	}
	if in.Cc != nil {
		m.Cc = *in.Cc
	}
	if in.Bcc != nil {
		m.Bcc = *in.Bcc
	}
	if in.Subject != nil {
		m.Subject = *in.Subject
	}
	if in.Body != nil {
		m.Body = *in.Body
	}
	next, err := mailmsg.Build(m, d.Now())
	if err != nil {
		return draftProblem(ctx, err), DraftOut{}, nil
	}
	meta, err := d.Store.GetDraftMeta(ctx, a.ID, rawID)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return mcpkit.ErrorResult(ctx, "internal", "could not read the draft notes"), DraftOut{}, nil
	}
	if err := d.retry429(ctx, func() (e error) {
		_, e = p.UpdateDraft(ctx, rawID, provider.DraftInput{Raw: next, ThreadID: meta.ThreadID})
		return
	}); err != nil {
		return d.providerError(ctx, err), DraftOut{}, nil
	}
	meta.AccountID, meta.DraftID, meta.Updated = a.ID, rawID, d.Now().UTC()
	if err := d.Store.PutDraftMeta(ctx, meta); err != nil {
		return mcpkit.ErrorResult(ctx, "internal", "the draft was updated but its notes could not be saved"), DraftOut{}, nil
	}
	out, bad2 := d.draftOut(ctx, "updated", a, rawID, next)
	if bad2 != nil {
		return bad2, DraftOut{}, nil
	}
	if meta.ReplyAccountID != "" && meta.ReplyAccountID != a.ID {
		out.Warnings = append(out.Warnings, Warning{Code: "reply_from_differs", Message: "This is a reply to a message received by another account; the approval page will flag it."})
	}
	return nil, out, nil
}
