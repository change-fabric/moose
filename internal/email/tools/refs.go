package tools

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"

	"github.com/change-fabric/moose/internal/email/store"
)

// Refs is an account reference argument: one string or a list of strings. Each
// is an account id, an exact address, "all", or "group:<tag>".
type Refs []string

// UnmarshalJSON accepts a string, a list of strings or null.
func (r *Refs) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	switch {
	case bytes.Equal(b, []byte("null")):
		*r = nil
		return nil
	case len(b) > 0 && b[0] == '"':
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		*r = Refs{s}
		return nil
	}
	var list []string
	if err := json.Unmarshal(b, &list); err != nil {
		return fmt.Errorf("account must be a string or a list of strings")
	}
	*r = list
	return nil
}

// refsSchema is the input schema for an account argument.
func refsSchema(desc string) *jsonschema.Schema {
	return &jsonschema.Schema{
		Description: desc,
		AnyOf: []*jsonschema.Schema{
			{Type: "string"},
			{Type: "array", Items: &jsonschema.Schema{Type: "string"}},
		},
	}
}

const refsHelp = "Account id or exact address, \"all\", \"group:<tag>\", or a list of those. Omit for every account."

// NotFoundError says an account reference matched nothing. Candidates lists
// the accounts the caller could have meant; the resolver never guesses.
type NotFoundError struct {
	Ref        string
	Candidates []string
}

func (e *NotFoundError) Error() string {
	msg := fmt.Sprintf("no account matches %q", e.Ref)
	if len(e.Candidates) > 0 {
		msg += "; candidates: " + strings.Join(e.Candidates, ", ")
	}
	return msg
}

// Resolve picks accounts by reference. No references, an empty one or "all"
// is every account. "group:<tag>" is every account carrying that tag
// (case-insensitive) and must match at least one. Anything else must be
// exactly one account's id or address (address compared case-insensitively).
// A list is the union of its items, in the owner's account order, each account
// once. It never guesses: a reference that matches nothing returns a
// NotFoundError listing every account as a candidate.
//
// Resolve does not look at status: callers decide what to do with accounts
// that need reauthorization.
func Resolve(all []store.Account, refs []string) ([]store.Account, error) {
	var want []string
	for _, r := range refs {
		if r = strings.TrimSpace(r); r != "" {
			want = append(want, r)
		}
	}
	if len(want) == 0 {
		return all, nil
	}
	picked := map[string]bool{}
	for _, ref := range want {
		if strings.EqualFold(ref, "all") {
			return all, nil
		}
		n := 0
		if tag, ok := strings.CutPrefix(strings.ToLower(ref), "group:"); ok {
			tag = strings.TrimSpace(tag)
			for _, a := range all {
				for _, g := range a.Groups {
					if strings.EqualFold(g, tag) && tag != "" {
						picked[a.ID] = true
						n++
						break
					}
				}
			}
		} else {
			for _, a := range all {
				if a.ID == ref || strings.EqualFold(a.Address, ref) {
					picked[a.ID] = true
					n++
				}
			}
		}
		if n == 0 {
			return nil, &NotFoundError{Ref: ref, Candidates: candidates(all)}
		}
	}
	out := make([]store.Account, 0, len(picked))
	for _, a := range all {
		if picked[a.ID] {
			out = append(out, a)
		}
	}
	return out, nil
}

func candidates(all []store.Account) []string {
	c := make([]string, 0, len(all))
	for _, a := range all {
		c = append(c, fmt.Sprintf("%s (%s)", a.Address, a.ID))
	}
	sort.Strings(c)
	return c
}
