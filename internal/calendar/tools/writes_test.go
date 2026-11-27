package tools_test

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/change-fabric/moose/internal/accounts/store"
	"github.com/change-fabric/moose/internal/calendar/fake"
	"github.com/change-fabric/moose/internal/calendar/provider"
	"github.com/change-fabric/moose/internal/calendar/tools"
)

func berlinTime(h int) map[string]any {
	return map[string]any{"date_time": "2026-11-25T" + two(h) + ":00:00+01:00", "zone": "Europe/Berlin"}
}

func two(n int) string {
	if n < 10 {
		return "0" + string(rune('0'+n))
	}
	return string(rune('0'+n/10)) + string(rune('0'+n%10))
}

func create(extra ...any) map[string]any {
	return with(map[string]any{"account": "full@example.com", "title": "Focus", "start": berlinTime(9), "end": berlinTime(10)}, extra...)
}

func evID(acc string, ev provider.Event) string { return acc + ":" + fake.PrimaryID + "|" + ev.ID }

func countEvents(t *testing.T, p *fake.Provider) int {
	t.Helper()
	page, err := p.ListEvents(context.Background(), provider.ListEventsQuery{
		Start: utc(0, 0).DateTime.AddDate(0, 0, -400), End: utc(0, 0).DateTime.AddDate(0, 0, 400), Limit: 500})
	if err != nil {
		t.Fatal(err)
	}
	return len(page.Events)
}

// ---- calendar_create_event ----

func TestCreateAppliesDirectlyForASelfOnlyEvent(t *testing.T) {
	d, src, _ := deps(t)
	rem := 15
	out, errText := run[tools.WriteOut](t, d, tools.ToolCreateEvent, create(
		"description", "deep work", "location", "Desk", "video", true, "reminder_minutes", rem,
		"attendees", []any{map[string]any{"email": "full@example.com"}},
		"recurrence", map[string]any{"freq": "WEEKLY", "by_day": []any{"MO", "WE"}, "count": 4}))
	if errText != "" {
		t.Fatal(errText)
	}
	if out.Status != "created" || out.Event == nil || out.Event.Video != true || out.Event.ReminderMinutes == nil || *out.Event.ReminderMinutes != 15 ||
		out.Event.Recurrence == nil || out.Event.Recurrence.Freq != "WEEKLY" || out.Event.Recurrence.Count != 4 {
		t.Fatalf("out = %+v", out)
	}
	if !strings.HasPrefix(out.Event.ID, "acc_full:") || out.Event.CalendarID != "acc_full:"+fake.PrimaryID {
		t.Errorf("ids = %q %q", out.Event.ID, out.Event.CalendarID)
	}
	if got := src.byID["acc_full"].Notifications(); len(got) != 0 {
		t.Errorf("a self-only create notified %+v", got)
	}
	if n := countEvents(t, src.byID["acc_full"]); n != 4 {
		t.Errorf("%d occurrences stored, want the 4 of the series", n)
	}
}

func TestCreateWithAnotherAttendeeIsInvitesNotEnabledAndWritesNothing(t *testing.T) {
	d, src, _ := deps(t)
	for _, att := range [][]any{
		{map[string]any{"email": "bob@example.com"}},
		{map[string]any{"email": "full@example.com"}, map[string]any{"email": "Bob@Example.com"}},
	} {
		_, errText := run[tools.WriteOut](t, d, tools.ToolCreateEvent, create("attendees", att))
		if !strings.HasPrefix(errText, "invites_not_enabled") {
			t.Errorf("errText = %q, want invites_not_enabled", errText)
		}
	}
	if n := countEvents(t, src.byID["acc_full"]); n != 0 {
		t.Errorf("%d events were written", n)
	}
	if got := src.byID["acc_full"].Notifications(); len(got) != 0 {
		t.Errorf("notified %+v", got)
	}
}

func TestWritesNeverGuessAnAccount(t *testing.T) {
	d, src, _ := deps(t)
	_, errText := run[tools.WriteOut](t, d, tools.ToolCreateEvent, with(create(), "account", ""))
	if !strings.HasPrefix(errText, "needs_account") || !strings.Contains(errText, "full@example.com") {
		t.Errorf("no account = %q, want needs_account with candidates", errText)
	}
	if strings.Contains(errText, "mail@example.com") || strings.Contains(errText, "part@example.com") {
		t.Errorf("candidates %q list an account that cannot be written", errText)
	}
	_, errText = run[tools.WriteOut](t, d, tools.ToolCreateEvent, with(create(), "account", "nobody@example.com"))
	if !strings.HasPrefix(errText, "account_not_found") {
		t.Errorf("unknown account = %q", errText)
	}
	_, errText = run[tools.WriteOut](t, d, tools.ToolCreateEvent, with(create(), "account", "all"))
	if !strings.HasPrefix(errText, "invalid_input") {
		t.Errorf("all accounts = %q, want invalid_input", errText)
	}
	_, errText = run[tools.WriteOut](t, d, tools.ToolCreateEvent, with(create(), "account", "part@example.com"))
	if !strings.HasPrefix(errText, "calendar_not_granted") {
		t.Errorf("partial grant = %q", errText)
	}
	_, errText = run[tools.WriteOut](t, d, tools.ToolCreateEvent, with(create(), "account", "mail@example.com"))
	if !strings.HasPrefix(errText, "needs_reauth") {
		t.Errorf("needs_reauth account = %q", errText)
	}
	_, errText = run[tools.WriteOut](t, d, tools.ToolCreateEvent, with(create(), "account", "theirs@example.com"))
	if !strings.HasPrefix(errText, "account_not_found") {
		t.Errorf("another owner's account = %q", errText)
	}
	for id, p := range src.byID {
		if n := countEvents(t, p); n != 0 {
			t.Errorf("%s has %d events after refused writes", id, n)
		}
	}
}

func TestCreateOnANamedCalendarNeedsNoAccountButThatCalendarsAccount(t *testing.T) {
	d, src, _ := deps(t)
	src.byID["acc_msw"].AddCalendar(provider.Calendar{ID: "work2", Name: "Work 2", Role: provider.RoleOwner})
	out, errText := run[tools.WriteOut](t, d, tools.ToolCreateEvent, with(create(), "account", "", "calendar", "acc_msw:work2"))
	if errText != "" || out.Event == nil || !strings.HasPrefix(out.Event.ID, "acc_msw:work2|") {
		t.Fatalf("out = %+v, %q", out, errText)
	}
	_, errText = run[tools.WriteOut](t, d, tools.ToolCreateEvent, with(create(), "account", "full@example.com", "calendar", "acc_msw:work2"))
	if !strings.HasPrefix(errText, "invalid_input") {
		t.Errorf("a calendar of another account = %q", errText)
	}
	_, errText = run[tools.WriteOut](t, d, tools.ToolCreateEvent, with(create(), "calendar", "not-composite"))
	if !strings.HasPrefix(errText, "invalid_input") {
		t.Errorf("bad calendar id = %q", errText)
	}
	src.byID["acc_full"].AddCalendar(provider.Calendar{ID: "ro", Name: "RO", Role: provider.RoleReader})
	_, errText = run[tools.WriteOut](t, d, tools.ToolCreateEvent, with(create(), "account", "", "calendar", "acc_full:ro"))
	if !strings.HasPrefix(errText, "rejected") {
		t.Errorf("read-only calendar = %q, want rejected", errText)
	}
}

func TestCreateRefusesBadInputBeforeWriting(t *testing.T) {
	d, src, _ := deps(t)
	cases := map[string]map[string]any{
		"no title":           with(create(), "title", "  "),
		"end before start":   with(create(), "end", berlinTime(8)),
		"abbreviated zone":   with(create(), "start", map[string]any{"date_time": "2026-11-25T09:00:00-05:00", "zone": "EST"}),
		"no zone":            with(create(), "start", map[string]any{"date_time": "2026-11-25T09:00:00+01:00"}),
		"no offset":          with(create(), "start", map[string]any{"date_time": "2026-11-25T09:00:00", "zone": "Europe/Berlin"}),
		"date and date_time": with(create(), "start", map[string]any{"date": "2026-11-25", "date_time": "2026-11-25T09:00:00+01:00", "zone": "Europe/Berlin"}),
		"mixed forms":        with(create(), "end", map[string]any{"date": "2026-11-26"}),
		"reminder too long":  with(create(), "reminder_minutes", 99999),
		"key too long":       with(create(), "idempotency_key", strings.Repeat("k", 200)),
		"bad attendee":       with(create(), "attendees", []any{map[string]any{"email": "full"}}),
	}
	for name, args := range cases {
		_, errText := run[tools.WriteOut](t, d, tools.ToolCreateEvent, args)
		if !strings.HasPrefix(errText, "invalid_input") && !strings.HasPrefix(errText, "rejected") {
			t.Errorf("%s: errText = %q", name, errText)
		}
	}
	for name, rule := range map[string]map[string]any{
		"native rule":     {"freq": "HOURLY"},
		"by_day on daily": {"freq": "DAILY", "by_day": []any{"MO"}},
		"until and count": {"freq": "DAILY", "until": "2026-12-01", "count": 3},
	} {
		_, errText := run[tools.WriteOut](t, d, tools.ToolCreateEvent, with(create(), "recurrence", rule))
		// The schema refuses an unknown freq itself; the rest reach the provider rules.
		if !strings.HasPrefix(errText, "invalid_recurrence") && !strings.Contains(errText, "enum") {
			t.Errorf("%s: errText = %q, want invalid_recurrence", name, errText)
		}
	}
	if n := countEvents(t, src.byID["acc_full"]); n != 0 {
		t.Errorf("%d events written by refused creates", n)
	}
}

func TestCreateVideoOnAPersonalMicrosoftAccountIsUnsupportedHere(t *testing.T) {
	d, src, _ := deps(t)
	src.byID["acc_msp"].SetCapabilities(provider.Capabilities{FreeBusyOthers: false, Video: false, Recurrence: true})
	_, errText := run[tools.WriteOut](t, d, tools.ToolCreateEvent, with(create(), "account", "me@outlook.example", "video", true))
	if !strings.HasPrefix(errText, "unsupported_here") {
		t.Errorf("errText = %q", errText)
	}
	if n := countEvents(t, src.byID["acc_msp"]); n != 0 {
		t.Errorf("%d events", n)
	}
	// The same event without video works on that account.
	if _, errText := run[tools.WriteOut](t, d, tools.ToolCreateEvent, with(create(), "account", "me@outlook.example")); errText != "" {
		t.Error(errText)
	}
}

func TestCreateIdempotencyKeyReturnsTheFirstEvent(t *testing.T) {
	d, src, _ := deps(t)
	first, errText := run[tools.WriteOut](t, d, tools.ToolCreateEvent, create("idempotency_key", "plan-1"))
	if errText != "" {
		t.Fatal(errText)
	}
	again, errText := run[tools.WriteOut](t, d, tools.ToolCreateEvent, create("idempotency_key", "plan-1"))
	if errText != "" {
		t.Fatal(errText)
	}
	if !again.Replay || again.Event == nil || again.Event.ID != first.Event.ID || first.Replay {
		t.Errorf("first = %+v, again = %+v, want a replay of the same event", first, again)
	}
	if n := countEvents(t, src.byID["acc_full"]); n != 1 {
		t.Errorf("%d events after a retried create, want 1", n)
	}
	// The same key with different details is a mistake, not a replay.
	_, errText = run[tools.WriteOut](t, d, tools.ToolCreateEvent, create("idempotency_key", "plan-1", "title", "Something else"))
	if !strings.HasPrefix(errText, "idempotency_key_reused") {
		t.Errorf("errText = %q", errText)
	}
	// Keys are per account: the same key on another account is a new create.
	other, errText := run[tools.WriteOut](t, d, tools.ToolCreateEvent, with(create("idempotency_key", "plan-1"), "account", "me@contoso.example"))
	if errText != "" || other.Replay || other.Event == nil {
		t.Errorf("other account = %+v, %q", other, errText)
	}
	// A deleted event leaves the key spent, never silently re-created.
	if _, errText := run[tools.WriteOut](t, d, tools.ToolDeleteEvent, map[string]any{"id": first.Event.ID}); errText != "" {
		t.Fatal(errText)
	}
	_, errText = run[tools.WriteOut](t, d, tools.ToolCreateEvent, create("idempotency_key", "plan-1"))
	if !strings.HasPrefix(errText, "idempotency_key_spent") {
		t.Errorf("errText = %q, want idempotency_key_spent", errText)
	}
}

func TestACreateThatFailedBeforeWritingReleasesItsKey(t *testing.T) {
	d, src, _ := deps(t)
	src.byID["acc_full"].Err = provider.ErrAuth
	_, errText := run[tools.WriteOut](t, d, tools.ToolCreateEvent, create("idempotency_key", "retry-me"))
	if !strings.HasPrefix(errText, "needs_reauth") {
		t.Fatalf("errText = %q", errText)
	}
	src.byID["acc_full"].Err = nil
	out, errText := run[tools.WriteOut](t, d, tools.ToolCreateEvent, create("idempotency_key", "retry-me"))
	if errText != "" || out.Replay || out.Event == nil {
		t.Errorf("retry after a refused create = %+v, %q", out, errText)
	}
}

// A create whose outcome was never seen leaves the key without an event id; the
// retry goes to the provider again with the same dedupe key and finds the event
// the first call made.
func TestARetryAfterAnUnseenCreateFindsTheSameEvent(t *testing.T) {
	d, src, st := deps(t)
	var mu sync.Mutex
	lost := true
	src.wrap = func(id string, p provider.Provider) provider.Provider {
		return &hook{Provider: p, afterCreate: func() error {
			mu.Lock()
			defer mu.Unlock()
			if lost {
				lost = false
				return context.DeadlineExceeded // the provider made the event; the answer never arrived
			}
			return nil
		}}
	}
	_, errText := run[tools.WriteOut](t, d, tools.ToolCreateEvent, create("idempotency_key", "lost-answer"))
	if !strings.HasPrefix(errText, "timeout") {
		t.Fatalf("errText = %q", errText)
	}
	if n := countEvents(t, src.byID["acc_full"]); n != 1 {
		t.Fatalf("%d events after the lost answer, want the one the provider made", n)
	}
	out, errText := run[tools.WriteOut](t, d, tools.ToolCreateEvent, create("idempotency_key", "lost-answer"))
	if errText != "" || out.Event == nil {
		t.Fatal(errText)
	}
	if n := countEvents(t, src.byID["acc_full"]); n != 1 {
		t.Errorf("%d events after the retry, want 1: the dedupe key must find the first event", n)
	}
	rec, _, err := st.ReserveCalendarIdempotency(context.Background(), store.CalendarIdempotency{AccountID: "acc_full", Key: "lost-answer", RequestHash: "x"})
	if err == nil || rec.EventID == "" {
		t.Errorf("idempotency record = %+v, %v; want the event id recorded after the retry", rec, err)
	}
}

// hook intercepts writes to model concurrent changes and lost answers.
type hook struct {
	provider.Provider
	beforeUpdate func(ref provider.EventRef)
	beforeDelete func(ref provider.EventRef)
	afterCreate  func() error
}

func (h *hook) CreateEvent(ctx context.Context, cal string, in provider.EventInput, o provider.CreateOptions) (provider.Event, error) {
	ev, err := h.Provider.CreateEvent(ctx, cal, in, o)
	if err == nil && h.afterCreate != nil {
		if aerr := h.afterCreate(); aerr != nil {
			return provider.Event{}, aerr
		}
	}
	return ev, err
}

func (h *hook) UpdateEvent(ctx context.Context, ref provider.EventRef, s provider.Scope, p provider.EventPatch, v string) (provider.Event, error) {
	if h.beforeUpdate != nil {
		h.beforeUpdate(ref)
	}
	return h.Provider.UpdateEvent(ctx, ref, s, p, v)
}

func (h *hook) DeleteEvent(ctx context.Context, ref provider.EventRef, s provider.Scope, v string) error {
	if h.beforeDelete != nil {
		h.beforeDelete(ref)
	}
	return h.Provider.DeleteEvent(ctx, ref, s, v)
}

// ---- calendar_update_event ----

func seedSelf(src *source, acc string, title string) provider.Event {
	return src.byID[acc].AddEvent(fake.PrimaryID, timed(title, 9))
}

func TestUpdateAppliesDirectlyToASelfOnlyEvent(t *testing.T) {
	d, src, _ := deps(t)
	ev := seedSelf(src, "acc_full", "Old")
	out, errText := run[tools.WriteOut](t, d, tools.ToolUpdateEvent, map[string]any{
		"id": evID("acc_full", ev), "title": "New", "location": "Desk", "start": berlinTime(11), "end": berlinTime(12), "reminder_minutes": 5,
	})
	if errText != "" {
		t.Fatal(errText)
	}
	if out.Status != "updated" || out.Event == nil || !strings.Contains(out.Event.Content, "Title: New") || !strings.Contains(out.Event.Content, "Location: Desk") ||
		out.Event.ReminderMinutes == nil || *out.Event.ReminderMinutes != 5 || out.Event.Start.DateTime.Hour() != 11 {
		t.Fatalf("out = %+v", out)
	}
	if got := src.byID["acc_full"].Notifications(); len(got) != 0 {
		t.Errorf("notified %+v", got)
	}
	_, errText = run[tools.WriteOut](t, d, tools.ToolUpdateEvent, map[string]any{"id": evID("acc_full", ev)})
	if !strings.HasPrefix(errText, "invalid_input") {
		t.Errorf("an update that changes nothing = %q", errText)
	}
}

func TestUpdateOfAnEventWithAttendeesOrAddingOneIsInvitesNotEnabled(t *testing.T) {
	d, src, _ := deps(t)
	guests := src.byID["acc_full"].AddEvent(fake.PrimaryID, provider.Event{Title: "Team", Start: utc(9, 0), End: utc(10, 0),
		Attendees: []provider.Attendee{{Email: "bob@example.com"}}})
	_, errText := run[tools.WriteOut](t, d, tools.ToolUpdateEvent, map[string]any{"id": evID("acc_full", guests), "title": "Changed"})
	if !strings.HasPrefix(errText, "invites_not_enabled") {
		t.Errorf("existing attendee: %q", errText)
	}
	// Removing the only other attendee still counts: the union of before and after.
	_, errText = run[tools.WriteOut](t, d, tools.ToolUpdateEvent, map[string]any{"id": evID("acc_full", guests), "attendees": []any{}})
	if !strings.HasPrefix(errText, "invites_not_enabled") {
		t.Errorf("removing the attendee: %q", errText)
	}
	solo := seedSelf(src, "acc_full", "Solo")
	_, errText = run[tools.WriteOut](t, d, tools.ToolUpdateEvent, map[string]any{"id": evID("acc_full", solo), "attendees": []any{map[string]any{"email": "carol@example.com"}}})
	if !strings.HasPrefix(errText, "invites_not_enabled") {
		t.Errorf("adding an attendee: %q", errText)
	}
	// Someone else's meeting: the account is only invited, so changing it is not self-only.
	theirs := src.byID["acc_full"].AddEvent(fake.PrimaryID, provider.Event{Title: "Theirs", Start: utc(9, 0), End: utc(10, 0), Organizer: provider.Attendee{Email: "boss@example.com"}})
	_, errText = run[tools.WriteOut](t, d, tools.ToolUpdateEvent, map[string]any{"id": evID("acc_full", theirs), "title": "Mine now"})
	if !strings.HasPrefix(errText, "invites_not_enabled") {
		t.Errorf("another organizer: %q", errText)
	}
	for _, e := range []provider.Event{guests, solo, theirs} {
		got, _ := src.byID["acc_full"].GetEvent(context.Background(), provider.EventRef{CalendarID: fake.PrimaryID, EventID: e.ID})
		if got.Title != e.Title || len(got.Attendees) != len(e.Attendees) {
			t.Errorf("refused update changed %+v to %+v", e, got)
		}
	}
	if got := src.byID["acc_full"].Notifications(); len(got) != 0 {
		t.Errorf("notified %+v", got)
	}
}

// Someone else adds an attendee between the tool's read and its write. The
// conditional write is refused (412), the tool reads again, sees the attendee
// and stops; nothing is written and nobody is notified.
func TestAnAttendeeAddedBetweenReadAndWriteIsReclassifiedAndNotWritten(t *testing.T) {
	for _, tc := range []struct {
		name string
		tool string
		args func(id string) map[string]any
	}{
		{"update", tools.ToolUpdateEvent, func(id string) map[string]any { return map[string]any{"id": id, "title": "Changed"} }},
		{"delete", tools.ToolDeleteEvent, func(id string) map[string]any { return map[string]any{"id": id} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, src, _ := deps(t)
			ev := seedSelf(src, "acc_full", "Original")
			fired := 0
			trigger := func(ref provider.EventRef) {
				if fired == 0 {
					fired++
					src.byID["acc_full"].ExternalAddAttendee(ref, provider.Attendee{Email: "mallory@example.com"})
				}
			}
			src.wrap = func(id string, p provider.Provider) provider.Provider {
				return &hook{Provider: p, beforeUpdate: trigger, beforeDelete: trigger}
			}
			_, errText := run[tools.WriteOut](t, d, tc.tool, tc.args(evID("acc_full", ev)))
			if !strings.HasPrefix(errText, "invites_not_enabled") {
				t.Fatalf("errText = %q, want invites_not_enabled after the 412 re-read found the new attendee", errText)
			}
			got, err := src.byID["acc_full"].GetEvent(context.Background(), provider.EventRef{CalendarID: fake.PrimaryID, EventID: ev.ID})
			if err != nil || got.Title != "Original" {
				t.Errorf("event = %+v, %v: the refused write must leave it as it was", got, err)
			}
			if n := src.byID["acc_full"].Notifications(); len(n) != 0 {
				t.Errorf("notified %+v", n)
			}
		})
	}
}

// A concurrent change that adds nobody (a title edit in another client) is
// retried once at the new version: the call is still self-only.
func TestAConcurrentNonAttendeeChangeIsRetriedOnceAtTheNewVersion(t *testing.T) {
	d, src, _ := deps(t)
	ev := seedSelf(src, "acc_full", "Original")
	fired := 0
	src.wrap = func(id string, p provider.Provider) provider.Provider {
		return &hook{Provider: p, beforeUpdate: func(ref provider.EventRef) {
			if fired == 0 {
				fired++
				cur, _ := src.byID["acc_full"].GetEvent(context.Background(), provider.EventRef{CalendarID: fake.PrimaryID, EventID: ev.ID})
				loc := "moved elsewhere"
				_, _ = src.byID["acc_full"].UpdateEvent(context.Background(), ref, provider.ScopeInstance, provider.EventPatch{Location: &loc}, cur.Version)
			}
		}}
	}
	out, errText := run[tools.WriteOut](t, d, tools.ToolUpdateEvent, map[string]any{"id": evID("acc_full", ev), "title": "Changed"})
	if errText != "" || out.Event == nil || !strings.Contains(out.Event.Content, "Title: Changed") || !strings.Contains(out.Event.Content, "moved elsewhere") {
		t.Fatalf("out = %+v, %q", out, errText)
	}
}

func refOf(ev provider.Event) provider.EventRef {
	return provider.EventRef{CalendarID: fake.PrimaryID, EventID: ev.ID}
}

func TestUpdateKeepsChurningEventsFromLoopingForever(t *testing.T) {
	d, src, _ := deps(t)
	ev := seedSelf(src, "acc_full", "Busy")
	src.wrap = func(id string, p provider.Provider) provider.Provider {
		return &hook{Provider: p, beforeUpdate: func(r provider.EventRef) {
			cur, _ := src.byID["acc_full"].GetEvent(context.Background(), refOf(ev))
			loc := cur.Location + "!"
			_, _ = src.byID["acc_full"].UpdateEvent(context.Background(), r, provider.ScopeInstance, provider.EventPatch{Location: &loc}, cur.Version)
		}}
	}
	_, errText := run[tools.WriteOut](t, d, tools.ToolUpdateEvent, map[string]any{"id": evID("acc_full", ev), "title": "Changed"})
	if !strings.HasPrefix(errText, "event_changed") {
		t.Errorf("errText = %q, want event_changed after two lost races", errText)
	}
	got, _ := src.byID["acc_full"].GetEvent(context.Background(), refOf(ev))
	if got.Title != "Busy" {
		t.Errorf("title = %q: the lost races must not write", got.Title)
	}
}

func recurring(src *source, acc string) (provider.Event, []provider.Event) {
	p := src.byID[acc]
	master := p.AddEvent(fake.PrimaryID, provider.Event{Title: "Standup", Start: utc(9, 0), End: utc(10, 0),
		Recurrence: &provider.Recurrence{Freq: provider.FreqDaily, Count: 3}})
	page, _ := p.ListEvents(context.Background(), provider.ListEventsQuery{Start: utc(0, 0).DateTime, End: utc(0, 0).DateTime.AddDate(0, 0, 10)})
	return master, page.Events
}

func TestUpdateScopeInstanceAndSeries(t *testing.T) {
	d, src, _ := deps(t)
	master, occ := recurring(src, "acc_full")
	if len(occ) != 3 {
		t.Fatalf("%d occurrences", len(occ))
	}
	out, errText := run[tools.WriteOut](t, d, tools.ToolUpdateEvent, map[string]any{"id": evID("acc_full", occ[1]), "title": "Standup (moved)"})
	if errText != "" || out.Event == nil {
		t.Fatal(errText)
	}
	titles := func() []string {
		page, _ := src.byID["acc_full"].ListEvents(context.Background(), provider.ListEventsQuery{Start: utc(0, 0).DateTime, End: utc(0, 0).DateTime.AddDate(0, 0, 10)})
		var out []string
		for _, e := range page.Events {
			out = append(out, e.Title)
		}
		return out
	}
	if got := strings.Join(titles(), "|"); got != "Standup|Standup (moved)|Standup" {
		t.Fatalf("titles = %s", got)
	}
	// The whole series, named through an occurrence.
	occ = func() []provider.Event {
		page, _ := src.byID["acc_full"].ListEvents(context.Background(), provider.ListEventsQuery{Start: utc(0, 0).DateTime, End: utc(0, 0).DateTime.AddDate(0, 0, 10)})
		return page.Events
	}()
	if _, errText := run[tools.WriteOut](t, d, tools.ToolUpdateEvent, map[string]any{"id": evID("acc_full", occ[0]), "scope": "series", "location": "HQ"}); errText != "" {
		t.Fatal(errText)
	}
	m, _ := src.byID["acc_full"].GetEvent(context.Background(), refOf(master))
	if m.Location != "HQ" {
		t.Errorf("series location = %q", m.Location)
	}
	// Refusals.
	_, errText = run[tools.WriteOut](t, d, tools.ToolUpdateEvent, map[string]any{"id": evID("acc_full", master), "title": "x"})
	if !strings.HasPrefix(errText, "invalid_input") || !strings.Contains(errText, "series") {
		t.Errorf("instance scope on a master = %q", errText)
	}
	_, errText = run[tools.WriteOut](t, d, tools.ToolUpdateEvent, map[string]any{"id": evID("acc_full", occ[0]), "scope": "following", "title": "x"})
	if !strings.Contains(errText, "enum") && !strings.HasPrefix(errText, "invalid_input") {
		t.Errorf("following = %q", errText)
	}
	_, errText = run[tools.WriteOut](t, d, tools.ToolUpdateEvent, map[string]any{"id": evID("acc_full", occ[0]), "recurrence": map[string]any{"freq": "DAILY"}})
	if !strings.HasPrefix(errText, "invalid_input") {
		t.Errorf("an occurrence changing the series' rule = %q", errText)
	}
	// The series' rule changes through the series scope.
	if _, errText := run[tools.WriteOut](t, d, tools.ToolUpdateEvent, map[string]any{"id": evID("acc_full", master), "scope": "series", "recurrence": map[string]any{"freq": "DAILY", "count": 2}}); errText != "" {
		t.Error(errText)
	}
}

func TestUpdateAndDeleteRefuseAnotherOwnersOrUnknownEvents(t *testing.T) {
	d, src, _ := deps(t)
	ev := seedSelf(src, "acc_full", "x")
	for _, tool := range []string{tools.ToolUpdateEvent, tools.ToolDeleteEvent, tools.ToolRespondToInvite} {
		args := map[string]any{"id": "acc_theirs:" + fake.PrimaryID + "|" + ev.ID, "title": "x", "response": "accept"}
		if _, errText := run[map[string]any](t, d, tool, args); !strings.HasPrefix(errText, "account_not_found") {
			t.Errorf("%s on another owner's account = %q", tool, errText)
		}
		args["id"] = "acc_full:" + fake.PrimaryID + "|missing"
		if _, errText := run[map[string]any](t, d, tool, args); !strings.HasPrefix(errText, "not_found") {
			t.Errorf("%s on a missing event = %q", tool, errText)
		}
		args["id"] = "garbage"
		if _, errText := run[map[string]any](t, d, tool, args); !strings.HasPrefix(errText, "invalid_input") {
			t.Errorf("%s on a bad id = %q", tool, errText)
		}
		args["id"] = evID("acc_part", ev)
		if _, errText := run[map[string]any](t, d, tool, args); !strings.HasPrefix(errText, "calendar_not_granted") {
			t.Errorf("%s on a partial grant = %q", tool, errText)
		}
	}
}

// ---- calendar_delete_event ----

func TestDeleteAppliesDirectlyToASelfOnlyEventAndRefusesOthers(t *testing.T) {
	d, src, _ := deps(t)
	solo := seedSelf(src, "acc_full", "Solo")
	guests := src.byID["acc_full"].AddEvent(fake.PrimaryID, provider.Event{Title: "Team", Start: utc(9, 0), End: utc(10, 0),
		Attendees: []provider.Attendee{{Email: "bob@example.com"}}})
	theirs := src.byID["acc_full"].AddEvent(fake.PrimaryID, provider.Event{Title: "Theirs", Start: utc(9, 0), End: utc(10, 0), Organizer: provider.Attendee{Email: "boss@example.com"}})
	out, errText := run[tools.WriteOut](t, d, tools.ToolDeleteEvent, map[string]any{"id": evID("acc_full", solo)})
	if errText != "" || out.Status != "deleted" || out.DeletedID != evID("acc_full", solo) {
		t.Fatalf("out = %+v, %q", out, errText)
	}
	if _, err := src.byID["acc_full"].GetEvent(context.Background(), refOf(solo)); err == nil {
		t.Error("the event is still there")
	}
	for _, e := range []provider.Event{guests, theirs} {
		_, errText = run[tools.WriteOut](t, d, tools.ToolDeleteEvent, map[string]any{"id": evID("acc_full", e)})
		if !strings.HasPrefix(errText, "invites_not_enabled") {
			t.Errorf("delete of %q = %q", e.Title, errText)
		}
		if _, err := src.byID["acc_full"].GetEvent(context.Background(), refOf(e)); err != nil {
			t.Errorf("%q was deleted: %v", e.Title, err)
		}
	}
	if got := src.byID["acc_full"].Notifications(); len(got) != 0 {
		t.Errorf("notified %+v", got)
	}
}

func TestDeleteScopes(t *testing.T) {
	d, src, _ := deps(t)
	master, occ := recurring(src, "acc_full")
	if _, errText := run[tools.WriteOut](t, d, tools.ToolDeleteEvent, map[string]any{"id": evID("acc_full", occ[1])}); errText != "" {
		t.Fatal(errText)
	}
	page, _ := src.byID["acc_full"].ListEvents(context.Background(), provider.ListEventsQuery{Start: utc(0, 0).DateTime, End: utc(0, 0).DateTime.AddDate(0, 0, 10)})
	if len(page.Events) != 2 {
		t.Fatalf("%d occurrences after cancelling one", len(page.Events))
	}
	_, errText := run[tools.WriteOut](t, d, tools.ToolDeleteEvent, map[string]any{"id": evID("acc_full", master)})
	if !strings.HasPrefix(errText, "invalid_input") {
		t.Errorf("instance scope on a master = %q", errText)
	}
	if _, errText := run[tools.WriteOut](t, d, tools.ToolDeleteEvent, map[string]any{"id": evID("acc_full", page.Events[0]), "scope": "series"}); errText != "" {
		t.Fatal(errText)
	}
	if n := countEvents(t, src.byID["acc_full"]); n != 0 {
		t.Errorf("%d events after deleting the series", n)
	}
	_, errText = run[tools.WriteOut](t, d, tools.ToolDeleteEvent, map[string]any{"id": evID("acc_full", master), "scope": "following"})
	if !strings.Contains(errText, "enum") && !strings.HasPrefix(errText, "invalid_input") {
		t.Errorf("following = %q", errText)
	}
}

// ---- calendar_respond_to_invite ----

func invitation(src *source, acc, self string) provider.Event {
	return src.byID[acc].AddEvent(fake.PrimaryID, provider.Event{
		Title: "Their meeting", Start: utc(9, 0), End: utc(10, 0), Organizer: provider.Attendee{Email: "boss@example.com", Name: "Boss"},
		Attendees: []provider.Attendee{{Email: "boss@example.com", Response: provider.ResponseAccepted}, {Email: self, Response: provider.ResponseNone}},
	})
}

func TestRespondAppliesDirectlyAndTellsTheOrganizer(t *testing.T) {
	d, src, _ := deps(t)
	inv := invitation(src, "acc_full", "full@example.com")
	out, errText := run[tools.RespondOut](t, d, tools.ToolRespondToInvite, map[string]any{"id": evID("acc_full", inv), "response": "tentative", "comment": "  Might be late\n"})
	if errText != "" {
		t.Fatal(errText)
	}
	if out.Status != "responded" || out.Response != "tentative" || out.NotifiedOrganizer != "boss@example.com" || out.PreviousResponse != provider.ResponseNone {
		t.Errorf("out = %+v", out)
	}
	got, _ := src.byID["acc_full"].GetEvent(context.Background(), refOf(inv))
	if got.SelfResponse != provider.ResponseTentative {
		t.Errorf("self response = %q", got.SelfResponse)
	}
	notes := src.byID["acc_full"].Notifications()
	if len(notes) != 1 || notes[0].Kind != "rsvp" || len(notes[0].To) != 1 || notes[0].To[0] != "boss@example.com" {
		t.Errorf("notifications = %+v, want one rsvp to the organizer only", notes)
	}
	// No comment is fine, and so is changing one's mind.
	if _, errText := run[tools.RespondOut](t, d, tools.ToolRespondToInvite, map[string]any{"id": evID("acc_full", inv), "response": "decline"}); errText != "" {
		t.Error(errText)
	}
}

func TestRespondCapsAndCleansTheComment(t *testing.T) {
	d, src, _ := deps(t)
	inv := invitation(src, "acc_full", "full@example.com")
	id := evID("acc_full", inv)
	for name, comment := range map[string]string{
		"over 500":  strings.Repeat("é", 501),
		"html":      "see <b>this</b>",
		"html tag":  "<script>alert(1)</script>",
		"close tag": "</div>",
	} {
		_, errText := run[tools.RespondOut](t, d, tools.ToolRespondToInvite, map[string]any{"id": id, "response": "accept", "comment": comment})
		if !strings.HasPrefix(errText, "invalid_input") {
			t.Errorf("%s: errText = %q", name, errText)
		}
	}
	if n := len(src.byID["acc_full"].Notifications()); n != 0 {
		t.Errorf("refused comments notified %d", n)
	}
	// Exactly 500 characters, a bare comparison and control characters are accepted as plain text.
	for name, comment := range map[string]string{
		"exactly 500": strings.Repeat("é", 500),
		"comparison":  "I need 5 < 10 minutes",
		"control":     "bell\x07 and nul\x00 gone",
	} {
		if _, errText := run[tools.RespondOut](t, d, tools.ToolRespondToInvite, map[string]any{"id": id, "response": "accept", "comment": comment}); errText != "" {
			t.Errorf("%s: %s", name, errText)
		}
	}
	if _, errText := run[tools.RespondOut](t, d, tools.ToolRespondToInvite, map[string]any{"id": id, "response": "maybe"}); !strings.Contains(errText, "enum") && !strings.HasPrefix(errText, "invalid_input") {
		t.Errorf("bad response = %q", errText)
	}
}

func TestRespondRefusesYourOwnEventAndAnEventYouAreNotInvitedTo(t *testing.T) {
	d, src, _ := deps(t)
	own := seedSelf(src, "acc_full", "Mine")
	_, errText := run[tools.RespondOut](t, d, tools.ToolRespondToInvite, map[string]any{"id": evID("acc_full", own), "response": "accept"})
	if !strings.HasPrefix(errText, "invalid_input") {
		t.Errorf("own event = %q", errText)
	}
	stranger := src.byID["acc_full"].AddEvent(fake.PrimaryID, provider.Event{Title: "Not for me", Start: utc(9, 0), End: utc(10, 0),
		Organizer: provider.Attendee{Email: "boss@example.com"}, Attendees: []provider.Attendee{{Email: "boss@example.com"}, {Email: "dan@example.com"}}})
	_, errText = run[tools.RespondOut](t, d, tools.ToolRespondToInvite, map[string]any{"id": evID("acc_full", stranger), "response": "accept"})
	if !strings.HasPrefix(errText, "rejected") {
		t.Errorf("not invited = %q", errText)
	}
	if n := len(src.byID["acc_full"].Notifications()); n != 0 {
		t.Errorf("notified %d", n)
	}
}

func TestWriteToolDescriptionsStateTheRules(t *testing.T) {
	cs := connect(t, tools.Deps{Store: seed(t), FallbackOwner: "pat"})
	list, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tl := range list.Tools {
		switch tl.Name {
		case tools.ToolRespondToInvite:
			if !strings.Contains(tl.Description, "Your reply notifies the organizer.") || !strings.Contains(tl.Description, "500 characters") {
				t.Errorf("respond description = %q", tl.Description)
			}
		case tools.ToolCreateEvent, tools.ToolUpdateEvent, tools.ToolDeleteEvent:
			if !strings.Contains(tl.Description, "invites_not_enabled") || !strings.Contains(tl.Description, "conditional on the version") && tl.Name != tools.ToolCreateEvent {
				t.Errorf("%s description = %q", tl.Name, tl.Description)
			}
		}
	}
}
