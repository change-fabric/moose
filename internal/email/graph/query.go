package graph

import (
	"fmt"
	"strings"
	"time"

	"github.com/change-fabric/moose/internal/email/provider"
)

// Compiled is a neutral Query rendered for Graph. Exactly one of Search and
// Filter drives the request: Search (KQL) when the query has free text,
// Filter (OData) otherwise. OrderBy is set only with Filter, because $search
// results come back ranked by Graph and cannot be re-ordered.
type Compiled struct {
	// Search is the KQL string, without the outer quotes $search needs.
	Search string
	// Filter is the OData $filter expression.
	Filter string
	// OrderBy is the $orderby expression, empty with Search.
	OrderBy string
}

// CompileQuery turns a neutral Query into Graph syntax (DialectGraph). Mailbox,
// Limit and Cursor are not part of it: Mailbox picks the request path, and the
// caller applies the page parameters.
//
// Every user value is cleaned of quotes, backslashes and control characters and
// then emitted as one quoted phrase (KQL) or one quoted literal (OData), so
// input such as `x OR from:anyone` cannot add operators.
//
// With Text, From, To, Subject, the date bounds and HasAttachment all compile to
// KQL properties. Unread cannot: whether $search and $filter combine is an
// UNVERIFIED spike row (docs/microsoft-graph-spike.md, "search-filter"), so
// Unread together with Text is refused with ErrInvalidQuery rather than
// guessed at or silently post-filtered into short pages.
//
// Without Text everything compiles to $filter. Exchange rejects a filter whose
// sort property is not first (InefficientFilter), so the filter always opens
// with a receivedDateTime bound, the real After when given or the epoch.
func CompileQuery(q provider.Query) (Compiled, error) {
	text := clean(q.Text)
	if text != "" {
		if q.Unread {
			return Compiled{}, fmt.Errorf("%w: unread cannot be combined with a text search on Microsoft accounts", provider.ErrInvalidQuery)
		}
		var terms []string
		add := func(op, v string) {
			if v = clean(v); v != "" {
				terms = append(terms, op+`"`+v+`"`)
			}
		}
		terms = append(terms, `"`+text+`"`)
		add("from:", q.From)
		add("to:", q.To)
		add("subject:", q.Subject)
		if !q.After.IsZero() {
			terms = append(terms, "received>="+q.After.UTC().Format("2006-01-02"))
		}
		if !q.Before.IsZero() {
			terms = append(terms, "received<="+q.Before.UTC().Format("2006-01-02"))
		}
		if q.HasAttachment {
			terms = append(terms, "hasattachment:true")
		}
		return Compiled{Search: strings.Join(terms, " ")}, nil
	}

	after := "1970-01-01T00:00:00Z"
	if !q.After.IsZero() {
		after = q.After.UTC().Format(time.RFC3339)
	}
	clauses := []string{"receivedDateTime ge " + after}
	if !q.Before.IsZero() {
		clauses = append(clauses, "receivedDateTime lt "+q.Before.UTC().Format(time.RFC3339))
	}
	if v := clean(q.From); v != "" {
		clauses = append(clauses, "from/emailAddress/address eq "+odataString(v))
	}
	if v := clean(q.To); v != "" {
		clauses = append(clauses, "toRecipients/any(r:r/emailAddress/address eq "+odataString(v)+")")
	}
	if v := clean(q.Subject); v != "" {
		clauses = append(clauses, "contains(subject,"+odataString(v)+")")
	}
	if q.HasAttachment {
		clauses = append(clauses, "hasAttachments eq true")
	}
	if q.Unread {
		clauses = append(clauses, "isRead eq false")
	}
	return Compiled{Filter: strings.Join(clauses, " and "), OrderBy: "receivedDateTime desc"}, nil
}

// odataString renders v (already cleaned) as an OData string literal.
func odataString(v string) string { return "'" + strings.ReplaceAll(v, "'", "''") + "'" }

// clean drops characters that could end a quoted phrase and collapses whitespace.
func clean(s string) string {
	s = strings.Map(func(r rune) rune {
		if r == '"' || r == '\\' {
			return -1
		}
		if r < ' ' || r == 0x7f {
			return ' '
		}
		return r
	}, s)
	return strings.Join(strings.Fields(s), " ")
}
