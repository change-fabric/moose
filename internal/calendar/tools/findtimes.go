package tools

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/change-fabric/moose/internal/accounts/fanout"
	"github.com/change-fabric/moose/internal/audit"
	"github.com/change-fabric/moose/internal/calendar/provider"
	"github.com/change-fabric/moose/internal/mcpkit"
)

const (
	defaultSlots      = 5
	maxSlots          = 20
	defaultGranule    = 15
	defaultDayStart   = "09:00"
	defaultDayEnd     = "17:00"
	minDuration       = 5
	maxDuration       = 8 * 60
	findTimesMaxScan  = 62
	minuteLayout      = "15:04"
	endOfDayMinute    = 24 * 60
	granuleMaxMinutes = 120
)

// FindTimesIn is the calendar_find_meeting_times input.
type FindTimesIn struct {
	// Account narrows whose own calendars count as busy. Omitted, every account
	// with calendar access is counted.
	Account fanout.Refs `json:"account,omitempty"`
	// Emails are the other people who must be free.
	Emails          []string `json:"emails,omitempty"`
	Start           string   `json:"start,omitempty"`
	End             string   `json:"end,omitempty"`
	DurationMinutes int      `json:"duration_minutes"`
	// Zone is the IANA zone the working hours are in and bare dates mean. Default UTC.
	Zone string `json:"zone,omitempty"`
	// DayStart and DayEnd are the working hours as HH:MM ("24:00" ends at midnight).
	DayStart           string   `json:"day_start,omitempty"`
	DayEnd             string   `json:"day_end,omitempty"`
	Days               []string `json:"days,omitempty"`
	GranularityMinutes int      `json:"granularity_minutes,omitempty"`
	MaxResults         int      `json:"max_results,omitempty"`
}

// SlotOut is one proposed meeting time.
type SlotOut struct {
	Rank  int       `json:"rank"`
	Start time.Time `json:"start"`
	End   time.Time `json:"end"`
	Zone  string    `json:"zone"`
}

// FindTimesOut is the calendar_find_meeting_times output.
type FindTimesOut struct {
	Slots []SlotOut `json:"slots"`
	// Unverified lists people whose availability could not be read. They were
	// not counted as free or busy: confirm with them before booking.
	Unverified []UnavailableOut `json:"unverified,omitempty"`
	Accounts   []AccountReport  `json:"accounts"`
	Note       string           `json:"note,omitempty"`
	Hint       string           `json:"hint,omitempty"`
}

func (d Deps) registerFindMeetingTimes(s *sdk.Server) {
	str := func(desc string) *jsonschema.Schema { return &jsonschema.Schema{Type: "string", Description: desc} }
	mcpkit.AddTool(s, &sdk.Tool{
		Name: ToolFindMeetingTimes,
		Description: "Find times when you and other people are all free. It merges the busy time of every granted account's own calendars with the busy time of the listed people " +
			"(from calendar_free_busy), keeps the working hours of the chosen days, and returns ranked slots of the requested length: slots that do not touch another commitment come first, " +
			"one per day before a second on the same day, earliest first. People whose availability could not be read are listed in `unverified` and were not counted. " +
			"A personal Microsoft account contributes its own calendar only and reports `degraded: own_calendar_only`. It proposes; it books nothing.",
		InputSchema: &jsonschema.Schema{
			Type:     "object",
			Required: []string{"duration_minutes"},
			Properties: map[string]*jsonschema.Schema{
				"account":             fanout.RefsSchema("Whose own calendars count as busy. " + fanout.RefsHelp),
				"emails":              {Type: "array", Items: &jsonschema.Schema{Type: "string"}, Description: fmt.Sprintf("Addresses of the other people, at most %d.", maxPeople)},
				"start":               str("Search from: RFC 3339 or YYYY-MM-DD. Default now."),
				"end":                 str("Search until: RFC 3339 or YYYY-MM-DD. Default 7 days after start; at most 62 days."),
				"duration_minutes":    {Type: "integer", Description: fmt.Sprintf("Meeting length, %d to %d minutes.", minDuration, maxDuration)},
				"zone":                str("IANA zone of the working hours, such as Europe/Berlin. Default UTC."),
				"day_start":           str("Working day start, HH:MM. Default " + defaultDayStart + "."),
				"day_end":             str("Working day end, HH:MM (24:00 for midnight). Default " + defaultDayEnd + "."),
				"days":                {Type: "array", Items: &jsonschema.Schema{Type: "string"}, Description: "Working days as MO, TU, WE, TH, FR, SA, SU. Default MO to FR."},
				"granularity_minutes": {Type: "integer", Description: fmt.Sprintf("Slots start on multiples of this many minutes. Default %d.", defaultGranule)},
				"max_results":         {Type: "integer", Description: fmt.Sprintf("Slots to return, 1 to %d. Default %d.", maxSlots, defaultSlots)},
			},
		},
	}, mcpkit.ReadOnly(), d.findTimes)
}

func (d Deps) findTimes(ctx context.Context, req *sdk.CallToolRequest, in FindTimesIn) (*sdk.CallToolResult, FindTimesOut, error) {
	audit.FromContext(ctx).SetAction(ToolFindMeetingTimes, "", "")
	invalid := func(msg string) (*sdk.CallToolResult, FindTimesOut, error) {
		return mcpkit.ErrorResult(ctx, "invalid_input", msg), FindTimesOut{}, nil
	}
	_, picked, bad := d.pick(ctx, req, in.Account)
	if bad != nil {
		return bad, FindTimesOut{}, nil
	}
	if in.DurationMinutes < minDuration || in.DurationMinutes > maxDuration {
		return invalid(fmt.Sprintf("duration_minutes must be between %d and %d", minDuration, maxDuration))
	}
	start, end, loc, zone, err := d.windowArgs(in.Start, in.End, in.Zone, findTimesMaxScan*24*time.Hour)
	if err != nil {
		return invalid(err.Error())
	}
	if zone == "" {
		zone = "UTC"
	}
	emails, err := cleanEmails(in.Emails)
	if err != nil {
		return invalid(err.Error())
	}
	p, err := slotParams(in, start, end, loc)
	if err != nil {
		return invalid(err.Error())
	}

	r := d.askBusy(ctx, busyQuery{picked: picked, byAccount: map[string][]string{}, emails: emails, start: start, end: end})
	out := FindTimesOut{Slots: []SlotOut{}, Unverified: r.unavailable, Accounts: r.reports, Hint: narrowHint(len(picked))}

	// A failed read of own calendars means the answer would treat busy time as
	// free, so it is refused rather than guessed.
	var busy []provider.Interval
	readable := 0
	for _, b := range r.busy {
		busy = append(busy, b.Busy...)
		readable++
	}
	for _, rep := range r.reports {
		if rep.Status != fanout.StatusOK && !strings.HasPrefix(rep.Reason, "calendar_") {
			out.Note = "Some accounts could not be read (see accounts); their busy time is not counted, so confirm before booking."
		}
	}
	if readable == 0 {
		return mcpkit.ErrorResult(ctx, "no_calendars_readable", "no account's calendar could be read; see calendar_list_accounts for the grant state of each"), FindTimesOut{}, nil
	}
	if len(out.Unverified) > 0 {
		out.Note = strings.TrimSpace(out.Note + " People in unverified were not counted as free or busy.")
	}
	for i, sl := range FindSlots(busy, p) {
		out.Slots = append(out.Slots, SlotOut{Rank: i + 1, Start: sl.Start, End: sl.End, Zone: zone})
	}
	return nil, out, nil
}

func slotParams(in FindTimesIn, start, end time.Time, loc *time.Location) (SlotParams, error) {
	p := SlotParams{Start: start, End: end, Duration: time.Duration(in.DurationMinutes) * time.Minute, Loc: loc, Days: map[time.Weekday]bool{}}
	var err error
	ds, de := orStr(in.DayStart, defaultDayStart), orStr(in.DayEnd, defaultDayEnd)
	if p.DayStart, err = parseMinute("day_start", ds); err != nil {
		return p, err
	}
	if p.DayEnd, err = parseMinute("day_end", de); err != nil {
		return p, err
	}
	if p.DayEnd <= p.DayStart {
		return p, errors.New("day_end must be after day_start")
	}
	if p.DayEnd-p.DayStart < in.DurationMinutes {
		return p, errors.New("the working day is shorter than the meeting")
	}
	days := in.Days
	if len(days) == 0 {
		days = []string{"MO", "TU", "WE", "TH", "FR"}
	}
	codes := map[string]time.Weekday{"MO": time.Monday, "TU": time.Tuesday, "WE": time.Wednesday, "TH": time.Thursday, "FR": time.Friday, "SA": time.Saturday, "SU": time.Sunday}
	for _, c := range days {
		wd, ok := codes[strings.ToUpper(strings.TrimSpace(c))]
		if !ok {
			return p, fmt.Errorf("days entry %q is not one of MO, TU, WE, TH, FR, SA, SU", clip(c, 20))
		}
		p.Days[wd] = true
	}
	g := in.GranularityMinutes
	switch {
	case g < 0 || g > granuleMaxMinutes:
		return p, fmt.Errorf("granularity_minutes must be between 1 and %d", granuleMaxMinutes)
	case g == 0:
		g = defaultGranule
	}
	p.Granularity = time.Duration(g) * time.Minute
	switch {
	case in.MaxResults < 0 || in.MaxResults > maxSlots:
		return p, fmt.Errorf("max_results must be between 1 and %d", maxSlots)
	case in.MaxResults == 0:
		p.Max = defaultSlots
	default:
		p.Max = in.MaxResults
	}
	return p, nil
}

func orStr(s, d string) string {
	if strings.TrimSpace(s) == "" {
		return d
	}
	return strings.TrimSpace(s)
}

// parseMinute reads "HH:MM" (or "24:00") as minutes after midnight.
func parseMinute(name, s string) (int, error) {
	if s == "24:00" {
		return endOfDayMinute, nil
	}
	t, err := time.Parse(minuteLayout, s)
	if err != nil {
		return 0, fmt.Errorf("%s must be HH:MM, got %q", name, clip(s, 20))
	}
	return t.Hour()*60 + t.Minute(), nil
}

// ---- the slot search ----

// SlotParams describe a search for meeting times.
type SlotParams struct {
	// Start and End bound the search; a slot lies wholly inside them.
	Start, End time.Time
	Duration   time.Duration
	// Loc is the zone the working hours and the day boundaries are in.
	Loc *time.Location
	// DayStart and DayEnd are minutes after local midnight (DayEnd up to 1440).
	DayStart, DayEnd int
	Days             map[time.Weekday]bool
	// Granularity is the step slot starts are aligned to, from local midnight.
	Granularity time.Duration
	Max         int
}

// Slot is a proposed time.
type Slot struct{ Start, End time.Time }

type candidate struct {
	Slot
	day   int
	tight bool
}

// FindSlots returns up to p.Max free slots of p.Duration inside the working
// hours of the chosen days, outside every busy interval, ranked: a slot that
// does not touch a commitment on either side ranks before one that does (back
// to back with a meeting), the best slot of each day ranks before a second one
// on the same day, and otherwise earlier is better. Returned slots do not
// overlap. busy need not be sorted or merged.
func FindSlots(busy []provider.Interval, p SlotParams) []Slot {
	if p.Max <= 0 || p.Duration <= 0 || p.Granularity <= 0 || p.Loc == nil {
		return nil
	}
	merged := provider.MergeIntervals(busy)
	var cands []candidate
	first := p.Start.In(p.Loc)
	dayN := 0
	for day := time.Date(first.Year(), first.Month(), first.Day(), 0, 0, 0, 0, p.Loc); day.Before(p.End); day = time.Date(day.Year(), day.Month(), day.Day()+1, 0, 0, 0, 0, p.Loc) {
		dayN++
		if !p.Days[day.Weekday()] {
			continue
		}
		ws := time.Date(day.Year(), day.Month(), day.Day(), 0, p.DayStart, 0, 0, p.Loc)
		we := time.Date(day.Year(), day.Month(), day.Day(), 0, p.DayEnd, 0, 0, p.Loc)
		if ws.Before(p.Start) {
			ws = p.Start
		}
		if we.After(p.End) {
			we = p.End
		}
		if !we.After(ws) {
			continue
		}
		cands = append(cands, daySlots(day, ws, we, merged, p, dayN)...)
	}

	byDay := map[int][]candidate{}
	var days []int
	for _, c := range cands {
		if _, ok := byDay[c.day]; !ok {
			days = append(days, c.day)
		}
		byDay[c.day] = append(byDay[c.day], c)
	}
	rankLess := func(a, b candidate) bool {
		if a.tight != b.tight {
			return !a.tight
		}
		return a.Start.Before(b.Start)
	}
	var firsts []candidate
	for _, dn := range days {
		list := byDay[dn]
		sort.SliceStable(list, func(i, j int) bool { return rankLess(list[i], list[j]) })
		firsts = append(firsts, list[0])
	}
	sort.SliceStable(firsts, func(i, j int) bool { return rankLess(firsts[i], firsts[j]) })

	var picked []candidate
	overlaps := func(c candidate) bool {
		for _, o := range picked {
			if c.Start.Before(o.End) && o.Start.Before(c.End) {
				return true
			}
		}
		return false
	}
	for _, c := range firsts {
		if len(picked) < p.Max {
			picked = append(picked, c)
		}
	}
	// Further rounds give each day, in turn, its next best slot that does not
	// overlap one already chosen, so a second slot on a day comes only after
	// every day has had a first.
	for progress := true; progress && len(picked) < p.Max; {
		progress = false
		for _, dn := range days {
			if len(picked) >= p.Max {
				break
			}
			for _, c := range byDay[dn] {
				if !overlaps(c) {
					picked = append(picked, c)
					progress = true
					break
				}
			}
		}
	}
	out := make([]Slot, len(picked))
	for i, c := range picked {
		out[i] = c.Slot
	}
	return out
}

// daySlots lists the candidate slots of one working day [ws, we], the free gaps
// between the merged busy intervals cut to the day.
func daySlots(day, ws, we time.Time, busy []provider.Interval, p SlotParams, dayN int) []candidate {
	type gap struct {
		start, end            time.Time
		busyBefore, busyAfter bool
	}
	var gaps []gap
	cur, busyBefore := ws, false
	for _, b := range busy {
		if !b.End.After(cur) {
			continue
		}
		if !b.Start.Before(we) {
			break
		}
		if b.Start.After(cur) {
			gaps = append(gaps, gap{start: cur, end: b.Start, busyBefore: busyBefore, busyAfter: true})
		}
		cur, busyBefore = b.End, true
	}
	if we.After(cur) {
		gaps = append(gaps, gap{start: cur, end: we, busyBefore: busyBefore})
	}

	var out []candidate
	for _, g := range gaps {
		if g.end.Sub(g.start) < p.Duration {
			continue
		}
		// Align the first start up to the granularity, counted from local midnight.
		sinceMidnight := g.start.Sub(day)
		start := g.start
		if rem := sinceMidnight % p.Granularity; rem != 0 {
			start = g.start.Add(p.Granularity - rem)
		}
		for ; !start.Add(p.Duration).After(g.end); start = start.Add(p.Granularity) {
			end := start.Add(p.Duration)
			tight := (g.busyBefore && start.Equal(g.start)) || (g.busyAfter && end.Equal(g.end))
			out = append(out, candidate{Slot: Slot{Start: start, End: end}, day: dayN, tight: tight})
		}
	}
	return out
}
