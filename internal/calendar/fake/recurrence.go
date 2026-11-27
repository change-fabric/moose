package fake

import (
	"strconv"
	"time"

	"github.com/change-fabric/moose/internal/calendar/provider"
)

// maxScan bounds how many days an expansion walks, so a rule that never ends
// cannot spin.
const maxScan = 20000

var weekdayCode = map[time.Weekday]string{
	time.Monday: "MO", time.Tuesday: "TU", time.Wednesday: "WE", time.Thursday: "TH",
	time.Friday: "FR", time.Saturday: "SA", time.Sunday: "SU",
}

func stamp(start provider.EventTime) string {
	if start.IsAllDay() {
		t, _ := time.Parse(provider.DateLayout, start.Date)
		return t.Format("20060102")
	}
	return start.DateTime.UTC().Format("20060102T150405Z")
}

func daysBetween(a, b time.Time) int {
	a = time.Date(a.Year(), a.Month(), a.Day(), 0, 0, 0, 0, time.UTC)
	b = time.Date(b.Year(), b.Month(), b.Day(), 0, 0, 0, 0, time.UTC)
	return int(b.Sub(a).Hours() / 24)
}

func weekStart(t time.Time) time.Time {
	d := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
	return d.AddDate(0, 0, -((int(d.Weekday()) + 6) % 7))
}

// occurrence is one generated occurrence of a series.
type occurrence struct {
	start, end provider.EventTime
	startAt    time.Time // instant, for ordering and window checks
}

// generate walks the series' occurrences in order up to limit and calls fn with
// each; fn returns false to stop. The rule is applied in the master's own zone,
// so a weekly 09:00 meeting stays at 09:00 across a daylight-saving change.
func generate(master provider.Event, limit time.Time, fn func(occurrence) bool) {
	r := master.Recurrence
	if r == nil {
		return
	}
	interval := r.Interval
	if interval < 1 {
		interval = 1
	}
	allDay := master.Start.IsAllDay()
	loc := time.UTC
	zone := master.Start.Zone
	var first time.Time
	var dur time.Duration
	if allDay {
		first, _ = time.Parse(provider.DateLayout, master.Start.Date)
		end, _ := time.Parse(provider.DateLayout, master.End.Date)
		dur = end.Sub(first)
	} else {
		var err error
		if loc, err = provider.LoadZone(zone); err != nil {
			return
		}
		first = master.Start.DateTime.In(loc)
		dur = master.End.DateTime.Sub(master.Start.DateTime)
	}
	var until time.Time
	if r.Until != "" {
		until, _ = time.Parse(provider.DateLayout, r.Until)
	}
	byDay := map[string]bool{}
	for _, d := range r.ByDay {
		byDay[d] = true
	}
	count := 0
	for i := 0; i < maxScan; i++ {
		day := first.AddDate(0, 0, i)
		at := time.Date(day.Year(), day.Month(), day.Day(), first.Hour(), first.Minute(), first.Second(), first.Nanosecond(), loc)
		if at.After(limit) {
			return
		}
		if !until.IsZero() && time.Date(day.Year(), day.Month(), day.Day(), 0, 0, 0, 0, time.UTC).After(until) {
			return
		}
		ok := false
		switch r.Freq {
		case provider.FreqDaily:
			ok = i%interval == 0
		case provider.FreqWeekly:
			weeks := daysBetween(weekStart(first), weekStart(day)) / 7
			want := weekdayCode[first.Weekday()]
			if len(byDay) > 0 {
				ok = byDay[weekdayCode[day.Weekday()]]
			} else {
				ok = weekdayCode[day.Weekday()] == want
			}
			ok = ok && weeks%interval == 0
		case provider.FreqMonthly:
			months := (day.Year()-first.Year())*12 + int(day.Month()) - int(first.Month())
			ok = day.Day() == first.Day() && months%interval == 0
		case provider.FreqYearly:
			ok = day.Month() == first.Month() && day.Day() == first.Day() && (day.Year()-first.Year())%interval == 0
		}
		if !ok {
			continue
		}
		count++
		if r.Count > 0 && count > r.Count {
			return
		}
		var occ occurrence
		if allDay {
			occ = occurrence{
				start:   provider.EventTime{Date: at.Format(provider.DateLayout)},
				end:     provider.EventTime{Date: at.Add(dur).Format(provider.DateLayout)},
				startAt: at,
			}
		} else {
			occ = occurrence{
				start:   provider.EventTime{DateTime: at, Zone: zone},
				end:     provider.EventTime{DateTime: at.Add(dur).In(loc), Zone: zone},
				startAt: at,
			}
		}
		if !fn(occ) {
			return
		}
	}
}

func overlaps(ev provider.Event, from, to time.Time) bool {
	return endInstant(ev).After(from) && startInstant(ev).Before(to)
}

// expand returns the events of rec that overlap [from, to): the event itself,
// or each live occurrence of a series.
func (p *Provider) expand(rec *record, from, to time.Time) []provider.Event {
	if rec.ev.Recurrence == nil {
		if ev := p.view(rec); overlaps(ev, from, to) {
			return []provider.Event{ev}
		}
		return nil
	}
	var out []provider.Event
	generate(rec.ev, to, func(o occurrence) bool {
		id := rec.ev.ID + "__" + stamp(o.start)
		ev, ok := p.build(rec, id, o)
		if ok && overlaps(ev, from, to) {
			out = append(out, ev)
		}
		return true
	})
	return out
}

// build makes the event for occurrence o of rec, or reports it cancelled.
func (p *Provider) build(rec *record, id string, o occurrence) (provider.Event, bool) {
	if ov := rec.overrides[id]; ov != nil {
		if ov.cancelled {
			return provider.Event{}, false
		}
		ev := cloneEvent(ov.ev)
		ev.ID, ev.CalendarID, ev.SeriesID = id, rec.ev.CalendarID, rec.ev.ID
		ev.Version = "v" + strconv.Itoa(rec.ver) + "." + strconv.Itoa(ov.ver)
		ev.SelfResponse = selfResponse(ev, p.self)
		orig := o.start
		ev.OriginalStart = &orig
		return ev, true
	}
	ev := cloneEvent(rec.ev)
	ev.ID, ev.SeriesID = id, rec.ev.ID
	ev.Start, ev.End = o.start, o.end
	orig := o.start
	ev.OriginalStart = &orig
	ev.Version = "v" + strconv.Itoa(rec.ver) + ".0"
	ev.SelfResponse = selfResponse(ev, p.self)
	return ev, true
}

// occurrence returns the occurrence of rec named by id, if the series really has it.
func (p *Provider) occurrence(rec *record, id string) (provider.Event, bool) {
	var found provider.Event
	var ok bool
	// Walk until the stamp is passed; the series is ordered.
	limit := time.Now().AddDate(30, 0, 0)
	generate(rec.ev, limit, func(o occurrence) bool {
		if rec.ev.ID+"__"+stamp(o.start) != id {
			return true
		}
		found, ok = p.build(rec, id, o)
		return false
	})
	return found, ok
}
