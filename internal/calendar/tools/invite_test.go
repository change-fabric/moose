package tools_test

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/change-fabric/moose/internal/accounts/envelope"
	"github.com/change-fabric/moose/internal/accounts/store"
	"github.com/change-fabric/moose/internal/approver/api"
	"github.com/change-fabric/moose/internal/calendar/fake"
	"github.com/change-fabric/moose/internal/calendar/inviteflow"
	"github.com/change-fabric/moose/internal/calendar/provider"
	"github.com/change-fabric/moose/internal/calendar/tools"
	"github.com/change-fabric/moose/internal/email/sendflow/flowtest"
)

// inviteRig is the tools wired to a real invite engine over the fake approver.
type inviteRig struct {
	d   tools.Deps
	src *source
	st  store.Store
	ap  *flowtest.Approver
}

func newInviteRig(t *testing.T) *inviteRig {
	t.Helper()
	d, src, st := deps(t)
	seal, err := envelope.NewLocal(bytes.Repeat([]byte("k"), 32))
	if err != nil {
		t.Fatal(err)
	}
	ap := flowtest.New()
	ap.Now = func() time.Time { return now }
	d.Invites = &inviteflow.Service{
		Store: st, Seal: seal, Approver: ap, Providers: src, ReceiptKey: ap.PublicKeyPEM(), RPID: flowtest.RPID, Live: true,
		Now: func() time.Time { return now }, Sleep: func(context.Context, time.Duration) error { return nil },
	}
	return &inviteRig{d: d, src: src, st: st, ap: ap}
}

func (r *inviteRig) seedWithAttendees(title string, emails ...string) provider.Event {
	var att []provider.Attendee
	for _, e := range emails {
		att = append(att, provider.Attendee{Email: e})
	}
	ev := timed(title, 9)
	ev.Attendees = att
	return r.src.byID["acc_full"].AddEvent(fake.PrimaryID, ev)
}

func attendeesArg(emails ...string) []any {
	var out []any
	for _, e := range emails {
		out = append(out, map[string]any{"email": e})
	}
	return out
}

func notified(c []tools.Recipient) map[string]string {
	m := map[string]string{}
	for _, r := range c {
		m[r.Email] = r.Change
	}
	return m
}

func TestCreateWithAttendeesNeedsApprovalWritesNothingAndApplyingItInvitesThem(t *testing.T) {
	r := newInviteRig(t)
	out, errText := run[tools.WriteOut](t, r.d, tools.ToolCreateEvent, create("attendees", attendeesArg("full@example.com", "bob@other.example", "carol@example.com")))
	if errText != "" {
		t.Fatal(errText)
	}
	if out.Status != tools.StatusNeedsApproval || out.Event != nil || out.Approval == nil || out.Approval.RequestID == "" || out.Approval.ApprovalURL == "" || out.Approval.Operation != api.CalOpCreate {
		t.Fatalf("out = %+v", out)
	}
	got := notified(out.Approval.Notifies)
	if len(got) != 2 || got["bob@other.example"] != api.CalChangeAdded || got["carol@example.com"] != api.CalChangeAdded {
		t.Fatalf("notifies = %+v", out.Approval.Notifies)
	}
	for _, n := range out.Approval.Notifies {
		if (n.Email == "bob@other.example") != n.External {
			t.Errorf("external flag of %+v", n)
		}
	}
	if n := countEvents(t, r.src.byID["acc_full"]); n != 0 {
		t.Fatalf("%d events were written before the approval", n)
	}
	if got := r.src.byID["acc_full"].Notifications(); len(got) != 0 {
		t.Fatalf("notified before the approval: %+v", got)
	}
	// Asking again returns the same request.
	again, _ := run[tools.WriteOut](t, r.d, tools.ToolCreateEvent, create("attendees", attendeesArg("full@example.com", "bob@other.example", "carol@example.com")))
	if again.Approval == nil || again.Approval.RequestID != out.Approval.RequestID {
		t.Errorf("second call = %+v", again.Approval)
	}
	// Pending until the owner approves.
	st, errText := run[tools.InviteStatusOut](t, r.d, tools.ToolInviteStatus, map[string]any{"request_id": out.Approval.RequestID})
	if errText != "" || st.Status != "pending" || st.ApprovalURL == "" {
		t.Fatalf("pending = %+v %q", st, errText)
	}
	r.ap.Approve(out.Approval.RequestID)
	st, errText = run[tools.InviteStatusOut](t, r.d, tools.ToolInviteStatus, map[string]any{"request_id": out.Approval.RequestID})
	if errText != "" || st.Status != store.CalendarApplied || st.Operation != api.CalOpCreate || st.Account.ID != "acc_full" ||
		!strings.HasPrefix(st.EventID, "acc_full:") || !strings.HasPrefix(st.CalendarID, "acc_full:") {
		t.Fatalf("applied = %+v %q", st, errText)
	}
	if n := countEvents(t, r.src.byID["acc_full"]); n != 1 {
		t.Fatalf("%d events, want 1", n)
	}
	if inv := r.src.byID["acc_full"].Notifications(); len(inv) != 1 || len(inv[0].To) != 2 {
		t.Fatalf("invitations = %+v", inv)
	}
	// The event the status names can be read back.
	ev, errText := run[tools.EventOut](t, r.d, tools.ToolGetEvent, map[string]any{"id": st.EventID})
	if errText != "" || ev.ID != st.EventID {
		t.Fatalf("get = %+v %q", ev, errText)
	}
}

func TestUpdateAndDeleteWithAttendeesNeedApprovalAndShowRemovedPeople(t *testing.T) {
	r := newInviteRig(t)
	ev := r.seedWithAttendees("Sync", "full@example.com", "keep@example.com", "gone@other.example")
	id := evID("acc_full", ev)
	upd, errText := run[tools.WriteOut](t, r.d, tools.ToolUpdateEvent, map[string]any{
		"id": id, "title": "Sync v2", "attendees": attendeesArg("full@example.com", "keep@example.com", "new@other.example"),
	})
	if errText != "" || upd.Status != tools.StatusNeedsApproval || upd.Approval == nil || upd.Approval.Operation != api.CalOpUpdate {
		t.Fatalf("update = %+v %q", upd, errText)
	}
	got := notified(upd.Approval.Notifies)
	if got["gone@other.example"] != api.CalChangeRemoved || got["new@other.example"] != api.CalChangeAdded || got["keep@example.com"] != api.CalChangeKept {
		t.Fatalf("notifies = %+v", upd.Approval.Notifies)
	}
	cur, _ := r.src.byID["acc_full"].GetEvent(context.Background(), provider.EventRef{CalendarID: fake.PrimaryID, EventID: ev.ID})
	if cur.Title != "Sync" || len(cur.Attendees) != 3 {
		t.Fatalf("the event changed before approval: %+v", cur)
	}
	del, errText := run[tools.WriteOut](t, r.d, tools.ToolDeleteEvent, map[string]any{"id": id})
	if errText != "" || del.Status != tools.StatusNeedsApproval || del.Approval == nil || del.Approval.Operation != api.CalOpCancel || len(del.Approval.Notifies) != 2 {
		t.Fatalf("delete = %+v %q", del, errText)
	}
	for _, n := range del.Approval.Notifies {
		if n.Change != api.CalChangeCancelled {
			t.Errorf("notify %+v", n)
		}
	}
	if len(r.src.byID["acc_full"].Notifications()) != 0 {
		t.Error("someone was notified before any approval")
	}
	// Approve and apply the update, then the cancel (which now sees a changed event: stale).
	r.ap.Approve(upd.Approval.RequestID)
	st, _ := run[tools.InviteStatusOut](t, r.d, tools.ToolInviteStatus, map[string]any{"request_id": upd.Approval.RequestID})
	if st.Status != store.CalendarApplied {
		t.Fatalf("update status = %+v", st)
	}
	r.ap.Approve(del.Approval.RequestID)
	st, _ = run[tools.InviteStatusOut](t, r.d, tools.ToolInviteStatus, map[string]any{"request_id": del.Approval.RequestID})
	if st.Status != "stale" {
		t.Fatalf("a cancel approved against the old version must be stale: %+v", st)
	}
}

func TestAnAttendeeChangeBetweenApprovalAndDispatchIsReportedStale(t *testing.T) {
	r := newInviteRig(t)
	ev := r.seedWithAttendees("Sync", "full@example.com", "bob@other.example")
	upd, _ := run[tools.WriteOut](t, r.d, tools.ToolUpdateEvent, map[string]any{"id": evID("acc_full", ev), "title": "Sync v2"})
	if upd.Approval == nil {
		t.Fatalf("update = %+v", upd)
	}
	r.ap.Approve(upd.Approval.RequestID)
	r.src.byID["acc_full"].ExternalAddAttendee(provider.EventRef{CalendarID: fake.PrimaryID, EventID: ev.ID}, provider.Attendee{Email: "eve@other.example"})
	st, errText := run[tools.InviteStatusOut](t, r.d, tools.ToolInviteStatus, map[string]any{"request_id": upd.Approval.RequestID})
	if errText != "" || st.Status != "stale" || !strings.Contains(st.Note, "nothing was written") {
		t.Fatalf("status = %+v %q", st, errText)
	}
	if len(r.src.byID["acc_full"].Notifications()) != 0 {
		t.Error("a stale approval notified someone")
	}
}

func TestAttendeeOnlyEventsAreRejectedBeforeARequestExists(t *testing.T) {
	r := newInviteRig(t)
	ev := r.src.byID["acc_full"].AddEvent(fake.PrimaryID, provider.Event{
		Title: "Theirs", Start: utc(9, 0), End: utc(10, 0), Organizer: provider.Attendee{Email: "boss@other.example"},
		Attendees: []provider.Attendee{{Email: "boss@other.example"}, {Email: "full@example.com", Self: true}},
	})
	_, errText := run[tools.WriteOut](t, r.d, tools.ToolUpdateEvent, map[string]any{"id": evID("acc_full", ev), "title": "Mine"})
	if !strings.HasPrefix(errText, "organizer_only") || !strings.Contains(errText, "calendar_respond_to_invite") {
		t.Errorf("update = %q", errText)
	}
	_, errText = run[tools.WriteOut](t, r.d, tools.ToolDeleteEvent, map[string]any{"id": evID("acc_full", ev)})
	if !strings.HasPrefix(errText, "organizer_only") {
		t.Errorf("delete = %q", errText)
	}
	if r.ap.Creates != 0 {
		t.Errorf("%d approver requests for events the account does not organize", r.ap.Creates)
	}
}

func TestAttendeeWritesStayInvitesNotEnabledWhenTheAccountOrServerIsNotReady(t *testing.T) {
	r := newInviteRig(t)
	if err := r.st.UpdateAccount(context.Background(), "pat", "acc_full", store.AccountPatch{InviteEnabled: store.Ptr(false)}); err != nil {
		t.Fatal(err)
	}
	_, errText := run[tools.WriteOut](t, r.d, tools.ToolCreateEvent, create("attendees", attendeesArg("bob@other.example")))
	if !strings.HasPrefix(errText, "invites_not_enabled") || !strings.Contains(errText, "calendar_account_enable_invites") {
		t.Errorf("invites off = %q", errText)
	}
	ev := r.seedWithAttendees("Sync", "full@example.com", "bob@other.example")
	_, errText = run[tools.WriteOut](t, r.d, tools.ToolDeleteEvent, map[string]any{"id": evID("acc_full", ev)})
	if !strings.HasPrefix(errText, "invites_not_enabled") {
		t.Errorf("delete with invites off = %q", errText)
	}
	if r.ap.Creates != 0 {
		t.Error("an approver request was made for an account with invites off")
	}
	// A server with no approval service behaves as before, even for an enabled account.
	d, _, _ := deps(t)
	_, errText = run[tools.WriteOut](t, d, tools.ToolCreateEvent, create("attendees", attendeesArg("bob@other.example")))
	if !strings.HasPrefix(errText, "invites_not_enabled") {
		t.Errorf("no engine = %q", errText)
	}
	_, errText = run[tools.InviteStatusOut](t, d, tools.ToolInviteStatus, map[string]any{"request_id": "x"})
	if !strings.HasPrefix(errText, "invites_unavailable") {
		t.Errorf("status without an engine = %q", errText)
	}
	_, errText = run[tools.EnableInvitesOut](t, d, tools.ToolEnableInvites, map[string]any{"account": "full@example.com", "from_address": "full@example.com", "enable": true})
	if !strings.HasPrefix(errText, "invites_unavailable") {
		t.Errorf("enable without an engine = %q", errText)
	}
}

func TestSelfOnlyWritesStillApplyDirectlyWithInvitesOn(t *testing.T) {
	r := newInviteRig(t)
	out, errText := run[tools.WriteOut](t, r.d, tools.ToolCreateEvent, create())
	if errText != "" || out.Status != "created" || out.Approval != nil {
		t.Fatalf("out = %+v %q", out, errText)
	}
	if r.ap.Creates != 0 {
		t.Error("a self-only event went to approval")
	}
}

func TestInviteStatusOfSomeoneElsesRequestIsNotFound(t *testing.T) {
	r := newInviteRig(t)
	out, _ := run[tools.WriteOut](t, r.d, tools.ToolCreateEvent, create("attendees", attendeesArg("bob@other.example")))
	other := r.d
	other.FallbackOwner = "other"
	_, errText := run[tools.InviteStatusOut](t, other, tools.ToolInviteStatus, map[string]any{"request_id": out.Approval.RequestID})
	if !strings.HasPrefix(errText, "not_found") {
		t.Errorf("errText = %q", errText)
	}
	_, errText = run[tools.InviteStatusOut](t, r.d, tools.ToolInviteStatus, map[string]any{"request_id": " "})
	if !strings.HasPrefix(errText, "invalid_input") {
		t.Errorf("empty id = %q", errText)
	}
}

func TestEnableInvitesNeedsAPasskeyApprovalAndDisablingIsDirect(t *testing.T) {
	r := newInviteRig(t)
	if err := r.st.UpdateAccount(context.Background(), "pat", "acc_full", store.AccountPatch{InviteEnabled: store.Ptr(false)}); err != nil {
		t.Fatal(err)
	}
	args := map[string]any{"account": "full@example.com", "from_address": "full@example.com", "enable": true}
	out, errText := run[tools.EnableInvitesOut](t, r.d, tools.ToolEnableInvites, args)
	if errText != "" || out.Status != "pending" || out.ApprovalURL == "" || out.RequestID == "" || out.InviteEnabled || out.Account.ID != "acc_full" {
		t.Fatalf("enable = %+v %q", out, errText)
	}
	if kind, _, _ := r.ap.Request(out.RequestID); kind != api.KindAccountInviteEnable {
		t.Errorf("kind = %s", kind)
	}
	// Still off until the owner approves.
	if a, _ := r.st.GetAccount(context.Background(), "pat", "acc_full"); a.InviteEnabled {
		t.Fatal("invites were enabled without an approval")
	}
	r.ap.Approve(out.RequestID)
	// calendar_list_accounts applies the approval that arrived.
	_, listed := call(t, r.d, nil)
	if !byID(listed)["acc_full"].Calendar.InviteEnabled {
		t.Fatal("the approved enable was not applied by calendar_list_accounts")
	}
	again, errText := run[tools.EnableInvitesOut](t, r.d, tools.ToolEnableInvites, args)
	if errText != "" || again.Status != "already_enabled" || !again.InviteEnabled {
		t.Fatalf("again = %+v %q", again, errText)
	}
	off, errText := run[tools.EnableInvitesOut](t, r.d, tools.ToolEnableInvites, map[string]any{"account": "full@example.com", "from_address": "full@example.com", "enable": false})
	if errText != "" || off.Status != "disabled" || off.InviteEnabled {
		t.Fatalf("disable = %+v %q", off, errText)
	}
	if a, _ := r.st.GetAccount(context.Background(), "pat", "acc_full"); a.InviteEnabled {
		t.Fatal("disable did not turn invites off")
	}
	// After a disable the used approval does nothing, and a write is refused again.
	_, errText = run[tools.WriteOut](t, r.d, tools.ToolCreateEvent, create("attendees", attendeesArg("bob@other.example")))
	if !strings.HasPrefix(errText, "invites_not_enabled") {
		t.Errorf("after disable = %q", errText)
	}
}

func TestEnableInvitesRefusals(t *testing.T) {
	r := newInviteRig(t)
	if err := r.st.UpdateAccount(context.Background(), "pat", "acc_full", store.AccountPatch{InviteEnabled: store.Ptr(false)}); err != nil {
		t.Fatal(err)
	}
	cases := map[string]struct {
		args map[string]any
		code string
	}{
		"no account":       {map[string]any{"account": "", "from_address": "full@example.com", "enable": true}, "needs_account"},
		"unknown account":  {map[string]any{"account": "nobody@example.com", "from_address": "nobody@example.com", "enable": true}, "account_not_found"},
		"wrong from":       {map[string]any{"account": "full@example.com", "from_address": "part@example.com", "enable": true}, "from_mismatch"},
		"calendar partial": {map[string]any{"account": "part@example.com", "from_address": "part@example.com", "enable": true}, "calendar_not_granted"},
		"needs reauth":     {map[string]any{"account": "mail@example.com", "from_address": "mail@example.com", "enable": true}, "needs_reauth"},
		"another owner":    {map[string]any{"account": "theirs@example.com", "from_address": "theirs@example.com", "enable": true}, "account_not_found"},
	}
	for name, c := range cases {
		if _, errText := run[tools.EnableInvitesOut](t, r.d, tools.ToolEnableInvites, c.args); !strings.HasPrefix(errText, c.code) {
			t.Errorf("%s: %q, want %s", name, errText, c.code)
		}
	}
	if r.ap.Creates != 0 {
		t.Errorf("%d approver requests were created by refused enables", r.ap.Creates)
	}
}
