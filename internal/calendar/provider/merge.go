package provider

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

// DefaultPageSize is the page size a provider uses when the query has none.
const DefaultPageSize = 25

// MaxMergedWindow bounds how many events one merged multi-calendar page reads
// from each calendar: the offset of the page plus its size. Past it the call is
// refused rather than walking an unbounded series of provider pages.
const MaxMergedWindow = 500

// PageFetcher returns the first n events of one calendar for the query, in
// start order (following the provider's own pages as needed).
type PageFetcher func(ctx context.Context, calendarID string, n int) ([]Event, error)

type mergedToken struct {
	Offset int `json:"o"`
}

const mergedPrefix = "m:"

// ListMerged serves one page of several calendars at once. A provider's own
// page token follows one calendar; for several, the page is the events after
// the token's offset in the start-ordered union, so the order is exact and a
// token is a small number. It reads offset+limit events from every calendar,
// which is why MaxMergedWindow caps it.
func ListMerged(ctx context.Context, ids []string, limit int, token string, fetch PageFetcher) (EventPage, error) {
	if limit <= 0 {
		limit = DefaultPageSize
	}
	offset := 0
	if token != "" {
		raw, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(token, mergedPrefix))
		var t mergedToken
		if !strings.HasPrefix(token, mergedPrefix) || err != nil || json.Unmarshal(raw, &t) != nil || t.Offset < 0 {
			return EventPage{}, fmt.Errorf("%w: page token is not valid", ErrInvalidQuery)
		}
		offset = t.Offset
	}
	need := offset + limit + 1
	if need > MaxMergedWindow {
		return EventPage{}, fmt.Errorf("%w: too far into a multi-calendar listing; narrow the window or list one calendar", ErrInvalidQuery)
	}
	var all []Event
	for _, id := range ids {
		evs, err := fetch(ctx, id, need)
		if err != nil {
			return EventPage{}, err
		}
		all = append(all, evs...)
	}
	sort.SliceStable(all, func(i, j int) bool { return startInstant(all[i]).Before(startInstant(all[j])) })
	if offset > len(all) {
		offset = len(all)
	}
	all = all[offset:]
	page := EventPage{Events: all}
	if len(all) > limit {
		page.Events = all[:limit]
		raw, _ := json.Marshal(mergedToken{Offset: offset + limit})
		page.NextPageToken = mergedPrefix + base64.RawURLEncoding.EncodeToString(raw)
	}
	if page.Events == nil {
		page.Events = []Event{}
	}
	return page, nil
}

func startInstant(e Event) time.Time {
	if e.Start.IsAllDay() {
		t, _ := time.Parse(DateLayout, e.Start.Date)
		return t
	}
	return e.Start.DateTime
}

// InZone returns ev with its timed start, end and original start shown in the
// IANA zone name. All-day values are unchanged. An unknown zone leaves ev as it
// is; callers validate the zone first.
func InZone(ev Event, zone string) Event {
	loc, err := LoadZone(zone)
	if err != nil {
		return ev
	}
	conv := func(t EventTime) EventTime {
		if t.IsAllDay() || t.DateTime.IsZero() {
			return t
		}
		return EventTime{DateTime: t.DateTime.In(loc), Zone: zone}
	}
	ev.Start, ev.End = conv(ev.Start), conv(ev.End)
	if ev.OriginalStart != nil {
		o := conv(*ev.OriginalStart)
		ev.OriginalStart = &o
	}
	return ev
}

// ValidateWindow checks a ListEvents or FreeBusy window.
func ValidateWindow(start, end time.Time) error {
	if start.IsZero() || end.IsZero() || !end.After(start) {
		return fmt.Errorf("%w: the window needs a start and a later end", ErrInvalidQuery)
	}
	return nil
}
