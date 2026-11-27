package tools

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/jsonschema-go/jsonschema"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/change-fabric/moose/internal/accounts/fanout"
	"github.com/change-fabric/moose/internal/accounts/store"
	"github.com/change-fabric/moose/internal/audit"
	"github.com/change-fabric/moose/internal/email/provider"
	"github.com/change-fabric/moose/internal/htmltext"
	"github.com/change-fabric/moose/internal/mcpkit"
)

// Caps. They apply to the wrapped output, so the wrapper is never cut off.
const (
	// MaxBody is the most one wrapped message body may take.
	MaxBody = 32 << 10
	// MaxResult is the most body content one tool result may carry.
	MaxResult = 200 << 10
	// bodyRawLimit bounds what is parsed and wrapped before the cap applies.
	bodyRawLimit = 4 * MaxBody
	// minBodyBudget is the least room worth spending on one more body.
	minBodyBudget = 2 << 10

	defaultSearchLimit = 10
	maxSenderLookups   = 10
	maxOmittedIDs      = 500
	// summaryField caps the subject and the snippet in a search result.
	summaryField = 300
	// maxFieldLen caps free-text search fields.
	maxFieldLen = 500
	// hitBudget is the most one search hit is counted to cost in the result cap.
	hitBudget = 1536
)

// AccountRef names an account in a result (R4). Accounts are labelled by
// their address.
type AccountRef struct {
	ID      string `json:"id"`
	Address string `json:"address"`
}

func refOf(a store.Account) AccountRef { return AccountRef{ID: a.ID, Address: a.Address} }

// ---- email_list_mailboxes ----

// ListMailboxesIn is the email_list_mailboxes input.
type ListMailboxesIn struct {
	Account fanout.Refs `json:"account,omitempty"`
}

// MailboxOut is one mailbox (label or folder) of one account.
type MailboxOut struct {
	Account AccountRef `json:"account"`
	ID      string     `json:"id"`
	Name    string     `json:"name"`
	System  bool       `json:"system"`
}

// ListMailboxesOut is the email_list_mailboxes output.
type ListMailboxesOut struct {
	Mailboxes []MailboxOut           `json:"mailboxes"`
	Accounts  []fanout.AccountReport `json:"accounts"`
	Hint      string                 `json:"hint,omitempty"`
}

func (d Deps) registerListMailboxes(s *sdk.Server) {
	mcpkit.AddTool(s, &sdk.Tool{
		Name: ToolListMailboxes,
		Description: "List the mailboxes (labels or folders) of the connected accounts, so a search can be limited to one. " +
			"Omit account to cover every account that is ok; accounts that need reconnecting are reported in `accounts` and skipped.",
		InputSchema: &jsonschema.Schema{
			Type:       "object",
			Properties: map[string]*jsonschema.Schema{"account": fanout.RefsSchema(fanout.RefsHelp)},
		},
	}, mcpkit.ReadOnly(), func(ctx context.Context, req *sdk.CallToolRequest, in ListMailboxesIn) (*sdk.CallToolResult, ListMailboxesOut, error) {
		audit.FromContext(ctx).SetAction(ToolListMailboxes, "", "")
		_, picked, bad := d.pick(ctx, req, in.Account)
		if bad != nil {
			return bad, ListMailboxesOut{}, nil
		}
		results, reports, _ := fanout.Run(ctx, d.Fanout, picked, func(ctx context.Context, a store.Account) (fanout.PerAccountResult[MailboxOut], error) {
			p, err := d.provider(ctx, a)
			if err != nil {
				return fanout.PerAccountResult[MailboxOut]{}, err
			}
			var boxes []provider.Mailbox
			if err := d.retry429(ctx, func() (e error) { boxes, e = p.ListMailboxes(ctx); return }); err != nil {
				return fanout.PerAccountResult[MailboxOut]{}, err
			}
			var r fanout.PerAccountResult[MailboxOut]
			for _, b := range boxes {
				r.Items = append(r.Items, MailboxOut{Account: refOf(a), ID: b.ID, Name: b.Name, System: b.System})
			}
			return r, nil
		})
		out := ListMailboxesOut{Mailboxes: []MailboxOut{}, Accounts: reports, Hint: narrowHint(len(picked))}
		for _, a := range picked {
			out.Mailboxes = append(out.Mailboxes, results[a.ID].Items...)
		}
		return nil, out, nil
	})
}

// ---- email_search ----

// SearchIn is the email_search input. Text fields are literal values, never
// provider syntax.
type SearchIn struct {
	Account       fanout.Refs `json:"account,omitempty"`
	From          string      `json:"from,omitempty"`
	To            string      `json:"to,omitempty"`
	Subject       string      `json:"subject,omitempty"`
	Text          string      `json:"text,omitempty"`
	After         string      `json:"after,omitempty"`
	Before        string      `json:"before,omitempty"`
	HasAttachment bool        `json:"has_attachment,omitempty"`
	Mailbox       string      `json:"mailbox,omitempty"`
	Unread        bool        `json:"unread,omitempty"`
	Limit         int         `json:"limit,omitempty"`
	Cursor        string      `json:"cursor,omitempty"`
}

// MessageSummary is one search hit. Subject and snippet come from outside, so
// they travel inside Content, wrapped as untrusted; every other field is
// either ours or reduced to a plain address.
type MessageSummary struct {
	ID             string     `json:"id"`
	ThreadID       string     `json:"thread_id,omitempty"`
	Account        AccountRef `json:"account"`
	Date           time.Time  `json:"date"`
	Unread         bool       `json:"unread"`
	Mailboxes      []string   `json:"mailboxes,omitempty"`
	From           string     `json:"from"`
	SenderTrust    string     `json:"sender_trust"`
	HasAttachments bool       `json:"has_attachments"`
	Content        string     `json:"content"`
}

// SearchOut is the email_search output (R3 shape).
type SearchOut struct {
	Results  []MessageSummary       `json:"results"`
	Accounts []fanout.AccountReport `json:"accounts"`
	// Cursor resumes the search: pass it back with the same fields. It is
	// absent when every account is exhausted.
	Cursor string `json:"cursor,omitempty"`
	Hint   string `json:"hint,omitempty"`
}

func (d Deps) registerSearch(s *sdk.Server) {
	str := func(desc string) *jsonschema.Schema { return &jsonschema.Schema{Type: "string", Description: desc} }
	mcpkit.AddTool(s, &sdk.Tool{
		Name: ToolSearch,
		Description: "Search mail across accounts. With no account it searches every account that is ok and merges the hits newest first; " +
			"`accounts` says how each account fared (ok, timeout, error, needs_reauth, skipped). If a page is partial or has more, " +
			"pass `cursor` back with the same fields to continue. Each hit's subject and snippet are inside an untrusted wrapper. " +
			contentWrapperNote,
		InputSchema: &jsonschema.Schema{
			Type: "object",
			Properties: map[string]*jsonschema.Schema{
				"account":        fanout.RefsSchema(fanout.RefsHelp),
				"from":           str("Sender address or name contains this."),
				"to":             str("A recipient address or name contains this."),
				"subject":        str("Subject contains this."),
				"text":           str("Free text to find in the message."),
				"after":          str("Only messages after this date (RFC 3339 or YYYY-MM-DD)."),
				"before":         str("Only messages before this date (RFC 3339 or YYYY-MM-DD)."),
				"has_attachment": {Type: "boolean"},
				"mailbox":        str("A mailbox id from email_list_mailboxes, such as INBOX."),
				"unread":         {Type: "boolean"},
				"limit":          {Type: "integer", Description: fmt.Sprintf("Hits per account, 1 to %d. Default %d.", provider.MaxFetch, defaultSearchLimit)},
				"cursor":         str("The cursor from the previous page of this same search."),
			},
		},
	}, mcpkit.ReadOnly(), d.search)
}

func (d Deps) search(ctx context.Context, req *sdk.CallToolRequest, in SearchIn) (*sdk.CallToolResult, SearchOut, error) {
	audit.FromContext(ctx).SetAction(ToolSearch, "", "")
	owner, picked, bad := d.pick(ctx, req, in.Account)
	if bad != nil {
		return bad, SearchOut{}, nil
	}
	q, err := buildQuery(in)
	if err != nil {
		return mcpkit.ErrorResult(ctx, "invalid_input", err.Error()), SearchOut{}, nil
	}
	fp := fanout.Fingerprint(struct {
		F, T, S, X, A, B, M string
		H, U                bool
	}{q.From, q.To, q.Subject, q.Text, in.After, in.Before, q.Mailbox, q.HasAttachment, q.Unread})

	pos := map[string]string{}
	targets := picked
	if in.Cursor != "" {
		c, err := d.openCursor(in.Cursor, owner, fp)
		if err != nil {
			return mcpkit.ErrorResult(ctx, "invalid_cursor", err.Error()), SearchOut{}, nil
		}
		pos, targets = c.Pos, nil
		for _, a := range picked {
			if _, ok := pos[a.ID]; ok {
				targets = append(targets, a)
			}
		}
	}

	live := 0
	for _, a := range targets {
		if a.Status == store.StatusOK {
			live++
		}
	}
	// Size the page so the worst case fits the result cap: a hit costs at most
	// about 1.5 KiB.
	per := q.Limit
	if room := MaxResult * 9 / 10 / hitBudget / max(live, 1); per > room {
		per = max(room, 1)
	}

	results, reports, keep := fanout.Run(ctx, d.Fanout, targets, func(ctx context.Context, a store.Account) (fanout.PerAccountResult[MessageSummary], error) {
		p, err := d.provider(ctx, a)
		if err != nil {
			return fanout.PerAccountResult[MessageSummary]{}, err
		}
		qa := q
		qa.Cursor, qa.Limit = pos[a.ID], per
		var res provider.SearchResult
		if err := d.retry429(ctx, func() (e error) { res, e = p.Search(ctx, qa); return }); err != nil {
			return fanout.PerAccountResult[MessageSummary]{}, err
		}
		tr := d.newTrust(a, nil)
		r := fanout.PerAccountResult[MessageSummary]{Next: res.NextCursor}
		for _, m := range res.Messages {
			d.recordSent(ctx, a, m)
			r.Items = append(r.Items, summarize(a, m, tr.of(ctx, m.From.Email)))
		}
		return r, nil
	})

	out := SearchOut{Results: []MessageSummary{}, Accounts: reports, Hint: narrowHint(len(picked))}
	newPos := map[string]string{}
	// With very many accounts even one hit each can pass the cap. Whole
	// accounts, taken from the end, wait for the next page instead: their hits
	// are not cut, and their position is kept.
	budget := MaxResult * 9 / 10
	for _, a := range targets {
		if r, ok := results[a.ID]; ok {
			cost := 0
			for _, it := range r.Items {
				cost += len(it.Content) + 400
			}
			if cost > budget && len(out.Results) > 0 {
				newPos[a.ID] = pos[a.ID]
				for j := range out.Accounts {
					if out.Accounts[j].AccountID == a.ID {
						out.Accounts[j].Returned, out.Accounts[j].HasMore, out.Accounts[j].Reason = 0, true, "result_size"
					}
				}
				continue
			}
			budget -= cost
			out.Results = append(out.Results, r.Items...)
			if r.Next != "" {
				newPos[a.ID] = r.Next
			}
		} else if keep[a.ID] {
			newPos[a.ID] = pos[a.ID] // a timed-out account keeps its place
		}
	}
	sort.SliceStable(out.Results, func(i, j int) bool {
		a, b := out.Results[i], out.Results[j]
		if !a.Date.Equal(b.Date) {
			return a.Date.After(b.Date)
		}
		return a.ID < b.ID
	})
	if len(newPos) > 0 {
		out.Cursor = d.sealCursor(fanout.CursorBody{Owner: owner, Query: fp, Pos: newPos})
	}
	return nil, out, nil
}

func buildQuery(in SearchIn) (provider.Query, error) {
	for name, v := range map[string]string{"from": in.From, "to": in.To, "subject": in.Subject, "text": in.Text, "mailbox": in.Mailbox} {
		if len(v) > maxFieldLen {
			return provider.Query{}, fmt.Errorf("%s is longer than %d bytes", name, maxFieldLen)
		}
	}
	q := provider.Query{From: in.From, To: in.To, Subject: in.Subject, Text: in.Text, Mailbox: in.Mailbox,
		HasAttachment: in.HasAttachment, Unread: in.Unread}
	var err error
	if q.After, err = parseDate("after", in.After); err != nil {
		return q, err
	}
	if q.Before, err = parseDate("before", in.Before); err != nil {
		return q, err
	}
	switch {
	case in.Limit < 0:
		return q, errors.New("limit must be positive")
	case in.Limit == 0:
		q.Limit = defaultSearchLimit
	default:
		q.Limit = min(in.Limit, provider.MaxFetch)
	}
	return q, nil
}

func parseDate(name, s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, nil
	}
	for _, layout := range []string{time.RFC3339, "2006-01-02"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("%s must be an RFC 3339 time or YYYY-MM-DD, got %q", name, clip(s, 40))
}

func summarize(a store.Account, m provider.Message, trust string) MessageSummary {
	prov := provenance(a, m, trust)
	body := "Subject: " + oneLine(clip(m.Subject, summaryField)) + "\nSnippet: " + oneLine(clip(m.Snippet, summaryField))
	return MessageSummary{
		ID: compositeID(a.ID, m.ID), ThreadID: threadID(a, m), Account: refOf(a), Date: m.Date, Unread: m.Unread,
		Mailboxes: m.Mailboxes, From: plainAddr(m.From.Email), SenderTrust: trust, HasAttachments: len(m.Attachments) > 0,
		Content: mcpkit.Untrusted(prov, body),
	}
}

// ---- email_get_message ----

// GetMessageIn is the email_get_message input.
type GetMessageIn struct {
	ID string `json:"id"`
}

// AttachmentOut describes an attachment without its name, which is outside
// content and appears inside the wrapped text instead.
type AttachmentOut struct {
	ID       string `json:"id"`
	MIMEType string `json:"mime_type,omitempty"`
	Size     int64  `json:"size"`
}

// MessageOut is one full message.
type MessageOut struct {
	ID          string          `json:"id"`
	ThreadID    string          `json:"thread_id,omitempty"`
	Account     AccountRef      `json:"account"`
	Date        time.Time       `json:"date"`
	Unread      bool            `json:"unread"`
	Mailboxes   []string        `json:"mailboxes,omitempty"`
	From        string          `json:"from"`
	To          []string        `json:"to"`
	Cc          []string        `json:"cc"`
	SenderTrust string          `json:"sender_trust"`
	Attachments []AttachmentOut `json:"attachments"`
	// Content is the headers (from, to, cc, date, subject, attachment names)
	// and the text body, HTML converted to text, inside the untrusted wrapper.
	Content   string `json:"content"`
	Truncated bool   `json:"truncated,omitempty"`
	// Omitted means the body was left out to keep the result under its size cap.
	Omitted bool `json:"omitted,omitempty"`
}

func (d Deps) registerGetMessage(s *sdk.Server) {
	mcpkit.AddTool(s, &sdk.Tool{
		Name: ToolGetMessage,
		Description: "Read one message by its id (the composite acc_...:message id a search returns). Headers and the text body " +
			fmt.Sprintf("(HTML converted to text, capped at %d KiB) ", MaxBody>>10) + "come back inside an untrusted wrapper. " + contentWrapperNote,
		InputSchema: &jsonschema.Schema{
			Type:       "object",
			Required:   []string{"id"},
			Properties: map[string]*jsonschema.Schema{"id": {Type: "string", Description: "Composite message id from email_search."}},
		},
	}, mcpkit.ReadOnly(), func(ctx context.Context, req *sdk.CallToolRequest, in GetMessageIn) (*sdk.CallToolResult, MessageOut, error) {
		audit.FromContext(ctx).SetAction(ToolGetMessage, "", "")
		a, p, raw, bad := d.single(ctx, req, in.ID)
		if bad != nil {
			return bad, MessageOut{}, nil
		}
		ctx, cancel := context.WithTimeout(ctx, d.Fanout.Overall)
		defer cancel()
		var m provider.Message
		if err := d.retry429(ctx, func() (e error) { m, e = p.GetMessage(ctx, raw); return }); err != nil {
			return d.providerError(ctx, err), MessageOut{}, nil
		}
		tr := d.newTrust(a, p)
		d.recordSent(ctx, a, m)
		return nil, buildMessage(a, m, tr.of(ctx, m.From.Email), MaxBody), nil
	})
}

// ---- email_get_thread ----

// ThreadOut is a conversation, oldest message first.
type ThreadOut struct {
	ID       string       `json:"id"`
	Account  AccountRef   `json:"account"`
	Messages []MessageOut `json:"messages"`
	// OmittedIDs lists messages left out because the result hit its size cap;
	// read them one by one with email_get_message.
	OmittedIDs []string `json:"omitted_ids,omitempty"`
}

func (d Deps) registerGetThread(s *sdk.Server) {
	mcpkit.AddTool(s, &sdk.Tool{
		Name: ToolGetThread,
		Description: "Read a whole conversation by thread id (the composite acc_...:thread id), oldest message first. " +
			fmt.Sprintf("Each body is capped at %d KiB and the whole result at %d KiB; messages that do not fit are listed in omitted_ids. ", MaxBody>>10, MaxResult>>10) +
			contentWrapperNote,
		InputSchema: &jsonschema.Schema{
			Type:       "object",
			Required:   []string{"id"},
			Properties: map[string]*jsonschema.Schema{"id": {Type: "string", Description: "Composite thread id from email_search or email_get_message."}},
		},
	}, mcpkit.ReadOnly(), func(ctx context.Context, req *sdk.CallToolRequest, in GetMessageIn) (*sdk.CallToolResult, ThreadOut, error) {
		audit.FromContext(ctx).SetAction(ToolGetThread, "", "")
		a, p, raw, bad := d.single(ctx, req, in.ID)
		if bad != nil {
			return bad, ThreadOut{}, nil
		}
		ctx, cancel := context.WithTimeout(ctx, d.Fanout.Overall)
		defer cancel()
		var t provider.Thread
		if err := d.retry429(ctx, func() (e error) { t, e = p.GetThread(ctx, raw); return }); err != nil {
			return d.providerError(ctx, err), ThreadOut{}, nil
		}
		out := ThreadOut{ID: compositeID(a.ID, t.ID), Account: refOf(a), Messages: []MessageOut{}}
		tr := d.newTrust(a, p)
		remaining := MaxResult
		for _, m := range t.Messages {
			d.recordSent(ctx, a, m)
			if remaining < minBodyBudget {
				if len(out.OmittedIDs) < maxOmittedIDs {
					out.OmittedIDs = append(out.OmittedIDs, compositeID(a.ID, m.ID))
				}
				continue
			}
			mo := buildMessage(a, m, tr.of(ctx, m.From.Email), min(MaxBody, remaining))
			remaining -= len(mo.Content)
			out.Messages = append(out.Messages, mo)
		}
		return nil, out, nil
	})
}

// buildMessage wraps one message. maxContent is the cap on the wrapped output.
func buildMessage(a store.Account, m provider.Message, trust string, maxContent int) MessageOut {
	body := m.BodyText
	if strings.TrimSpace(body) == "" && m.BodyHTML != "" {
		body = htmltext.Convert(m.BodyHTML)
	}
	body = strings.ReplaceAll(body, "\r\n", "\n")
	clipped := false
	if len(body) > bodyRawLimit {
		body, clipped = strings.ToValidUTF8(body[:bodyRawLimit], ""), true
	}
	if strings.TrimSpace(body) == "" {
		body = "(no text body)"
	}

	var h strings.Builder
	h.WriteString("From: " + fmtAddr(m.From) + "\n")
	if len(m.To) > 0 {
		h.WriteString("To: " + fmtAddrs(m.To) + "\n")
	}
	if len(m.Cc) > 0 {
		h.WriteString("Cc: " + fmtAddrs(m.Cc) + "\n")
	}
	if !m.Date.IsZero() {
		h.WriteString("Date: " + m.Date.UTC().Format(time.RFC3339) + "\n")
	}
	h.WriteString("Subject: " + oneLine(m.Subject) + "\n")
	atts := make([]AttachmentOut, 0, len(m.Attachments))
	for _, at := range m.Attachments {
		atts = append(atts, AttachmentOut{ID: at.ID, MIMEType: at.MIMEType, Size: at.Size})
		h.WriteString(fmt.Sprintf("Attachment: %s (%s, %d bytes, id %s)\n", oneLine(clip(at.Filename, 200)), oneLine(at.MIMEType), at.Size, oneLine(at.ID)))
	}
	h.WriteString("\n")

	content, cut := mcpkit.UntrustedCapped(provenance(a, m, trust), h.String()+body, maxContent)
	return MessageOut{
		ID: compositeID(a.ID, m.ID), ThreadID: threadID(a, m), Account: refOf(a), Date: m.Date, Unread: m.Unread,
		Mailboxes: m.Mailboxes, From: plainAddr(m.From.Email), To: plainAddrs(m.To), Cc: plainAddrs(m.Cc),
		SenderTrust: trust, Attachments: atts, Content: content, Truncated: cut || clipped,
	}
}

// provenance lists the trusted facts first and the sender's own address last,
// and Provenance strips what could fake another key.
func provenance(a store.Account, m provider.Message, trust string) string {
	return mcpkit.Provenance(
		"sender_trust", trust,
		"account", a.Address,
		"account_id", a.ID,
		"message_id", compositeID(a.ID, m.ID),
		"from", m.From.Email,
	)
}

func threadID(a store.Account, m provider.Message) string {
	if m.ThreadID == "" {
		return ""
	}
	return compositeID(a.ID, m.ThreadID)
}

// ---- helpers shared by the read tools ----

// pick resolves the account argument for a fan-out tool.
func (d Deps) pick(ctx context.Context, req *sdk.CallToolRequest, refs fanout.Refs) (string, []store.Account, *sdk.CallToolResult) {
	owner := d.owner(req)
	if owner == "" {
		return "", nil, mcpkit.ErrorResult(ctx, "unauthorized", "no signed-in owner for this request")
	}
	if d.Providers == nil {
		return "", nil, mcpkit.ErrorResult(ctx, "internal", "mail access is not configured on this server")
	}
	accts, err := d.Store.ListAccounts(ctx, owner)
	if err != nil {
		return "", nil, mcpkit.ErrorResult(ctx, "internal", "could not list accounts")
	}
	picked, err := fanout.Resolve(accts, refs)
	if err != nil {
		return "", nil, resolveError(ctx, err)
	}
	return owner, picked, nil
}

// single resolves a composite id to its account and provider. An id that names
// an account the owner does not have is account_not_found, never a lookup in
// someone else's mailbox.
func (d Deps) single(ctx context.Context, req *sdk.CallToolRequest, id string) (store.Account, provider.Provider, string, *sdk.CallToolResult) {
	owner := d.owner(req)
	if owner == "" {
		return store.Account{}, nil, "", mcpkit.ErrorResult(ctx, "unauthorized", "no signed-in owner for this request")
	}
	if d.Providers == nil {
		return store.Account{}, nil, "", mcpkit.ErrorResult(ctx, "internal", "mail access is not configured on this server")
	}
	acctID, raw, ok := splitID(id)
	if !ok {
		return store.Account{}, nil, "", mcpkit.ErrorResult(ctx, "invalid_input", "id must be a composite id like acc_xxxx:message_id, as returned by email_search")
	}
	accts, err := d.Store.ListAccounts(ctx, owner)
	if err != nil {
		return store.Account{}, nil, "", mcpkit.ErrorResult(ctx, "internal", "could not list accounts")
	}
	var acct *store.Account
	for i := range accts {
		if accts[i].ID == acctID {
			acct = &accts[i]
		}
	}
	if acct == nil {
		return store.Account{}, nil, "", resolveError(ctx, &fanout.NotFoundError{Ref: acctID, Candidates: fanout.Candidates(accts)})
	}
	switch acct.Status {
	case store.StatusOK:
	case store.StatusNeedsReauth:
		return store.Account{}, nil, "", mcpkit.ErrorResult(ctx, "needs_reauth", acct.Address+" needs to be reconnected on the moose accounts page")
	default:
		return store.Account{}, nil, "", mcpkit.ErrorResult(ctx, "account_unavailable", fmt.Sprintf("%s is %s", acct.Address, acct.Status))
	}
	p, err := d.provider(ctx, *acct)
	if err != nil {
		return store.Account{}, nil, "", d.providerError(ctx, err)
	}
	return *acct, p, raw, nil
}

func (d Deps) provider(ctx context.Context, a store.Account) (provider.Provider, error) {
	return d.Providers.Provider(ctx, a)
}

func (d Deps) providerError(ctx context.Context, err error) *sdk.CallToolResult {
	switch {
	case errors.Is(err, provider.ErrNotFound):
		return mcpkit.ErrorResult(ctx, "not_found", "the provider has no such message or thread in that account")
	case errors.Is(err, provider.ErrAuth):
		return mcpkit.ErrorResult(ctx, "needs_reauth", "this account needs to be reconnected on the moose accounts page")
	case errors.Is(err, provider.ErrRateLimited):
		return mcpkit.ErrorResult(ctx, "rate_limited", "the provider is limiting requests for this account; try again shortly")
	case errors.Is(err, context.DeadlineExceeded):
		return mcpkit.ErrorResult(ctx, "timeout", "the provider did not answer in time")
	}
	return mcpkit.ErrorResult(ctx, "provider_error", "the provider returned an error")
}

func narrowHint(n int) string {
	if n > fanout.HintAbove {
		return fmt.Sprintf("%d accounts are in scope. Narrow with account: \"group:<tag>\" or a list of addresses to go faster.", n)
	}
	return ""
}

// ---- sender trust and contact recording ----

// trust resolves sender trust for one account's reads. p is nil in search,
// which never calls the provider for this.
type trust struct {
	d       Deps
	a       store.Account
	p       provider.Provider
	cache   map[string]string
	lookups int
}

func (d Deps) newTrust(a store.Account, p provider.Provider) *trust {
	return &trust{d: d, a: a, p: p, cache: map[string]string{}}
}

// of returns the sender's trust in this account: self, trusted, known, new or
// blocked. A stored contact wins. Otherwise, when a provider is present, the
// account's Sent mail is checked once (up to maxSenderLookups per call): an
// address the account has written to is known. Anything else is new, and a
// new address is recorded so the screener can list it.
func (t *trust) of(ctx context.Context, addr string) string {
	addr = strings.ToLower(strings.TrimSpace(addr))
	if addr == "" {
		return store.TrustNew
	}
	if v, ok := t.cache[addr]; ok {
		return v
	}
	v := t.resolve(ctx, addr)
	t.cache[addr] = v
	return v
}

func (t *trust) resolve(ctx context.Context, addr string) string {
	if strings.EqualFold(addr, t.a.Address) {
		return "self"
	}
	c, err := t.d.Store.GetContact(ctx, t.a.ID, addr)
	if err == nil {
		return c.Trust
	}
	if !errors.Is(err, store.ErrNotFound) {
		return store.TrustNew
	}
	if t.p == nil {
		return store.TrustNew
	}
	if t.lookups < maxSenderLookups {
		t.lookups++
		res, err := t.p.Search(ctx, provider.Query{To: addr, Mailbox: provider.MailboxSent, Limit: 1})
		if err == nil && len(res.Messages) > 0 {
			t.d.putContact(ctx, t.a.ID, addr, store.TrustKnown)
			return store.TrustKnown
		}
		if err == nil {
			t.d.putContact(ctx, t.a.ID, addr, store.TrustNew)
		}
	}
	return store.TrustNew
}

func (d Deps) putContact(ctx context.Context, accountID, addr, trust string) {
	// Bookkeeping must not fail a read, and must never lower a stored trust.
	if c, err := d.Store.GetContact(ctx, accountID, addr); err == nil {
		if !(c.Trust == store.TrustNew && trust == store.TrustKnown) {
			return
		}
		c.Trust, c.Updated = trust, d.Now().UTC()
		_ = d.Store.PutContact(ctx, c)
		return
	}
	_ = d.Store.PutContact(ctx, store.Contact{AccountID: accountID, Address: addr, Trust: trust, Updated: d.Now().UTC()})
}

// recordSent records every recipient of a message in the account's Sent
// mailbox as known: the account has written to them.
func (d Deps) recordSent(ctx context.Context, a store.Account, m provider.Message) {
	sent := false
	for _, b := range m.Mailboxes {
		if b == provider.MailboxSent {
			sent = true
		}
	}
	if !sent {
		return
	}
	n := 0
	for _, list := range [][]provider.Address{m.To, m.Cc} {
		for _, r := range list {
			if addr := strings.ToLower(strings.TrimSpace(r.Email)); addr != "" && !strings.EqualFold(addr, a.Address) && n < 50 {
				d.putContact(ctx, a.ID, addr, store.TrustKnown)
				n++
			}
		}
	}
}

// ---- text helpers ----

// plainAddr reduces an address from outside to characters that cannot carry
// instructions or markup.
func plainAddr(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 254 {
		s = s[:254]
	}
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', strings.ContainsRune("._%+-@", r):
			return r
		}
		return '?'
	}, s)
}

func plainAddrs(list []provider.Address) []string {
	out := make([]string, 0, len(list))
	for _, a := range list {
		out = append(out, plainAddr(a.Email))
	}
	return out
}

func fmtAddr(a provider.Address) string {
	if a.Name == "" {
		return oneLine(a.Email)
	}
	return oneLine(clip(a.Name, 120)) + " <" + oneLine(a.Email) + ">"
}

func fmtAddrs(list []provider.Address) string {
	parts := make([]string, 0, len(list))
	for _, a := range list {
		parts = append(parts, fmtAddr(a))
	}
	return strings.Join(parts, ", ")
}

// oneLine puts s on one line so a header value cannot fake another header.
func oneLine(s string) string {
	return strings.Join(strings.Fields(strings.Map(func(r rune) rune {
		if r == '\r' || r == '\n' || r == ' ' || r == ' ' {
			return ' '
		}
		return r
	}, s)), " ")
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := strings.ToValidUTF8(s[:n], "")
	for !utf8.ValidString(cut) {
		cut = cut[:len(cut)-1]
	}
	return cut + "..."
}
