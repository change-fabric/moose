// Package graph implements the read side of provider.Provider on Microsoft
// Graph (Exchange Online and Outlook.com mail): mailboxes, search, messages,
// threads by conversationId, and attachment streaming. Drafts and send land in
// a later phase and answer provider.ErrUnsupported until then.
//
// Nothing here reaches users yet: no connect flow builds a Client, and the
// whole Microsoft path stays behind email.microsoft.enabled=false.
package graph

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/change-fabric/moose/internal/email/provider"
	"github.com/change-fabric/moose/internal/graphapi"
)

const (
	defaultLimit = 25
	maxLimit     = provider.MaxFetch
	// maxThreadPages and threadPage bound GetThread: 4 pages of 50 messages.
	maxThreadPages = 4
	threadPage     = 50
)

// wellKnown maps neutral system mailbox ids to Graph's well-known folder names.
var wellKnown = []struct{ neutral, alias string }{
	{provider.MailboxInbox, "inbox"},
	{provider.MailboxSent, "sentitems"},
	{provider.MailboxDraft, "drafts"},
	{provider.MailboxTrash, "deleteditems"},
	{provider.MailboxSpam, "junkemail"},
}

func aliasFor(neutral string) (string, bool) {
	for _, w := range wellKnown {
		if w.neutral == neutral {
			return w.alias, true
		}
	}
	return "", false
}

const messageSelect = "id,conversationId,parentFolderId,from,toRecipients,ccRecipients,subject," +
	"receivedDateTime,sentDateTime,bodyPreview,internetMessageId,isRead,hasAttachments"

const fullSelect = messageSelect + ",body,internetMessageHeaders"

const attachmentSelect = "id,name,contentType,size,isInline"

// Client is one connected Microsoft mailbox.
type Client struct {
	g *graphapi.Client

	mu     sync.Mutex
	system map[string]string // folder id -> neutral id, filled on first use
}

var _ provider.Provider = (*Client)(nil)

// New builds a Client over a Graph HTTP client (graphapi.New for production, or
// one with BaseURL pointed at an httptest server).
func New(g *graphapi.Client) *Client { return &Client{g: g} }

// Capabilities reports Graph's features: conversations are native, there are
// folders rather than labels, search is $search/$filter, and sending goes
// through a draft Exchange re-serialises (semantic match plus verify).
func (c *Client) Capabilities() provider.Capabilities {
	return provider.Capabilities{Threads: true, Labels: false, SearchDialect: provider.DialectGraph, NativeDraftSend: true, ReadOnly: true}
}

// systemIDs returns the folder id to neutral id map for the five system
// folders, resolving them once per Client. Failures are not cached.
func (c *Client) systemIDs(ctx context.Context) (map[string]string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.system != nil {
		return c.system, nil
	}
	m := make(map[string]string, len(wellKnown))
	for _, w := range wellKnown {
		var f struct {
			ID string `json:"id"`
		}
		if err := c.g.GetJSON(ctx, "/me/mailFolders/"+w.alias, url.Values{"$select": {"id"}}, nil, &f); err != nil {
			return nil, err
		}
		if f.ID != "" {
			m[f.ID] = w.neutral
		}
	}
	c.system = m
	return m, nil
}

type folder struct {
	ID          string `json:"id"`
	DisplayName string `json:"displayName"`
}

// ListMailboxes returns the five system folders under their neutral ids, then
// the user's top-level folders under their Graph ids.
func (c *Client) ListMailboxes(ctx context.Context) ([]provider.Mailbox, error) {
	sys, err := c.systemIDs(ctx)
	if err != nil {
		return nil, err
	}
	var all []folder
	q := url.Values{"$top": {"100"}, "$select": {"id,displayName"}}
	for page := 0; page < 10; page++ {
		var res struct {
			Value []folder `json:"value"`
			Next  string   `json:"@odata.nextLink"`
		}
		if err := c.g.GetJSON(ctx, "/me/mailFolders", q, nil, &res); err != nil {
			return nil, err
		}
		all = append(all, res.Value...)
		if res.Next == "" {
			break
		}
		skip, err := pagingParams(res.Next)
		if err != nil {
			return nil, err
		}
		for k, v := range skip {
			q[k] = v
		}
	}
	byID := map[string]folder{}
	for _, f := range all {
		byID[f.ID] = f
	}
	out := make([]provider.Mailbox, 0, len(all)+len(wellKnown))
	for _, w := range wellKnown {
		for id, n := range sys {
			if n != w.neutral {
				continue
			}
			name := w.alias
			if f, ok := byID[id]; ok && f.DisplayName != "" {
				name = f.DisplayName
			}
			out = append(out, provider.Mailbox{ID: n, Name: name, System: true})
		}
	}
	for _, f := range all {
		if _, isSys := sys[f.ID]; !isSys {
			out = append(out, provider.Mailbox{ID: f.ID, Name: f.DisplayName})
		}
	}
	return out, nil
}

// messagesPath is the collection a query runs against.
func messagesPath(mailbox string) string {
	if mailbox == "" {
		return "/me/messages"
	}
	if alias, ok := aliasFor(strings.ToUpper(mailbox)); ok {
		return "/me/mailFolders/" + alias + "/messages"
	}
	return "/me/mailFolders/" + graphapi.Segment(mailbox) + "/messages"
}

// Search runs q. The cursor carries only Graph's paging token; the rest of the
// request is rebuilt from q, so a cursor cannot change what is searched.
func (c *Client) Search(ctx context.Context, q provider.Query) (provider.SearchResult, error) {
	compiled, err := CompileQuery(q)
	if err != nil {
		return provider.SearchResult{}, err
	}
	limit := q.Limit
	switch {
	case limit <= 0:
		limit = defaultLimit
	case limit > maxLimit:
		limit = maxLimit
	}
	params := url.Values{"$top": {fmt.Sprint(limit)}, "$select": {messageSelect}}
	if compiled.Search != "" {
		params.Set("$search", `"`+strings.ReplaceAll(compiled.Search, `"`, `\"`)+`"`)
	} else {
		params.Set("$filter", compiled.Filter)
		params.Set("$orderby", compiled.OrderBy)
	}
	if q.Cursor != "" {
		paging, err := decodeCursor(q.Cursor)
		if err != nil {
			return provider.SearchResult{}, err
		}
		for k, v := range paging {
			params[k] = v
		}
	}
	sys, err := c.systemIDs(ctx)
	if err != nil {
		return provider.SearchResult{}, err
	}
	var res struct {
		Value []message `json:"value"`
		Next  string    `json:"@odata.nextLink"`
	}
	if err := c.g.GetJSON(ctx, messagesPath(q.Mailbox), params, nil, &res); err != nil {
		return provider.SearchResult{}, err
	}
	out := provider.SearchResult{}
	for _, m := range res.Value {
		out.Messages = append(out.Messages, m.convert(sys))
	}
	if res.Next != "" {
		paging, err := pagingParams(res.Next)
		if err != nil {
			return provider.SearchResult{}, err
		}
		out.NextCursor = paging.Encode()
	}
	return out, nil
}

// GetMessage returns one message with its body, attachment list and References.
func (c *Client) GetMessage(ctx context.Context, id string) (provider.Message, error) {
	sys, err := c.systemIDs(ctx)
	if err != nil {
		return provider.Message{}, err
	}
	var m message
	q := url.Values{"$select": {fullSelect}, "$expand": {"attachments($select=" + attachmentSelect + ")"}}
	if err := c.g.GetJSON(ctx, "/me/messages/"+graphapi.Segment(id), q, nil, &m); err != nil {
		return provider.Message{}, err
	}
	return m.convert(sys), nil
}

// GetThread returns the conversation oldest first. The id is a conversationId.
// $orderby is not used with the conversationId filter (Exchange rejects it as
// an InefficientFilter), so the messages are sorted here.
func (c *Client) GetThread(ctx context.Context, id string) (provider.Thread, error) {
	sys, err := c.systemIDs(ctx)
	if err != nil {
		return provider.Thread{}, err
	}
	q := url.Values{
		"$filter": {"conversationId eq " + odataString(clean(id))},
		"$top":    {fmt.Sprint(threadPage)},
		"$select": {fullSelect},
		"$expand": {"attachments($select=" + attachmentSelect + ")"},
	}
	var msgs []message
	for page := 0; page < maxThreadPages; page++ {
		var res struct {
			Value []message `json:"value"`
			Next  string    `json:"@odata.nextLink"`
		}
		if err := c.g.GetJSON(ctx, "/me/messages", q, nil, &res); err != nil {
			return provider.Thread{}, err
		}
		msgs = append(msgs, res.Value...)
		if res.Next == "" {
			break
		}
		paging, err := pagingParams(res.Next)
		if err != nil {
			return provider.Thread{}, err
		}
		for k, v := range paging {
			q[k] = v
		}
	}
	if len(msgs) == 0 {
		return provider.Thread{}, fmt.Errorf("%w: no messages in conversation", provider.ErrNotFound)
	}
	out := provider.Thread{ID: id}
	for _, m := range msgs {
		out.Messages = append(out.Messages, m.convert(sys))
	}
	sort.SliceStable(out.Messages, func(i, j int) bool { return out.Messages[i].Date.Before(out.Messages[j].Date) })
	return out, nil
}

type attachment struct {
	Type        string `json:"@odata.type"`
	ID          string `json:"id"`
	Name        string `json:"name"`
	ContentType string `json:"contentType"`
	Size        int64  `json:"size"`
}

// OpenAttachment streams one file attachment from /attachments/{id}/$value. An
// attachment over provider.MaxAttachment is ErrTooLarge and is not downloaded;
// a stream that turns out larger than declared is cut off with ErrTooLarge.
func (c *Client) OpenAttachment(ctx context.Context, messageID, attachmentID string) (io.ReadCloser, provider.AttachmentMeta, error) {
	base := "/me/messages/" + graphapi.Segment(messageID) + "/attachments/" + graphapi.Segment(attachmentID)
	var a attachment
	if err := c.g.GetJSON(ctx, base, url.Values{"$select": {attachmentSelect}}, nil, &a); err != nil {
		return nil, provider.AttachmentMeta{}, err
	}
	meta := provider.AttachmentMeta{ID: a.ID, Filename: a.Name, MIMEType: a.ContentType, Size: a.Size}
	if a.Type == "#microsoft.graph.referenceAttachment" {
		return nil, meta, fmt.Errorf("%w: reference attachments are links, not content", provider.ErrRejected)
	}
	if a.Size > provider.MaxAttachment {
		return nil, meta, fmt.Errorf("%w: %d bytes", provider.ErrTooLarge, a.Size)
	}
	resp, err := c.g.Do(ctx, graphapi.Request{Path: base + "/$value", Header: http.Header{"Accept": {"*/*"}}})
	if err != nil {
		return nil, meta, err
	}
	if resp.ContentLength > provider.MaxAttachment {
		resp.Body.Close()
		return nil, meta, fmt.Errorf("%w: %d bytes", provider.ErrTooLarge, resp.ContentLength)
	}
	return &capped{rc: resp.Body, left: provider.MaxAttachment}, meta, nil
}

// capped fails a read once more than left bytes have been returned.
type capped struct {
	rc   io.ReadCloser
	left int64
}

func (c *capped) Read(p []byte) (int, error) {
	if c.left < 0 {
		return 0, provider.ErrTooLarge
	}
	if int64(len(p)) > c.left+1 {
		p = p[:c.left+1]
	}
	n, err := c.rc.Read(p)
	c.left -= int64(n)
	if c.left < 0 {
		return n - 1, provider.ErrTooLarge
	}
	return n, err
}

func (c *capped) Close() error { return c.rc.Close() }

// RawMessage returns the message as RFC822 bytes from /$value. Exchange adds
// X-MS-* and Thread-Index noise, so callers parse it, they do not compare it
// byte for byte. A message over provider.MaxDraftRaw is ErrDraftTooLarge.
func (c *Client) RawMessage(ctx context.Context, id string) ([]byte, error) {
	resp, err := c.g.Do(ctx, graphapi.Request{Path: "/me/messages/" + graphapi.Segment(id) + "/$value", Header: http.Header{"Accept": {"*/*"}}})
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, provider.MaxDraftRaw+1))
	if err != nil {
		return nil, fmt.Errorf("graph: read message: %w", err)
	}
	if len(raw) > provider.MaxDraftRaw {
		return nil, provider.ErrDraftTooLarge
	}
	return raw, nil
}

func unsupported(op string) error { return fmt.Errorf("graph %s: %w", op, provider.ErrUnsupported) }

func (c *Client) CreateDraft(context.Context, provider.DraftInput) (provider.Draft, error) {
	return provider.Draft{}, unsupported("create draft")
}

func (c *Client) UpdateDraft(context.Context, string, provider.DraftInput) (provider.Draft, error) {
	return provider.Draft{}, unsupported("update draft")
}

func (c *Client) GetDraftRaw(context.Context, string) ([]byte, error) {
	return nil, unsupported("get draft")
}

func (c *Client) SendDraft(context.Context, []byte, string) (provider.SentMessage, error) {
	return provider.SentMessage{}, unsupported("send")
}

func (c *Client) DeleteDraft(context.Context, string) error { return unsupported("delete draft") }

// pagingParams extracts the $skip / $skiptoken parameters of an @odata.nextLink.
// Only those are kept: the host and the rest of the query are never taken from
// the answer, so a cursor cannot redirect a request or its bearer token.
func pagingParams(nextLink string) (url.Values, error) {
	u, err := url.Parse(nextLink)
	if err != nil {
		return nil, fmt.Errorf("graph: bad nextLink: %w", err)
	}
	return keepPaging(u.Query())
}

func keepPaging(in url.Values) (url.Values, error) {
	out := url.Values{}
	for k, v := range in {
		if lk := strings.ToLower(k); lk == "$skip" || lk == "$skiptoken" {
			out[lk] = v
		}
	}
	if len(out) == 0 {
		return nil, errors.New("graph: nextLink has no paging token")
	}
	return out, nil
}

func decodeCursor(cursor string) (url.Values, error) {
	v, err := url.ParseQuery(cursor)
	if err != nil {
		return nil, fmt.Errorf("%w: bad cursor", provider.ErrInvalidQuery)
	}
	out, err := keepPaging(v)
	if err != nil || len(out) != len(v) {
		return nil, fmt.Errorf("%w: bad cursor", provider.ErrInvalidQuery)
	}
	return out, nil
}

// message is the slice of Graph's message resource this client reads.
type message struct {
	ID               string      `json:"id"`
	ConversationID   string      `json:"conversationId"`
	ParentFolderID   string      `json:"parentFolderId"`
	From             *recipient  `json:"from"`
	To               []recipient `json:"toRecipients"`
	Cc               []recipient `json:"ccRecipients"`
	Subject          string      `json:"subject"`
	ReceivedDateTime time.Time   `json:"receivedDateTime"`
	SentDateTime     time.Time   `json:"sentDateTime"`
	BodyPreview      string      `json:"bodyPreview"`
	InternetMsgID    string      `json:"internetMessageId"`
	IsRead           bool        `json:"isRead"`
	Body             *struct {
		ContentType string `json:"contentType"`
		Content     string `json:"content"`
	} `json:"body"`
	Headers []struct {
		Name  string `json:"name"`
		Value string `json:"value"`
	} `json:"internetMessageHeaders"`
	Attachments []attachment `json:"attachments"`
}

type recipient struct {
	EmailAddress struct {
		Name    string `json:"name"`
		Address string `json:"address"`
	} `json:"emailAddress"`
}

func (r recipient) addr() provider.Address {
	return provider.Address{Name: r.EmailAddress.Name, Email: r.EmailAddress.Address}
}

func addrs(rs []recipient) []provider.Address {
	if len(rs) == 0 {
		return nil
	}
	out := make([]provider.Address, len(rs))
	for i, r := range rs {
		out[i] = r.addr()
	}
	return out
}

func (m message) convert(sys map[string]string) provider.Message {
	out := provider.Message{
		ID: m.ID, ThreadID: m.ConversationID,
		To: addrs(m.To), Cc: addrs(m.Cc),
		Subject: m.Subject, Snippet: m.BodyPreview,
		RFCMessageID: m.InternetMsgID, Unread: !m.IsRead,
		Date: m.ReceivedDateTime,
	}
	if out.Date.IsZero() {
		out.Date = m.SentDateTime
	}
	if m.From != nil {
		out.From = m.From.addr()
	}
	if m.ParentFolderID != "" {
		if n, ok := sys[m.ParentFolderID]; ok {
			out.Mailboxes = []string{n}
		} else {
			out.Mailboxes = []string{m.ParentFolderID}
		}
	}
	if m.Body != nil {
		if strings.EqualFold(m.Body.ContentType, "html") {
			out.BodyHTML = m.Body.Content
		} else {
			out.BodyText = m.Body.Content
		}
	}
	for _, h := range m.Headers {
		if strings.EqualFold(h.Name, "References") {
			out.References = strings.Join(strings.Fields(h.Value), " ")
		}
	}
	for _, a := range m.Attachments {
		if a.Name == "" {
			continue
		}
		out.Attachments = append(out.Attachments, provider.AttachmentMeta{ID: a.ID, Filename: a.Name, MIMEType: a.ContentType, Size: a.Size})
	}
	return out
}
