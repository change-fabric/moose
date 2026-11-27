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
	// rsvpComments are the comments RSVP patches carried.
	rsvpComments []string
}

func newStub(t *testing.T) (*stub, *httptest.Server) {
	s := &stub{t: t, p: fake.New(providertest.SelfAddress), hits: map[string]int{}, zone: "Europe/Berlin"}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /calendar/v3/users/me/calendarList", s.calendarList)
	mux.HandleFunc("GET /calendar/v3/calendars/{cal}/events", s.events)
	mux.HandleFunc("GET /calendar/v3/calendars/{cal}/events/{id}", s.event)
	mux.HandleFunc("POST /calendar/v3/freeBusy", s.freeBusy)
	mux.HandleFunc("POST /calendar/v3/calendars/{cal}/events", s.insert)
	mux.HandleFunc("PATCH /calendar/v3/calendars/{cal}/events/{id}", s.patch)
	mux.HandleFunc("DELETE /calendar/v3/calendars/{cal}/events/{id}", s.remove)
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
	case errors.Is(err, provider.ErrPreconditionFailed):
		code, reason = http.StatusPreconditionFailed, "conditionNotMet"
	case errors.Is(err, provider.ErrRejected), errors.Is(err, provider.ErrUnsupportedHere), errors.Is(err, provider.ErrInvalidTime), errors.Is(err, provider.ErrInvalidRecurrence):
		code, reason = http.StatusForbidden, "forbidden"
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
	if errors.Is(err, provider.ErrNotFound) {
		// A client-supplied event id is the create dedupe key to the fake.
		if found, ok, ferr := s.p.FindByDedupeKey(r.Context(), r.PathValue("cal"), r.PathValue("id")); ferr == nil && ok {
			ev, err = found, nil
		}
	}
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
func (s seeder) Notified(t *testing.T) []providertest.Notification {
	var out []providertest.Notification
	for _, n := range s.p.Notifications() {
		out = append(out, providertest.Notification{Kind: n.Kind, To: n.To})
	}
	return out
}

func newClient(srv *httptest.Server) *google.Client {
	c := google.New(nil)
	c.BaseURL = srv.URL + "/calendar/v3"
	return c
}

func TestConformanceWrite(t *testing.T) {
	providertest.RunWrite(t, func(t *testing.T) (provider.Provider, providertest.Seeder) {
		s, srv := newStub(t)
		return newClient(srv), seeder{s.p}
	})
}

func TestConformanceRead(t *testing.T) {
	providertest.RunRead(t, func(t *testing.T) (provider.Provider, providertest.Seeder) {
		s, srv := newStub(t)
		return newClient(srv), seeder{s.p}
	})
}

// ---- writes ----

type wireTime struct {
	Date     string `json:"date"`
	DateTime string `json:"dateTime"`
	TimeZone string `json:"timeZone"`
}

func (w wireTime) toTime() (provider.EventTime, error) {
	if w.Date != "" {
		return provider.EventTime{Date: w.Date}, nil
	}
	t, err := time.Parse(time.RFC3339, w.DateTime)
	if err != nil {
		return provider.EventTime{}, err
	}
	loc, err := time.LoadLocation(w.TimeZone)
	if err != nil {
		return provider.EventTime{}, err
	}
	return provider.EventTime{DateTime: t.In(loc), Zone: w.TimeZone}, nil
}

type wireAttendee struct {
	Email       string `json:"email"`
	DisplayName string `json:"displayName"`
	Optional    bool   `json:"optional"`
	Self        bool   `json:"self"`
	Status      string `json:"responseStatus"`
	Comment     string `json:"comment"`
}

func (a wireAttendee) toAttendee() provider.Attendee {
	return provider.Attendee{Email: a.Email, Name: a.DisplayName, Optional: a.Optional}
}

func trimTag(h string) string { return strings.Trim(strings.TrimSpace(h), `"`) }

// scopeOf picks what a Google write to id means: an instance id is one
// occurrence; a master or one-off is the whole event.
func (s *stub) scopeOf(r *http.Request, id string) (provider.EventRef, provider.Scope, error) {
	ref := provider.EventRef{CalendarID: r.PathValue("cal"), EventID: id}
	ev, err := s.p.GetEvent(r.Context(), ref)
	if err != nil {
		return ref, "", err
	}
	if ev.SeriesID != "" && ev.ID != ev.SeriesID {
		return ref, provider.ScopeInstance, nil
	}
	return ref, provider.ScopeSeries, nil
}

func (s *stub) insert(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ID          string         `json:"id"`
		Summary     string         `json:"summary"`
		Description string         `json:"description"`
		Location    string         `json:"location"`
		Start       wireTime       `json:"start"`
		End         wireTime       `json:"end"`
		Recurrence  []string       `json:"recurrence"`
		Attendees   []wireAttendee `json:"attendees"`
		Conference  map[string]any `json:"conferenceData"`
		Reminders   *struct {
			UseDefault bool `json:"useDefault"`
			Overrides  []struct {
				Minutes int `json:"minutes"`
			} `json:"overrides"`
		} `json:"reminders"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		s.fail(w, fmt.Errorf("%w: %v", provider.ErrInvalidQuery, err))
		return
	}
	if r.URL.Query().Get("sendUpdates") == "" {
		s.t.Errorf("insert must say sendUpdates")
	}
	if body.Conference != nil && r.URL.Query().Get("conferenceDataVersion") != "1" {
		s.t.Errorf("a conferenceData request needs conferenceDataVersion=1")
	}
	st, err1 := body.Start.toTime()
	en, err2 := body.End.toTime()
	if err1 != nil || err2 != nil {
		s.fail(w, fmt.Errorf("%w: bad time", provider.ErrInvalidQuery))
		return
	}
	in := provider.EventInput{Title: body.Summary, Description: body.Description, Location: body.Location, Start: st, End: en, Video: body.Conference != nil}
	if len(body.Recurrence) > 0 {
		loc := time.UTC
		if !st.IsAllDay() {
			loc = st.DateTime.Location()
		}
		rec := provider.ParseRRULE(body.Recurrence, loc)
		if rec.IsOpaque() {
			s.fail(w, fmt.Errorf("%w: rule %q", provider.ErrInvalidQuery, rec.Native))
			return
		}
		in.Recurrence = &rec
	}
	for _, a := range body.Attendees {
		in.Attendees = append(in.Attendees, a.toAttendee())
	}
	if body.Reminders != nil && len(body.Reminders.Overrides) > 0 {
		m := body.Reminders.Overrides[0].Minutes
		in.ReminderMinutes = &m
	}
	ev, err := s.p.CreateEvent(r.Context(), r.PathValue("cal"), in, provider.CreateOptions{DedupeKey: body.ID})
	if err != nil {
		s.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(gEvent(ev))
}

func (s *stub) patch(w http.ResponseWriter, r *http.Request) {
	raw := map[string]json.RawMessage{}
	if err := json.NewDecoder(r.Body).Decode(&raw); err != nil {
		s.fail(w, fmt.Errorf("%w: %v", provider.ErrInvalidQuery, err))
		return
	}
	ifVer := trimTag(r.Header.Get("If-Match"))
	ref, scope, err := s.scopeOf(r, r.PathValue("id"))
	if err != nil {
		s.fail(w, err)
		return
	}
	// An attendees-only body that changes just the account's own answer is an RSVP.
	if len(raw) == 1 && raw["attendees"] != nil {
		var list []wireAttendee
		_ = json.Unmarshal(raw["attendees"], &list)
		cur, _ := s.p.GetEvent(r.Context(), ref)
		if resp, comment, ok := rsvpOf(cur, list); ok {
			if cur.Version != ifVer {
				s.fail(w, fmt.Errorf("%w: have %s", provider.ErrPreconditionFailed, cur.Version))
				return
			}
			if err := s.p.Respond(r.Context(), ref, resp, comment); err != nil {
				s.fail(w, err)
				return
			}
			s.rsvpComments = append(s.rsvpComments, comment)
			now, _ := s.p.GetEvent(r.Context(), ref)
			_ = json.NewEncoder(w).Encode(gEvent(now))
			return
		}
	}
	var patch provider.EventPatch
	str := func(key string) *string {
		v, ok := raw[key]
		if !ok {
			return nil
		}
		var x string
		_ = json.Unmarshal(v, &x)
		return &x
	}
	patch.Title, patch.Description, patch.Location = str("summary"), str("description"), str("location")
	for key, dst := range map[string]**provider.EventTime{"start": &patch.Start, "end": &patch.End} {
		if v, ok := raw[key]; ok {
			var wt wireTime
			_ = json.Unmarshal(v, &wt)
			t, err := wt.toTime()
			if err != nil {
				s.fail(w, fmt.Errorf("%w: bad time", provider.ErrInvalidQuery))
				return
			}
			*dst = &t
		}
	}
	if v, ok := raw["recurrence"]; ok {
		var lines []string
		_ = json.Unmarshal(v, &lines)
		if len(lines) == 0 {
			patch.ClearRecurrence = true
		} else {
			rec := provider.ParseRRULE(lines, time.UTC)
			patch.Recurrence = &rec
		}
	}
	if v, ok := raw["attendees"]; ok {
		var list []wireAttendee
		_ = json.Unmarshal(v, &list)
		out := []provider.Attendee{}
		for _, a := range list {
			out = append(out, a.toAttendee())
		}
		patch.Attendees = &out
	}
	if v, ok := raw["conferenceData"]; ok {
		on := string(v) != "null"
		patch.Video = &on
	}
	if v, ok := raw["reminders"]; ok {
		var rm struct {
			Overrides []struct {
				Minutes int `json:"minutes"`
			} `json:"overrides"`
		}
		_ = json.Unmarshal(v, &rm)
		if len(rm.Overrides) > 0 {
			m := rm.Overrides[0].Minutes
			patch.ReminderMinutes = &m
		}
	}
	ev, err := s.p.UpdateEvent(r.Context(), ref, scope, patch, ifVer)
	if err != nil {
		s.fail(w, err)
		return
	}
	_ = json.NewEncoder(w).Encode(gEvent(ev))
}

// rsvpOf reports whether list differs from cur only in the account's own
// response, and what that response and comment are.
func rsvpOf(cur provider.Event, list []wireAttendee) (provider.Response, string, bool) {
	if len(list) != len(cur.Attendees) {
		return "", "", false
	}
	var resp provider.Response
	comment := ""
	for i, a := range list {
		if !strings.EqualFold(a.Email, cur.Attendees[i].Email) {
			return "", "", false
		}
		if a.Self {
			switch a.Status {
			case "accepted":
				resp = provider.RespondAccept
			case "tentative":
				resp = provider.RespondTentative
			case "declined":
				resp = provider.RespondDecline
			default:
				return "", "", false
			}
			comment = a.Comment
		}
	}
	return resp, comment, resp != ""
}

func (s *stub) remove(w http.ResponseWriter, r *http.Request) {
	ref, scope, err := s.scopeOf(r, r.PathValue("id"))
	if err != nil {
		s.fail(w, err)
		return
	}
	if err := s.p.DeleteEvent(r.Context(), ref, scope, trimTag(r.Header.Get("If-Match"))); err != nil {
		s.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
