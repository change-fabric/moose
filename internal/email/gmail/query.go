package gmail

import (
	"strconv"
	"strings"

	"github.com/change-fabric/moose/internal/email/provider"
)

// systemMailboxes are Gmail's built-in labels, searched with in:.
var systemMailboxes = map[string]string{
	"INBOX": "inbox", "SENT": "sent", "DRAFT": "drafts", "TRASH": "trash",
	"SPAM": "spam", "STARRED": "starred", "IMPORTANT": "important",
}

// CompileQuery turns a neutral Query into Gmail search syntax. Every user
// value is emitted as one quoted phrase with quotes and backslashes removed, so
// input such as `x OR in:anywhere` cannot add operators. Mailbox, paging and
// Limit are not part of the string: Mailbox compiles to an in:/label: term,
// and the caller applies Limit and Cursor as request parameters.
func CompileQuery(q provider.Query) string {
	var terms []string
	add := func(op, v string) {
		if v = clean(v); v != "" {
			terms = append(terms, op+quote(v))
		}
	}
	add("from:", q.From)
	add("to:", q.To)
	add("subject:", q.Subject)
	if v := clean(q.Text); v != "" {
		terms = append(terms, quote(v))
	}
	if !q.After.IsZero() {
		terms = append(terms, "after:"+strconv.FormatInt(q.After.Unix(), 10))
	}
	if !q.Before.IsZero() {
		terms = append(terms, "before:"+strconv.FormatInt(q.Before.Unix(), 10))
	}
	if q.HasAttachment {
		terms = append(terms, "has:attachment")
	}
	if q.Unread {
		terms = append(terms, "is:unread")
	}
	if m := clean(q.Mailbox); m != "" {
		if in, ok := systemMailboxes[strings.ToUpper(m)]; ok {
			terms = append(terms, "in:"+in)
		} else {
			terms = append(terms, "label:"+quote(m))
		}
	}
	return strings.Join(terms, " ")
}

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

func quote(s string) string { return `"` + s + `"` }
