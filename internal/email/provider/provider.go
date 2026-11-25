// Package provider defines the mailbox-provider abstraction moose-email is
// written against. Tools and the approval flow use only these types, so adding
// a provider means implementing Provider, not touching the tools. Gmail is the
// only implementation today.
package provider

import (
	"context"
	"errors"
	"io"
	"time"
)

// Errors a Provider returns. Implementations wrap them with detail.
var (
	// ErrNotFound means the message, thread, draft, attachment or mailbox does not exist.
	ErrNotFound = errors.New("provider: not found")
	// ErrAuth means the stored credential no longer works and the account needs reauthorization.
	ErrAuth = errors.New("provider: authorization failed")
	// ErrInvalidQuery means a Query or cursor could not be used.
	ErrInvalidQuery = errors.New("provider: invalid query")
	// ErrRateLimited means the provider asked the caller to slow down (HTTP 429 or
	// a quota error). It is transient: back off and retry.
	ErrRateLimited = errors.New("provider: rate limited")
)

// MaxFetch is the most messages a provider fetches metadata for in one
// Search call. Each is a separate request on Gmail, so the cap bounds the cost
// of a search on one account.
const MaxFetch = 25

// SearchDialect names the native search syntax a provider compiles Query into.
type SearchDialect string

const (
	DialectGmail SearchDialect = "gmail"
	DialectNone  SearchDialect = "none"
)

// Capabilities describes what an account's provider supports, so tools can
// adapt instead of branching on the provider name.
type Capabilities struct {
	// Threads reports server-side conversation threading.
	Threads bool `json:"threads"`
	// Labels reports that messages carry many mailboxes (labels) rather than one folder.
	Labels bool `json:"labels"`
	// SearchDialect is the native syntax Query compiles to.
	SearchDialect SearchDialect `json:"search_dialect"`
	// NativeDraftSend reports that the provider can send a stored draft by id.
	// moose never relies on it: approved sends use the exact stored bytes.
	NativeDraftSend bool `json:"native_draft_send"`
}

// Query is the neutral search request. Zero fields are unset. Text fields are
// literal user input, never provider syntax: compilers must quote them.
type Query struct {
	From          string
	To            string
	Subject       string
	Text          string
	After         time.Time
	Before        time.Time
	HasAttachment bool
	// Mailbox is a Mailbox.ID from ListMailboxes.
	Mailbox string
	Unread  bool
	// Limit caps the page size. Zero means the provider default.
	Limit int
	// Cursor is the opaque NextCursor of a previous page.
	Cursor string
}

// Address is a mailbox address with an optional display name.
type Address struct {
	Name  string `json:"name,omitempty"`
	Email string `json:"email"`
}

// Mailbox is a label or folder.
type Mailbox struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	System bool   `json:"system"`
}

// AttachmentMeta describes an attachment without its bytes.
type AttachmentMeta struct {
	ID       string `json:"id"`
	Filename string `json:"filename"`
	MIMEType string `json:"mime_type"`
	Size     int64  `json:"size"`
}

// Message is one message. Header and body text are untrusted content.
type Message struct {
	ID        string    `json:"id"`
	ThreadID  string    `json:"thread_id,omitempty"`
	Mailboxes []string  `json:"mailboxes,omitempty"`
	From      Address   `json:"from"`
	To        []Address `json:"to,omitempty"`
	Cc        []Address `json:"cc,omitempty"`
	Subject   string    `json:"subject"`
	Date      time.Time `json:"date"`
	Snippet   string    `json:"snippet,omitempty"`
	BodyText  string    `json:"body_text,omitempty"`
	// BodyHTML is the HTML alternative, raw. Tools convert it to text when
	// BodyText is empty; it is never returned to the caller as HTML.
	BodyHTML    string           `json:"body_html,omitempty"`
	Unread      bool             `json:"unread"`
	Attachments []AttachmentMeta `json:"attachments,omitempty"`
}

// Thread is a conversation, oldest message first.
type Thread struct {
	ID       string    `json:"id"`
	Messages []Message `json:"messages"`
}

// SearchResult is one page of messages, newest first.
type SearchResult struct {
	Messages []Message `json:"messages"`
	// NextCursor is empty on the last page.
	NextCursor string `json:"next_cursor,omitempty"`
}

// DraftInput carries a complete RFC822 message. Building the message belongs to
// the caller so the bytes that were reviewed are the bytes stored.
type DraftInput struct {
	Raw      []byte
	ThreadID string
}

// Draft is a stored draft.
type Draft struct {
	ID        string `json:"id"`
	MessageID string `json:"message_id,omitempty"`
	ThreadID  string `json:"thread_id,omitempty"`
}

// SentMessage identifies a message the provider accepted for delivery.
type SentMessage struct {
	MessageID string `json:"message_id"`
	ThreadID  string `json:"thread_id,omitempty"`
}

// Provider is one connected mailbox account.
type Provider interface {
	Capabilities() Capabilities
	ListMailboxes(ctx context.Context) ([]Mailbox, error)
	Search(ctx context.Context, q Query) (SearchResult, error)
	GetMessage(ctx context.Context, id string) (Message, error)
	GetThread(ctx context.Context, id string) (Thread, error)
	// OpenAttachment streams one attachment. The caller closes it.
	OpenAttachment(ctx context.Context, messageID, attachmentID string) (io.ReadCloser, AttachmentMeta, error)
	CreateDraft(ctx context.Context, in DraftInput) (Draft, error)
	UpdateDraft(ctx context.Context, id string, in DraftInput) (Draft, error)
	// GetDraftRaw returns the draft's RFC822 bytes.
	GetDraftRaw(ctx context.Context, id string) ([]byte, error)
	// SendDraft delivers exactly raw. It never re-reads a draft, so an approved
	// snapshot is what goes out.
	SendDraft(ctx context.Context, raw []byte, threadID string) (SentMessage, error)
}
