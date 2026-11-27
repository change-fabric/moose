// Package provider defines the calendar-provider abstraction moose-calendar is
// written against, the way internal/email/provider does for mail. Tools and the
// invite approval flow use only these types and this interface, so a provider
// is added by implementing Provider, not by touching the tools.
//
// The neutral model is deliberately smaller than either provider's:
//
//   - Times are an RFC3339 instant with offset plus an IANA zone name, or an
//     all-day calendar date (EventTime). Windows are instants.
//   - Recurrence is a constrained subset (Recurrence): frequency, interval,
//     by-day, and an until date or a count. Anything else is refused on write
//     with ErrInvalidRecurrence; on read the provider's own rule comes back as an
//     opaque Native string.
//   - Series scope is the instance or the whole series; "this and following" is
//     refused.
//   - Attendees imply notification on both providers: Graph always emails them
//     and moose asks Google to. There is no switch.
//   - Every update and delete is conditional on the event version the caller
//     read (Event.Version: Google etag, Graph changeKey), so a write never lands
//     on a changed event.
package provider

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	mail "github.com/change-fabric/moose/internal/email/provider"
)

// Errors a Provider returns. Implementations wrap them with detail. The first
// group is the mail providers' own values, shared on purpose: the fan-out and
// retry code in internal/accounts/fanout classifies a provider failure with
// errors.Is against them, and it must do so identically for both services.
var (
	// ErrNotFound means the calendar or event does not exist.
	ErrNotFound = mail.ErrNotFound
	// ErrAuth means the stored credential no longer works or lacks the scope the
	// call needs, and the account needs reauthorization or a calendar grant.
	ErrAuth = mail.ErrAuth
	// ErrInvalidQuery means a window, text query or cursor could not be used.
	ErrInvalidQuery = mail.ErrInvalidQuery
	// ErrRateLimited means the provider asked the caller to slow down.
	ErrRateLimited = mail.ErrRateLimited
	// ErrRejected means the provider definitely refused the request, so nothing
	// was changed.
	ErrRejected = mail.ErrRejected
)

// RateLimitError is a rate-limit answer carrying the provider's Retry-After hint.
type RateLimitError = mail.RateLimitError

// Errors specific to calendars.
var (
	// ErrUnsupportedHere means the operation cannot be done on this account's
	// provider or account type: video on a personal Microsoft account, free/busy
	// of other people on one, or a write the provider cannot make conditional.
	// Its text carries the stable code CodeUnsupportedHere.
	ErrUnsupportedHere = errors.New("provider: " + CodeUnsupportedHere)
	// ErrInvalidRecurrence means a recurrence is outside the neutral subset or
	// internally inconsistent. It is returned before anything is sent.
	ErrInvalidRecurrence = errors.New("provider: invalid recurrence")
	// ErrPreconditionFailed means the event changed since the version the caller
	// holds (HTTP 412). Nothing was written and nobody was notified; re-read the
	// event and decide again.
	ErrPreconditionFailed = errors.New("provider: event changed since it was read")
	// ErrInvalidTime means an EventTime or window is malformed: no zone, a zone
	// that is not an IANA name, an end before the start, or both a date and an
	// instant.
	ErrInvalidTime = errors.New("provider: invalid time")
)

// CodeUnsupportedHere is the stable tool error code for ErrUnsupportedHere.
const CodeUnsupportedHere = "unsupported_here"

// Access roles a calendar can grant the account.
const (
	RoleOwner        = "owner"
	RoleWriter       = "writer"
	RoleReader       = "reader"
	RoleFreeBusyOnly = "free_busy_only"
)

// Calendar is one calendar of an account.
type Calendar struct {
	// ID is the provider's calendar id, opaque to callers.
	ID   string `json:"id"`
	Name string `json:"name"`
	// Primary marks the account's own default calendar.
	Primary bool `json:"primary"`
	// Role is what the account may do on it (Role* constants).
	Role string `json:"role"`
	// TimeZone is the calendar's default IANA zone, when the provider says.
	TimeZone string `json:"time_zone,omitempty"`
}

// CanWrite reports whether the role allows creating and changing events.
func (c Calendar) CanWrite() bool { return c.Role == RoleOwner || c.Role == RoleWriter }

// EventTime is a point or a day. Exactly one of DateTime and Date is set.
type EventTime struct {
	// DateTime is the instant, RFC3339 with its offset, for a timed event.
	DateTime time.Time `json:"date_time,omitzero"`
	// Zone is the IANA zone the event is anchored to ("Europe/Berlin"). It is what
	// recurring events follow across daylight-saving changes, so it is required
	// with DateTime.
	Zone string `json:"zone,omitempty"`
	// Date is a calendar day, "2006-01-02", for an all-day event. The end of an
	// all-day event is exclusive: a one-day event on the 5th ends on the 6th.
	Date string `json:"date,omitempty"`
}

// DateLayout is the layout of EventTime.Date.
const DateLayout = "2006-01-02"

// IsAllDay reports whether t is a date rather than an instant.
func (t EventTime) IsAllDay() bool { return t.Date != "" }

// IsZero reports whether neither a date nor an instant is set.
func (t EventTime) IsZero() bool { return t.Date == "" && t.DateTime.IsZero() }

// Validate checks that exactly one form is set and that it is well formed.
func (t EventTime) Validate() error {
	switch {
	case t.Date != "" && !t.DateTime.IsZero():
		return fmt.Errorf("%w: set a date or an instant, not both", ErrInvalidTime)
	case t.Date != "":
		if t.Zone != "" {
			return fmt.Errorf("%w: an all-day date takes no zone", ErrInvalidTime)
		}
		if _, err := time.Parse(DateLayout, t.Date); err != nil {
			return fmt.Errorf("%w: date %q is not YYYY-MM-DD", ErrInvalidTime, t.Date)
		}
		return nil
	case t.DateTime.IsZero():
		return fmt.Errorf("%w: neither a date nor an instant", ErrInvalidTime)
	case t.Zone == "":
		return fmt.Errorf("%w: a timed event needs an IANA zone such as Europe/Berlin", ErrInvalidTime)
	}
	if _, err := LoadZone(t.Zone); err != nil {
		return err
	}
	return nil
}

// LoadZone loads an IANA zone name. Abbreviations ("EST", "PST") and fixed
// offsets are refused: they carry no daylight-saving rules, and the two
// providers disagree on what they mean.
func LoadZone(name string) (*time.Location, error) {
	if name == "" || strings.EqualFold(name, "local") || strings.EqualFold(name, "utc+0") {
		return nil, fmt.Errorf("%w: zone %q is not an IANA name", ErrInvalidTime, name)
	}
	if !strings.Contains(name, "/") && name != "UTC" && name != "GMT" {
		return nil, fmt.Errorf("%w: zone %q is not an IANA name such as Europe/Berlin", ErrInvalidTime, name)
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		return nil, fmt.Errorf("%w: zone %q is not an IANA name", ErrInvalidTime, name)
	}
	return loc, nil
}

// Start of the day for an all-day value, or the instant.
func (t EventTime) instant() (time.Time, error) {
	if t.IsAllDay() {
		return time.Parse(DateLayout, t.Date)
	}
	return t.DateTime, nil
}

// Before reports whether t is strictly before u, comparing a date as midnight
// UTC. It is meant for end-after-start checks of values of the same form.
func (t EventTime) Before(u EventTime) bool {
	a, errA := t.instant()
	b, errB := u.instant()
	return errA == nil && errB == nil && a.Before(b)
}

// Recurrence frequencies.
const (
	FreqDaily   = "DAILY"
	FreqWeekly  = "WEEKLY"
	FreqMonthly = "MONTHLY"
	FreqYearly  = "YEARLY"
)

// Weekdays, as RFC 5545 names them.
var Weekdays = []string{"MO", "TU", "WE", "TH", "FR", "SA", "SU"}

// Recurrence is the neutral recurrence subset. On write only Freq, Interval,
// ByDay, Until and Count are used and Native must be empty. On read, a rule
// that fits the subset fills those fields; a rule that does not is returned as
// Native alone, in the provider's own syntax, opaque to callers.
type Recurrence struct {
	Freq string `json:"freq,omitempty"`
	// Interval is every N periods; 0 means 1.
	Interval int `json:"interval,omitempty"`
	// ByDay lists weekdays (MO..SU) and is allowed with WEEKLY only.
	ByDay []string `json:"by_day,omitempty"`
	// Until is the last day of the series, "2006-01-02", inclusive. Set Until or
	// Count, not both; neither means it never ends.
	Until string `json:"until,omitempty"`
	Count int    `json:"count,omitempty"`
	// Native is the provider's own rule (an RRULE line, or a Graph
	// patternedRecurrence rendered as JSON), read-only and opaque.
	Native string `json:"native,omitempty"`
}

// Validate checks r is inside the neutral subset. It returns an error wrapping
// ErrInvalidRecurrence with the reason.
func (r Recurrence) Validate() error {
	bad := func(format string, a ...any) error {
		return fmt.Errorf("%w: %s", ErrInvalidRecurrence, fmt.Sprintf(format, a...))
	}
	if r.Native != "" {
		return bad("a provider rule cannot be written; use freq, interval, by_day, until or count")
	}
	switch r.Freq {
	case FreqDaily, FreqWeekly, FreqMonthly, FreqYearly:
	default:
		return bad("freq must be DAILY, WEEKLY, MONTHLY or YEARLY, got %q", r.Freq)
	}
	if r.Interval < 0 || r.Interval > 999 {
		return bad("interval must be between 1 and 999")
	}
	if r.Count < 0 || r.Count > 999 {
		return bad("count must be between 1 and 999")
	}
	if r.Count > 0 && r.Until != "" {
		return bad("set until or count, not both")
	}
	if r.Until != "" {
		if _, err := time.Parse(DateLayout, r.Until); err != nil {
			return bad("until %q is not YYYY-MM-DD", r.Until)
		}
	}
	if len(r.ByDay) > 0 {
		if r.Freq != FreqWeekly {
			return bad("by_day is allowed with WEEKLY only")
		}
		seen := map[string]bool{}
		for _, d := range r.ByDay {
			if !isWeekday(d) {
				return bad("by_day entry %q is not one of MO, TU, WE, TH, FR, SA, SU", d)
			}
			if seen[d] {
				return bad("by_day lists %s twice", d)
			}
			seen[d] = true
		}
	}
	return nil
}

func isWeekday(d string) bool {
	for _, w := range Weekdays {
		if d == w {
			return true
		}
	}
	return false
}

// IsOpaque reports whether r came back from a read as a native rule only.
func (r Recurrence) IsOpaque() bool { return r.Native != "" && r.Freq == "" }

// Attendee response statuses.
const (
	ResponseNone      = "needs_action"
	ResponseAccepted  = "accepted"
	ResponseTentative = "tentative"
	ResponseDeclined  = "declined"
)

// Attendee is one invitee.
type Attendee struct {
	Email string `json:"email"`
	Name  string `json:"name,omitempty"`
	// Response is the attendee's answer (Response* constants).
	Response string `json:"response,omitempty"`
	Optional bool   `json:"optional,omitempty"`
	// External marks an address outside the account's own domain. Providers
	// leave it false; moose's tools set it, because it needs the account's address.
	External bool `json:"external,omitempty"`
	// Self marks the account itself among the attendees.
	Self bool `json:"self,omitempty"`
}

// Event statuses.
const (
	StatusConfirmed = "confirmed"
	StatusTentative = "tentative"
	StatusCancelled = "cancelled"
)

// Event is a calendar event as moose sees it.
type Event struct {
	// ID is the provider's event id: for a recurring event's occurrence, the
	// occurrence's own id.
	ID         string `json:"id"`
	CalendarID string `json:"calendar_id"`
	// SeriesID is the recurring series' master id, set on occurrences and the
	// master itself; empty for a one-off event.
	SeriesID string `json:"series_id,omitempty"`
	// OriginalStart is where an occurrence sat in the unmodified series.
	OriginalStart *EventTime `json:"original_start,omitempty"`
	Title         string     `json:"title"`
	Description   string     `json:"description,omitempty"`
	Location      string     `json:"location,omitempty"`
	Start         EventTime  `json:"start"`
	End           EventTime  `json:"end"`
	// Recurrence is set on a series' master and on its occurrences.
	Recurrence *Recurrence `json:"recurrence,omitempty"`
	Organizer  Attendee    `json:"organizer"`
	Attendees  []Attendee  `json:"attendees,omitempty"`
	// Video says an online meeting is attached; VideoURL is its join link.
	Video    bool   `json:"video,omitempty"`
	VideoURL string `json:"video_url,omitempty"`
	// ReminderMinutes is the one reminder, minutes before the start; nil is none.
	ReminderMinutes *int   `json:"reminder_minutes,omitempty"`
	Status          string `json:"status,omitempty"`
	// Version is what a conditional write must present: the Google etag or the
	// Graph changeKey. It changes on every change to the event.
	Version string `json:"version"`
	// SelfResponse is the account's own answer when it is an attendee.
	SelfResponse string    `json:"self_response,omitempty"`
	Created      time.Time `json:"created,omitzero"`
	Updated      time.Time `json:"updated,omitzero"`
}

// HasOtherAttendees reports whether anyone other than the account itself is an
// attendee. selfEmail is the account's address; an attendee marked Self never
// counts. The organizer does not count unless listed among the attendees.
func (e Event) HasOtherAttendees(selfEmail string) bool { return HasOthers(e.Attendees, selfEmail) }

// HasOthers reports whether list holds an attendee other than selfEmail.
func HasOthers(list []Attendee, selfEmail string) bool {
	for _, a := range list {
		if a.Self || strings.EqualFold(strings.TrimSpace(a.Email), strings.TrimSpace(selfEmail)) {
			continue
		}
		if strings.TrimSpace(a.Email) != "" {
			return true
		}
	}
	return false
}

// EventInput is what a create supplies.
type EventInput struct {
	Title       string
	Description string
	Location    string
	Start, End  EventTime
	Recurrence  *Recurrence
	// Attendees are invited, and notified, on creation.
	Attendees []Attendee
	// Video asks for an online meeting (Meet or Teams).
	Video bool
	// ReminderMinutes is the one reminder; nil means the calendar's default.
	ReminderMinutes *int
}

// EventPatch is a partial update: a nil field is left alone. An empty
// Attendees slice (non-nil) removes everyone; nil leaves the list as is.
type EventPatch struct {
	Title       *string
	Description *string
	Location    *string
	Start, End  *EventTime
	Recurrence  *Recurrence
	// ClearRecurrence turns a series into a one-off event.
	ClearRecurrence bool
	Attendees       *[]Attendee
	Video           *bool
	ReminderMinutes *int
}

// Scope says what a change to a recurring event applies to. Callers read the
// event first; a scope only matters for an occurrence or a series.
type Scope string

const (
	// ScopeInstance changes the one occurrence the ref names.
	ScopeInstance Scope = "instance"
	// ScopeSeries changes the whole series the ref belongs to.
	ScopeSeries Scope = "series"
)

// Valid reports whether s is a scope moose supports. "this and following" is
// not one of them.
func (s Scope) Valid() bool { return s == ScopeInstance || s == ScopeSeries }

// EventRef names an event on a calendar.
type EventRef struct {
	CalendarID string
	EventID    string
}

// CreateOptions are the extras of a create.
type CreateOptions struct {
	// DedupeKey is a moose-supplied key the provider records with the event so a
	// retry or a reconciliation finds the same event instead of making a second:
	// Google takes it as the client-supplied event id, Graph as the transactionId.
	// It must be stable for one logical create and distinct between creates.
	DedupeKey string
}

// ListEventsQuery selects events in a window.
type ListEventsQuery struct {
	// CalendarIDs defaults to the primary calendar when empty.
	CalendarIDs []string
	// Start and End bound the window; occurrences of recurring events are
	// expanded. End must be after Start.
	Start, End time.Time
	// Zone is the IANA zone events are rendered in; empty means each event's own.
	Zone string
	// Text is a free-text filter over title, description, location and attendees.
	Text string
	// PageToken continues a previous page.
	PageToken string
	// Limit is the page size; 0 means the provider default.
	Limit int
}

// EventPage is one page of events, ordered by start.
type EventPage struct {
	Events []Event `json:"events"`
	// NextPageToken is empty on the last page.
	NextPageToken string `json:"next_page_token,omitempty"`
}

// FreeBusyQuery asks when people are busy.
type FreeBusyQuery struct {
	// CalendarIDs are the account's own calendars; empty means the primary.
	CalendarIDs []string
	// Emails are other people whose availability is wanted. A provider that
	// cannot answer for others returns ErrUnsupportedHere when this is set.
	Emails     []string
	Start, End time.Time
}

// Interval is a half-open [Start, End) span of busy time.
type Interval struct {
	Start time.Time `json:"start"`
	End   time.Time `json:"end"`
}

// FreeBusyResult maps each requested calendar id or address to its busy time.
// A subject the provider could not resolve is listed in Unavailable, not
// treated as free.
type FreeBusyResult struct {
	Busy        map[string][]Interval `json:"busy"`
	Unavailable map[string]string     `json:"unavailable,omitempty"`
}

// Capabilities describe what an account's provider supports.
type Capabilities struct {
	// FreeBusyOthers is whether FreeBusy can answer for other people. False on
	// a personal Microsoft account, where only the account's own calendar works.
	FreeBusyOthers bool `json:"free_busy_others"`
	// Video is whether Video:true can be honoured (Meet; Teams on work or school).
	Video bool `json:"video"`
	// Recurrence is whether the provider accepts the neutral recurrence subset.
	Recurrence bool `json:"recurrence"`
}

// Response is an answer to an invitation.
type Response string

const (
	RespondAccept    Response = "accept"
	RespondTentative Response = "tentative"
	RespondDecline   Response = "decline"
)

// Valid reports whether r is one of the three answers.
func (r Response) Valid() bool {
	return r == RespondAccept || r == RespondTentative || r == RespondDecline
}

// MaxResponseComment caps the optional plain-text comment of an RSVP.
const MaxResponseComment = 500

// Provider is a calendar provider bound to one account. Implementations are
// safe for concurrent use.
type Provider interface {
	Capabilities() Capabilities
	// ListCalendars returns the account's calendars, the primary one first.
	ListCalendars(ctx context.Context) ([]Calendar, error)
	// ListEvents returns events in the window ordered by start, occurrences of
	// recurring events expanded.
	ListEvents(ctx context.Context, q ListEventsQuery) (EventPage, error)
	// GetEvent returns one event. ErrNotFound if it does not exist.
	GetEvent(ctx context.Context, ref EventRef) (Event, error)
	// FreeBusy returns busy intervals.
	FreeBusy(ctx context.Context, q FreeBusyQuery) (FreeBusyResult, error)
	// CreateEvent creates an event. Attendees are notified. A second call with
	// the same opts.DedupeKey returns the event the first one made.
	CreateEvent(ctx context.Context, calendarID string, in EventInput, opts CreateOptions) (Event, error)
	// UpdateEvent applies patch, conditional on the event still being at
	// ifVersion (ErrPreconditionFailed otherwise, with nothing written and
	// nobody notified). scope is ScopeInstance or ScopeSeries.
	UpdateEvent(ctx context.Context, ref EventRef, scope Scope, patch EventPatch, ifVersion string) (Event, error)
	// DeleteEvent deletes or, for an event with attendees, cancels it,
	// conditional on ifVersion as UpdateEvent is.
	DeleteEvent(ctx context.Context, ref EventRef, scope Scope, ifVersion string) error
	// Respond sets the account's own answer to an invitation and notifies the
	// organizer. comment is plain text of at most MaxResponseComment characters.
	Respond(ctx context.Context, ref EventRef, resp Response, comment string) error
}
