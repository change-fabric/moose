package pages

import (
	"fmt"
	"strings"
	"time"

	"github.com/change-fabric/moose/internal/approver/api"
)

// inviteView is the body of a calendar-invite page. Everything in it is
// rendered from the signed payload, never from the preview, so the page cannot
// disagree with what the receipt covers.
type inviteView struct {
	// Op is create, update or cancel; Headline says what approving does.
	Op       string
	Headline string
	Role     string
	Target   string
	// Rows are the event's fields. On an update a changed row says what it was.
	Rows       []fieldRow
	Recipients []recipientRow
	Changed    int
}

// fieldRow is one field of the event. Untrusted rows carry text written by
// whoever created the event or invited the account: it is shown in full, as
// plain text, and marked so.
type fieldRow struct {
	Label     string
	Value     string
	Was       string
	HasWas    bool
	Changed   bool
	Untrusted bool
	// Block rows (the description) are shown in a scrollable preformatted box.
	Block bool
	// Empty is true for a field with no value, which is shown as "none".
	Empty bool
}

type recipientRow struct {
	Address  string
	Name     string
	External bool
	Note     string
}

// newInviteView lays the payload out for review.
func newInviteView(p api.CalendarInvitePayload) *inviteView {
	v := &inviteView{Op: p.Operation, Role: "You organize this event."}
	switch p.Operation {
	case api.CalOpCreate:
		v.Headline = "Create this event and invite the people below. They will be emailed an invitation."
	case api.CalOpUpdate:
		v.Headline = "Change this event. The people below will be emailed the update."
	case api.CalOpCancel:
		v.Headline = "Cancel this event. The people below will be emailed the cancellation."
	}
	switch {
	case p.Operation == api.CalOpCreate:
		v.Target = "A new event"
	case p.Target.Scope == api.CalScopeSeries:
		v.Target = "The whole series"
	case p.Target.OriginalStart != nil:
		v.Target = "One occurrence only, the one that was scheduled for " + timeText(*p.Target.OriginalStart)
	default:
		v.Target = "This one event"
	}

	cur := p.Proposed
	if p.Operation == api.CalOpCancel {
		cur = p.Original
	}
	was := p.Original
	if p.Operation != api.CalOpUpdate {
		was = nil
	}
	field := func(label string, get func(*api.CalEvent) string, untrusted, block bool) {
		now := get(cur)
		r := fieldRow{Label: label, Value: now, Untrusted: untrusted, Block: block, Empty: now == ""}
		if was != nil {
			if before := get(was); before != now {
				r.Changed, r.HasWas, r.Was = true, true, before
				v.Changed++
			}
		}
		v.Rows = append(v.Rows, r)
	}
	field("Title", func(e *api.CalEvent) string { return e.Title }, true, false)
	field("Starts", func(e *api.CalEvent) string { return timeText(e.Start) }, false, false)
	field("Ends", func(e *api.CalEvent) string { return timeText(e.End) }, false, false)
	field("Repeats", func(e *api.CalEvent) string { return recurrenceText(e.Recurrence) }, false, false)
	field("Location", func(e *api.CalEvent) string { return e.Location }, true, false)
	field("Online meeting", func(e *api.CalEvent) string {
		if e.Video {
			return "Yes, a video link is attached"
		}
		return "No"
	}, false, false)
	field("Reminder", func(e *api.CalEvent) string {
		if e.ReminderMinutes == nil {
			return ""
		}
		return fmt.Sprintf("%d minutes before", *e.ReminderMinutes)
	}, false, false)
	field("Attendees", attendeesText, true, false)
	field("Description", func(e *api.CalEvent) string { return e.Description }, true, true)

	notes := map[string]string{
		api.CalChangeAdded:     "will be invited",
		api.CalChangeKept:      "will be notified of the change",
		api.CalChangeRemoved:   "will be notified of removal",
		api.CalChangeCancelled: "will be notified of the cancellation",
	}
	for _, r := range p.Recipients {
		v.Recipients = append(v.Recipients, recipientRow{Address: r.Email, Name: r.Name, External: r.External, Note: notes[r.Change]})
	}
	return v
}

func attendeesText(e *api.CalEvent) string {
	var parts []string
	for _, a := range e.Attendees {
		s := a.Email
		if a.Name != "" {
			s = a.Name + " <" + a.Email + ">"
		}
		if a.Optional {
			s += " (optional)"
		}
		parts = append(parts, s)
	}
	return strings.Join(parts, "; ")
}

// timeText shows a time with its zone, or an all-day date.
func timeText(t api.CalTime) string {
	if t.Date != "" {
		return t.Date + " (all day)"
	}
	at, err := time.Parse(time.RFC3339, t.DateTime)
	if err != nil {
		return t.DateTime + " " + t.Zone
	}
	if loc, err := time.LoadLocation(t.Zone); err == nil {
		at = at.In(loc)
	}
	return at.Format("Mon 2 Jan 2006 15:04") + " " + t.Zone + " (UTC" + at.Format("-07:00") + ")"
}

var freqWords = map[string]string{"DAILY": "day", "WEEKLY": "week", "MONTHLY": "month", "YEARLY": "year"}

func recurrenceText(r *api.CalRecurrence) string {
	switch {
	case r == nil:
		return ""
	case r.Freq == "":
		if r.Native != "" {
			return "A repeat rule that moose cannot summarise: " + r.Native
		}
		return ""
	}
	unit := freqWords[r.Freq]
	s := "Every " + unit
	if r.Interval > 1 {
		s = fmt.Sprintf("Every %d %ss", r.Interval, unit)
	}
	if len(r.ByDay) > 0 {
		s += " on " + strings.Join(r.ByDay, ", ")
	}
	switch {
	case r.Count > 0:
		s += fmt.Sprintf(", %d times", r.Count)
	case r.Until != "":
		s += ", until " + r.Until
	default:
		s += ", with no end"
	}
	return s
}
