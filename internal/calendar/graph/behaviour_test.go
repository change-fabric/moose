package graph_test

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/change-fabric/moose/internal/calendar/provider"
	"github.com/change-fabric/moose/internal/calendar/provider/providertest"
)

func berlin(t *testing.T, d, h int) provider.EventTime {
	t.Helper()
	l, _ := time.LoadLocation("Europe/Berlin")
	return provider.EventTime{DateTime: time.Date(2026, 3, d, h, 0, 0, 0, l), Zone: "Europe/Berlin"}
}

func win(t *testing.T) (time.Time, time.Time) {
	t.Helper()
	l, _ := time.LoadLocation("Europe/Berlin")
	return time.Date(2026, 3, 1, 0, 0, 0, 0, l), time.Date(2026, 4, 1, 0, 0, 0, 0, l)
}

func TestTextFilterScansServerPagesAndMatchesTitleDescriptionAndAttendees(t *testing.T) {
	s := newStub(t)
	c := s.client(false)
	prim := fakePrimary(t, c)
	for i := range 60 { // more than one server page of 50 that do not match
		s.p.AddEvent(prim, provider.Event{Title: fmt.Sprintf("noise %d", i), Start: berlin(t, 4, 8), End: berlin(t, 4, 9)})
	}
	s.p.AddEvent(prim, provider.Event{Title: "Roadmap", Description: "the QUARTERLY budget", Start: berlin(t, 5, 8), End: berlin(t, 5, 9)})
	s.p.AddEvent(prim, provider.Event{Title: "Lunch", Start: berlin(t, 6, 8), End: berlin(t, 6, 9), Attendees: []provider.Attendee{{Email: "carol@example.com", Name: "Carol Quarterly"}}})
	from, to := win(t)
	page, err := c.ListEvents(t.Context(), provider.ListEventsQuery{Start: from, End: to, Text: "quarterly", Limit: 5})
	if err != nil {
		t.Fatal(err)
	}
	var titles []string
	for _, e := range page.Events {
		titles = append(titles, e.Title)
	}
	if strings.Join(titles, ",") != "Roadmap,Lunch" || page.NextPageToken != "" {
		t.Fatalf("titles = %v next %q, want both matches and an exhausted listing", titles, page.NextPageToken)
	}
	if s.calendarViewCalls < 2 {
		t.Errorf("%d calendarView calls: a text filter applied here must read past the first server page", s.calendarViewCalls)
	}
}

func TestTextFilterStopsAtTheScanCapWithAToken(t *testing.T) {
	s := newStub(t)
	c := s.client(false)
	prim := fakePrimary(t, c)
	for i := range 520 { // more than ten server pages of 50
		s.p.AddEvent(prim, provider.Event{Title: fmt.Sprintf("noise %d", i), Start: berlin(t, 4, 8), End: berlin(t, 4, 9)})
	}
	from, to := win(t)
	page, err := c.ListEvents(t.Context(), provider.ListEventsQuery{Start: from, End: to, Text: "nothing like this", Limit: 5})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Events) != 0 || page.NextPageToken == "" || s.calendarViewCalls != 10 {
		t.Errorf("events %d, token %q, calls %d: want no matches, 10 server pages read and a token to go on", len(page.Events), page.NextPageToken, s.calendarViewCalls)
	}
	// The token continues from where the scan stopped.
	before := s.calendarViewCalls
	if _, err := c.ListEvents(t.Context(), provider.ListEventsQuery{Start: from, End: to, Text: "nothing like this", Limit: 5, PageToken: page.NextPageToken}); err != nil {
		t.Fatal(err)
	}
	if s.calendarViewCalls == before {
		t.Error("the continuation read nothing")
	}
}

func fakePrimary(t *testing.T, c provider.Provider) string {
	t.Helper()
	cals, err := c.ListCalendars(t.Context())
	if err != nil || len(cals) == 0 || !cals[0].Primary {
		t.Fatalf("calendars = %+v, %v", cals, err)
	}
	return cals[0].ID
}

func TestZoneIsSentAsPreferAndEventsAreShownInIt(t *testing.T) {
	s := newStub(t)
	c := s.client(false)
	prim := fakePrimary(t, c)
	s.p.AddEvent(prim, provider.Event{Title: "x", Start: berlin(t, 4, 9), End: berlin(t, 4, 10)})
	from, to := win(t)
	page, err := c.ListEvents(t.Context(), provider.ListEventsQuery{Start: from, End: to, Zone: "Asia/Tokyo"})
	if err != nil || len(page.Events) != 1 {
		t.Fatal(page, err)
	}
	e := page.Events[0]
	if e.Start.Zone != "Asia/Tokyo" || e.Start.DateTime.Hour() != 17 || e.End.Zone != "Asia/Tokyo" {
		t.Errorf("event = %+v, want 17:00 in Tokyo", e.Start)
	}
	last := s.reqs[len(s.reqs)-1]
	if p := last.Header.Get("Prefer"); !strings.Contains(p, `outlook.timezone="Asia/Tokyo"`) || !strings.Contains(p, `IdType="ImmutableId"`) {
		t.Errorf("Prefer = %q", p)
	}
	// With no zone the times are asked for in UTC and shown in each event's own zone.
	page, err = c.ListEvents(t.Context(), provider.ListEventsQuery{Start: from, End: to})
	if err != nil || page.Events[0].Start.Zone != "Europe/Berlin" || page.Events[0].Start.DateTime.Hour() != 9 {
		t.Fatal(page, err)
	}
	if p := s.reqs[len(s.reqs)-1].Header.Get("Prefer"); !strings.Contains(p, `outlook.timezone="UTC"`) {
		t.Errorf("Prefer = %q, want UTC when no zone is asked for", p)
	}
	for _, bad := range []string{"EST", "Mars/Olympus", "local"} {
		if _, err := c.ListEvents(t.Context(), provider.ListEventsQuery{Start: from, End: to, Zone: bad}); !errors.Is(err, provider.ErrInvalidQuery) {
			t.Errorf("zone %q err = %v, want ErrInvalidQuery", bad, err)
		}
	}
}

func TestPersonalAccountNeverAsksGraphForOthers(t *testing.T) {
	s := newStub(t)
	c := s.client(true)
	from, to := win(t)
	_, err := c.FreeBusy(t.Context(), provider.FreeBusyQuery{Emails: []string{"bob@example.com"}, Start: from, End: to})
	if !errors.Is(err, provider.ErrUnsupportedHere) {
		t.Fatalf("err = %v, want ErrUnsupportedHere", err)
	}
	if len(s.reqs) != 0 {
		t.Errorf("%d requests were made before refusing", len(s.reqs))
	}
	if caps := c.Capabilities(); caps.FreeBusyOthers || caps.Video || !caps.Recurrence {
		t.Errorf("capabilities = %+v", caps)
	}
	if caps := s.client(false).Capabilities(); !caps.FreeBusyOthers || !caps.Video {
		t.Errorf("work capabilities = %+v", caps)
	}
}

func TestRawGraphEventsAreMappedAsDocumented(t *testing.T) {
	s := newStub(t)
	c := s.client(false)
	prim := fakePrimary(t, c)
	s.rawView = []map[string]any{
		{"id": "html", "subject": "Board", "changeKey": "ck1", "isAllDay": false, "isCancelled": false, "showAs": "busy",
			"start": map[string]any{"dateTime": "2026-03-04T09:00:00.0000000", "timeZone": "UTC"}, "end": map[string]any{"dateTime": "2026-03-04T10:00:00.0000000", "timeZone": "UTC"},
			"originalStartTimeZone": "Pacific Standard Time",
			"body":                  map[string]any{"contentType": "html", "content": "<p>Agenda</p><ul><li>one</li></ul>"},
			"location":              map[string]any{"displayName": "HQ"},
			"organizer":             map[string]any{"emailAddress": map[string]any{"name": "Ann", "address": "ANN@example.com"}},
			"attendees": []map[string]any{
				{"type": "required", "status": map[string]any{"response": "accepted"}, "emailAddress": map[string]any{"address": "Me@Example.com"}},
				{"type": "optional", "status": map[string]any{"response": "none"}, "emailAddress": map[string]any{"address": "x@example.com"}},
			},
			"responseStatus":  map[string]any{"response": "tentativelyAccepted"},
			"isOnlineMeeting": true, "onlineMeeting": map[string]any{"joinUrl": "https://teams.example.com/j"},
			"isReminderOn": true, "reminderMinutesBeforeStart": 30},
		{"id": "allday", "subject": "Holiday", "isAllDay": true, "showAs": "free",
			"start": map[string]any{"dateTime": "2026-03-10T00:00:00.0000000", "timeZone": "UTC"}, "end": map[string]any{"dateTime": "2026-03-12T00:00:00.0000000", "timeZone": "UTC"}},
		{"id": "gone", "subject": "Cancelled", "isCancelled": true, "showAs": "busy",
			"start": map[string]any{"dateTime": "2026-03-04T12:00:00.0000000", "timeZone": "UTC"}, "end": map[string]any{"dateTime": "2026-03-04T13:00:00.0000000", "timeZone": "UTC"}},
		{"id": "odd", "subject": "Odd zone", "showAs": "busy", "originalStartTimeZone": "tzone://Microsoft/Custom",
			"start": map[string]any{"dateTime": "2026-03-04T14:00:00", "timeZone": "UTC"}, "end": map[string]any{"dateTime": "2026-03-04T15:00:00", "timeZone": "UTC"}},
	}
	from, to := win(t)
	page, err := c.ListEvents(t.Context(), provider.ListEventsQuery{Start: from, End: to})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Events) != 3 {
		t.Fatalf("events = %d, want the cancelled one dropped", len(page.Events))
	}
	h, ad, odd := page.Events[0], page.Events[1], page.Events[2]
	if h.Start.Zone != "America/Los_Angeles" || h.Start.DateTime.Hour() != 1 || !h.Start.DateTime.Equal(time.Date(2026, 3, 4, 9, 0, 0, 0, time.UTC)) {
		t.Errorf("a Windows zone name was not mapped: %+v", h.Start)
	}
	if h.Description != "Agenda\n\n- one" && !strings.Contains(h.Description, "Agenda") || strings.Contains(h.Description, "<") {
		t.Errorf("html body = %q, want text", h.Description)
	}
	if h.Location != "HQ" || h.Organizer.Email != "ANN@example.com" || h.Organizer.Name != "Ann" || h.Organizer.Self {
		t.Errorf("event = %+v", h)
	}
	if len(h.Attendees) != 2 || !h.Attendees[0].Self || h.Attendees[0].Response != provider.ResponseAccepted || h.Attendees[1].Self || !h.Attendees[1].Optional || h.Attendees[1].Response != provider.ResponseNone {
		t.Errorf("attendees = %+v, want the account recognised case-insensitively", h.Attendees)
	}
	if h.SelfResponse != provider.ResponseTentative || !h.Video || h.VideoURL != "https://teams.example.com/j" || h.ReminderMinutes == nil || *h.ReminderMinutes != 30 || h.Version != "ck1" {
		t.Errorf("event = %+v", h)
	}
	if !ad.Start.IsAllDay() || ad.Start.Date != "2026-03-10" || ad.End.Date != "2026-03-12" || ad.Start.Zone != "" {
		t.Errorf("all-day = %+v", ad)
	}
	if odd.Start.Zone != "UTC" || odd.Start.DateTime.Hour() != 14 {
		t.Errorf("an unknown creation zone is shown as UTC with the right instant: %+v", odd.Start)
	}

	// Free/busy from the same raw events: free (all-day), cancelled and declined do not block.
	s.rawView = append(s.rawView,
		map[string]any{"id": "decl", "showAs": "busy", "responseStatus": map[string]any{"response": "declined"},
			"start": map[string]any{"dateTime": "2026-03-04T16:00:00.0000000", "timeZone": "UTC"}, "end": map[string]any{"dateTime": "2026-03-04T17:00:00.0000000", "timeZone": "UTC"}},
		map[string]any{"id": "oof", "showAs": "oof",
			"start": map[string]any{"dateTime": "2026-03-04T17:00:00.0000000", "timeZone": "UTC"}, "end": map[string]any{"dateTime": "2026-03-04T18:00:00.0000000", "timeZone": "UTC"}},
		map[string]any{"id": "else", "showAs": "workingElsewhere",
			"start": map[string]any{"dateTime": "2026-03-04T19:00:00.0000000", "timeZone": "UTC"}, "end": map[string]any{"dateTime": "2026-03-04T20:00:00.0000000", "timeZone": "UTC"}},
		map[string]any{"id": "tent", "showAs": "tentative",
			"start": map[string]any{"dateTime": "2026-03-04T18:00:00.0000000", "timeZone": "UTC"}, "end": map[string]any{"dateTime": "2026-03-04T18:30:00.0000000", "timeZone": "UTC"}},
	)
	res, err := c.FreeBusy(t.Context(), provider.FreeBusyQuery{Start: from, End: to})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, b := range res.Busy[prim] {
		got = append(got, fmt.Sprintf("%02d:%02d-%02d:%02d", b.Start.Hour(), b.Start.Minute(), b.End.Hour(), b.End.Minute()))
	}
	// Board 09-10 and Odd zone 14-15 are busy; oof 17-18 and tentative 18-18:30 join; declined, free,
	// working elsewhere and cancelled do not.
	if strings.Join(got, ",") != "09:00-10:00,14:00-15:00,17:00-18:30" {
		t.Errorf("busy = %v", got)
	}
}

func TestAMissingCalendarGrantIsAnAuthFailure(t *testing.T) {
	s := newStub(t)
	c := s.client(false)
	prim := fakePrimary(t, c)
	s.deny = true
	from, to := win(t)
	if _, err := c.ListEvents(t.Context(), provider.ListEventsQuery{Start: from, End: to}); !errors.Is(err, provider.ErrAuth) {
		t.Errorf("403 on calendarView err = %v, want ErrAuth (the calendar consent is missing)", err)
	}
	if _, err := c.GetEvent(t.Context(), provider.EventRef{CalendarID: prim, EventID: "x"}); !errors.Is(err, provider.ErrAuth) {
		t.Errorf("403 on the event err = %v, want ErrAuth", err)
	}
}

func TestRetryAfterReachesTheCaller(t *testing.T) {
	s := newStub(t)
	c := s.client(false)
	fakePrimary(t, c)
	s.p.Err = &provider.RateLimitError{RetryAfter: providertest.RetryAfter}
	from, to := win(t)
	_, err := c.ListEvents(t.Context(), provider.ListEventsQuery{Start: from, End: to, CalendarIDs: []string{"x"}})
	var rl *provider.RateLimitError
	if !errors.As(err, &rl) || rl.RetryAfter != providertest.RetryAfter {
		t.Errorf("err = %v, want the Retry-After hint kept", err)
	}
}

func TestMergedCalendarsPageInStartOrder(t *testing.T) {
	s := newStub(t)
	c := s.client(false)
	prim := fakePrimary(t, c)
	s.p.AddCalendar(provider.Calendar{ID: "team", Name: "Team", Role: provider.RoleWriter})
	for i := range 4 {
		s.p.AddEvent(prim, provider.Event{Title: fmt.Sprintf("p%d", i), Start: berlin(t, 4, 8+2*i), End: berlin(t, 4, 9+2*i)})
		s.p.AddEvent("team", provider.Event{Title: fmt.Sprintf("t%d", i), Start: berlin(t, 4, 9+2*i), End: berlin(t, 4, 10+2*i)})
	}
	from, to := win(t)
	var got []string
	token := ""
	for range 10 {
		page, err := c.ListEvents(t.Context(), provider.ListEventsQuery{Start: from, End: to, CalendarIDs: []string{prim, "team"}, Limit: 3, PageToken: token})
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range page.Events {
			got = append(got, e.Title)
		}
		if token = page.NextPageToken; token == "" {
			break
		}
	}
	if strings.Join(got, ",") != "p0,t0,p1,t1,p2,t2,p3,t3" {
		t.Errorf("merged = %v", got)
	}
}
