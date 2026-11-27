package tools

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode"

	"github.com/google/jsonschema-go/jsonschema"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/change-fabric/moose/internal/accounts/fanout"
	"github.com/change-fabric/moose/internal/accounts/store"
	"github.com/change-fabric/moose/internal/audit"
	"github.com/change-fabric/moose/internal/calendar/provider"
	"github.com/change-fabric/moose/internal/mcpkit"
)

// Tool names of the write tools.
const (
	ToolCreateEvent     = "calendar_create_event"
	ToolUpdateEvent     = "calendar_update_event"
	ToolDeleteEvent     = "calendar_delete_event"
	ToolRespondToInvite = "calendar_respond_to_invite"
)

// CodeInvitesNotEnabled is returned for a write that would notify anyone other
// than the account itself. Self-only events apply directly; events with other
// attendees need the invite approval flow, which a later phase adds.
const CodeInvitesNotEnabled = "invites_not_enabled"

// Caps on write arguments.
const (
	maxTitle     = 500
	maxAttendees = 200
	maxIdemKey   = 128
)

// WriteGuidance is appended to every write tool's description.
const WriteGuidance = "Name the account (an id or exact address) or pass a calendar id from calendar_list_calendars; without either you get needs_account. " +
	"This tool changes an event only when nobody but the account itself is involved: an event with any other attendee, before or after the change, " +
	"returns invites_not_enabled and nothing is written, because Google and Microsoft both email attendees on any change. " +
	"Every update and delete reads the event first and is conditional on the version it read, so a change made by someone else in between is never overwritten."

func str(desc string) *jsonschema.Schema {
	return &jsonschema.Schema{Type: "string", Description: desc}
}

// TimeIn is a time argument: an RFC 3339 instant with its offset plus an IANA
// zone, or an all-day date.
type TimeIn struct {
	DateTime string `json:"date_time,omitempty"`
	Zone     string `json:"zone,omitempty"`
	Date     string `json:"date,omitempty"`
}

func timeSchema(desc string) *jsonschema.Schema {
	return &jsonschema.Schema{
		Type: "object", Description: desc,
		Properties: map[string]*jsonschema.Schema{
			"date_time": str("RFC 3339 with its offset, e.g. 2026-03-04T09:00:00+01:00. Needs zone."),
			"zone":      str("IANA zone the event is anchored to, e.g. Europe/Berlin. Not an abbreviation such as EST."),
			"date":      str("YYYY-MM-DD for an all-day event, instead of date_time and zone. The end date is exclusive."),
		},
	}
}

func (t TimeIn) convert(name string) (provider.EventTime, error) {
	var out provider.EventTime
	switch {
	case t.Date != "" && t.DateTime != "":
		return out, fmt.Errorf("%s: give a date or a date_time, not both", name)
	case t.Date != "":
		out = provider.EventTime{Date: strings.TrimSpace(t.Date), Zone: strings.TrimSpace(t.Zone)}
	case t.DateTime != "":
		at, err := time.Parse(time.RFC3339, strings.TrimSpace(t.DateTime))
		if err != nil {
			return out, fmt.Errorf("%s.date_time must be RFC 3339 with an offset, got %q", name, clip(t.DateTime, 40))
		}
		out = provider.EventTime{DateTime: at, Zone: strings.TrimSpace(t.Zone)}
		if out.Zone != "" {
			if loc, err := provider.LoadZone(out.Zone); err == nil {
				out.DateTime = at.In(loc)
			}
		}
	default:
		return out, fmt.Errorf("%s needs a date or a date_time with a zone", name)
	}
	if err := out.Validate(); err != nil {
		return out, fmt.Errorf("%s: %s", name, strings.TrimPrefix(err.Error(), "provider: invalid time: "))
	}
	return out, nil
}

// RecurrenceIn is the neutral recurrence subset.
type RecurrenceIn struct {
	Freq     string   `json:"freq"`
	Interval int      `json:"interval,omitempty"`
	ByDay    []string `json:"by_day,omitempty"`
	Until    string   `json:"until,omitempty"`
	Count    int      `json:"count,omitempty"`
}

func recurrenceSchema() *jsonschema.Schema {
	return &jsonschema.Schema{
		Type:        "object",
		Description: "A simple repeat rule. Anything more complex is refused.",
		Required:    []string{"freq"},
		Properties: map[string]*jsonschema.Schema{
			"freq":     {Type: "string", Enum: []any{"DAILY", "WEEKLY", "MONTHLY", "YEARLY"}},
			"interval": {Type: "integer", Description: "Every N periods. Default 1."},
			"by_day":   {Type: "array", Items: &jsonschema.Schema{Type: "string", Enum: []any{"MO", "TU", "WE", "TH", "FR", "SA", "SU"}}, Description: "Weekdays, WEEKLY only."},
			"until":    str("Last day of the series, YYYY-MM-DD, inclusive. Use until or count, not both; neither means it never ends."),
			"count":    {Type: "integer", Description: "Number of occurrences."},
		},
	}
}

func (r RecurrenceIn) convert() provider.Recurrence {
	return provider.Recurrence{Freq: strings.ToUpper(strings.TrimSpace(r.Freq)), Interval: r.Interval, ByDay: r.ByDay, Until: strings.TrimSpace(r.Until), Count: r.Count}
}

// AttendeeIn is an invitee to write.
type AttendeeIn struct {
	Email    string `json:"email"`
	Name     string `json:"name,omitempty"`
	Optional bool   `json:"optional,omitempty"`
}

func attendeesSchema(desc string) *jsonschema.Schema {
	return &jsonschema.Schema{
		Type: "array", Description: desc,
		Items: &jsonschema.Schema{Type: "object", Required: []string{"email"}, Properties: map[string]*jsonschema.Schema{
			"email": str("Address."), "name": str("Display name."), "optional": {Type: "boolean"},
		}},
	}
}

func convertAttendees(in []AttendeeIn) []provider.Attendee {
	out := make([]provider.Attendee, 0, len(in))
	for _, a := range in {
		out = append(out, provider.Attendee{Email: strings.TrimSpace(a.Email), Name: strings.TrimSpace(a.Name), Optional: a.Optional})
	}
	return out
}

// ---- shared write helpers ----

// writeTarget resolves the account (and calendar) a write is for. A write never
// guesses: with neither an account nor a calendar it is needs_account.
func (d Deps) writeTarget(ctx context.Context, req *sdk.CallToolRequest, accountRef, calendarArg string) (store.Account, string, *sdk.CallToolResult) {
	owner := d.owner(req)
	if owner == "" {
		return store.Account{}, "", mcpkit.ErrorResult(ctx, "unauthorized", "no signed-in owner for this request")
	}
	if d.Providers == nil {
		return store.Account{}, "", mcpkit.ErrorResult(ctx, "internal", "calendar access is not configured on this server")
	}
	accts, err := d.Store.ListAccounts(ctx, owner)
	if err != nil {
		return store.Account{}, "", mcpkit.ErrorResult(ctx, "internal", "could not list accounts")
	}
	var calAccount, calID string
	if c := strings.TrimSpace(calendarArg); c != "" {
		var ok bool
		if calAccount, calID, ok = parseCalendarID(c); !ok {
			return store.Account{}, "", mcpkit.ErrorResult(ctx, "invalid_input", "calendar must be a composite id like acc_xxxx:calendar, as returned by calendar_list_calendars")
		}
	}
	ref := strings.TrimSpace(accountRef)
	if ref == "" {
		ref = calAccount
	}
	if ref == "" {
		var writable []store.Account
		for _, a := range accts {
			if a.Status == store.StatusOK && granted(a) {
				writable = append(writable, a)
			}
		}
		return store.Account{}, "", mcpkit.ErrorResult(ctx, "needs_account", "name the account (account: an id or exact address) or pass a calendar id; candidates: "+strings.Join(fanout.Candidates(writable), ", "))
	}
	picked, err := fanout.Resolve(accts, []string{ref})
	if err != nil {
		return store.Account{}, "", resolveError(ctx, err)
	}
	if len(picked) != 1 {
		return store.Account{}, "", mcpkit.ErrorResult(ctx, "invalid_input", "name exactly one account by id or address")
	}
	a := picked[0]
	if calAccount != "" && calAccount != a.ID {
		return store.Account{}, "", mcpkit.ErrorResult(ctx, "invalid_input", "the calendar belongs to a different account than the one named")
	}
	if bad := d.usable(ctx, a); bad != nil {
		return store.Account{}, "", bad
	}
	return a, calID, nil
}

// definitelyNotWritten reports a failure that proves the provider changed
// nothing, so an idempotency key may be released for a retry.
func definitelyNotWritten(err error) bool {
	for _, target := range []error{
		provider.ErrRejected, provider.ErrInvalidTime, provider.ErrInvalidRecurrence, provider.ErrUnsupportedHere,
		provider.ErrAuth, provider.ErrNotFound, provider.ErrInvalidQuery, provider.ErrRateLimited, provider.ErrPreconditionFailed,
	} {
		if errors.Is(err, target) {
			return true
		}
	}
	return false
}

func (d Deps) writeError(ctx context.Context, err error) *sdk.CallToolResult {
	switch {
	case errors.Is(err, provider.ErrPreconditionFailed):
		return mcpkit.ErrorResult(ctx, "event_changed", "the event changed while it was being written and nothing was changed; read it again and decide again")
	case errors.Is(err, provider.ErrInvalidRecurrence):
		return mcpkit.ErrorResult(ctx, "invalid_recurrence", "the recurrence is outside what moose can write: freq, interval, by_day (weekly), and until or count")
	case errors.Is(err, provider.ErrInvalidTime):
		return mcpkit.ErrorResult(ctx, "invalid_input", "the times are not usable: "+strings.TrimPrefix(err.Error(), "provider: invalid time: "))
	case errors.Is(err, provider.ErrRejected):
		return mcpkit.ErrorResult(ctx, "rejected", "the provider refused the change; nothing was changed")
	}
	return d.providerError(ctx, err)
}

func invitesNotEnabled(ctx context.Context, what string) *sdk.CallToolResult {
	return mcpkit.ErrorResult(ctx, CodeInvitesNotEnabled,
		what+" involves someone other than the account itself, so it would notify them. Inviting others is not enabled yet; "+
			"nothing was written. To decline an invitation use calendar_respond_to_invite.")
}

// involvesOthers reports whether an event, or a proposed attendee list, has
// anyone but the account: another attendee, or another organizer.
func involvesOthers(self string, ev provider.Event) bool {
	if provider.HasOthers(ev.Attendees, self) {
		return true
	}
	org := strings.TrimSpace(ev.Organizer.Email)
	return org != "" && !ev.Organizer.Self && !strings.EqualFold(org, strings.TrimSpace(self))
}

// isOccurrence reports whether ev is one occurrence of a series.
func isOccurrence(ev provider.Event) bool { return ev.SeriesID != "" && ev.ID != ev.SeriesID }

func isMaster(ev provider.Event) bool { return ev.SeriesID != "" && ev.ID == ev.SeriesID }

func parseScope(s string) (provider.Scope, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "instance":
		return provider.ScopeInstance, nil
	case "series":
		return provider.ScopeSeries, nil
	}
	return "", errors.New(`scope must be "instance" (this occurrence) or "series" (the whole series); "this and following" is not supported`)
}

// plan is what an update or delete is aimed at after the event is read: the
// event as named, the ref and version the write presents, and every event whose
// attendees count for the self-only decision.
type plan struct {
	ref     provider.EventRef
	version string
	seen    []provider.Event
}

// aim reads the event and decides where the write goes. A series written
// through one of its occurrences is aimed at the master with the master's own
// version (the provider's strict path); the attendees of both count.
func (d Deps) aim(ctx context.Context, p provider.Provider, ref provider.EventRef, scope provider.Scope) (plan, error) {
	var cur provider.Event
	if err := d.retry429(ctx, func() (e error) { cur, e = p.GetEvent(ctx, ref); return }); err != nil {
		return plan{}, err
	}
	pl := plan{ref: ref, version: cur.Version, seen: []provider.Event{cur}}
	if scope == provider.ScopeSeries && isOccurrence(cur) {
		mref := provider.EventRef{CalendarID: ref.CalendarID, EventID: cur.SeriesID}
		var m provider.Event
		if err := d.retry429(ctx, func() (e error) { m, e = p.GetEvent(ctx, mref); return }); err != nil {
			return plan{}, err
		}
		pl.ref, pl.version = mref, m.Version
		pl.seen = append(pl.seen, m)
	}
	return pl, nil
}

func (pl plan) involvesOthers(self string, proposed *[]provider.Attendee) bool {
	for _, ev := range pl.seen {
		if involvesOthers(self, ev) {
			return true
		}
	}
	return proposed != nil && provider.HasOthers(*proposed, self)
}

// ---- calendar_create_event ----

// CreateEventIn is the calendar_create_event input.
type CreateEventIn struct {
	Account         string        `json:"account,omitempty"`
	Calendar        string        `json:"calendar,omitempty"`
	Title           string        `json:"title"`
	Description     string        `json:"description,omitempty"`
	Location        string        `json:"location,omitempty"`
	Start           TimeIn        `json:"start"`
	End             TimeIn        `json:"end"`
	Recurrence      *RecurrenceIn `json:"recurrence,omitempty"`
	Attendees       []AttendeeIn  `json:"attendees,omitempty"`
	Video           bool          `json:"video,omitempty"`
	ReminderMinutes *int          `json:"reminder_minutes,omitempty"`
	IdempotencyKey  string        `json:"idempotency_key,omitempty"`
}

// WriteOut is the output of the event write tools.
type WriteOut struct {
	// Status is created, updated or deleted.
	Status string `json:"status"`
	// Replay is true when an idempotency key had already made this event and the
	// existing event is returned without a second write.
	Replay bool `json:"replay,omitempty"`
	// Event is the event after the write (absent after a delete).
	Event *EventOut `json:"event,omitempty"`
	// DeletedID is the composite id of the removed event.
	DeletedID string `json:"deleted_id,omitempty"`
}

func (d Deps) registerCreateEvent(s *sdk.Server) {
	mcpkit.AddTool(s, &sdk.Tool{
		Name: ToolCreateEvent,
		Description: "Create an event on the account's own calendar, applied directly. It must involve only the account itself: no attendees other than the account. " +
			"Supports a simple repeat rule, an online meeting (video: Google Meet, or Teams on a work or school Microsoft account), and one reminder in minutes. " +
			"Pass idempotency_key (any stable string you choose for this one event) so a retry returns the first event instead of making a second. " + WriteGuidance,
		InputSchema: &jsonschema.Schema{
			Type:     "object",
			Required: []string{"title", "start", "end"},
			Properties: map[string]*jsonschema.Schema{
				"account":          str("The account: its id or exact address. Optional when calendar is given."),
				"calendar":         str("Composite calendar id from calendar_list_calendars. Default: the account's primary calendar."),
				"title":            str("Event title."),
				"description":      str("Plain-text description."),
				"location":         str("Location text."),
				"start":            timeSchema("Start."),
				"end":              timeSchema("End (exclusive for an all-day event). Must be after the start and of the same form."),
				"recurrence":       recurrenceSchema(),
				"attendees":        attendeesSchema("Only the account itself may be listed; any other address returns invites_not_enabled."),
				"video":            {Type: "boolean", Description: "Attach an online meeting."},
				"reminder_minutes": {Type: "integer", Description: fmt.Sprintf("One reminder, minutes before the start, 0 to %d.", provider.MaxReminderMinutes)},
				"idempotency_key":  str("A stable key for this create, up to 128 characters."),
			},
		},
	}, mcpkit.Write(), d.createEvent)
}

func (d Deps) createEvent(ctx context.Context, req *sdk.CallToolRequest, in CreateEventIn) (*sdk.CallToolResult, WriteOut, error) {
	audit.FromContext(ctx).SetAction(ToolCreateEvent, "", "")
	a, calID, bad := d.writeTarget(ctx, req, in.Account, in.Calendar)
	if bad != nil {
		return bad, WriteOut{}, nil
	}
	if strings.TrimSpace(in.Title) == "" || len(in.Title) > maxTitle || len(in.Location) > maxTitle || len(in.Description) > MaxDescription ||
		len(in.Attendees) > maxAttendees || len(in.IdempotencyKey) > maxIdemKey {
		return mcpkit.ErrorResult(ctx, "invalid_input", fmt.Sprintf("title is required (at most %d bytes); location at most %d bytes; description at most %d KiB; at most %d attendees; idempotency_key at most %d characters",
			maxTitle, maxTitle, MaxDescription>>10, maxAttendees, maxIdemKey)), WriteOut{}, nil
	}
	ev := provider.EventInput{
		Title: strings.TrimSpace(in.Title), Description: in.Description, Location: strings.TrimSpace(in.Location),
		Attendees: convertAttendees(in.Attendees), Video: in.Video, ReminderMinutes: in.ReminderMinutes,
	}
	var err error
	if ev.Start, err = in.Start.convert("start"); err != nil {
		return mcpkit.ErrorResult(ctx, "invalid_input", err.Error()), WriteOut{}, nil
	}
	if ev.End, err = in.End.convert("end"); err != nil {
		return mcpkit.ErrorResult(ctx, "invalid_input", err.Error()), WriteOut{}, nil
	}
	if in.Recurrence != nil {
		r := in.Recurrence.convert()
		ev.Recurrence = &r
	}
	if err := provider.ValidateEventInput(ev); err != nil {
		return d.writeError(ctx, err), WriteOut{}, nil
	}
	if provider.HasOthers(ev.Attendees, a.Address) {
		return invitesNotEnabled(ctx, "this event"), WriteOut{}, nil
	}

	p, err := d.Providers.CalendarProvider(ctx, a)
	if err != nil {
		return d.providerError(ctx, err), WriteOut{}, nil
	}
	ctx, cancel := context.WithTimeout(ctx, d.Fanout.Overall)
	defer cancel()
	if calID == "" {
		var cals []provider.Calendar
		if err := d.retry429(ctx, func() (e error) { cals, e = p.ListCalendars(ctx); return }); err != nil {
			return d.writeError(ctx, err), WriteOut{}, nil
		}
		for _, c := range cals {
			if c.Primary {
				calID = c.ID
				break
			}
		}
		if calID == "" {
			return mcpkit.ErrorResult(ctx, "not_found", "the account has no primary calendar; pass a calendar id"), WriteOut{}, nil
		}
	}

	opts := provider.CreateOptions{}
	key := strings.TrimSpace(in.IdempotencyKey)
	if key != "" {
		hash := createHash(calID, ev)
		rec, created, err := d.Store.ReserveCalendarIdempotency(ctx, store.CalendarIdempotency{AccountID: a.ID, Key: key, RequestHash: hash})
		switch {
		case errors.Is(err, store.ErrIdempotencyKeyReused):
			return mcpkit.ErrorResult(ctx, "idempotency_key_reused", "that idempotency_key was already used with different event details; choose a new key for a different event"), WriteOut{}, nil
		case err != nil:
			return mcpkit.ErrorResult(ctx, "internal", "could not record the idempotency key"), WriteOut{}, nil
		case !created && rec.EventID != "":
			existing, gerr := d.readEvent(ctx, p, provider.EventRef{CalendarID: calID, EventID: rec.EventID})
			if gerr == nil {
				out := d.render(ctx, a, existing)
				return nil, WriteOut{Status: "created", Replay: true, Event: &out}, nil
			}
			if !errors.Is(gerr, provider.ErrNotFound) {
				return d.writeError(ctx, gerr), WriteOut{}, nil
			}
			// The event was deleted since: the key is spent, make the caller choose a new one.
			return mcpkit.ErrorResult(ctx, "idempotency_key_spent", "that idempotency_key made an event that has since been deleted; choose a new key to create another"), WriteOut{}, nil
		}
		// A key with no event id is a create whose outcome was never recorded:
		// the provider's dedupe key makes running it again safe.
		opts.DedupeKey = a.ID + "/" + calID + "/" + key
	}

	var made provider.Event
	err = d.retry429(ctx, func() (e error) { made, e = p.CreateEvent(ctx, calID, ev, opts); return })
	if err != nil {
		if key != "" && definitelyNotWritten(err) {
			_ = d.Store.ReleaseCalendarIdempotency(ctx, a.ID, key)
		}
		return d.writeError(ctx, err), WriteOut{}, nil
	}
	if key != "" {
		_, _ = d.Store.CompleteCalendarIdempotency(ctx, a.ID, key, made.ID)
	}
	if made.CalendarID == "" {
		made.CalendarID = calID
	}
	out := d.render(ctx, a, made)
	return nil, WriteOut{Status: "created", Event: &out}, nil
}

func createHash(calID string, in provider.EventInput) string {
	raw, _ := json.Marshal(struct {
		Cal string
		In  provider.EventInput
	}{calID, in})
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func (d Deps) readEvent(ctx context.Context, p provider.Provider, ref provider.EventRef) (provider.Event, error) {
	var ev provider.Event
	err := d.retry429(ctx, func() (e error) { ev, e = p.GetEvent(ctx, ref); return })
	if ev.CalendarID == "" {
		ev.CalendarID = ref.CalendarID
	}
	return ev, err
}

func (d Deps) render(ctx context.Context, a store.Account, ev provider.Event) EventOut {
	return buildEvent(a, ev, d.newOrganizerTrust(a).of(ctx, ev.Organizer.Email), true, MaxEventContent)
}

// ---- calendar_update_event ----

// UpdateEventIn is the calendar_update_event input. A field left out is left
// alone; attendees, when given, replaces the whole list.
type UpdateEventIn struct {
	ID              string        `json:"id"`
	Scope           string        `json:"scope,omitempty"`
	Title           *string       `json:"title,omitempty"`
	Description     *string       `json:"description,omitempty"`
	Location        *string       `json:"location,omitempty"`
	Start           *TimeIn       `json:"start,omitempty"`
	End             *TimeIn       `json:"end,omitempty"`
	Recurrence      *RecurrenceIn `json:"recurrence,omitempty"`
	ClearRecurrence bool          `json:"clear_recurrence,omitempty"`
	Attendees       *[]AttendeeIn `json:"attendees,omitempty"`
	Video           *bool         `json:"video,omitempty"`
	ReminderMinutes *int          `json:"reminder_minutes,omitempty"`
}

var scopeSchema = &jsonschema.Schema{Type: "string", Enum: []any{"instance", "series"},
	Description: `instance (default): this event or occurrence only. series: the whole recurring series. "This and following" is not supported.`}

func (d Deps) registerUpdateEvent(s *sdk.Server) {
	mcpkit.AddTool(s, &sdk.Tool{
		Name: ToolUpdateEvent,
		Description: "Change an event on the account's own calendar, applied directly. Only the fields you pass change. " +
			"The event must involve only the account itself, both as it is and as it would be after the change. " +
			"For one occurrence of a repeating event use the occurrence's id with scope instance; for the whole series use scope series. " + WriteGuidance,
		InputSchema: &jsonschema.Schema{
			Type:     "object",
			Required: []string{"id"},
			Properties: map[string]*jsonschema.Schema{
				"id":               str("Composite event id from calendar_list_events or calendar_get_event."),
				"scope":            scopeSchema,
				"title":            str("New title."),
				"description":      str("New plain-text description."),
				"location":         str("New location."),
				"start":            timeSchema("New start."),
				"end":              timeSchema("New end."),
				"recurrence":       recurrenceSchema(),
				"clear_recurrence": {Type: "boolean", Description: "Turn a series into a single event."},
				"attendees":        attendeesSchema("Replaces the attendee list. Only the account itself may be listed."),
				"video":            {Type: "boolean", Description: "true attaches an online meeting, false removes it."},
				"reminder_minutes": {Type: "integer", Description: "The one reminder, minutes before the start."},
			},
		},
	}, mcpkit.Write(), d.updateEvent)
}

func (d Deps) updateEvent(ctx context.Context, req *sdk.CallToolRequest, in UpdateEventIn) (*sdk.CallToolResult, WriteOut, error) {
	audit.FromContext(ctx).SetAction(ToolUpdateEvent, "", "")
	accID, ref, ok := parseEventID(in.ID)
	if !ok {
		return mcpkit.ErrorResult(ctx, "invalid_input", "id must be a composite id like acc_xxxx:calendar|event, as returned by calendar_list_events"), WriteOut{}, nil
	}
	scope, err := parseScope(in.Scope)
	if err != nil {
		return mcpkit.ErrorResult(ctx, "invalid_input", err.Error()), WriteOut{}, nil
	}
	patch, err := in.patch()
	if err != nil {
		return mcpkit.ErrorResult(ctx, "invalid_input", err.Error()), WriteOut{}, nil
	}
	if err := provider.ValidatePatch(patch); err != nil {
		return d.writeError(ctx, err), WriteOut{}, nil
	}
	a, bad := d.accountByID(ctx, req, accID)
	if bad != nil {
		return bad, WriteOut{}, nil
	}
	p, err := d.Providers.CalendarProvider(ctx, a)
	if err != nil {
		return d.providerError(ctx, err), WriteOut{}, nil
	}
	ctx, cancel := context.WithTimeout(ctx, d.Fanout.Overall)
	defer cancel()

	for attempt := 0; ; attempt++ {
		pl, err := d.aim(ctx, p, ref, scope)
		if err != nil {
			return d.writeError(ctx, err), WriteOut{}, nil
		}
		cur := pl.seen[0]
		switch {
		case scope == provider.ScopeInstance && isMaster(cur):
			return mcpkit.ErrorResult(ctx, "invalid_input", "that id is a repeating series; pass scope series to change the whole series, or an occurrence's id for one occurrence"), WriteOut{}, nil
		case scope == provider.ScopeInstance && (patch.Recurrence != nil || patch.ClearRecurrence) && isOccurrence(cur):
			return mcpkit.ErrorResult(ctx, "invalid_input", "an occurrence cannot change its series' repeat rule; use scope series"), WriteOut{}, nil
		}
		var proposed *[]provider.Attendee
		if patch.Attendees != nil {
			proposed = patch.Attendees
		}
		// The union of the attendees now and after the change decides it.
		if pl.involvesOthers(a.Address, proposed) {
			return invitesNotEnabled(ctx, "this event"), WriteOut{}, nil
		}
		var updated provider.Event
		err = d.retry429(ctx, func() (e error) { updated, e = p.UpdateEvent(ctx, pl.ref, scope, patch, pl.version); return })
		if errors.Is(err, provider.ErrPreconditionFailed) && attempt == 0 {
			// Someone changed the event between the read and the write, and nothing
			// was written. Read again and classify again: if attendees appeared the
			// next pass stops, otherwise the change is still self-only and is retried
			// once at the new version.
			continue
		}
		if err != nil {
			return d.writeError(ctx, err), WriteOut{}, nil
		}
		if updated.CalendarID == "" {
			updated.CalendarID = pl.ref.CalendarID
		}
		out := d.render(ctx, a, updated)
		return nil, WriteOut{Status: "updated", Event: &out}, nil
	}
}

func (in UpdateEventIn) patch() (provider.EventPatch, error) {
	var p provider.EventPatch
	p.Title, p.Description, p.Location, p.Video, p.ReminderMinutes = in.Title, in.Description, in.Location, in.Video, in.ReminderMinutes
	if p.Title != nil {
		t := strings.TrimSpace(*p.Title)
		if t == "" || len(t) > maxTitle {
			return p, fmt.Errorf("title must be 1 to %d bytes", maxTitle)
		}
		p.Title = &t
	}
	if (p.Description != nil && len(*p.Description) > MaxDescription) || (p.Location != nil && len(*p.Location) > maxTitle) {
		return p, errors.New("description or location is too long")
	}
	var err error
	if in.Start != nil {
		var t provider.EventTime
		if t, err = in.Start.convert("start"); err != nil {
			return p, err
		}
		p.Start = &t
	}
	if in.End != nil {
		var t provider.EventTime
		if t, err = in.End.convert("end"); err != nil {
			return p, err
		}
		p.End = &t
	}
	if in.Recurrence != nil {
		r := in.Recurrence.convert()
		p.Recurrence = &r
	}
	p.ClearRecurrence = in.ClearRecurrence
	if in.Attendees != nil {
		if len(*in.Attendees) > maxAttendees {
			return p, fmt.Errorf("at most %d attendees", maxAttendees)
		}
		list := convertAttendees(*in.Attendees)
		p.Attendees = &list
	}
	if p.Title == nil && p.Description == nil && p.Location == nil && p.Start == nil && p.End == nil && p.Recurrence == nil &&
		!p.ClearRecurrence && p.Attendees == nil && p.Video == nil && p.ReminderMinutes == nil {
		return p, errors.New("nothing to change: pass at least one field to update")
	}
	return p, nil
}

// ---- calendar_delete_event ----

// DeleteEventIn is the calendar_delete_event input.
type DeleteEventIn struct {
	ID    string `json:"id"`
	Scope string `json:"scope,omitempty"`
}

func (d Deps) registerDeleteEvent(s *sdk.Server) {
	mcpkit.AddTool(s, &sdk.Tool{
		Name: ToolDeleteEvent,
		Description: "Delete an event from the account's own calendar, applied directly. It cannot be undone. " +
			"The event must involve only the account itself: deleting an event that has attendees (or that someone else organizes) would send a cancellation, so it returns invites_not_enabled. " +
			"For one occurrence of a repeating event use the occurrence's id with scope instance; for the whole series use scope series. " + WriteGuidance,
		InputSchema: &jsonschema.Schema{
			Type:     "object",
			Required: []string{"id"},
			Properties: map[string]*jsonschema.Schema{
				"id":    str("Composite event id from calendar_list_events or calendar_get_event."),
				"scope": scopeSchema,
			},
		},
	}, mcpkit.Destructive(), d.deleteEvent)
}

func (d Deps) deleteEvent(ctx context.Context, req *sdk.CallToolRequest, in DeleteEventIn) (*sdk.CallToolResult, WriteOut, error) {
	audit.FromContext(ctx).SetAction(ToolDeleteEvent, "", "")
	accID, ref, ok := parseEventID(in.ID)
	if !ok {
		return mcpkit.ErrorResult(ctx, "invalid_input", "id must be a composite id like acc_xxxx:calendar|event, as returned by calendar_list_events"), WriteOut{}, nil
	}
	scope, err := parseScope(in.Scope)
	if err != nil {
		return mcpkit.ErrorResult(ctx, "invalid_input", err.Error()), WriteOut{}, nil
	}
	a, bad := d.accountByID(ctx, req, accID)
	if bad != nil {
		return bad, WriteOut{}, nil
	}
	p, err := d.Providers.CalendarProvider(ctx, a)
	if err != nil {
		return d.providerError(ctx, err), WriteOut{}, nil
	}
	ctx, cancel := context.WithTimeout(ctx, d.Fanout.Overall)
	defer cancel()

	for attempt := 0; ; attempt++ {
		pl, err := d.aim(ctx, p, ref, scope)
		if err != nil {
			return d.writeError(ctx, err), WriteOut{}, nil
		}
		if scope == provider.ScopeInstance && isMaster(pl.seen[0]) {
			return mcpkit.ErrorResult(ctx, "invalid_input", "that id is a repeating series; pass scope series to delete the whole series, or an occurrence's id for one occurrence"), WriteOut{}, nil
		}
		if pl.involvesOthers(a.Address, nil) {
			return invitesNotEnabled(ctx, "deleting this event"), WriteOut{}, nil
		}
		err = d.retry429(ctx, func() error { return p.DeleteEvent(ctx, pl.ref, scope, pl.version) })
		if errors.Is(err, provider.ErrPreconditionFailed) && attempt == 0 {
			continue
		}
		if err != nil {
			return d.writeError(ctx, err), WriteOut{}, nil
		}
		return nil, WriteOut{Status: "deleted", DeletedID: eventID(a, pl.ref.CalendarID, pl.ref.EventID)}, nil
	}
}

// ---- calendar_respond_to_invite ----

// RespondIn is the calendar_respond_to_invite input.
type RespondIn struct {
	ID       string `json:"id"`
	Response string `json:"response"`
	Comment  string `json:"comment,omitempty"`
}

// RespondOut is the calendar_respond_to_invite output.
type RespondOut struct {
	Status   string `json:"status"`
	Response string `json:"response"`
	// NotifiedOrganizer is the organizer's address: your reply was sent to them.
	NotifiedOrganizer string `json:"notified_organizer"`
	// PreviousResponse is the account's answer before this one.
	PreviousResponse string `json:"previous_response,omitempty"`
	EventID          string `json:"event_id"`
}

var markupLike = regexp.MustCompile(`</?[A-Za-z!][^>]*>`)

// cleanComment keeps a comment to plain text: control characters go (newline
// and tab stay), markup is refused, and the length is capped.
func cleanComment(s string) (string, error) {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) && r != '\n' && r != '\t' {
			return -1
		}
		return r
	}, strings.ReplaceAll(s, "\r\n", "\n"))
	s = strings.TrimSpace(s)
	if len([]rune(s)) > provider.MaxResponseComment {
		return "", fmt.Errorf("comment is over %d characters", provider.MaxResponseComment)
	}
	if markupLike.MatchString(s) {
		return "", errors.New("comment is plain text only: no HTML or markup")
	}
	return s, nil
}

func (d Deps) registerRespond(s *sdk.Server) {
	mcpkit.AddTool(s, &sdk.Tool{
		Name: ToolRespondToInvite,
		Description: "Answer an invitation: accept, tentative or decline, with an optional plain-text comment of at most 500 characters. " +
			"Your reply notifies the organizer. This applies directly with no approval step: it is the one calendar action that notifies someone else without one, " +
			"because it only tells the organizer your own answer. It does not change the event or its other attendees. " +
			"Name the event by its composite id from calendar_list_events; the account is the one the id names.",
		InputSchema: &jsonschema.Schema{
			Type:     "object",
			Required: []string{"id", "response"},
			Properties: map[string]*jsonschema.Schema{
				"id":       str("Composite event id of the invitation."),
				"response": {Type: "string", Enum: []any{"accept", "tentative", "decline"}},
				"comment":  str("Optional plain-text note to the organizer, at most 500 characters. No HTML."),
			},
		},
	}, mcpkit.Write(), d.respond)
}

func (d Deps) respond(ctx context.Context, req *sdk.CallToolRequest, in RespondIn) (*sdk.CallToolResult, RespondOut, error) {
	audit.FromContext(ctx).SetAction(ToolRespondToInvite, "", "")
	accID, ref, ok := parseEventID(in.ID)
	if !ok {
		return mcpkit.ErrorResult(ctx, "invalid_input", "id must be a composite id like acc_xxxx:calendar|event, as returned by calendar_list_events"), RespondOut{}, nil
	}
	resp := provider.Response(strings.ToLower(strings.TrimSpace(in.Response)))
	if !resp.Valid() {
		return mcpkit.ErrorResult(ctx, "invalid_input", "response must be accept, tentative or decline"), RespondOut{}, nil
	}
	comment, err := cleanComment(in.Comment)
	if err != nil {
		return mcpkit.ErrorResult(ctx, "invalid_input", err.Error()), RespondOut{}, nil
	}
	a, bad := d.accountByID(ctx, req, accID)
	if bad != nil {
		return bad, RespondOut{}, nil
	}
	p, err := d.Providers.CalendarProvider(ctx, a)
	if err != nil {
		return d.providerError(ctx, err), RespondOut{}, nil
	}
	ctx, cancel := context.WithTimeout(ctx, d.Fanout.Overall)
	defer cancel()
	cur, err := d.readEvent(ctx, p, ref)
	if err != nil {
		return d.writeError(ctx, err), RespondOut{}, nil
	}
	if cur.Organizer.Self || strings.EqualFold(strings.TrimSpace(cur.Organizer.Email), strings.TrimSpace(a.Address)) {
		return mcpkit.ErrorResult(ctx, "invalid_input", "the account organizes this event, so there is no invitation to answer"), RespondOut{}, nil
	}
	if err := d.retry429(ctx, func() error { return p.Respond(ctx, ref, resp, comment) }); err != nil {
		return d.writeError(ctx, err), RespondOut{}, nil
	}
	return nil, RespondOut{
		Status: "responded", Response: string(resp), NotifiedOrganizer: plainAddr(cur.Organizer.Email),
		PreviousResponse: cur.SelfResponse, EventID: eventID(a, ref.CalendarID, ref.EventID),
	}, nil
}
