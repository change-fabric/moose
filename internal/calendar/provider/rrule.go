package provider

import (
	"sort"
	"strconv"
	"strings"
	"time"
)

// ParseRRULE reads the recurrence lines of a Google event ("RRULE:FREQ=...",
// "EXDATE:...") into the neutral subset. A rule that fits is returned with
// Freq, Interval, ByDay, Until and Count set. Anything else (several rules,
// EXDATE or RDATE lines, BYMONTHDAY, BYSETPOS, a numbered BYDAY such as 2MO, a
// WKST other than Monday, an UNTIL that cannot be read) comes back as Native
// alone: the lines joined by newlines, opaque to callers.
//
// zone is the event's own zone. A timed series ends at an UNTIL instant, which
// the neutral form states as the last day in that zone.
func ParseRRULE(lines []string, zone *time.Location) Recurrence {
	native := Recurrence{Native: strings.Join(lines, "\n")}
	var rule string
	for _, l := range lines {
		l = strings.TrimSpace(l)
		if l == "" {
			continue
		}
		name, val, ok := strings.Cut(l, ":")
		if !ok || strings.ToUpper(name) != "RRULE" || rule != "" {
			return native
		}
		rule = val
	}
	if rule == "" {
		return native
	}
	var r Recurrence
	for _, part := range strings.Split(rule, ";") {
		k, v, ok := strings.Cut(part, "=")
		if !ok {
			return native
		}
		switch strings.ToUpper(k) {
		case "FREQ":
			r.Freq = strings.ToUpper(v)
		case "INTERVAL":
			n, err := strconv.Atoi(v)
			if err != nil || n < 1 {
				return native
			}
			if n > 1 {
				r.Interval = n
			}
		case "COUNT":
			n, err := strconv.Atoi(v)
			if err != nil || n < 1 {
				return native
			}
			r.Count = n
		case "UNTIL":
			d, ok := untilDate(v, zone)
			if !ok {
				return native
			}
			r.Until = d
		case "BYDAY":
			for _, d := range strings.Split(strings.ToUpper(v), ",") {
				if !isWeekday(d) {
					return native
				}
				r.ByDay = append(r.ByDay, d)
			}
		case "WKST":
			if strings.ToUpper(v) != "MO" {
				return native
			}
		default:
			return native
		}
	}
	if r.Validate() != nil {
		return native
	}
	return r
}

func untilDate(v string, zone *time.Location) (string, bool) {
	if t, err := time.Parse("20060102", v); err == nil {
		return t.Format(DateLayout), true
	}
	for _, layout := range []string{"20060102T150405Z", "20060102T150405"} {
		if t, err := time.Parse(layout, v); err == nil {
			if zone == nil {
				zone = time.UTC
			}
			return t.In(zone).Format(DateLayout), true
		}
	}
	return "", false
}

// MergeIntervals returns in with overlapping and touching intervals joined, in
// start order. The input is not modified.
func MergeIntervals(in []Interval) []Interval {
	if len(in) == 0 {
		return nil
	}
	s := append([]Interval(nil), in...)
	sort.SliceStable(s, func(i, j int) bool { return s[i].Start.Before(s[j].Start) })
	out := []Interval{s[0]}
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
