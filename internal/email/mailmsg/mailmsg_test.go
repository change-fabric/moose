package mailmsg_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/change-fabric/moose/internal/email/mailmsg"
)

var at = time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)

func TestBuildThenParseRoundTrips(t *testing.T) {
	raw, err := mailmsg.Build(mailmsg.Message{
		From: "Pat <pat@example.com>", To: []string{"Ann <ann@example.com>", "bob@example.com"}, Cc: []string{"cy@example.com"},
		Bcc: []string{"dee@example.com"}, Subject: "Café plans", Body: "Hi,\nline two with ünïcode and a very long line " + strings.Repeat("x", 200) + "\n",
		InReplyTo: "<a@b>", References: "<z@b> <a@b>",
	}, at)
	if err != nil {
		t.Fatal(err)
	}
	p, err := mailmsg.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if p.From.Address != "pat@example.com" || p.Subject != "Café plans" || !strings.Contains(p.Body, "ünïcode") ||
		p.InReplyTo != "<a@b>" || p.References != "<z@b> <a@b>" {
		t.Fatalf("parsed = %+v", p)
	}
	if got := strings.Join(p.Recipients(), ","); got != "ann@example.com,bob@example.com,cy@example.com,dee@example.com" {
		t.Fatalf("recipients = %s", got)
	}
	if len(p.Bcc) != 1 || p.Bcc[0].Address != "dee@example.com" {
		t.Fatalf("bcc = %+v", p.Bcc)
	}
	// Compose then Build again gives an equivalent message.
	raw2, err := mailmsg.Build(p.Compose(), at)
	if err != nil {
		t.Fatal(err)
	}
	p2, err := mailmsg.Parse(raw2)
	if err != nil || p2.Body != p.Body || p2.Subject != p.Subject {
		t.Fatalf("rebuilt = %+v, %v", p2, err)
	}
}

func TestBuildRefusals(t *testing.T) {
	ok := mailmsg.Message{From: "a@example.com", To: []string{"b@example.com"}, Subject: "s", Body: "b"}
	for name, mut := range map[string]func(*mailmsg.Message){
		"no recipients":      func(m *mailmsg.Message) { m.To = nil },
		"bad address":        func(m *mailmsg.Message) { m.To = []string{"not an address"} },
		"newline in subject": func(m *mailmsg.Message) { m.Subject = "hi\r\nBcc: evil@example.com" },
		"newline in address": func(m *mailmsg.Message) { m.To = []string{"b@example.com\r\nBcc: e@example.com"} },
		"bad utf8 body":      func(m *mailmsg.Message) { m.Body = "\xff\xfe" },
		"nul body":           func(m *mailmsg.Message) { m.Body = "a\x00b" },
		"too large":          func(m *mailmsg.Message) { m.Body = strings.Repeat("y", mailmsg.MaxRaw) },
		"header injection":   func(m *mailmsg.Message) { m.InReplyTo = "<a@b>\r\nBcc: e@example.com" },
	} {
		m := ok
		mut(&m)
		if _, err := mailmsg.Build(m, at); !mailmsg.IsProblem(err) {
			t.Errorf("%s: err = %v, want a Problem", name, err)
		}
	}
}

const head = "From: a@example.com\r\nTo: b@example.com\r\nSubject: s\r\n"

func TestParseRejectsEverythingButOnePlainTextBody(t *testing.T) {
	multipartMixed := head + "MIME-Version: 1.0\r\nContent-Type: multipart/mixed; boundary=XX\r\n\r\n" +
		"--XX\r\nContent-Type: text/plain\r\n\r\nhello\r\n--XX\r\nContent-Type: application/pdf; name=\"a.pdf\"\r\nContent-Disposition: attachment; filename=\"a.pdf\"\r\n\r\nJVBER\r\n--XX--\r\n"
	alt := head + "Content-Type: multipart/alternative; boundary=YY\r\n\r\n--YY\r\nContent-Type: text/plain\r\n\r\nhi\r\n--YY\r\nContent-Type: text/html\r\n\r\n<b>hi</b>\r\n--YY--\r\n"
	for name, tc := range map[string]struct{ raw, want string }{
		"attachment": {multipartMixed, "draft has 1 attachment; remove it, the PoC sends plain text only"},
		"multipart":  {alt, "multipart/alternative"},
		"html only":  {head + "Content-Type: text/html; charset=utf-8\r\n\r\n<p>hi</p>", "HTML only"},
		"pdf body":   {head + "Content-Type: application/pdf\r\n\r\nx", "application/pdf"},
		"reply-to":   {head + "Reply-To: evil@example.com\r\n\r\nhi", "Reply-To"},
		"sender":     {head + "Sender: evil@example.com\r\n\r\nhi", "Sender"},
		"x header":   {head + "X-Anything: 1\r\n\r\nhi", "X-Anything"},
		"two from":   {"From: a@example.com\r\nFrom: c@example.com\r\nTo: b@example.com\r\n\r\nhi", "more than one From"},
		"no to":      {"From: a@example.com\r\nSubject: s\r\n\r\nhi", "no recipients"},
		"charset":    {head + "Content-Type: text/plain; charset=shift_jis\r\n\r\nhi", "shift_jis"},
		"cte":        {head + "Content-Transfer-Encoding: uuencode\r\n\r\nhi", "uuencode"},
		"two froms":  {"From: a@example.com, c@example.com\r\nTo: b@example.com\r\n\r\nhi", "exactly one From"},
		"oversize":   {head + "\r\n" + strings.Repeat("z", mailmsg.MaxRaw), "limit is 256 KB"},
		"not rfc822": {"", "not a valid RFC822"},
	} {
		_, err := mailmsg.Parse([]byte(tc.raw))
		var p *mailmsg.Problem
		if !errors.As(err, &p) || !strings.Contains(p.Msg, tc.want) {
			t.Errorf("%s: err = %v, want a Problem containing %q", name, err, tc.want)
		}
	}
	multi := head + "Content-Type: multipart/mixed; boundary=XX\r\n\r\n" +
		"--XX\r\nContent-Type: text/plain\r\n\r\nhello\r\n--XX\r\nContent-Disposition: attachment; filename=a\r\n\r\nx\r\n--XX\r\nContent-Disposition: attachment; filename=b\r\n\r\ny\r\n--XX--\r\n"
	if _, err := mailmsg.Parse([]byte(multi)); err == nil || !strings.Contains(err.Error(), "2 attachments; remove them") {
		t.Errorf("two attachments: %v", err)
	}
}

func TestParseDecodesTransferEncodingsAndCharsets(t *testing.T) {
	for name, tc := range map[string]struct{ raw, want string }{
		"plain 7bit":    {head + "\r\nline1\r\nline2\r\n", "line1\nline2\n"},
		"no headers ct": {head + "Content-Type: text/plain\r\n\r\nx", "x"},
		"qp utf8":       {head + "Content-Type: text/plain; charset=utf-8\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\ncaf=C3=A9", "café"},
		"base64":        {head + "Content-Transfer-Encoding: base64\r\n\r\naGVsbG8g\r\nd29ybGQ=\r\n", "hello world"},
		"latin1":        {head + "Content-Type: text/plain; charset=iso-8859-1\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\ncaf=E9", "café"},
	} {
		p, err := mailmsg.Parse([]byte(tc.raw))
		if err != nil || p.Body != tc.want {
			t.Errorf("%s: body %q, err %v; want %q", name, p.Body, err, tc.want)
		}
	}
}

func TestParseDecodesEncodedSubjects(t *testing.T) {
	p, err := mailmsg.Parse([]byte("From: a@example.com\r\nTo: b@example.com\r\nSubject: =?UTF-8?Q?Caf=C3=A9?=\r\n\r\nx"))
	if err != nil || p.Subject != "Café" {
		t.Fatalf("subject %q, err %v", p.Subject, err)
	}
}
