// Package fake is an in-memory provider.Provider for tests. It filters on the
// neutral Query directly, so tool tests do not depend on Gmail syntax.
package fake

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/change-fabric/moose/internal/email/provider"
)

// Provider is a concurrency-safe in-memory mailbox.
type Provider struct {
	mu          sync.Mutex
	caps        provider.Capabilities
	mailboxes   []provider.Mailbox
	messages    []provider.Message
	attachments map[string][]byte // "msgID/attID"
	drafts      map[string]*draft
	sent        []Sent
	seq         int
	// Err, when set, is returned by every call. Tests use it to simulate outages.
	Err error
	// SendErr, when set, is returned by SendDraft only (and nothing is delivered).
	SendErr error
	// SendHook, when set, runs inside SendDraft after the message is recorded
	// and before it returns, so tests can fail the caller after the provider
	// accepted the message.
	SendHook func()
	// Delay makes read calls wait this long (or until the context ends), to
	// simulate a slow account.
	Delay time.Duration
}

func (p *Provider) wait(ctx context.Context) error {
	if p.Delay <= 0 {
		return ctx.Err()
	}
	select {
	case <-time.After(p.Delay):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

type draft struct {
	provider.Draft
	raw []byte
}

// Sent records one SendDraft call.
type Sent struct {
	Raw      []byte
	ThreadID string
	Result   provider.SentMessage
}

var _ provider.Provider = (*Provider)(nil)

// New returns an empty provider with Gmail-like capabilities and an INBOX.
func New() *Provider {
	return &Provider{
		caps:        provider.Capabilities{Threads: true, Labels: true, SearchDialect: provider.DialectNone},
		mailboxes:   []provider.Mailbox{{ID: "INBOX", Name: "INBOX", System: true}, {ID: "SENT", Name: "SENT", System: true}},
		attachments: map[string][]byte{},
		drafts:      map[string]*draft{},
	}
}

// SetCapabilities overrides the reported capabilities.
func (p *Provider) SetCapabilities(c provider.Capabilities) { p.mu.Lock(); p.caps = c; p.mu.Unlock() }

// AddMailbox registers a mailbox.
func (p *Provider) AddMailbox(m provider.Mailbox) {
	p.mu.Lock()
	p.mailboxes = append(p.mailboxes, m)
	p.mu.Unlock()
}

// AddMessage stores m. An empty ID is assigned and an empty ThreadID defaults to the ID.
func (p *Provider) AddMessage(m provider.Message) provider.Message {
	p.mu.Lock()
	defer p.mu.Unlock()
	if m.ID == "" {
		p.seq++
		m.ID = "m" + strconv.Itoa(p.seq)
	}
	if m.ThreadID == "" {
		m.ThreadID = "t-" + m.ID
	}
	p.messages = append(p.messages, m)
	return m
}

// AddAttachment attaches data to message msgID.
func (p *Provider) AddAttachment(msgID string, meta provider.AttachmentMeta, data []byte) {
	p.mu.Lock()
	defer p.mu.Unlock()
	meta.Size = int64(len(data))
	for i := range p.messages {
		if p.messages[i].ID == msgID {
			p.messages[i].Attachments = append(p.messages[i].Attachments, meta)
		}
	}
	p.attachments[msgID+"/"+meta.ID] = data
}

// SentMessages returns what SendDraft has delivered, oldest first.
func (p *Provider) SentMessages() []Sent {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]Sent(nil), p.sent...)
}

func (p *Provider) Capabilities() provider.Capabilities {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.caps
}

func (p *Provider) ListMailboxes(ctx context.Context) ([]provider.Mailbox, error) {
	if err := p.wait(ctx); err != nil {
		return nil, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.Err != nil {
		return nil, p.Err
	}
	return append([]provider.Mailbox(nil), p.mailboxes...), nil
}

// Search filters newest first. The cursor is the integer offset into the result list.
func (p *Provider) Search(ctx context.Context, q provider.Query) (provider.SearchResult, error) {
	if err := p.wait(ctx); err != nil {
		return provider.SearchResult{}, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.Err != nil {
		return provider.SearchResult{}, p.Err
	}
	offset := 0
	if q.Cursor != "" {
		n, err := strconv.Atoi(q.Cursor)
		if err != nil || n < 0 {
			return provider.SearchResult{}, fmt.Errorf("%w: bad cursor %q", provider.ErrInvalidQuery, q.Cursor)
		}
		offset = n
	}
	limit := q.Limit
	if limit <= 0 {
		limit = 25
	}
	var hits []provider.Message
	for _, m := range p.messages {
		if matches(m, q) {
			hits = append(hits, m)
		}
	}
	sort.SliceStable(hits, func(i, j int) bool { return hits[i].Date.After(hits[j].Date) })
	if offset > len(hits) {
		offset = len(hits)
	}
	end := offset + limit
	res := provider.SearchResult{}
	if end < len(hits) {
		res.NextCursor = strconv.Itoa(end)
	} else {
		end = len(hits)
	}
	res.Messages = hits[offset:end]
	return res, nil
}

func matches(m provider.Message, q provider.Query) bool {
	has := func(s, sub string) bool { return strings.Contains(strings.ToLower(s), strings.ToLower(sub)) }
	switch {
	case q.From != "" && !has(m.From.Email+" "+m.From.Name, q.From):
		return false
	case q.To != "" && !anyAddr(m.To, q.To):
		return false
	case q.Subject != "" && !has(m.Subject, q.Subject):
		return false
	case q.Text != "" && !has(m.Subject+" "+m.BodyText+" "+m.Snippet, q.Text):
		return false
	case !q.After.IsZero() && !m.Date.After(q.After):
		return false
	case !q.Before.IsZero() && !m.Date.Before(q.Before):
		return false
	case q.HasAttachment && len(m.Attachments) == 0:
		return false
	case q.Unread && !m.Unread:
		return false
	case q.Mailbox != "" && !contains(m.Mailboxes, q.Mailbox):
		return false
	}
	return true
}

func anyAddr(list []provider.Address, sub string) bool {
	for _, a := range list {
		if strings.Contains(strings.ToLower(a.Email+" "+a.Name), strings.ToLower(sub)) {
			return true
		}
	}
	return false
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func (p *Provider) GetMessage(ctx context.Context, id string) (provider.Message, error) {
	if err := p.wait(ctx); err != nil {
		return provider.Message{}, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.Err != nil {
		return provider.Message{}, p.Err
	}
	for _, m := range p.messages {
		if m.ID == id {
			return m, nil
		}
	}
	return provider.Message{}, fmt.Errorf("%w: message %s", provider.ErrNotFound, id)
}

func (p *Provider) GetThread(ctx context.Context, id string) (provider.Thread, error) {
	if err := p.wait(ctx); err != nil {
		return provider.Thread{}, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.Err != nil {
		return provider.Thread{}, p.Err
	}
	t := provider.Thread{ID: id}
	for _, m := range p.messages {
		if m.ThreadID == id {
			t.Messages = append(t.Messages, m)
		}
	}
	if len(t.Messages) == 0 {
		return provider.Thread{}, fmt.Errorf("%w: thread %s", provider.ErrNotFound, id)
	}
	sort.SliceStable(t.Messages, func(i, j int) bool { return t.Messages[i].Date.Before(t.Messages[j].Date) })
	return t, nil
}

func (p *Provider) OpenAttachment(_ context.Context, messageID, attachmentID string) (io.ReadCloser, provider.AttachmentMeta, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.Err != nil {
		return nil, provider.AttachmentMeta{}, p.Err
	}
	for _, m := range p.messages {
		if m.ID != messageID {
			continue
		}
		for _, a := range m.Attachments {
			if a.ID == attachmentID {
				return io.NopCloser(bytes.NewReader(p.attachments[messageID+"/"+attachmentID])), a, nil
			}
		}
	}
	return nil, provider.AttachmentMeta{}, fmt.Errorf("%w: attachment %s/%s", provider.ErrNotFound, messageID, attachmentID)
}

func (p *Provider) CreateDraft(_ context.Context, in provider.DraftInput) (provider.Draft, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.Err != nil {
		return provider.Draft{}, p.Err
	}
	p.seq++
	d := &draft{Draft: provider.Draft{ID: "d" + strconv.Itoa(p.seq), MessageID: "dm" + strconv.Itoa(p.seq), ThreadID: in.ThreadID}, raw: append([]byte(nil), in.Raw...)}
	p.drafts[d.ID] = d
	return d.Draft, nil
}

func (p *Provider) UpdateDraft(_ context.Context, id string, in provider.DraftInput) (provider.Draft, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.Err != nil {
		return provider.Draft{}, p.Err
	}
	d, ok := p.drafts[id]
	if !ok {
		return provider.Draft{}, fmt.Errorf("%w: draft %s", provider.ErrNotFound, id)
	}
	d.raw, d.ThreadID = append([]byte(nil), in.Raw...), in.ThreadID
	return d.Draft, nil
}

func (p *Provider) GetDraftRaw(_ context.Context, id string) ([]byte, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.Err != nil {
		return nil, p.Err
	}
	d, ok := p.drafts[id]
	if !ok {
		return nil, fmt.Errorf("%w: draft %s", provider.ErrNotFound, id)
	}
	if len(d.raw) > provider.MaxDraftRaw {
		return nil, fmt.Errorf("%w: draft %s", provider.ErrDraftTooLarge, id)
	}
	return append([]byte(nil), d.raw...), nil
}

func (p *Provider) DeleteDraft(_ context.Context, id string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.Err != nil {
		return p.Err
	}
	if _, ok := p.drafts[id]; !ok {
		return fmt.Errorf("%w: draft %s", provider.ErrNotFound, id)
	}
	delete(p.drafts, id)
	return nil
}

func (p *Provider) SendDraft(_ context.Context, raw []byte, threadID string) (provider.SentMessage, error) {
	p.mu.Lock()
	if p.Err != nil {
		defer p.mu.Unlock()
		return provider.SentMessage{}, p.Err
	}
	if p.SendErr != nil {
		defer p.mu.Unlock()
		return provider.SentMessage{}, p.SendErr
	}
	p.seq++
	res := provider.SentMessage{MessageID: "s" + strconv.Itoa(p.seq), ThreadID: threadID}
	p.sent = append(p.sent, Sent{Raw: append([]byte(nil), raw...), ThreadID: threadID, Result: res})
	hook := p.SendHook
	p.mu.Unlock()
	if hook != nil {
		hook()
	}
	return res, nil
}

// DraftCount reports how many drafts exist.
func (p *Provider) DraftCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.drafts)
}

// Draft returns a draft's record (including its thread) and its raw bytes.
func (p *Provider) Draft(id string) (provider.Draft, []byte, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	d, ok := p.drafts[id]
	if !ok {
		return provider.Draft{}, nil, false
	}
	return d.Draft, append([]byte(nil), d.raw...), true
}

// SetDraftRaw replaces a draft's bytes without going through UpdateDraft, which
// is how a test edits a draft behind moose's back (the owner editing in Gmail).
func (p *Provider) SetDraftRaw(id string, raw []byte) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if d, ok := p.drafts[id]; ok {
		d.raw = append([]byte(nil), raw...)
	}
}
