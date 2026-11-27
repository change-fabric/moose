package inviteflow

import (
	"strings"
	"time"

	"github.com/change-fabric/moose/internal/accounts/store"
	"github.com/change-fabric/moose/internal/approver/api"
	"github.com/change-fabric/moose/internal/calendar/provider"
)

func domainOf(addr string) string {
	if i := strings.LastIndex(addr, "@"); i >= 0 {
		return strings.ToLower(strings.TrimSpace(addr[i+1:]))
	}
	return ""
}

func calTime(t provider.EventTime) api.CalTime {
	if t.IsAllDay() {
		return api.CalTime{Date: t.Date}
	}
	return api.CalTime{DateTime: t.DateTime.Format(time.RFC3339), Zone: t.Zone}
}

func calRecurrence(r *provider.Recurrence) *api.CalRecurrence {
	if r == nil {
		return nil
	}
	return &api.CalRecurrence{Freq: r.Freq, Interval: r.Interval, ByDay: append([]string(nil), r.ByDay...), Until: r.Until, Count: r.Count, Native: r.Native}
}

func calAttendees(a store.Account, list []provider.Attendee) []api.CalAttendee {
	own := domainOf(a.Address)
	out := make([]api.CalAttendee, 0, len(list))
	for _, x := range list {
		email := strings.ToLower(strings.TrimSpace(x.Email))
		if email == "" {
			continue
		}
		out = append(out, api.CalAttendee{Email: email, Name: x.Name, Optional: x.Optional, External: domainOf(email) != own})
	}
	return out
}

func copyInt(p *int) *int {
	if p == nil {
		return nil
	}
	v := *p
	return &v
}

// eventOf is an event as it stands, for the payload's original.
func eventOf(a store.Account, ev provider.Event) api.CalEvent {
	return api.CalEvent{
		Title: ev.Title, Description: ev.Description, Location: ev.Location,
		Start: calTime(ev.Start), End: calTime(ev.End), Recurrence: calRecurrence(ev.Recurrence),
		Attendees: calAttendees(a, ev.Attendees), Video: ev.Video, ReminderMinutes: copyInt(ev.ReminderMinutes),
	}
}

// eventOfInput is the event a create would make.
func eventOfInput(a store.Account, in provider.EventInput) api.CalEvent {
	return api.CalEvent{
		Title: in.Title, Description: in.Description, Location: in.Location,
		Start: calTime(in.Start), End: calTime(in.End), Recurrence: calRecurrence(in.Recurrence),
		Attendees: calAttendees(a, in.Attendees), Video: in.Video, ReminderMinutes: copyInt(in.ReminderMinutes),
	}
}

// patched is base with patch applied, as the provider will leave the event: the
// payload's proposed event.
func patched(a store.Account, base api.CalEvent, p provider.EventPatch) api.CalEvent {
	out := base
	out.Attendees = append(make([]api.CalAttendee, 0, len(base.Attendees)), base.Attendees...)
	if p.Title != nil {
		out.Title = *p.Title
	}
	if p.Description != nil {
		out.Description = *p.Description
	}
	if p.Location != nil {
		out.Location = *p.Location
	}
	if p.Start != nil {
		out.Start = calTime(*p.Start)
	}
	if p.End != nil {
		out.End = calTime(*p.End)
	}
	switch {
	case p.ClearRecurrence:
		out.Recurrence = nil
	case p.Recurrence != nil:
		out.Recurrence = calRecurrence(p.Recurrence)
	}
	if p.Attendees != nil {
		out.Attendees = calAttendees(a, *p.Attendees)
	}
	if p.Video != nil {
		out.Video = *p.Video
	}
	if p.ReminderMinutes != nil {
		out.ReminderMinutes = copyInt(p.ReminderMinutes)
	}
	return out
}

func sameInstant(a, b api.CalTime) bool {
	if a.Date != "" || b.Date != "" {
		return a.Date == b.Date
	}
	x, err1 := time.Parse(time.RFC3339, a.DateTime)
	y, err2 := time.Parse(time.RFC3339, b.DateTime)
	return err1 == nil && err2 == nil && x.Equal(y)
}

func attendeeSet(a store.Account, e api.CalEvent) map[string]bool {
	m := map[string]bool{}
	for _, x := range e.Attendees {
		if addr := strings.ToLower(strings.TrimSpace(x.Email)); addr != "" && !strings.EqualFold(addr, a.Address) {
			m[addr] = true
		}
	}
	return m
}

// matches reports whether the event as the provider holds it now carries what
// was approved: title, description, location, times and the people other than
// the account. It is how an update whose outcome was never seen is recognised
// as having landed. Video, reminder and recurrence are not compared; they are
// not reliably readable back, and a match on the rest with a new version is
// the evidence used.
func matches(a store.Account, cur provider.Event, want api.CalEvent) bool {
	got := eventOf(a, cur)
	if got.Title != want.Title || strings.TrimSpace(got.Description) != strings.TrimSpace(want.Description) || got.Location != want.Location {
		return false
	}
	if !sameInstant(got.Start, want.Start) || !sameInstant(got.End, want.End) {
		return false
	}
	g, w := attendeeSet(a, got), attendeeSet(a, want)
	if len(g) != len(w) {
		return false
	}
	for k := range w {
		if !g[k] {
			return false
		}
	}
	return true
}

// sameRecipients reports whether two recipient lists name the same people with
// the same change and external flag.
func sameRecipients(x, y []api.CalRecipient) bool {
	if len(x) != len(y) {
		return false
	}
	for i := range x {
		if !strings.EqualFold(x[i].Email, y[i].Email) || x[i].Change != y[i].Change || x[i].External != y[i].External {
			return false
		}
	}
	return true
}
