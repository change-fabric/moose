// Package providertest is the conformance suite every calendar provider.Provider
// must pass. It is run against the in-memory fake (internal/calendar/fake) and
// against each real provider's HTTP stub (Google Calendar, Microsoft Graph), so
// the tools can rely on one behaviour: ordering and paging, occurrence
// expansion, the version precondition on every update and delete, the create
// dedupe key, who is notified, and how failures map to errors.
//
// RunRead covers reads and free/busy. RunWrite covers creates, updates,
// deletes, series scope and RSVP.
package providertest

import (
	"testing"
	"time"

	"github.com/change-fabric/moose/internal/calendar/provider"
)

// SelfAddress is the address of the account every factory-built provider is
// bound to. The suites arrange events around it.
const SelfAddress = "me@example.com"

// RetryAfter is the hint a rate-limit failure carries.
const RetryAfter = 7 * time.Second

// Failure is a fault a Seeder can arrange for every following call.
type Failure int

const (
	// FailNone clears any earlier failure.
	FailNone Failure = iota
	// FailAuth makes calls fail as a revoked or insufficient credential: ErrAuth.
	FailAuth
	// FailRateLimit makes calls fail as 429 with Retry-After: RetryAfter.
	FailRateLimit
)

// Notification is an email the provider sent because of a call under test.
type Notification struct {
	// Kind is invite, update, cancel or rsvp.
	Kind string
	// To are the recipient addresses.
	To []string
}

// Seeder arranges the provider behind a Factory. Each provider supplies its own:
// the fake writes to memory, a stub server seeds its tables.
type Seeder interface {
	// SeedCalendar adds a non-primary calendar the account can see.
	SeedCalendar(t *testing.T, c provider.Calendar)
	// SeedEvent stores ev on calendarID without notifying anyone, as if it
	// already existed, and returns it as the provider now reports it. The
	// organizer is the account unless ev names another.
	SeedEvent(t *testing.T, calendarID string, ev provider.Event) provider.Event
	// SeedBusy records busy time for another person.
	SeedBusy(t *testing.T, email string, in provider.Interval)
	// ExternalAddAttendee adds an attendee as another client would: the event's
	// version changes and moose sends nothing.
	ExternalAddAttendee(t *testing.T, ref provider.EventRef, a provider.Attendee)
	// Fail arranges a failure for the calls that follow.
	Fail(t *testing.T, f Failure)
	// Notified returns the emails sent by the provider so far.
	Notified(t *testing.T) []Notification
}

// Factory returns a fresh provider for SelfAddress with the capabilities the
// suites should expect, and the Seeder that arranges it.
type Factory func(t *testing.T) (provider.Provider, Seeder)

// primaryID returns the id of the account's primary calendar.
func primaryID(t *testing.T, p provider.Provider) string {
	t.Helper()
	cals, err := p.ListCalendars(t.Context())
	if err != nil {
		t.Fatalf("ListCalendars: %v", err)
	}
	for _, c := range cals {
		if c.Primary {
			return c.ID
		}
	}
	t.Fatal("no primary calendar")
	return ""
}

// berlin is the zone the suites use for timed events: it has a daylight-saving
// change on 2026-03-29, which the recurrence cases cross.
const berlin = "Europe/Berlin"

func loc(t *testing.T, zone string) *time.Location {
	t.Helper()
	l, err := time.LoadLocation(zone)
	if err != nil {
		t.Fatal(err)
	}
	return l
}

// at is a timed EventTime in zone.
func at(t *testing.T, zone string, y int, m time.Month, d, h, min int) provider.EventTime {
	t.Helper()
	return provider.EventTime{DateTime: time.Date(y, m, d, h, min, 0, 0, loc(t, zone)), Zone: zone}
}

func day(y int, m time.Month, d int) provider.EventTime {
	return provider.EventTime{Date: time.Date(y, m, d, 0, 0, 0, 0, time.UTC).Format(provider.DateLayout)}
}

// timed is a one-hour event on 2026-03-<d> at hour h Berlin time.
func timedEvent(t *testing.T, title string, d, h int) provider.Event {
	t.Helper()
	return provider.Event{Title: title, Start: at(t, berlin, 2026, time.March, d, h, 0), End: at(t, berlin, 2026, time.March, d, h+1, 0)}
}

func window(t *testing.T, fromDay, toDay int) (time.Time, time.Time) {
	t.Helper()
	l := loc(t, berlin)
	return time.Date(2026, time.March, fromDay, 0, 0, 0, 0, l), time.Date(2026, time.March, toDay, 0, 0, 0, 0, l)
}

func ids(events []provider.Event) []string {
	out := make([]string, len(events))
	for i, e := range events {
		out[i] = e.ID
	}
	return out
}

func titles(events []provider.Event) []string {
	out := make([]string, len(events))
	for i, e := range events {
		out[i] = e.Title
	}
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func sameInstant(a, b provider.EventTime) bool {
	if a.IsAllDay() || b.IsAllDay() {
		return a.Date == b.Date
	}
	return a.DateTime.Equal(b.DateTime)
}

func has(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func notesOf(t *testing.T, s Seeder, kind string) []Notification {
	t.Helper()
	var out []Notification
	for _, n := range s.Notified(t) {
		if n.Kind == kind {
			out = append(out, n)
		}
	}
	return out
}
