package google_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/change-fabric/moose/internal/calendar/fake"
	"github.com/change-fabric/moose/internal/calendar/provider"
)

// capture records the write requests the client sends.
type captured struct {
	method, path, query, ifMatch string
	body                         map[string]any
}

func capture(s *stub) *[]captured {
	var got []captured
	s.override = func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method == http.MethodGet {
			return false
		}
		raw, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(bytes.NewReader(raw))
		c := captured{method: r.Method, path: r.URL.Path, query: r.URL.RawQuery, ifMatch: r.Header.Get("If-Match")}
		_ = json.Unmarshal(raw, &c.body)
		got = append(got, c)
		return false
	}
	return &got
}

func berlinInput(t *testing.T) provider.EventInput {
	return provider.EventInput{Title: "Sync", Start: tm(t, "Europe/Berlin", 4, 9), End: tm(t, "Europe/Berlin", 4, 10)}
}

func TestCreateAsksGoogleToNotifyOnlyWhenThereAreGuests(t *testing.T) {
	s, srv := newStub(t)
	c := newClient(srv)
	got := capture(s)
	if _, err := c.CreateEvent(t.Context(), fake.PrimaryID, berlinInput(t), provider.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	in := berlinInput(t)
	in.Attendees = []provider.Attendee{{Email: "bob@example.com"}}
	if _, err := c.CreateEvent(t.Context(), fake.PrimaryID, in, provider.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if len(*got) != 2 || !strings.Contains((*got)[0].query, "sendUpdates=none") || !strings.Contains((*got)[1].query, "sendUpdates=all") {
		t.Fatalf("requests = %+v, want sendUpdates none then all", *got)
	}
}

func TestCreateSendsADerivedEventIDAndAMeetRequest(t *testing.T) {
	s, srv := newStub(t)
	c := newClient(srv)
	got := capture(s)
	in := berlinInput(t)
	in.Video = true
	rem := 15
	in.ReminderMinutes = &rem
	in.Recurrence = &provider.Recurrence{Freq: provider.FreqWeekly, ByDay: []string{"MO", "WE"}, Interval: 2, Until: "2026-06-30"}
	if _, err := c.CreateEvent(t.Context(), fake.PrimaryID, in, provider.CreateOptions{DedupeKey: "request one"}); err != nil {
		t.Fatal(err)
	}
	r := (*got)[0]
	id, _ := r.body["id"].(string)
	if len(id) != 64 || strings.Trim(id, "0123456789abcdef") != "" {
		t.Errorf("event id %q is not a base32hex-safe derived id", id)
	}
	if strings.Contains(id, "request") {
		t.Errorf("event id %q leaks the key", id)
	}
	if !strings.Contains(r.query, "conferenceDataVersion=1") {
		t.Errorf("query %q lacks conferenceDataVersion=1", r.query)
	}
	cd, _ := r.body["conferenceData"].(map[string]any)
	cr, _ := cd["createRequest"].(map[string]any)
	if key, _ := cr["conferenceSolutionKey"].(map[string]any); key["type"] != "hangoutsMeet" {
		t.Errorf("conferenceData = %+v", cd)
	}
	rec, _ := r.body["recurrence"].([]any)
	if len(rec) != 1 || rec[0] != "RRULE:FREQ=WEEKLY;INTERVAL=2;BYDAY=MO,WE;UNTIL=20260630T215959Z" {
		t.Errorf("recurrence = %v (until is the end of the day in Berlin, in UTC)", rec)
	}
	rm, _ := r.body["reminders"].(map[string]any)
	if rm["useDefault"] != false {
		t.Errorf("reminders = %+v", rm)
	}
}

func TestAnAllDayCreateSendsDatesWithoutAZone(t *testing.T) {
	s, srv := newStub(t)
	c := newClient(srv)
	got := capture(s)
	in := provider.EventInput{Title: "Holiday", Start: provider.EventTime{Date: "2026-03-04"}, End: provider.EventTime{Date: "2026-03-05"},
		Recurrence: &provider.Recurrence{Freq: provider.FreqYearly, Until: "2030-03-04"}}
	if _, err := c.CreateEvent(t.Context(), fake.PrimaryID, in, provider.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	r := (*got)[0]
	if st, _ := r.body["start"].(map[string]any); st["date"] != "2026-03-04" || st["timeZone"] != nil || st["dateTime"] != nil {
		t.Errorf("start = %+v", st)
	}
	if rec, _ := r.body["recurrence"].([]any); len(rec) != 1 || rec[0] != "RRULE:FREQ=YEARLY;UNTIL=20300304" {
		t.Errorf("recurrence = %v", rec)
	}
}

func TestADuplicateEventIDOnCreateReturnsTheExistingEvent(t *testing.T) {
	s, srv := newStub(t)
	c := newClient(srv)
	first, err := c.CreateEvent(t.Context(), fake.PrimaryID, berlinInput(t), provider.CreateOptions{DedupeKey: "k"})
	if err != nil {
		t.Fatal(err)
	}
	var existing string
	s.override = func(w http.ResponseWriter, r *http.Request) bool {
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/events"):
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"error":{"code":409,"message":"The requested identifier already exists.","errors":[{"reason":"duplicate"}]}}`))
			return true
		case r.Method == http.MethodGet && len(r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]) == 64:
			existing = r.URL.Path
			r.URL.Path = "/calendar/v3/calendars/" + fake.PrimaryID + "/events/" + first.ID
		}
		return false
	}
	again, err := c.CreateEvent(t.Context(), fake.PrimaryID, berlinInput(t), provider.CreateOptions{DedupeKey: "k"})
	if err != nil || again.ID != first.ID || existing == "" {
		t.Fatalf("again = %+v, %v, read %q: a 409 on the derived id must read that event back", again, err, existing)
	}
	// Without a dedupe key a 409 is just a refusal.
	if _, err := c.CreateEvent(t.Context(), fake.PrimaryID, berlinInput(t), provider.CreateOptions{}); !errors.Is(err, provider.ErrRejected) {
		t.Errorf("409 without a key err = %v, want ErrRejected", err)
	}
}

func TestUpdatePresentsTheEtagAndRefusesToWriteBlind(t *testing.T) {
	s, srv := newStub(t)
	c := newClient(srv)
	ev, _ := c.CreateEvent(t.Context(), fake.PrimaryID, berlinInput(t), provider.CreateOptions{})
	got := capture(s)
	title := "Renamed"
	if _, err := c.UpdateEvent(t.Context(), provider.EventRef{CalendarID: fake.PrimaryID, EventID: ev.ID}, provider.ScopeSeries, provider.EventPatch{Title: &title}, ev.Version); err != nil {
		t.Fatal(err)
	}
	r := (*got)[0]
	if r.method != http.MethodPatch || r.ifMatch != ev.Version || !strings.Contains(r.query, "sendUpdates=all") {
		t.Errorf("request = %+v, want a PATCH with If-Match %s and sendUpdates=all", r, ev.Version)
	}
	*got = nil
	for _, version := range []string{"", "  "} {
		_, uerr := c.UpdateEvent(t.Context(), provider.EventRef{CalendarID: fake.PrimaryID, EventID: ev.ID}, provider.ScopeInstance, provider.EventPatch{Title: &title}, version)
		derr := c.DeleteEvent(t.Context(), provider.EventRef{CalendarID: fake.PrimaryID, EventID: ev.ID}, provider.ScopeInstance, version)
		if !errors.Is(uerr, provider.ErrPreconditionFailed) || !errors.Is(derr, provider.ErrPreconditionFailed) {
			t.Errorf("blind write errs = %v, %v", uerr, derr)
		}
	}
	if len(*got) != 0 {
		t.Errorf("a write with no version sent %d requests", len(*got))
	}
}

func TestAStaleEtagIsA412NobodyIsToldAbout(t *testing.T) {
	s, srv := newStub(t)
	c := newClient(srv)
	in := berlinInput(t)
	in.Attendees = []provider.Attendee{{Email: "bob@example.com"}}
	ev, _ := c.CreateEvent(t.Context(), fake.PrimaryID, in, provider.CreateOptions{})
	ref := provider.EventRef{CalendarID: fake.PrimaryID, EventID: ev.ID}
	s.p.ExternalAddAttendee(ref, provider.Attendee{Email: "mallory@example.com"})
	before := len(s.p.Notifications())
	title := "x"
	if _, err := c.UpdateEvent(t.Context(), ref, provider.ScopeInstance, provider.EventPatch{Title: &title}, ev.Version); !errors.Is(err, provider.ErrPreconditionFailed) {
		t.Fatalf("err = %v, want ErrPreconditionFailed", err)
	}
	if err := c.DeleteEvent(t.Context(), ref, provider.ScopeInstance, ev.Version); !errors.Is(err, provider.ErrPreconditionFailed) {
		t.Fatalf("delete err = %v, want ErrPreconditionFailed", err)
	}
	if len(s.p.Notifications()) != before {
		t.Errorf("a stale write notified %+v", s.p.Notifications()[before:])
	}
}

func TestASeriesWriteThroughAnOccurrenceGoesToTheMasterWithTheMastersEtag(t *testing.T) {
	s, srv := newStub(t)
	c := newClient(srv)
	in := berlinInput(t)
	in.Recurrence = &provider.Recurrence{Freq: provider.FreqDaily, Count: 3}
	master, err := c.CreateEvent(t.Context(), fake.PrimaryID, in, provider.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	from, to := window(t)
	page, err := c.ListEvents(t.Context(), provider.ListEventsQuery{Start: from, End: to})
	if err != nil || len(page.Events) != 3 {
		t.Fatal(page, err)
	}
	occ := page.Events[1]
	got := capture(s)
	loc := "HQ"
	if _, err := c.UpdateEvent(t.Context(), provider.EventRef{CalendarID: fake.PrimaryID, EventID: occ.ID}, provider.ScopeSeries, provider.EventPatch{Location: &loc}, occ.Version); err != nil {
		t.Fatal(err)
	}
	r := (*got)[0]
	if !strings.HasSuffix(r.path, "/events/"+master.ID) || r.ifMatch == "" {
		t.Errorf("request = %+v, want a PATCH of the master %s", r, master.ID)
	}
	// An occurrence read at an older version is refused before any write.
	*got = nil
	if _, err := c.UpdateEvent(t.Context(), provider.EventRef{CalendarID: fake.PrimaryID, EventID: occ.ID}, provider.ScopeSeries, provider.EventPatch{Location: &loc}, occ.Version); !errors.Is(err, provider.ErrPreconditionFailed) {
		t.Errorf("err = %v, want ErrPreconditionFailed for an occurrence at a stale version", err)
	}
	if len(*got) != 0 {
		t.Errorf("a stale series write sent %+v", *got)
	}
}

func TestClearingARecurrenceSendsNull(t *testing.T) {
	s, srv := newStub(t)
	c := newClient(srv)
	in := berlinInput(t)
	in.Recurrence = &provider.Recurrence{Freq: provider.FreqDaily, Count: 3}
	m, _ := c.CreateEvent(t.Context(), fake.PrimaryID, in, provider.CreateOptions{})
	got := capture(s)
	if _, err := c.UpdateEvent(t.Context(), provider.EventRef{CalendarID: fake.PrimaryID, EventID: m.ID}, provider.ScopeSeries, provider.EventPatch{ClearRecurrence: true}, m.Version); err != nil {
		t.Fatal(err)
	}
	if v, present := (*got)[0].body["recurrence"]; !present || v != nil {
		t.Errorf("body = %+v, want recurrence: null", (*got)[0].body)
	}
}

func TestRSVPPatchesOnlyTheAccountsAnswerWithTheEtagJustRead(t *testing.T) {
	s, srv := newStub(t)
	c := newClient(srv)
	invite := s.p.AddEvent(fake.PrimaryID, provider.Event{
		Title: "Theirs", Start: tm(t, "Europe/Berlin", 4, 9), End: tm(t, "Europe/Berlin", 4, 10),
		Organizer: provider.Attendee{Email: "boss@example.com"},
		Attendees: []provider.Attendee{{Email: "boss@example.com", Response: provider.ResponseAccepted}, {Email: "me@example.com", Response: provider.ResponseNone}, {Email: "dan@example.com", Response: provider.ResponseTentative}},
	})
	got := capture(s)
	if err := c.Respond(t.Context(), provider.EventRef{CalendarID: fake.PrimaryID, EventID: invite.ID}, provider.RespondTentative, "Might be late"); err != nil {
		t.Fatal(err)
	}
	r := (*got)[0]
	if r.method != http.MethodPatch || r.ifMatch == "" || !strings.Contains(r.query, "sendUpdates=all") || len(r.body) != 1 {
		t.Fatalf("request = %+v", r)
	}
	atts, _ := r.body["attendees"].([]any)
	if len(atts) != 3 {
		t.Fatalf("attendees = %v, want every guest kept", atts)
	}
	for _, a := range atts {
		m := a.(map[string]any)
		if self, _ := m["self"].(bool); self {
			if m["responseStatus"] != "tentative" || m["comment"] != "Might be late" {
				t.Errorf("self = %+v", m)
			}
		} else if m["comment"] != nil {
			t.Errorf("another guest got a comment: %+v", m)
		}
	}
	if len(s.rsvpComments) != 1 || s.rsvpComments[0] != "Might be late" {
		t.Errorf("stub saw comments %v", s.rsvpComments)
	}
}

func TestAReadOnlyCalendarIsRefusedBeforeAnyWrite(t *testing.T) {
	s, srv := newStub(t)
	c := newClient(srv)
	s.p.AddCalendar(provider.Calendar{ID: "team", Name: "Team", Role: provider.RoleReader})
	got := capture(s)
	if _, err := c.CreateEvent(t.Context(), "team", berlinInput(t), provider.CreateOptions{}); !errors.Is(err, provider.ErrRejected) {
		t.Errorf("err = %v, want ErrRejected", err)
	}
	title := "x"
	if _, err := c.UpdateEvent(t.Context(), provider.EventRef{CalendarID: "team", EventID: "e"}, provider.ScopeInstance, provider.EventPatch{Title: &title}, "v"); !errors.Is(err, provider.ErrRejected) {
		t.Errorf("update err = %v, want ErrRejected", err)
	}
	if len(*got) != 0 {
		t.Errorf("sent %+v", *got)
	}
}
