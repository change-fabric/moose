package provider_test

import (
	"errors"
	"testing"
	"time"

	"github.com/change-fabric/moose/internal/calendar/provider"
	mail "github.com/change-fabric/moose/internal/email/provider"
)

func TestSharedErrorsAreTheMailProvidersValues(t *testing.T) {
	// internal/accounts/fanout classifies provider failures with errors.Is
	// against the mail values; a calendar failure must match them too.
	rl := &provider.RateLimitError{RetryAfter: time.Second}
	if !errors.Is(rl, mail.ErrRateLimited) || !errors.Is(rl, provider.ErrRateLimited) {
		t.Error("a calendar RateLimitError does not match ErrRateLimited")
	}
	for name, pair := range map[string][2]error{
		"auth": {provider.ErrAuth, mail.ErrAuth}, "not found": {provider.ErrNotFound, mail.ErrNotFound},
		"invalid query": {provider.ErrInvalidQuery, mail.ErrInvalidQuery}, "rejected": {provider.ErrRejected, mail.ErrRejected},
	} {
		if !errors.Is(pair[0], pair[1]) {
			t.Errorf("%s differs between the calendar and mail providers", name)
		}
	}
	if provider.ErrUnsupportedHere.Error() != "provider: unsupported_here" {
		t.Errorf("ErrUnsupportedHere text = %q, it carries the stable code", provider.ErrUnsupportedHere)
	}
}

func TestEventTimeValidate(t *testing.T) {
	berlin, _ := time.LoadLocation("Europe/Berlin")
	ok := time.Date(2026, 3, 4, 9, 0, 0, 0, berlin)
	for name, tc := range map[string]struct {
		in   provider.EventTime
		good bool
	}{
		"timed with zone":    {provider.EventTime{DateTime: ok, Zone: "Europe/Berlin"}, true},
		"utc":                {provider.EventTime{DateTime: ok, Zone: "UTC"}, true},
		"all day":            {provider.EventTime{Date: "2026-03-04"}, true},
		"timed without zone": {provider.EventTime{DateTime: ok}, false},
		"abbreviation":       {provider.EventTime{DateTime: ok, Zone: "EST"}, false},
		"unknown zone":       {provider.EventTime{DateTime: ok, Zone: "Mars/Olympus"}, false},
		"local":              {provider.EventTime{DateTime: ok, Zone: "Local"}, false},
		"both":               {provider.EventTime{DateTime: ok, Zone: "UTC", Date: "2026-03-04"}, false},
		"bad date":           {provider.EventTime{Date: "4 March"}, false},
		"date with zone":     {provider.EventTime{Date: "2026-03-04", Zone: "UTC"}, false},
		"empty":              {provider.EventTime{}, false},
	} {
		err := tc.in.Validate()
		if (err == nil) != tc.good {
			t.Errorf("%s: err = %v", name, err)
		}
		if err != nil && !errors.Is(err, provider.ErrInvalidTime) {
			t.Errorf("%s: err %v does not wrap ErrInvalidTime", name, err)
		}
	}
}

func TestRecurrenceValidate(t *testing.T) {
	for name, tc := range map[string]struct {
		in   provider.Recurrence
		good bool
	}{
		"daily":             {provider.Recurrence{Freq: "DAILY"}, true},
		"every 2 weeks":     {provider.Recurrence{Freq: "WEEKLY", Interval: 2, ByDay: []string{"MO", "FR"}, Count: 10}, true},
		"monthly until":     {provider.Recurrence{Freq: "MONTHLY", Until: "2027-01-31"}, true},
		"yearly":            {provider.Recurrence{Freq: "YEARLY", Interval: 1}, true},
		"lowercase freq":    {provider.Recurrence{Freq: "daily"}, false},
		"no freq":           {provider.Recurrence{}, false},
		"negative interval": {provider.Recurrence{Freq: "DAILY", Interval: -1}, false},
		"huge count":        {provider.Recurrence{Freq: "DAILY", Count: 5000}, false},
		"until and count":   {provider.Recurrence{Freq: "DAILY", Until: "2027-01-01", Count: 2}, false},
		"bad until":         {provider.Recurrence{Freq: "DAILY", Until: "2027/01/01"}, false},
		"by_day on monthly": {provider.Recurrence{Freq: "MONTHLY", ByDay: []string{"MO"}}, false},
		"bad weekday":       {provider.Recurrence{Freq: "WEEKLY", ByDay: []string{"Monday"}}, false},
		"native on write":   {provider.Recurrence{Freq: "DAILY", Native: "FREQ=DAILY"}, false},
	} {
		err := tc.in.Validate()
		if (err == nil) != tc.good {
			t.Errorf("%s: err = %v", name, err)
		}
		if err != nil && !errors.Is(err, provider.ErrInvalidRecurrence) {
			t.Errorf("%s: err %v does not wrap ErrInvalidRecurrence", name, err)
		}
	}
	if !(provider.Recurrence{Native: "FREQ=DAILY;BYSETPOS=1"}).IsOpaque() || (provider.Recurrence{Freq: "DAILY", Native: "x"}).IsOpaque() {
		t.Error("IsOpaque must be true only for a native rule with no neutral fields")
	}
}

func TestHasOthers(t *testing.T) {
	self := "Me@Example.com"
	for name, tc := range map[string]struct {
		in   []provider.Attendee
		want bool
	}{
		"none":            {nil, false},
		"only me":         {[]provider.Attendee{{Email: "me@example.com"}}, false},
		"me flagged":      {[]provider.Attendee{{Email: "alias@example.com", Self: true}}, false},
		"me and a friend": {[]provider.Attendee{{Email: "me@example.com"}, {Email: "bob@example.com"}}, true},
		"blank address":   {[]provider.Attendee{{Email: "  "}}, false},
	} {
		if got := provider.HasOthers(tc.in, self); got != tc.want {
			t.Errorf("%s: HasOthers = %v, want %v", name, got, tc.want)
		}
	}
	if !(provider.Event{Attendees: []provider.Attendee{{Email: "bob@example.com"}}}).HasOtherAttendees(self) {
		t.Error("Event.HasOtherAttendees disagrees with HasOthers")
	}
}

func TestScopeAndResponse(t *testing.T) {
	if !provider.ScopeInstance.Valid() || !provider.ScopeSeries.Valid() || provider.Scope("following").Valid() || provider.Scope("").Valid() {
		t.Error("only instance and series are scopes; \"this and following\" is refused")
	}
	for _, r := range []provider.Response{provider.RespondAccept, provider.RespondTentative, provider.RespondDecline} {
		if !r.Valid() {
			t.Errorf("%s should be valid", r)
		}
	}
	if provider.Response("maybe").Valid() {
		t.Error("maybe is not a response")
	}
}

func TestCalendarCanWrite(t *testing.T) {
	for role, want := range map[string]bool{
		provider.RoleOwner: true, provider.RoleWriter: true, provider.RoleReader: false, provider.RoleFreeBusyOnly: false, "": false,
	} {
		if got := (provider.Calendar{Role: role}).CanWrite(); got != want {
			t.Errorf("role %q CanWrite = %v", role, got)
		}
	}
}
