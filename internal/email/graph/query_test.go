package graph_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/change-fabric/moose/internal/email/graph"
	"github.com/change-fabric/moose/internal/email/provider"
)

func TestCompileQuery(t *testing.T) {
	after := time.Date(2026, 3, 1, 10, 30, 0, 0, time.FixedZone("x", 3600))
	before := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	cases := []struct {
		name       string
		q          provider.Query
		search     string
		filter     string
		orderBy    string
		wantErr    bool
		notContain []string
	}{
		{
			name:    "empty query still opens with the sort property",
			q:       provider.Query{},
			filter:  "receivedDateTime ge 1970-01-01T00:00:00Z",
			orderBy: "receivedDateTime desc",
		},
		{
			name:    "unread filter keeps receivedDateTime first",
			q:       provider.Query{Unread: true},
			filter:  "receivedDateTime ge 1970-01-01T00:00:00Z and isRead eq false",
			orderBy: "receivedDateTime desc",
		},
		{
			name:    "structured fields compile to filter",
			q:       provider.Query{From: "bob@example.com", To: "me@example.com", Subject: "it's", After: after, Before: before, HasAttachment: true},
			filter:  "receivedDateTime ge 2026-03-01T09:30:00Z and receivedDateTime lt 2026-04-01T00:00:00Z and from/emailAddress/address eq 'bob@example.com' and toRecipients/any(r:r/emailAddress/address eq 'me@example.com') and contains(subject,'it''s') and hasAttachments eq true",
			orderBy: "receivedDateTime desc",
		},
		{
			name:   "text compiles to KQL with no filter or order",
			q:      provider.Query{Text: "quarterly plan", From: "bob@example.com", Subject: "q1", After: after, Before: before, HasAttachment: true},
			search: `"quarterly plan" from:"bob@example.com" subject:"q1" received>=2026-03-01 received<=2026-04-01 hasattachment:true`,
		},
		{
			name:       "quotes and backslashes cannot end the phrase",
			q:          provider.Query{Text: `x" OR from:evil\`, From: `a" OR to:b`},
			search:     `"x OR from:evil" from:"a OR to:b"`,
			notContain: []string{`\`},
		},
		{
			name:       "control characters collapse to spaces",
			q:          provider.Query{Text: "a\r\nb\x00c"},
			search:     `"a b c"`,
			notContain: []string{"\n", "\x00"},
		},
		{
			name:       "single quotes are doubled in filter literals",
			q:          provider.Query{Subject: "x') or isRead eq false or contains(subject,'"},
			filter:     "receivedDateTime ge 1970-01-01T00:00:00Z and contains(subject,'x'') or isRead eq false or contains(subject,''')",
			orderBy:    "receivedDateTime desc",
			notContain: nil,
		},
		{name: "unread with text is refused", q: provider.Query{Text: "plan", Unread: true}, wantErr: true},
		{name: "blank text after cleaning is a filter query", q: provider.Query{Text: ` " \ `}, filter: "receivedDateTime ge 1970-01-01T00:00:00Z", orderBy: "receivedDateTime desc"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := graph.CompileQuery(tc.q)
			if tc.wantErr {
				if !errors.Is(err, provider.ErrInvalidQuery) {
					t.Fatalf("err = %v, want ErrInvalidQuery", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got.Search != tc.search || got.Filter != tc.filter || got.OrderBy != tc.orderBy {
				t.Errorf("got  search=%q filter=%q orderby=%q\nwant search=%q filter=%q orderby=%q", got.Search, got.Filter, got.OrderBy, tc.search, tc.filter, tc.orderBy)
			}
			for _, s := range tc.notContain {
				if strings.Contains(got.Search+got.Filter, s) {
					t.Errorf("output contains %q", s)
				}
			}
		})
	}
}
