package google

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/change-fabric/moose/internal/calendar/provider"
)

// The write side, built against documented Google Calendar v3 behaviour. The
// rows of docs/microsoft-graph-spike.md marked UNVERIFIED (not Microsoft) list
// what is assumed: If-Match on events.patch and events.delete, a client-supplied
// event id on insert, sendUpdates, conferenceData.createRequest, and the
// attendee-only patch that is an RSVP.
//
// Every update, delete and RSVP is conditional (If-Match with the etag the
// caller read). A write with no version is refused here without a request: a
// blind overwrite is never a fallback.

func (c *Client) checkWritable(ctx context.Context, calendarID string) error {
	cals, err := c.calendars(ctx)
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

func timeBody(t provider.EventTime) map[string]any {
	if t.IsAllDay() {
		return map[string]any{"date": t.Date, "dateTime": nil, "timeZone": nil}
	}
	return map[string]any{"dateTime": t.DateTime.Format(time.RFC3339), "timeZone": t.Zone, "date": nil}
}

func attendeeBodies(list []provider.Attendee) []map[string]any {
	out := make([]map[string]any, 0, len(list))
	for _, a := range list {
		m := map[string]any{"email": strings.TrimSpace(a.Email)}
		if a.Name != "" {
			m["displayName"] = a.Name
		}
		if a.Optional {
			m["optional"] = true
		}
		out = append(out, m)
	}
	return out
}

func reminderBody(minutes int) map[string]any {
	return map[string]any{"useDefault": false, "overrides": []map[string]any{{"method": "popup", "minutes": minutes}}}
}

// eventID derives the Google event id for a dedupe key. Google ids are 5 to
// 1024 characters from base32hex (a-v and 0-9); lowercase hex of a hash fits
// and carries no user text.
func eventIDFor(key string) string {
	sum := sha256.Sum256([]byte("moose-calendar-create:" + key))
	return hex.EncodeToString(sum[:])
}

func meetRequest(seed string) map[string]any {
	if seed == "" {
		var b [12]byte
		_, _ = rand.Read(b[:])
		seed = hex.EncodeToString(b[:])
	}
	return map[string]any{"createRequest": map[string]any{
		"requestId":             eventIDFor("meet:" + seed),
		"conferenceSolutionKey": map[string]any{"type": "hangoutsMeet"},
	}}
}

// CreateEvent inserts an event. A DedupeKey becomes the event's id, so a retry
// or a reconciliation after an unseen outcome finds the same event: a second
// insert with that id answers 409, and the event is read back instead.
func (c *Client) CreateEvent(ctx context.Context, calendarID string, in provider.EventInput, opts provider.CreateOptions) (provider.Event, error) {
	if err := provider.ValidateEventInput(in); err != nil {
		return provider.Event{}, err
	}
	if err := c.checkWritable(ctx, calendarID); err != nil {
		return provider.Event{}, err
	}
	body := map[string]any{"summary": in.Title}
	if in.Start.IsAllDay() {
		body["start"], body["end"] = map[string]any{"date": in.Start.Date}, map[string]any{"date": in.End.Date}
	} else {
		body["start"] = map[string]any{"dateTime": in.Start.DateTime.Format(time.RFC3339), "timeZone": in.Start.Zone}
		body["end"] = map[string]any{"dateTime": in.End.DateTime.Format(time.RFC3339), "timeZone": in.End.Zone}
	}
	if in.Description != "" {
		body["description"] = in.Description
	}
	if in.Location != "" {
		body["location"] = in.Location
	}
	if in.Recurrence != nil {
		body["recurrence"] = []string{provider.FormatRRULE(*in.Recurrence, in.Start)}
	}
	if len(in.Attendees) > 0 {
		body["attendees"] = attendeeBodies(in.Attendees)
	}
	if in.ReminderMinutes != nil {
		body["reminders"] = reminderBody(*in.ReminderMinutes)
	}
	q := url.Values{"sendUpdates": {"none"}}
	if len(in.Attendees) > 0 {
		q.Set("sendUpdates", "all")
	}
	if in.Video {
		q.Set("conferenceDataVersion", "1")
		body["conferenceData"] = meetRequest(opts.DedupeKey)
	}
	id := ""
	if opts.DedupeKey != "" {
		id = eventIDFor(opts.DedupeKey)
		body["id"] = id
	}
	var g gEvent
	err := c.do(ctx, http.MethodPost, "/calendars/"+seg(calendarID)+"/events", q, body, &g)
	if err != nil {
		var se *statusError
		if id != "" && errors.As(err, &se) && se.Status == http.StatusConflict {
			// The id exists: this create already happened. Return that event.
			if ev, gerr := c.GetEvent(ctx, provider.EventRef{CalendarID: calendarID, EventID: id}); gerr == nil {
				return ev, nil
			}
		}
		return provider.Event{}, provider.AsRejected(err)
	}
	ev, err := c.toEvent(calendarID, g, nil, c.zoneOf(ctx, calendarID))
	if err != nil {
		return provider.Event{}, err
	}
	if in.Video && !ev.Video {
		// Meet links are made asynchronously; one read usually finds the link.
		if again, gerr := c.GetEvent(ctx, provider.EventRef{CalendarID: calendarID, EventID: g.ID}); gerr == nil {
			ev = again
		}
	}
	return ev, nil
}

// FindByDedupeKey implements provider.DedupeFinder: a create's dedupe key is the
// event's id, so the lookup is a read by that id. A missing or deleted event is
// not found.
func (c *Client) FindByDedupeKey(ctx context.Context, calendarID, key string) (provider.Event, bool, error) {
	ev, err := c.GetEvent(ctx, provider.EventRef{CalendarID: calendarID, EventID: eventIDFor(key)})
	if errors.Is(err, provider.ErrNotFound) {
		return provider.Event{}, false, nil
	}
	if err != nil {
		return provider.Event{}, false, err
	}
	return ev, true, nil
}

// target resolves what a write is sent to and the etag it must present. An
// instance, and a one-off or master named directly, is written as named with
// the caller's version. A series written through one of its occurrences goes
// to the master: the occurrence is read to confirm it is still at the version
// the caller holds, then the master is read for its own etag, which is what the
// conditional write presents. The window between those reads is small and the
// If-Match still guards the write itself; tools that can name the master
// directly do, and so get the strict path.
func (c *Client) target(ctx context.Context, ref provider.EventRef, scope provider.Scope, ifVersion string) (provider.EventRef, string, *gEvent, error) {
	if scope == provider.ScopeInstance {
		return ref, ifVersion, nil, nil
	}
	var g gEvent
	if err := c.do(ctx, http.MethodGet, "/calendars/"+seg(ref.CalendarID)+"/events/"+seg(ref.EventID), nil, nil, &g); err != nil {
		return ref, "", nil, err
	}
	if g.RecurringEventID == "" {
		return ref, ifVersion, &g, nil
	}
	if g.ETag != ifVersion {
		return ref, "", nil, fmt.Errorf("%w: occurrence is at %s, caller read %s", provider.ErrPreconditionFailed, g.ETag, ifVersion)
	}
	var m gEvent
	mref := provider.EventRef{CalendarID: ref.CalendarID, EventID: g.RecurringEventID}
	if err := c.do(ctx, http.MethodGet, "/calendars/"+seg(mref.CalendarID)+"/events/"+seg(mref.EventID), nil, nil, &m); err != nil {
		return ref, "", nil, err
	}
	return mref, m.ETag, &m, nil
}

func ifMatch(etag string) http.Header { return http.Header{"If-Match": {etag}} }

// UpdateEvent patches an event, conditional on its etag.
func (c *Client) UpdateEvent(ctx context.Context, ref provider.EventRef, scope provider.Scope, patch provider.EventPatch, ifVersion string) (provider.Event, error) {
	if err := provider.RequireVersion(ref, scope, ifVersion); err != nil {
		return provider.Event{}, err
	}
	if err := provider.ValidatePatch(patch); err != nil {
		return provider.Event{}, err
	}
	if err := c.checkWritable(ctx, ref.CalendarID); err != nil {
		return provider.Event{}, err
	}
	tref, etag, cur, err := c.target(ctx, ref, scope, ifVersion)
	if err != nil {
		return provider.Event{}, err
	}
	body := map[string]any{}
	if patch.Title != nil {
		body["summary"] = *patch.Title
	}
	if patch.Description != nil {
		body["description"] = *patch.Description
	}
	if patch.Location != nil {
		body["location"] = *patch.Location
	}
	if patch.Start != nil {
		body["start"] = timeBody(*patch.Start)
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
		body["recurrence"] = []string{provider.FormatRRULE(*patch.Recurrence, start)}
	}
	if patch.Attendees != nil {
		body["attendees"] = attendeeBodies(*patch.Attendees)
	}
	if patch.ReminderMinutes != nil {
		body["reminders"] = reminderBody(*patch.ReminderMinutes)
	}
	q := url.Values{"sendUpdates": {"all"}}
	if patch.Video != nil {
		q.Set("conferenceDataVersion", "1")
		if *patch.Video {
			body["conferenceData"] = meetRequest("")
		} else {
			body["conferenceData"] = nil
		}
	}
	if len(body) == 0 {
		return provider.Event{}, fmt.Errorf("%w: the update changes nothing", provider.ErrRejected)
	}
	var g gEvent
	if err := c.doH(ctx, http.MethodPatch, "/calendars/"+seg(tref.CalendarID)+"/events/"+seg(tref.EventID), q, ifMatch(etag), body, &g); err != nil {
		return provider.Event{}, provider.AsRejected(err)
	}
	rec := (*provider.Recurrence)(nil)
	if g.RecurringEventID != "" && len(g.Recurrence) == 0 {
		rec = c.seriesRecurrence(ctx, tref.CalendarID, g.RecurringEventID)
	}
	return c.toEvent(tref.CalendarID, g, rec, c.zoneOf(ctx, tref.CalendarID))
}

// seriesStart is the start an RRULE's UNTIL is anchored to: the patch's own
// start, else the event's.
func (c *Client) seriesStart(ctx context.Context, ref provider.EventRef, patch provider.EventPatch, cur *gEvent) (provider.EventTime, error) {
	if patch.Start != nil {
		return *patch.Start, nil
	}
	if cur == nil {
		cur = &gEvent{}
		if err := c.do(ctx, http.MethodGet, "/calendars/"+seg(ref.CalendarID)+"/events/"+seg(ref.EventID), nil, nil, cur); err != nil {
			return provider.EventTime{}, err
		}
	}
	zone := cur.Start.TimeZone
	if zone == "" {
		zone = c.zoneOf(ctx, ref.CalendarID)
	}
	return c.convertTime(cur.Start, zone)
}

// DeleteEvent deletes an event, conditional on its etag. Google emails the
// attendees of an event with guests as a cancellation (sendUpdates=all).
func (c *Client) DeleteEvent(ctx context.Context, ref provider.EventRef, scope provider.Scope, ifVersion string) error {
	if err := provider.RequireVersion(ref, scope, ifVersion); err != nil {
		return err
	}
	if err := c.checkWritable(ctx, ref.CalendarID); err != nil {
		return err
	}
	tref, etag, _, err := c.target(ctx, ref, scope, ifVersion)
	if err != nil {
		return err
	}
	q := url.Values{"sendUpdates": {"all"}}
	return provider.AsRejected(c.doH(ctx, http.MethodDelete, "/calendars/"+seg(tref.CalendarID)+"/events/"+seg(tref.EventID), q, ifMatch(etag), nil, nil))
}

// Respond sets the account's own answer. Google has no RSVP endpoint: the
// answer is the self attendee's responseStatus, set with a patch that presents
// the etag just read, so an RSVP never overwrites a concurrent change to the
// guest list. sendUpdates=all tells the organizer.
func (c *Client) Respond(ctx context.Context, ref provider.EventRef, resp provider.Response, comment string) error {
	if !resp.Valid() {
		return fmt.Errorf("%w: response must be accept, tentative or decline", provider.ErrRejected)
	}
	if len([]rune(comment)) > provider.MaxResponseComment {
		return fmt.Errorf("%w: comment is over %d characters", provider.ErrRejected, provider.MaxResponseComment)
	}
	if ref.CalendarID == "" || ref.EventID == "" {
		return fmt.Errorf("%w: a calendar id and an event id are required", provider.ErrNotFound)
	}
	var raw struct {
		ETag      string           `json:"etag"`
		Status    string           `json:"status"`
		Attendees []map[string]any `json:"attendees"`
	}
	path := "/calendars/" + seg(ref.CalendarID) + "/events/" + seg(ref.EventID)
	if err := c.do(ctx, http.MethodGet, path, nil, nil, &raw); err != nil {
		return err
	}
	if raw.Status == "cancelled" {
		return fmt.Errorf("%w: the event was cancelled", provider.ErrNotFound)
	}
	status := map[provider.Response]string{
		provider.RespondAccept: "accepted", provider.RespondTentative: "tentative", provider.RespondDecline: "declined",
	}[resp]
	found := false
	for _, a := range raw.Attendees {
		if self, _ := a["self"].(bool); self {
			a["responseStatus"] = status
			if comment != "" {
				a["comment"] = comment
			} else {
				delete(a, "comment")
			}
			found = true
		}
	}
	if !found {
		return fmt.Errorf("%w: the account is not an attendee of this event", provider.ErrRejected)
	}
	q := url.Values{"sendUpdates": {"all"}}
	return provider.AsRejected(c.doH(ctx, http.MethodPatch, path, q, ifMatch(raw.ETag), map[string]any{"attendees": raw.Attendees}, nil))
}
