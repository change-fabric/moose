package google_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/change-fabric/moose/internal/calendar/fake"
	"github.com/change-fabric/moose/internal/calendar/provider"
)

func tm(t *testing.T, zone string, d, h int) provider.EventTime {
	t.Helper()
	l, err := time.LoadLocation(zone)
	if err != nil {
		t.Fatal(err)
	}
	return provider.EventTime{DateTime: time.Date(2026, 3, d, h, 0, 0, 0, l), Zone: zone}
}

func window(t *testing.T) (time.Time, time.Time) {
	t.Helper()
	l, _ := time.LoadLocation("Europe/Berlin")
	return time.Date(2026, 3, 1, 0, 0, 0, 0, l), time.Date(2026, 4, 30, 0, 0, 0, 0, l)
}

func TestListEventsAsksForSingleEventsOrderedByStartInAUTCWindow(t *testing.T) {
	s, srv := newStub(t)
	c := newClient(srv)
	s.p.AddEvent(fake.PrimaryID, provider.Event{Title: "x", Start: tm(t, "Europe/Berlin", 4, 9), End: tm(t, "Europe/Berlin", 4, 10)})
	from, to := window(t)
	var seen string
	s.override = func(w http.ResponseWriter, r *http.Request) bool {
		if strings.HasSuffix(r.URL.Path, "/events") {
			seen = r.URL.RawQuery
		}
		return false
	}
	if _, err := c.ListEvents(t.Context(), provider.ListEventsQuery{Start: from, End: to, Text: "x y", Limit: 7}); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"singleEvents=true", "orderBy=startTime", "maxResults=7", "q=x+y", "timeMin=2026-02-28T23%3A00%3A00Z", "timeMax=2026-04-29T22%3A00%3A00Z"} {
		if !strings.Contains(seen, want) {
			t.Errorf("query %q lacks %s", seen, want)
		}
	}
	// The primary calendar is looked up once and then used by its real id.
	if got := s.hits["GET /calendar/v3/users/me/calendarList"]; got != 1 {
		t.Errorf("calendar list read %d times", got)
	}
	if got := s.hits["GET /calendar/v3/calendars/"+fake.PrimaryID+"/events"]; got != 1 {
		t.Errorf("events read %d times from the primary id", got)
	}
}

func TestOccurrencesGetTheirSeriesRuleFromOneMasterReadPerSeries(t *testing.T) {
	s, srv := newStub(t)
	c := newClient(srv)
	for i := range 3 {
		m := provider.Event{Title: fmt.Sprintf("series %d", i), Start: tm(t, "Europe/Berlin", 23, 9+i), End: tm(t, "Europe/Berlin", 23, 10+i),
			Recurrence: &provider.Recurrence{Freq: provider.FreqWeekly, ByDay: []string{"MO"}, Count: 3}}
		s.p.AddEvent(fake.PrimaryID, m)
	}
	from, to := window(t)
	page, err := c.ListEvents(t.Context(), provider.ListEventsQuery{Start: from, End: to})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Events) != 9 {
		t.Fatalf("%d occurrences", len(page.Events))
	}
	for _, e := range page.Events {
		r := e.Recurrence
		if r == nil || r.Freq != "WEEKLY" || r.Count != 3 || r.Native != "" || strings.Join(r.ByDay, "") != "MO" {
			t.Fatalf("occurrence recurrence = %+v", r)
		}
	}
	masterReads := 0
	for k, n := range s.hits {
		if strings.Contains(k, "/events/") {
			masterReads += n
		}
	}
	if masterReads != 3 {
		t.Errorf("%d master reads for 3 series of 3 occurrences, want one per series", masterReads)
	}
}

func TestAMasterThatCannotBeReadLeavesTheRuleOpaque(t *testing.T) {
	s, srv := newStub(t)
	c := newClient(srv)
	s.p.AddEvent(fake.PrimaryID, provider.Event{Title: "s", Start: tm(t, "Europe/Berlin", 23, 9), End: tm(t, "Europe/Berlin", 23, 10),
		Recurrence: &provider.Recurrence{Freq: provider.FreqDaily, Count: 2}})
	s.override = func(w http.ResponseWriter, r *http.Request) bool {
		if strings.Contains(r.URL.Path, "/events/") {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":{"code":404,"message":"gone","errors":[{"reason":"notFound"}]}}`))
			return true
		}
		return false
	}
	from, to := window(t)
	page, err := c.ListEvents(t.Context(), provider.ListEventsQuery{Start: from, End: to})
	if err != nil || len(page.Events) != 2 {
		t.Fatal(page, err)
	}
	for _, e := range page.Events {
		if e.Recurrence == nil || !e.Recurrence.IsOpaque() {
			t.Errorf("recurrence = %+v, want opaque when the master is unreadable", e.Recurrence)
		}
	}
}

func TestZoneShowsEventsInTheRequestedZoneAndRejectsBadOnes(t *testing.T) {
	s, srv := newStub(t)
	c := newClient(srv)
	s.p.AddEvent(fake.PrimaryID, provider.Event{Title: "x", Start: tm(t, "Europe/Berlin", 4, 9), End: tm(t, "Europe/Berlin", 4, 10)})
	from, to := window(t)
	page, err := c.ListEvents(t.Context(), provider.ListEventsQuery{Start: from, End: to, Zone: "Asia/Tokyo"})
	if err != nil || len(page.Events) != 1 || page.Events[0].Start.Zone != "Asia/Tokyo" || page.Events[0].Start.DateTime.Hour() != 17 {
		t.Fatal(page, err)
	}
	if _, err := c.ListEvents(t.Context(), provider.ListEventsQuery{Start: from, End: to, Zone: "PST"}); !errors.Is(err, provider.ErrInvalidQuery) {
		t.Errorf("bad zone err = %v", err)
	}
}

func TestAnEventWithoutAZoneUsesTheCalendarsZone(t *testing.T) {
	s, srv := newStub(t)
	c := newClient(srv)
	s.override = func(w http.ResponseWriter, r *http.Request) bool {
		if !strings.HasSuffix(r.URL.Path, "/events") {
			return false
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"timeZone": "America/New_York", "items": []map[string]any{
			{"id": "a", "etag": `"1"`, "status": "confirmed", "summary": "no zone",
				"start": map[string]any{"dateTime": "2026-03-04T09:00:00-05:00"}, "end": map[string]any{"dateTime": "2026-03-04T10:00:00-05:00"}},
			{"id": "b", "etag": `"1"`, "status": "cancelled", "summary": "gone",
				"start": map[string]any{"dateTime": "2026-03-04T11:00:00-05:00"}, "end": map[string]any{"dateTime": "2026-03-04T12:00:00-05:00"}},
			{"id": "c", "etag": `"1"`, "status": "confirmed", "summary": "odd zone",
				"start": map[string]any{"dateTime": "2026-03-04T13:00:00Z", "timeZone": "Custom/Nowhere"}, "end": map[string]any{"dateTime": "2026-03-04T14:00:00Z", "timeZone": "Custom/Nowhere"}},
			{"id": "d", "etag": `"1"`, "status": "confirmed", "summary": "broken", "start": map[string]any{}, "end": map[string]any{}},
		}})
		return true
	}
	from, to := window(t)
	page, err := c.ListEvents(t.Context(), provider.ListEventsQuery{Start: from, End: to})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Events) != 2 {
		t.Fatalf("events = %+v, want the cancelled and the unreadable ones dropped", page.Events)
	}
	if e := page.Events[0]; e.Start.Zone != "America/New_York" || e.Start.DateTime.Hour() != 9 {
		t.Errorf("zone-less event = %+v, want the calendar's zone", e.Start)
	}
	if e := page.Events[1]; e.Start.Zone != "UTC" || e.Start.DateTime.Hour() != 13 {
		t.Errorf("event with an unusable zone = %+v, want UTC with the instant kept", e.Start)
	}
}

func TestErrorsMapToTheSharedSentinels(t *testing.T) {
	s, srv := newStub(t)
	c := newClient(srv)
	from, to := window(t)
	reply := func(status int, reason string, retryAfter string) {
		s.override = func(w http.ResponseWriter, r *http.Request) bool {
			if strings.HasSuffix(r.URL.Path, "/calendarList") {
				return false
			}
			if retryAfter != "" {
				w.Header().Set("Retry-After", retryAfter)
			}
			w.WriteHeader(status)
			_, _ = fmt.Fprintf(w, `{"error":{"code":%d,"message":"m","errors":[{"reason":%q}]}}`, status, reason)
			return true
		}
	}
	list := func() error {
		_, err := c.ListEvents(t.Context(), provider.ListEventsQuery{Start: from, End: to})
		return err
	}
	for _, tc := range []struct {
		status int
		reason string
		retry  string
		is     error
	}{
		{401, "authError", "", provider.ErrAuth},
		{403, "insufficientPermissions", "", provider.ErrAuth},
		{403, "rateLimitExceeded", "3", provider.ErrRateLimited},
		{403, "userRateLimitExceeded", "", provider.ErrRateLimited},
		{429, "rateLimitExceeded", "7", provider.ErrRateLimited},
		{404, "notFound", "", provider.ErrNotFound},
		{410, "deleted", "", provider.ErrNotFound},
		{400, "invalid", "", provider.ErrInvalidQuery},
		{403, "forbidden", "", provider.ErrRejected},
		{409, "conflict", "", provider.ErrRejected},
	} {
		reply(tc.status, tc.reason, tc.retry)
		err := list()
		if !errors.Is(err, tc.is) {
			t.Errorf("%d %s: err = %v, want %v", tc.status, tc.reason, err, tc.is)
		}
		var rl *provider.RateLimitError
		if tc.is == provider.ErrRateLimited && tc.retry != "" {
			want := map[string]time.Duration{"3": 3 * time.Second, "7": 7 * time.Second}[tc.retry]
			if !errors.As(err, &rl) || rl.RetryAfter != want {
				t.Errorf("%d: retry-after = %v, want %s", tc.status, err, want)
			}
		}
	}
	reply(500, "backendError", "")
	if err := list(); err == nil || errors.Is(err, provider.ErrRejected) || errors.Is(err, provider.ErrAuth) {
		t.Errorf("500 err = %v, want a plain failure", err)
	}
}

func TestFreeBusyOfOthersAsksOneRequestAndReportsUnreadablePeople(t *testing.T) {
	s, srv := newStub(t)
	c := newClient(srv)
	from, to := window(t)
	s.p.AddBusy("bob@example.com", provider.Interval{Start: tm(t, "Europe/Berlin", 4, 9).DateTime, End: tm(t, "Europe/Berlin", 4, 10).DateTime})
	res, err := c.FreeBusy(t.Context(), provider.FreeBusyQuery{Emails: []string{"Bob@Example.com", "ghost@example.com"}, Start: from, End: to})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Busy["bob@example.com"]) != 1 {
		t.Errorf("busy = %+v", res.Busy)
	}
	if res.Unavailable["ghost@example.com"] == "" {
		t.Errorf("unavailable = %+v, want the unreadable person listed, not free", res.Unavailable)
	}
	if got := s.hits["POST /calendar/v3/freeBusy"]; got != 1 {
		t.Errorf("%d freeBusy requests, want one", got)
	}
	many := make([]string, 60)
	for i := range many {
		many[i] = fmt.Sprintf("p%d@example.com", i)
	}
	if _, err := c.FreeBusy(t.Context(), provider.FreeBusyQuery{Emails: many, Start: from, End: to}); !errors.Is(err, provider.ErrInvalidQuery) {
		t.Errorf("too many people err = %v", err)
	}
	// An own calendar Google cannot find is an error, not free time.
	if _, err := c.FreeBusy(t.Context(), provider.FreeBusyQuery{CalendarIDs: []string{"nope@group.calendar.google.com"}, Start: from, End: to}); !errors.Is(err, provider.ErrNotFound) {
		t.Errorf("own calendar missing err = %v, want ErrNotFound", err)
	}
}

func TestMergedCalendarsPageInStartOrder(t *testing.T) {
	s, srv := newStub(t)
	c := newClient(srv)
	s.p.AddCalendar(provider.Calendar{ID: "team@group", Name: "Team", Role: provider.RoleWriter})
	for i := range 4 {
		s.p.AddEvent(fake.PrimaryID, provider.Event{Title: fmt.Sprintf("p%d", i), Start: tm(t, "Europe/Berlin", 4, 8+2*i), End: tm(t, "Europe/Berlin", 4, 9+2*i)})
		s.p.AddEvent("team@group", provider.Event{Title: fmt.Sprintf("t%d", i), Start: tm(t, "Europe/Berlin", 4, 9+2*i), End: tm(t, "Europe/Berlin", 4, 10+2*i)})
	}
	from, to := window(t)
	var got []string
	token := ""
	for range 10 {
		page, err := c.ListEvents(t.Context(), provider.ListEventsQuery{Start: from, End: to, CalendarIDs: []string{fake.PrimaryID, "team@group"}, Limit: 3, PageToken: token})
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

func TestCalendarRolesAndNames(t *testing.T) {
	s, srv := newStub(t)
	c := newClient(srv)
	s.p.AddCalendar(provider.Calendar{ID: "w", Name: "W", Role: provider.RoleWriter})
	s.p.AddCalendar(provider.Calendar{ID: "r", Name: "R", Role: provider.RoleReader})
	s.p.AddCalendar(provider.Calendar{ID: "f", Name: "F", Role: provider.RoleFreeBusyOnly})
	cals, err := c.ListCalendars(t.Context())
	if err != nil || len(cals) != 4 || !cals[0].Primary || cals[0].TimeZone != "Europe/Berlin" {
		t.Fatalf("calendars = %+v, %v", cals, err)
	}
	roles := map[string]string{}
	for _, cal := range cals {
		roles[cal.ID] = cal.Role
	}
	if roles["w"] != provider.RoleWriter || roles["r"] != provider.RoleReader || roles["f"] != provider.RoleFreeBusyOnly || roles[fake.PrimaryID] != provider.RoleOwner {
		t.Errorf("roles = %v", roles)
	}
}
