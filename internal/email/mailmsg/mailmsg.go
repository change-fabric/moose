// Package mailmsg builds and reads the plain-text RFC822 messages moose sends.
//
// Outbound mail is deliberately narrow: one text/plain body, no attachments, no
// multipart, and only the headers listed in allowedHeaders. Parse is the single
// reader every send step shares, so the preview the approver shows, the
// recipients the policy checks and the bytes that go out are all derived from
// the same parse of the same bytes.
package mailmsg

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/mail"
	"net/textproto"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/change-fabric/moose/internal/email/provider"
)

// MaxRaw is the largest message, as raw bytes, moose builds, reads or sends.
const MaxRaw = provider.MaxDraftRaw

// MaxRecipients bounds To, Cc and Bcc together.
const MaxRecipients = 100

// Problem is a draft that cannot be sent. Its text is written for the person
// or agent who has to fix the draft, and is safe to show them.
type Problem struct{ Msg string }

func (p *Problem) Error() string { return p.Msg }

func problemf(format string, a ...any) error { return &Problem{Msg: fmt.Sprintf(format, a...)} }

// IsProblem reports whether err is a Problem.
func IsProblem(err error) bool {
	var p *Problem
	return errors.As(err, &p)
}

// Message is a message to compose. Addresses may carry a display name
// ("Ann <ann@example.com>").
type Message struct {
	From       string
	To         []string
	Cc         []string
	Bcc        []string
	Subject    string
	Body       string
	InReplyTo  string
	References string
}

// Build composes a single-part text/plain RFC822 message.
func Build(m Message, now time.Time) ([]byte, error) {
	from, err := parseOne("From", m.From)
	if err != nil {
		return nil, err
	}
	var hdr bytes.Buffer
	put := func(k, v string) { fmt.Fprintf(&hdr, "%s: %s\r\n", k, v) }
	put("From", from.String())
	total := 0
	for _, l := range []struct {
		name string
		in   []string
	}{{"To", m.To}, {"Cc", m.Cc}, {"Bcc", m.Bcc}} {
		list, err := parseList(l.name, l.in)
		if err != nil {
			return nil, err
		}
		total += len(list)
		if len(list) > 0 {
			parts := make([]string, len(list))
			for i, a := range list {
				parts[i] = a.String()
			}
			put(l.name, strings.Join(parts, ", "))
		}
	}
	if total == 0 {
		return nil, problemf("the draft has no recipients; add at least one in to, cc or bcc")
	}
	if total > MaxRecipients {
		return nil, problemf("the draft has %d recipients; the limit is %d", total, MaxRecipients)
	}
	if err := noControl("subject", m.Subject, false); err != nil {
		return nil, err
	}
	put("Subject", mime.QEncoding.Encode("utf-8", m.Subject))
	put("Date", now.UTC().Format(time.RFC1123Z))
	for _, h := range []struct{ k, v string }{{"In-Reply-To", m.InReplyTo}, {"References", m.References}} {
		if h.v == "" {
			continue
		}
		if err := noControl(strings.ToLower(h.k), h.v, false); err != nil {
			return nil, err
		}
		put(h.k, h.v)
	}
	put("MIME-Version", "1.0")
	put("Content-Type", `text/plain; charset="UTF-8"`)
	put("Content-Transfer-Encoding", "quoted-printable")
	if !utf8.ValidString(m.Body) {
		return nil, problemf("the body is not valid UTF-8 text")
	}
	if strings.ContainsRune(m.Body, 0) {
		return nil, problemf("the body contains a NUL character")
	}
	var body bytes.Buffer
	qw := quotedprintable.NewWriter(&body)
	if _, err := qw.Write([]byte(m.Body)); err != nil {
		return nil, err
	}
	if err := qw.Close(); err != nil {
		return nil, err
	}
	out := append(hdr.Bytes(), '\r', '\n')
	out = append(out, body.Bytes()...)
	if len(out) > MaxRaw {
		return nil, problemf("the message would be %d bytes; the limit is %d KB", len(out), MaxRaw>>10)
	}
	return out, nil
}

func noControl(what, s string, allowNewline bool) error {
	for _, r := range s {
		if r == '\n' && allowNewline {
			continue
		}
		if r < 0x20 || r == 0x7f {
			return problemf("the %s contains a control character", what)
		}
	}
	return nil
}

func parseOne(what, s string) (mail.Address, error) {
	if err := noControl(what, s, false); err != nil {
		return mail.Address{}, err
	}
	a, err := mail.ParseAddress(strings.TrimSpace(s))
	if err != nil {
		return mail.Address{}, problemf("%s %q is not a valid email address", what, clip(s))
	}
	return *a, nil
}

func parseList(what string, in []string) ([]mail.Address, error) {
	var out []mail.Address
	for _, s := range in {
		if strings.TrimSpace(s) == "" {
			continue
		}
		a, err := parseOne(what, s)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, nil
}

func clip(s string) string {
	if len(s) > 60 {
		return strings.ToValidUTF8(s[:60], "") + "..."
	}
	return s
}

// Parsed is a message read back from RFC822 bytes.
type Parsed struct {
	From       mail.Address
	To, Cc     []mail.Address
	Bcc        []mail.Address
	Subject    string
	Body       string // decoded, with \n line endings
	InReplyTo  string
	References string
	MessageID  string
}

// Recipients returns every distinct recipient address (To, Cc and Bcc),
// lowercased and sorted.
func (p Parsed) Recipients() []string {
	seen := map[string]bool{}
	var out []string
	for _, l := range [][]mail.Address{p.To, p.Cc, p.Bcc} {
		for _, a := range l {
			addr := strings.ToLower(strings.TrimSpace(a.Address))
			if addr != "" && !seen[addr] {
				seen[addr] = true
				out = append(out, addr)
			}
		}
	}
	sort.Strings(out)
	return out
}

// Compose turns a Parsed message back into a Message for rebuilding (a draft
// update starts from the parse of the current draft).
func (p Parsed) Compose() Message {
	str := func(l []mail.Address) []string {
		out := make([]string, len(l))
		for i, a := range l {
			out[i] = a.String()
		}
		return out
	}
	return Message{From: p.From.String(), To: str(p.To), Cc: str(p.Cc), Bcc: str(p.Bcc), Subject: p.Subject,
		Body: p.Body, InReplyTo: p.InReplyTo, References: p.References}
}

// allowedHeaders are the only headers a sendable message may carry. Anything
// else could change who sees or answers the mail without appearing in the
// approval preview.
var allowedHeaders = map[string]bool{
	"From": true, "To": true, "Cc": true, "Bcc": true, "Subject": true, "Date": true,
	"Message-Id": true, "In-Reply-To": true, "References": true,
	"Mime-Version": true, "Content-Type": true, "Content-Transfer-Encoding": true,
}

var singleHeaders = []string{"From", "To", "Cc", "Bcc", "Subject", "Date", "Message-Id", "In-Reply-To", "References", "Mime-Version", "Content-Type", "Content-Transfer-Encoding"}

// Parse reads raw and enforces the plain-text rule: a single text/plain body
// with no attachments and no multipart structure. A message that breaks the
// rule returns a *Problem naming what to remove.
func Parse(raw []byte) (Parsed, error) {
	if len(raw) > MaxRaw {
		return Parsed{}, problemf("the draft is %d bytes; the limit is %d KB", len(raw), MaxRaw>>10)
	}
	msg, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		return Parsed{}, problemf("the draft is not a valid RFC822 message: %v", err)
	}
	var names []string
	for k := range msg.Header {
		if !allowedHeaders[textproto.CanonicalMIMEHeaderKey(k)] {
			names = append(names, k)
		}
	}
	if len(names) > 0 {
		sort.Strings(names)
		return Parsed{}, problemf("the draft has header %s, which moose does not send; remove it (allowed: From, To, Cc, Bcc, Subject, Date, Message-ID, In-Reply-To, References)", names[0])
	}
	for _, k := range singleHeaders {
		if len(msg.Header[k]) > 1 {
			return Parsed{}, problemf("the draft has more than one %s header", k)
		}
	}
	var p Parsed
	p.MessageID = strings.TrimSpace(msg.Header.Get("Message-Id"))
	p.InReplyTo = strings.TrimSpace(msg.Header.Get("In-Reply-To"))
	p.References = strings.TrimSpace(msg.Header.Get("References"))

	from, err := mail.ParseAddressList(msg.Header.Get("From"))
	if err != nil || len(from) != 1 {
		return Parsed{}, problemf("the draft needs exactly one From address")
	}
	p.From = *from[0]
	for _, f := range []struct {
		name string
		dst  *[]mail.Address
	}{{"To", &p.To}, {"Cc", &p.Cc}, {"Bcc", &p.Bcc}} {
		v := strings.TrimSpace(msg.Header.Get(f.name))
		if v == "" {
			continue
		}
		list, err := mail.ParseAddressList(v)
		if err != nil {
			return Parsed{}, problemf("the %s header is not a valid address list", f.name)
		}
		for _, a := range list {
			*f.dst = append(*f.dst, *a)
		}
	}
	if n := len(p.To) + len(p.Cc) + len(p.Bcc); n == 0 {
		return Parsed{}, problemf("the draft has no recipients; add at least one in to, cc or bcc")
	} else if n > MaxRecipients {
		return Parsed{}, problemf("the draft has %d recipients; the limit is %d", n, MaxRecipients)
	}
	subject, err := new(mime.WordDecoder).DecodeHeader(msg.Header.Get("Subject"))
	if err != nil {
		return Parsed{}, problemf("the subject is not valid encoded text")
	}
	if err := noControl("subject", subject, false); err != nil {
		return Parsed{}, err
	}
	p.Subject = subject

	body, err := plainBody(msg)
	if err != nil {
		return Parsed{}, err
	}
	p.Body = body
	return p, nil
}

// plainBody returns the decoded text of a single text/plain part, or a Problem
// that says what the draft contains instead.
func plainBody(msg *mail.Message) (string, error) {
	ct := msg.Header.Get("Content-Type")
	mt, params := "text/plain", map[string]string{}
	if strings.TrimSpace(ct) != "" {
		var err error
		if mt, params, err = mime.ParseMediaType(ct); err != nil {
			return "", problemf("the draft's Content-Type is not valid")
		}
	}
	switch {
	case strings.HasPrefix(mt, "multipart/"):
		n := countAttachments(msg.Body, params["boundary"], 0)
		if n > 0 {
			return "", problemf("draft has %d attachment%s; remove %s, the PoC sends plain text only", n, plural(n), pronoun(n))
		}
		return "", problemf("the draft is %s; the PoC sends one plain-text body only, so rewrite it as plain text with no HTML alternative", mt)
	case mt == "text/html":
		return "", problemf("the draft is HTML only; the PoC sends plain text only, so rewrite the body as plain text")
	case mt != "text/plain":
		return "", problemf("the draft body is %s; the PoC sends plain text only", mt)
	}
	var r io.Reader = io.LimitReader(msg.Body, MaxRaw+1)
	switch cte := strings.ToLower(strings.TrimSpace(msg.Header.Get("Content-Transfer-Encoding"))); cte {
	case "", "7bit", "8bit", "binary":
	case "quoted-printable":
		r = quotedprintable.NewReader(r)
	case "base64":
		r = base64.NewDecoder(base64.StdEncoding, newlineStripper{r})
	default:
		return "", problemf("the draft uses transfer encoding %q, which moose does not send", cte)
	}
	b, err := io.ReadAll(r)
	if err != nil {
		return "", problemf("the draft body could not be decoded: %v", err)
	}
	switch cs := strings.ToLower(params["charset"]); cs {
	case "", "utf-8", "utf8", "us-ascii", "ascii":
	case "iso-8859-1", "latin1", "iso8859-1":
		rs := make([]rune, len(b))
		for i, c := range b {
			rs[i] = rune(c)
		}
		b = []byte(string(rs))
	default:
		return "", problemf("the draft body is in charset %q; rewrite it as UTF-8", cs)
	}
	if !utf8.Valid(b) {
		return "", problemf("the draft body is not valid UTF-8 text")
	}
	s := strings.ReplaceAll(strings.ReplaceAll(string(b), "\r\n", "\n"), "\r", "\n")
	if strings.ContainsRune(s, 0) {
		return "", problemf("the draft body contains a NUL character")
	}
	return s, nil
}

type newlineStripper struct{ r io.Reader }

func (n newlineStripper) Read(p []byte) (int, error) {
	m, err := n.r.Read(p)
	out := p[:0]
	for _, c := range p[:m] {
		if c != '\r' && c != '\n' {
			out = append(out, c)
		}
	}
	return len(out), err
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

func pronoun(n int) string {
	if n == 1 {
		return "it"
	}
	return "them"
}

// countAttachments walks a multipart body and counts the parts that are
// attachments: a filename, or a disposition of attachment. It reads at most
// MaxRaw bytes and goes at most four levels deep.
func countAttachments(r io.Reader, boundary string, depth int) int {
	if boundary == "" || depth > 4 {
		return 0
	}
	mr := multipart.NewReader(io.LimitReader(r, MaxRaw), boundary)
	n := 0
	for {
		part, err := mr.NextRawPart()
		if err != nil {
			return n
		}
		disp, dparams, _ := mime.ParseMediaType(part.Header.Get("Content-Disposition"))
		mt, params, _ := mime.ParseMediaType(part.Header.Get("Content-Type"))
		switch {
		case strings.HasPrefix(mt, "multipart/"):
			n += countAttachments(part, params["boundary"], depth+1)
		case disp == "attachment" || dparams["filename"] != "" || params["name"] != "":
			n++
		}
	}
}
