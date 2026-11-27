// Package provider defines the mailbox-provider abstraction moose-email is
// written against. Tools and the approval flow use only these types, so adding
// a provider means implementing Provider, not touching the tools. Gmail sends
// the exact approved bytes; Microsoft Graph (internal/email/graph) sends through
// a staging draft Exchange re-serialises, so it also implements NativeSender and
// gets the semantic guarantee.
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
	// ErrTooLarge means an attachment is over MaxAttachment and was not downloaded.
	ErrTooLarge = errors.New("provider: attachment too large")
	// ErrDraftTooLarge means a draft is over MaxDraftRaw and was not downloaded.
	ErrDraftTooLarge = errors.New("provider: draft too large")
	// ErrRejected means the provider definitely refused the request (an HTTP 4xx
	// that is not an auth, not-found or rate-limit answer), so nothing was done.
	ErrRejected = errors.New("provider: request rejected")
	// ErrSendRejected means a send call was answered with a response that
	// proves the provider refused it, so nothing was delivered. It is returned
	// together with ErrRejected, and only for the enumerated rejection answers
	// of a provider; transport errors, timeouts, 5xx and every other answer to a
	// send are not wrapped in it, because the message may have gone out.
	ErrSendRejected = errors.New("provider: send provably rejected")
	// ErrUnsupported means the provider does not implement the operation (yet).
	// Its text carries the stable code CodeUnsupportedForProvider.
	ErrUnsupported = errors.New("provider: " + CodeUnsupportedForProvider)
)

// CodeUnsupportedForProvider is the stable code reported when the account's
// provider does not support the requested operation.
const CodeUnsupportedForProvider = "unsupported_for_provider"

// RateLimitError is a rate-limit answer that carries the provider's Retry-After
// hint. It wraps ErrRateLimited, so errors.Is(err, ErrRateLimited) holds and
// callers that only classify keep working. RetryAfter is zero when the provider
// gave no hint.
type RateLimitError struct {
	RetryAfter time.Duration
	// Err is optional provider detail.
	Err error
}

func (e *RateLimitError) Error() string {
	msg := ErrRateLimited.Error()
	if e.RetryAfter > 0 {
		msg += " (retry after " + e.RetryAfter.String() + ")"
	}
	if e.Err != nil {
		msg += ": " + e.Err.Error()
	}
	return msg
}

// Unwrap makes errors.Is(err, ErrRateLimited) true.
func (e *RateLimitError) Unwrap() error { return ErrRateLimited }

// Neutral system mailbox ids. Providers translate them to and from their native
// ids (Gmail label ids happen to match; Graph uses inbox, sentitems, drafts,
// deleteditems and junkemail). Tools use these constants, never a native id.
const (
	MailboxInbox = "INBOX"
	MailboxSent  = "SENT"
	MailboxDraft = "DRAFT"
	MailboxTrash = "TRASH"
	MailboxSpam  = "SPAM"
)

// MaxDraftRaw is the largest draft, as raw RFC822 bytes, a provider reads for an
// approval snapshot. A larger draft fails before any approval request exists.
const MaxDraftRaw = 256 << 10

// MaxAttachment is the largest attachment a provider downloads. It matches the
// text extractor's download cap, and it bounds the memory a provider that must
// read an attachment whole (Gmail returns it as one base64 JSON field) can use.
const MaxAttachment = 25 << 20

// MaxFetch is the most messages a provider fetches metadata for in one
// Search call. Each is a separate request on Gmail, so the cap bounds the cost
// of a search on one account.
const MaxFetch = 25

// SearchDialect names the native search syntax a provider compiles Query into.
type SearchDialect string

const (
	DialectGmail SearchDialect = "gmail"
	DialectGraph SearchDialect = "graph"
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
	// NativeDraftSend reports that sending goes through a draft the provider
	// stores and re-serialises (Microsoft Graph), so the sent bytes cannot be
	// guaranteed. Such providers get the semantic guarantee: From, recipients,
	// subject and decoded body of what is sent equal the approved parse, checked
	// against the Sent copy. Providers without it (Gmail) send the exact stored
	// bytes.
	NativeDraftSend bool `json:"native_draft_send"`
	// ReadOnly reports that the account can only be read: drafts and send are
	// not implemented, so the write tools answer unsupported_for_provider.
	// No provider reports it now that Microsoft Graph has drafts and send; the
	// field stays for accounts stored while Graph was read-only.
	ReadOnly bool `json:"read_only,omitempty"`
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
	// RFCMessageID and References are the Message-ID and References headers,
	// used to thread a reply.
	RFCMessageID string `json:"rfc_message_id,omitempty"`
	References   string `json:"references,omitempty"`
	BodyText     string `json:"body_text,omitempty"`
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

// Staged is a staging draft created for a send.
type Staged struct {
	// ID is the provider's stable id for the draft (Graph: the ImmutableId). The
	// same id names the message after it moves to Sent Items, which is how a
	// send is correlated without looking at subjects or times.
	ID string `json:"id"`
	// InternetMessageID is the RFC822 Message-ID the provider gave the draft,
	// when it has one yet. It is recorded in the ledger and compared to the Sent
	// copy.
	InternetMessageID string `json:"internet_message_id,omitempty"`
}

// SentState is where a staged message stands.
type SentState int

const (
	// SentPending means the message is still a draft (or anywhere but Sent
	// Items): the send is asynchronous, so this is never evidence of failure.
	SentPending SentState = iota
	// SentFound means the message is in Sent Items; SentCopy carries it.
	SentFound
	// SentMissing means the provider no longer knows the id. It proves nothing:
	// the message may have been sent and then deleted, or never existed.
	SentMissing
)

// SentCopy is the answer to LookupSent.
type SentCopy struct {
	State SentState
	// ID is the id of the Sent Items copy (the staging id on Graph).
	ID                string
	InternetMessageID string
	// Raw is the canonical rebuild of the Sent copy (the same construction as
	// GetDraftRaw), set when State is SentFound. Callers parse it and compare
	// fields; they never compare bytes.
	Raw []byte
}

// NativeSender is implemented by providers with Capabilities.NativeDraftSend.
// The send engine drives it in steps so the ledger can record the staging id
// and the send intent before the send call is issued, and so an outcome it did
// not see can be resolved later by the staging id alone.
type NativeSender interface {
	// StageDraft creates a draft from raw (the approved snapshot).
	StageDraft(ctx context.Context, raw []byte) (Staged, error)
	// IssueSend submits the staging draft. nil means the provider accepted it
	// (Graph: 202), not that it is delivered. A response that proves refusal
	// wraps ErrSendRejected; anything else, including a lost response, is
	// returned as is and must be treated as unknown.
	IssueSend(ctx context.Context, stagingID string) error
	// LookupSent looks the staged message up by its id.
	LookupSent(ctx context.Context, stagingID string) (SentCopy, error)
	// DeleteStaging removes the staging draft, and only while it is still a
	// draft: a message that has moved to Sent Items is never deleted. A missing
	// message is ErrNotFound.
	DeleteStaging(ctx context.Context, stagingID string) error
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
	// GetDraftRaw returns the draft's RFC822 bytes. A draft over MaxDraftRaw
	// fails with ErrDraftTooLarge without its content being read.
	GetDraftRaw(ctx context.Context, id string) ([]byte, error)
	// SendDraft delivers raw. The guarantee depends on the provider. Without
	// Capabilities.NativeDraftSend (Gmail) it is byte-exact: exactly raw goes out,
	// no draft is re-read, so the approved snapshot is what is sent. With
	// NativeDraftSend (Graph) it is a semantic match plus verify: the provider
	// re-serialises the message, so the caller compares the Sent copy's From,
	// recipients, subject and decoded body to the approved parse.
	SendDraft(ctx context.Context, raw []byte, threadID string) (SentMessage, error)
	// DeleteDraft removes a draft. A missing draft is ErrNotFound.
	DeleteDraft(ctx context.Context, id string) error
}
