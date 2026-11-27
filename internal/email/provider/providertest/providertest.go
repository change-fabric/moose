// Package providertest is the conformance suite every provider.Provider must
// pass. It is split in three so a provider can be brought up in stages:
//
//   - RunRead: mailboxes with neutral ids, search paging, messages, threads,
//     attachments, and how rate limits and auth failures surface.
//   - RunDraft: create, update (the id stays), snapshot determinism, delete.
//   - RunSend: send returns an id, the delivered message verifies against the
//     approved one, and a send whose outcome was lost is never reported as a
//     definite failure.
//
// Each suite takes a Factory that returns a fresh provider and a Seeder that
// arranges the provider's backend (a fake, or an httptest stub of the real
// service), so the same assertions run against every implementation.
package providertest

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/mail"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/change-fabric/moose/internal/email/provider"
)

// RetryAfter is the Retry-After a Seeder's FailRateLimit reports. A provider
// must surface it unchanged in provider.RateLimitError.
const RetryAfter = 7 * time.Second

// Factory returns a fresh, empty provider and the Seeder for its backend.
type Factory func(t *testing.T) (provider.Provider, Seeder)

// Failure is a way a backend can misbehave.
type Failure int

const (
	// FailNone clears any failure.
	FailNone Failure = iota
	// FailAuth makes every call answer that the credential is no good.
	FailAuth
	// FailRateLimit makes every call answer 429 with Retry-After of RetryAfter.
	FailRateLimit
	// FailSendRejected makes SendDraft answer a definite refusal; nothing is delivered.
	FailSendRejected
	// FailSendResponseLost makes SendDraft deliver the message and then drop the
	// response, so the caller sees a transport error for a message that went out.
	FailSendResponseLost
)

// SeedMessage describes a message to place in the backend.
type SeedMessage struct {
	// Mailbox is a neutral mailbox id (provider.MailboxInbox and friends).
	Mailbox string
	// ThreadKey groups messages into one conversation.
	ThreadKey  string
	From       provider.Address
	To         []provider.Address
	Subject    string
	Body       string
	Date       time.Time
	Unread     bool
	Attachment *SeedAttachment
}

// SeedAttachment is one attachment on a seeded message.
type SeedAttachment struct {
	Filename string
	MIMEType string
	Data     []byte
}

// Seeded is what the backend assigned to a seeded message.
type Seeded struct {
	ID           string
	ThreadID     string
	AttachmentID string
}

// Seeder arranges and inspects the backend behind a provider.
type Seeder interface {
	// SeedMessage stores m and returns the ids the provider will use for it.
	SeedMessage(t *testing.T, m SeedMessage) Seeded
	// Fail sets how the backend misbehaves until the next Fail call.
	Fail(t *testing.T, f Failure)
	// Delivered returns the raw RFC822 of every message the backend delivered
	// for SendDraft calls, oldest first, as the recipient side would hold it.
	Delivered(t *testing.T) [][]byte
}

func addr(email string) provider.Address { return provider.Address{Email: email} }

var epoch = time.Date(2026, 3, 4, 9, 0, 0, 0, time.UTC)

// Fields is the part of a message the send guarantee is about: who it is from
// and to, its subject, and its decoded text body.
type Fields struct {
	From    string
	To, Cc  []string
	Bcc     []string
	Subject string
	Body    string
}

// ParseFields reads the guaranteed fields out of raw, tolerating the extra
// headers and re-encoding a provider may add. Addresses compare by lowercased
// email, the body by its decoded text with line endings and trailing space
// normalised.
func ParseFields(raw []byte) (Fields, error) {
	msg, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		return Fields{}, err
	}
	var f Fields
	one, err := mail.ParseAddress(msg.Header.Get("From"))
	if err != nil {
		return Fields{}, fmt.Errorf("from: %w", err)
	}
	f.From = strings.ToLower(one.Address)
	list := func(name string) ([]string, error) {
		v := strings.TrimSpace(msg.Header.Get(name))
		if v == "" {
			return nil, nil
		}
		as, err := mail.ParseAddressList(v)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		out := make([]string, len(as))
		for i, a := range as {
			out[i] = strings.ToLower(a.Address)
		}
		sort.Strings(out)
		return out, nil
	}
	if f.To, err = list("To"); err != nil {
		return Fields{}, err
	}
	if f.Cc, err = list("Cc"); err != nil {
		return Fields{}, err
	}
	if f.Bcc, err = list("Bcc"); err != nil {
		return Fields{}, err
	}
	if f.Subject, err = new(mime.WordDecoder).DecodeHeader(msg.Header.Get("Subject")); err != nil {
		return Fields{}, fmt.Errorf("subject: %w", err)
	}
	body, err := textBody(msg.Header.Get("Content-Type"), msg.Header.Get("Content-Transfer-Encoding"), msg.Body)
	if err != nil {
		return Fields{}, err
	}
	f.Body = normalise(body)
	return f, nil
}

func normalise(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	lines := strings.Split(s, "\n")
	for i := range lines {
		lines[i] = strings.TrimRight(lines[i], " \t")
	}
	return strings.TrimRight(strings.Join(lines, "\n"), "\n")
}

func textBody(contentType, cte string, r io.Reader) (string, error) {
	mt, params, err := mime.ParseMediaType(contentType)
	if err != nil || contentType == "" {
		mt = "text/plain"
	}
	if strings.HasPrefix(mt, "multipart/") {
		mr := multipart.NewReader(r, params["boundary"])
		for {
			part, err := mr.NextPart()
			if err != nil {
				return "", fmt.Errorf("no text/plain part: %w", err)
			}
			pt, _, _ := mime.ParseMediaType(part.Header.Get("Content-Type"))
			if pt == "text/plain" {
				return textBody(part.Header.Get("Content-Type"), part.Header.Get("Content-Transfer-Encoding"), part)
			}
		}
	}
	switch strings.ToLower(strings.TrimSpace(cte)) {
	case "quoted-printable":
		r = quotedprintable.NewReader(r)
	case "base64":
		r = base64.NewDecoder(base64.StdEncoding, &stripNewlines{r})
	}
	b, err := io.ReadAll(r)
	return string(b), err
}

// stripNewlines drops line breaks from a base64 stream.
type stripNewlines struct{ r io.Reader }

func (s *stripNewlines) Read(p []byte) (int, error) {
	n, err := s.r.Read(p)
	out := p[:0]
	for _, c := range p[:n] {
		if c != '\r' && c != '\n' {
			out = append(out, c)
		}
	}
	return len(out), err
}

// SameFields reports how got differs from want, or "" when they match.
func SameFields(want, got Fields) string {
	var diffs []string
	check := func(name string, a, b any) {
		if fmt.Sprint(a) != fmt.Sprint(b) {
			diffs = append(diffs, fmt.Sprintf("%s: want %v, got %v", name, a, b))
		}
	}
	check("From", want.From, got.From)
	check("To", want.To, got.To)
	check("Cc", want.Cc, got.Cc)
	check("Bcc", want.Bcc, got.Bcc)
	check("Subject", want.Subject, got.Subject)
	check("Body", fmt.Sprintf("%q", want.Body), fmt.Sprintf("%q", got.Body))
	return strings.Join(diffs, "; ")
}
