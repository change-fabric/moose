// Package google implements the read side of calendar provider.Provider on the
// Google Calendar v3 REST API, over net/http with an oauth2 token source and a
// base-URL override for httptest. Events are read with singleEvents=true and
// orderBy=startTime, so recurring events arrive as their occurrences.
//
// Writes live in write.go: conditional on the event's etag (If-Match), with
// sendUpdates=all so attendees are notified, and a client-supplied event id as
// the create dedupe key.
//
// The whole calendar service stays behind email.calendar.enabled=false.
package google

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/oauth2"

	"github.com/change-fabric/moose/internal/calendar/provider"
	mail "github.com/change-fabric/moose/internal/email/provider"
)

// DefaultBaseURL is the Google Calendar v3 root.
const DefaultBaseURL = "https://www.googleapis.com/calendar/v3"

const (
	maxErrorBody = 64 << 10
	// maxPageResults is Google's own cap on maxResults.
	maxPageResults = 250
	// maxFreeBusyItems is Google's cap on items in one freeBusy request.
	maxFreeBusyItems = 50
	// maxMasterLookups bounds the extra reads that fetch a series' recurrence
	// rule for the occurrences on a page.
	maxMasterLookups = 25
)

// Client is one connected Google account's calendar.
type Client struct {
	// BaseURL is the API root without a trailing slash; tests point it at a stub.
	BaseURL string
	// HTTP performs the requests. New wires bearer auth into it.
	HTTP *http.Client

	mu   sync.Mutex
	cals []provider.Calendar // cached ListCalendars answer for this client's lifetime
}

var _ provider.Provider = (*Client)(nil)

// New returns a Client that authorizes every request from ts. A nil ts sends no
// Authorization header, which is for tests against a stub.
func New(ts oauth2.TokenSource) *Client {
	hc := &http.Client{Timeout: 60 * time.Second}
	if ts != nil {
		hc.Transport = &oauth2.Transport{Source: oauth2.ReuseTokenSource(nil, ts), Base: http.DefaultTransport}
	}
	return &Client{BaseURL: DefaultBaseURL, HTTP: hc}
}

// Capabilities: Google answers free/busy for other people when the calendar is
// shared with the account (or in the same Workspace), has Meet, and accepts the
// neutral recurrence subset as an RRULE.
func (c *Client) Capabilities() provider.Capabilities {
	return provider.Capabilities{FreeBusyOthers: true, Video: true, Recurrence: true}
}

// ---- HTTP ----

func (c *Client) do(ctx context.Context, method, path string, q url.Values, body any, out any) error {
	return c.doH(ctx, method, path, q, nil, body, out)
}

// doH is do with extra request headers (If-Match on conditional writes).
func (c *Client) doH(ctx context.Context, method, path string, q url.Values, hdr http.Header, body any, out any) error {
	u := strings.TrimRight(c.BaseURL, "/") + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	var rd io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("google calendar: %w", err)
		}
		rd = strings.NewReader(string(raw))
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rd)
	if err != nil {
		return fmt.Errorf("google calendar: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	for k, vs := range hdr {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	hc := c.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		return fmt.Errorf("google calendar: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return mapResponse(resp)
	}
	if out == nil {
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("google calendar: decode %s: %w", path, err)
	}
	return nil
}

type apiError struct {
	Error struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Errors  []struct {
			Reason string `json:"reason"`
		} `json:"errors"`
	} `json:"error"`
}

// mapResponse turns a non-2xx Google answer into an error wrapping a provider
// sentinel:
//
//	412                                        -> ErrPreconditionFailed
//	401, or 403 insufficientPermissions        -> ErrAuth
//	403/429 rate and quota reasons, 429        -> RateLimitError
//	404, 410                                   -> ErrNotFound
//	400                                        -> ErrInvalidQuery
//	any other 4xx                              -> ErrRejected
func mapResponse(resp *http.Response) error {
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
	var ae apiError
	_ = json.Unmarshal(raw, &ae)
	reason := ""
	if len(ae.Error.Errors) > 0 {
		reason = ae.Error.Errors[0].Reason
	}
	msg := ae.Error.Message
	if len(msg) > 300 {
		msg = msg[:300] + "..."
	}
	detail := fmt.Sprintf("google calendar %d %s: %s", resp.StatusCode, reason, msg)
	wrap := func(err error) error { return &statusError{Status: resp.StatusCode, Reason: reason, Err: err} }
	switch {
	case resp.StatusCode == http.StatusTooManyRequests, isRateReason(reason):
		return &mail.RateLimitError{RetryAfter: parseRetryAfter(resp.Header.Get("Retry-After")), Err: errors.New(detail)}
	case resp.StatusCode == http.StatusPreconditionFailed:
		return wrap(fmt.Errorf("%w: %s", provider.ErrPreconditionFailed, detail))
	case resp.StatusCode == http.StatusUnauthorized, reason == "insufficientPermissions", reason == "authError":
		return wrap(fmt.Errorf("%w: %s", provider.ErrAuth, detail))
	case resp.StatusCode == http.StatusNotFound, resp.StatusCode == http.StatusGone:
		return wrap(fmt.Errorf("%w: %s", provider.ErrNotFound, detail))
	case resp.StatusCode == http.StatusBadRequest:
		return wrap(fmt.Errorf("%w: %s", provider.ErrInvalidQuery, detail))
	case resp.StatusCode >= 400 && resp.StatusCode < 500:
		return wrap(fmt.Errorf("%w: %s", provider.ErrRejected, detail))
	}
	return wrap(fmt.Errorf("google calendar: %s", detail))
}

// statusError keeps the HTTP status and Google's first error reason next to the
// sentinel-wrapping error, so a create can tell a duplicate id (409) from any
// other refusal without parsing text.
type statusError struct {
	Status int
	Reason string
	Err    error
}

func (e *statusError) Error() string { return e.Err.Error() }
func (e *statusError) Unwrap() error { return e.Err }

func isRateReason(r string) bool {
	switch r {
	case "rateLimitExceeded", "userRateLimitExceeded", "quotaExceeded", "dailyLimitExceeded":
		return true
	}
	return false
}

func parseRetryAfter(v string) time.Duration {
	if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n > 0 {
		return time.Duration(n) * time.Second
	}
	return 0
}

func seg(s string) string { return url.PathEscape(s) }

// ---- calendars ----

type calendarListEntry struct {
	ID         string `json:"id"`
	Summary    string `json:"summary"`
	Primary    bool   `json:"primary"`
	AccessRole string `json:"accessRole"`
	TimeZone   string `json:"timeZone"`
	Deleted    bool   `json:"deleted"`
	// SummaryOverride is the account's own name for a calendar it subscribes to.
	SummaryOverride string `json:"summaryOverride"`
}

func roleOf(access string) string {
	switch access {
	case "owner":
		return provider.RoleOwner
	case "writer":
		return provider.RoleWriter
	case "reader":
		return provider.RoleReader
	}
	return provider.RoleFreeBusyOnly
}

// ListCalendars returns the account's calendar list, the primary first.
func (c *Client) ListCalendars(ctx context.Context) ([]provider.Calendar, error) {
	var out []provider.Calendar
	token := ""
	for range 20 {
		q := url.Values{"maxResults": {"250"}}
		if token != "" {
			q.Set("pageToken", token)
		}
		var page struct {
			Items         []calendarListEntry `json:"items"`
			NextPageToken string              `json:"nextPageToken"`
		}
		if err := c.do(ctx, http.MethodGet, "/users/me/calendarList", q, nil, &page); err != nil {
			return nil, err
		}
		for _, e := range page.Items {
			if e.Deleted {
				continue
			}
			name := e.SummaryOverride
			if name == "" {
				name = e.Summary
			}
			out = append(out, provider.Calendar{ID: e.ID, Name: name, Primary: e.Primary, Role: roleOf(e.AccessRole), TimeZone: e.TimeZone})
		}
		if token = page.NextPageToken; token == "" {
			break
		}
	}
	// The primary first, the rest in the order Google gave.
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

func (c *Client) calendars(ctx context.Context) ([]provider.Calendar, error) {
	c.mu.Lock()
	cached := c.cals
	c.mu.Unlock()
	if cached != nil {
		return cached, nil
	}
	return c.ListCalendars(ctx)
}

func (c *Client) primaryID(ctx context.Context) (string, error) {
	cals, err := c.calendars(ctx)
	if err != nil {
		return "", err
	}
	for _, cal := range cals {
		if cal.Primary {
			return cal.ID, nil
		}
	}
	return "primary", nil
}

// zoneOf is the calendar's default zone, "UTC" when unknown.
func (c *Client) zoneOf(ctx context.Context, calendarID string) string {
	cals, err := c.calendars(ctx)
	if err == nil {
		for _, cal := range cals {
			if cal.ID == calendarID && cal.TimeZone != "" {
				return cal.TimeZone
			}
		}
	}
	return "UTC"
}

// ---- events ----

type gTime struct {
	Date     string `json:"date,omitempty"`
	DateTime string `json:"dateTime,omitempty"`
	TimeZone string `json:"timeZone,omitempty"`
}

type gPerson struct {
	Email       string `json:"email"`
	DisplayName string `json:"displayName"`
	Self        bool   `json:"self"`
}

type gAttendee struct {
	gPerson
	ResponseStatus string `json:"responseStatus"`
	Optional       bool   `json:"optional"`
}

type gEvent struct {
	ID                string      `json:"id"`
	ETag              string      `json:"etag"`
	Status            string      `json:"status"`
	Summary           string      `json:"summary"`
	Description       string      `json:"description"`
	Location          string      `json:"location"`
	Created           string      `json:"created"`
	Updated           string      `json:"updated"`
	Start             gTime       `json:"start"`
	End               gTime       `json:"end"`
	OriginalStartTime *gTime      `json:"originalStartTime"`
	Recurrence        []string    `json:"recurrence"`
	RecurringEventID  string      `json:"recurringEventId"`
	Organizer         gPerson     `json:"organizer"`
	Attendees         []gAttendee `json:"attendees"`
	HangoutLink       string      `json:"hangoutLink"`
	ConferenceData    *struct {
		EntryPoints []struct {
			EntryPointType string `json:"entryPointType"`
			URI            string `json:"uri"`
		} `json:"entryPoints"`
	} `json:"conferenceData"`
	Reminders *struct {
		UseDefault bool `json:"useDefault"`
		Overrides  []struct {
			Method  string `json:"method"`
			Minutes int    `json:"minutes"`
		} `json:"overrides"`
	} `json:"reminders"`
}

type eventList struct {
	Items         []gEvent `json:"items"`
	NextPageToken string   `json:"nextPageToken"`
	TimeZone      string   `json:"timeZone"`
}

func (c *Client) convertTime(t gTime, fallbackZone string) (provider.EventTime, error) {
	if t.Date != "" {
		return provider.EventTime{Date: t.Date}, nil
	}
	if t.DateTime == "" {
		return provider.EventTime{}, errors.New("event has no start or end")
	}
	at, err := time.Parse(time.RFC3339, t.DateTime)
	if err != nil {
		return provider.EventTime{}, fmt.Errorf("google calendar: bad time %q", t.DateTime)
	}
	zone := t.TimeZone
	if zone == "" {
		zone = fallbackZone
	}
	loc, err := provider.LoadZone(zone)
	if err != nil {
		zone, loc = "UTC", time.UTC
	}
	return provider.EventTime{DateTime: at.In(loc), Zone: zone}, nil
}

func parseStamp(s string) time.Time {
	t, _ := time.Parse(time.RFC3339, s)
	return t
}

func attendeeResponse(s string) string {
	switch s {
	case "accepted":
		return provider.ResponseAccepted
	case "declined":
		return provider.ResponseDeclined
	case "tentative":
		return provider.ResponseTentative
	}
	return provider.ResponseNone
}

// toEvent maps a Google event. rec is the series' recurrence for an occurrence
// (nil when unknown or a one-off). calZone is the zone used for a time Google
// gave without one.
func (c *Client) toEvent(calendarID string, g gEvent, rec *provider.Recurrence, calZone string) (provider.Event, error) {
	start, err := c.convertTime(g.Start, calZone)
	if err != nil {
		return provider.Event{}, err
	}
	end, err := c.convertTime(g.End, start.Zone)
	if err != nil {
		return provider.Event{}, err
	}
	ev := provider.Event{
		ID: g.ID, CalendarID: calendarID, Title: g.Summary, Description: g.Description, Location: g.Location,
		Start: start, End: end, Version: g.ETag, Status: g.Status,
		Created: parseStamp(g.Created), Updated: parseStamp(g.Updated),
		Organizer: provider.Attendee{Email: g.Organizer.Email, Name: g.Organizer.DisplayName, Self: g.Organizer.Self},
	}
	switch {
	case g.RecurringEventID != "":
		ev.SeriesID = g.RecurringEventID
	case len(g.Recurrence) > 0:
		ev.SeriesID = g.ID
	}
	if g.OriginalStartTime != nil {
		if o, err := c.convertTime(*g.OriginalStartTime, start.Zone); err == nil {
			ev.OriginalStart = &o
		}
	}
	switch {
	case len(g.Recurrence) > 0:
		loc, _ := provider.LoadZone(start.Zone)
		r := provider.ParseRRULE(g.Recurrence, loc)
		ev.Recurrence = &r
	case rec != nil:
		cp := *rec
		ev.Recurrence = &cp
	}
	for _, a := range g.Attendees {
		ev.Attendees = append(ev.Attendees, provider.Attendee{
			Email: a.Email, Name: a.DisplayName, Response: attendeeResponse(a.ResponseStatus), Optional: a.Optional, Self: a.Self,
		})
		if a.Self {
			ev.SelfResponse = attendeeResponse(a.ResponseStatus)
		}
	}
	if g.ConferenceData != nil {
		for _, ep := range g.ConferenceData.EntryPoints {
			if ep.EntryPointType == "video" && ep.URI != "" {
				ev.Video, ev.VideoURL = true, ep.URI
				break
			}
		}
	}
	if !ev.Video && g.HangoutLink != "" {
		ev.Video, ev.VideoURL = true, g.HangoutLink
	}
	if g.Reminders != nil && !g.Reminders.UseDefault && len(g.Reminders.Overrides) > 0 {
		m := g.Reminders.Overrides[0].Minutes
		ev.ReminderMinutes = &m
	}
	return ev, nil
}

// ListEvents returns events in the window ordered by start. With one calendar
// the page token is Google's own; with several the page is cut from the merged
// union (provider.ListMerged).
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
	limit = min(limit, maxPageResults)

	var page provider.EventPage
	var err error
	if len(ids) == 1 {
		page, err = c.listOne(ctx, ids[0], q, limit, q.PageToken)
	} else {
		page, err = provider.ListMerged(ctx, ids, limit, q.PageToken, func(ctx context.Context, id string, n int) ([]provider.Event, error) {
			var all []provider.Event
			token := ""
			for len(all) < n {
				p, err := c.listOne(ctx, id, q, min(n-len(all), maxPageResults), token)
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

func (c *Client) listOne(ctx context.Context, calendarID string, q provider.ListEventsQuery, limit int, token string) (provider.EventPage, error) {
	v := url.Values{
		"singleEvents": {"true"},
		"orderBy":      {"startTime"},
		"timeMin":      {q.Start.UTC().Format(time.RFC3339)},
		"timeMax":      {q.End.UTC().Format(time.RFC3339)},
		"maxResults":   {strconv.Itoa(limit)},
	}
	if q.Text != "" {
		v.Set("q", q.Text)
	}
	if token != "" {
		v.Set("pageToken", token)
	}
	var list eventList
	if err := c.do(ctx, http.MethodGet, "/calendars/"+seg(calendarID)+"/events", v, nil, &list); err != nil {
		return provider.EventPage{}, err
	}
	calZone := list.TimeZone
	if calZone == "" {
		calZone = c.zoneOf(ctx, calendarID)
	}
	masters := map[string]*provider.Recurrence{}
	lookups := 0
	out := provider.EventPage{Events: make([]provider.Event, 0, len(list.Items)), NextPageToken: list.NextPageToken}
	for _, g := range list.Items {
		if g.Status == "cancelled" {
			continue
		}
		var rec *provider.Recurrence
		if g.RecurringEventID != "" && len(g.Recurrence) == 0 {
			if r, seen := masters[g.RecurringEventID]; seen {
				rec = r
			} else if lookups < maxMasterLookups {
				lookups++
				rec = c.seriesRecurrence(ctx, calendarID, g.RecurringEventID)
				masters[g.RecurringEventID] = rec
			} else {
				rec = &provider.Recurrence{Native: "unavailable"}
			}
		}
		ev, err := c.toEvent(calendarID, g, rec, calZone)
		if err != nil {
			continue
		}
		out.Events = append(out.Events, ev)
	}
	return out, nil
}

// seriesRecurrence reads a series master to learn the rule its occurrences
// follow. Failure is not an error for the listing: the rule is reported opaque.
func (c *Client) seriesRecurrence(ctx context.Context, calendarID, masterID string) *provider.Recurrence {
	var m gEvent
	if err := c.do(ctx, http.MethodGet, "/calendars/"+seg(calendarID)+"/events/"+seg(masterID), nil, nil, &m); err != nil || len(m.Recurrence) == 0 {
		return &provider.Recurrence{Native: "unavailable"}
	}
	zone := m.Start.TimeZone
	if zone == "" {
		zone = c.zoneOf(ctx, calendarID)
	}
	loc, err := provider.LoadZone(zone)
	if err != nil {
		loc = time.UTC
	}
	r := provider.ParseRRULE(m.Recurrence, loc)
	return &r
}

// GetEvent returns one event or occurrence.
func (c *Client) GetEvent(ctx context.Context, ref provider.EventRef) (provider.Event, error) {
	if ref.CalendarID == "" || ref.EventID == "" {
		return provider.Event{}, fmt.Errorf("%w: a calendar id and an event id are required", provider.ErrNotFound)
	}
	var g gEvent
	if err := c.do(ctx, http.MethodGet, "/calendars/"+seg(ref.CalendarID)+"/events/"+seg(ref.EventID), nil, nil, &g); err != nil {
		return provider.Event{}, err
	}
	if g.Status == "cancelled" {
		return provider.Event{}, fmt.Errorf("%w: the event was cancelled", provider.ErrNotFound)
	}
	var rec *provider.Recurrence
	if g.RecurringEventID != "" && len(g.Recurrence) == 0 {
		rec = c.seriesRecurrence(ctx, ref.CalendarID, g.RecurringEventID)
	}
	return c.toEvent(ref.CalendarID, g, rec, c.zoneOf(ctx, ref.CalendarID))
}

// FreeBusy asks Google's freeBusy endpoint, which merges each calendar's busy
// time and leaves out events the account declined. Another person's calendar
// that cannot be read comes back in Unavailable, never as free.
func (c *Client) FreeBusy(ctx context.Context, q provider.FreeBusyQuery) (provider.FreeBusyResult, error) {
	if err := provider.ValidateWindow(q.Start, q.End); err != nil {
		return provider.FreeBusyResult{}, err
	}
	ids := q.CalendarIDs
	if len(ids) == 0 {
		id, err := c.primaryID(ctx)
		if err != nil {
			return provider.FreeBusyResult{}, err
		}
		ids = []string{id}
	}
	type item struct {
		ID string `json:"id"`
	}
	var items []item
	own := map[string]bool{}
	for _, id := range ids {
		items = append(items, item{id})
		own[id] = true
	}
	for _, e := range q.Emails {
		if e = strings.ToLower(strings.TrimSpace(e)); e != "" && !own[e] {
			items = append(items, item{e})
		}
	}
	if len(items) > maxFreeBusyItems {
		return provider.FreeBusyResult{}, fmt.Errorf("%w: at most %d calendars and people per call", provider.ErrInvalidQuery, maxFreeBusyItems)
	}
	body := map[string]any{
		"timeMin": q.Start.UTC().Format(time.RFC3339), "timeMax": q.End.UTC().Format(time.RFC3339), "items": items,
	}
	var resp struct {
		Calendars map[string]struct {
			Busy []struct {
				Start string `json:"start"`
				End   string `json:"end"`
			} `json:"busy"`
			Errors []struct {
				Reason string `json:"reason"`
			} `json:"errors"`
		} `json:"calendars"`
	}
	if err := c.do(ctx, http.MethodPost, "/freeBusy", nil, body, &resp); err != nil {
		return provider.FreeBusyResult{}, err
	}
	out := provider.FreeBusyResult{Busy: map[string][]provider.Interval{}}
	for _, it := range items {
		cal, ok := resp.Calendars[it.ID]
		if !ok || len(cal.Errors) > 0 {
			reason := "not_returned"
			if ok {
				reason = cal.Errors[0].Reason
			}
			if own[it.ID] {
				return provider.FreeBusyResult{}, fmt.Errorf("%w: calendar %s: %s", provider.ErrNotFound, it.ID, reason)
			}
			if out.Unavailable == nil {
				out.Unavailable = map[string]string{}
			}
			out.Unavailable[it.ID] = reason
			continue
		}
		ivs := make([]provider.Interval, 0, len(cal.Busy))
		for _, b := range cal.Busy {
			s, e := parseStamp(b.Start), parseStamp(b.End)
			if s.IsZero() || e.IsZero() {
				continue
			}
			ivs = append(ivs, provider.Interval{Start: s, End: e})
		}
		out.Busy[it.ID] = provider.MergeIntervals(ivs)
	}
	return out, nil
}
