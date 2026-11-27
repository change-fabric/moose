package api

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

// Calendar invite operations.
const (
	CalOpCreate = "create"
	CalOpUpdate = "update"
	CalOpCancel = "cancel"
)

// Organizer roles. Only the organizer may create, change or cancel an event
// that has attendees; a payload that names the attendee role is refused, which
// is how an organizer-only operation on an event the account was merely invited
// to is stopped before a request exists.
const (
	CalRoleOrganizer = "organizer"
	CalRoleAttendee  = "attendee"
)

// Scopes of a calendar write's target.
const (
	CalScopeInstance = "instance"
	CalScopeSeries   = "series"
)

// What a recipient is told.
const (
	// CalChangeAdded is a new attendee, who receives an invitation.
	CalChangeAdded = "added"
	// CalChangeKept is an attendee who stays and receives the update.
	CalChangeKept = "kept"
	// CalChangeRemoved is an attendee taken off an event that continues, who is
	// told they were removed.
	CalChangeRemoved = "removed"
	// CalChangeCancelled is an attendee of an event being cancelled.
	CalChangeCancelled = "cancelled"
)

// maxInviteText caps each description inside an invite payload.
const maxInviteText = 64 << 10

// CalTime is an event time in a payload: an RFC 3339 instant with its offset
// plus an IANA zone, or an all-day date.
type CalTime struct {
	DateTime string `json:"date_time,omitempty"`
	Zone     string `json:"zone,omitempty"`
	Date     string `json:"date,omitempty"`
}

// CalRecurrence is a recurrence in a payload: the neutral subset, or the
// provider's own rule as an opaque string on an event that was read.
type CalRecurrence struct {
	Freq     string   `json:"freq,omitempty"`
	Interval int      `json:"interval,omitempty"`
	ByDay    []string `json:"by_day,omitempty"`
	Until    string   `json:"until,omitempty"`
	Count    int      `json:"count,omitempty"`
	Native   string   `json:"native,omitempty"`
}

// CalAttendee is an invitee in a payload.
type CalAttendee struct {
	Email    string `json:"email"`
	Name     string `json:"name,omitempty"`
	Optional bool   `json:"optional,omitempty"`
	External bool   `json:"external,omitempty"`
}

// CalEvent is the event as it is, or as it will be, in a payload. Title and
// description are untrusted text; the approval page shows them in full.
type CalEvent struct {
	Title       string         `json:"title"`
	Description string         `json:"description,omitempty"`
	Location    string         `json:"location,omitempty"`
	Start       CalTime        `json:"start"`
	End         CalTime        `json:"end"`
	Recurrence  *CalRecurrence `json:"recurrence,omitempty"`
	Attendees   []CalAttendee  `json:"attendees"`
	Video       bool           `json:"video,omitempty"`
	// ReminderMinutes is the one reminder; null is none.
	ReminderMinutes *int `json:"reminder_minutes"`
}

// CalRecipient is one person the provider will email because of the operation.
type CalRecipient struct {
	Email    string `json:"email"`
	Name     string `json:"name,omitempty"`
	External bool   `json:"external,omitempty"`
	// Change is added, kept, removed or cancelled.
	Change string `json:"change"`
}

// CalTarget says which event a write is aimed at.
type CalTarget struct {
	// Scope is instance (one occurrence, or a one-off event) or series.
	Scope      string `json:"scope,omitempty"`
	CalendarID string `json:"calendar_id"`
	// EventID is empty for a create.
	EventID string `json:"event_id,omitempty"`
	// OriginalStart is where an occurrence sat in its series, when the target is
	// one occurrence of a recurring event.
	OriginalStart *CalTime `json:"original_start,omitempty"`
}

// CalendarInvitePayload is the calendar-invite payload: one create, update or
// cancel of an event that notifies someone other than the account. The receipt
// covers all of it, so the approval is bound to exactly this change to exactly
// this event and this set of people.
type CalendarInvitePayload struct {
	Owner       string `json:"owner"`
	AccountID   string `json:"account_id"`
	FromAddress string `json:"from_address"`
	// Operation is create, update or cancel.
	Operation string `json:"operation"`
	// OrganizerRole is organizer; attendee is refused (see CalRoleAttendee).
	OrganizerRole string    `json:"organizer_role"`
	Target        CalTarget `json:"target"`
	// Version is the provider's event version (Google etag, Graph changeKey) the
	// change was approved against; empty for a create. Dispatch rejects the
	// approval as stale when the event is no longer at it.
	Version string `json:"version,omitempty"`
	// Original is the event as read, null for a create. Proposed is the event
	// after the change, null for a cancel.
	Original *CalEvent `json:"original"`
	Proposed *CalEvent `json:"proposed"`
	// Recipients is everyone the provider will email: added, kept and removed
	// attendees (or all cancelled ones), with external flags.
	Recipients []CalRecipient `json:"recipients"`
	// SnapshotSHA256 is the hash of the sealed snapshot moose-calendar holds and
	// executes: the exact provider input behind Proposed.
	SnapshotSHA256 string `json:"snapshot_sha256"`
}

// InviteEnablePayload is the account-invite-enable payload.
type InviteEnablePayload struct {
	Owner       string `json:"owner"`
	AccountID   string `json:"account_id"`
	FromAddress string `json:"from_address"`
}

// InviteKinds are the kinds that change a calendar event for other people and
// therefore need a ready account with invites on and calendar granted.
func InviteKinds(kind string) bool { return kind == KindCalendarInvite }

// RecipientsFor derives who the provider will email for an operation by the
// account `from`, from the original and proposed events, and so what a
// calendar-invite payload's Recipients must be. The account itself is never a
// recipient. The list is ordered added, kept, removed, cancelled, then by
// address. The external flag is relative to the account's own domain.
func RecipientsFor(from, op string, original, proposed *CalEvent) []CalRecipient {
	selfDomain := domainOfAddress(from)
	index := func(e *CalEvent) map[string]CalAttendee {
		m := map[string]CalAttendee{}
		if e == nil {
			return m
		}
		for _, a := range e.Attendees {
			addr := strings.ToLower(strings.TrimSpace(a.Email))
			if addr == "" || strings.EqualFold(addr, strings.TrimSpace(from)) {
				continue
			}
			a.Email = addr
			m[addr] = a
		}
		return m
	}
	was, will := index(original), index(proposed)
	var out []CalRecipient
	add := func(a CalAttendee, change string) {
		out = append(out, CalRecipient{Email: a.Email, Name: a.Name, External: domainOfAddress(a.Email) != selfDomain, Change: change})
	}
	if op == CalOpCancel {
		for _, a := range was {
			add(a, CalChangeCancelled)
		}
	} else {
		for addr, a := range will {
			if _, existed := was[addr]; existed {
				add(a, CalChangeKept)
			} else {
				add(a, CalChangeAdded)
			}
		}
		for addr, a := range was {
			if _, stays := will[addr]; !stays {
				add(a, CalChangeRemoved)
			}
		}
	}
	rank := map[string]int{CalChangeAdded: 0, CalChangeKept: 1, CalChangeRemoved: 2, CalChangeCancelled: 3}
	sort.Slice(out, func(i, j int) bool {
		if rank[out[i].Change] != rank[out[j].Change] {
			return rank[out[i].Change] < rank[out[j].Change]
		}
		return out[i].Email < out[j].Email
	})
	return out
}

func domainOfAddress(addr string) string {
	if i := strings.LastIndex(addr, "@"); i >= 0 {
		return strings.ToLower(strings.TrimSpace(addr[i+1:]))
	}
	return ""
}

func checkCalTime(name string, t CalTime) error {
	switch {
	case t.Date != "" && t.DateTime != "":
		return fmt.Errorf("%s has both a date and a date_time", name)
	case t.Date == "" && t.DateTime == "":
		return fmt.Errorf("%s needs a date or a date_time", name)
	case t.DateTime != "" && t.Zone == "":
		return fmt.Errorf("%s needs a zone with its date_time", name)
	}
	return nil
}

func checkCalEvent(name string, e *CalEvent) error {
	if strings.TrimSpace(e.Title) == "" {
		return fmt.Errorf("%s needs a title", name)
	}
	if len(e.Description) > maxInviteText || len(e.Title) > maxInviteText {
		return fmt.Errorf("%s text is too long to approve", name)
	}
	if e.Attendees == nil {
		return fmt.Errorf("%s attendees are required (use [])", name)
	}
	if err := checkCalTime(name+".start", e.Start); err != nil {
		return err
	}
	return checkCalTime(name+".end", e.End)
}

func checkInvite(v *CalendarInvitePayload) error {
	if err := need("owner", v.Owner, "account_id", v.AccountID, "from_address", v.FromAddress, "target.calendar_id", v.Target.CalendarID); err != nil {
		return err
	}
	if !sha256Hex.MatchString(v.SnapshotSHA256) {
		return errors.New("snapshot_sha256 must be a hex sha256")
	}
	switch v.OrganizerRole {
	case CalRoleOrganizer:
	case CalRoleAttendee:
		return errors.New("only the organizer can create, change or cancel an event that has attendees; this account is only an attendee")
	default:
		return fmt.Errorf("organizer_role must be %q", CalRoleOrganizer)
	}
	switch v.Operation {
	case CalOpCreate:
		if v.Original != nil || v.Proposed == nil || v.Version != "" || v.Target.EventID != "" || v.Target.Scope != "" {
			return errors.New("a create has a proposed event only: no original, version, event_id or scope")
		}
	case CalOpUpdate:
		if v.Original == nil || v.Proposed == nil {
			return errors.New("an update needs the original and the proposed event")
		}
	case CalOpCancel:
		if v.Original == nil || v.Proposed != nil {
			return errors.New("a cancel needs the original event and no proposed event")
		}
	default:
		return fmt.Errorf("operation must be %s, %s or %s", CalOpCreate, CalOpUpdate, CalOpCancel)
	}
	if v.Operation != CalOpCreate {
		if err := need("target.event_id", v.Target.EventID, "version", v.Version); err != nil {
			return err
		}
		if v.Target.Scope != CalScopeInstance && v.Target.Scope != CalScopeSeries {
			return errors.New("target.scope must be instance or series")
		}
	}
	if v.Original != nil {
		if err := checkCalEvent("original", v.Original); err != nil {
			return err
		}
	}
	if v.Proposed != nil {
		if err := checkCalEvent("proposed", v.Proposed); err != nil {
			return err
		}
	}
	want := RecipientsFor(v.FromAddress, v.Operation, v.Original, v.Proposed)
	if len(want) == 0 {
		return errors.New("the change notifies nobody but the account, so it needs no invite approval")
	}
	if len(v.Recipients) != len(want) {
		return errors.New("recipients must list exactly the people the change notifies")
	}
	for i, r := range v.Recipients {
		w := want[i]
		if !strings.EqualFold(strings.TrimSpace(r.Email), w.Email) || r.Change != w.Change || r.External != w.External {
			return errors.New("recipients must list exactly the people the change notifies, with their change and external flag")
		}
	}
	return nil
}
