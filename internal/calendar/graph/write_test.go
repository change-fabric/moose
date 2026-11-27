package graph_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/change-fabric/moose/internal/calendar/fake"
	"github.com/change-fabric/moose/internal/calendar/graph"
	"github.com/change-fabric/moose/internal/calendar/provider"
	"github.com/change-fabric/moose/internal/graphapi"
)

type sent struct {
	method, path, ifMatch, prefer string
	body                          map[string]any
}

// tap records the write requests a client sends, bodies included.
type tap struct {
	next http.RoundTripper
	got  *[]sent
}

func (t tap) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.Method != http.MethodGet {
		var raw []byte
		if r.Body != nil {
			raw, _ = io.ReadAll(r.Body)
			r.Body = io.NopCloser(bytes.NewReader(raw))
		}
		c := sent{method: r.Method, path: r.URL.Path, ifMatch: r.Header.Get("If-Match"), prefer: r.Header.Get("Prefer")}
		_ = json.Unmarshal(raw, &c.body)
		*t.got = append(*t.got, c)
	}
	return t.next.RoundTrip(r)
}

func tapped(s *stub, personal bool) (*graph.Client, *[]sent) {
	got := &[]sent{}
	g := graphapi.New(nil)
	g.BaseURL = s.srv.URL
	g.HTTP = &http.Client{Transport: tap{next: http.DefaultTransport, got: got}}
	return graph.New(g, graph.Options{Self: "me@example.com", Personal: personal}), got
}

func input(t *testing.T) provider.EventInput {
	return provider.EventInput{Title: "Sync", Start: berlin(t, 4, 9), End: berlin(t, 4, 10)}
}

func TestCreateSendsIANATimesTransactionIDAndTeamsRequest(t *testing.T) {
	s := newStub(t)
	c, got := tapped(s, false)
	in := input(t)
	in.Video = true
	in.Description = "agenda"
	in.Location = "HQ"
	rem := 10
	in.ReminderMinutes = &rem
	in.Attendees = []provider.Attendee{{Email: "bob@example.com", Optional: true}}
	if _, err := c.CreateEvent(t.Context(), fake.PrimaryID, in, provider.CreateOptions{DedupeKey: "request-1"}); err != nil {
		t.Fatal(err)
	}
	var r sent
	for _, w := range *got {
		if w.method == http.MethodPost && strings.HasSuffix(w.path, "/events") {
			r = w
		}
	}
	if st, _ := r.body["start"].(map[string]any); st["timeZone"] != "Europe/Berlin" || st["dateTime"] != "2026-03-04T09:00:00.0000000" {
		t.Errorf("start = %+v, want the local wall time and its IANA zone", st)
	}
	if !strings.Contains(r.prefer, `outlook.timezone="Europe/Berlin"`) {
		t.Errorf("Prefer = %q", r.prefer)
	}
	if r.body["isOnlineMeeting"] != true || r.body["onlineMeetingProvider"] != "teamsForBusiness" {
		t.Errorf("body = %+v, want a Teams request", r.body)
	}
	tx, _ := r.body["transactionId"].(string)
	if len(tx) != 64 || strings.Contains(tx, "request") {
		t.Errorf("transactionId = %q, want a derived value", tx)
	}
	if body, _ := r.body["body"].(map[string]any); body["contentType"] != "text" || body["content"] != "agenda" {
		t.Errorf("body = %+v, want plain text", body)
	}
	atts, _ := r.body["attendees"].([]any)
	if len(atts) != 1 || atts[0].(map[string]any)["type"] != "optional" {
		t.Errorf("attendees = %v", atts)
	}
	if r.body["isReminderOn"] != true || r.body["reminderMinutesBeforeStart"] != float64(10) {
		t.Errorf("reminder = %v %v", r.body["isReminderOn"], r.body["reminderMinutesBeforeStart"])
	}
}

func TestRecurrenceIsWrittenAsPatternedRecurrenceAnchoredAtTheStart(t *testing.T) {
	cases := []struct {
		name string
		rule provider.Recurrence
		want map[string]any
	}{
		{"weekly days", provider.Recurrence{Freq: provider.FreqWeekly, ByDay: []string{"MO", "WE"}, Interval: 2, Count: 5},
			map[string]any{"type": "weekly", "interval": float64(2), "firstDayOfWeek": "monday"}},
		{"weekly follows the start's weekday", provider.Recurrence{Freq: provider.FreqWeekly},
			map[string]any{"type": "weekly", "interval": float64(1)}},
		{"monthly on the start's day", provider.Recurrence{Freq: provider.FreqMonthly, Until: "2026-12-31"},
			map[string]any{"type": "absoluteMonthly", "dayOfMonth": float64(4)}},
		{"yearly on the start's day and month", provider.Recurrence{Freq: provider.FreqYearly},
			map[string]any{"type": "absoluteYearly", "dayOfMonth": float64(4), "month": float64(3)}},
		{"daily", provider.Recurrence{Freq: provider.FreqDaily, Interval: 3},
			map[string]any{"type": "daily", "interval": float64(3)}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newStub(t)
			c, got := tapped(s, false)
			in := input(t)
			rule := tc.rule
			in.Recurrence = &rule
			if _, err := c.CreateEvent(t.Context(), fake.PrimaryID, in, provider.CreateOptions{}); err != nil {
				t.Fatal(err)
			}
			rec, _ := (*got)[len(*got)-1].body["recurrence"].(map[string]any)
			pat, _ := rec["pattern"].(map[string]any)
			for k, v := range tc.want {
				if pat[k] != v {
					t.Errorf("pattern[%s] = %v, want %v (pattern %+v)", k, pat[k], v, pat)
				}
			}
			rng, _ := rec["range"].(map[string]any)
			if rng["startDate"] != "2026-03-04" || rng["recurrenceTimeZone"] != "Europe/Berlin" {
				t.Errorf("range = %+v, want startDate 2026-03-04 in Europe/Berlin", rng)
			}
			switch {
			case tc.rule.Count > 0:
				if rng["type"] != "numbered" || rng["numberOfOccurrences"] != float64(tc.rule.Count) {
					t.Errorf("range = %+v", rng)
				}
			case tc.rule.Until != "":
				if rng["type"] != "endDate" || rng["endDate"] != tc.rule.Until {
					t.Errorf("range = %+v", rng)
				}
			default:
				if rng["type"] != "noEnd" {
					t.Errorf("range = %+v", rng)
				}
			}
			if tc.name == "weekly follows the start's weekday" {
				if days, _ := pat["daysOfWeek"].([]any); len(days) != 1 || days[0] != "wednesday" {
					t.Errorf("daysOfWeek = %v, want the start's weekday", days)
				}
			}
		})
	}
}

func TestVideoOnAPersonalAccountIsRefusedWithoutARequest(t *testing.T) {
	s := newStub(t)
	c, got := tapped(s, true)
	in := input(t)
	in.Video = true
	if _, err := c.CreateEvent(t.Context(), fake.PrimaryID, in, provider.CreateOptions{}); !errors.Is(err, provider.ErrUnsupportedHere) {
		t.Fatalf("err = %v, want ErrUnsupportedHere", err)
	}
	on := true
	if _, err := c.UpdateEvent(t.Context(), provider.EventRef{CalendarID: fake.PrimaryID, EventID: "e"}, provider.ScopeInstance, provider.EventPatch{Video: &on}, "v"); !errors.Is(err, provider.ErrUnsupportedHere) {
		t.Fatalf("update err = %v, want ErrUnsupportedHere", err)
	}
	if len(*got) != 0 {
		t.Errorf("sent %+v", *got)
	}
}

func TestUpdatePresentsTheChangeKeyAsAnETagAndRefusesToWriteBlind(t *testing.T) {
	s := newStub(t)
	c, got := tapped(s, false)
	ev, err := c.CreateEvent(t.Context(), fake.PrimaryID, input(t), provider.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	*got = nil
	title := "Renamed"
	ref := provider.EventRef{CalendarID: fake.PrimaryID, EventID: ev.ID}
	if _, err := c.UpdateEvent(t.Context(), ref, provider.ScopeInstance, provider.EventPatch{Title: &title}, ev.Version); err != nil {
		t.Fatal(err)
	}
	r := (*got)[0]
	if r.method != http.MethodPatch || r.ifMatch != `W/"`+ev.Version+`"` || r.body["subject"] != "Renamed" {
		t.Errorf("request = %+v, want a PATCH with If-Match W/\"%s\"", r, ev.Version)
	}
	*got = nil
	for _, v := range []string{"", " "} {
		_, uerr := c.UpdateEvent(t.Context(), ref, provider.ScopeInstance, provider.EventPatch{Title: &title}, v)
		derr := c.DeleteEvent(t.Context(), ref, provider.ScopeInstance, v)
		if !errors.Is(uerr, provider.ErrPreconditionFailed) || !errors.Is(derr, provider.ErrPreconditionFailed) {
			t.Errorf("blind write errs = %v, %v", uerr, derr)
		}
	}
	if len(*got) != 0 {
		t.Errorf("a write without a version sent %+v", *got)
	}
}

func TestA412IsAPreconditionFailureAndNobodyIsNotified(t *testing.T) {
	s := newStub(t)
	c, _ := tapped(s, false)
	in := input(t)
	in.Attendees = []provider.Attendee{{Email: "bob@example.com"}}
	ev, _ := c.CreateEvent(t.Context(), fake.PrimaryID, in, provider.CreateOptions{})
	ref := provider.EventRef{CalendarID: fake.PrimaryID, EventID: ev.ID}
	s.p.ExternalAddAttendee(ref, provider.Attendee{Email: "mallory@example.com"})
	before := len(s.p.Notifications())
	title := "x"
	if _, err := c.UpdateEvent(t.Context(), ref, provider.ScopeInstance, provider.EventPatch{Title: &title}, ev.Version); !errors.Is(err, provider.ErrPreconditionFailed) {
		t.Fatalf("update err = %v", err)
	}
	if err := c.DeleteEvent(t.Context(), ref, provider.ScopeInstance, ev.Version); !errors.Is(err, provider.ErrPreconditionFailed) {
		t.Fatalf("delete err = %v", err)
	}
	if len(s.p.Notifications()) != before {
		t.Errorf("a stale write notified %+v", s.p.Notifications()[before:])
	}
}

func TestDeleteCancelsAnEventWithGuestsAndDeletesOneWithout(t *testing.T) {
	s := newStub(t)
	c, got := tapped(s, false)
	with := input(t)
	with.Attendees = []provider.Attendee{{Email: "bob@example.com"}}
	a, _ := c.CreateEvent(t.Context(), fake.PrimaryID, with, provider.CreateOptions{})
	b, _ := c.CreateEvent(t.Context(), fake.PrimaryID, input(t), provider.CreateOptions{})
	self := input(t)
	self.Attendees = []provider.Attendee{{Email: "me@example.com"}}
	d, _ := c.CreateEvent(t.Context(), fake.PrimaryID, self, provider.CreateOptions{})
	*got = nil
	for _, ev := range []provider.Event{a, b, d} {
		if err := c.DeleteEvent(t.Context(), provider.EventRef{CalendarID: fake.PrimaryID, EventID: ev.ID}, provider.ScopeInstance, ev.Version); err != nil {
			t.Fatal(err)
		}
	}
	if len(*got) != 3 {
		t.Fatalf("writes = %+v", *got)
	}
	if !strings.HasSuffix((*got)[0].path, "/cancel") || (*got)[0].method != http.MethodPost || (*got)[0].ifMatch == "" {
		t.Errorf("an event with a guest must be cancelled with If-Match, got %+v", (*got)[0])
	}
	for _, w := range (*got)[1:] {
		if w.method != http.MethodDelete || w.ifMatch == "" {
			t.Errorf("a self-only event must be deleted with If-Match, got %+v", w)
		}
	}
}

func TestASeriesWriteThroughAnOccurrenceGoesToTheMaster(t *testing.T) {
	s := newStub(t)
	c, got := tapped(s, false)
	in := input(t)
	in.Recurrence = &provider.Recurrence{Freq: provider.FreqDaily, Count: 3}
	master, err := c.CreateEvent(t.Context(), fake.PrimaryID, in, provider.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	from, to := win(t)
	page, err := c.ListEvents(t.Context(), provider.ListEventsQuery{Start: from, End: to})
	if err != nil || len(page.Events) != 3 {
		t.Fatal(page, err)
	}
	occ := page.Events[1]
	*got = nil
	loc := "HQ"
	if _, err := c.UpdateEvent(t.Context(), provider.EventRef{CalendarID: fake.PrimaryID, EventID: occ.ID}, provider.ScopeSeries, provider.EventPatch{Location: &loc}, occ.Version); err != nil {
		t.Fatal(err)
	}
	if len(*got) != 1 || !strings.HasSuffix((*got)[0].path, "/events/"+master.ID) {
		t.Fatalf("writes = %+v, want one PATCH of the master %s", *got, master.ID)
	}
	*got = nil
	if _, err := c.UpdateEvent(t.Context(), provider.EventRef{CalendarID: fake.PrimaryID, EventID: occ.ID}, provider.ScopeSeries, provider.EventPatch{Location: &loc}, occ.Version); !errors.Is(err, provider.ErrPreconditionFailed) {
		t.Errorf("an occurrence at a stale version err = %v, want ErrPreconditionFailed", err)
	}
	if len(*got) != 0 {
		t.Errorf("a stale series write sent %+v", *got)
	}
}

func TestRespondUsesTheActionEndpointWithTheCommentAndSendsTheResponse(t *testing.T) {
	s := newStub(t)
	c, got := tapped(s, false)
	invite := s.p.AddEvent(fake.PrimaryID, provider.Event{
		Title: "Theirs", Start: berlin(t, 4, 9), End: berlin(t, 4, 10), Organizer: provider.Attendee{Email: "boss@example.com"},
		Attendees: []provider.Attendee{{Email: "boss@example.com", Response: provider.ResponseAccepted}, {Email: "me@example.com", Response: provider.ResponseNone}},
	})
	ref := provider.EventRef{CalendarID: fake.PrimaryID, EventID: invite.ID}
	for resp, action := range map[provider.Response]string{provider.RespondAccept: "accept", provider.RespondTentative: "tentativelyAccept", provider.RespondDecline: "decline"} {
		*got = nil
		if err := c.Respond(t.Context(), ref, resp, "note "+action); err != nil {
			t.Fatal(err)
		}
		r := (*got)[0]
		if r.method != http.MethodPost || !strings.HasSuffix(r.path, "/"+action) || r.body["comment"] != "note "+action || r.body["sendResponse"] != true {
			t.Errorf("%s: request = %+v", action, r)
		}
	}
	// Responding to a meeting the account organizes is the provider's refusal, mapped to ErrRejected.
	own, _ := c.CreateEvent(t.Context(), fake.PrimaryID, input(t), provider.CreateOptions{})
	if err := c.Respond(t.Context(), provider.EventRef{CalendarID: fake.PrimaryID, EventID: own.ID}, provider.RespondAccept, ""); !errors.Is(err, provider.ErrRejected) {
		t.Errorf("err = %v, want ErrRejected", err)
	}
}

func TestA400OnAWriteIsARefusalNotABadQuery(t *testing.T) {
	s := newStub(t)
	c, _ := tapped(s, false)
	// The stub answers 400 for a recurrence it cannot read back.
	in := input(t)
	in.Recurrence = &provider.Recurrence{Freq: provider.FreqDaily, Count: 2}
	s.reject400 = true
	if _, err := c.CreateEvent(t.Context(), fake.PrimaryID, in, provider.CreateOptions{}); !errors.Is(err, provider.ErrRejected) || errors.Is(err, provider.ErrInvalidQuery) && !errors.Is(err, provider.ErrRejected) {
		t.Errorf("err = %v, want ErrRejected", err)
	}
}

func TestAReadOnlyCalendarIsRefusedBeforeAnyWrite(t *testing.T) {
	s := newStub(t)
	c, got := tapped(s, false)
	s.p.AddCalendar(provider.Calendar{ID: "team", Name: "Team", Role: provider.RoleReader})
	if _, err := c.CreateEvent(t.Context(), "team", input(t), provider.CreateOptions{}); !errors.Is(err, provider.ErrRejected) {
		t.Errorf("err = %v, want ErrRejected", err)
	}
	for _, w := range *got {
		t.Errorf("sent %+v", w)
	}
}
