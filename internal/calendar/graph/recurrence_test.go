package graph

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/change-fabric/moose/internal/calendar/provider"
)

func pr(pattern, rng string) json.RawMessage {
	return json.RawMessage(`{"pattern":` + pattern + `,"range":` + rng + `}`)
}

func TestRecurrenceOfNeutralSubset(t *testing.T) {
	start := provider.EventTime{DateTime: time.Date(2026, 3, 23, 9, 0, 0, 0, time.UTC), Zone: "UTC"} // a Monday, the 23rd
	for _, tc := range []struct {
		name string
		raw  json.RawMessage
		want provider.Recurrence
	}{
		{"daily no end", pr(`{"type":"daily","interval":1}`, `{"type":"noEnd","startDate":"2026-03-23"}`), provider.Recurrence{Freq: "DAILY"}},
		{"every other week two days until", pr(`{"type":"weekly","interval":2,"daysOfWeek":["monday","wednesday"],"firstDayOfWeek":"monday"}`, `{"type":"endDate","endDate":"2026-06-30"}`),
			provider.Recurrence{Freq: "WEEKLY", Interval: 2, ByDay: []string{"MO", "WE"}, Until: "2026-06-30"}},
		{"weekly numbered", pr(`{"type":"weekly","interval":1,"daysOfWeek":["friday"],"firstDayOfWeek":"sunday"}`, `{"type":"numbered","numberOfOccurrences":6}`),
			provider.Recurrence{Freq: "WEEKLY", ByDay: []string{"FR"}, Count: 6}},
		{"weekly with a sunday week start is fine for interval one", pr(`{"type":"weekly","interval":1,"daysOfWeek":["monday","tuesday"],"firstDayOfWeek":"sunday"}`, `{"type":"noEnd"}`),
			provider.Recurrence{Freq: "WEEKLY", ByDay: []string{"MO", "TU"}}},
		{"monthly on the start day", pr(`{"type":"absoluteMonthly","interval":3,"dayOfMonth":23}`, `{"type":"noEnd"}`), provider.Recurrence{Freq: "MONTHLY", Interval: 3}},
		{"yearly on the start date", pr(`{"type":"absoluteYearly","interval":1,"dayOfMonth":23,"month":3}`, `{"type":"numbered","numberOfOccurrences":4}`), provider.Recurrence{Freq: "YEARLY", Count: 4}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := recurrenceOf(tc.raw, start)
			if got.Native != "" || got.Freq != tc.want.Freq || got.Interval != tc.want.Interval || got.Until != tc.want.Until ||
				got.Count != tc.want.Count || strings.Join(got.ByDay, ",") != strings.Join(tc.want.ByDay, ",") {
				t.Errorf("recurrenceOf = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestRecurrenceOfOutsideTheSubsetIsTheGraphJSON(t *testing.T) {
	start := provider.EventTime{DateTime: time.Date(2026, 3, 23, 9, 0, 0, 0, time.UTC), Zone: "UTC"}
	for name, raw := range map[string]json.RawMessage{
		"second tuesday":           pr(`{"type":"relativeMonthly","interval":1,"daysOfWeek":["tuesday"],"index":"second"}`, `{"type":"noEnd"}`),
		"monthly on another day":   pr(`{"type":"absoluteMonthly","interval":1,"dayOfMonth":15}`, `{"type":"noEnd"}`),
		"yearly on another month":  pr(`{"type":"absoluteYearly","interval":1,"dayOfMonth":23,"month":4}`, `{"type":"noEnd"}`),
		"relative yearly":          pr(`{"type":"relativeYearly","interval":1,"daysOfWeek":["monday"],"index":"first","month":3}`, `{"type":"noEnd"}`),
		"two weekdays, sunday wk":  pr(`{"type":"weekly","interval":2,"daysOfWeek":["monday","wednesday"],"firstDayOfWeek":"sunday"}`, `{"type":"noEnd"}`),
		"unknown pattern":          pr(`{"type":"fortnightlyish","interval":1}`, `{"type":"noEnd"}`),
		"unknown range":            pr(`{"type":"daily","interval":1}`, `{"type":"whenever"}`),
		"unknown day":              pr(`{"type":"weekly","interval":1,"daysOfWeek":["someday"]}`, `{"type":"noEnd"}`),
		"numbered without a count": pr(`{"type":"daily","interval":1}`, `{"type":"numbered","numberOfOccurrences":0}`),
		"not json":                 json.RawMessage(`"x"`),
	} {
		t.Run(name, func(t *testing.T) {
			got := recurrenceOf(raw, start)
			if !got.IsOpaque() {
				t.Fatalf("recurrenceOf = %+v, want an opaque native rule", got)
			}
			if !json.Valid([]byte(got.Native)) || strings.ContainsAny(got.Native, "\n") {
				t.Errorf("native = %q, want compact JSON", got.Native)
			}
			if got.Validate() == nil {
				t.Error("a native rule must not validate for writing")
			}
		})
	}
}

func TestGraphTimeAndZoneNames(t *testing.T) {
	for _, tc := range []struct {
		in   dateTimeZone
		want time.Time
		zone string
	}{
		{dateTimeZone{"2026-03-04T09:00:00.0000000", "UTC"}, time.Date(2026, 3, 4, 9, 0, 0, 0, time.UTC), "UTC"},
		{dateTimeZone{"2026-03-04T09:00:00.0000000", "Europe/Berlin"}, time.Date(2026, 3, 4, 8, 0, 0, 0, time.UTC), "Europe/Berlin"},
		{dateTimeZone{"2026-03-04T09:00:00", "W. Europe Standard Time"}, time.Date(2026, 3, 4, 8, 0, 0, 0, time.UTC), "Europe/Berlin"},
		{dateTimeZone{"2026-03-04T09:00:00.0000000", ""}, time.Date(2026, 3, 4, 9, 0, 0, 0, time.UTC), "UTC"},
		{dateTimeZone{"2026-03-04T09:00:00.0000000", "Custom Zone"}, time.Date(2026, 3, 4, 9, 0, 0, 0, time.UTC), "UTC"},
	} {
		got, zone, err := parseGraphTime(tc.in)
		if err != nil || !got.Equal(tc.want) || zone != tc.zone {
			t.Errorf("parseGraphTime(%+v) = %v %q %v, want %v %q", tc.in, got, zone, err, tc.want, tc.zone)
		}
	}
	if _, _, err := parseGraphTime(dateTimeZone{"yesterday", "UTC"}); err == nil {
		t.Error("a bad time was accepted")
	}
	for name, iana := range windowsZones {
		if _, err := provider.LoadZone(iana); err != nil {
			t.Errorf("windows zone %q maps to %q, which is not a loadable IANA zone", name, iana)
		}
	}
	for in, want := range map[string]string{"Europe/Paris": "Europe/Paris", "Pacific Standard Time": "America/Los_Angeles", "": "UTC", "tzone://Microsoft/Custom": "UTC"} {
		if got := eventZone(in); got != want {
			t.Errorf("eventZone(%q) = %q, want %q", in, got, want)
		}
	}
}
