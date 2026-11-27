// Package graph implements the read side of calendar provider.Provider on
// Microsoft Graph (Exchange Online and Outlook.com calendars), over
// internal/graphapi: calendarView for windows, getSchedule for other people's
// availability on work or school accounts, and recurrence read into the neutral
// subset or an opaque native rule.
//
// Writes arrive in a later phase; until then the write methods refuse.
//
// Everything here is built against documented Graph behaviour and synthetic
// fixtures. The rows of docs/microsoft-graph-spike.md marked UNVERIFIED list
// what is assumed, and the whole Microsoft path stays behind
// email.microsoft.enabled=false.
package graph

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/change-fabric/moose/internal/calendar/provider"
	"github.com/change-fabric/moose/internal/graphapi"
	"github.com/change-fabric/moose/internal/htmltext"
)

const (
	// maxFetch is the calendarView page size ceiling moose asks for.
	maxFetch = 250
	// scanPage is the server page size while a text filter is applied here.
	scanPage = 50
	// maxScanPages bounds how many server pages one filtered page reads.
	maxScanPages = 10
	// maxBusyPages bounds the pages read to compute free/busy of a calendar.
	maxBusyPages = 10
	// maxSchedules is Graph's cap on addresses in one getSchedule call.
	maxSchedules = 20
	// maxMasterLookups bounds the extra reads that fetch a series' recurrence.
	maxMasterLookups = 25
)

const eventSelect = "id,subject,body,start,end,location,isAllDay,isCancelled,organizer,attendees," +
	"isOnlineMeeting,onlineMeeting,onlineMeetingUrl,isReminderOn,reminderMinutesBeforeStart,type,seriesMasterId," +
	"changeKey,createdDateTime,lastModifiedDateTime,responseStatus,originalStart,originalStartTimeZone,recurrence"

const busySelect = "id,start,end,isAllDay,isCancelled,showAs,responseStatus"

// Options describe the account a Client is bound to.
type Options struct {
	// Self is the account's address, used to tell the account's own attendee
	// entry and its owned calendars.
	Self string
	// Personal marks an Outlook.com account: other people's availability and
	// Teams meetings are not available there.
	Personal bool
}

// Client is one connected Microsoft account's calendar.
type Client struct {
	g    *graphapi.Client
	opts Options

	mu   sync.Mutex
	cals []provider.Calendar
}

var _ provider.Provider = (*Client)(nil)

// New builds a Client over a Graph HTTP client (graphapi.New for production, or
// one with BaseURL pointed at an httptest server).
func New(g *graphapi.Client, opts Options) *Client { return &Client{g: g, opts: opts} }

// Capabilities: a personal account cannot see other people's free/busy or make
// Teams meetings; both account types accept the neutral recurrence subset.
func (c *Client) Capabilities() provider.Capabilities {
	return provider.Capabilities{FreeBusyOthers: !c.opts.Personal, Video: !c.opts.Personal, Recurrence: true}
}

// mapErr marks a 403 as a missing grant: the app was refused for this
// resource, which on the calendar endpoints means the calendar consent is
// absent or was withdrawn. Everything else keeps graphapi's mapping.
func mapErr(err error) error {
	var ge *graphapi.Error
	if errors.As(err, &ge) && ge.Status == http.StatusForbidden && !errors.Is(err, provider.ErrAuth) {
		return fmt.Errorf("%w: %v", provider.ErrAuth, err)
	}
	return err
}

func (c *Client) get(ctx context.Context, path string, q url.Values, prefer string, out any) error {
	var h http.Header
	if prefer != "" {
		h = http.Header{"Prefer": {prefer}}
	}
	return mapErr(c.g.GetJSON(ctx, path, q, h, out))
}

// ---- calendars ----

type calendarRes struct {
	ID                string `json:"id"`
	Name              string `json:"name"`
	IsDefaultCalendar bool   `json:"isDefaultCalendar"`
	CanEdit           bool   `json:"canEdit"`
	Owner             *struct {
		Address string `json:"address"`
	} `json:"owner"`
}

// ListCalendars returns the account's calendars, the default one first.
func (c *Client) ListCalendars(ctx context.Context) ([]provider.Calendar, error) {
	var out []provider.Calendar
	q := url.Values{"$select": {"id,name,isDefaultCalendar,canEdit,owner"}, "$top": {"50"}}
	for range 10 {
		var res struct {
			Value []calendarRes `json:"value"`
			Next  string        `json:"@odata.nextLink"`
		}
		if err := c.get(ctx, "/me/calendars", q, "", &res); err != nil {
			return nil, err
		}
		for _, r := range res.Value {
			role := provider.RoleReader
			switch {
			case r.IsDefaultCalendar || (r.Owner != nil && c.opts.Self != "" && strings.EqualFold(r.Owner.Address, c.opts.Self)):
				role = provider.RoleOwner
			case r.CanEdit:
				role = provider.RoleWriter
			}
			out = append(out, provider.Calendar{ID: r.ID, Name: r.Name, Primary: r.IsDefaultCalendar, Role: role})
		}
		if res.Next == "" {
			break
		}
		paging, err := pagingParams(res.Next)
		if err != nil {
			return nil, err
		}
		for k, v := range paging {
			q[k] = v
		}
	}
	for i, cal := range out {
		if cal.Primary && i > 0 {
			copy(out[1:i+1], out[:i])
			out[0] = cal
			break
		}
	}
	c.mu.Lock()
	c.cals = append([]provider.Calendar(nil), out...)
	c.mu.Unlock()
	return out, nil
}

func (c *Client) primaryID(ctx context.Context) (string, error) {
	c.mu.Lock()
	cached := c.cals
	c.mu.Unlock()
	if cached == nil {
		var err error
		if cached, err = c.ListCalendars(ctx); err != nil {
			return "", err
		}
	}
	for _, cal := range cached {
		if cal.Primary {
			return cal.ID, nil
		}
	}
	return "", fmt.Errorf("%w: the account has no default calendar", provider.ErrNotFound)
}

// ---- events ----

type dateTimeZone struct {
	DateTime string `json:"dateTime"`
	TimeZone string `json:"timeZone"`
}

type emailAddr struct {
	Name    string `json:"name"`
	Address string `json:"address"`
}

type eventRes struct {
	ID          string `json:"id"`
	Subject     string `json:"subject"`
	ChangeKey   string `json:"changeKey"`
	Created     string `json:"createdDateTime"`
	Modified    string `json:"lastModifiedDateTime"`
	IsAllDay    bool   `json:"isAllDay"`
	IsCancelled bool   `json:"isCancelled"`
	Type        string `json:"type"`
	SeriesID    string `json:"seriesMasterId"`
	Body        *struct {
		ContentType string `json:"contentType"`
		Content     string `json:"content"`
	} `json:"body"`
	Start    dateTimeZone `json:"start"`
	End      dateTimeZone `json:"end"`
	Location *struct {
		DisplayName string `json:"displayName"`
	} `json:"location"`
	Organizer *struct {
		EmailAddress emailAddr `json:"emailAddress"`
	} `json:"organizer"`
	Attendees []struct {
		Type   string `json:"type"`
		Status struct {
			Response string `json:"response"`
		} `json:"status"`
		EmailAddress emailAddr `json:"emailAddress"`
	} `json:"attendees"`
	IsOnlineMeeting bool `json:"isOnlineMeeting"`
	OnlineMeeting   *struct {
		JoinURL string `json:"joinUrl"`
	} `json:"onlineMeeting"`
	OnlineMeetingURL string `json:"onlineMeetingUrl"`
	IsReminderOn     bool   `json:"isReminderOn"`
	ReminderMinutes  *int   `json:"reminderMinutesBeforeStart"`
	ShowAs           string `json:"showAs"`
	ResponseStatus   *struct {
		Response string `json:"response"`
	} `json:"responseStatus"`
	OriginalStart         string          `json:"originalStart"`
	OriginalStartTimeZone string          `json:"originalStartTimeZone"`
	Recurrence            json.RawMessage `json:"recurrence"`
}

// parseGraphTime reads Graph's zone-less dateTime ("2026-03-04T09:00:00.0000000")
// in the zone it names. Graph names the zone either as an IANA name (the value
// moose asks for in the Prefer header) or "UTC".
func parseGraphTime(v dateTimeZone) (time.Time, string, error) {
	zone := v.TimeZone
	if zone == "" {
		zone = "UTC"
	}
	loc, err := provider.LoadZone(zone)
	if err != nil {
		if win, ok := windowsZones[zone]; ok {
			zone = win
			loc, err = provider.LoadZone(zone)
		}
		if err != nil {
			zone, loc = "UTC", time.UTC
		}
	}
	s := v.DateTime
	var t time.Time
	for _, layout := range []string{"2006-01-02T15:04:05.9999999", "2006-01-02T15:04:05", time.RFC3339Nano} {
		if t, err = time.ParseInLocation(layout, s, loc); err == nil {
			return t, zone, nil
		}
	}
	return time.Time{}, "", fmt.Errorf("graph calendar: bad time %q", s)
}

// eventZone is the zone an event is anchored to when it is shown without a
// requested zone: the zone it was created in.
func eventZone(name string) string {
	if _, err := provider.LoadZone(name); err == nil {
		return name
	}
	if z, ok := windowsZones[name]; ok {
		return z
	}
	return "UTC"
}

var graphDays = map[string]string{
	"monday": "MO", "tuesday": "TU", "wednesday": "WE", "thursday": "TH", "friday": "FR", "saturday": "SA", "sunday": "SU",
}

func attendeeResponse(r string) string {
	switch r {
	case "accepted", "organizer":
		return provider.ResponseAccepted
	case "tentativelyAccepted":
		return provider.ResponseTentative
	case "declined":
		return provider.ResponseDeclined
	}
	return provider.ResponseNone
}

func (c *Client) toEvent(calendarID string, r eventRes, rec *provider.Recurrence, ownZone bool) (provider.Event, error) {
	st, stZone, err := parseGraphTime(r.Start)
	if err != nil {
		return provider.Event{}, err
	}
	en, _, err := parseGraphTime(r.End)
	if err != nil {
		return provider.Event{}, err
	}
	ev := provider.Event{
		ID: r.ID, CalendarID: calendarID, Title: r.Subject, Version: r.ChangeKey, Status: provider.StatusConfirmed,
	}
	ev.Created, _ = time.Parse(time.RFC3339, r.Created)
	ev.Updated, _ = time.Parse(time.RFC3339, r.Modified)
	if r.IsCancelled {
		ev.Status = provider.StatusCancelled
	}
	if r.Location != nil {
		ev.Location = r.Location.DisplayName
	}
	if r.Body != nil && r.Body.Content != "" {
		if strings.EqualFold(r.Body.ContentType, "html") {
			ev.Description = htmltext.Convert(r.Body.Content)
		} else {
			ev.Description = strings.TrimSpace(r.Body.Content)
		}
	}
	if r.IsAllDay {
		ev.Start = provider.EventTime{Date: st.Format(provider.DateLayout)}
		ev.End = provider.EventTime{Date: en.Format(provider.DateLayout)}
	} else {
		zone := stZone
		if ownZone {
			zone = eventZone(r.OriginalStartTimeZone)
		}
		loc, _ := provider.LoadZone(zone)
		ev.Start = provider.EventTime{DateTime: st.In(loc), Zone: zone}
		ev.End = provider.EventTime{DateTime: en.In(loc), Zone: zone}
	}
	if r.Organizer != nil {
		ev.Organizer = provider.Attendee{Email: r.Organizer.EmailAddress.Address, Name: r.Organizer.EmailAddress.Name,
			Self: c.isSelf(r.Organizer.EmailAddress.Address)}
	}
	for _, a := range r.Attendees {
		at := provider.Attendee{
			Email: a.EmailAddress.Address, Name: a.EmailAddress.Name, Response: attendeeResponse(a.Status.Response),
			Optional: a.Type == "optional", Self: c.isSelf(a.EmailAddress.Address),
		}
		ev.Attendees = append(ev.Attendees, at)
	}
	if r.ResponseStatus != nil {
		ev.SelfResponse = attendeeResponse(r.ResponseStatus.Response)
		if r.ResponseStatus.Response == "none" || r.ResponseStatus.Response == "notResponded" {
			ev.SelfResponse = provider.ResponseNone
		}
	}
	if len(ev.Attendees) == 0 {
		// An event with nobody else on it has no self response to report.
		ev.SelfResponse = ""
	}
	if r.IsOnlineMeeting || r.OnlineMeeting != nil {
		ev.Video = true
		if r.OnlineMeeting != nil {
			ev.VideoURL = r.OnlineMeeting.JoinURL
		}
	}
	if r.IsReminderOn && r.ReminderMinutes != nil {
		ev.ReminderMinutes = r.ReminderMinutes
	}
	switch {
	case r.SeriesID != "":
		ev.SeriesID = r.SeriesID
	case r.Type == "seriesMaster":
		ev.SeriesID = r.ID
	}
	if r.OriginalStart != "" && !r.IsAllDay {
		if t, err := time.Parse(time.RFC3339, r.OriginalStart); err == nil {
			loc, _ := provider.LoadZone(ev.Start.Zone)
			ev.OriginalStart = &provider.EventTime{DateTime: t.In(loc), Zone: ev.Start.Zone}
		}
	} else if r.OriginalStart != "" {
		if t, err := time.Parse(time.RFC3339, r.OriginalStart); err == nil {
			ev.OriginalStart = &provider.EventTime{Date: t.Format(provider.DateLayout)}
		}
	}
	switch {
	case len(r.Recurrence) > 0 && string(r.Recurrence) != "null":
		x := recurrenceOf(r.Recurrence, ev.Start)
		ev.Recurrence = &x
	case rec != nil:
		cp := *rec
		ev.Recurrence = &cp
	}
	return ev, nil
}

func (c *Client) isSelf(addr string) bool {
	return c.opts.Self != "" && strings.EqualFold(strings.TrimSpace(addr), strings.TrimSpace(c.opts.Self))
}

// zonePrefer is the Prefer value that renders event times in zone, UTC when the
// caller asked for none (each event is then shown in its own zone afterwards).
func zonePrefer(zone string) string {
	if zone == "" {
		zone = "UTC"
	}
	return `outlook.timezone="` + zone + `", outlook.body-content-type="text"`
}

// ListEvents returns events in the window, occurrences expanded, ordered by
// start. With one calendar the cursor is Graph's own paging token; with several
// the page is cut from the merged union (provider.ListMerged).
//
// Graph's calendarView takes no free-text search, so Text is matched here over
// title, description, location, organizer and attendees. A filtered page reads
// whole server pages until it has limit matches, so it can carry up to one
// server page more than limit and, when the scan cap is hit first, fewer.
func (c *Client) ListEvents(ctx context.Context, q provider.ListEventsQuery) (provider.EventPage, error) {
	if err := provider.ValidateWindow(q.Start, q.End); err != nil {
		return provider.EventPage{}, err
	}
	if q.Zone != "" {
		if _, err := provider.LoadZone(q.Zone); err != nil {
			return provider.EventPage{}, fmt.Errorf("%w: %v", provider.ErrInvalidQuery, err)
		}
	}
	ids := q.CalendarIDs
	if len(ids) == 0 {
		id, err := c.primaryID(ctx)
		if err != nil {
			return provider.EventPage{}, err
		}
		ids = []string{id}
	}
	limit := q.Limit
	if limit <= 0 {
		limit = provider.DefaultPageSize
	}
	limit = min(limit, maxFetch)

	var page provider.EventPage
	var err error
	if len(ids) == 1 {
		page, err = c.listOne(ctx, ids[0], q, limit, q.PageToken)
	} else {
		page, err = provider.ListMerged(ctx, ids, limit, q.PageToken, func(ctx context.Context, id string, n int) ([]provider.Event, error) {
			var all []provider.Event
			token := ""
			for len(all) < n {
				p, err := c.listOne(ctx, id, q, min(n-len(all), maxFetch), token)
				if err != nil {
					return nil, err
				}
				all = append(all, p.Events...)
				if token = p.NextPageToken; token == "" {
					break
				}
			}
			return all, nil
		})
	}
	if err != nil {
		return provider.EventPage{}, err
	}
	if q.Zone != "" {
		for i := range page.Events {
			page.Events[i] = provider.InZone(page.Events[i], q.Zone)
		}
	}
	return page, nil
}

func windowQuery(start, end time.Time) url.Values {
	return url.Values{
		"startDateTime": {start.UTC().Format("2006-01-02T15:04:05.0000000") + "Z"},
		"endDateTime":   {end.UTC().Format("2006-01-02T15:04:05.0000000") + "Z"},
	}
}

func (c *Client) listOne(ctx context.Context, calendarID string, q provider.ListEventsQuery, limit int, token string) (provider.EventPage, error) {
	path := "/me/calendars/" + graphapi.Segment(calendarID) + "/calendarView"
	filter := strings.ToLower(strings.TrimSpace(q.Text))
	pageSize := limit
	if filter != "" {
		pageSize = scanPage
	}
	paging := url.Values{}
	if token != "" {
		var err error
		if paging, err = decodeCursor(token); err != nil {
			return provider.EventPage{}, err
		}
	}
	masters := map[string]*provider.Recurrence{}
	lookups := 0
	out := provider.EventPage{Events: []provider.Event{}}
	for scanned := 0; scanned < maxScanPages; scanned++ {
		v := windowQuery(q.Start, q.End)
		v.Set("$top", fmt.Sprint(pageSize))
		v.Set("$orderby", "start/dateTime")
		v.Set("$select", eventSelect)
		for k, vs := range paging {
			v[k] = vs
		}
		var res struct {
			Value []eventRes `json:"value"`
			Next  string     `json:"@odata.nextLink"`
		}
		if err := c.get(ctx, path, v, zonePrefer(q.Zone), &res); err != nil {
			return provider.EventPage{}, err
		}
		for _, r := range res.Value {
			if r.IsCancelled {
				continue
			}
			var rec *provider.Recurrence
			if r.SeriesID != "" && len(r.Recurrence) == 0 {
				if m, seen := masters[r.SeriesID]; seen {
					rec = m
				} else if lookups < maxMasterLookups {
					lookups++
					rec = c.seriesRecurrence(ctx, calendarID, r.SeriesID)
					masters[r.SeriesID] = rec
				} else {
					rec = &provider.Recurrence{Native: "unavailable"}
				}
			}
			ev, err := c.toEvent(calendarID, r, rec, q.Zone == "")
			if err != nil {
				continue
			}
			if filter == "" || matches(ev, filter) {
				out.Events = append(out.Events, ev)
			}
		}
		out.NextPageToken = ""
		if res.Next != "" {
			p, err := pagingParams(res.Next)
			if err != nil {
				return provider.EventPage{}, err
			}
			paging, out.NextPageToken = p, p.Encode()
		}
		if out.NextPageToken == "" || len(out.Events) >= limit || filter == "" {
			break
		}
	}
	return out, nil
}

func matches(ev provider.Event, lowerQuery string) bool {
	hay := []string{ev.Title, ev.Description, ev.Location, ev.Organizer.Email, ev.Organizer.Name}
	for _, a := range ev.Attendees {
		hay = append(hay, a.Email, a.Name)
	}
	for _, h := range hay {
		if strings.Contains(strings.ToLower(h), lowerQuery) {
			return true
		}
	}
	return false
}

// seriesRecurrence reads a series master to learn the rule its occurrences
// follow. A failure is not an error for the listing: the rule is reported opaque.
func (c *Client) seriesRecurrence(ctx context.Context, calendarID, masterID string) *provider.Recurrence {
	var m eventRes
	q := url.Values{"$select": {"id,start,recurrence"}}
	if err := c.get(ctx, "/me/calendars/"+graphapi.Segment(calendarID)+"/events/"+graphapi.Segment(masterID), q, zonePrefer(""), &m); err != nil ||
		len(m.Recurrence) == 0 || string(m.Recurrence) == "null" {
		return &provider.Recurrence{Native: "unavailable"}
	}
	st, zone, err := parseGraphTime(m.Start)
	if err != nil {
		return &provider.Recurrence{Native: "unavailable"}
	}
	r := recurrenceOf(m.Recurrence, provider.EventTime{DateTime: st, Zone: zone})
	return &r
}

// GetEvent returns one event or occurrence, in the zone it was created in.
func (c *Client) GetEvent(ctx context.Context, ref provider.EventRef) (provider.Event, error) {
	if ref.CalendarID == "" || ref.EventID == "" {
		return provider.Event{}, fmt.Errorf("%w: a calendar id and an event id are required", provider.ErrNotFound)
	}
	var r eventRes
	q := url.Values{"$select": {eventSelect}}
	if err := c.get(ctx, "/me/calendars/"+graphapi.Segment(ref.CalendarID)+"/events/"+graphapi.Segment(ref.EventID), q, zonePrefer(""), &r); err != nil {
		return provider.Event{}, err
	}
	if r.IsCancelled {
		return provider.Event{}, fmt.Errorf("%w: the event was cancelled", provider.ErrNotFound)
	}
	var rec *provider.Recurrence
	if r.SeriesID != "" && len(r.Recurrence) == 0 {
		rec = c.seriesRecurrence(ctx, ref.CalendarID, r.SeriesID)
	}
	return c.toEvent(ref.CalendarID, r, rec, true)
}

// ---- free/busy ----

// busyStatus reports whether a showAs or schedule status blocks the time.
func busyStatus(s string) bool {
	switch s {
	case "busy", "tentative", "oof":
		return true
	}
	return false
}

// FreeBusy computes busy time for the account's own calendars from calendarView
// (events marked free, cancelled, or declined by the account do not count), and
// for other people from getSchedule on work or school accounts. A personal
// account asked about others returns ErrUnsupportedHere. A person whose
// schedule Graph could not return is in Unavailable, never free.
func (c *Client) FreeBusy(ctx context.Context, q provider.FreeBusyQuery) (provider.FreeBusyResult, error) {
	if err := provider.ValidateWindow(q.Start, q.End); err != nil {
		return provider.FreeBusyResult{}, err
	}
	if len(q.Emails) > 0 && c.opts.Personal {
		return provider.FreeBusyResult{}, fmt.Errorf("%w: free/busy of other people is not available on a personal Microsoft account", provider.ErrUnsupportedHere)
	}
	if len(q.Emails) > maxSchedules {
		return provider.FreeBusyResult{}, fmt.Errorf("%w: at most %d people per call", provider.ErrInvalidQuery, maxSchedules)
	}
	ids := q.CalendarIDs
	if len(ids) == 0 {
		id, err := c.primaryID(ctx)
		if err != nil {
			return provider.FreeBusyResult{}, err
		}
		ids = []string{id}
	}
	out := provider.FreeBusyResult{Busy: map[string][]provider.Interval{}}
	for _, id := range ids {
		ivs, err := c.ownBusy(ctx, id, q.Start, q.End)
		if err != nil {
			return provider.FreeBusyResult{}, err
		}
		out.Busy[id] = provider.MergeIntervals(ivs)
	}
	if len(q.Emails) == 0 {
		return out, nil
	}
	body := map[string]any{
		"schedules":                q.Emails,
		"startTime":                map[string]string{"dateTime": q.Start.UTC().Format("2006-01-02T15:04:05"), "timeZone": "UTC"},
		"endTime":                  map[string]string{"dateTime": q.End.UTC().Format("2006-01-02T15:04:05"), "timeZone": "UTC"},
		"availabilityViewInterval": 30,
	}
	raw, _ := json.Marshal(body)
	resp, err := c.g.Do(ctx, graphapi.Request{
		Method: http.MethodPost, Path: "/me/calendar/getSchedule", Body: strings.NewReader(string(raw)),
		ContentType: "application/json", Header: http.Header{"Prefer": {`outlook.timezone="UTC"`}},
	})
	if err != nil {
		return provider.FreeBusyResult{}, mapErr(err)
	}
	defer resp.Body.Close()
	var sched struct {
		Value []struct {
			ScheduleID    string `json:"scheduleId"`
			ScheduleItems []struct {
				Status string       `json:"status"`
				Start  dateTimeZone `json:"start"`
				End    dateTimeZone `json:"end"`
			} `json:"scheduleItems"`
			Error *struct {
				ResponseCode string `json:"responseCode"`
				Message      string `json:"message"`
			} `json:"error"`
		} `json:"value"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&sched); err != nil {
		return provider.FreeBusyResult{}, fmt.Errorf("graph calendar: decode getSchedule: %w", err)
	}
	answered := map[string]bool{}
	for _, s := range sched.Value {
		key := matchEmail(q.Emails, s.ScheduleID)
		if key == "" {
			continue
		}
		answered[key] = true
		if s.Error != nil {
			markUnavailable(&out, key, orDefault(s.Error.ResponseCode, "error"))
			continue
		}
		ivs := []provider.Interval{}
		for _, it := range s.ScheduleItems {
			if !busyStatus(it.Status) {
				continue
			}
			st, _, err1 := parseGraphTime(it.Start)
			en, _, err2 := parseGraphTime(it.End)
			if err1 != nil || err2 != nil {
				continue
			}
			ivs = append(ivs, provider.Interval{Start: st, End: en})
		}
		out.Busy[key] = provider.MergeIntervals(ivs)
		if out.Busy[key] == nil {
			out.Busy[key] = []provider.Interval{}
		}
	}
	for _, e := range q.Emails {
		if !answered[e] {
			markUnavailable(&out, e, "not_returned")
		}
	}
	return out, nil
}

func orDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

func matchEmail(requested []string, got string) string {
	for _, r := range requested {
		if strings.EqualFold(strings.TrimSpace(r), strings.TrimSpace(got)) {
			return r
		}
	}
	return ""
}

func (c *Client) ownBusy(ctx context.Context, calendarID string, start, end time.Time) ([]provider.Interval, error) {
	path := "/me/calendars/" + graphapi.Segment(calendarID) + "/calendarView"
	paging := url.Values{}
	var out []provider.Interval
	for range maxBusyPages {
		v := windowQuery(start, end)
		v.Set("$top", fmt.Sprint(maxFetch))
		v.Set("$select", busySelect)
		for k, vs := range paging {
			v[k] = vs
		}
		var res struct {
			Value []eventRes `json:"value"`
			Next  string     `json:"@odata.nextLink"`
		}
		if err := c.get(ctx, path, v, `outlook.timezone="UTC"`, &res); err != nil {
			return nil, err
		}
		for _, r := range res.Value {
			if r.IsCancelled || !busyStatus(r.ShowAs) || (r.ResponseStatus != nil && r.ResponseStatus.Response == "declined") {
				continue
			}
			st, _, err1 := parseGraphTime(r.Start)
			en, _, err2 := parseGraphTime(r.End)
			if err1 != nil || err2 != nil {
				continue
			}
			out = append(out, provider.Interval{Start: st, End: en})
		}
		if res.Next == "" {
			return out, nil
		}
		p, err := pagingParams(res.Next)
		if err != nil {
			return nil, err
		}
		paging = p
	}
	return nil, fmt.Errorf("%w: too many events in the window to compute free/busy; narrow it", provider.ErrInvalidQuery)
}

// ---- paging ----

func pagingParams(nextLink string) (url.Values, error) {
	u, err := url.Parse(nextLink)
	if err != nil {
		return nil, fmt.Errorf("graph calendar: bad nextLink: %w", err)
	}
	return keepPaging(u.Query())
}

func keepPaging(in url.Values) (url.Values, error) {
	out := url.Values{}
	for k, v := range in {
		if lk := strings.ToLower(k); lk == "$skip" || lk == "$skiptoken" {
			out[lk] = v
		}
	}
	if len(out) == 0 {
		return nil, errors.New("graph calendar: nextLink has no paging token")
	}
	return out, nil
}

func decodeCursor(cursor string) (url.Values, error) {
	v, err := url.ParseQuery(cursor)
	if err != nil {
		return nil, fmt.Errorf("%w: bad page token", provider.ErrInvalidQuery)
	}
	out, err := keepPaging(v)
	if err != nil || len(out) != len(v) {
		return nil, fmt.Errorf("%w: bad page token", provider.ErrInvalidQuery)
	}
	return out, nil
}

// markUnavailable records that a person's availability could not be read.
func markUnavailable(res *provider.FreeBusyResult, key, reason string) {
	if res.Unavailable == nil {
		res.Unavailable = map[string]string{}
	}
	res.Unavailable[key] = reason
}

// ---- writes (a later phase) ----

var errWritesLater = errors.New("graph calendar: writes are not implemented yet")

func (c *Client) CreateEvent(context.Context, string, provider.EventInput, provider.CreateOptions) (provider.Event, error) {
	return provider.Event{}, errWritesLater
}

func (c *Client) UpdateEvent(context.Context, provider.EventRef, provider.Scope, provider.EventPatch, string) (provider.Event, error) {
	return provider.Event{}, errWritesLater
}

func (c *Client) DeleteEvent(context.Context, provider.EventRef, provider.Scope, string) error {
	return errWritesLater
}

func (c *Client) Respond(context.Context, provider.EventRef, provider.Response, string) error {
	return errWritesLater
}
