package inviteflow_test

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/change-fabric/moose/internal/accounts/envelope"
	"github.com/change-fabric/moose/internal/accounts/store"
	"github.com/change-fabric/moose/internal/accounts/store/sqlite"
	"github.com/change-fabric/moose/internal/approver/api"
	"github.com/change-fabric/moose/internal/approver/receipt"
	"github.com/change-fabric/moose/internal/calendar/fake"
	"github.com/change-fabric/moose/internal/calendar/inviteflow"
	"github.com/change-fabric/moose/internal/calendar/provider"
	"github.com/change-fabric/moose/internal/email/sendflow/flowtest"
)

var ctx = context.Background()

const (
	calEvents = "https://www.googleapis.com/auth/calendar.events"
	calRead   = "https://www.googleapis.com/auth/calendar.readonly"
)

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// faulty wraps the fake calendar so a write can fail the way a lost response
// does: after the provider applied it (lostAfter) or before it did (lostBefore).
// It also lets a test look at the ledger from inside the call.
type faulty struct {
	provider.Provider
	f *fake.Provider

	mu         sync.Mutex
	lostAfter  map[string]int
	lostBefore map[string]int
	calls      map[string]int
	noFinder   bool
	// during runs inside a write call, before it reaches the provider.
	during func(op string)
}

func newFaulty(f *fake.Provider) *faulty {
	return &faulty{Provider: f, f: f, lostAfter: map[string]int{}, lostBefore: map[string]int{}, calls: map[string]int{}}
}

func (p *faulty) begin(op string) (loseBefore bool) {
	p.mu.Lock()
	p.calls[op]++
	if p.lostBefore[op] > 0 {
		p.lostBefore[op]--
		loseBefore = true
	}
	during := p.during
	p.mu.Unlock()
	if during != nil {
		during(op)
	}
	return loseBefore
}

func (p *faulty) after(op string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.lostAfter[op] > 0 {
		p.lostAfter[op]--
		return errors.New("connection reset by peer")
	}
	return nil
}

func (p *faulty) count(op string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls[op]
}

var errReset = errors.New("connection reset by peer")

func (p *faulty) CreateEvent(c context.Context, cal string, in provider.EventInput, o provider.CreateOptions) (provider.Event, error) {
	if p.begin("create") {
		return provider.Event{}, errReset
	}
	ev, err := p.f.CreateEvent(c, cal, in, o)
	if err == nil {
		err = p.after("create")
	}
	return ev, err
}

func (p *faulty) UpdateEvent(c context.Context, ref provider.EventRef, s provider.Scope, patch provider.EventPatch, v string) (provider.Event, error) {
	if p.begin("update") {
		return provider.Event{}, errReset
	}
	ev, err := p.f.UpdateEvent(c, ref, s, patch, v)
	if err == nil {
		err = p.after("update")
	}
	return ev, err
}

func (p *faulty) DeleteEvent(c context.Context, ref provider.EventRef, s provider.Scope, v string) error {
	if p.begin("delete") {
		return errReset
	}
	err := p.f.DeleteEvent(c, ref, s, v)
	if err == nil {
		err = p.after("delete")
	}
	return err
}

// finder is the DedupeFinder half, hidden when noFinder is set.
type finderFaulty struct{ *faulty }

func (p finderFaulty) FindByDedupeKey(c context.Context, cal, key string) (provider.Event, bool, error) {
	return p.f.FindByDedupeKey(c, cal, key)
}

type providers struct {
	byID map[string]provider.Provider
}

func (p providers) CalendarProvider(_ context.Context, a store.Account) (provider.Provider, error) {
	if v, ok := p.byID[a.ID]; ok {
		return v, nil
	}
	return nil, errors.New("no provider for " + a.ID)
}

type rig struct {
	t    *testing.T
	st   store.Store
	ap   *flowtest.Approver
	fake *fake.Provider
	prov *faulty
	svc  *inviteflow.Service
	clk  *clock
	acct store.Account
}

func newRig(t *testing.T, live bool) *rig {
	t.Helper()
	st, err := sqlite.OpenStore(ctx, filepath.Join(t.TempDir(), "i.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	seal, err := envelope.NewLocal(bytes.Repeat([]byte("k"), 32))
	if err != nil {
		t.Fatal(err)
	}
	clk := &clock{t: time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)}
	ap := flowtest.New()
	ap.Now = clk.Now
	f := fake.New("me@example.com")
	acct := store.Account{Owner: "pat", ID: "acc_one", Provider: store.ProviderGoogle, ProviderUID: "me@example.com", Address: "me@example.com",
		Nickname: "Me", Status: store.StatusOK, InviteEnabled: true, HourlyCap: 10, Created: clk.Now(), Updated: clk.Now(),
		Services: map[string]store.ServiceGrant{store.ServiceCalendar: {Scopes: []string{calEvents, calRead}, GrantedAt: clk.Now()}}}
	if err := st.CreateAccount(ctx, acct); err != nil {
		t.Fatal(err)
	}
	r := &rig{t: t, st: st, ap: ap, fake: f, prov: newFaulty(f), clk: clk, acct: acct}
	r.svc = &inviteflow.Service{
		Store: st, Seal: seal, Approver: ap, Providers: providers{byID: map[string]provider.Provider{acct.ID: finderFaulty{r.prov}}},
		ReceiptKey: ap.PublicKeyPEM(), RPID: flowtest.RPID, Live: live, Now: clk.Now,
		Sleep: func(context.Context, time.Duration) error { return nil },
	}
	return r
}

func (r *rig) noFinder() {
	r.svc.Providers = providers{byID: map[string]provider.Provider{r.acct.ID: r.prov}}
}

func at(h int) provider.EventTime {
	return provider.EventTime{DateTime: time.Date(2026, 11, 26, h, 0, 0, 0, time.UTC), Zone: "UTC"}
}

func attendees(emails ...string) []provider.Attendee {
	var out []provider.Attendee
	for _, e := range emails {
		out = append(out, provider.Attendee{Email: e})
	}
	return out
}

// seed stores an event the account organizes with the given attendees.
func (r *rig) seed(title string, emails ...string) provider.Event {
	r.t.Helper()
	return r.fake.AddEvent(fake.PrimaryID, provider.Event{Title: title, Description: "agenda", Start: at(9), End: at(10), Attendees: attendees(emails...)})
}

func ref(ev provider.Event) provider.EventRef {
	return provider.EventRef{CalendarID: fake.PrimaryID, EventID: ev.ID}
}

func (r *rig) refusal(err error) *inviteflow.Error {
	r.t.Helper()
	var e *inviteflow.Error
	if !errors.As(err, &e) {
		r.t.Fatalf("err = %v, want an *inviteflow.Error", err)
	}
	return e
}

func (r *rig) create(title string, emails ...string) inviteflow.RequestOut {
	r.t.Helper()
	in := provider.EventInput{Title: title, Description: "d", Start: at(9), End: at(10), Attendees: attendees(emails...)}
	out, err := r.svc.RequestInvite(ctx, inviteflow.RequestIn{Account: r.acct, Operation: api.CalOpCreate, CalendarID: fake.PrimaryID, Event: &in})
	if err != nil {
		r.t.Fatalf("RequestInvite create: %v", err)
	}
	return out
}

func (r *rig) update(ev provider.Event, patch provider.EventPatch) (inviteflow.RequestOut, error) {
	return r.svc.RequestInvite(ctx, inviteflow.RequestIn{Account: r.acct, Operation: api.CalOpUpdate, Ref: ref(ev), Scope: provider.ScopeInstance, Patch: &patch})
}

func (r *rig) cancel(ev provider.Event) (inviteflow.RequestOut, error) {
	return r.svc.RequestInvite(ctx, inviteflow.RequestIn{Account: r.acct, Operation: api.CalOpCancel, Ref: ref(ev), Scope: provider.ScopeInstance})
}

func (r *rig) mustUpdate(ev provider.Event, patch provider.EventPatch) inviteflow.RequestOut {
	r.t.Helper()
	out, err := r.update(ev, patch)
	if err != nil {
		r.t.Fatalf("RequestInvite update: %v", err)
	}
	return out
}

func (r *rig) mustCancel(ev provider.Event) inviteflow.RequestOut {
	r.t.Helper()
	out, err := r.cancel(ev)
	if err != nil {
		r.t.Fatalf("RequestInvite cancel: %v", err)
	}
	return out
}

func (r *rig) status(id string) inviteflow.StatusOut {
	r.t.Helper()
	out, err := r.svc.Status(ctx, "pat", id)
	if err != nil {
		r.t.Fatalf("Status(%s): %v", id, err)
	}
	return out
}

func (r *rig) approved(out inviteflow.RequestOut) string {
	r.t.Helper()
	r.ap.Approve(out.RequestID)
	return out.RequestID
}

func (r *rig) payload(id string) api.CalendarInvitePayload {
	r.t.Helper()
	ap, err := r.st.GetApproval(ctx, id)
	if err != nil {
		r.t.Fatal(err)
	}
	p, err := api.ParsePayload(api.KindCalendarInvite, ap.Payload)
	if err != nil || p.Invite == nil {
		r.t.Fatalf("stored payload: %v", err)
	}
	return *p.Invite
}

func (r *rig) events() int {
	r.t.Helper()
	page, err := r.fake.ListEvents(ctx, provider.ListEventsQuery{Start: at(0).DateTime.AddDate(0, 0, -30), End: at(0).DateTime.AddDate(0, 0, 30), Limit: 500})
	if err != nil {
		r.t.Fatal(err)
	}
	return len(page.Events)
}

func (r *rig) notes(kind string) []string {
	var out []string
	for _, n := range r.fake.Notifications() {
		if n.Kind == kind {
			out = append(out, n.To...)
		}
	}
	return out
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// ---- the request ----

func TestCreateRequestCarriesTheWholeChangeAndCreatesNothing(t *testing.T) {
	r := newRig(t, true)
	out := r.create("Kickoff", "me@example.com", "Bob@Other.example", "carol@example.com")
	if out.Status != "pending" || out.ApprovalURL == "" || out.RequestID == "" {
		t.Fatalf("out = %+v", out)
	}
	kind, _, _ := r.ap.Request(out.RequestID)
	if kind != api.KindCalendarInvite {
		t.Fatalf("kind = %s", kind)
	}
	p := r.payload(out.RequestID)
	if p.Operation != api.CalOpCreate || p.OrganizerRole != api.CalRoleOrganizer || p.Original != nil || p.Proposed == nil || p.Proposed.Title != "Kickoff" ||
		p.Owner != "pat" || p.AccountID != r.acct.ID || p.FromAddress != "me@example.com" || p.Target.CalendarID != fake.PrimaryID {
		t.Fatalf("payload = %+v", p)
	}
	if len(p.Recipients) != 2 || p.Recipients[0].Email != "carol@example.com" && p.Recipients[0].Email != "bob@other.example" {
		t.Fatalf("recipients = %+v", p.Recipients)
	}
	for _, rc := range p.Recipients {
		if rc.Change != api.CalChangeAdded || rc.Email == "me@example.com" || (rc.Email == "bob@other.example") != rc.External {
			t.Errorf("recipient %+v", rc)
		}
	}
	pv := r.ap.Preview(out.RequestID)
	if pv.AccountID != r.acct.ID || pv.FromAddress != "me@example.com" || pv.InviteEnabled == nil || !*pv.InviteEnabled || pv.CalendarGranted == nil || !*pv.CalendarGranted {
		t.Errorf("preview = %+v", pv)
	}
	if r.events() != 0 || r.prov.count("create") != 0 {
		t.Fatal("requesting wrote to the calendar")
	}
	// Requesting the same change again returns the same request.
	if again := r.create("Kickoff", "me@example.com", "Bob@Other.example", "carol@example.com"); again.RequestID != out.RequestID {
		t.Errorf("second request %s, want %s", again.RequestID, out.RequestID)
	}
	if r.ap.Creates != 1 {
		t.Errorf("approver creates = %d, want 1", r.ap.Creates)
	}
}

func TestUpdatePayloadHasOriginalProposedRoleTargetVersionAndRemovedAttendees(t *testing.T) {
	r := newRig(t, true)
	ev := r.seed("Sync", "me@example.com", "keep@example.com", "gone@other.example")
	title := "Sync v2"
	out := r.mustUpdate(ev, provider.EventPatch{Title: &title, Attendees: &[]provider.Attendee{{Email: "me@example.com"}, {Email: "keep@example.com"}, {Email: "new@other.example"}}})
	p := r.payload(out.RequestID)
	if p.Original == nil || p.Original.Title != "Sync" || p.Proposed == nil || p.Proposed.Title != "Sync v2" || p.OrganizerRole != api.CalRoleOrganizer ||
		p.Target.EventID != ev.ID || p.Target.Scope != api.CalScopeInstance || p.Version != ev.Version || p.Version == "" {
		t.Fatalf("payload = %+v", p)
	}
	got := map[string]string{}
	for _, rc := range p.Recipients {
		got[rc.Email] = rc.Change
	}
	want := map[string]string{"keep@example.com": api.CalChangeKept, "new@other.example": api.CalChangeAdded, "gone@other.example": api.CalChangeRemoved}
	if len(got) != 3 || got["keep@example.com"] != want["keep@example.com"] || got["new@other.example"] != want["new@other.example"] || got["gone@other.example"] != want["gone@other.example"] {
		t.Fatalf("recipients = %v", got)
	}
	if r.prov.count("update") != 0 {
		t.Fatal("requesting wrote to the calendar")
	}
}

func TestAnOrganizerOnlyOperationOnAnAttendeeEventIsRejectedBeforeARequestExists(t *testing.T) {
	r := newRig(t, true)
	ev := r.fake.AddEvent(fake.PrimaryID, provider.Event{
		Title: "Theirs", Start: at(9), End: at(10), Organizer: provider.Attendee{Email: "boss@other.example"},
		Attendees: []provider.Attendee{{Email: "boss@other.example"}, {Email: "me@example.com", Self: true}, {Email: "x@other.example"}},
	})
	title := "Mine now"
	if _, err := r.update(ev, provider.EventPatch{Title: &title}); r.refusal(err).Code != inviteflow.CodeOrganizerOnly {
		t.Errorf("update by an attendee: %v", err)
	}
	if _, err := r.cancel(ev); r.refusal(err).Code != inviteflow.CodeOrganizerOnly {
		t.Errorf("cancel by an attendee: %v", err)
	}
	if _, err := r.update(ev, provider.EventPatch{Attendees: &[]provider.Attendee{{Email: "me@example.com"}}}); r.refusal(err).Code != inviteflow.CodeOrganizerOnly {
		t.Errorf("attendee change by an attendee: %v", err)
	}
	if r.ap.Creates != 0 {
		t.Fatalf("%d approver requests were created for operations only the organizer may do", r.ap.Creates)
	}
}

func TestRequestsAreRefusedForAnAccountThatIsNotReady(t *testing.T) {
	cases := map[string]struct {
		mut  func(*rig)
		code string
	}{
		"invites off": {func(r *rig) {
			must(r.t, r.st.UpdateAccount(ctx, "pat", r.acct.ID, store.AccountPatch{InviteEnabled: store.Ptr(false)}))
		}, inviteflow.CodeInvitesNotOn},
		"calendar partial": {func(r *rig) {
			g := map[string]store.ServiceGrant{store.ServiceCalendar: {Scopes: []string{calRead}, GrantedAt: r.clk.Now()}}
			must(r.t, r.st.UpdateAccount(ctx, "pat", r.acct.ID, store.AccountPatch{Services: &g}))
		}, inviteflow.CodeNotGranted},
		"needs reauth": {func(r *rig) {
			must(r.t, r.st.UpdateAccount(ctx, "pat", r.acct.ID, store.AccountPatch{Status: store.Ptr(store.StatusNeedsReauth)}))
		}, inviteflow.CodeAccountNotReady},
		"kill switch": {func(r *rig) { must(r.t, r.st.SetKillSwitch(ctx, true, "testing", r.clk.Now())) }, inviteflow.CodeKillSwitch},
		"provider kill switch": {func(r *rig) {
			must(r.t, r.st.SetProviderKillSwitch(ctx, store.ProviderGoogle, true, "testing", r.clk.Now()))
		}, inviteflow.CodeKillSwitch},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			r := newRig(t, true)
			c.mut(r)
			a, err := r.st.GetAccount(ctx, "pat", r.acct.ID)
			must(t, err)
			in := provider.EventInput{Title: "x", Start: at(9), End: at(10), Attendees: attendees("bob@other.example")}
			_, err = r.svc.RequestInvite(ctx, inviteflow.RequestIn{Account: a, Operation: api.CalOpCreate, CalendarID: fake.PrimaryID, Event: &in})
			if e := r.refusal(err); e.Code != c.code {
				t.Errorf("code = %s, want %s", e.Code, c.code)
			}
			if r.ap.Creates != 0 {
				t.Error("an approver request was created for a refused request")
			}
		})
	}
}

func TestAChangeThatInvolvesNobodyButTheAccountNeedsNoInviteApproval(t *testing.T) {
	r := newRig(t, true)
	ev := r.seed("Focus", "me@example.com")
	title := "Deep focus"
	if _, err := r.update(ev, provider.EventPatch{Title: &title}); r.refusal(err).Code != inviteflow.CodeNoInviteNeeded {
		t.Errorf("err = %v", err)
	}
	in := provider.EventInput{Title: "x", Start: at(9), End: at(10)}
	if _, err := r.svc.RequestInvite(ctx, inviteflow.RequestIn{Account: r.acct, Operation: api.CalOpCreate, CalendarID: fake.PrimaryID, Event: &in}); r.refusal(err).Code != inviteflow.CodeNoInviteNeeded {
		t.Errorf("create err = %v", err)
	}
}

func TestHourlyCapRefusesARequestAtTheLimit(t *testing.T) {
	r := newRig(t, true)
	for range r.acct.HourlyCap {
		must(t, r.st.ReserveCap(ctx, r.acct.ID, r.clk.Now(), r.acct.HourlyCap))
	}
	in := provider.EventInput{Title: "x", Start: at(9), End: at(10), Attendees: attendees("bob@other.example")}
	_, err := r.svc.RequestInvite(ctx, inviteflow.RequestIn{Account: r.acct, Operation: api.CalOpCreate, CalendarID: fake.PrimaryID, Event: &in})
	if e := r.refusal(err); e.Code != inviteflow.CodeRateLimited {
		t.Errorf("code = %s", e.Code)
	}
}

// ---- dispatch ----

func TestApprovedCreateApplies(t *testing.T) {
	r := newRig(t, true)
	out := r.create("Kickoff", "me@example.com", "bob@other.example", "carol@example.com")
	if st := r.status(out.RequestID); st.Status != inviteflow.StatusPending || st.ApprovalURL == "" {
		t.Fatalf("pending status = %+v", st)
	}
	id := r.approved(out)
	got := r.status(id)
	if got.Status != store.CalendarApplied || got.EventID == "" || got.Operation != api.CalOpCreate || got.CalendarID != fake.PrimaryID {
		t.Fatalf("status = %+v", got)
	}
	if r.events() != 1 || r.prov.count("create") != 1 {
		t.Fatalf("%d events, %d create calls", r.events(), r.prov.count("create"))
	}
	if inv := r.notes("invite"); len(inv) != 2 {
		t.Fatalf("invites = %v, want bob and carol once each", inv)
	}
	row, err := r.st.GetCalendarRequest(ctx, id)
	must(t, err)
	if row.State != store.CalendarApplied || row.DedupeKey != id || row.ProviderEventID != got.EventID {
		t.Fatalf("ledger row = %+v", row)
	}
	// The sealed snapshot is dropped once the request is done.
	if ap, _ := r.st.GetApproval(ctx, id); len(ap.Snapshot) != 0 || ap.Status != store.ApprovalConsumed {
		t.Errorf("approval after apply = status %s, snapshot %d bytes", ap.Status, len(ap.Snapshot))
	}
}

func TestReplayDoesNotApplyTwice(t *testing.T) {
	r := newRig(t, true)
	id := r.approved(r.create("Kickoff", "me@example.com", "bob@other.example"))
	first := r.status(id)
	consumes := r.ap.ConsumeCalls
	second := r.status(id)
	if first.Status != store.CalendarApplied || second.Status != store.CalendarApplied || second.EventID != first.EventID {
		t.Fatalf("first %+v second %+v", first, second)
	}
	if r.events() != 1 || r.ap.ConsumeCalls != consumes {
		t.Fatalf("%d events; consume calls %d -> %d (a terminal ledger row must be returned without calling the approver)", r.events(), consumes, r.ap.ConsumeCalls)
	}
}

func TestApprovedUpdateAppliesAndNotifiesAddedKeptAndRemoved(t *testing.T) {
	r := newRig(t, true)
	ev := r.seed("Sync", "me@example.com", "keep@example.com", "gone@other.example")
	title := "Sync v2"
	out := r.mustUpdate(ev, provider.EventPatch{Title: &title, Attendees: &[]provider.Attendee{{Email: "me@example.com"}, {Email: "keep@example.com"}, {Email: "new@other.example"}}})
	id := r.approved(out)
	got := r.status(id)
	if got.Status != store.CalendarApplied || got.EventID != ev.ID {
		t.Fatalf("status = %+v", got)
	}
	cur, err := r.fake.GetEvent(ctx, ref(ev))
	must(t, err)
	if cur.Title != "Sync v2" || len(cur.Attendees) != 3 {
		t.Fatalf("event = %+v", cur)
	}
	upd := map[string]bool{}
	for _, a := range r.notes("update") {
		upd[a] = true
	}
	for _, want := range []string{"keep@example.com", "new@other.example", "gone@other.example"} {
		if !upd[want] {
			t.Errorf("%s was not notified; notified %v", want, upd)
		}
	}
	if r.prov.count("update") != 1 {
		t.Errorf("%d update calls", r.prov.count("update"))
	}
}

func TestApprovedCancelAppliesAndNotifiesEveryAttendee(t *testing.T) {
	r := newRig(t, true)
	ev := r.seed("Offsite", "me@example.com", "bob@other.example", "carol@example.com")
	id := r.approved(r.mustCancel(ev))
	got := r.status(id)
	if got.Status != store.CalendarApplied {
		t.Fatalf("status = %+v", got)
	}
	if _, err := r.fake.GetEvent(ctx, ref(ev)); !errors.Is(err, provider.ErrNotFound) {
		t.Errorf("the event still exists: %v", err)
	}
	if c := r.notes("cancel"); len(c) != 2 {
		t.Errorf("cancellations = %v", c)
	}
}

func TestAnAttendeeAddedBetweenApprovalAndDispatchIsStaleAndWritesNothing(t *testing.T) {
	for name, op := range map[string]string{"update": api.CalOpUpdate, "cancel": api.CalOpCancel} {
		t.Run(name, func(t *testing.T) {
			r := newRig(t, true)
			ev := r.seed("Sync", "me@example.com", "bob@other.example")
			var out inviteflow.RequestOut
			if op == api.CalOpUpdate {
				title := "Sync v2"
				out = r.mustUpdate(ev, provider.EventPatch{Title: &title})
			} else {
				out = r.mustCancel(ev)
			}
			id := r.approved(out)
			// Someone else adds a person after the approval.
			r.fake.ExternalAddAttendee(ref(ev), provider.Attendee{Email: "eve@other.example"})
			got := r.status(id)
			if got.Status != inviteflow.StatusStale || got.State != store.CalendarFailed || !strings.Contains(got.Note, "changed after you approved") {
				t.Fatalf("status = %+v, want stale", got)
			}
			if r.prov.count("update") != 0 || r.prov.count("delete") != 0 || len(r.fake.Notifications()) != 0 {
				t.Fatalf("a stale approval wrote: updates %d deletes %d notes %v", r.prov.count("update"), r.prov.count("delete"), r.fake.Notifications())
			}
			cur, err := r.fake.GetEvent(ctx, ref(ev))
			must(t, err)
			if cur.Title != "Sync" || len(cur.Attendees) != 3 {
				t.Errorf("event was changed: %+v", cur)
			}
			if n, _ := r.st.HourlyCount(ctx, r.acct.ID, r.clk.Now()); n != 0 {
				t.Errorf("hourly count = %d, the slot of a stale approval must be released", n)
			}
			if again := r.status(id); again.Status != inviteflow.StatusStale {
				t.Errorf("status again = %+v", again)
			}
			// A new request against the new version is possible.
			if op == api.CalOpUpdate {
				title := "Sync v3"
				if fresh := r.mustUpdate(ev, provider.EventPatch{Title: &title}); fresh.RequestID == out.RequestID {
					t.Error("the stale request was reused")
				}
			}
		})
	}
}

func TestAConcurrentWriteAtTheMomentOfDispatchIsStale(t *testing.T) {
	// The event changes after dispatch re-read it and before the write: the
	// provider's If-Match rejects it and nothing is written.
	r := newRig(t, true)
	ev := r.seed("Sync", "me@example.com", "bob@other.example")
	title := "Sync v2"
	id := r.approved(r.mustUpdate(ev, provider.EventPatch{Title: &title}))
	r.prov.during = func(op string) {
		if op == "update" {
			r.fake.ExternalAddAttendee(ref(ev), provider.Attendee{Email: "eve@other.example"})
		}
	}
	got := r.status(id)
	if got.Status != inviteflow.StatusStale {
		t.Fatalf("status = %+v", got)
	}
	cur, _ := r.fake.GetEvent(ctx, ref(ev))
	if cur.Title != "Sync" {
		t.Errorf("the write landed on a changed event: %+v", cur)
	}
}

func TestLiveFalseStopsAtWouldApplyAndTouchesNothing(t *testing.T) {
	r := newRig(t, false)
	id := r.approved(r.create("Kickoff", "me@example.com", "bob@other.example"))
	got := r.status(id)
	if got.Status != store.CalendarWouldApply || !got.DryRun {
		t.Fatalf("status = %+v", got)
	}
	if r.events() != 0 || r.prov.count("create") != 0 || r.ap.ConsumeCalls == 0 {
		t.Fatalf("events %d, create calls %d, consume calls %d", r.events(), r.prov.count("create"), r.ap.ConsumeCalls)
	}
	if again := r.status(id); again.Status != store.CalendarWouldApply {
		t.Errorf("replay = %+v", again)
	}
}

func TestKillSwitchFlippedAfterApprovalBlocksDispatch(t *testing.T) {
	r := newRig(t, true)
	id := r.approved(r.create("Kickoff", "me@example.com", "bob@other.example"))
	must(t, r.st.SetKillSwitch(ctx, true, "testing", r.clk.Now()))
	out := r.status(id)
	if out.Status != inviteflow.StatusBlocked || !strings.Contains(out.Note, "kill switch") {
		t.Fatalf("status = %+v", out)
	}
	if _, err := r.st.GetCalendarRequest(ctx, id); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("a killed dispatch must not claim: %v", err)
	}
	if r.ap.ConsumeCalls != 0 || r.events() != 0 {
		t.Errorf("consume calls %d, events %d", r.ap.ConsumeCalls, r.events())
	}
	must(t, r.st.SetKillSwitch(ctx, false, "", r.clk.Now()))
	must(t, r.st.SetProviderKillSwitch(ctx, store.ProviderGoogle, true, "provider off", r.clk.Now()))
	if out := r.status(id); out.Status != inviteflow.StatusBlocked || !strings.Contains(out.Note, "google kill switch") {
		t.Fatalf("with the provider switch on: %+v", out)
	}
	must(t, r.st.SetProviderKillSwitch(ctx, store.ProviderGoogle, false, "", r.clk.Now()))
	if out := r.status(id); out.Status != store.CalendarApplied {
		t.Fatalf("after the switches are off: %+v", out)
	}
}

func TestInvitesTurnedOffAfterApprovalBlocksDispatch(t *testing.T) {
	r := newRig(t, true)
	id := r.approved(r.create("Kickoff", "me@example.com", "bob@other.example"))
	must(t, r.st.UpdateAccount(ctx, "pat", r.acct.ID, store.AccountPatch{InviteEnabled: store.Ptr(false)}))
	if out := r.status(id); out.Status != inviteflow.StatusBlocked {
		t.Fatalf("status = %+v", out)
	}
	if r.events() != 0 {
		t.Error("an event was created for an account with invites off")
	}
}

func TestDeniedAndExpiredRequestsCanBeRequestedAgain(t *testing.T) {
	r := newRig(t, true)
	a := r.create("Kickoff", "me@example.com", "bob@other.example")
	r.ap.Deny(a.RequestID)
	if out := r.status(a.RequestID); out.Status != inviteflow.StatusDenied {
		t.Fatalf("denied: %+v", out)
	}
	b := r.create("Kickoff", "me@example.com", "bob@other.example")
	if b.RequestID == a.RequestID || b.Status != "pending" {
		t.Fatalf("after a denial: %+v", b)
	}
	r.clk.Advance(20 * time.Minute)
	if out := r.status(b.RequestID); out.Status != inviteflow.StatusExpired {
		t.Fatalf("expired: %+v", out)
	}
	c := r.create("Kickoff", "me@example.com", "bob@other.example")
	if c.RequestID == b.RequestID || c.RequestID == a.RequestID {
		t.Fatalf("after an expiry: %+v", c)
	}
	if r.events() != 0 {
		t.Error("a denied or expired request created an event")
	}
}

func TestReceiptSubstitutionIsRejected(t *testing.T) {
	r := newRig(t, true)
	a := r.create("One", "me@example.com", "bob@other.example")
	b := r.create("Two", "me@example.com", "carol@other.example")
	r.ap.Approve(a.RequestID)
	r.ap.Approve(b.RequestID)
	_, bSHA, _ := r.ap.Request(b.RequestID)
	now := r.clk.Now()
	for name, rc := range map[string]func() receipt.Receipt{
		"another request_id": func() receipt.Receipt { return r.ap.Receipt(a.RequestID) },
		"another payload": func() receipt.Receipt {
			return r.ap.SignFor(b.RequestID, api.KindCalendarInvite, strings.Repeat("0", 64), now, now.Add(15*time.Minute))
		},
		"another kind": func() receipt.Receipt {
			return r.ap.SignFor(b.RequestID, api.KindAccountInviteEnable, bSHA, now, now.Add(15*time.Minute))
		},
		"the email-send kind": func() receipt.Receipt {
			return r.ap.SignFor(b.RequestID, api.KindEmailSend, bSHA, now, now.Add(15*time.Minute))
		},
		"expired": func() receipt.Receipt {
			return r.ap.SignFor(b.RequestID, api.KindCalendarInvite, bSHA, now.Add(-40*time.Minute), now.Add(-25*time.Minute))
		},
	} {
		r.ap.Tamper(b.RequestID, rc())
		if out := r.status(b.RequestID); out.Status != inviteflow.StatusReceiptInvalid {
			t.Errorf("%s: status = %+v, want receipt_invalid", name, out)
		}
		if _, err := r.st.GetCalendarRequest(ctx, b.RequestID); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("%s: a bad receipt must not claim: %v", name, err)
		}
	}
	if r.events() != 0 || r.ap.ConsumeCalls != 0 {
		t.Fatalf("events %d, consumed %d", r.events(), r.ap.ConsumeCalls)
	}
	r.ap.Tamper(b.RequestID, r.ap.SignFor(b.RequestID, api.KindCalendarInvite, bSHA, now, now.Add(15*time.Minute)))
	if out := r.status(b.RequestID); out.Status != store.CalendarApplied {
		t.Fatalf("genuine receipt: %+v", out)
	}
}

func TestAMissingSnapshotIsNotApplied(t *testing.T) {
	r := newRig(t, true)
	out := r.create("One", "me@example.com", "bob@other.example")
	// The sealed snapshot is gone (deleted, or never written).
	if _, err := r.st.TransitionApproval(ctx, out.RequestID, []string{store.ApprovalPending}, store.ApprovalPending, true); err != nil {
		t.Fatal(err)
	}
	if ap, _ := r.st.GetApproval(ctx, out.RequestID); len(ap.Snapshot) != 0 {
		t.Fatal("test setup: the snapshot is still there")
	}
	r.ap.Approve(out.RequestID)
	got := r.status(out.RequestID)
	if got.State != store.CalendarFailed || r.events() != 0 {
		t.Fatalf("a request with no snapshot: %+v, %d events", got, r.events())
	}
	if n(r, t) != 0 {
		t.Errorf("hourly count = %d after a failed claim, want the slot released", n(r, t))
	}
}

// ---- idempotency, concurrency, the shared cap ----

func TestTenConcurrentStatusCallsApplyExactlyOnce(t *testing.T) {
	r := newRig(t, true)
	id := r.approved(r.create("Kickoff", "me@example.com", "bob@other.example"))
	var wg sync.WaitGroup
	errs := make(chan string, 10)
	for range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := r.svc.Status(ctx, "pat", id); err != nil {
				errs <- err.Error()
			}
		}()
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Errorf("a concurrent call errored: %s", e)
	}
	if r.events() != 1 || r.prov.count("create") != 1 {
		t.Fatalf("%d events, %d create calls, want exactly 1", r.events(), r.prov.count("create"))
	}
	if out := r.status(id); out.Status != store.CalendarApplied {
		t.Fatalf("final status = %+v", out)
	}
	if n(r, t) != 1 {
		t.Errorf("hourly count = %d, want 1", n(r, t))
	}
	if len(r.notes("invite")) != 1 {
		t.Errorf("invites = %v", r.notes("invite"))
	}
}

func TestConcurrentInvitesAtCapMinusOneHaveExactlyOneClaimant(t *testing.T) {
	r := newRig(t, true)
	r.clk.t = time.Date(2026, 11, 25, 12, 50, 0, 0, time.UTC) // the hour turns inside the approvals' 15 minutes
	for range r.acct.HourlyCap - 1 {
		must(t, r.st.ReserveCap(ctx, r.acct.ID, r.clk.Now(), r.acct.HourlyCap))
	}
	ids := []string{r.approved(r.create("One", "me@example.com", "a@other.example")), r.approved(r.create("Two", "me@example.com", "b@other.example"))}
	var wg sync.WaitGroup
	var rateLimited atomic.Int32
	for _, id := range ids {
		for range 4 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				out, err := r.svc.Status(ctx, "pat", id)
				if err != nil {
					t.Errorf("status: %v", err)
					return
				}
				if out.Status == inviteflow.StatusRateLimited {
					rateLimited.Add(1)
				}
			}()
		}
	}
	wg.Wait()
	if r.events() != 1 {
		t.Fatalf("%d events, want exactly one claim to win the last slot", r.events())
	}
	if rateLimited.Load() == 0 {
		t.Error("the losing request must report rate_limited")
	}
	r.clk.Advance(11 * time.Minute)
	for _, id := range ids {
		r.status(id)
	}
	if r.events() != 2 {
		t.Errorf("after the hour turned: %d events, want 2", r.events())
	}
}

func TestTheHourlyCapIsSharedWithSends(t *testing.T) {
	r := newRig(t, true)
	for i := range r.acct.HourlyCap - 2 {
		must(t, r.st.ClaimSend(ctx, store.SendRow{RequestID: "send-" + string(rune('a'+i)), Owner: "pat", AccountID: r.acct.ID, Created: r.clk.Now()}, r.acct.HourlyCap))
	}
	id := r.approved(r.create("Kickoff", "me@example.com", "bob@other.example"))
	other := r.approved(r.create("Second", "me@example.com", "carol@other.example"))
	if out := r.status(id); out.Status != store.CalendarApplied {
		t.Fatalf("the invite slot: %+v", out)
	}
	// A send takes the last slot: the second invite meets the shared cap.
	must(t, r.st.ClaimSend(ctx, store.SendRow{RequestID: "send-last", Owner: "pat", AccountID: r.acct.ID, Created: r.clk.Now()}, r.acct.HourlyCap))
	if out := r.status(other); out.Status != inviteflow.StatusRateLimited {
		t.Fatalf("at the shared cap: %+v", out)
	}
	if r.events() != 1 {
		t.Errorf("%d events", r.events())
	}
	// The invite's slot counts against sends too.
	if err := r.st.ClaimSend(ctx, store.SendRow{RequestID: "send-over", Owner: "pat", AccountID: r.acct.ID, Created: r.clk.Now()}, r.acct.HourlyCap); !errors.Is(err, store.ErrRateLimited) {
		t.Errorf("a send at the shared cap: %v", err)
	}
}

func TestConsumeUsesTheRequestsOwnClaimantAndAnotherClaimantFailsTheRequest(t *testing.T) {
	r := newRig(t, true)
	id := r.approved(r.create("Kickoff", "me@example.com", "bob@other.example"))
	r.status(id)
	if r.ap.Claimant(id) != id {
		t.Errorf("claimant = %q, want the request id", r.ap.Claimant(id))
	}
	// Another claimant consumed the approval first: moose never claimed it.
	r2 := newRig(t, true)
	id2 := r2.approved(r2.create("Kickoff", "me@example.com", "bob@other.example"))
	if _, err := r2.ap.Consume(ctx, id2, "someone-else"); err != nil {
		t.Fatal(err)
	}
	if out := r2.status(id2); out.Status != inviteflow.StatusAlreadyClaimed || r2.events() != 0 {
		t.Fatalf("an approval consumed elsewhere: %+v, %d events", out, r2.events())
	}
	// Another claimant consumed it after moose claimed the row: the request fails.
	r3 := newRig(t, true)
	id3 := r3.approved(r3.create("Kickoff", "me@example.com", "bob@other.example"))
	must(t, r3.st.ClaimCalendarRequest(ctx, store.CalendarRow{RequestID: id3, Owner: "pat", AccountID: r3.acct.ID, Operation: api.CalOpCreate, DedupeKey: id3, Created: r3.clk.Now()}, r3.acct.HourlyCap))
	if _, err := r3.ap.Consume(ctx, id3, "someone-else"); err != nil {
		t.Fatal(err)
	}
	if out := r3.status(id3); out.State != store.CalendarFailed || r3.events() != 0 || n(r3, t) != 0 {
		t.Fatalf("an approval consumed by another claimant after the claim: %+v, %d events, cap %d", out, r3.events(), n(r3, t))
	}
}

func TestApproverDownReturnsPendingAndClaimsNothing(t *testing.T) {
	r := newRig(t, true)
	out := r.create("Kickoff", "me@example.com", "bob@other.example")
	r.ap.Approve(out.RequestID)
	r.ap.Down = true
	if got := r.status(out.RequestID); got.Status != inviteflow.StatusPending {
		t.Fatalf("status = %+v", got)
	}
	if _, err := r.st.GetCalendarRequest(ctx, out.RequestID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("nothing may be claimed while the approver is down: %v", err)
	}
	r.ap.Down = false
	if got := r.status(out.RequestID); got.Status != store.CalendarApplied {
		t.Fatalf("after it is back: %+v", got)
	}
}

func TestStatusOfAnotherOwnersRequestIsNotFound(t *testing.T) {
	r := newRig(t, true)
	out := r.create("Kickoff", "me@example.com", "bob@other.example")
	if _, err := r.svc.Status(ctx, "intruder", out.RequestID); r.refusal(err).Code != inviteflow.CodeNotFound {
		t.Errorf("another owner: %v", err)
	}
	if _, err := r.svc.Status(ctx, "pat", "nope"); r.refusal(err).Code != inviteflow.CodeNotFound {
		t.Errorf("unknown id: %v", err)
	}
	en, err := r.svc.RequestEnable(ctx, r.disabledAccount(), r.acct.Address)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.svc.Status(ctx, "pat", en.RequestID); r.refusal(err).Code != inviteflow.CodeNotFound {
		t.Errorf("an enable request is not an invite request: %v", err)
	}
}

// ---- crash recovery and unknown outcomes ----

func TestRecordedLedgerFieldsExistBeforeTheProviderIsCalled(t *testing.T) {
	r := newRig(t, true)
	ev := r.seed("Sync", "me@example.com", "bob@other.example")
	title := "Sync v2"
	upd := r.approved(r.mustUpdate(ev, provider.EventPatch{Title: &title}))
	crt := r.approved(r.create("Kickoff", "me@example.com", "carol@other.example"))
	seen := map[string]store.CalendarRow{}
	r.prov.during = func(op string) {
		id := map[string]string{"update": upd, "create": crt}[op]
		row, err := r.st.GetCalendarRequest(ctx, id)
		if err != nil {
			t.Errorf("%s: no ledger row during the call: %v", op, err)
		}
		seen[op] = row
	}
	r.status(upd)
	r.status(crt)
	if row := seen["update"]; row.State != store.CalendarDispatching || row.ProviderEventID != ev.ID || row.Version != ev.Version || row.Operation != api.CalOpUpdate {
		t.Errorf("update row during the call = %+v", row)
	}
	if row := seen["create"]; row.State != store.CalendarDispatching || row.DedupeKey != crt || row.Operation != api.CalOpCreate {
		t.Errorf("create row during the call = %+v", row)
	}
}

func TestCreateCrashAfterProviderSuccessIsFoundByItsDedupeKey(t *testing.T) {
	r := newRig(t, true)
	r.prov.lostAfter["create"] = 1
	id := r.approved(r.create("Kickoff", "me@example.com", "bob@other.example"))
	first := r.status(id)
	if first.Status != store.CalendarUnknown {
		t.Fatalf("a lost response: %+v", first)
	}
	if r.events() != 1 {
		t.Fatalf("setup: the provider did create the event (%d events)", r.events())
	}
	second := r.status(id)
	if second.Status != store.CalendarApplied || second.EventID == "" || !strings.Contains(second.Reconciliation, "dedupe key") {
		t.Fatalf("after reconciliation: %+v", second)
	}
	if r.events() != 1 || r.prov.count("create") != 1 {
		t.Fatalf("%d events, %d create calls: reconciliation must never create", r.events(), r.prov.count("create"))
	}
	if len(r.notes("invite")) != 1 {
		t.Errorf("invites = %v", r.notes("invite"))
	}
	if again := r.status(id); again.Status != store.CalendarApplied {
		t.Errorf("replay = %+v", again)
	}
}

func TestCreateThatNeverReachedTheProviderFailsAfterTheGraceAndFreesTheSlot(t *testing.T) {
	r := newRig(t, true)
	r.prov.lostBefore["create"] = 1
	id := r.approved(r.create("Kickoff", "me@example.com", "bob@other.example"))
	if first := r.status(id); first.Status != store.CalendarUnknown {
		t.Fatalf("first = %+v", first)
	}
	// Right away, "not found" could be a call the provider is still completing.
	if soon := r.status(id); soon.Status != store.CalendarUnknown || !strings.Contains(soon.Reconciliation, "No event carries") {
		t.Fatalf("soon after = %+v", soon)
	}
	r.clk.Advance(inviteflow.DefaultReconcileGap + time.Second)
	got := r.status(id)
	if got.State != store.CalendarFailed || !strings.Contains(got.Reconciliation, "did not land") {
		t.Fatalf("after the grace = %+v", got)
	}
	if n(r, t) != 0 {
		t.Errorf("hourly count = %d, the slot of a failed create must be released", n(r, t))
	}
	if r.events() != 0 || r.prov.count("create") != 1 {
		t.Errorf("%d events, %d create calls", r.events(), r.prov.count("create"))
	}
}

func TestCreateOnAProviderWithoutALookupStaysUnknown(t *testing.T) {
	r := newRig(t, true)
	r.noFinder()
	r.prov.lostAfter["create"] = 1
	id := r.approved(r.create("Kickoff", "me@example.com", "bob@other.example"))
	r.status(id)
	r.clk.Advance(time.Hour)
	for range 2 {
		if got := r.status(id); got.Status != store.CalendarUnknown || !strings.Contains(got.Reconciliation, "cannot look an event up") {
			t.Fatalf("status = %+v", got)
		}
	}
	if r.prov.count("create") != 1 {
		t.Errorf("%d create calls", r.prov.count("create"))
	}
}

func TestUpdateCrashAfterProviderSuccessIsRecognisedByVersionAndContent(t *testing.T) {
	r := newRig(t, true)
	ev := r.seed("Sync", "me@example.com", "bob@other.example")
	title := "Sync v2"
	r.prov.lostAfter["update"] = 1
	id := r.approved(r.mustUpdate(ev, provider.EventPatch{Title: &title}))
	if first := r.status(id); first.Status != store.CalendarUnknown {
		t.Fatalf("first = %+v", first)
	}
	got := r.status(id)
	if got.Status != store.CalendarApplied || !strings.Contains(got.Reconciliation, "landed") {
		t.Fatalf("after reconciliation = %+v", got)
	}
	if r.prov.count("update") != 1 {
		t.Fatalf("%d update calls: an update must never be replayed", r.prov.count("update"))
	}
}

func TestUpdateThatMayNotHaveLandedStaysUnknownAndIsNeverReplayed(t *testing.T) {
	r := newRig(t, true)
	ev := r.seed("Sync", "me@example.com", "bob@other.example")
	title := "Sync v2"
	r.prov.lostBefore["update"] = 1
	id := r.approved(r.mustUpdate(ev, provider.EventPatch{Title: &title}))
	r.status(id)
	r.clk.Advance(48 * time.Hour)
	for range 3 {
		got := r.status(id)
		if got.Status != store.CalendarUnknown || !strings.Contains(got.Reconciliation, "will not be replayed") {
			t.Fatalf("inconclusive = %+v", got)
		}
	}
	if r.prov.count("update") != 1 {
		t.Fatalf("%d update calls, want the one that was lost", r.prov.count("update"))
	}
	// Someone else edits the event meanwhile: still inconclusive.
	r.fake.ExternalAddAttendee(ref(ev), provider.Attendee{Email: "eve@other.example"})
	if got := r.status(id); got.Status != store.CalendarUnknown || !strings.Contains(got.Reconciliation, "does not carry") {
		t.Fatalf("after another edit = %+v", got)
	}
}

func TestCancelCrashAfterProviderSuccessIsRecognisedByTheEventBeingGone(t *testing.T) {
	r := newRig(t, true)
	ev := r.seed("Offsite", "me@example.com", "bob@other.example")
	r.prov.lostAfter["delete"] = 1
	id := r.approved(r.mustCancel(ev))
	if first := r.status(id); first.Status != store.CalendarUnknown {
		t.Fatalf("first = %+v", first)
	}
	got := r.status(id)
	if got.Status != store.CalendarApplied || !strings.Contains(got.Reconciliation, "cancel landed") {
		t.Fatalf("after reconciliation = %+v", got)
	}
	if r.prov.count("delete") != 1 {
		t.Fatalf("%d delete calls: a cancel must never be replayed", r.prov.count("delete"))
	}
}

func TestCancelThatMayNotHaveLandedStaysUnknown(t *testing.T) {
	r := newRig(t, true)
	ev := r.seed("Offsite", "me@example.com", "bob@other.example")
	r.prov.lostBefore["delete"] = 1
	id := r.approved(r.mustCancel(ev))
	r.status(id)
	for range 2 {
		if got := r.status(id); got.Status != store.CalendarUnknown || !strings.Contains(got.Reconciliation, "still exists") {
			t.Fatalf("status = %+v", got)
		}
	}
	if r.prov.count("delete") != 1 {
		t.Errorf("%d delete calls", r.prov.count("delete"))
	}
}

func TestACrashInDispatchingBecomesUnknownThenIsSettledNeverReplayed(t *testing.T) {
	r := newRig(t, true)
	id := r.approved(r.create("Kickoff", "me@example.com", "bob@other.example"))
	// The earlier process claimed the row, started the call and died; the
	// provider did apply it, under the request's dedupe key.
	must(t, r.st.ClaimCalendarRequest(ctx, store.CalendarRow{RequestID: id, Owner: "pat", AccountID: r.acct.ID, Operation: api.CalOpCreate, DedupeKey: id, Created: r.clk.Now()}, r.acct.HourlyCap))
	_, err := r.st.AdvanceCalendarRequest(ctx, id, store.CalendarClaimed, store.CalendarDispatching, "", r.clk.Now())
	must(t, err)
	if _, err := r.fake.CreateEvent(ctx, fake.PrimaryID, provider.EventInput{Title: "Kickoff", Description: "d", Start: at(9), End: at(10), Attendees: attendees("bob@other.example")}, provider.CreateOptions{DedupeKey: id}); err != nil {
		t.Fatal(err)
	}
	// A call that is still in flight is left alone.
	if out := r.status(id); out.Status != inviteflow.StatusDispatching {
		t.Fatalf("fresh dispatching row: %+v", out)
	}
	r.clk.Advance(inviteflow.DefaultStaleAfter + time.Second)
	out := r.status(id)
	if out.Status != store.CalendarApplied || out.EventID == "" {
		t.Fatalf("stale dispatching row: %+v", out)
	}
	if r.events() != 1 || r.prov.count("create") != 0 {
		t.Fatalf("%d events, %d create calls: a crashed dispatch must never be replayed", r.events(), r.prov.count("create"))
	}
}

func TestCrashAfterClaimedResumesAtConsume(t *testing.T) {
	r := newRig(t, true)
	id := r.approved(r.create("Kickoff", "me@example.com", "bob@other.example"))
	must(t, r.st.ClaimCalendarRequest(ctx, store.CalendarRow{RequestID: id, Owner: "pat", AccountID: r.acct.ID, Operation: api.CalOpCreate, DedupeKey: id, Created: r.clk.Now()}, r.acct.HourlyCap))
	out := r.status(id)
	if out.Status != store.CalendarApplied || r.events() != 1 {
		t.Fatalf("status %+v, %d events", out, r.events())
	}
	if r.ap.ConsumeCalls == 0 || r.ap.Claimant(id) != id {
		t.Errorf("the resumed call must consume as the request's own claimant (%q)", r.ap.Claimant(id))
	}
	if n(r, t) != 1 {
		t.Errorf("the resumed claim must not take a second cap slot: %d", n(r, t))
	}
}

type failingAdvance struct {
	store.Store
	to string
}

func (f *failingAdvance) AdvanceCalendarRequest(c context.Context, id, from, to, ev string, now time.Time) (store.CalendarRow, error) {
	if to == f.to {
		return store.CalendarRow{}, errors.New("disk on fire")
	}
	return f.Store.AdvanceCalendarRequest(c, id, from, to, ev, now)
}

func TestProviderAcceptedButNotRecordedIsSettledByReconciliation(t *testing.T) {
	for name, op := range map[string]string{"create": api.CalOpCreate, "update": api.CalOpUpdate, "cancel": api.CalOpCancel} {
		t.Run(name, func(t *testing.T) {
			r := newRig(t, true)
			var id string
			ev := r.seed("Sync", "me@example.com", "bob@other.example")
			switch op {
			case api.CalOpCreate:
				id = r.approved(r.create("Kickoff", "me@example.com", "bob@other.example"))
			case api.CalOpUpdate:
				title := "Sync v2"
				id = r.approved(r.mustUpdate(ev, provider.EventPatch{Title: &title}))
			default:
				id = r.approved(r.mustCancel(ev))
			}
			// The ledger write that records `applied` fails, as a crash right after
			// the provider accepted the change would.
			r.svc.Store = &failingAdvance{Store: r.st, to: store.CalendarApplied}
			out, err := r.svc.Status(ctx, "pat", id)
			if err != nil || out.Status != inviteflow.StatusDispatching {
				t.Fatalf("status = %+v, %v", out, err)
			}
			r.svc.Store = r.st // the process restarts with a healthy store
			r.clk.Advance(inviteflow.DefaultStaleAfter + time.Second)
			got := r.status(id)
			if got.Status != store.CalendarApplied || got.Reconciliation == "" {
				t.Fatalf("after the crash window: %+v", got)
			}
			calls := map[string]string{api.CalOpCreate: "create", api.CalOpUpdate: "update", api.CalOpCancel: "delete"}[op]
			if c := r.prov.count(calls); c != 1 {
				t.Fatalf("%d provider calls, the change must never be written twice", c)
			}
		})
	}
}

func TestAChangeTheProviderRefusesIsFailedAndFreesTheSlot(t *testing.T) {
	r := newRig(t, true)
	id := r.approved(r.create("Kickoff", "me@example.com", "bob@other.example"))
	r.fake.Err = provider.ErrAuth
	got := r.status(id)
	if got.State != store.CalendarFailed || n(r, t) != 0 {
		t.Fatalf("status = %+v", got)
	}
	r.fake.Err = nil
	if again := r.status(id); again.State != store.CalendarFailed || r.events() != 0 {
		t.Fatalf("a failed request must stay failed: %+v", again)
	}
}

func n(r *rig, t *testing.T) int {
	t.Helper()
	c, _ := r.st.HourlyCount(ctx, r.acct.ID, r.clk.Now())
	return c
}

func (r *rig) disabledAccount() store.Account {
	r.t.Helper()
	must(r.t, r.st.UpdateAccount(ctx, "pat", r.acct.ID, store.AccountPatch{InviteEnabled: store.Ptr(false)}))
	a, err := r.st.GetAccount(ctx, "pat", r.acct.ID)
	must(r.t, err)
	r.acct = a
	return a
}
