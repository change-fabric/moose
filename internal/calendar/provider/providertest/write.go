package providertest

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/change-fabric/moose/internal/calendar/provider"
)

func newInput(t *testing.T, title string) provider.EventInput {
	t.Helper()
	return provider.EventInput{
		Title: title, Start: at(t, berlin, 2026, time.March, 4, 9, 0), End: at(t, berlin, 2026, time.March, 4, 10, 0),
	}
}

func withAttendees(in provider.EventInput, emails ...string) provider.EventInput {
	for _, e := range emails {
		in.Attendees = append(in.Attendees, provider.Attendee{Email: e})
	}
	return in
}

func ref(cal string, e provider.Event) provider.EventRef {
	return provider.EventRef{CalendarID: cal, EventID: e.ID}
}

func countIn(t *testing.T, p provider.Provider) int {
	t.Helper()
	from := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
	page, err := p.ListEvents(t.Context(), provider.ListEventsQuery{Start: from, End: from.AddDate(1, 0, 0), Limit: 500})
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	return len(page.Events)
}

func str(s string) *string { return &s }

// RunWrite runs the write conformance suite: create and its dedupe key,
// validation, the version precondition on update and delete (a stale write
// changes nothing and notifies nobody), notification recipients, series scope,
// RSVP and failure mapping.
func RunWrite(t *testing.T, factory Factory) {
	t.Run("CreateRoundTripsAndNotifiesAttendees", func(t *testing.T) {
		p, s := factory(t)
		prim := primaryID(t, p)
		rem := 10
		in := withAttendees(newInput(t, "Kickoff"), "bob@example.com", "carol@example.com")
		in.Description, in.Location, in.ReminderMinutes = "Intro", "Room 1", &rem
		ev, err := p.CreateEvent(t.Context(), prim, in, provider.CreateOptions{DedupeKey: "k-1"})
		if err != nil {
			t.Fatal(err)
		}
		if ev.ID == "" || ev.Version == "" || ev.CalendarID != prim {
			t.Fatalf("created = %+v", ev)
		}
		got, err := p.GetEvent(t.Context(), ref(prim, ev))
		if err != nil {
			t.Fatal(err)
		}
		if got.Title != "Kickoff" || got.Description != "Intro" || got.Location != "Room 1" || got.Start.Zone != berlin ||
			!sameInstant(got.Start, in.Start) || !sameInstant(got.End, in.End) || got.Organizer.Email != SelfAddress {
			t.Fatalf("event = %+v", got)
		}
		if got.ReminderMinutes == nil || *got.ReminderMinutes != 10 {
			t.Errorf("reminder = %v", got.ReminderMinutes)
		}
		var emails []string
		for _, a := range got.Attendees {
			emails = append(emails, strings.ToLower(a.Email))
		}
		if !has(emails, "bob@example.com") || !has(emails, "carol@example.com") {
			t.Errorf("attendees = %v", emails)
		}
		invites := notesOf(t, s, "invite")
		if len(invites) != 1 || !has(invites[0].To, "bob@example.com") || !has(invites[0].To, "carol@example.com") || has(invites[0].To, SelfAddress) {
			t.Errorf("invites = %+v, want one to the two attendees and not to the account", invites)
		}
	})

	t.Run("SelfOnlyCreateNotifiesNobody", func(t *testing.T) {
		p, s := factory(t)
		prim := primaryID(t, p)
		if _, err := p.CreateEvent(t.Context(), prim, newInput(t, "Focus time"), provider.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
		if _, err := p.CreateEvent(t.Context(), prim, withAttendees(newInput(t, "Just me"), SelfAddress), provider.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
		if n := s.Notified(t); len(n) != 0 {
			t.Errorf("self-only events sent %+v", n)
		}
	})

	t.Run("CreateDedupeKeyReturnsTheSameEvent", func(t *testing.T) {
		p, _ := factory(t)
		prim := primaryID(t, p)
		first, err := p.CreateEvent(t.Context(), prim, newInput(t, "Once"), provider.CreateOptions{DedupeKey: "req-1"})
		if err != nil {
			t.Fatal(err)
		}
		again, err := p.CreateEvent(t.Context(), prim, newInput(t, "Once"), provider.CreateOptions{DedupeKey: "req-1"})
		if err != nil {
			t.Fatal(err)
		}
		if again.ID != first.ID || countIn(t, p) != 1 {
			t.Fatalf("a repeated dedupe key made %s then %s, %d events; want one event", first.ID, again.ID, countIn(t, p))
		}
		other, err := p.CreateEvent(t.Context(), prim, newInput(t, "Twice"), provider.CreateOptions{DedupeKey: "req-2"})
		if err != nil || other.ID == first.ID || countIn(t, p) != 2 {
			t.Fatalf("another key = %+v, %v, %d events", other, err, countIn(t, p))
		}
	})

	t.Run("FindByDedupeKeyLooksUpWithoutCreating", func(t *testing.T) {
		p, _ := factory(t)
		f, ok := p.(provider.DedupeFinder)
		if !ok {
			t.Skip("provider has no DedupeFinder")
		}
		prim := primaryID(t, p)
		if _, found, err := f.FindByDedupeKey(t.Context(), prim, "never-made"); err != nil || found {
			t.Fatalf("lookup of an unused key = found %v, err %v", found, err)
		}
		if n := countIn(t, p); n != 0 {
			t.Fatalf("a lookup created %d events", n)
		}
		made, err := p.CreateEvent(t.Context(), prim, newInput(t, "Findable"), provider.CreateOptions{DedupeKey: "req-find"})
		if err != nil {
			t.Fatal(err)
		}
		got, found, err := f.FindByDedupeKey(t.Context(), prim, "req-find")
		if err != nil || !found || got.ID != made.ID || got.Title != "Findable" {
			t.Fatalf("lookup = %+v found %v err %v, want event %s", got, found, err, made.ID)
		}
		if _, found, _ := f.FindByDedupeKey(t.Context(), prim, "req-other"); found {
			t.Fatal("a different key found the event")
		}
	})

	t.Run("CreateRefusesBadTimesAndRecurrenceBeforeWriting", func(t *testing.T) {
		p, s := factory(t)
		prim := primaryID(t, p)
		badTime := map[string]func(*provider.EventInput){
			"end before start": func(in *provider.EventInput) { in.End = in.Start },
			"no zone":          func(in *provider.EventInput) { in.Start.Zone = "" },
			"abbreviation":     func(in *provider.EventInput) { in.Start.Zone, in.End.Zone = "EST", "EST" },
			"date and instant": func(in *provider.EventInput) { in.Start.Date = "2026-03-04" },
			"mixed forms":      func(in *provider.EventInput) { in.End = day(2026, time.March, 5) },
			"empty start":      func(in *provider.EventInput) { in.Start = provider.EventTime{} },
		}
		for name, mut := range badTime {
			in := newInput(t, "bad")
			mut(&in)
			if _, err := p.CreateEvent(t.Context(), prim, in, provider.CreateOptions{}); !errors.Is(err, provider.ErrInvalidTime) {
				t.Errorf("%s: err = %v, want ErrInvalidTime", name, err)
			}
		}
		badRule := map[string]provider.Recurrence{
			"unknown freq":      {Freq: "HOURLY"},
			"native rule":       {Freq: provider.FreqDaily, Native: "FREQ=DAILY;BYSETPOS=1"},
			"by_day on daily":   {Freq: provider.FreqDaily, ByDay: []string{"MO"}},
			"until and count":   {Freq: provider.FreqDaily, Until: "2026-04-01", Count: 3},
			"bad weekday":       {Freq: provider.FreqWeekly, ByDay: []string{"XX"}},
			"duplicate weekday": {Freq: provider.FreqWeekly, ByDay: []string{"MO", "MO"}},
			"bad until":         {Freq: provider.FreqDaily, Until: "04/01/2026"},
			"empty":             {},
		}
		for name, rule := range badRule {
			in := newInput(t, "bad")
			in.Recurrence = &rule
			if _, err := p.CreateEvent(t.Context(), prim, in, provider.CreateOptions{}); !errors.Is(err, provider.ErrInvalidRecurrence) {
				t.Errorf("%s: err = %v, want ErrInvalidRecurrence", name, err)
			}
		}
		if n := countIn(t, p); n != 0 {
			t.Errorf("%d events were created by refused requests", n)
		}
		if n := s.Notified(t); len(n) != 0 {
			t.Errorf("refused requests notified %+v", n)
		}
	})

	t.Run("CreateVideoFollowsCapabilities", func(t *testing.T) {
		p, _ := factory(t)
		prim := primaryID(t, p)
		in := newInput(t, "Call")
		in.Video = true
		ev, err := p.CreateEvent(t.Context(), prim, in, provider.CreateOptions{})
		if !p.Capabilities().Video {
			if !errors.Is(err, provider.ErrUnsupportedHere) {
				t.Fatalf("video on an account that cannot err = %v, want ErrUnsupportedHere", err)
			}
			if n := countIn(t, p); n != 0 {
				t.Errorf("a refused video create left %d events", n)
			}
			return
		}
		if err != nil {
			t.Fatal(err)
		}
		if !ev.Video || ev.VideoURL == "" {
			t.Errorf("event = %+v, want an online meeting", ev)
		}
	})

	t.Run("CreateOnAReadOnlyCalendarIsRefused", func(t *testing.T) {
		p, s := factory(t)
		s.SeedCalendar(t, provider.Calendar{ID: "team", Name: "Team", Role: provider.RoleReader})
		if _, err := p.CreateEvent(t.Context(), "team", newInput(t, "no"), provider.CreateOptions{}); !errors.Is(err, provider.ErrRejected) {
			t.Errorf("err = %v, want ErrRejected", err)
		}
	})

	t.Run("UpdateAppliesThePatchAndChangesTheVersion", func(t *testing.T) {
		p, _ := factory(t)
		prim := primaryID(t, p)
		ev, _ := p.CreateEvent(t.Context(), prim, newInput(t, "Old title"), provider.CreateOptions{})
		newStart, newEnd := at(t, berlin, 2026, time.March, 4, 11, 0), at(t, berlin, 2026, time.March, 4, 12, 30)
		up, err := p.UpdateEvent(t.Context(), ref(prim, ev), provider.ScopeInstance, provider.EventPatch{
			Title: str("New title"), Location: str("Room 9"), Start: &newStart, End: &newEnd,
		}, ev.Version)
		if err != nil {
			t.Fatal(err)
		}
		if up.ID != ev.ID || up.Title != "New title" || up.Location != "Room 9" || up.Version == "" || up.Version == ev.Version ||
			!sameInstant(up.Start, newStart) || !sameInstant(up.End, newEnd) {
			t.Fatalf("updated = %+v", up)
		}
		got, _ := p.GetEvent(t.Context(), ref(prim, ev))
		if got.Version != up.Version || got.Title != "New title" || got.Description != "" {
			t.Errorf("stored = %+v; fields the patch did not name must stay", got)
		}
	})

	t.Run("UpdateWithAStaleVersionWritesNothingAndNotifiesNobody", func(t *testing.T) {
		p, s := factory(t)
		prim := primaryID(t, p)
		ev, err := p.CreateEvent(t.Context(), prim, withAttendees(newInput(t, "Original"), "bob@example.com"), provider.CreateOptions{})
		if err != nil {
			t.Fatal(err)
		}
		sentBefore := len(s.Notified(t))
		// Someone else adds an attendee between our read and our write.
		s.ExternalAddAttendee(t, ref(prim, ev), provider.Attendee{Email: "mallory@example.com"})
		_, err = p.UpdateEvent(t.Context(), ref(prim, ev), provider.ScopeInstance, provider.EventPatch{Title: str("Changed")}, ev.Version)
		if !errors.Is(err, provider.ErrPreconditionFailed) {
			t.Fatalf("update at a stale version err = %v, want ErrPreconditionFailed", err)
		}
		got, _ := p.GetEvent(t.Context(), ref(prim, ev))
		if got.Title != "Original" {
			t.Errorf("a refused update changed the title to %q", got.Title)
		}
		if after := len(s.Notified(t)); after != sentBefore {
			t.Errorf("a refused update notified %d attendees' worth of mail: %+v", after-sentBefore, s.Notified(t))
		}
		// Re-reading and retrying at the new version works.
		if _, err := p.UpdateEvent(t.Context(), ref(prim, ev), provider.ScopeInstance, provider.EventPatch{Title: str("Changed")}, got.Version); err != nil {
			t.Errorf("update at the fresh version: %v", err)
		}
		// No version at all is not a licence to overwrite.
		if _, err := p.UpdateEvent(t.Context(), ref(prim, ev), provider.ScopeInstance, provider.EventPatch{Title: str("Blind")}, ""); !errors.Is(err, provider.ErrPreconditionFailed) {
			t.Errorf("update without a version err = %v, want ErrPreconditionFailed", err)
		}
	})

	t.Run("UpdateNotifiesKeptAndRemovedAttendees", func(t *testing.T) {
		p, s := factory(t)
		prim := primaryID(t, p)
		ev, _ := p.CreateEvent(t.Context(), prim, withAttendees(newInput(t, "Team"), "bob@example.com", "carol@example.com"), provider.CreateOptions{})
		kept := []provider.Attendee{{Email: "bob@example.com"}}
		if _, err := p.UpdateEvent(t.Context(), ref(prim, ev), provider.ScopeInstance, provider.EventPatch{Attendees: &kept}, ev.Version); err != nil {
			t.Fatal(err)
		}
		updates := notesOf(t, s, "update")
		if len(updates) == 0 {
			t.Fatal("removing an attendee notified nobody")
		}
		var to []string
		for _, n := range updates {
			to = append(to, n.To...)
		}
		if !has(to, "bob@example.com") || !has(to, "carol@example.com") {
			t.Errorf("update recipients = %v, want the kept and the removed attendee", to)
		}
		got, _ := p.GetEvent(t.Context(), ref(prim, ev))
		for _, a := range got.Attendees {
			if strings.EqualFold(a.Email, "carol@example.com") {
				t.Errorf("carol is still an attendee: %+v", got.Attendees)
			}
		}
	})

	t.Run("UpdateRefusesAnUnsupportedScope", func(t *testing.T) {
		p, _ := factory(t)
		prim := primaryID(t, p)
		ev, _ := p.CreateEvent(t.Context(), prim, newInput(t, "x"), provider.CreateOptions{})
		_, err := p.UpdateEvent(t.Context(), ref(prim, ev), provider.Scope("following"), provider.EventPatch{Title: str("y")}, ev.Version)
		if err == nil {
			t.Fatal("\"this and following\" was accepted")
		}
		if err := p.DeleteEvent(t.Context(), ref(prim, ev), provider.Scope("following"), ev.Version); err == nil {
			t.Fatal("a delete with scope \"following\" was accepted")
		}
		if got, _ := p.GetEvent(t.Context(), ref(prim, ev)); got.Title != "x" {
			t.Errorf("refused calls changed the event: %+v", got)
		}
	})

	t.Run("DeleteWithAStaleVersionKeepsTheEventAndNotifiesNobody", func(t *testing.T) {
		p, s := factory(t)
		prim := primaryID(t, p)
		ev, _ := p.CreateEvent(t.Context(), prim, withAttendees(newInput(t, "Keep me"), "bob@example.com"), provider.CreateOptions{})
		before := len(s.Notified(t))
		s.ExternalAddAttendee(t, ref(prim, ev), provider.Attendee{Email: "mallory@example.com"})
		if err := p.DeleteEvent(t.Context(), ref(prim, ev), provider.ScopeInstance, ev.Version); !errors.Is(err, provider.ErrPreconditionFailed) {
			t.Fatalf("delete at a stale version err = %v, want ErrPreconditionFailed", err)
		}
		if _, err := p.GetEvent(t.Context(), ref(prim, ev)); err != nil {
			t.Errorf("the event is gone after a refused delete: %v", err)
		}
		if after := len(s.Notified(t)); after != before {
			t.Errorf("a refused delete sent mail: %+v", s.Notified(t))
		}
		if err := p.DeleteEvent(t.Context(), ref(prim, ev), provider.ScopeInstance, ""); !errors.Is(err, provider.ErrPreconditionFailed) {
			t.Errorf("delete without a version err = %v, want ErrPreconditionFailed", err)
		}
	})

	t.Run("DeleteCancelsAndNotifiesAttendees", func(t *testing.T) {
		p, s := factory(t)
		prim := primaryID(t, p)
		ev, _ := p.CreateEvent(t.Context(), prim, withAttendees(newInput(t, "Gone"), "bob@example.com"), provider.CreateOptions{})
		if err := p.DeleteEvent(t.Context(), ref(prim, ev), provider.ScopeInstance, ev.Version); err != nil {
			t.Fatal(err)
		}
		if _, err := p.GetEvent(t.Context(), ref(prim, ev)); !errors.Is(err, provider.ErrNotFound) {
			t.Errorf("deleted event err = %v, want ErrNotFound", err)
		}
		cancels := notesOf(t, s, "cancel")
		if len(cancels) != 1 || !has(cancels[0].To, "bob@example.com") {
			t.Errorf("cancellations = %+v", cancels)
		}
		if err := p.DeleteEvent(t.Context(), ref(prim, ev), provider.ScopeInstance, ev.Version); !errors.Is(err, provider.ErrNotFound) {
			t.Errorf("second delete err = %v, want ErrNotFound", err)
		}
	})

	t.Run("SeriesScopeSeparatesOneOccurrenceFromTheSeries", func(t *testing.T) {
		p, _ := factory(t)
		prim := primaryID(t, p)
		in := newInput(t, "Standup")
		in.Recurrence = &provider.Recurrence{Freq: provider.FreqDaily, Count: 4}
		master, err := p.CreateEvent(t.Context(), prim, in, provider.CreateOptions{})
		if err != nil {
			t.Fatal(err)
		}
		from, to := window(t, 4, 10)
		list := func() []provider.Event {
			page, err := p.ListEvents(t.Context(), provider.ListEventsQuery{Start: from, End: to})
			if err != nil {
				t.Fatal(err)
			}
			return page.Events
		}
		occ := list()
		if len(occ) != 4 {
			t.Fatalf("%d occurrences, want 4", len(occ))
		}

		// Change one occurrence only.
		up, err := p.UpdateEvent(t.Context(), ref(prim, occ[1]), provider.ScopeInstance, provider.EventPatch{Title: str("Standup (moved room)")}, occ[1].Version)
		if err != nil {
			t.Fatal(err)
		}
		if up.Title != "Standup (moved room)" || up.SeriesID == "" {
			t.Errorf("updated occurrence = %+v", up)
		}
		got := list()
		want := []string{"Standup", "Standup (moved room)", "Standup", "Standup"}
		if !equalStrings(titles(got), want) {
			t.Fatalf("titles after an instance update = %v, want %v", titles(got), want)
		}

		// Cancel one occurrence only.
		if err := p.DeleteEvent(t.Context(), ref(prim, got[2]), provider.ScopeInstance, got[2].Version); err != nil {
			t.Fatal(err)
		}
		got = list()
		if len(got) != 3 {
			t.Fatalf("%d occurrences after cancelling one, want 3", len(got))
		}

		// Change the whole series through an occurrence.
		for _, e := range got {
			if e.SeriesID == "" {
				t.Fatalf("occurrence without a series id: %+v", e)
			}
		}
		if _, err := p.UpdateEvent(t.Context(), ref(prim, got[0]), provider.ScopeSeries, provider.EventPatch{Location: str("HQ")}, got[0].Version); err != nil {
			t.Fatal(err)
		}
		for _, e := range list() {
			if e.Location != "HQ" && e.Title != "Standup (moved room)" {
				t.Errorf("series update missed occurrence %+v", e)
			}
		}
		// The series update made every earlier occurrence version stale.
		if _, err := p.UpdateEvent(t.Context(), ref(prim, got[0]), provider.ScopeInstance, provider.EventPatch{Title: str("stale")}, got[0].Version); !errors.Is(err, provider.ErrPreconditionFailed) {
			t.Errorf("update at a version from before the series changed err = %v, want ErrPreconditionFailed", err)
		}

		// Delete the series.
		fresh := list()
		if err := p.DeleteEvent(t.Context(), ref(prim, fresh[0]), provider.ScopeSeries, fresh[0].Version); err != nil {
			t.Fatal(err)
		}
		if n := len(list()); n != 0 {
			t.Errorf("%d occurrences left after deleting the series", n)
		}
		if _, err := p.GetEvent(t.Context(), ref(prim, master)); !errors.Is(err, provider.ErrNotFound) {
			t.Errorf("series master err = %v, want ErrNotFound", err)
		}
	})

	t.Run("RespondSetsTheAccountsAnswerAndNotifiesTheOrganizer", func(t *testing.T) {
		p, s := factory(t)
		prim := primaryID(t, p)
		invite := s.SeedEvent(t, prim, provider.Event{
			Title: "Their meeting", Start: at(t, berlin, 2026, time.March, 4, 9, 0), End: at(t, berlin, 2026, time.March, 4, 10, 0),
			Organizer: provider.Attendee{Email: "boss@example.com", Name: "Boss"},
			Attendees: []provider.Attendee{{Email: "boss@example.com", Response: provider.ResponseAccepted}, {Email: SelfAddress, Response: provider.ResponseNone}},
		})
		if err := p.Respond(t.Context(), ref(prim, invite), provider.RespondAccept, "See you there"); err != nil {
			t.Fatal(err)
		}
		got, _ := p.GetEvent(t.Context(), ref(prim, invite))
		if got.SelfResponse != provider.ResponseAccepted {
			t.Errorf("self response = %q, want accepted", got.SelfResponse)
		}
		rsvps := notesOf(t, s, "rsvp")
		if len(rsvps) != 1 || !has(rsvps[0].To, "boss@example.com") {
			t.Errorf("rsvp notifications = %+v, want one to the organizer", rsvps)
		}
		if err := p.Respond(t.Context(), ref(prim, invite), provider.RespondDecline, ""); err != nil {
			t.Fatal(err)
		}
		if got, _ = p.GetEvent(t.Context(), ref(prim, invite)); got.SelfResponse != provider.ResponseDeclined {
			t.Errorf("self response = %q, want declined", got.SelfResponse)
		}
		if err := p.Respond(t.Context(), ref(prim, invite), provider.Response("maybe"), ""); err == nil {
			t.Error("an unknown response was accepted")
		}
		if err := p.Respond(t.Context(), ref(prim, invite), provider.RespondAccept, strings.Repeat("x", provider.MaxResponseComment+1)); !errors.Is(err, provider.ErrRejected) {
			t.Errorf("an over-long comment err = %v, want ErrRejected", err)
		}
		own, _ := p.CreateEvent(t.Context(), prim, newInput(t, "Mine"), provider.CreateOptions{})
		if err := p.Respond(t.Context(), ref(prim, own), provider.RespondAccept, ""); !errors.Is(err, provider.ErrRejected) {
			t.Errorf("responding to an event the account is not invited to err = %v, want ErrRejected", err)
		}
		if err := p.Respond(t.Context(), provider.EventRef{CalendarID: prim, EventID: "nope"}, provider.RespondAccept, ""); !errors.Is(err, provider.ErrNotFound) {
			t.Errorf("responding to a missing event err = %v, want ErrNotFound", err)
		}
	})

	t.Run("WriteFailuresMapToSharedErrors", func(t *testing.T) {
		p, s := factory(t)
		prim := primaryID(t, p)
		ev, _ := p.CreateEvent(t.Context(), prim, newInput(t, "x"), provider.CreateOptions{})
		calls := map[string]func() error{
			"CreateEvent": func() error {
				_, err := p.CreateEvent(t.Context(), prim, newInput(t, "y"), provider.CreateOptions{})
				return err
			},
			"UpdateEvent": func() error {
				_, err := p.UpdateEvent(t.Context(), ref(prim, ev), provider.ScopeInstance, provider.EventPatch{Title: str("z")}, ev.Version)
				return err
			},
			"DeleteEvent": func() error {
				return p.DeleteEvent(t.Context(), ref(prim, ev), provider.ScopeInstance, ev.Version)
			},
			"Respond": func() error { return p.Respond(t.Context(), ref(prim, ev), provider.RespondAccept, "") },
		}
		s.Fail(t, FailAuth)
		for name, call := range calls {
			if err := call(); !errors.Is(err, provider.ErrAuth) {
				t.Errorf("%s with a revoked credential err = %v, want ErrAuth", name, err)
			}
		}
		s.Fail(t, FailRateLimit)
		for name, call := range calls {
			err := call()
			var rl *provider.RateLimitError
			if !errors.Is(err, provider.ErrRateLimited) || !errors.As(err, &rl) || rl.RetryAfter != RetryAfter {
				t.Errorf("%s when throttled err = %v, want a RateLimitError with Retry-After %s", name, err, RetryAfter)
			}
		}
		s.Fail(t, FailNone)
		if got, err := p.GetEvent(t.Context(), ref(prim, ev)); err != nil || got.Title != "x" {
			t.Errorf("failed writes changed the event: %+v, %v", got, err)
		}
	})
}
