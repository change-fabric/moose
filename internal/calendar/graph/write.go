package graph

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/change-fabric/moose/internal/calendar/provider"
	"github.com/change-fabric/moose/internal/graphapi"
)

// The write side, built against documented Graph behaviour and synthetic
// fixtures. The UNVERIFIED rows of docs/microsoft-graph-spike.md list what is
// assumed: If-Match with the event's @odata.etag (W/"<changeKey>") on PATCH,
// DELETE and POST .../cancel, transactionId on create, IANA names in the body's
// timeZone, patternedRecurrence with a range start date, isOnlineMeeting with
// teamsForBusiness, and the accept, tentativelyAccept and decline actions.
//
// Graph always emails attendees on these calls: there is no switch, which is
// why the tools send an event with other attendees to approval first.
//
// Every update, delete and cancel is conditional on the changeKey the caller
// read. A write with no version is refused here without a request.

func (c *Client) calendarList(ctx context.Context) ([]provider.Calendar, error) {
	c.mu.Lock()
	cached := c.cals
	c.mu.Unlock()
	if cached != nil {
		return cached, nil
	}
	return c.ListCalendars(ctx)
}

func (c *Client) checkWritable(ctx context.Context, calendarID string) error {
	cals, err := c.calendarList(ctx)
	if err != nil {
		return err
	}
	for _, cal := range cals {
		if cal.ID == calendarID {
			if !cal.CanWrite() {
				return fmt.Errorf("%w: calendar %q is read-only for this account", provider.ErrRejected, calendarID)
			}
			return nil
		}
	}
	return fmt.Errorf("%w: calendar %q", provider.ErrNotFound, calendarID)
}

func (c *Client) noVideoHere() error {
	if c.opts.Personal {
		return fmt.Errorf("%w: online meetings (Teams) are not available on a personal Microsoft account", provider.ErrUnsupportedHere)
	}
	return nil
}

const graphTimeLayout = "2006-01-02T15:04:05.0000000"

func timeBody(t provider.EventTime) map[string]any {
	if t.IsAllDay() {
		d, _ := time.Parse(provider.DateLayout, t.Date)
		return map[string]any{"dateTime": d.Format(graphTimeLayout), "timeZone": "UTC"}
	}
	loc, err := provider.LoadZone(t.Zone)
	if err != nil {
		loc = time.UTC
	}
	return map[string]any{"dateTime": t.DateTime.In(loc).Format(graphTimeLayout), "timeZone": t.Zone}
}

func attendeeBodies(list []provider.Attendee) []map[string]any {
	out := make([]map[string]any, 0, len(list))
	for _, a := range list {
		typ := "required"
		if a.Optional {
			typ = "optional"
		}
		addr := map[string]any{"address": strings.TrimSpace(a.Email)}
		if a.Name != "" {
			addr["name"] = a.Name
		}
		out = append(out, map[string]any{"emailAddress": addr, "type": typ})
	}
	return out
}

var graphDayNames = map[string]string{
	"MO": "monday", "TU": "tuesday", "WE": "wednesday", "TH": "thursday", "FR": "friday", "SA": "saturday", "SU": "sunday",
}

var weekdayCodes = []string{"SU", "MO", "TU", "WE", "TH", "FR", "SA"}

// patternedBody renders r as a Graph patternedRecurrence anchored at start:
// monthly and yearly follow the start's day and month, a weekly rule with no
// weekdays follows the start's weekday, and the range begins on the start's
// date in its zone. The week starts on Monday, the first day the read side
// treats as the neutral default.
func patternedBody(r provider.Recurrence, start provider.EventTime) map[string]any {
	var first time.Time
	zone := "UTC"
	if start.IsAllDay() {
		first, _ = time.Parse(provider.DateLayout, start.Date)
	} else {
		first, zone = start.DateTime, start.Zone
		if loc, err := provider.LoadZone(zone); err == nil {
			first = first.In(loc)
		}
	}
	pat := map[string]any{"interval": max(r.Interval, 1)}
	switch r.Freq {
	case provider.FreqDaily:
		pat["type"] = "daily"
	case provider.FreqWeekly:
		pat["type"] = "weekly"
		days := []string{}
		for _, d := range r.ByDay {
			days = append(days, graphDayNames[d])
		}
		if len(days) == 0 {
			days = append(days, graphDayNames[weekdayCodes[first.Weekday()]])
		}
		pat["daysOfWeek"], pat["firstDayOfWeek"] = days, "monday"
	case provider.FreqMonthly:
		pat["type"], pat["dayOfMonth"] = "absoluteMonthly", first.Day()
	case provider.FreqYearly:
		pat["type"], pat["dayOfMonth"], pat["month"] = "absoluteYearly", first.Day(), int(first.Month())
	}
	rng := map[string]any{"startDate": first.Format(provider.DateLayout), "recurrenceTimeZone": zone}
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

func bodyText(s string) map[string]any { return map[string]any{"contentType": "text", "content": s} }

// transactionIDFor derives the transactionId for a dedupe key: lowercase hex of
// a hash, so no user text rides in it and it is stable for one logical create.
func transactionIDFor(key string) string {
	sum := sha256.Sum256([]byte("moose-calendar-create:" + key))
	return hex.EncodeToString(sum[:])
}

func (c *Client) send(ctx context.Context, method, path string, hdr http.Header, body any, out any) error {
	var rd *bytes.Reader
	req := graphapi.Request{Method: method, Path: path, Header: hdr}
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("graph calendar: %w", err)
		}
		rd = bytes.NewReader(raw)
		req.Body, req.ContentType = rd, "application/json"
	}
	resp, err := c.g.Do(ctx, req)
	if err != nil {
		return mapErr(err)
	}
	defer resp.Body.Close()
	if out == nil {
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("graph calendar: decode %s: %w", path, err)
	}
	return nil
}

func eventPath(ref provider.EventRef) string {
	return "/me/calendars/" + graphapi.Segment(ref.CalendarID) + "/events/" + graphapi.Segment(ref.EventID)
}

func ifMatch(version string) http.Header {
	return http.Header{"If-Match": {`W/"` + version + `"`}}
}

func withPrefer(h http.Header, prefer string) http.Header {
	if h == nil {
		h = http.Header{}
	}
	h.Set("Prefer", prefer)
	return h
}

// CreateEvent creates an event. A DedupeKey becomes the transactionId, so a
// retry of the same create does not make a second event.
func (c *Client) CreateEvent(ctx context.Context, calendarID string, in provider.EventInput, opts provider.CreateOptions) (provider.Event, error) {
	if err := provider.ValidateEventInput(in); err != nil {
		return provider.Event{}, err
	}
	if in.Video {
		if err := c.noVideoHere(); err != nil {
			return provider.Event{}, err
		}
	}
	if err := c.checkWritable(ctx, calendarID); err != nil {
		return provider.Event{}, err
	}
	body := map[string]any{
		"subject": in.Title, "start": timeBody(in.Start), "end": timeBody(in.End), "isAllDay": in.Start.IsAllDay(),
	}
	if in.Description != "" {
		body["body"] = bodyText(in.Description)
	}
	if in.Location != "" {
		body["location"] = map[string]any{"displayName": in.Location}
	}
	if in.Recurrence != nil {
		body["recurrence"] = patternedBody(*in.Recurrence, in.Start)
	}
	if len(in.Attendees) > 0 {
		body["attendees"] = attendeeBodies(in.Attendees)
	}
	if in.ReminderMinutes != nil {
		body["isReminderOn"], body["reminderMinutesBeforeStart"] = true, *in.ReminderMinutes
	}
	if in.Video {
		body["isOnlineMeeting"], body["onlineMeetingProvider"] = true, "teamsForBusiness"
	}
	if opts.DedupeKey != "" {
		body["transactionId"] = transactionIDFor(opts.DedupeKey)
	}
	zone := in.Start.Zone
	if in.Start.IsAllDay() {
		zone = "UTC"
	}
	var r eventRes
	path := "/me/calendars/" + graphapi.Segment(calendarID) + "/events"
	if err := c.send(ctx, http.MethodPost, path, withPrefer(nil, zonePrefer(zone)), body, &r); err != nil {
		return provider.Event{}, provider.AsRejected(err)
	}
	ev, err := c.toEvent(calendarID, r, nil, false)
	if err != nil {
		return provider.Event{}, err
	}
	if in.Video && !ev.Video {
		if again, gerr := c.GetEvent(ctx, provider.EventRef{CalendarID: calendarID, EventID: r.ID}); gerr == nil {
			ev = again
		}
	}
	return ev, nil
}

func (c *Client) readRaw(ctx context.Context, ref provider.EventRef) (eventRes, error) {
	var r eventRes
	q := url.Values{"$select": {eventSelect}}
	if err := c.get(ctx, eventPath(ref), q, zonePrefer(""), &r); err != nil {
		return r, err
	}
	return r, nil
}

// target resolves what a write goes to and the changeKey it presents, as the
// Google client does: an instance, a one-off and a master named directly are
// written as named with the caller's version; a series written through one of
// its occurrences goes to the master, after the occurrence is confirmed to be
// still at the version the caller holds. cur is the event read to decide, nil
// when no read was needed.
func (c *Client) target(ctx context.Context, ref provider.EventRef, scope provider.Scope, ifVersion string) (provider.EventRef, string, *eventRes, error) {
	if scope == provider.ScopeInstance {
		return ref, ifVersion, nil, nil
	}
	r, err := c.readRaw(ctx, ref)
	if err != nil {
		return ref, "", nil, err
	}
	if r.SeriesID == "" || r.Type == "seriesMaster" {
		return ref, ifVersion, &r, nil
	}
	if r.ChangeKey != ifVersion {
		return ref, "", nil, fmt.Errorf("%w: occurrence is at %s, caller read %s", provider.ErrPreconditionFailed, r.ChangeKey, ifVersion)
	}
	mref := provider.EventRef{CalendarID: ref.CalendarID, EventID: r.SeriesID}
	m, err := c.readRaw(ctx, mref)
	if err != nil {
		return ref, "", nil, err
	}
	return mref, m.ChangeKey, &m, nil
}

// UpdateEvent patches an event, conditional on its changeKey.
func (c *Client) UpdateEvent(ctx context.Context, ref provider.EventRef, scope provider.Scope, patch provider.EventPatch, ifVersion string) (provider.Event, error) {
	if err := provider.RequireVersion(ref, scope, ifVersion); err != nil {
		return provider.Event{}, err
	}
	if err := provider.ValidatePatch(patch); err != nil {
		return provider.Event{}, err
	}
	if patch.Video != nil && *patch.Video {
		if err := c.noVideoHere(); err != nil {
			return provider.Event{}, err
		}
	}
	if err := c.checkWritable(ctx, ref.CalendarID); err != nil {
		return provider.Event{}, err
	}
	tref, version, cur, err := c.target(ctx, ref, scope, ifVersion)
	if err != nil {
		return provider.Event{}, err
	}
	body := map[string]any{}
	if patch.Title != nil {
		body["subject"] = *patch.Title
	}
	if patch.Description != nil {
		body["body"] = bodyText(*patch.Description)
	}
	if patch.Location != nil {
		body["location"] = map[string]any{"displayName": *patch.Location}
	}
	if patch.Start != nil {
		body["start"], body["isAllDay"] = timeBody(*patch.Start), patch.Start.IsAllDay()
	}
	if patch.End != nil {
		body["end"] = timeBody(*patch.End)
	}
	switch {
	case patch.ClearRecurrence:
		body["recurrence"] = nil
	case patch.Recurrence != nil:
		start, serr := c.seriesStart(ctx, tref, patch, cur)
		if serr != nil {
			return provider.Event{}, serr
		}
		body["recurrence"] = patternedBody(*patch.Recurrence, start)
	}
	if patch.Attendees != nil {
		body["attendees"] = attendeeBodies(*patch.Attendees)
	}
	if patch.Video != nil {
		body["isOnlineMeeting"] = *patch.Video
		if *patch.Video {
			body["onlineMeetingProvider"] = "teamsForBusiness"
		}
	}
	if patch.ReminderMinutes != nil {
		body["isReminderOn"], body["reminderMinutesBeforeStart"] = true, *patch.ReminderMinutes
	}
	if len(body) == 0 {
		return provider.Event{}, fmt.Errorf("%w: the update changes nothing", provider.ErrRejected)
	}
	hdr := withPrefer(ifMatch(version), zonePrefer(""))
	var r eventRes
	if err := c.send(ctx, http.MethodPatch, eventPath(tref), hdr, body, &r); err != nil {
		return provider.Event{}, provider.AsRejected(err)
	}
	var rec *provider.Recurrence
	if r.SeriesID != "" && len(r.Recurrence) == 0 {
		rec = c.seriesRecurrence(ctx, tref.CalendarID, r.SeriesID)
	}
	return c.toEvent(tref.CalendarID, r, rec, true)
}

// seriesStart is the start a recurrence's range is anchored to: the patch's own
// start, else the event's, shown in the zone it was created in.
func (c *Client) seriesStart(ctx context.Context, ref provider.EventRef, patch provider.EventPatch, cur *eventRes) (provider.EventTime, error) {
	if patch.Start != nil {
		return *patch.Start, nil
	}
	if cur == nil {
		r, err := c.readRaw(ctx, ref)
		if err != nil {
			return provider.EventTime{}, err
		}
		cur = &r
	}
	ev, err := c.toEvent(ref.CalendarID, *cur, nil, true)
	if err != nil {
		return provider.EventTime{}, err
	}
	return ev.Start, nil
}

// DeleteEvent removes an event, conditional on its changeKey. An event the
// account organizes that has other attendees is cancelled instead (POST
// .../cancel), which is what makes Graph send the cancellation; anything else
// is a plain DELETE.
func (c *Client) DeleteEvent(ctx context.Context, ref provider.EventRef, scope provider.Scope, ifVersion string) error {
	if err := provider.RequireVersion(ref, scope, ifVersion); err != nil {
		return err
	}
	if err := c.checkWritable(ctx, ref.CalendarID); err != nil {
		return err
	}
	tref, version, cur, err := c.target(ctx, ref, scope, ifVersion)
	if err != nil {
		return err
	}
	if cur == nil {
		r, rerr := c.readRaw(ctx, tref)
		if rerr != nil {
			return rerr
		}
		cur = &r
	}
	if c.cancelsWithNotice(*cur) {
		return provider.AsRejected(c.send(ctx, http.MethodPost, eventPath(tref)+"/cancel", ifMatch(version), map[string]any{"comment": ""}, nil))
	}
	return provider.AsRejected(c.send(ctx, http.MethodDelete, eventPath(tref), ifMatch(version), nil, nil))
}

// cancelsWithNotice reports whether removing r is a meeting cancellation: the
// account is its organizer and someone else is invited.
func (c *Client) cancelsWithNotice(r eventRes) bool {
	if r.Organizer == nil || !c.isSelf(r.Organizer.EmailAddress.Address) {
		return false
	}
	for _, a := range r.Attendees {
		if !c.isSelf(a.EmailAddress.Address) && strings.TrimSpace(a.EmailAddress.Address) != "" {
			return true
		}
	}
	return false
}

// Respond answers an invitation with Graph's accept, tentativelyAccept or
// decline action, sending the response (and the comment) to the organizer.
func (c *Client) Respond(ctx context.Context, ref provider.EventRef, resp provider.Response, comment string) error {
	action := map[provider.Response]string{
		provider.RespondAccept: "accept", provider.RespondTentative: "tentativelyAccept", provider.RespondDecline: "decline",
	}[resp]
	if action == "" {
		return fmt.Errorf("%w: response must be accept, tentative or decline", provider.ErrRejected)
	}
	if len([]rune(comment)) > provider.MaxResponseComment {
		return fmt.Errorf("%w: comment is over %d characters", provider.ErrRejected, provider.MaxResponseComment)
	}
	if ref.CalendarID == "" || ref.EventID == "" {
		return fmt.Errorf("%w: a calendar id and an event id are required", provider.ErrNotFound)
	}
	body := map[string]any{"comment": comment, "sendResponse": true}
	return provider.AsRejected(c.send(ctx, http.MethodPost, eventPath(ref)+"/"+action, nil, body, nil))
}
