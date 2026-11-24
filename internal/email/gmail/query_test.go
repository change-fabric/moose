package gmail_test

import (
	"testing"
	"time"

	"github.com/change-fabric/moose/internal/email/gmail"
	"github.com/change-fabric/moose/internal/email/provider"
)

func TestCompileQuery(t *testing.T) {
	d1 := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	d2 := time.Date(2026, 2, 3, 0, 0, 0, 0, time.UTC)
	tests := []struct {
		name string
		q    provider.Query
		want string
	}{
		{"empty", provider.Query{}, ""},
		{"from", provider.Query{From: "bob@example.com"}, `from:"bob@example.com"`},
		{"all fields", provider.Query{
			From: "a@x.com", To: "b@x.com", Subject: "q3 plan", Text: "budget", After: d1, Before: d2,
			HasAttachment: true, Unread: true, Mailbox: "INBOX",
		}, `from:"a@x.com" to:"b@x.com" subject:"q3 plan" "budget" after:1767323045 before:1770076800 has:attachment is:unread in:inbox`},
		{"system mailbox is case-insensitive", provider.Query{Mailbox: "sent"}, `in:sent`},
		{"user label", provider.Query{Mailbox: "Clients/Acme"}, `label:"Clients/Acme"`},
		{"quotes stripped", provider.Query{Subject: `say "hi" \ there`}, `subject:"say hi there"`},
		{"operator injection stays one phrase", provider.Query{Text: `x" OR in:anywhere -from:boss`}, `"x OR in:anywhere -from:boss"`},
		{"control chars and blanks", provider.Query{From: "  \n\t ", Text: "a\nb"}, `"a b"`},
		{"limit and cursor are not in the string", provider.Query{Limit: 5, Cursor: "c"}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := gmail.CompileQuery(tt.q); got != tt.want {
				t.Errorf("got  %q\nwant %q", got, tt.want)
			}
		})
	}
}
