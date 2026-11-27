package graph

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/mail"
	"net/url"
	"strings"
	"time"

	"github.com/change-fabric/moose/internal/email/mailmsg"
	"github.com/change-fabric/moose/internal/email/provider"
	"github.com/change-fabric/moose/internal/graphapi"
)

// preferText asks Graph for a plain-text body, converting an HTML one itself,
// so the canonical rebuild always has the text body mailmsg requires.
const preferText = `outlook.body-content-type="text"`

const draftSelect = "id,internetMessageId,conversationId,parentFolderId,isDraft,hasAttachments," +
	"from,toRecipients,ccRecipients,bccRecipients,subject,body,lastModifiedDateTime,sentDateTime"

// draftMsg is the slice of Graph's message resource the draft code reads.
type draftMsg struct {
	ID                   string      `json:"id"`
	InternetMessageID    string      `json:"internetMessageId"`
	ConversationID       string      `json:"conversationId"`
	ParentFolderID       string      `json:"parentFolderId"`
	IsDraft              bool        `json:"isDraft"`
	HasAttachments       bool        `json:"hasAttachments"`
	From                 *recipient  `json:"from"`
	To                   []recipient `json:"toRecipients"`
	Cc                   []recipient `json:"ccRecipients"`
	Bcc                  []recipient `json:"bccRecipients"`
	Subject              string      `json:"subject"`
	LastModifiedDateTime time.Time   `json:"lastModifiedDateTime"`
	SentDateTime         time.Time   `json:"sentDateTime"`
	Body                 *struct {
		ContentType string `json:"contentType"`
		Content     string `json:"content"`
	} `json:"body"`
}

func mkRecipients(in []mail.Address) []recipient {
	out := make([]recipient, 0, len(in))
	for _, a := range in {
		var r recipient
		r.EmailAddress.Name, r.EmailAddress.Address = a.Name, a.Address
		out = append(out, r)
	}
	return out
}

// draftJSON is the JSON body of POST /me/messages and PATCH /me/messages/{id}
// for an approved message. Every field is always written, so a PATCH replaces
// the draft's content rather than merging with it.
func draftJSON(raw []byte) ([]byte, error) {
	p, err := mailmsg.Parse(raw)
	if err != nil {
		return nil, err
	}
	body := map[string]any{
		"subject":       p.Subject,
		"body":          map[string]string{"contentType": "text", "content": p.Body},
		"from":          mkRecipients([]mail.Address{p.From})[0],
		"toRecipients":  mkRecipients(p.To),
		"ccRecipients":  mkRecipients(p.Cc),
		"bccRecipients": mkRecipients(p.Bcc),
	}
	return json.Marshal(body)
}

// canonical rebuilds a message from Graph's JSON through mailmsg.Build, so the
// bytes are a pure function of the message's fields: the same draft reads the
// same every time, whatever headers Exchange keeps for itself. Date is pinned
// to the given time (lastModifiedDateTime for a draft, sentDateTime for a Sent
// copy), and no Message-ID is written. A message moose cannot send as plain
// text (attachments, no recipients) is a mailmsg.Problem.
func canonical(m draftMsg, date time.Time) ([]byte, error) {
	if m.HasAttachments {
		return nil, &mailmsg.Problem{Msg: "the draft has attachments; moose sends plain-text messages without attachments only"}
	}
	if m.From == nil || m.From.EmailAddress.Address == "" {
		return nil, &mailmsg.Problem{Msg: "the draft has no From address"}
	}
	if m.Body == nil || !strings.EqualFold(m.Body.ContentType, "text") {
		return nil, &mailmsg.Problem{Msg: "the draft body is not plain text; moose sends a single text/plain part only"}
	}
	if len(m.Body.Content) > provider.MaxDraftRaw {
		return nil, provider.ErrDraftTooLarge
	}
	fmtAddrs := func(rs []recipient) []string {
		var out []string
		for _, r := range rs {
			a := mail.Address{Name: r.EmailAddress.Name, Address: r.EmailAddress.Address}
			out = append(out, a.String())
		}
		return out
	}
	from := mail.Address{Name: m.From.EmailAddress.Name, Address: m.From.EmailAddress.Address}
	raw, err := mailmsg.Build(mailmsg.Message{
		From: from.String(), To: fmtAddrs(m.To), Cc: fmtAddrs(m.Cc), Bcc: fmtAddrs(m.Bcc),
		Subject: m.Subject, Body: m.Body.Content,
	}, date)
	if err != nil {
		return nil, err
	}
	return raw, nil
}

func messagePath(id string) string { return "/me/messages/" + graphapi.Segment(id) }

// getDraftMsg reads one message in the shape the canonical rebuild needs.
func (c *Client) getDraftMsg(ctx context.Context, id string) (draftMsg, error) {
	var m draftMsg
	err := c.g.GetJSON(ctx, messagePath(id), url.Values{"$select": {draftSelect}}, http.Header{"Prefer": {preferText}}, &m)
	return m, err
}

// requireDraft returns ErrNotFound unless id names a draft. Drafts are the only
// messages moose edits or permanently deletes, so a received or sent message's
// id is never accepted here.
func (c *Client) requireDraft(ctx context.Context, id string) error {
	var m struct {
		IsDraft bool `json:"isDraft"`
	}
	if err := c.g.GetJSON(ctx, messagePath(id), url.Values{"$select": {"id,isDraft"}}, nil, &m); err != nil {
		return err
	}
	if !m.IsDraft {
		return fmt.Errorf("%w: message %s is not a draft", provider.ErrNotFound, id)
	}
	return nil
}

// sendJSON sends a JSON body with the given method (POST or PATCH) and decodes
// the answer into out when out is non-nil.
func (c *Client) sendJSON(ctx context.Context, method, path string, body []byte, out any) error {
	resp, err := c.g.Do(ctx, graphapi.Request{Method: method, Path: path, Body: bytes.NewReader(body), ContentType: "application/json"})
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if out == nil {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("graph: decode %s: %w", path, err)
	}
	return nil
}

// CreateDraft stores raw as a draft (POST /me/messages with JSON). The id is the
// ImmutableId, so it survives moves and edits. DraftInput.ThreadID is not used:
// Graph assigns the conversation itself, and a draft's In-Reply-To and
// References cannot be set on create.
func (c *Client) CreateDraft(ctx context.Context, in provider.DraftInput) (provider.Draft, error) {
	body, err := draftJSON(in.Raw)
	if err != nil {
		return provider.Draft{}, err
	}
	var m draftMsg
	if err := c.sendJSON(ctx, http.MethodPost, "/me/messages", body, &m); err != nil {
		return provider.Draft{}, err
	}
	if m.ID == "" {
		return provider.Draft{}, errors.New("graph: create draft returned no id")
	}
	return provider.Draft{ID: m.ID, MessageID: m.InternetMessageID, ThreadID: m.ConversationID}, nil
}

// UpdateDraft replaces a draft's content with PATCH. The id does not change.
func (c *Client) UpdateDraft(ctx context.Context, id string, in provider.DraftInput) (provider.Draft, error) {
	body, err := draftJSON(in.Raw)
	if err != nil {
		return provider.Draft{}, err
	}
	if err := c.requireDraft(ctx, id); err != nil {
		return provider.Draft{}, err
	}
	var m draftMsg
	if err := c.sendJSON(ctx, http.MethodPatch, messagePath(id), body, &m); err != nil {
		return provider.Draft{}, err
	}
	out := provider.Draft{ID: id, MessageID: m.InternetMessageID, ThreadID: m.ConversationID}
	if m.ID != "" && m.ID != id {
		return provider.Draft{}, fmt.Errorf("graph: update changed the draft id from %q to %q", id, m.ID)
	}
	return out, nil
}

// GetDraftRaw returns the canonical rebuild of a draft: its Graph JSON run
// through mailmsg.Build with Date set to lastModifiedDateTime. It is
// deterministic for an unchanged draft and differs after any edit, which is
// what the approval snapshot and the after-send comparison need.
func (c *Client) GetDraftRaw(ctx context.Context, id string) ([]byte, error) {
	m, err := c.getDraftMsg(ctx, id)
	if err != nil {
		return nil, err
	}
	if !m.IsDraft {
		return nil, fmt.Errorf("%w: message %s is not a draft", provider.ErrNotFound, id)
	}
	return canonical(m, m.LastModifiedDateTime)
}

// DeleteDraft permanently deletes a draft (POST /messages/{id}/permanentDelete),
// so it does not linger in Deleted Items. A message that is not a draft is
// ErrNotFound and is left alone.
func (c *Client) DeleteDraft(ctx context.Context, id string) error {
	if err := c.requireDraft(ctx, id); err != nil {
		return err
	}
	return c.sendJSON(ctx, http.MethodPost, messagePath(id)+"/permanentDelete", nil, nil)
}
