// Package gmail implements provider.Provider on the Gmail REST API.
package gmail

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/mail"
	"strconv"
	"strings"
	"time"

	"google.golang.org/api/gmail/v1"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"

	"github.com/change-fabric/moose/internal/email/provider"
)

const (
	userMe         = "me"
	defaultLimit   = 25
	maxLimit       = 100
	maxAttachBytes = 50 << 20
)

// Client is one connected Gmail account.
type Client struct {
	svc *gmail.Service
}

var _ provider.Provider = (*Client)(nil)

// New builds a Client. Pass option.WithTokenSource (or WithHTTPClient) for
// credentials, and option.WithEndpoint to point at an httptest server.
func New(ctx context.Context, opts ...option.ClientOption) (*Client, error) {
	svc, err := gmail.NewService(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("gmail: %w", err)
	}
	return &Client{svc: svc}, nil
}

// Capabilities reports Gmail's features. Sends use messages.send on stored
// bytes, so NativeDraftSend is false.
func (c *Client) Capabilities() provider.Capabilities {
	return provider.Capabilities{Threads: true, Labels: true, SearchDialect: provider.DialectGmail}
}

func (c *Client) ListMailboxes(ctx context.Context) ([]provider.Mailbox, error) {
	res, err := c.svc.Users.Labels.List(userMe).Context(ctx).Do()
	if err != nil {
		return nil, mapErr(err)
	}
	out := make([]provider.Mailbox, 0, len(res.Labels))
	for _, l := range res.Labels {
		out = append(out, provider.Mailbox{ID: l.Id, Name: l.Name, System: l.Type == "system"})
	}
	return out, nil
}

func (c *Client) Search(ctx context.Context, q provider.Query) (provider.SearchResult, error) {
	limit := q.Limit
	switch {
	case limit <= 0:
		limit = defaultLimit
	case limit > maxLimit:
		limit = maxLimit
	}
	call := c.svc.Users.Messages.List(userMe).Context(ctx).MaxResults(int64(limit))
	if s := CompileQuery(q); s != "" {
		call = call.Q(s)
	}
	if q.Cursor != "" {
		call = call.PageToken(q.Cursor)
	}
	res, err := call.Do()
	if err != nil {
		return provider.SearchResult{}, mapErr(err)
	}
	out := provider.SearchResult{NextCursor: res.NextPageToken}
	for _, ref := range res.Messages {
		m, err := c.svc.Users.Messages.Get(userMe, ref.Id).Format("metadata").
			MetadataHeaders("From", "To", "Cc", "Subject", "Date").Context(ctx).Do()
		if err != nil {
			return provider.SearchResult{}, mapErr(err)
		}
		out.Messages = append(out.Messages, convert(m))
	}
	return out, nil
}

func (c *Client) GetMessage(ctx context.Context, id string) (provider.Message, error) {
	m, err := c.svc.Users.Messages.Get(userMe, id).Format("full").Context(ctx).Do()
	if err != nil {
		return provider.Message{}, mapErr(err)
	}
	return convert(m), nil
}

func (c *Client) GetThread(ctx context.Context, id string) (provider.Thread, error) {
	t, err := c.svc.Users.Threads.Get(userMe, id).Format("full").Context(ctx).Do()
	if err != nil {
		return provider.Thread{}, mapErr(err)
	}
	out := provider.Thread{ID: t.Id}
	for _, m := range t.Messages {
		out.Messages = append(out.Messages, convert(m))
	}
	return out, nil
}

func (c *Client) OpenAttachment(ctx context.Context, messageID, attachmentID string) (io.ReadCloser, provider.AttachmentMeta, error) {
	m, err := c.GetMessage(ctx, messageID)
	if err != nil {
		return nil, provider.AttachmentMeta{}, err
	}
	var meta provider.AttachmentMeta
	for _, a := range m.Attachments {
		if a.ID == attachmentID {
			meta = a
		}
	}
	if meta.ID == "" {
		return nil, provider.AttachmentMeta{}, fmt.Errorf("%w: attachment %s on message %s", provider.ErrNotFound, attachmentID, messageID)
	}
	if meta.Size > maxAttachBytes {
		return nil, provider.AttachmentMeta{}, fmt.Errorf("gmail: attachment is %d bytes, over the %d limit", meta.Size, maxAttachBytes)
	}
	body, err := c.svc.Users.Messages.Attachments.Get(userMe, messageID, attachmentID).Context(ctx).Do()
	if err != nil {
		return nil, provider.AttachmentMeta{}, mapErr(err)
	}
	data, err := decode(body.Data)
	if err != nil {
		return nil, provider.AttachmentMeta{}, fmt.Errorf("gmail: attachment data: %w", err)
	}
	return io.NopCloser(bytes.NewReader(data)), meta, nil
}

func (c *Client) CreateDraft(ctx context.Context, in provider.DraftInput) (provider.Draft, error) {
	d, err := c.svc.Users.Drafts.Create(userMe, draftBody("", in)).Context(ctx).Do()
	if err != nil {
		return provider.Draft{}, mapErr(err)
	}
	return convertDraft(d), nil
}

func (c *Client) UpdateDraft(ctx context.Context, id string, in provider.DraftInput) (provider.Draft, error) {
	d, err := c.svc.Users.Drafts.Update(userMe, id, draftBody(id, in)).Context(ctx).Do()
	if err != nil {
		return provider.Draft{}, mapErr(err)
	}
	return convertDraft(d), nil
}

func (c *Client) GetDraftRaw(ctx context.Context, id string) ([]byte, error) {
	d, err := c.svc.Users.Drafts.Get(userMe, id).Format("raw").Context(ctx).Do()
	if err != nil {
		return nil, mapErr(err)
	}
	if d.Message == nil {
		return nil, fmt.Errorf("%w: draft %s has no message", provider.ErrNotFound, id)
	}
	return decode(d.Message.Raw)
}

func (c *Client) SendDraft(ctx context.Context, raw []byte, threadID string) (provider.SentMessage, error) {
	m, err := c.svc.Users.Messages.Send(userMe, &gmail.Message{
		Raw:      base64.URLEncoding.EncodeToString(raw),
		ThreadId: threadID,
	}).Context(ctx).Do()
	if err != nil {
		return provider.SentMessage{}, mapErr(err)
	}
	return provider.SentMessage{MessageID: m.Id, ThreadID: m.ThreadId}, nil
}

func draftBody(id string, in provider.DraftInput) *gmail.Draft {
	return &gmail.Draft{Id: id, Message: &gmail.Message{
		Raw:      base64.URLEncoding.EncodeToString(in.Raw),
		ThreadId: in.ThreadID,
	}}
}

func convertDraft(d *gmail.Draft) provider.Draft {
	out := provider.Draft{ID: d.Id}
	if d.Message != nil {
		out.MessageID, out.ThreadID = d.Message.Id, d.Message.ThreadId
	}
	return out
}

// convert maps a Gmail message to the neutral type. Only text/plain parts feed
// BodyText; HTML is not rendered or returned.
func convert(m *gmail.Message) provider.Message {
	out := provider.Message{ID: m.Id, ThreadID: m.ThreadId, Snippet: m.Snippet, Mailboxes: m.LabelIds}
	for _, l := range m.LabelIds {
		if l == "UNREAD" {
			out.Unread = true
		}
	}
	if m.InternalDate > 0 {
		out.Date = time.UnixMilli(m.InternalDate).UTC()
	}
	if p := m.Payload; p != nil {
		for _, h := range p.Headers {
			switch strings.ToLower(h.Name) {
			case "from":
				out.From = parseAddr(h.Value)
			case "to":
				out.To = parseAddrs(h.Value)
			case "cc":
				out.Cc = parseAddrs(h.Value)
			case "subject":
				out.Subject = h.Value
			case "date":
				if t, err := mail.ParseDate(h.Value); err == nil {
					out.Date = t.UTC()
				}
			}
		}
		var body strings.Builder
		walk(p, &out, &body)
		out.BodyText = body.String()
	}
	return out
}

func walk(p *gmail.MessagePart, out *provider.Message, body *strings.Builder) {
	switch {
	case p.Filename != "" && p.Body != nil && p.Body.AttachmentId != "":
		out.Attachments = append(out.Attachments, provider.AttachmentMeta{
			ID: p.Body.AttachmentId, Filename: p.Filename, MIMEType: p.MimeType, Size: p.Body.Size,
		})
	case strings.HasPrefix(p.MimeType, "text/plain") && p.Body != nil && p.Body.Data != "":
		if b, err := decode(p.Body.Data); err == nil {
			body.Write(b)
		}
	}
	for _, c := range p.Parts {
		walk(c, out, body)
	}
}

func parseAddr(s string) provider.Address {
	a, err := mail.ParseAddress(s)
	if err != nil {
		return provider.Address{Email: strings.TrimSpace(s)}
	}
	return provider.Address{Name: a.Name, Email: a.Address}
}

func parseAddrs(s string) []provider.Address {
	list, err := mail.ParseAddressList(s)
	if err != nil {
		if strings.TrimSpace(s) == "" {
			return nil
		}
		return []provider.Address{{Email: strings.TrimSpace(s)}}
	}
	out := make([]provider.Address, 0, len(list))
	for _, a := range list {
		out = append(out, provider.Address{Name: a.Name, Email: a.Address})
	}
	return out
}

// decode accepts Gmail's base64url with or without padding.
func decode(s string) ([]byte, error) {
	return base64.RawURLEncoding.DecodeString(strings.TrimRight(s, "="))
}

// mapErr turns Google API errors into the provider sentinels.
func mapErr(err error) error {
	var ge *googleapi.Error
	if errors.As(err, &ge) {
		switch ge.Code {
		case http.StatusNotFound:
			return fmt.Errorf("%w: %v", provider.ErrNotFound, err)
		case http.StatusUnauthorized:
			return fmt.Errorf("%w: %v", provider.ErrAuth, err)
		case http.StatusBadRequest:
			return fmt.Errorf("%w: %v", provider.ErrInvalidQuery, err)
		}
		return fmt.Errorf("gmail: HTTP %s: %w", strconv.Itoa(ge.Code), err)
	}
	return fmt.Errorf("gmail: %w", err)
}
