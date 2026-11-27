package approver_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/change-fabric/moose/internal/approver/api"
	"github.com/change-fabric/moose/internal/approver/receipt"
)

func calEvent(title, desc string, attendees ...string) *api.CalEvent {
	e := &api.CalEvent{
		Title: title, Description: desc,
		Start:     api.CalTime{DateTime: "2026-03-04T09:00:00+01:00", Zone: "Europe/Berlin"},
		End:       api.CalTime{DateTime: "2026-03-04T10:00:00+01:00", Zone: "Europe/Berlin"},
		Attendees: []api.CalAttendee{},
	}
	for _, a := range attendees {
		e.Attendees = append(e.Attendees, api.CalAttendee{Email: a, External: !strings.HasSuffix(a, "@example.com")})
	}
	return e
}

type inviteOpts struct {
	Key       string
	Op        string
	Original  *api.CalEvent
	Proposed  *api.CalEvent
	NoInvites bool
	NoCal     bool
	Status    string
	Role      string
}

func inviteBody(a acct, o inviteOpts) api.CreateBody {
	if o.Op == "" {
		o.Op = api.CalOpCreate
	}
	if o.Key == "" {
		o.Key = "inv-" + a.ID + "-" + o.Op
	}
	if o.Status == "" {
		o.Status = "ok"
	}
	if o.Role == "" {
		o.Role = api.CalRoleOrganizer
	}
	p := api.CalendarInvitePayload{
		Owner: owner, AccountID: a.ID, FromAddress: a.Addr, Operation: o.Op, OrganizerRole: o.Role,
		Target: api.CalTarget{CalendarID: "cal-primary"}, Original: o.Original, Proposed: o.Proposed,
		SnapshotSHA256: sum("snapshot-" + o.Key),
	}
	if o.Op != api.CalOpCreate {
		p.Target.EventID, p.Target.Scope, p.Version = "ev1", api.CalScopeInstance, "v1"
	}
	p.Recipients = api.RecipientsFor(a.Addr, o.Op, o.Original, o.Proposed)
	payload, _ := json.Marshal(p)
	yes, no := true, false
	inv, cal := &yes, &yes
	if o.NoInvites {
		inv = &no
	}
	if o.NoCal {
		cal = &no
	}
	preview, _ := json.Marshal(api.Preview{
		AccountID: a.ID, Nickname: a.Nick, FromAddress: a.Addr, DisplayName: a.Display, ColorIndex: a.Color,
		AccountStatus: o.Status, InviteEnabled: inv, CalendarGranted: cal,
	})
	return api.CreateBody{IdempotencyKey: o.Key, Kind: api.KindCalendarInvite, Payload: payload, Preview: preview}
}

func TestInvitePageShowsBannerFullUntrustedContentAndRecipients(t *testing.T) {
	h := newHarness(t)
	longDesc := strings.Repeat("Agenda line that must not be clipped.\n", 120) + "END-OF-DESCRIPTION"
	title := `Kickoff <script>alert(1)</script> & "friends" ` + strings.Repeat("long ", 40)
	id := h.create(inviteBody(work, inviteOpts{Proposed: calEvent(title, longDesc, "alice@example.com", "bob@elsewhere.example")})).RequestID
	r := h.get("/approve/r/" + id)
	if r.Code != http.StatusOK {
		t.Fatalf("page = %d", r.Code)
	}
	for _, want := range []string{
		"Organizer account", "alice@example.com", "Send invitations from Work (alice@example.com)",
		`data-needs-from="true"`, "Approve this invitation",
		"END-OF-DESCRIPTION", "&lt;script&gt;alert(1)&lt;/script&gt;", "&amp; &#34;friends&#34;",
		"are written by whoever made the event", "untrusted content",
		"bob@elsewhere.example", "external", "will be invited",
		"Wed 4 Mar 2026 09:00 Europe/Berlin (UTC&#43;01:00)",
	} {
		if !strings.Contains(r.Body, want) {
			t.Errorf("page lacks %q", want)
		}
	}
	if strings.Contains(r.Body, "<script>alert(1)") {
		t.Error("untrusted title reached the page as markup")
	}
	if strings.Contains(r.Body, "alice@example.com</span>") && strings.Count(r.Body, "will be invited") != 1 {
		t.Error("the account itself is listed as a recipient")
	}
	// The pending page needs the FROM tap before the passkey.
	if !strings.Contains(r.Body, `id="confirm-from"`) {
		t.Error("no FROM confirm step")
	}
	if d := h.decide(id, "approve"); d.Code != http.StatusConflict || !strings.Contains(d.Body, "from_not_confirmed") {
		t.Errorf("approve before the tap = %d %s", d.Code, d.Body)
	}
}

func TestInviteUpdateHighlightsEachChangedFieldAgainstTheOriginal(t *testing.T) {
	h := newHarness(t)
	was := calEvent("Weekly sync", "Old agenda", "alice@example.com", "bob@elsewhere.example", "carol@example.com")
	will := calEvent("Weekly sync", "New agenda <b>bold</b>", "alice@example.com", "bob@elsewhere.example", "dave@example.com")
	will.Start = api.CalTime{DateTime: "2026-03-04T10:00:00+01:00", Zone: "Europe/Berlin"}
	will.End = api.CalTime{DateTime: "2026-03-04T11:00:00+01:00", Zone: "Europe/Berlin"}
	id := h.create(inviteBody(work, inviteOpts{Op: api.CalOpUpdate, Original: was, Proposed: will})).RequestID
	body := h.get("/approve/r/" + id).Body
	if !strings.Contains(body, "Approve this event update") {
		t.Error("no update heading")
	}
	// Starts, Ends, Attendees and Description changed; Title and Location did not.
	if n := strings.Count(body, `class="changed-row"`); n != 4 {
		t.Errorf("%d changed rows, want 4 (starts, ends, attendees, description)", n)
	}
	for _, want := range []string{"Old agenda", "New agenda &lt;b&gt;bold&lt;/b&gt;", "Was:", "09:00 Europe/Berlin", "10:00 Europe/Berlin", "carol@example.com", "dave@example.com"} {
		if !strings.Contains(body, want) {
			t.Errorf("update page lacks %q", want)
		}
	}
	// carol is removed from the event, so she is told; dave is new; bob keeps it.
	for _, want := range []string{"will be notified of removal", "will be invited", "will be notified of the change"} {
		if !strings.Contains(body, want) {
			t.Errorf("update page lacks %q", want)
		}
	}
	if strings.Contains(body, "<b>bold</b>") {
		t.Error("a description with markup reached the page as markup")
	}
}

func TestInviteCancelListsEveryoneAsNotifiedOfTheCancellation(t *testing.T) {
	h := newHarness(t)
	was := calEvent("Offsite", "Plans", "alice@example.com", "bob@elsewhere.example", "carol@example.com")
	id := h.create(inviteBody(work, inviteOpts{Op: api.CalOpCancel, Original: was})).RequestID
	body := h.get("/approve/r/" + id).Body
	if !strings.Contains(body, "Approve this cancellation") || strings.Count(body, "will be notified of the cancellation") != 2 {
		t.Error("cancel page does not list both attendees as notified of the cancellation")
	}
	if strings.Contains(body, `class="changed-row"`) {
		t.Error("a cancel has nothing to highlight")
	}
}

func TestInviteRefusalsAtCreate(t *testing.T) {
	h := newHarness(t)
	two := calEvent("Sync", "", "alice@example.com", "bob@elsewhere.example")
	for name, o := range map[string]inviteOpts{
		"an attendee-only account": {Role: api.CalRoleAttendee, Key: "k1", Op: api.CalOpUpdate, Original: two, Proposed: calEvent("Sync 2", "", "alice@example.com", "bob@elsewhere.example")},
		"invites not enabled":      {NoInvites: true, Key: "k2", Proposed: two},
		"calendar not granted":     {NoCal: true, Key: "k3", Proposed: two},
		"account needing reauth":   {Status: "needs_reauth", Key: "k4", Proposed: two},
	} {
		_, _, err := h.client.Create(context.Background(), inviteBody(work, o))
		var ae *api.Error
		if !errors.As(err, &ae) || ae.Status != http.StatusUnprocessableEntity {
			t.Errorf("%s: err = %v, want a 422", name, err)
		}
	}
	// A preview that names another account than the payload is refused.
	b := inviteBody(work, inviteOpts{Key: "k5", Proposed: two})
	b.Preview, _ = json.Marshal(api.Preview{AccountID: personal.ID, Nickname: "x", FromAddress: personal.Addr, AccountStatus: "ok"})
	if _, _, err := h.client.Create(context.Background(), b); err == nil {
		t.Error("a preview for another account was accepted")
	}
}

func TestInviteApprovalCarriesAReceiptThatBindsTheWholePayload(t *testing.T) {
	h := newHarness(t)
	h.enroll()
	body := inviteBody(work, inviteOpts{Proposed: calEvent("Kickoff", "d", "alice@example.com", "bob@elsewhere.example")})
	resp := h.create(body)
	h.approveSend(resp.RequestID)
	st, err := h.client.Status(context.Background(), resp.RequestID)
	if err != nil || st.Status != "approved" || st.Receipt == nil {
		t.Fatalf("status = %+v, %v", st, err)
	}
	if err := receipt.Verify(h.pub, *st.Receipt, receipt.Want{
		RequestID: resp.RequestID, Kind: api.KindCalendarInvite, PayloadSHA256: canonSHA(t, body.Payload), RPID: rpID, Now: h.clock(),
	}); err != nil {
		t.Fatalf("receipt: %v", err)
	}
	if err := receipt.Verify(h.pub, *st.Receipt, receipt.Want{
		RequestID: resp.RequestID, Kind: api.KindAccountInviteEnable, PayloadSHA256: canonSHA(t, body.Payload), RPID: rpID, Now: h.clock(),
	}); err == nil {
		t.Error("a calendar-invite receipt verified as an account-invite-enable receipt")
	}
	if _, err := h.client.Consume(context.Background(), resp.RequestID, "claimant"); err != nil {
		t.Fatalf("consume: %v", err)
	}
	if _, err := h.client.Consume(context.Background(), resp.RequestID, "someone-else"); !errors.Is(err, api.ErrAlreadyConsumed) {
		t.Errorf("a second claimant err = %v, want ErrAlreadyConsumed", err)
	}
}

func TestAccountInviteEnablePageMirrorsSendEnable(t *testing.T) {
	h := newHarness(t)
	payload, _ := json.Marshal(api.InviteEnablePayload{Owner: owner, AccountID: work.ID, FromAddress: work.Addr})
	yes := true
	preview, _ := json.Marshal(api.Preview{
		Action: "Enable invitations", AccountID: work.ID, Nickname: work.Nick, FromAddress: work.Addr, DisplayName: work.Display,
		ColorIndex: work.Color, AccountStatus: "ok", CalendarGranted: &yes,
	})
	id := h.create(api.CreateBody{IdempotencyKey: "enable-inv", Kind: api.KindAccountInviteEnable, Payload: payload, Preview: preview}).RequestID
	body := h.get("/approve/r/" + id).Body
	for _, want := range []string{"Enable invitations from this account", "Enable invitations from Work (alice@example.com)", "Organizer account", `data-needs-from="true"`, "without a passkey"} {
		if !strings.Contains(body, want) {
			t.Errorf("enable page lacks %q", want)
		}
	}
	// Without a calendar grant the approver refuses to create it.
	no := false
	bad, _ := json.Marshal(api.Preview{AccountID: work.ID, Nickname: work.Nick, FromAddress: work.Addr, AccountStatus: "ok", CalendarGranted: &no})
	if _, _, err := h.client.Create(context.Background(), api.CreateBody{IdempotencyKey: "enable-inv-2", Kind: api.KindAccountInviteEnable, Payload: payload, Preview: bad}); err == nil {
		t.Error("an enable request without a calendar grant was accepted")
	}
}
