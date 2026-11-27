package graph_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/change-fabric/moose/internal/calendar/fake"
	"github.com/change-fabric/moose/internal/calendar/graph"
	"github.com/change-fabric/moose/internal/calendar/provider"
	"github.com/change-fabric/moose/internal/calendar/provider/providertest"
	"github.com/change-fabric/moose/internal/graphapi"
)

const graphLayout = "2006-01-02T15:04:05.0000000"

// stub is a Microsoft Graph server over a fake.Provider. It speaks Graph's
// JSON, honours the Prefer: outlook.timezone header, pages with
// @odata.nextLink, reports an event's creation zone as a Windows zone name, and
// answers getSchedule, so the conformance suite exercises the real client's
// parsing and error mapping against Graph-shaped answers.
type stub struct {
	t    *testing.T
	p    *fake.Provider
	srv  *httptest.Server
	reqs []*http.Request
	// calendarViewCalls counts calendarView requests, for call-pattern tests.
	calendarViewCalls int
	// Items overrides the events calendarView returns (a raw Graph "value"),
	// for fixtures the fake cannot produce.
	rawView []map[string]any
	// deny makes the calendar endpoints answer 403 ErrorAccessDenied, as Graph
	// does when the app holds no calendar consent.
	deny bool
}

func (s *stub) denied(w http.ResponseWriter) bool {
	if !s.deny {
		return false
	}
	w.WriteHeader(http.StatusForbidden)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"code": "ErrorAccessDenied", "message": "Access is denied."}})
	return true
}

func newStub(t *testing.T) *stub {
	s := &stub{t: t, p: fake.New(providertest.SelfAddress)}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /me/calendars", s.calendars)
	mux.HandleFunc("GET /me/calendars/{cal}/calendarView", s.calendarView)
	mux.HandleFunc("GET /me/calendars/{cal}/events/{id}", s.event)
	mux.HandleFunc("POST /me/calendar/getSchedule", s.getSchedule)
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Prefer") == "" || !strings.Contains(r.Header.Get("Prefer"), `IdType="ImmutableId"`) {
			t.Errorf("request %s %s lacks the ImmutableId Prefer header: %q", r.Method, r.URL.Path, r.Header.Get("Prefer"))
		}
		s.reqs = append(s.reqs, r)
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *stub) client(personal bool) *graph.Client {
	g := graphapi.New(nil)
	g.BaseURL = s.srv.URL
	return graph.New(g, graph.Options{Self: providertest.SelfAddress, Personal: personal})
}

func (s *stub) fail(w http.ResponseWriter, err error) {
	var rl *provider.RateLimitError
	status, code := http.StatusInternalServerError, "generalException"
	switch {
	case errors.As(err, &rl):
		status, code = http.StatusTooManyRequests, "TooManyRequests"
		w.Header().Set("Retry-After", strconv.Itoa(int(rl.RetryAfter.Seconds())))
	case errors.Is(err, provider.ErrAuth):
		status, code = http.StatusUnauthorized, "InvalidAuthenticationToken"
	case errors.Is(err, provider.ErrNotFound):
		status, code = http.StatusNotFound, "ErrorItemNotFound"
	case errors.Is(err, provider.ErrInvalidQuery):
		status, code = http.StatusBadRequest, "ErrorInvalidRequest"
	}
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"code": code, "message": err.Error()}})
}

func (s *stub) calendars(w http.ResponseWriter, r *http.Request) {
	cals, err := s.p.ListCalendars(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	var value []map[string]any
	for i := len(cals) - 1; i >= 0; i-- { // the default calendar last, to prove ordering
		c := cals[i]
		value = append(value, map[string]any{
			"id": c.ID, "name": c.Name, "isDefaultCalendar": c.Primary, "canEdit": c.Role == provider.RoleWriter || c.Role == provider.RoleOwner,
			"owner": map[string]any{"address": providertest.SelfAddress},
		})
	}
	// Owned secondary calendars are owner; a shared one is not owned by us.
	for _, v := range value {
		for _, c := range cals {
			if c.ID == v["id"] && c.Role != provider.RoleOwner && !c.Primary {
				v["owner"] = map[string]any{"address": "someone@example.com"}
			}
		}
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"value": value})
}

var windowsName = map[string]string{"Europe/Berlin": "W. Europe Standard Time", "UTC": "UTC"}

func zoneFromPrefer(p string) string {
	for _, part := range strings.Split(p, ",") {
		part = strings.TrimSpace(part)
		if strings.HasPrefix(part, "outlook.timezone=") {
			return strings.Trim(strings.TrimPrefix(part, "outlook.timezone="), `"`)
		}
	}
	return "UTC"
}

func dtz(t provider.EventTime, zone string, allDay bool) map[string]any {
	if allDay {
		d, _ := time.Parse(provider.DateLayout, t.Date)
		return map[string]any{"dateTime": d.Format(graphLayout), "timeZone": zone}
	}
	loc, _ := time.LoadLocation(zone)
	return map[string]any{"dateTime": t.DateTime.In(loc).Format(graphLayout), "timeZone": zone}
}

func response(a string) string {
	return map[string]string{provider.ResponseAccepted: "accepted", provider.ResponseDeclined: "declined", provider.ResponseTentative: "tentativelyAccepted", provider.ResponseNone: "notResponded", "": "notResponded"}[a]
}

func patterned(r *provider.Recurrence, start provider.EventTime) map[string]any {
	pat := map[string]any{"interval": max(r.Interval, 1)}
	switch r.Freq {
	case provider.FreqDaily:
		pat["type"] = "daily"
	case provider.FreqWeekly:
		pat["type"] = "weekly"
		names := map[string]string{"MO": "monday", "TU": "tuesday", "WE": "wednesday", "TH": "thursday", "FR": "friday", "SA": "saturday", "SU": "sunday"}
		var days []string
		for _, d := range r.ByDay {
			days = append(days, names[d])
		}
		if len(days) == 0 {
			days = []string{strings.ToLower(start.DateTime.Weekday().String())}
		}
		pat["daysOfWeek"], pat["firstDayOfWeek"] = days, "sunday"
	case provider.FreqMonthly:
		pat["type"], pat["dayOfMonth"] = "absoluteMonthly", start.DateTime.Day()
	case provider.FreqYearly:
		pat["type"], pat["dayOfMonth"], pat["month"] = "absoluteYearly", start.DateTime.Day(), int(start.DateTime.Month())
	}
	rng := map[string]any{"startDate": start.DateTime.Format(provider.DateLayout), "recurrenceTimeZone": start.Zone}
	switch {
	case r.Count > 0:
		rng["type"], rng["numberOfOccurrences"] = "numbered", r.Count
	case r.Until != "":
		rng["type"], rng["endDate"] = "endDate", r.Until
	default:
		rng["type"] = "noEnd"
	}
	return map[string]any{"pattern": pat, "range": rng}
}

func gEvent(ev provider.Event, zone string) map[string]any {
	allDay := ev.Start.IsAllDay()
	own := ev.Start.Zone
	if allDay {
		own = "UTC"
	}
	out := map[string]any{
		"id": ev.ID, "subject": ev.Title, "changeKey": "ck-" + ev.Version, "isAllDay": allDay, "isCancelled": false, "showAs": "busy",
		"start": dtz(ev.Start, zone, allDay), "end": dtz(ev.End, zone, allDay),
		"createdDateTime": "2026-01-01T00:00:00Z", "lastModifiedDateTime": "2026-01-02T00:00:00Z",
		"body":                  map[string]any{"contentType": "text", "content": ev.Description},
		"location":              map[string]any{"displayName": ev.Location},
		"organizer":             map[string]any{"emailAddress": map[string]any{"name": ev.Organizer.Name, "address": ev.Organizer.Email}},
		"originalStartTimeZone": firstNonEmpty(windowsName[own], own),
		"type":                  "singleInstance",
	}
	if ev.SeriesID != "" && ev.ID != ev.SeriesID {
		out["type"], out["seriesMasterId"] = "occurrence", ev.SeriesID
		if ev.OriginalStart != nil {
			out["originalStart"] = ev.OriginalStart.DateTime.UTC().Format(time.RFC3339)
		}
	} else if ev.Recurrence != nil {
		out["type"], out["recurrence"] = "seriesMaster", patterned(ev.Recurrence, ev.Start)
	}
	var attendees []map[string]any
	for _, a := range ev.Attendees {
		typ := "required"
		if a.Optional {
			typ = "optional"
		}
		attendees = append(attendees, map[string]any{"type": typ, "status": map[string]any{"response": response(a.Response)},
			"emailAddress": map[string]any{"name": a.Name, "address": a.Email}})
	}
	if attendees != nil {
		out["attendees"] = attendees
		out["responseStatus"] = map[string]any{"response": response(ev.SelfResponse)}
	} else {
		out["responseStatus"] = map[string]any{"response": "organizer"}
	}
	if ev.ReminderMinutes != nil {
		out["isReminderOn"], out["reminderMinutesBeforeStart"] = true, *ev.ReminderMinutes
	} else {
		out["isReminderOn"] = false
	}
	if ev.Video {
		out["isOnlineMeeting"], out["onlineMeeting"] = true, map[string]any{"joinUrl": ev.VideoURL}
	}
	return out
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func parseWindow(q url.Values) (time.Time, time.Time, error) {
	from, err1 := time.Parse(time.RFC3339Nano, q.Get("startDateTime"))
	to, err2 := time.Parse(time.RFC3339Nano, q.Get("endDateTime"))
	if err1 != nil || err2 != nil {
		return from, to, fmt.Errorf("%w: bad window", provider.ErrInvalidQuery)
	}
	return from, to, nil
}

func (s *stub) calendarView(w http.ResponseWriter, r *http.Request) {
	s.calendarViewCalls++
	if s.denied(w) {
		return
	}
	q := r.URL.Query()
	if s.rawView != nil {
		_ = json.NewEncoder(w).Encode(map[string]any{"value": s.rawView})
		return
	}
	from, to, err := parseWindow(q)
	if err != nil {
		s.fail(w, err)
		return
	}
	limit, _ := strconv.Atoi(q.Get("$top"))
	token := q.Get("$skiptoken")
	page, err := s.p.ListEvents(r.Context(), provider.ListEventsQuery{CalendarIDs: []string{r.PathValue("cal")}, Start: from, End: to, Limit: limit, PageToken: token})
	if err != nil {
		s.fail(w, err)
		return
	}
	zone := zoneFromPrefer(r.Header.Get("Prefer"))
	value := make([]map[string]any, 0, len(page.Events))
	for _, ev := range page.Events {
		value = append(value, gEvent(ev, zone))
	}
	out := map[string]any{"value": value}
	if page.NextPageToken != "" {
		nq := url.Values{"$skiptoken": {page.NextPageToken}, "$top": {strconv.Itoa(limit)}, "startDateTime": {q.Get("startDateTime")}}
		out["@odata.nextLink"] = "https://elsewhere.invalid" + r.URL.Path + "?" + nq.Encode()
	}
	_ = json.NewEncoder(w).Encode(out)
}

func (s *stub) event(w http.ResponseWriter, r *http.Request) {
	if s.denied(w) {
		return
	}
	ev, err := s.p.GetEvent(r.Context(), provider.EventRef{CalendarID: r.PathValue("cal"), EventID: r.PathValue("id")})
	if err != nil {
		s.fail(w, err)
		return
	}
	_ = json.NewEncoder(w).Encode(gEvent(ev, zoneFromPrefer(r.Header.Get("Prefer"))))
}

func (s *stub) getSchedule(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Schedules []string `json:"schedules"`
		StartTime struct {
			DateTime string `json:"dateTime"`
			TimeZone string `json:"timeZone"`
		} `json:"startTime"`
		EndTime struct {
			DateTime string `json:"dateTime"`
		} `json:"endTime"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	if body.StartTime.TimeZone != "UTC" {
		s.t.Errorf("getSchedule must ask in UTC, got %q", body.StartTime.TimeZone)
	}
	from, _ := time.Parse("2006-01-02T15:04:05", body.StartTime.DateTime)
	to, _ := time.Parse("2006-01-02T15:04:05", body.EndTime.DateTime)
	var value []map[string]any
	for _, who := range body.Schedules {
		res, err := s.p.FreeBusy(r.Context(), provider.FreeBusyQuery{Emails: []string{who}, Start: from, End: to})
		if err != nil {
			s.fail(w, err)
			return
		}
		if reason, bad := res.Unavailable[who]; bad {
			value = append(value, map[string]any{"scheduleId": who, "error": map[string]any{"responseCode": "ErrorMailRecipientNotFound", "message": reason}})
			continue
		}
		items := []map[string]any{}
		for _, iv := range res.Busy[who] {
			items = append(items, map[string]any{
				"status": "busy",
				"start":  map[string]any{"dateTime": iv.Start.UTC().Format(graphLayout), "timeZone": "UTC"},
				"end":    map[string]any{"dateTime": iv.End.UTC().Format(graphLayout), "timeZone": "UTC"},
			})
		}
		// A free slot Graph lists must not count as busy.
		items = append(items, map[string]any{
			"status": "free",
			"start":  map[string]any{"dateTime": from.Format(graphLayout), "timeZone": "UTC"},
			"end":    map[string]any{"dateTime": from.Add(time.Minute).Format(graphLayout), "timeZone": "UTC"},
		})
		value = append(value, map[string]any{"scheduleId": who, "scheduleItems": items})
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"value": value})
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

func factory(personal bool) providertest.Factory {
	return func(t *testing.T) (provider.Provider, providertest.Seeder) {
		s := newStub(t)
		caps := provider.Capabilities{FreeBusyOthers: !personal, Video: !personal, Recurrence: true}
		s.p.SetCapabilities(caps)
		return s.client(personal), seeder{s.p}
	}
}

func TestConformanceReadWorkOrSchool(t *testing.T) { providertest.RunRead(t, factory(false)) }

func TestConformanceReadPersonal(t *testing.T) { providertest.RunRead(t, factory(true)) }
