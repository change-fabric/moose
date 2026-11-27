// Package fake is an in-memory calendar provider.Provider for tests. It plays
// the parts of a real calendar the tools depend on: a primary calendar and
// others with access roles, recurring series that expand into occurrences with
// per-occurrence overrides, event versions that change on every write, a
// precondition on every update and delete, a create dedupe key, free/busy, an
// RSVP, and a record of who would have been emailed.
package fake

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/change-fabric/moose/internal/calendar/provider"
)

// PrimaryID is the id of the account's primary calendar.
const PrimaryID = "cal-primary"

// Notification records an email the provider would have sent.
type Notification struct {
	// Kind is invite, update, cancel or rsvp.
	Kind    string
	EventID string
	To      []string
}

// Provider is a concurrency-safe in-memory calendar account.
type Provider struct {
	mu        sync.Mutex
	self      string
	caps      provider.Capabilities
	calendars []provider.Calendar
	events    map[string]*record // by event id (masters and one-offs)
	dedupe    map[string]string  // dedupe key -> event id
	others    map[string][]provider.Interval
	notes     []Notification
	seq       int

	// Err, when set, is returned by every call. Tests use it to simulate an
	// outage, a revoked credential or a rate limit.
	Err error
	// Now is the clock for created and updated stamps; nil means time.Now.
	Now func() time.Time
}

// record is a stored event: a one-off, or a series master with its overrides.
type record struct {
	ev        provider.Event
	ver       int
	overrides map[string]*override // by occurrence id
}

// override is an occurrence that was changed or cancelled on its own.
type override struct {
	ev        provider.Event
	ver       int
	cancelled bool
}

// New returns a fake for the account whose address is self, with the full
// capability set and one primary calendar.
func New(self string) *Provider {
	return &Provider{
		self:      self,
		caps:      provider.Capabilities{FreeBusyOthers: true, Video: true, Recurrence: true},
		calendars: []provider.Calendar{{ID: PrimaryID, Name: "Calendar", Primary: true, Role: provider.RoleOwner, TimeZone: "UTC"}},
		events:    map[string]*record{},
		dedupe:    map[string]string{},
		others:    map[string][]provider.Interval{},
	}
}

// SetCapabilities replaces what the fake reports and enforces.
func (p *Provider) SetCapabilities(c provider.Capabilities) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.caps = c
}

// AddCalendar adds a non-primary calendar.
func (p *Provider) AddCalendar(c provider.Calendar) {
	p.mu.Lock()
	defer p.mu.Unlock()
	c.Primary = false
	p.calendars = append(p.calendars, c)
}

// AddBusy records busy time for another person, for FreeBusy.
func (p *Provider) AddBusy(email string, in provider.Interval) {
	p.mu.Lock()
	defer p.mu.Unlock()
	k := strings.ToLower(email)
	p.others[k] = append(p.others[k], in)
}

// AddEvent stores ev on calendarID as seeded data, assigning its id and
// version, with no notification. The organizer defaults to the account.
func (p *Provider) AddEvent(calendarID string, ev provider.Event) provider.Event {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.seq++
	ev.ID = "ev" + strconv.Itoa(p.seq)
	ev.CalendarID = calendarID
	if ev.Organizer.Email == "" {
		ev.Organizer = provider.Attendee{Email: p.self, Self: true}
	}
	if ev.Status == "" {
		ev.Status = provider.StatusConfirmed
	}
	if ev.Recurrence != nil {
		ev.SeriesID = ev.ID
	}
	rec := &record{ev: ev, ver: 1, overrides: map[string]*override{}}
	p.events[ev.ID] = rec
	return p.view(rec)
}

// ExternalAddAttendee adds an attendee as if someone else edited the event: the
// version changes and nobody is notified by moose. It models the race the
// version precondition guards.
func (p *Provider) ExternalAddAttendee(ref provider.EventRef, a provider.Attendee) {
	p.mu.Lock()
	defer p.mu.Unlock()
	rec, _, ok := p.find(ref)
	if !ok {
		return
	}
	if a.Response == "" {
		a.Response = provider.ResponseNone
	}
	rec.ev.Attendees = append(rec.ev.Attendees, a)
	rec.ver++
}

// Notifications returns the emails sent so far.
func (p *Provider) Notifications() []Notification {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]Notification(nil), p.notes...)
}

func (p *Provider) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now()
}

func (p *Provider) check(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return p.Err
}

// Capabilities implements provider.Provider.
func (p *Provider) Capabilities() provider.Capabilities {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.caps
}

// ListCalendars implements provider.Provider.
func (p *Provider) ListCalendars(ctx context.Context) ([]provider.Calendar, error) {
	if err := p.check(ctx); err != nil {
		return nil, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]provider.Calendar(nil), p.calendars...), nil
}

func (p *Provider) calendar(id string) (provider.Calendar, bool) {
	for _, c := range p.calendars {
		if c.ID == id {
			return c, true
		}
	}
	return provider.Calendar{}, false
}

// ---- reading ----

// ListEvents implements provider.Provider.
func (p *Provider) ListEvents(ctx context.Context, q provider.ListEventsQuery) (provider.EventPage, error) {
	if err := p.check(ctx); err != nil {
		return provider.EventPage{}, err
	}
	if q.Start.IsZero() || q.End.IsZero() || !q.End.After(q.Start) {
		return provider.EventPage{}, fmt.Errorf("%w: the window must have an end after its start", provider.ErrInvalidQuery)
	}
	offset := 0
	if q.PageToken != "" {
		n, err := strconv.Atoi(strings.TrimPrefix(q.PageToken, "pt-"))
		if err != nil || !strings.HasPrefix(q.PageToken, "pt-") || n < 0 {
			return provider.EventPage{}, fmt.Errorf("%w: unusable page token", provider.ErrInvalidQuery)
		}
		offset = n
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	cals := q.CalendarIDs
	if len(cals) == 0 {
		cals = []string{PrimaryID}
	}
	for _, id := range cals {
		if _, ok := p.calendar(id); !ok {
			return provider.EventPage{}, fmt.Errorf("%w: calendar %q", provider.ErrNotFound, id)
		}
	}
	var all []provider.Event
	for _, rec := range p.events {
		if !contains(cals, rec.ev.CalendarID) {
			continue
		}
		for _, ev := range p.expand(rec, q.Start, q.End) {
			if q.Text == "" || matches(ev, q.Text) {
				all = append(all, ev)
			}
		}
	}
	sort.SliceStable(all, func(i, j int) bool {
		a, b := startInstant(all[i]), startInstant(all[j])
		if !a.Equal(b) {
			return a.Before(b)
		}
		return all[i].ID < all[j].ID
	})
	if offset > len(all) {
		offset = len(all)
	}
	all = all[offset:]
	page := provider.EventPage{}
	limit := q.Limit
	if limit <= 0 {
		limit = 100
	}
	if len(all) > limit {
		all = all[:limit]
		page.NextPageToken = "pt-" + strconv.Itoa(offset+limit)
	}
	page.Events = all
	return page, nil
}

// GetEvent implements provider.Provider.
func (p *Provider) GetEvent(ctx context.Context, ref provider.EventRef) (provider.Event, error) {
	if err := p.check(ctx); err != nil {
		return provider.Event{}, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	rec, occ, ok := p.find(ref)
	if !ok {
		return provider.Event{}, fmt.Errorf("%w: event %q", provider.ErrNotFound, ref.EventID)
	}
	if occ != "" {
		ev, ok := p.occurrence(rec, occ)
		if !ok {
			return provider.Event{}, fmt.Errorf("%w: occurrence %q", provider.ErrNotFound, ref.EventID)
		}
		return ev, nil
	}
	return p.view(rec), nil
}

// find resolves a ref to its record and, for an occurrence id, the occurrence key.
func (p *Provider) find(ref provider.EventRef) (rec *record, occ string, ok bool) {
	if r, found := p.events[ref.EventID]; found && (ref.CalendarID == "" || r.ev.CalendarID == ref.CalendarID) {
		return r, "", true
	}
	if master, _, found := strings.Cut(ref.EventID, "__"); found {
		if r, found := p.events[master]; found && r.ev.Recurrence != nil && (ref.CalendarID == "" || r.ev.CalendarID == ref.CalendarID) {
			return r, ref.EventID, true
		}
	}
	return nil, "", false
}

func (p *Provider) view(r *record) provider.Event {
	ev := cloneEvent(r.ev)
	ev.Version = "v" + strconv.Itoa(r.ver)
	ev.SelfResponse = selfResponse(ev, p.self)
	return ev
}

func selfResponse(ev provider.Event, self string) string {
	for _, a := range ev.Attendees {
		if a.Self || strings.EqualFold(a.Email, self) {
			return a.Response
		}
	}
	return ""
}

// ---- free/busy ----

// FreeBusy implements provider.Provider.
func (p *Provider) FreeBusy(ctx context.Context, q provider.FreeBusyQuery) (provider.FreeBusyResult, error) {
	if err := p.check(ctx); err != nil {
		return provider.FreeBusyResult{}, err
	}
	if q.Start.IsZero() || !q.End.After(q.Start) {
		return provider.FreeBusyResult{}, fmt.Errorf("%w: the window must have an end after its start", provider.ErrInvalidQuery)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(q.Emails) > 0 && !p.caps.FreeBusyOthers {
		return provider.FreeBusyResult{}, fmt.Errorf("%w: free/busy of other people is not available for this account", provider.ErrUnsupportedHere)
	}
	cals := q.CalendarIDs
	if len(cals) == 0 {
		cals = []string{PrimaryID}
	}
	res := provider.FreeBusyResult{Busy: map[string][]provider.Interval{}, Unavailable: map[string]string{}}
	for _, id := range cals {
		if _, ok := p.calendar(id); !ok {
			res.Unavailable[id] = "calendar not found"
			continue
		}
		busy := []provider.Interval{}
		for _, rec := range p.events {
			if rec.ev.CalendarID != id {
				continue
			}
			for _, ev := range p.expand(rec, q.Start, q.End) {
				if ev.Status == provider.StatusCancelled || selfResponse(ev, p.self) == provider.ResponseDeclined {
					continue
				}
				busy = append(busy, provider.Interval{Start: startInstant(ev), End: endInstant(ev)})
			}
		}
		res.Busy[id] = mergeIntervals(busy)
	}
	for _, e := range q.Emails {
		list, ok := p.others[strings.ToLower(e)]
		if !ok {
			res.Unavailable[e] = "no such person"
			continue
		}
		res.Busy[e] = mergeIntervals(clip(list, q.Start, q.End))
	}
	if len(res.Unavailable) == 0 {
		res.Unavailable = nil
	}
	return res, nil
}

func clip(in []provider.Interval, from, to time.Time) []provider.Interval {
	out := []provider.Interval{}
	for _, iv := range in {
		if iv.End.After(from) && iv.Start.Before(to) {
			out = append(out, iv)
		}
	}
	return out
}

func mergeIntervals(in []provider.Interval) []provider.Interval {
	if len(in) == 0 {
		return []provider.Interval{}
	}
	s := append([]provider.Interval(nil), in...)
	sort.Slice(s, func(i, j int) bool { return s[i].Start.Before(s[j].Start) })
	out := []provider.Interval{s[0]}
	for _, iv := range s[1:] {
		last := &out[len(out)-1]
		if !iv.Start.After(last.End) {
			if iv.End.After(last.End) {
				last.End = iv.End
			}
			continue
		}
		out = append(out, iv)
	}
	return out
}

// ---- writing ----

func (p *Provider) writable(calendarID string) error {
	c, ok := p.calendar(calendarID)
	if !ok {
		return fmt.Errorf("%w: calendar %q", provider.ErrNotFound, calendarID)
	}
	if !c.CanWrite() {
		return fmt.Errorf("%w: calendar %q is read-only for this account", provider.ErrRejected, calendarID)
	}
	return nil
}

// CreateEvent implements provider.Provider.
func (p *Provider) CreateEvent(ctx context.Context, calendarID string, in provider.EventInput, opts provider.CreateOptions) (provider.Event, error) {
	if err := p.check(ctx); err != nil {
		return provider.Event{}, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.writable(calendarID); err != nil {
		return provider.Event{}, err
	}
	if opts.DedupeKey != "" {
		if id, ok := p.dedupe[calendarID+"/"+opts.DedupeKey]; ok {
			return p.view(p.events[id]), nil
		}
	}
	if err := provider.ValidateEventInput(in); err != nil {
		return provider.Event{}, err
	}
	if in.Video && !p.caps.Video {
		return provider.Event{}, fmt.Errorf("%w: online meetings are not available for this account", provider.ErrUnsupportedHere)
	}
	p.seq++
	now := p.now().UTC()
	ev := provider.Event{
		ID: "ev" + strconv.Itoa(p.seq), CalendarID: calendarID, Title: in.Title, Description: in.Description, Location: in.Location,
		Start: in.Start, End: in.End, Organizer: provider.Attendee{Email: p.self, Self: true},
		ReminderMinutes: in.ReminderMinutes, Status: provider.StatusConfirmed, Created: now, Updated: now,
	}
	if in.Recurrence != nil {
		r := *in.Recurrence
		ev.Recurrence, ev.SeriesID = &r, ev.ID
	}
	for _, a := range in.Attendees {
		if a.Response == "" {
			a.Response = provider.ResponseNone
		}
		a.Self = strings.EqualFold(a.Email, p.self)
		if a.Self {
			a.Response = provider.ResponseAccepted
		}
		ev.Attendees = append(ev.Attendees, a)
	}
	if in.Video {
		ev.Video, ev.VideoURL = true, "https://meet.example/"+ev.ID
	}
	rec := &record{ev: ev, ver: 1, overrides: map[string]*override{}}
	p.events[ev.ID] = rec
	if opts.DedupeKey != "" {
		p.dedupe[calendarID+"/"+opts.DedupeKey] = ev.ID
	}
	p.notify("invite", ev.ID, others(ev.Attendees, p.self))
	return p.view(rec), nil
}

// FindByDedupeKey implements provider.DedupeFinder.
func (p *Provider) FindByDedupeKey(ctx context.Context, calendarID, key string) (provider.Event, bool, error) {
	if err := p.check(ctx); err != nil {
		return provider.Event{}, false, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	id, ok := p.dedupe[calendarID+"/"+key]
	if !ok {
		return provider.Event{}, false, nil
	}
	rec, ok := p.events[id]
	if !ok {
		return provider.Event{}, false, nil
	}
	return p.view(rec), true, nil
}

func (p *Provider) notify(kind, eventID string, to []string) {
	if len(to) == 0 {
		return
	}
	p.notes = append(p.notes, Notification{Kind: kind, EventID: eventID, To: to})
}

func others(list []provider.Attendee, self string) []string {
	var out []string
	for _, a := range list {
		if !a.Self && !strings.EqualFold(a.Email, self) {
			out = append(out, a.Email)
		}
	}
	return out
}

// UpdateEvent implements provider.Provider.
func (p *Provider) UpdateEvent(ctx context.Context, ref provider.EventRef, scope provider.Scope, patch provider.EventPatch, ifVersion string) (provider.Event, error) {
	if err := p.check(ctx); err != nil {
		return provider.Event{}, err
	}
	if !scope.Valid() {
		return provider.Event{}, fmt.Errorf("%w: scope must be instance or series", provider.ErrRejected)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	rec, occ, ok := p.find(ref)
	if !ok {
		return provider.Event{}, fmt.Errorf("%w: event %q", provider.ErrNotFound, ref.EventID)
	}
	if err := p.writable(rec.ev.CalendarID); err != nil {
		return provider.Event{}, err
	}
	// The precondition is on the event the ref names, as the caller last read it.
	var current provider.Event
	if occ != "" {
		var ok bool
		if current, ok = p.occurrence(rec, occ); !ok {
			return provider.Event{}, fmt.Errorf("%w: occurrence %q", provider.ErrNotFound, ref.EventID)
		}
	} else {
		current = p.view(rec)
	}
	if ifVersion == "" || current.Version != ifVersion {
		return provider.Event{}, fmt.Errorf("%w: have %s, caller read %q", provider.ErrPreconditionFailed, current.Version, ifVersion)
	}
	if patch.Video != nil && *patch.Video && !p.caps.Video {
		return provider.Event{}, fmt.Errorf("%w: online meetings are not available for this account", provider.ErrUnsupportedHere)
	}
	if patch.Recurrence != nil {
		if err := patch.Recurrence.Validate(); err != nil {
			return provider.Event{}, err
		}
	}
	notifyBefore := others(current.Attendees, p.self)

	target := &rec.ev
	if occ != "" && scope == provider.ScopeInstance {
		if patch.Recurrence != nil || patch.ClearRecurrence {
			return provider.Event{}, fmt.Errorf("%w: an occurrence cannot change its series' recurrence", provider.ErrInvalidRecurrence)
		}
		ov := rec.overrides[occ]
		if ov == nil {
			ov = &override{ev: cloneEvent(current), ver: 1}
			ov.ev.Version = ""
			rec.overrides[occ] = ov
		}
		if err := applyPatch(&ov.ev, patch, p.self); err != nil {
			return provider.Event{}, err
		}
		ov.ver++
		ov.ev.Updated = p.now().UTC()
		out, _ := p.occurrence(rec, occ)
		p.notify("update", occ, union(notifyBefore, others(out.Attendees, p.self)))
		return out, nil
	}
	if err := applyPatch(target, patch, p.self); err != nil {
		return provider.Event{}, err
	}
	rec.ver++
	target.Updated = p.now().UTC()
	out := p.view(rec)
	if occ != "" {
		out, _ = p.occurrence(rec, occ)
	}
	p.notify("update", rec.ev.ID, union(notifyBefore, others(rec.ev.Attendees, p.self)))
	return out, nil
}

func applyPatch(ev *provider.Event, patch provider.EventPatch, self string) error {
	next := cloneEvent(*ev)
	if patch.Title != nil {
		next.Title = *patch.Title
	}
	if patch.Description != nil {
		next.Description = *patch.Description
	}
	if patch.Location != nil {
		next.Location = *patch.Location
	}
	if patch.Start != nil {
		next.Start = *patch.Start
	}
	if patch.End != nil {
		next.End = *patch.End
	}
	if err := provider.ValidateEventInput(provider.EventInput{Start: next.Start, End: next.End, Recurrence: nil, Attendees: attendeesOf(patch, next)}); err != nil {
		return err
	}
	switch {
	case patch.ClearRecurrence:
		next.Recurrence = nil
	case patch.Recurrence != nil:
		r := *patch.Recurrence
		next.Recurrence = &r
		if next.SeriesID == "" {
			next.SeriesID = next.ID
		}
	}
	if patch.Attendees != nil {
		old := map[string]provider.Attendee{}
		for _, a := range ev.Attendees {
			old[strings.ToLower(a.Email)] = a
		}
		next.Attendees = nil
		for _, a := range *patch.Attendees {
			if prev, ok := old[strings.ToLower(a.Email)]; ok && a.Response == "" {
				a.Response = prev.Response
			}
			if a.Response == "" {
				a.Response = provider.ResponseNone
			}
			a.Self = strings.EqualFold(a.Email, self)
			next.Attendees = append(next.Attendees, a)
		}
	}
	if patch.Video != nil {
		next.Video = *patch.Video
		next.VideoURL = ""
		if next.Video {
			next.VideoURL = "https://meet.example/" + next.ID
		}
	}
	if patch.ReminderMinutes != nil {
		v := *patch.ReminderMinutes
		next.ReminderMinutes = &v
	}
	*ev = next
	return nil
}

func attendeesOf(patch provider.EventPatch, next provider.Event) []provider.Attendee {
	if patch.Attendees != nil {
		return *patch.Attendees
	}
	return next.Attendees
}

func union(a, b []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, l := range [][]string{a, b} {
		for _, s := range l {
			if k := strings.ToLower(s); !seen[k] {
				seen[k] = true
				out = append(out, s)
			}
		}
	}
	return out
}

// DeleteEvent implements provider.Provider.
func (p *Provider) DeleteEvent(ctx context.Context, ref provider.EventRef, scope provider.Scope, ifVersion string) error {
	if err := p.check(ctx); err != nil {
		return err
	}
	if !scope.Valid() {
		return fmt.Errorf("%w: scope must be instance or series", provider.ErrRejected)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	rec, occ, ok := p.find(ref)
	if !ok {
		return fmt.Errorf("%w: event %q", provider.ErrNotFound, ref.EventID)
	}
	if err := p.writable(rec.ev.CalendarID); err != nil {
		return err
	}
	var current provider.Event
	if occ != "" {
		var ok bool
		if current, ok = p.occurrence(rec, occ); !ok {
			return fmt.Errorf("%w: occurrence %q", provider.ErrNotFound, ref.EventID)
		}
	} else {
		current = p.view(rec)
	}
	if ifVersion == "" || current.Version != ifVersion {
		return fmt.Errorf("%w: have %s, caller read %q", provider.ErrPreconditionFailed, current.Version, ifVersion)
	}
	to := others(current.Attendees, p.self)
	if occ != "" && scope == provider.ScopeInstance {
		ov := rec.overrides[occ]
		if ov == nil {
			ov = &override{ev: cloneEvent(current), ver: 1}
			rec.overrides[occ] = ov
		}
		ov.cancelled = true
		p.notify("cancel", occ, to)
		return nil
	}
	delete(p.events, rec.ev.ID)
	for k, id := range p.dedupe {
		if id == rec.ev.ID {
			delete(p.dedupe, k)
		}
	}
	p.notify("cancel", rec.ev.ID, to)
	return nil
}

// Respond implements provider.Provider.
func (p *Provider) Respond(ctx context.Context, ref provider.EventRef, resp provider.Response, comment string) error {
	if err := p.check(ctx); err != nil {
		return err
	}
	if !resp.Valid() {
		return fmt.Errorf("%w: response must be accept, tentative or decline", provider.ErrRejected)
	}
	if len([]rune(comment)) > provider.MaxResponseComment {
		return fmt.Errorf("%w: comment is over %d characters", provider.ErrRejected, provider.MaxResponseComment)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	rec, occ, ok := p.find(ref)
	if !ok {
		return fmt.Errorf("%w: event %q", provider.ErrNotFound, ref.EventID)
	}
	status := map[provider.Response]string{
		provider.RespondAccept: provider.ResponseAccepted, provider.RespondTentative: provider.ResponseTentative, provider.RespondDecline: provider.ResponseDeclined,
	}[resp]
	set := func(ev *provider.Event) bool {
		for i := range ev.Attendees {
			if ev.Attendees[i].Self || strings.EqualFold(ev.Attendees[i].Email, p.self) {
				ev.Attendees[i].Response = status
				return true
			}
		}
		return false
	}
	if occ != "" {
		cur, ok := p.occurrence(rec, occ)
		if !ok {
			return fmt.Errorf("%w: occurrence %q", provider.ErrNotFound, ref.EventID)
		}
		if !set(&cur) {
			return fmt.Errorf("%w: the account is not an attendee of this event", provider.ErrRejected)
		}
		ov := rec.overrides[occ]
		if ov == nil {
			ov = &override{ver: 1}
			rec.overrides[occ] = ov
		}
		cur.Version = ""
		ov.ev, ov.ver = cur, ov.ver+1
	} else {
		if !set(&rec.ev) {
			return fmt.Errorf("%w: the account is not an attendee of this event", provider.ErrRejected)
		}
		rec.ver++
	}
	p.notify("rsvp", ref.EventID, []string{rec.ev.Organizer.Email})
	return nil
}

// ---- helpers ----

func cloneEvent(e provider.Event) provider.Event {
	c := e
	c.Attendees = append([]provider.Attendee(nil), e.Attendees...)
	if e.Recurrence != nil {
		r := *e.Recurrence
		r.ByDay = append([]string(nil), e.Recurrence.ByDay...)
		c.Recurrence = &r
	}
	if e.OriginalStart != nil {
		o := *e.OriginalStart
		c.OriginalStart = &o
	}
	if e.ReminderMinutes != nil {
		v := *e.ReminderMinutes
		c.ReminderMinutes = &v
	}
	return c
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func matches(ev provider.Event, text string) bool {
	hay := strings.ToLower(ev.Title + "\n" + ev.Description + "\n" + ev.Location)
	for _, a := range ev.Attendees {
		hay += "\n" + strings.ToLower(a.Email+" "+a.Name)
	}
	for _, term := range strings.Fields(strings.ToLower(text)) {
		if !strings.Contains(hay, term) {
			return false
		}
	}
	return true
}

func startInstant(e provider.Event) time.Time {
	if e.Start.IsAllDay() {
		t, _ := time.Parse(provider.DateLayout, e.Start.Date)
		return t
	}
	return e.Start.DateTime
}

func endInstant(e provider.Event) time.Time {
	if e.End.IsAllDay() {
		t, _ := time.Parse(provider.DateLayout, e.End.Date)
		return t
	}
	return e.End.DateTime
}
