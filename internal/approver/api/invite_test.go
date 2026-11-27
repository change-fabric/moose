package api

import (
	"encoding/json"
	"strings"
	"testing"
)

func ev(title string, attendees ...string) *CalEvent {
	e := &CalEvent{
		Title: title, Start: CalTime{DateTime: "2026-03-04T09:00:00+01:00", Zone: "Europe/Berlin"},
		End: CalTime{DateTime: "2026-03-04T10:00:00+01:00", Zone: "Europe/Berlin"}, Attendees: []CalAttendee{},
	}
	for _, a := range attendees {
		e.Attendees = append(e.Attendees, CalAttendee{Email: a})
	}
	return e
}

func invite(op string, original, proposed *CalEvent) CalendarInvitePayload {
	p := CalendarInvitePayload{
		Owner: "o", AccountID: "acc_1", FromAddress: "me@example.com", Operation: op, OrganizerRole: CalRoleOrganizer,
		Target: CalTarget{CalendarID: "cal"}, Original: original, Proposed: proposed, SnapshotSHA256: strings.Repeat("b", 64),
	}
	if op != CalOpCreate {
		p.Target.EventID, p.Target.Scope, p.Version = "ev1", CalScopeInstance, "v1"
	}
	p.Recipients = RecipientsFor(p.FromAddress, op, original, proposed)
	return p
}

func parseInvite(t *testing.T, p CalendarInvitePayload) error {
	t.Helper()
	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	_, err = ParsePayload(KindCalendarInvite, raw)
	return err
}

func TestRecipientsForCoversAddedKeptRemovedAndCancelled(t *testing.T) {
	was := ev("Sync", "me@example.com", "keep@example.com", "gone@other.example")
	will := ev("Sync", "me@example.com", "keep@example.com", "New@Other.example")
	got := RecipientsFor("me@example.com", CalOpUpdate, was, will)
	want := []CalRecipient{
		{Email: "new@other.example", External: true, Change: CalChangeAdded},
		{Email: "keep@example.com", Change: CalChangeKept},
		{Email: "gone@other.example", External: true, Change: CalChangeRemoved},
	}
	if len(got) != len(want) {
		t.Fatalf("recipients = %+v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("recipient %d = %+v, want %+v", i, got[i], want[i])
		}
	}
	for _, r := range RecipientsFor("me@example.com", CalOpCancel, was, nil) {
		if r.Change != CalChangeCancelled || r.Email == "me@example.com" {
			t.Errorf("cancel recipient %+v", r)
		}
	}
	if got := RecipientsFor("me@example.com", CalOpCreate, nil, ev("Solo", "me@example.com")); len(got) != 0 {
		t.Errorf("a self-only create notifies %+v", got)
	}
}

func TestCalendarInvitePayloadAcceptsEachOperation(t *testing.T) {
	was := ev("Sync", "me@example.com", "bob@other.example")
	will := ev("Sync v2", "me@example.com")
	for name, p := range map[string]CalendarInvitePayload{
		"create": invite(CalOpCreate, nil, ev("Kickoff", "bob@other.example")),
		"update": invite(CalOpUpdate, was, will),
		"cancel": invite(CalOpCancel, was, nil),
	} {
		if err := parseInvite(t, p); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	// A create carries a null original; the kind parses and keeps the pointer fields.
	raw, _ := json.Marshal(invite(CalOpCreate, nil, ev("Kickoff", "bob@other.example")))
	parsed, err := ParsePayload(KindCalendarInvite, raw)
	if err != nil || parsed.Invite == nil || parsed.Invite.Original != nil || parsed.Invite.Proposed == nil || parsed.AccountID != "acc_1" || parsed.From != "me@example.com" {
		t.Fatalf("parsed = %+v, %v", parsed, err)
	}
	if !strings.Contains(string(parsed.Canonical), `"original":null`) {
		t.Errorf("a create's canonical payload must say original null: %s", parsed.Canonical)
	}
}

func TestCalendarInvitePayloadRefusals(t *testing.T) {
	was := ev("Sync", "me@example.com", "bob@other.example")
	will := ev("Sync v2", "me@example.com", "bob@other.example")
	good := func() CalendarInvitePayload { return invite(CalOpUpdate, was, will) }
	cases := map[string]func(p *CalendarInvitePayload){
		"attendee role":           func(p *CalendarInvitePayload) { p.OrganizerRole = CalRoleAttendee },
		"unknown role":            func(p *CalendarInvitePayload) { p.OrganizerRole = "" },
		"unknown operation":       func(p *CalendarInvitePayload) { p.Operation = "move" },
		"update without original": func(p *CalendarInvitePayload) { p.Original = nil },
		"update without proposed": func(p *CalendarInvitePayload) { p.Proposed = nil },
		"no version":              func(p *CalendarInvitePayload) { p.Version = "" },
		"no event id":             func(p *CalendarInvitePayload) { p.Target.EventID = "" },
		"bad scope":               func(p *CalendarInvitePayload) { p.Target.Scope = "following" },
		"bad snapshot hash":       func(p *CalendarInvitePayload) { p.SnapshotSHA256 = "zz" },
		"no recipients":           func(p *CalendarInvitePayload) { p.Recipients = nil },
		"an extra recipient": func(p *CalendarInvitePayload) {
			p.Recipients = append(p.Recipients, CalRecipient{Email: "eve@other.example", External: true, Change: CalChangeAdded})
		},
		"wrong change":   func(p *CalendarInvitePayload) { p.Recipients[0].Change = CalChangeRemoved },
		"wrong external": func(p *CalendarInvitePayload) { p.Recipients[0].External = false },
		"no calendar":    func(p *CalendarInvitePayload) { p.Target.CalendarID = "" },
		"no title":       func(p *CalendarInvitePayload) { p.Proposed.Title = " " },
		"huge description": func(p *CalendarInvitePayload) {
			p.Proposed = ev("t", "me@example.com", "bob@other.example")
			p.Proposed.Description = strings.Repeat("x", maxInviteText+1)
		},
	}
	for name, mut := range cases {
		p := good()
		// Deep enough: the events are shared, so copy before mutating them.
		o, pr := *p.Original, *p.Proposed
		p.Original, p.Proposed = &o, &pr
		p.Recipients = append([]CalRecipient(nil), p.Recipients...)
		mut(&p)
		if err := parseInvite(t, p); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// A create must not carry an original, a version or a target event.
	c := invite(CalOpCreate, nil, ev("Kickoff", "bob@other.example"))
	c.Version = "v1"
	if parseInvite(t, c) == nil {
		t.Error("a create with a version was accepted")
	}
	// A change that notifies nobody is not an invite and needs no approval.
	if err := parseInvite(t, invite(CalOpUpdate, ev("a", "me@example.com"), ev("b", "me@example.com"))); err == nil {
		t.Error("a self-only update was accepted as an invite")
	}
	// A cancel has no proposed event.
	cc := invite(CalOpCancel, was, nil)
	cc.Proposed = will
	if parseInvite(t, cc) == nil {
		t.Error("a cancel with a proposed event was accepted")
	}
}

func TestCalendarInvitePayloadDecodesStrictly(t *testing.T) {
	raw, _ := json.Marshal(invite(CalOpCreate, nil, ev("Kickoff", "bob@other.example")))
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	m["extra"] = "smuggled"
	bad, _ := json.Marshal(m)
	if _, err := ParsePayload(KindCalendarInvite, bad); err == nil {
		t.Error("an unknown key was accepted")
	}
	if _, err := ParsePayload(KindAccountInviteEnable, []byte(`{"owner":"o","account_id":"a","from_address":"f@x.y","extra":1}`)); err == nil {
		t.Error("an unknown key was accepted on account-invite-enable")
	}
}

func TestInviteEnablePayloadAndKinds(t *testing.T) {
	p, err := ParsePayload(KindAccountInviteEnable, []byte(`{"owner":"o","account_id":"a","from_address":"f@x.y"}`))
	if err != nil || p.InviteEnable == nil || p.AccountID != "a" || p.From != "f@x.y" {
		t.Fatalf("parsed = %+v, %v", p, err)
	}
	if _, err := ParsePayload(KindAccountInviteEnable, []byte(`{"owner":"o","account_id":"a"}`)); err == nil {
		t.Error("an enable without from_address was accepted")
	}
	for kind, want := range map[string]bool{KindCalendarInvite: true, KindAccountInviteEnable: true, KindContactTrustBatch: false} {
		if FromKinds(kind) != want {
			t.Errorf("FromKinds(%s) = %v", kind, !want)
		}
	}
	if !InviteKinds(KindCalendarInvite) || InviteKinds(KindAccountInviteEnable) || InviteKinds(KindEmailSend) {
		t.Error("InviteKinds must be calendar-invite only")
	}
}

func TestCheckPreviewGatesInviteKinds(t *testing.T) {
	raw, _ := json.Marshal(invite(CalOpCreate, nil, ev("Kickoff", "bob@other.example")))
	p, err := ParsePayload(KindCalendarInvite, raw)
	if err != nil {
		t.Fatal(err)
	}
	yes, no := true, false
	pv := func(mut func(*Preview)) Preview {
		v := Preview{AccountID: "acc_1", FromAddress: "me@example.com", Nickname: "Me", AccountStatus: "ok", InviteEnabled: &yes, CalendarGranted: &yes}
		mut(&v)
		return v
	}
	if c := checkPreview(p, pv(func(*Preview) {})); c != nil {
		t.Fatalf("a ready account was refused: %+v", c)
	}
	for name, v := range map[string]Preview{
		"invites off":     pv(func(v *Preview) { v.InviteEnabled = &no }),
		"invites unknown": pv(func(v *Preview) { v.InviteEnabled = nil }),
		"calendar off":    pv(func(v *Preview) { v.CalendarGranted = &no }),
		"calendar absent": pv(func(v *Preview) { v.CalendarGranted = nil }),
		"needs reauth":    pv(func(v *Preview) { v.AccountStatus = "needs_reauth" }),
		"other account":   pv(func(v *Preview) { v.AccountID = "acc_2" }),
		"other address":   pv(func(v *Preview) { v.FromAddress = "else@example.com" }),
	} {
		if c := checkPreview(p, v); c == nil {
			t.Errorf("%s: preview accepted", name)
		}
	}
	en, _ := ParsePayload(KindAccountInviteEnable, []byte(`{"owner":"o","account_id":"acc_1","from_address":"me@example.com"}`))
	if c := checkPreview(en, pv(func(v *Preview) { v.InviteEnabled = nil })); c != nil {
		t.Errorf("an enable request needs no invites flag: %+v", c)
	}
	if c := checkPreview(en, pv(func(v *Preview) { v.CalendarGranted = nil })); c == nil {
		t.Error("an enable request without a calendar grant was accepted")
	}
}
