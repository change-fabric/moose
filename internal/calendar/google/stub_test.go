package google_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/change-fabric/moose/internal/calendar/fake"
	"github.com/change-fabric/moose/internal/calendar/google"
	"github.com/change-fabric/moose/internal/calendar/provider"
	"github.com/change-fabric/moose/internal/calendar/provider/providertest"
)

// stub is a Google Calendar v3 API server over a fake.Provider: it speaks
// Google's JSON and status codes and keeps its data in the fake, so the
// conformance suite exercises the real client's parsing, paging and error
// mapping against Google-shaped answers.
type stub struct {
	t *testing.T
	p *fake.Provider
	// Hits counts requests by "METHOD path", for tests that assert call patterns.
	hits map[string]int
	// Calendar zones are reported on the calendar list and event list.
	zone string
	// override, when set, answers a request instead of the stub; it reports
	// whether it did.
	override func(w http.ResponseWriter, r *http.Request) bool
}

func newStub(t *testing.T) (*stub, *httptest.Server) {
	s := &stub{t: t, p: fake.New(providertest.SelfAddress), hits: map[string]int{}, zone: "Europe/Berlin"}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /calendar/v3/users/me/calendarList", s.calendarList)
	mux.HandleFunc("GET /calendar/v3/calendars/{cal}/events", s.events)
	mux.HandleFunc("GET /calendar/v3/calendars/{cal}/events/{id}", s.event)
	mux.HandleFunc("POST /calendar/v3/freeBusy", s.freeBusy)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.hits[r.Method+" "+r.URL.Path]++
		if s.override != nil && s.override(w, r) {
			return
		}
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	return s, srv
}

func (s *stub) fail(w http.ResponseWriter, err error) {
	var rl *provider.RateLimitError
	code, reason := http.StatusInternalServerError, "backendError"
	switch {
	case errors.As(err, &rl):
		code, reason = http.StatusTooManyRequests, "rateLimitExceeded"
		w.Header().Set("Retry-After", strconv.Itoa(int(rl.RetryAfter.Seconds())))
	case errors.Is(err, provider.ErrAuth):
		code, reason = http.StatusUnauthorized, "authError"
	case errors.Is(err, provider.ErrNotFound):
		code, reason = http.StatusNotFound, "notFound"
	case errors.Is(err, provider.ErrInvalidQuery):
		code, reason = http.StatusBadRequest, "invalid"
	}
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{
		"code": code, "message": err.Error(), "errors": []map[string]string{{"reason": reason}},
	}})
}

func (s *stub) calendarList(w http.ResponseWriter, r *http.Request) {
	cals, err := s.p.ListCalendars(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	var items []map[string]any
	// Google lists the primary wherever it likes; put it last to prove the
	// client orders it first.
	for i := len(cals) - 1; i >= 0; i-- {
		c := cals[i]
		role := map[string]string{provider.RoleOwner: "owner", provider.RoleWriter: "writer", provider.RoleReader: "reader", provider.RoleFreeBusyOnly: "freeBusyReader"}[c.Role]
		items = append(items, map[string]any{"id": c.ID, "summary": c.Name, "primary": c.Primary, "accessRole": role, "timeZone": s.zone})
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"items": items})
}

func gTime(t provider.EventTime) map[string]any {
	if t.IsAllDay() {
		return map[string]any{"date": t.Date}
	}
	return map[string]any{"dateTime": t.DateTime.Format(time.RFC3339), "timeZone": t.Zone}
}

func rrule(r *provider.Recurrence) string {
	parts := []string{"FREQ=" + r.Freq}
	if r.Interval > 1 {
		parts = append(parts, "INTERVAL="+strconv.Itoa(r.Interval))
	}
	if len(r.ByDay) > 0 {
		parts = append(parts, "BYDAY="+strings.Join(r.ByDay, ","))
	}
	if r.Until != "" {
		parts = append(parts, "UNTIL="+strings.ReplaceAll(r.Until, "-", ""))
	}
	if r.Count > 0 {
		parts = append(parts, "COUNT="+strconv.Itoa(r.Count))
	}
	return "RRULE:" + strings.Join(parts, ";")
}

func gEvent(ev provider.Event) map[string]any {
	out := map[string]any{
		"id": ev.ID, "etag": `"` + ev.Version + `"`, "status": ev.Status, "summary": ev.Title,
		"description": ev.Description, "location": ev.Location,
		"start": gTime(ev.Start), "end": gTime(ev.End),
		"organizer": map[string]any{"email": ev.Organizer.Email, "displayName": ev.Organizer.Name, "self": ev.Organizer.Self || ev.Organizer.Email == providertest.SelfAddress},
	}
	if ev.SeriesID != "" && ev.ID != ev.SeriesID {
		out["recurringEventId"] = ev.SeriesID
		if ev.OriginalStart != nil {
			out["originalStartTime"] = gTime(*ev.OriginalStart)
		}
	} else if ev.Recurrence != nil {
		out["recurrence"] = []string{rrule(ev.Recurrence)}
	}
	var attendees []map[string]any
	for _, a := range ev.Attendees {
		attendees = append(attendees, map[string]any{
			"email": a.Email, "displayName": a.Name, "optional": a.Optional,
			"self":           a.Self || strings.EqualFold(a.Email, providertest.SelfAddress),
			"responseStatus": map[string]string{provider.ResponseNone: "needsAction", provider.ResponseAccepted: "accepted", provider.ResponseDeclined: "declined", provider.ResponseTentative: "tentative", "": "needsAction"}[a.Response],
		})
	}
	if attendees != nil {
		out["attendees"] = attendees
	}
	if ev.ReminderMinutes != nil {
		out["reminders"] = map[string]any{"useDefault": false, "overrides": []map[string]any{{"method": "popup", "minutes": *ev.ReminderMinutes}}}
	} else {
		out["reminders"] = map[string]any{"useDefault": true}
	}
	if ev.Video {
		out["conferenceData"] = map[string]any{"entryPoints": []map[string]string{{"entryPointType": "video", "uri": ev.VideoURL}}}
	}
	return out
}

func (s *stub) events(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if q.Get("singleEvents") != "true" || q.Get("orderBy") != "startTime" {
		s.t.Errorf("events.list must ask for singleEvents=true and orderBy=startTime: %v", q)
	}
	from, err1 := time.Parse(time.RFC3339, q.Get("timeMin"))
	to, err2 := time.Parse(time.RFC3339, q.Get("timeMax"))
	if err1 != nil || err2 != nil {
		s.fail(w, fmt.Errorf("%w: bad window", provider.ErrInvalidQuery))
		return
	}
	limit, _ := strconv.Atoi(q.Get("maxResults"))
	page, err := s.p.ListEvents(r.Context(), provider.ListEventsQuery{
		CalendarIDs: []string{r.PathValue("cal")}, Start: from, End: to, Text: q.Get("q"), PageToken: q.Get("pageToken"), Limit: limit,
	})
	if err != nil {
		s.fail(w, err)
		return
	}
	items := make([]map[string]any, 0, len(page.Events))
	for _, ev := range page.Events {
		items = append(items, gEvent(ev))
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"items": items, "nextPageToken": page.NextPageToken, "timeZone": s.zone})
}

func (s *stub) event(w http.ResponseWriter, r *http.Request) {
	ev, err := s.p.GetEvent(r.Context(), provider.EventRef{CalendarID: r.PathValue("cal"), EventID: r.PathValue("id")})
	if err != nil {
		s.fail(w, err)
		return
	}
	_ = json.NewEncoder(w).Encode(gEvent(ev))
}

func (s *stub) freeBusy(w http.ResponseWriter, r *http.Request) {
	var body struct {
		TimeMin string              `json:"timeMin"`
		TimeMax string              `json:"timeMax"`
		Items   []map[string]string `json:"items"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	from, _ := time.Parse(time.RFC3339, body.TimeMin)
	to, _ := time.Parse(time.RFC3339, body.TimeMax)
	cals, err := s.p.ListCalendars(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	isCal := map[string]bool{}
	for _, c := range cals {
		isCal[c.ID] = true
	}
	out := map[string]any{}
	for _, it := range body.Items {
		id := it["id"]
		q := provider.FreeBusyQuery{Start: from, End: to}
		if isCal[id] {
			q.CalendarIDs = []string{id}
		} else {
			q.Emails = []string{id}
		}
		res, err := s.p.FreeBusy(r.Context(), q)
		if err != nil {
			s.fail(w, err)
			return
		}
		if reason, bad := res.Unavailable[id]; bad {
			out[id] = map[string]any{"errors": []map[string]string{{"domain": "global", "reason": reason}}, "busy": []any{}}
			continue
		}
		busy := []map[string]string{}
		for _, iv := range res.Busy[id] {
			busy = append(busy, map[string]string{"start": iv.Start.UTC().Format(time.RFC3339), "end": iv.End.UTC().Format(time.RFC3339)})
		}
		out[id] = map[string]any{"busy": busy}
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"calendars": out})
}

type seeder struct{ p *fake.Provider }

func (s seeder) SeedCalendar(t *testing.T, c provider.Calendar) { s.p.AddCalendar(c) }
func (s seeder) SeedEvent(t *testing.T, calendarID string, ev provider.Event) provider.Event {
	return s.p.AddEvent(calendarID, ev)
}
func (s seeder) SeedBusy(t *testing.T, email string, in provider.Interval) { s.p.AddBusy(email, in) }
func (s seeder) ExternalAddAttendee(t *testing.T, ref provider.EventRef, a provider.Attendee) {
	s.p.ExternalAddAttendee(ref, a)
}
func (s seeder) Fail(t *testing.T, f providertest.Failure) {
	switch f {
	case providertest.FailNone:
		s.p.Err = nil
	case providertest.FailAuth:
		s.p.Err = fmt.Errorf("%w: revoked", provider.ErrAuth)
	case providertest.FailRateLimit:
		s.p.Err = &provider.RateLimitError{RetryAfter: providertest.RetryAfter}
	}
}
func (s seeder) Notified(t *testing.T) []providertest.Notification { return nil }

func newClient(srv *httptest.Server) *google.Client {
	c := google.New(nil)
	c.BaseURL = srv.URL + "/calendar/v3"
	return c
}

func TestConformanceRead(t *testing.T) {
	providertest.RunRead(t, func(t *testing.T) (provider.Provider, providertest.Seeder) {
		s, srv := newStub(t)
		return newClient(srv), seeder{s.p}
	})
}
