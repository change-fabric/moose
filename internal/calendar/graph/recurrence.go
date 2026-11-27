package graph

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/change-fabric/moose/internal/calendar/provider"
)

type patternedRecurrence struct {
	Pattern struct {
		Type           string   `json:"type"`
		Interval       int      `json:"interval"`
		DaysOfWeek     []string `json:"daysOfWeek"`
		FirstDayOfWeek string   `json:"firstDayOfWeek"`
		DayOfMonth     int      `json:"dayOfMonth"`
		Month          int      `json:"month"`
		Index          string   `json:"index"`
	} `json:"pattern"`
	Range struct {
		Type                string `json:"type"`
		StartDate           string `json:"startDate"`
		EndDate             string `json:"endDate"`
		NumberOfOccurrences int    `json:"numberOfOccurrences"`
	} `json:"range"`
}

// recurrenceOf reads a Graph patternedRecurrence into the neutral subset when it
// is representable there, and otherwise returns it as Native: the Graph JSON,
// compact, opaque to callers. start is the series' first occurrence start, which
// the day-of-month and month of the absolute patterns must agree with (the
// neutral MONTHLY and YEARLY follow the start date).
//
// Representable: daily; weekly (with the first day of the week only mattering
// when an interval above 1 meets several days); absoluteMonthly on the start's
// day of month; absoluteYearly on the start's month and day; and a range that
// ends on a date, after a count, or never. The relative patterns ("second
// Tuesday") are native.
func recurrenceOf(raw json.RawMessage, start provider.EventTime) provider.Recurrence {
	var compact json.RawMessage
	if b, err := json.Marshal(raw); err == nil {
		compact = b
	}
	native := provider.Recurrence{Native: string(compact)}
	var p patternedRecurrence
	if err := json.Unmarshal(raw, &p); err != nil {
		return native
	}
	var r provider.Recurrence
	if p.Pattern.Interval > 1 {
		r.Interval = p.Pattern.Interval
	}
	startDay, startMonth := 0, 0
	if t := startTime(start); !t.IsZero() {
		startDay, startMonth = t.Day(), int(t.Month())
	}
	switch p.Pattern.Type {
	case "daily":
		r.Freq = provider.FreqDaily
	case "weekly":
		r.Freq = provider.FreqWeekly
		for _, d := range p.Pattern.DaysOfWeek {
			code, ok := graphDays[strings.ToLower(d)]
			if !ok {
				return native
			}
			r.ByDay = append(r.ByDay, code)
		}
		if p.Pattern.Interval > 1 && len(r.ByDay) > 1 && !strings.EqualFold(p.Pattern.FirstDayOfWeek, "monday") {
			return native
		}
	case "absoluteMonthly":
		if startDay == 0 || p.Pattern.DayOfMonth != startDay {
			return native
		}
		r.Freq = provider.FreqMonthly
	case "absoluteYearly":
		if startDay == 0 || p.Pattern.DayOfMonth != startDay || p.Pattern.Month != startMonth {
			return native
		}
		r.Freq = provider.FreqYearly
	default:
		return native
	}
	switch p.Range.Type {
	case "endDate":
		if len(p.Range.EndDate) != len(provider.DateLayout) {
			return native
		}
		r.Until = p.Range.EndDate
	case "numbered":
		if p.Range.NumberOfOccurrences < 1 {
			return native
		}
		r.Count = p.Range.NumberOfOccurrences
	case "noEnd":
	default:
		return native
	}
	if r.Validate() != nil {
		return native
	}
	return r
}

// windowsZones maps the Windows zone names Graph can report for a time zone the
// event was created in to IANA names. It lists the common ones; an unlisted name
// is shown as UTC (the instant is right, the label is not the event's own).
var windowsZones = map[string]string{
	"UTC":                             "UTC",
	"Pacific Standard Time":           "America/Los_Angeles",
	"Mountain Standard Time":          "America/Denver",
	"US Mountain Standard Time":       "America/Phoenix",
	"Central Standard Time":           "America/Chicago",
	"Eastern Standard Time":           "America/New_York",
	"Atlantic Standard Time":          "America/Halifax",
	"Alaskan Standard Time":           "America/Anchorage",
	"Hawaiian Standard Time":          "Pacific/Honolulu",
	"Canada Central Standard Time":    "America/Regina",
	"Mexico Standard Time":            "America/Mexico_City",
	"SA Pacific Standard Time":        "America/Bogota",
	"E. South America Standard Time":  "America/Sao_Paulo",
	"Argentina Standard Time":         "America/Argentina/Buenos_Aires",
	"GMT Standard Time":               "Europe/London",
	"Greenwich Standard Time":         "Atlantic/Reykjavik",
	"W. Europe Standard Time":         "Europe/Berlin",
	"Central Europe Standard Time":    "Europe/Budapest",
	"Central European Standard Time":  "Europe/Warsaw",
	"Romance Standard Time":           "Europe/Paris",
	"GTB Standard Time":               "Europe/Bucharest",
	"FLE Standard Time":               "Europe/Kiev",
	"E. Europe Standard Time":         "Europe/Chisinau",
	"Russian Standard Time":           "Europe/Moscow",
	"Turkey Standard Time":            "Europe/Istanbul",
	"South Africa Standard Time":      "Africa/Johannesburg",
	"Egypt Standard Time":             "Africa/Cairo",
	"W. Central Africa Standard Time": "Africa/Lagos",
	"E. Africa Standard Time":         "Africa/Nairobi",
	"Arab Standard Time":              "Asia/Riyadh",
	"Arabian Standard Time":           "Asia/Dubai",
	"India Standard Time":             "Asia/Kolkata",
	"Pakistan Standard Time":          "Asia/Karachi",
	"Bangladesh Standard Time":        "Asia/Dhaka",
	"SE Asia Standard Time":           "Asia/Bangkok",
	"China Standard Time":             "Asia/Shanghai",
	"Singapore Standard Time":         "Asia/Singapore",
	"Taipei Standard Time":            "Asia/Taipei",
	"Tokyo Standard Time":             "Asia/Tokyo",
	"Korea Standard Time":             "Asia/Seoul",
	"W. Australia Standard Time":      "Australia/Perth",
	"AUS Eastern Standard Time":       "Australia/Sydney",
	"E. Australia Standard Time":      "Australia/Brisbane",
	"New Zealand Standard Time":       "Pacific/Auckland",
}

func startTime(t provider.EventTime) time.Time {
	if t.IsAllDay() {
		d, _ := time.Parse(provider.DateLayout, t.Date)
		return d
	}
	return t.DateTime
}
