package provider

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// MaxReminderMinutes is the longest reminder lead either provider accepts (four weeks).
const MaxReminderMinutes = 40320

// ValidateEventInput checks a create before anything is sent: times well formed
// and of one form, the end after the start, a recurrence inside the neutral
// subset, usable attendee addresses and a reminder in range. Providers call it
// first so a refused request never reaches the network and never notifies.
func ValidateEventInput(in EventInput) error {
	if err := ValidateTimes(in.Start, in.End); err != nil {
		return err
	}
	if in.Recurrence != nil {
		if err := in.Recurrence.Validate(); err != nil {
			return err
		}
	}
	if err := ValidateAttendees(in.Attendees); err != nil {
		return err
	}
	return ValidateReminder(in.ReminderMinutes)
}

// ValidateTimes checks a start and end pair.
func ValidateTimes(start, end EventTime) error {
	if err := start.Validate(); err != nil {
		return err
	}
	if err := end.Validate(); err != nil {
		return err
	}
	if start.IsAllDay() != end.IsAllDay() {
		return fmt.Errorf("%w: start and end must both be dates or both be instants", ErrInvalidTime)
	}
	if !start.Before(end) {
		return fmt.Errorf("%w: the end must be after the start", ErrInvalidTime)
	}
	return nil
}

// ValidateAttendees checks every attendee has a usable address.
func ValidateAttendees(list []Attendee) error {
	for _, a := range list {
		if strings.TrimSpace(a.Email) == "" || !strings.Contains(a.Email, "@") {
			return fmt.Errorf("%w: attendee %q has no usable address", ErrRejected, a.Email)
		}
	}
	return nil
}

// ValidateReminder checks the one reminder is in range.
func ValidateReminder(m *int) error {
	if m != nil && (*m < 0 || *m > MaxReminderMinutes) {
		return fmt.Errorf("%w: reminder_minutes must be between 0 and %d", ErrRejected, MaxReminderMinutes)
	}
	return nil
}

// ValidatePatch checks the fields of a patch that can be checked without the
// event: a start or end on its own is well formed, both together are ordered,
// a recurrence is inside the subset, and the attendee and reminder values are
// usable. The pairing of a lone start with the stored end is the provider's.
func ValidatePatch(p EventPatch) error {
	switch {
	case p.Start != nil && p.End != nil:
		if err := ValidateTimes(*p.Start, *p.End); err != nil {
			return err
		}
	case p.Start != nil:
		if err := p.Start.Validate(); err != nil {
			return err
		}
	case p.End != nil:
		if err := p.End.Validate(); err != nil {
			return err
		}
	}
	if p.Recurrence != nil && p.ClearRecurrence {
		return fmt.Errorf("%w: set a recurrence or clear it, not both", ErrInvalidRecurrence)
	}
	if p.Recurrence != nil {
		if err := p.Recurrence.Validate(); err != nil {
			return err
		}
	}
	if p.Attendees != nil {
		if err := ValidateAttendees(*p.Attendees); err != nil {
			return err
		}
	}
	return ValidateReminder(p.ReminderMinutes)
}

// FormatRRULE renders r as the one RRULE line Google takes. start is the
// event's first start: a timed series ends at the last instant of the Until day
// in the event's zone, written in UTC as RFC 5545 requires for a zoned start,
// and an all-day series ends on the date. r must have passed Validate.
func FormatRRULE(r Recurrence, start EventTime) string {
	parts := []string{"FREQ=" + r.Freq}
	if r.Interval > 1 {
		parts = append(parts, fmt.Sprintf("INTERVAL=%d", r.Interval))
	}
	if len(r.ByDay) > 0 {
		parts = append(parts, "BYDAY="+strings.Join(r.ByDay, ","))
	}
	switch {
	case r.Until != "":
		day, _ := time.Parse(DateLayout, r.Until)
		if start.IsAllDay() {
			parts = append(parts, "UNTIL="+day.Format("20060102"))
			break
		}
		loc := time.UTC
		if l, err := LoadZone(start.Zone); err == nil {
			loc = l
		}
		endOfDay := time.Date(day.Year(), day.Month(), day.Day(), 23, 59, 59, 0, loc)
		parts = append(parts, "UNTIL="+endOfDay.UTC().Format("20060102T150405Z"))
	case r.Count > 0:
		parts = append(parts, fmt.Sprintf("COUNT=%d", r.Count))
	}
	return "RRULE:" + strings.Join(parts, ";")
}

// RequireVersion checks the arguments every conditional write shares: a scope
// moose supports, a named event, and a version. A write with no version is
// refused here, before any request: a blind overwrite is never a fallback.
func RequireVersion(ref EventRef, scope Scope, ifVersion string) error {
	if !scope.Valid() {
		return fmt.Errorf("%w: scope must be instance or series", ErrRejected)
	}
	if ref.CalendarID == "" || ref.EventID == "" {
		return fmt.Errorf("%w: a calendar id and an event id are required", ErrNotFound)
	}
	if strings.TrimSpace(ifVersion) == "" {
		return fmt.Errorf("%w: no version was given, and a write is never made without one", ErrPreconditionFailed)
	}
	return nil
}

// AsRejected turns a 400 from a write into ErrRejected: for a read it means a
// bad query, for a write it means the provider refused this change.
func AsRejected(err error) error {
	if err != nil && errors.Is(err, ErrInvalidQuery) && !errors.Is(err, ErrRejected) {
		return fmt.Errorf("%w: %v", ErrRejected, err)
	}
	return err
}
