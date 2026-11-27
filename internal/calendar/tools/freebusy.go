package tools

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/change-fabric/moose/internal/accounts/fanout"
	"github.com/change-fabric/moose/internal/accounts/store"
	"github.com/change-fabric/moose/internal/audit"
	"github.com/change-fabric/moose/internal/calendar/provider"
	"github.com/change-fabric/moose/internal/mcpkit"
)

// Subject kinds in a free/busy answer.
const (
	SubjectCalendar = "calendar"
	SubjectPerson   = "person"
)

// ReasonOwnCalendarOnly is why another person's availability is missing from a
// personal Microsoft account's answer.
const ReasonOwnCalendarOnly = "own_calendar_only"

// FreeBusyIn is the calendar_free_busy input.
type FreeBusyIn struct {
	Account fanout.Refs `json:"account,omitempty"`
	// Calendars are composite calendar ids of the accounts' own calendars.
	Calendars []string `json:"calendars,omitempty"`
	// Emails are other people whose availability is wanted.
	Emails []string `json:"emails,omitempty"`
	Start  string   `json:"start,omitempty"`
	End    string   `json:"end,omitempty"`
	Zone   string   `json:"zone,omitempty"`
}

// BusyOut is the busy time of one calendar or person, as seen by one account.
type BusyOut struct {
	Account AccountRef `json:"account"`
	// Subject is a composite calendar id or a person's address.
	Subject string `json:"subject"`
	Kind    string `json:"kind"`
	// Busy intervals are in UTC, merged, half open.
	Busy []provider.Interval `json:"busy"`
}

// UnavailableOut is a subject whose availability could not be read. It is never
// to be treated as free.
type UnavailableOut struct {
	Account AccountRef `json:"account"`
	Subject string     `json:"subject"`
	Reason  string     `json:"reason"`
}

// FreeBusyOut is the calendar_free_busy output.
type FreeBusyOut struct {
	Busy        []BusyOut        `json:"busy"`
	Unavailable []UnavailableOut `json:"unavailable,omitempty"`
	Accounts    []AccountReport  `json:"accounts"`
	Hint        string           `json:"hint,omitempty"`
}

func (d Deps) registerFreeBusy(s *sdk.Server) {
	str := func(desc string) *jsonschema.Schema { return &jsonschema.Schema{Type: "string", Description: desc} }
	mcpkit.AddTool(s, &sdk.Tool{
		Name: ToolFreeBusy,
		Description: "When calendars and people are busy in a window (at most 62 days, default the next 7 days). " +
			"Own calendars default to each account's primary; `emails` adds other people, asked of the first account that can see them. " +
			"Busy intervals are merged and in UTC. A calendar or person that cannot be read is listed in `unavailable` and is never to be treated as free. " +
			"A personal Microsoft account can answer for its own calendar only: it reports `degraded: own_calendar_only` in `accounts`, and people it could not look up are listed in `unavailable` with that reason.",
		InputSchema: &jsonschema.Schema{
			Type: "object",
			Properties: map[string]*jsonschema.Schema{
				"account":   fanout.RefsSchema(fanout.RefsHelp),
				"calendars": {Type: "array", Items: &jsonschema.Schema{Type: "string"}, Description: "Own calendar ids from calendar_list_calendars. Omit for each account's primary calendar."},
				"emails":    {Type: "array", Items: &jsonschema.Schema{Type: "string"}, Description: fmt.Sprintf("Addresses of other people, at most %d.", maxPeople)},
				"start":     str("Window start: RFC 3339 or YYYY-MM-DD. Default now."),
				"end":       str("Window end: RFC 3339 or YYYY-MM-DD. Default 7 days after start."),
				"zone":      str("IANA zone for bare dates. Default UTC."),
			},
		},
	}, mcpkit.ReadOnly(), d.freeBusy)
}

// busyQuery is one free/busy fan-out: which accounts, which calendars, which
// people, and who asks about the people.
type busyQuery struct {
	picked     []store.Account
	byAccount  map[string][]string
	emails     []string
	start, end time.Time
}

// busyResult is the merged outcome of a busyQuery.
type busyResult struct {
	busy        []BusyOut
	unavailable []UnavailableOut
	reports     []AccountReport
}

// cleanEmails trims, lowercases, de-duplicates and validates the people list.
func cleanEmails(in []string) ([]string, error) {
	if len(in) > maxPeople {
		return nil, fmt.Errorf("at most %d people per call", maxPeople)
	}
	seen := map[string]bool{}
	var out []string
	for _, e := range in {
		e = strings.ToLower(strings.TrimSpace(e))
		if e == "" {
			continue
		}
		if len(e) > 254 || !strings.Contains(e, "@") || strings.ContainsAny(e, " \t\r\n<>,;\"") {
			return nil, fmt.Errorf("%q is not an address", clip(e, 60))
		}
		if !seen[e] {
			seen[e] = true
			out = append(out, e)
		}
	}
	return out, nil
}

// splitCalendars groups composite calendar ids by account and narrows picked to
// the accounts named. A calendar of an account that is not among picked is an
// account_not_found failure.
func splitCalendars(picked []store.Account, calendars []string) ([]store.Account, map[string][]string, *fanout.NotFoundError, bool) {
	byAccount := map[string][]string{}
	if len(calendars) == 0 {
		return picked, byAccount, nil, true
	}
	keep := map[string]bool{}
	for _, c := range calendars {
		accID, calID, ok := parseCalendarID(c)
		if !ok {
			return nil, nil, nil, false
		}
		byAccount[accID] = append(byAccount[accID], calID)
		keep[accID] = true
	}
	var narrowed []store.Account
	for _, a := range picked {
		if keep[a.ID] {
			narrowed = append(narrowed, a)
			delete(keep, a.ID)
		}
	}
	if len(keep) > 0 {
		var missing []string
		for id := range keep {
			missing = append(missing, id)
		}
		sort.Strings(missing)
		return nil, nil, &fanout.NotFoundError{Ref: missing[0], Candidates: fanout.Candidates(picked)}, true
	}
	return narrowed, byAccount, nil, true
}

// askBusy runs the free/busy fan-out. People are asked about by the first
// account (in picked order) whose provider can see them; every other account
// answers for its own calendars only. When no account can see other people,
// each person is reported unavailable with own_calendar_only.
func (d Deps) askBusy(ctx context.Context, q busyQuery) busyResult {
	var asker string
	if len(q.emails) > 0 {
		for _, a := range q.picked {
			if a.Status == store.StatusOK && granted(a) && seesOthers(a) {
				asker = a.ID
				break
			}
		}
	}
	type answer struct {
		busy        []BusyOut
		unavailable []UnavailableOut
	}
	results, reports, _ := fanRun(ctx, d, q.picked, func(ctx context.Context, a store.Account, p provider.Provider) (fanout.PerAccountResult[answer], error) {
		fq := provider.FreeBusyQuery{CalendarIDs: q.byAccount[a.ID], Start: q.start, End: q.end}
		if a.ID == asker {
			fq.Emails = q.emails
		}
		var res provider.FreeBusyResult
		if err := d.retry429(ctx, func() (e error) { res, e = p.FreeBusy(ctx, fq); return }); err != nil {
			return fanout.PerAccountResult[answer]{}, err
		}
		ans := answer{}
		subjects := make([]string, 0, len(res.Busy))
		for k := range res.Busy {
			subjects = append(subjects, k)
		}
		sort.Strings(subjects)
		for _, k := range subjects {
			kind, subject := SubjectCalendar, calendarID(a, k)
			if isPerson(k, fq) {
				kind, subject = SubjectPerson, plainAddr(k)
			}
			busy := res.Busy[k]
			if busy == nil {
				busy = []provider.Interval{}
			}
			ans.busy = append(ans.busy, BusyOut{Account: refOf(a), Subject: subject, Kind: kind, Busy: busy})
		}
		un := make([]string, 0, len(res.Unavailable))
		for k := range res.Unavailable {
			un = append(un, k)
		}
		sort.Strings(un)
		for _, k := range un {
			subject := plainAddr(k)
			if !isPerson(k, fq) {
				subject = calendarID(a, k)
			}
			ans.unavailable = append(ans.unavailable, UnavailableOut{Account: refOf(a), Subject: subject, Reason: clip(oneLine(res.Unavailable[k]), 120)})
		}
		return fanout.PerAccountResult[answer]{Items: []answer{ans}}, nil
	})

	out := busyResult{reports: withDegraded(reports, q.picked)}
	for _, a := range q.picked {
		for _, ans := range results[a.ID].Items {
			out.busy = append(out.busy, ans.busy...)
			out.unavailable = append(out.unavailable, ans.unavailable...)
		}
	}
	if len(q.emails) > 0 && asker == "" {
		// No account can look people up: say so for each, on the first readable account.
		for _, a := range q.picked {
			if a.Status != store.StatusOK || !granted(a) {
				continue
			}
			for _, e := range q.emails {
				out.unavailable = append(out.unavailable, UnavailableOut{Account: refOf(a), Subject: plainAddr(e), Reason: ReasonOwnCalendarOnly})
			}
			break
		}
	}
	return out
}

func isPerson(key string, q provider.FreeBusyQuery) bool {
	for _, e := range q.Emails {
		if strings.EqualFold(e, key) {
			return true
		}
	}
	return false
}

func (d Deps) freeBusy(ctx context.Context, req *sdk.CallToolRequest, in FreeBusyIn) (*sdk.CallToolResult, FreeBusyOut, error) {
	audit.FromContext(ctx).SetAction(ToolFreeBusy, "", "")
	_, picked, bad := d.pick(ctx, req, in.Account)
	if bad != nil {
		return bad, FreeBusyOut{}, nil
	}
	start, end, _, _, err := d.windowArgs(in.Start, in.End, in.Zone, maxBusyWindow)
	if err != nil {
		return mcpkit.ErrorResult(ctx, "invalid_input", err.Error()), FreeBusyOut{}, nil
	}
	emails, err := cleanEmails(in.Emails)
	if err != nil {
		return mcpkit.ErrorResult(ctx, "invalid_input", err.Error()), FreeBusyOut{}, nil
	}
	narrowed, byAccount, nf, ok := splitCalendars(picked, in.Calendars)
	if !ok {
		return mcpkit.ErrorResult(ctx, "invalid_input", "calendars must be composite ids like acc_xxxx:calendar, as returned by calendar_list_calendars"), FreeBusyOut{}, nil
	}
	if nf != nil {
		return resolveError(ctx, nf), FreeBusyOut{}, nil
	}
	r := d.askBusy(ctx, busyQuery{picked: narrowed, byAccount: byAccount, emails: emails, start: start, end: end})
	out := FreeBusyOut{Busy: r.busy, Unavailable: r.unavailable, Accounts: r.reports, Hint: narrowHint(len(narrowed))}
	if out.Busy == nil {
		out.Busy = []BusyOut{}
	}
	return nil, out, nil
}
