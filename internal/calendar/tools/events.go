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

// ---- calendar_list_calendars ----

// ListCalendarsIn is the calendar_list_calendars input.
type ListCalendarsIn struct {
	Account fanout.Refs `json:"account,omitempty"`
}

// CalendarItem is one calendar of one account. Its name comes from outside
// (a shared calendar is named by whoever owns it), so it travels inside Content.
type CalendarItem struct {
	// ID is the composite calendar id, "acc_x:...", to pass to calendar_list_events.
	ID       string     `json:"id"`
	Account  AccountRef `json:"account"`
	Primary  bool       `json:"primary"`
	Role     string     `json:"role"`
	CanWrite bool       `json:"can_write"`
	TimeZone string     `json:"time_zone,omitempty"`
	Content  string     `json:"content"`
}

// ListCalendarsOut is the calendar_list_calendars output.
type ListCalendarsOut struct {
	Calendars []CalendarItem  `json:"calendars"`
	Accounts  []AccountReport `json:"accounts"`
	Hint      string          `json:"hint,omitempty"`
}

func (d Deps) registerListCalendars(s *sdk.Server) {
	mcpkit.AddTool(s, &sdk.Tool{
		Name: ToolListCalendars,
		Description: "List the calendars of the connected accounts, the primary one first for each. " +
			"Omit account to cover every account whose calendar access is granted; the others are reported in `accounts` and skipped. " +
			"Each calendar's name is inside an untrusted wrapper. " + contentWrapperNote,
		InputSchema: &jsonschema.Schema{
			Type:       "object",
			Properties: map[string]*jsonschema.Schema{"account": fanout.RefsSchema(fanout.RefsHelp)},
		},
	}, mcpkit.ReadOnly(), func(ctx context.Context, req *sdk.CallToolRequest, in ListCalendarsIn) (*sdk.CallToolResult, ListCalendarsOut, error) {
		audit.FromContext(ctx).SetAction(ToolListCalendars, "", "")
		_, picked, bad := d.pick(ctx, req, in.Account)
		if bad != nil {
			return bad, ListCalendarsOut{}, nil
		}
		results, reports, _ := fanRun(ctx, d, picked, func(ctx context.Context, a store.Account, p provider.Provider) (fanout.PerAccountResult[CalendarItem], error) {
			var cals []provider.Calendar
			if err := d.retry429(ctx, func() (e error) { cals, e = p.ListCalendars(ctx); return }); err != nil {
				return fanout.PerAccountResult[CalendarItem]{}, err
			}
			var r fanout.PerAccountResult[CalendarItem]
			for _, c := range cals {
				prov := mcpkit.Provenance("account", a.Address, "account_id", a.ID, "calendar_id", calendarID(a, c.ID))
				r.Items = append(r.Items, CalendarItem{
					ID: calendarID(a, c.ID), Account: refOf(a), Primary: c.Primary, Role: c.Role, CanWrite: c.CanWrite(), TimeZone: c.TimeZone,
					Content: mcpkit.Untrusted(prov, "Name: "+oneLine(clip(c.Name, 200))),
				})
			}
			return r, nil
		})
		out := ListCalendarsOut{Calendars: []CalendarItem{}, Accounts: withDegraded(reports, picked), Hint: narrowHint(len(picked))}
		for _, a := range picked {
			out.Calendars = append(out.Calendars, results[a.ID].Items...)
		}
		return nil, out, nil
	})
}

// withDegraded marks each report whose account is limited to its own calendar.
func withDegraded(reports []AccountReport, picked []store.Account) []AccountReport {
	byID := map[string]store.Account{}
	for _, a := range picked {
		byID[a.ID] = a
	}
	for i := range reports {
		reports[i].Degraded = degradedOf(byID[reports[i].AccountID])
	}
	return reports
}

// ---- event rendering ----

// AttendeeOut is one invitee with only fields that are ours or reduced to a
// plain address. The attendee's name is outside content and appears inside the
// wrapped text instead.
type AttendeeOut struct {
	Email    string `json:"email"`
	Response string `json:"response,omitempty"`
	Optional bool   `json:"optional,omitempty"`
	// External marks an address outside the account's own domain.
	External bool `json:"external,omitempty"`
	Self     bool `json:"self,omitempty"`
}

// RecurrenceOut is a recurrence in the neutral subset. A rule outside the
// subset has Native true and its text inside Content.
type RecurrenceOut struct {
	Freq     string   `json:"freq,omitempty"`
	Interval int      `json:"interval,omitempty"`
	ByDay    []string `json:"by_day,omitempty"`
	Until    string   `json:"until,omitempty"`
	Count    int      `json:"count,omitempty"`
	Native   bool     `json:"native,omitempty"`
}

// EventOut is one event. Title, description, location, organizer and attendee
// names, the join link and a native recurrence rule are all from outside, so
// they travel inside Content; every other field is ours or reduced to plain
// characters.
type EventOut struct {
	// ID is the composite event id, "acc_x:...", for calendar_get_event.
	ID string `json:"id"`
	// CalendarID is the composite calendar id.
	CalendarID string     `json:"calendar_id"`
	Account    AccountRef `json:"account"`
	// SeriesID is the composite id of the recurring series' master, set on
	// occurrences and the master itself.
	SeriesID        string              `json:"series_id,omitempty"`
	Start           provider.EventTime  `json:"start"`
	End             provider.EventTime  `json:"end"`
	AllDay          bool                `json:"all_day"`
	OriginalStart   *provider.EventTime `json:"original_start,omitempty"`
	Status          string              `json:"status,omitempty"`
	Organizer       string              `json:"organizer,omitempty"`
	OrganizerTrust  string              `json:"organizer_trust"`
	AttendeeCount   int                 `json:"attendee_count"`
	Attendees       []AttendeeOut       `json:"attendees,omitempty"`
	Video           bool                `json:"video,omitempty"`
	Recurrence      *RecurrenceOut      `json:"recurrence,omitempty"`
	ReminderMinutes *int                `json:"reminder_minutes,omitempty"`
	SelfResponse    string              `json:"self_response,omitempty"`
	Content         string              `json:"content"`
	Truncated       bool                `json:"truncated,omitempty"`
}

// organizerTrust resolves organizer trust from the shared contacts store: an
// organizer the owner trusts on mail is trusted on calendar. It reads only; an
// unknown address is new and is not recorded.
type organizerTrust struct {
	d     Deps
	a     store.Account
	cache map[string]string
}

func (d Deps) newOrganizerTrust(a store.Account) *organizerTrust {
	return &organizerTrust{d: d, a: a, cache: map[string]string{}}
}

func (t *organizerTrust) of(ctx context.Context, addr string) string {
	addr = strings.ToLower(strings.TrimSpace(addr))
	if addr == "" {
		return store.TrustNew
	}
	if v, ok := t.cache[addr]; ok {
		return v
	}
	v := store.TrustNew
	switch {
	case strings.EqualFold(addr, t.a.Address):
		v = "self"
	default:
		if c, err := t.d.Store.GetContact(ctx, t.a.ID, addr); err == nil {
			v = c.Trust
		}
	}
	t.cache[addr] = v
	return v
}

func domainOf(addr string) string {
	if i := strings.LastIndex(addr, "@"); i >= 0 {
		return strings.ToLower(strings.TrimSpace(addr[i+1:]))
	}
	return ""
}

func eventProvenance(a store.Account, id, organizer, trust string) string {
	return mcpkit.Provenance(
		"organizer_trust", trust,
		"account", a.Address,
		"account_id", a.ID,
		"event_id", id,
		"organizer", organizer,
	)
}

func recurrenceOut(r *provider.Recurrence) *RecurrenceOut {
	if r == nil {
		return nil
	}
	if r.IsOpaque() {
		return &RecurrenceOut{Native: true}
	}
	return &RecurrenceOut{Freq: r.Freq, Interval: r.Interval, ByDay: r.ByDay, Until: r.Until, Count: r.Count}
}

// buildEvent renders one event. full adds the description (up to MaxDescription),
// the attendee names and the join link; the summary form used in lists carries
// a short description snippet only. maxContent caps the wrapped output.
func buildEvent(a store.Account, ev provider.Event, trust string, full bool, maxContent int) EventOut {
	id := eventID(a, ev.CalendarID, ev.ID)
	selfDomain := domainOf(a.Address)

	var b strings.Builder
	b.WriteString("Title: " + oneLine(clip(ev.Title, 300)) + "\n")
	if ev.Location != "" {
		b.WriteString("Location: " + oneLine(clip(ev.Location, 300)) + "\n")
	}
	if ev.Organizer.Email != "" || ev.Organizer.Name != "" {
		b.WriteString("Organizer: " + fmtPerson(ev.Organizer.Name, ev.Organizer.Email) + "\n")
	}
	atts := make([]AttendeeOut, 0, len(ev.Attendees))
	for _, at := range ev.Attendees {
		d := domainOf(at.Email)
		atts = append(atts, AttendeeOut{
			Email: plainAddr(at.Email), Response: at.Response, Optional: at.Optional, Self: at.Self,
			External: d != "" && selfDomain != "" && d != selfDomain,
		})
		if full {
			b.WriteString("Attendee: " + fmtPerson(at.Name, at.Email) + "\n")
		}
	}
	if full && ev.VideoURL != "" {
		b.WriteString("Video: " + oneLine(clip(ev.VideoURL, 500)) + "\n")
	}
	if ev.Recurrence != nil && ev.Recurrence.IsOpaque() {
		b.WriteString("Recurrence (native): " + oneLine(clip(ev.Recurrence.Native, 500)) + "\n")
	}
	desc := strings.ReplaceAll(ev.Description, "\r\n", "\n")
	clipped := false
	switch {
	case strings.TrimSpace(desc) == "":
	case full:
		if len(desc) > MaxDescription {
			desc, clipped = strings.ToValidUTF8(desc[:MaxDescription], ""), true
		}
		b.WriteString("\n" + desc)
	default:
		b.WriteString("Description: " + oneLine(clip(desc, summaryDescription)) + "\n")
	}

	organizer := plainAddr(ev.Organizer.Email)
	content, cut := mcpkit.UntrustedCapped(eventProvenance(a, id, ev.Organizer.Email, trust), strings.TrimRight(b.String(), "\n"), maxContent)
	out := EventOut{
		ID: id, CalendarID: calendarID(a, ev.CalendarID), Account: refOf(a),
		Start: ev.Start, End: ev.End, AllDay: ev.Start.IsAllDay(), OriginalStart: ev.OriginalStart, Status: ev.Status,
		Organizer: organizer, OrganizerTrust: trust, AttendeeCount: len(ev.Attendees), Video: ev.Video,
		Recurrence: recurrenceOut(ev.Recurrence), ReminderMinutes: ev.ReminderMinutes, SelfResponse: ev.SelfResponse,
		Content: content, Truncated: cut || clipped,
	}
	if ev.SeriesID != "" {
		out.SeriesID = eventID(a, ev.CalendarID, ev.SeriesID)
	}
	if full {
		out.Attendees = atts
	}
	return out
}

// startInstant orders events: an all-day date sorts as midnight UTC.
func startInstant(e EventOut) time.Time {
	if e.Start.IsAllDay() {
		t, _ := time.Parse(provider.DateLayout, e.Start.Date)
		return t
	}
	return e.Start.DateTime
}

// ---- calendar_list_events ----

// ListEventsIn is the calendar_list_events input.
type ListEventsIn struct {
	Account fanout.Refs `json:"account,omitempty"`
	// Calendars are composite calendar ids from calendar_list_calendars. Omitted,
	// each account's primary calendar is read.
	Calendars []string `json:"calendars,omitempty"`
	Start     string   `json:"start,omitempty"`
	End       string   `json:"end,omitempty"`
	Zone      string   `json:"zone,omitempty"`
	Text      string   `json:"text,omitempty"`
	Limit     int      `json:"limit,omitempty"`
	Cursor    string   `json:"cursor,omitempty"`
}

// ListEventsOut is the calendar_list_events output.
type ListEventsOut struct {
	Events   []EventOut      `json:"events"`
	Accounts []AccountReport `json:"accounts"`
	// Cursor resumes the listing: pass it back with the same arguments. It is
	// absent when every account is exhausted.
	Cursor string `json:"cursor,omitempty"`
	Hint   string `json:"hint,omitempty"`
}

func (d Deps) registerListEvents(s *sdk.Server) {
	str := func(desc string) *jsonschema.Schema { return &jsonschema.Schema{Type: "string", Description: desc} }
	mcpkit.AddTool(s, &sdk.Tool{
		Name: ToolListEvents,
		Description: "List events in a time window across accounts, recurring events expanded into their occurrences, soonest first. " +
			"With no calendars it reads each account's primary calendar. The window defaults to the next 7 days (at most 366 days). " +
			"`accounts` says how each account fared (ok, timeout, error, needs_reauth, skipped with calendar_not_granted). " +
			"Pages are ordered within each account; if there is more, pass `cursor` back with the same arguments. " +
			"Times carry their IANA zone; all-day events carry a date whose end is exclusive. " +
			"A recurrence outside the simple subset (frequency, interval, weekdays, until or count) comes back as native text. " + contentWrapperNote,
		InputSchema: &jsonschema.Schema{
			Type: "object",
			Properties: map[string]*jsonschema.Schema{
				"account":   fanout.RefsSchema(fanout.RefsHelp),
				"calendars": {Type: "array", Items: &jsonschema.Schema{Type: "string"}, Description: "Calendar ids from calendar_list_calendars. Omit for each account's primary calendar."},
				"start":     str("Window start: RFC 3339 or YYYY-MM-DD. Default now."),
				"end":       str("Window end (exclusive): RFC 3339 or YYYY-MM-DD. Default 7 days after start."),
				"zone":      str("IANA zone such as Europe/Berlin: events are shown in it and a bare date means midnight in it. Default UTC for dates, each event's own zone for times."),
				"text":      str("Only events matching this text in the title, description, location or attendees."),
				"limit":     {Type: "integer", Description: fmt.Sprintf("Events per account, 1 to %d. Default %d.", maxListLimit, defaultListLimit)},
				"cursor":    str("The cursor from the previous page of this same listing."),
			},
		},
	}, mcpkit.ReadOnly(), d.listEvents)
}

func (d Deps) listEvents(ctx context.Context, req *sdk.CallToolRequest, in ListEventsIn) (*sdk.CallToolResult, ListEventsOut, error) {
	audit.FromContext(ctx).SetAction(ToolListEvents, "", "")
	owner, picked, bad := d.pick(ctx, req, in.Account)
	if bad != nil {
		return bad, ListEventsOut{}, nil
	}
	if len(in.Text) > maxField {
		return mcpkit.ErrorResult(ctx, "invalid_input", fmt.Sprintf("text is longer than %d bytes", maxField)), ListEventsOut{}, nil
	}
	start, end, _, zone, err := d.windowArgs(in.Start, in.End, in.Zone, maxListWindow)
	if err != nil {
		return mcpkit.ErrorResult(ctx, "invalid_input", err.Error()), ListEventsOut{}, nil
	}
	limit := in.Limit
	switch {
	case limit < 0:
		return mcpkit.ErrorResult(ctx, "invalid_input", "limit must be positive"), ListEventsOut{}, nil
	case limit == 0:
		limit = defaultListLimit
	default:
		limit = min(limit, maxListLimit)
	}

	// Calendars named by id narrow the accounts to the ones they belong to.
	picked, byAccount, nf, ok := splitCalendars(picked, in.Calendars)
	if !ok {
		return mcpkit.ErrorResult(ctx, "invalid_input", "calendars must be composite ids like acc_xxxx:calendar, as returned by calendar_list_calendars"), ListEventsOut{}, nil
	}
	if nf != nil {
		return resolveError(ctx, nf), ListEventsOut{}, nil
	}

	cals := append([]string(nil), in.Calendars...)
	sort.Strings(cals)
	fp := fanout.Fingerprint(struct {
		C    []string
		S, E string
		Z, T string
	}{cals, start.UTC().Format(time.RFC3339), end.UTC().Format(time.RFC3339), zone, strings.TrimSpace(in.Text)})

	pos := map[string]string{}
	targets := picked
	if in.Cursor != "" {
		c, err := d.openCursor(in.Cursor, owner, fp)
		if err != nil {
			return mcpkit.ErrorResult(ctx, "invalid_cursor", err.Error()), ListEventsOut{}, nil
		}
		pos, targets = c.Pos, nil
		for _, a := range picked {
			if _, ok := pos[a.ID]; ok {
				targets = append(targets, a)
			}
		}
	}

	live := 0
	for _, a := range targets {
		if a.Status == store.StatusOK && granted(a) {
			live++
		}
	}
	per := limit
	if room := maxPageEvents / max(live, 1); per > room {
		per = max(room, 1)
	}

	results, reports, keep := fanRun(ctx, d, targets, func(ctx context.Context, a store.Account, p provider.Provider) (fanout.PerAccountResult[EventOut], error) {
		q := provider.ListEventsQuery{
			CalendarIDs: byAccount[a.ID], Start: start, End: end, Zone: zone, Text: strings.TrimSpace(in.Text),
			PageToken: pos[a.ID], Limit: per,
		}
		var page provider.EventPage
		if err := d.retry429(ctx, func() (e error) { page, e = p.ListEvents(ctx, q); return }); err != nil {
			return fanout.PerAccountResult[EventOut]{}, err
		}
		tr := d.newOrganizerTrust(a)
		r := fanout.PerAccountResult[EventOut]{Next: page.NextPageToken}
		for _, ev := range page.Events {
			if zone != "" {
				ev = provider.InZone(ev, zone)
			}
			r.Items = append(r.Items, buildEvent(a, ev, tr.of(ctx, ev.Organizer.Email), false, MaxEventContent))
		}
		return r, nil
	})

	out := ListEventsOut{Events: []EventOut{}, Accounts: withDegraded(reports, targets), Hint: narrowHint(len(picked))}
	newPos := map[string]string{}
	for _, a := range targets {
		if r, ok := results[a.ID]; ok {
			out.Events = append(out.Events, r.Items...)
			if r.Next != "" {
				newPos[a.ID] = r.Next
			}
		} else if keep[a.ID] {
			newPos[a.ID] = pos[a.ID] // a timed-out or throttled account keeps its place
		}
	}
	sort.SliceStable(out.Events, func(i, j int) bool {
		a, b := out.Events[i], out.Events[j]
		if x, y := startInstant(a), startInstant(b); !x.Equal(y) {
			return x.Before(y)
		}
		return a.ID < b.ID
	})
	if len(newPos) > 0 {
		out.Cursor = d.sealCursor(fanout.CursorBody{Owner: owner, Query: fp, Pos: newPos})
	}
	return nil, out, nil
}

// ---- calendar_get_event ----

// GetEventIn is the calendar_get_event input.
type GetEventIn struct {
	ID string `json:"id"`
}

func (d Deps) registerGetEvent(s *sdk.Server) {
	mcpkit.AddTool(s, &sdk.Tool{
		Name: ToolGetEvent,
		Description: "Read one event or occurrence by its id (the composite acc_...:calendar|event id a listing returns): " +
			"times with their zone, attendees with their answers and whether they are external, the recurrence, the reminder, and whether there is a video meeting. " +
			fmt.Sprintf("The description (capped at %d KiB), attendee names and join link come back inside an untrusted wrapper. ", MaxDescription>>10) + contentWrapperNote,
		InputSchema: &jsonschema.Schema{
			Type:       "object",
			Required:   []string{"id"},
			Properties: map[string]*jsonschema.Schema{"id": {Type: "string", Description: "Composite event id from calendar_list_events."}},
		},
	}, mcpkit.ReadOnly(), func(ctx context.Context, req *sdk.CallToolRequest, in GetEventIn) (*sdk.CallToolResult, EventOut, error) {
		audit.FromContext(ctx).SetAction(ToolGetEvent, "", "")
		accID, ref, ok := parseEventID(in.ID)
		if !ok {
			return mcpkit.ErrorResult(ctx, "invalid_input", "id must be a composite id like acc_xxxx:calendar|event, as returned by calendar_list_events"), EventOut{}, nil
		}
		a, bad := d.accountByID(ctx, req, accID)
		if bad != nil {
			return bad, EventOut{}, nil
		}
		p, err := d.Providers.CalendarProvider(ctx, a)
		if err != nil {
			return d.providerError(ctx, err), EventOut{}, nil
		}
		ctx, cancel := context.WithTimeout(ctx, d.Fanout.Overall)
		defer cancel()
		var ev provider.Event
		if err := d.retry429(ctx, func() (e error) { ev, e = p.GetEvent(ctx, ref); return }); err != nil {
			return d.providerError(ctx, err), EventOut{}, nil
		}
		if ev.CalendarID == "" {
			ev.CalendarID = ref.CalendarID
		}
		tr := d.newOrganizerTrust(a)
		return nil, buildEvent(a, ev, tr.of(ctx, ev.Organizer.Email), true, MaxEventContent), nil
	})
}
