package providertest

import (
	"errors"
	"testing"
	"time"

	"github.com/change-fabric/moose/internal/calendar/provider"
)

// RunRead runs the read conformance suite: calendars, event windows, text
// filter, paging, all-day and recurring events, free/busy and failure mapping.
func RunRead(t *testing.T, factory Factory) {
	t.Run("ListCalendarsPrimaryFirst", func(t *testing.T) {
		p, s := factory(t)
		s.SeedCalendar(t, provider.Calendar{ID: "team", Name: "Team", Role: provider.RoleReader})
		cals, err := p.ListCalendars(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if len(cals) < 2 || !cals[0].Primary || cals[0].ID == "" {
			t.Fatalf("calendars = %+v, want the primary first", cals)
		}
		primaries := 0
		for _, c := range cals {
			if c.Primary {
				primaries++
			}
			if c.Role == "" || c.ID == "" {
				t.Errorf("calendar %+v has no id or role", c)
			}
		}
		if primaries != 1 {
			t.Errorf("%d primary calendars, want exactly 1", primaries)
		}
	})

	t.Run("ListEventsWindowOrderedByStart", func(t *testing.T) {
		p, s := factory(t)
		prim := primaryID(t, p)
		s.SeedEvent(t, prim, timedEvent(t, "late", 4, 15))
		s.SeedEvent(t, prim, timedEvent(t, "early", 4, 9))
		s.SeedEvent(t, prim, timedEvent(t, "outside", 20, 9))
		s.SeedEvent(t, prim, timedEvent(t, "next day", 5, 9))
		from, to := window(t, 4, 6)
		page, err := p.ListEvents(t.Context(), provider.ListEventsQuery{Start: from, End: to})
		if err != nil {
			t.Fatal(err)
		}
		if got := titles(page.Events); !equalStrings(got, []string{"early", "late", "next day"}) {
			t.Fatalf("events = %v, want early, late, next day in start order", got)
		}
		if page.NextPageToken != "" {
			t.Errorf("a short result has a next page token %q", page.NextPageToken)
		}
		e := page.Events[0]
		if e.ID == "" || e.CalendarID != prim || e.Version == "" || e.Start.Zone == "" || e.Start.DateTime.IsZero() {
			t.Errorf("event fields = %+v", e)
		}
	})

	t.Run("ListEventsDefaultsToThePrimaryCalendar", func(t *testing.T) {
		p, s := factory(t)
		prim := primaryID(t, p)
		s.SeedCalendar(t, provider.Calendar{ID: "team", Name: "Team", Role: provider.RoleWriter})
		s.SeedEvent(t, prim, timedEvent(t, "mine", 4, 9))
		s.SeedEvent(t, "team", timedEvent(t, "theirs", 4, 10))
		from, to := window(t, 4, 5)
		page, _ := p.ListEvents(t.Context(), provider.ListEventsQuery{Start: from, End: to})
		if got := titles(page.Events); !equalStrings(got, []string{"mine"}) {
			t.Fatalf("default calendars gave %v, want only the primary's event", got)
		}
		page, err := p.ListEvents(t.Context(), provider.ListEventsQuery{CalendarIDs: []string{prim, "team"}, Start: from, End: to})
		if err != nil {
			t.Fatal(err)
		}
		if got := titles(page.Events); !equalStrings(got, []string{"mine", "theirs"}) {
			t.Fatalf("both calendars gave %v", got)
		}
		if _, err := p.ListEvents(t.Context(), provider.ListEventsQuery{CalendarIDs: []string{"nope"}, Start: from, End: to}); !errors.Is(err, provider.ErrNotFound) {
			t.Errorf("unknown calendar err = %v, want ErrNotFound", err)
		}
	})

	t.Run("ListEventsTextFilter", func(t *testing.T) {
		p, s := factory(t)
		prim := primaryID(t, p)
		a := timedEvent(t, "Budget review", 4, 9)
		b := timedEvent(t, "Lunch", 4, 12)
		b.Description = "talk about the budget"
		c := timedEvent(t, "Standup", 4, 14)
		s.SeedEvent(t, prim, a)
		s.SeedEvent(t, prim, b)
		s.SeedEvent(t, prim, c)
		from, to := window(t, 4, 5)
		page, err := p.ListEvents(t.Context(), provider.ListEventsQuery{Start: from, End: to, Text: "budget"})
		if err != nil {
			t.Fatal(err)
		}
		if got := titles(page.Events); !equalStrings(got, []string{"Budget review", "Lunch"}) {
			t.Fatalf("text filter gave %v", got)
		}
	})

	t.Run("ListEventsPaging", func(t *testing.T) {
		p, s := factory(t)
		prim := primaryID(t, p)
		for i := range 5 {
			s.SeedEvent(t, prim, timedEvent(t, "e"+string(rune('a'+i)), 4, 8+i))
		}
		from, to := window(t, 4, 5)
		var seen []string
		token := ""
		for range 10 {
			page, err := p.ListEvents(t.Context(), provider.ListEventsQuery{Start: from, End: to, Limit: 2, PageToken: token})
			if err != nil {
				t.Fatal(err)
			}
			if len(page.Events) > 2 {
				t.Fatalf("a page of %d events with limit 2", len(page.Events))
			}
			seen = append(seen, ids(page.Events)...)
			if token = page.NextPageToken; token == "" {
				break
			}
		}
		if token != "" || len(seen) != 5 {
			t.Fatalf("paging ended with token %q after %d events, want 5", token, len(seen))
		}
		uniq := map[string]bool{}
		for _, id := range seen {
			uniq[id] = true
		}
		if len(uniq) != 5 {
			t.Errorf("pages repeated events: %v", seen)
		}
		if _, err := p.ListEvents(t.Context(), provider.ListEventsQuery{Start: from, End: to, PageToken: "garbage"}); !errors.Is(err, provider.ErrInvalidQuery) {
			t.Errorf("garbage page token err = %v, want ErrInvalidQuery", err)
		}
	})

	t.Run("ListEventsRejectsABackwardsWindow", func(t *testing.T) {
		p, _ := factory(t)
		from, to := window(t, 4, 5)
		if _, err := p.ListEvents(t.Context(), provider.ListEventsQuery{Start: to, End: from}); !errors.Is(err, provider.ErrInvalidQuery) {
			t.Errorf("backwards window err = %v, want ErrInvalidQuery", err)
		}
		if _, err := p.ListEvents(t.Context(), provider.ListEventsQuery{}); !errors.Is(err, provider.ErrInvalidQuery) {
			t.Errorf("empty window err = %v, want ErrInvalidQuery", err)
		}
	})

	t.Run("GetEventRoundTripsEveryField", func(t *testing.T) {
		p, s := factory(t)
		prim := primaryID(t, p)
		rem := 15
		seeded := s.SeedEvent(t, prim, provider.Event{
			Title: "Planning", Description: "Agenda inside", Location: "Room 4",
			Start: at(t, berlin, 2026, time.March, 4, 9, 30), End: at(t, berlin, 2026, time.March, 4, 10, 30),
			Attendees: []provider.Attendee{
				{Email: SelfAddress, Response: provider.ResponseAccepted},
				{Email: "bob@example.com", Name: "Bob", Response: provider.ResponseTentative},
			},
			ReminderMinutes: &rem,
		})
		got, err := p.GetEvent(t.Context(), provider.EventRef{CalendarID: prim, EventID: seeded.ID})
		if err != nil {
			t.Fatal(err)
		}
		if got.Title != "Planning" || got.Description != "Agenda inside" || got.Location != "Room 4" || got.Version == "" ||
			!sameInstant(got.Start, seeded.Start) || !sameInstant(got.End, seeded.End) || got.Start.Zone != berlin {
			t.Fatalf("event = %+v", got)
		}
		if got.ReminderMinutes == nil || *got.ReminderMinutes != 15 {
			t.Errorf("reminder = %v, want 15", got.ReminderMinutes)
		}
		if got.Organizer.Email != SelfAddress {
			t.Errorf("organizer = %+v, want the account", got.Organizer)
		}
		var bob *provider.Attendee
		for i := range got.Attendees {
			if got.Attendees[i].Email == "bob@example.com" {
				bob = &got.Attendees[i]
			}
		}
		if bob == nil || bob.Response != provider.ResponseTentative || bob.Name != "Bob" {
			t.Errorf("attendees = %+v", got.Attendees)
		}
		if got.SelfResponse != provider.ResponseAccepted {
			t.Errorf("self response = %q, want accepted", got.SelfResponse)
		}
		if _, err := p.GetEvent(t.Context(), provider.EventRef{CalendarID: prim, EventID: "does-not-exist"}); !errors.Is(err, provider.ErrNotFound) {
			t.Errorf("missing event err = %v, want ErrNotFound", err)
		}
	})

	t.Run("AllDayEventKeepsDatesAndAnExclusiveEnd", func(t *testing.T) {
		p, s := factory(t)
		prim := primaryID(t, p)
		s.SeedEvent(t, prim, provider.Event{Title: "Holiday", Start: day(2026, time.March, 10), End: day(2026, time.March, 12)})
		// A date has no zone: the windows are UTC days, so the comparison is by date.
		utcDay := func(d int) time.Time { return time.Date(2026, time.March, d, 0, 0, 0, 0, time.UTC) }
		from, to := utcDay(9), utcDay(14)
		page, err := p.ListEvents(t.Context(), provider.ListEventsQuery{Start: from, End: to})
		if err != nil || len(page.Events) != 1 {
			t.Fatalf("events = %+v, %v", page.Events, err)
		}
		e := page.Events[0]
		if !e.Start.IsAllDay() || e.Start.Date != "2026-03-10" || e.End.Date != "2026-03-12" || !e.Start.DateTime.IsZero() {
			t.Errorf("all-day event = %+v", e)
		}
		// The day after the exclusive end does not see it.
		page, _ = p.ListEvents(t.Context(), provider.ListEventsQuery{Start: utcDay(12), End: utcDay(14)})
		if len(page.Events) != 0 {
			t.Errorf("an event ending 12 March (exclusive) showed up on the 12th: %v", titles(page.Events))
		}
	})

	t.Run("RecurringEventsAreExpandedInTheirOwnZone", func(t *testing.T) {
		p, s := factory(t)
		prim := primaryID(t, p)
		master := timedEvent(t, "Weekly sync", 23, 9) // Monday 23 March, 09:00 Berlin
		master.Recurrence = &provider.Recurrence{Freq: provider.FreqWeekly, ByDay: []string{"MO", "WE"}, Count: 6}
		s.SeedEvent(t, prim, master)
		l := loc(t, berlin)
		from, to := time.Date(2026, time.March, 20, 0, 0, 0, 0, l), time.Date(2026, time.April, 20, 0, 0, 0, 0, l)
		page, err := p.ListEvents(t.Context(), provider.ListEventsQuery{Start: from, End: to})
		if err != nil {
			t.Fatal(err)
		}
		if len(page.Events) != 6 {
			t.Fatalf("%d occurrences, want the 6 the count allows: %v", len(page.Events), titles(page.Events))
		}
		seen := map[string]bool{}
		for _, e := range page.Events {
			if seen[e.ID] {
				t.Errorf("occurrence id %s repeated", e.ID)
			}
			seen[e.ID] = true
			if e.SeriesID == "" || e.Recurrence == nil || e.OriginalStart == nil {
				t.Errorf("occurrence %+v lacks its series link", e)
			}
			if h := e.Start.DateTime.In(l).Hour(); h != 9 || e.Start.Zone != berlin {
				t.Errorf("occurrence at %s (%s) drifted from 09:00 Berlin across the 29 March daylight-saving change", e.Start.DateTime.In(l), e.Start.Zone)
			}
			if e.End.DateTime.Sub(e.Start.DateTime) != time.Hour {
				t.Errorf("occurrence length = %s", e.End.DateTime.Sub(e.Start.DateTime))
			}
		}
		// The window cuts the series: only the first week.
		from, to = window(t, 23, 26)
		page, _ = p.ListEvents(t.Context(), provider.ListEventsQuery{Start: from, End: to})
		if len(page.Events) != 2 {
			t.Errorf("one week of the series gave %d occurrences, want Monday and Wednesday", len(page.Events))
		}
		got, err := p.GetEvent(t.Context(), provider.EventRef{CalendarID: prim, EventID: page.Events[1].ID})
		if err != nil || got.ID != page.Events[1].ID || got.Version != page.Events[1].Version {
			t.Errorf("GetEvent of an occurrence = %+v, %v", got, err)
		}
	})

	t.Run("FreeBusyOwnCalendarMergesAndSkipsDeclined", func(t *testing.T) {
		p, s := factory(t)
		prim := primaryID(t, p)
		s.SeedEvent(t, prim, timedEvent(t, "a", 4, 9))
		b := timedEvent(t, "b", 4, 9)
		b.Start, b.End = at(t, berlin, 2026, time.March, 4, 9, 30), at(t, berlin, 2026, time.March, 4, 11, 0)
		s.SeedEvent(t, prim, b)
		declined := timedEvent(t, "declined", 4, 14)
		declined.Attendees = []provider.Attendee{{Email: SelfAddress, Response: provider.ResponseDeclined}, {Email: "x@example.com"}}
		s.SeedEvent(t, prim, declined)
		from, to := window(t, 4, 5)
		res, err := p.FreeBusy(t.Context(), provider.FreeBusyQuery{Start: from, End: to})
		if err != nil {
			t.Fatal(err)
		}
		busy := res.Busy[prim]
		if len(busy) != 1 || !busy[0].Start.Equal(at(t, berlin, 2026, time.March, 4, 9, 0).DateTime) || !busy[0].End.Equal(at(t, berlin, 2026, time.March, 4, 11, 0).DateTime) {
			t.Fatalf("busy = %+v, want one merged 09:00-11:00 interval and nothing for the declined event", busy)
		}
	})

	t.Run("FreeBusyOfOthersFollowsCapabilities", func(t *testing.T) {
		p, s := factory(t)
		from, to := window(t, 4, 5)
		s.SeedBusy(t, "bob@example.com", provider.Interval{Start: at(t, berlin, 2026, time.March, 4, 13, 0).DateTime, End: at(t, berlin, 2026, time.March, 4, 14, 0).DateTime})
		res, err := p.FreeBusy(t.Context(), provider.FreeBusyQuery{Emails: []string{"bob@example.com"}, Start: from, End: to})
		if !p.Capabilities().FreeBusyOthers {
			if !errors.Is(err, provider.ErrUnsupportedHere) {
				t.Fatalf("free/busy of others on an account that cannot err = %v, want ErrUnsupportedHere", err)
			}
			return
		}
		if err != nil {
			t.Fatal(err)
		}
		if got := res.Busy["bob@example.com"]; len(got) != 1 || !got[0].Start.Equal(at(t, berlin, 2026, time.March, 4, 13, 0).DateTime) {
			t.Fatalf("bob busy = %+v", res.Busy)
		}
		res, err = p.FreeBusy(t.Context(), provider.FreeBusyQuery{Emails: []string{"nobody@example.com"}, Start: from, End: to})
		if err != nil {
			t.Fatal(err)
		}
		if _, free := res.Busy["nobody@example.com"]; free || res.Unavailable["nobody@example.com"] == "" {
			t.Errorf("an unresolvable person must be reported unavailable, not free: %+v", res)
		}
	})

	t.Run("FailuresMapToSharedErrors", func(t *testing.T) {
		p, s := factory(t)
		prim := primaryID(t, p)
		from, to := window(t, 4, 5)
		calls := map[string]func() error{
			"ListCalendars": func() error { _, err := p.ListCalendars(t.Context()); return err },
			"ListEvents": func() error {
				_, err := p.ListEvents(t.Context(), provider.ListEventsQuery{Start: from, End: to})
				return err
			},
			"GetEvent": func() error {
				_, err := p.GetEvent(t.Context(), provider.EventRef{CalendarID: prim, EventID: "x"})
				return err
			},
			"FreeBusy": func() error {
				_, err := p.FreeBusy(t.Context(), provider.FreeBusyQuery{Start: from, End: to})
				return err
			},
		}
		s.Fail(t, FailAuth)
		for name, call := range calls {
			if err := call(); !errors.Is(err, provider.ErrAuth) {
				t.Errorf("%s with a revoked credential err = %v, want ErrAuth", name, err)
			}
		}
		s.Fail(t, FailRateLimit)
		for name, call := range calls {
			err := call()
			var rl *provider.RateLimitError
			if !errors.Is(err, provider.ErrRateLimited) || !errors.As(err, &rl) || rl.RetryAfter != RetryAfter {
				t.Errorf("%s when throttled err = %v, want a RateLimitError with Retry-After %s", name, err, RetryAfter)
			}
		}
		s.Fail(t, FailNone)
	})
}
