package provider_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/change-fabric/moose/internal/calendar/provider"
)

func TestParseRRULENeutralSubset(t *testing.T) {
	berlin, _ := time.LoadLocation("Europe/Berlin")
	for _, tc := range []struct {
		name  string
		lines []string
		want  provider.Recurrence
	}{
		{"daily", []string{"RRULE:FREQ=DAILY"}, provider.Recurrence{Freq: "DAILY"}},
		{"interval", []string{"RRULE:FREQ=WEEKLY;INTERVAL=2;BYDAY=MO,WE"}, provider.Recurrence{Freq: "WEEKLY", Interval: 2, ByDay: []string{"MO", "WE"}}},
		{"count", []string{"RRULE:FREQ=MONTHLY;COUNT=6"}, provider.Recurrence{Freq: "MONTHLY", Count: 6}},
		{"until date", []string{"RRULE:FREQ=YEARLY;UNTIL=20300101"}, provider.Recurrence{Freq: "YEARLY", Until: "2030-01-01"}},
		{"interval one is the default", []string{"RRULE:FREQ=DAILY;INTERVAL=1"}, provider.Recurrence{Freq: "DAILY"}},
		{"monday week start is the default", []string{"RRULE:FREQ=WEEKLY;WKST=MO;BYDAY=FR"}, provider.Recurrence{Freq: "WEEKLY", ByDay: []string{"FR"}}},
		// 21:59:59Z on 31 March is 23:59:59 in Berlin (UTC+2 after the 29 March change): the last local day is the 31st.
		{"until instant becomes the local last day", []string{"RRULE:FREQ=DAILY;UNTIL=20260331T215959Z"}, provider.Recurrence{Freq: "DAILY", Until: "2026-03-31"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := provider.ParseRRULE(tc.lines, berlin)
			if got.Native != "" || got.Freq != tc.want.Freq || got.Interval != tc.want.Interval || got.Until != tc.want.Until ||
				got.Count != tc.want.Count || strings.Join(got.ByDay, ",") != strings.Join(tc.want.ByDay, ",") {
				t.Errorf("ParseRRULE(%v) = %+v, want %+v", tc.lines, got, tc.want)
			}
		})
	}
}

func TestParseRRULEOutsideTheSubsetIsNative(t *testing.T) {
	for name, lines := range map[string][]string{
		"numbered weekday":  {"RRULE:FREQ=MONTHLY;BYDAY=2TU"},
		"month day":         {"RRULE:FREQ=MONTHLY;BYMONTHDAY=15"},
		"set position":      {"RRULE:FREQ=MONTHLY;BYDAY=MO;BYSETPOS=-1"},
		"excluded date":     {"RRULE:FREQ=DAILY", "EXDATE:20260325T080000Z"},
		"two rules":         {"RRULE:FREQ=DAILY", "RRULE:FREQ=WEEKLY"},
		"other week start":  {"RRULE:FREQ=WEEKLY;WKST=SU;BYDAY=MO"},
		"hourly":            {"RRULE:FREQ=HOURLY"},
		"until and count":   {"RRULE:FREQ=DAILY;COUNT=3;UNTIL=20300101"},
		"bad until":         {"RRULE:FREQ=DAILY;UNTIL=soon"},
		"by day on daily":   {"RRULE:FREQ=DAILY;BYDAY=MO"},
		"only an exdate":    {"EXDATE:20260325T080000Z"},
		"not a rule":        {"nonsense"},
		"unknown parameter": {"RRULE:FREQ=DAILY;BYHOUR=9"},
		"negative interval": {"RRULE:FREQ=DAILY;INTERVAL=0"},
		"huge count":        {"RRULE:FREQ=DAILY;COUNT=100000"},
	} {
		t.Run(name, func(t *testing.T) {
			got := provider.ParseRRULE(lines, time.UTC)
			if !got.IsOpaque() || got.Native != strings.Join(lines, "\n") {
				t.Errorf("ParseRRULE(%v) = %+v, want the lines as an opaque native rule", lines, got)
			}
			if got.Validate() == nil {
				t.Error("a native rule must not validate for writing")
			}
		})
	}
}

func iv(h1, h2 int) provider.Interval {
	return provider.Interval{Start: time.Date(2026, 3, 4, h1, 0, 0, 0, time.UTC), End: time.Date(2026, 3, 4, h2, 0, 0, 0, time.UTC)}
}

func TestMergeIntervals(t *testing.T) {
	in := []provider.Interval{iv(13, 14), iv(9, 11), iv(10, 12), iv(12, 13), iv(20, 21)}
	got := provider.MergeIntervals(in)
	if len(got) != 2 || !got[0].Start.Equal(iv(9, 14).Start) || !got[0].End.Equal(iv(9, 14).End) || !got[1].Start.Equal(iv(20, 21).Start) {
		t.Errorf("merged = %+v, want 09-14 (touching joins) and 20-21", got)
	}
	if in[0] != iv(13, 14) {
		t.Error("the input was reordered")
	}
	if provider.MergeIntervals(nil) != nil {
		t.Error("merging nothing is nothing")
	}
}

func ev(id string, h int) provider.Event {
	return provider.Event{ID: id, Start: provider.EventTime{DateTime: time.Date(2026, 3, 4, h, 0, 0, 0, time.UTC), Zone: "UTC"}}
}

func TestListMergedOrdersPagesAndRejectsBadTokens(t *testing.T) {
	cals := map[string][]provider.Event{
		"a": {ev("a1", 8), ev("a2", 10), ev("a3", 12)},
		"b": {ev("b1", 9), ev("b2", 11)},
	}
	calls := 0
	fetch := func(_ context.Context, id string, n int) ([]provider.Event, error) {
		calls++
		l := cals[id]
		return l[:min(n, len(l))], nil
	}
	var seen []string
	token := ""
	for range 10 {
		page, err := provider.ListMerged(t.Context(), []string{"a", "b"}, 2, token, fetch)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range page.Events {
			seen = append(seen, e.ID)
		}
		if token = page.NextPageToken; token == "" {
			break
		}
	}
	if strings.Join(seen, ",") != "a1,b1,a2,b2,a3" {
		t.Errorf("merged order = %v", seen)
	}
	for name, tok := range map[string]string{"garbage": "x", "no prefix": "eyJvIjoxfQ", "bad base64": "m:***", "negative": "m:" + "eyJvIjotMX0"} {
		if _, err := provider.ListMerged(t.Context(), []string{"a", "b"}, 2, tok, fetch); !errors.Is(err, provider.ErrInvalidQuery) {
			t.Errorf("%s token err = %v, want ErrInvalidQuery", name, err)
		}
	}
	// A page deep into a long listing is refused, not walked.
	big := func(_ context.Context, id string, n int) ([]provider.Event, error) {
		return nil, errors.New("must not be called")
	}
	if _, err := provider.ListMerged(t.Context(), []string{"a", "b"}, 250, "m:eyJvIjo0MDB9", big); !errors.Is(err, provider.ErrInvalidQuery) {
		t.Errorf("deep page err = %v, want ErrInvalidQuery", err)
	}
	// A failing calendar fails the page.
	boom := errors.New("boom")
	if _, err := provider.ListMerged(t.Context(), []string{"a"}, 2, "", func(context.Context, string, int) ([]provider.Event, error) { return nil, boom }); !errors.Is(err, boom) {
		t.Errorf("fetch error = %v", err)
	}
}

func TestInZoneConvertsTimedValuesOnly(t *testing.T) {
	orig := provider.EventTime{DateTime: time.Date(2026, 3, 4, 8, 0, 0, 0, time.UTC), Zone: "UTC"}
	e := provider.Event{Start: orig, End: orig, OriginalStart: &orig}
	got := provider.InZone(e, "Asia/Tokyo")
	if got.Start.Zone != "Asia/Tokyo" || got.Start.DateTime.Hour() != 17 || !got.Start.DateTime.Equal(orig.DateTime) || got.OriginalStart.Zone != "Asia/Tokyo" {
		t.Errorf("converted = %+v", got)
	}
	if e.OriginalStart.Zone != "UTC" {
		t.Error("the original event was modified")
	}
	day := provider.Event{Start: provider.EventTime{Date: "2026-03-04"}, End: provider.EventTime{Date: "2026-03-05"}}
	if got := provider.InZone(day, "Asia/Tokyo"); got.Start.Date != "2026-03-04" || got.Start.Zone != "" {
		t.Errorf("an all-day event was changed: %+v", got)
	}
	if got := provider.InZone(e, "EST"); got.Start.Zone != "UTC" {
		t.Errorf("an invalid zone changed the event: %+v", got)
	}
}
