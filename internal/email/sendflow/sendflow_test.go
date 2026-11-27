package sendflow_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
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
	"github.com/change-fabric/moose/internal/email/fake"
	"github.com/change-fabric/moose/internal/email/mailmsg"
	"github.com/change-fabric/moose/internal/email/provider"
	"github.com/change-fabric/moose/internal/email/sendflow"
	"github.com/change-fabric/moose/internal/email/sendflow/flowtest"
)

var ctx = context.Background()

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

type providers map[string]provider.Provider

func (p providers) Provider(_ context.Context, a store.Account) (provider.Provider, error) {
	if v, ok := p[a.ID]; ok {
		return v, nil
	}
	return nil, errors.New("no provider for " + a.ID)
}

type rig struct {
	t    *testing.T
	st   store.Store
	ap   *flowtest.Approver
	prov *fake.Provider
	svc  *sendflow.Service
	clk  *clock
	acct store.Account
}

func newRig(t *testing.T, live bool) *rig {
	t.Helper()
	st, err := sqlite.OpenStore(ctx, filepath.Join(t.TempDir(), "e.db"))
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
	prov := fake.New()
	acct := store.Account{Owner: "pat", ID: "acc_one", Provider: "gmail", ProviderUID: "me@example.com", Address: "me@example.com",
		Nickname: "Me", Status: store.StatusOK, SendEnabled: true, HourlyCap: 10, Created: clk.Now(), Updated: clk.Now()}
	if err := st.CreateAccount(ctx, acct); err != nil {
		t.Fatal(err)
	}
	r := &rig{t: t, st: st, ap: ap, prov: prov, clk: clk, acct: acct}
	r.svc = &sendflow.Service{Store: st, Seal: seal, Approver: ap, Providers: providers{acct.ID: prov},
		ReceiptKey: ap.PublicKeyPEM(), RPID: flowtest.RPID, Live: live, Now: clk.Now}
	return r
}

// addAccount adds a second account with its own provider.
func (r *rig) addAccount(id, addr string) (store.Account, *fake.Provider) {
	r.t.Helper()
	a := store.Account{Owner: "pat", ID: id, Provider: "gmail", ProviderUID: addr, Address: addr, Nickname: id, Status: store.StatusOK,
		SendEnabled: true, HourlyCap: 10, ColorIndex: 1, Created: r.clk.Now(), Updated: r.clk.Now()}
	if err := r.st.CreateAccount(ctx, a); err != nil {
		r.t.Fatal(err)
	}
	p := fake.New()
	r.svc.Providers.(providers)[id] = p
	return a, p
}

func (r *rig) draft(to, subject, body string) string {
	r.t.Helper()
	return r.draftFor(r.prov, r.acct.Address, to, subject, body)
}

func (r *rig) draftFor(p *fake.Provider, from, to, subject, body string) string {
	r.t.Helper()
	raw, err := mailmsg.Build(mailmsg.Message{From: from, To: []string{to}, Subject: subject, Body: body}, r.clk.Now())
	if err != nil {
		r.t.Fatal(err)
	}
	d, err := p.CreateDraft(ctx, provider.DraftInput{Raw: raw})
	if err != nil {
		r.t.Fatal(err)
	}
	return d.ID
}

func (r *rig) request(draftID string) sendflow.Item {
	r.t.Helper()
	out, err := r.svc.RequestSend(ctx, sendflow.RequestIn{Account: r.acct, FromAddress: r.acct.Address, DraftIDs: []string{draftID}})
	if err != nil {
		r.t.Fatalf("RequestSend: %v", err)
	}
	if len(out.Items) != 1 {
		r.t.Fatalf("items = %+v", out.Items)
	}
	return out.Items[0]
}

// approved requests a send and approves it with a real receipt.
func (r *rig) approved(draftID string) string {
	r.t.Helper()
	it := r.request(draftID)
	r.ap.Approve(it.RequestID)
	return it.RequestID
}

func (r *rig) status(id string) sendflow.StatusOut {
	r.t.Helper()
	out, err := r.svc.Status(ctx, "pat", id)
	if err != nil {
		r.t.Fatalf("Status(%s): %v", id, err)
	}
	return out
}

func (r *rig) refusal(err error) *sendflow.Error {
	r.t.Helper()
	var e *sendflow.Error
	if !errors.As(err, &e) {
		r.t.Fatalf("err = %v, want a *sendflow.Error", err)
	}
	return e
}

func (r *rig) approval(id string) store.Approval {
	r.t.Helper()
	a, err := r.st.GetApproval(ctx, id)
	if err != nil {
		r.t.Fatal(err)
	}
	return a
}

func sha(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }

// ---- the happy path and what is sent ----

func TestSendsExactlyTheApprovedSnapshotNotTheEditedDraft(t *testing.T) {
	r := newRig(t, true)
	id := r.draft("ann@example.com", "Plans", "approved body")
	approvedRaw, _ := r.prov.GetDraftRaw(ctx, id)
	reqID := r.approved(id)

	// The owner edits the draft in the mailbox after approving.
	edited, _ := mailmsg.Build(mailmsg.Message{From: r.acct.Address, To: []string{"evil@example.net"}, Subject: "Changed", Body: "different"}, r.clk.Now())
	r.prov.SetDraftRaw(id, edited)

	out := r.status(reqID)
	if out.Status != store.SendSent || out.ProviderMessageID == "" {
		t.Fatalf("status = %+v", out)
	}
	sent := r.prov.SentMessages()
	if len(sent) != 1 || !bytes.Equal(sent[0].Raw, approvedRaw) || bytes.Equal(sent[0].Raw, edited) {
		t.Fatalf("sent %d messages; bytes equal approved = %v", len(sent), len(sent) == 1 && bytes.Equal(sent[0].Raw, approvedRaw))
	}
	if !out.SourceDraftRetained || out.SourceDraftID != id {
		t.Errorf("source draft not reported as retained: %+v", out)
	}
	if got, err := r.prov.GetDraftRaw(ctx, id); err != nil || !bytes.Equal(got, edited) {
		t.Errorf("the source draft must be left alone: %v", err)
	}
}

func TestRequestHashesAndPayloadCoverAccountAndFrom(t *testing.T) {
	r := newRig(t, true)
	id := r.draft("ann@example.com", "Hi", "body")
	raw, _ := r.prov.GetDraftRaw(ctx, id)
	it := r.request(id)
	ap := r.approval(it.RequestID)
	if ap.RFC822SHA256 != sha(raw) || ap.DraftID != id || ap.Kind != store.KindEmailSend {
		t.Fatalf("approval = %+v", ap)
	}
	p, err := api.ParsePayload(api.KindEmailSend, ap.Payload)
	if err != nil || p.Email.AccountID != r.acct.ID || p.Email.FromAddress != r.acct.Address || p.Email.RFC822SHA256 != sha(raw) || p.Email.DraftID != id {
		t.Fatalf("payload = %+v, %v", p.Email, err)
	}
	if len(ap.Snapshot) == 0 || bytes.Contains(ap.Snapshot, []byte("body")) {
		t.Error("the snapshot must be stored sealed")
	}
	if ap.TTL.IsZero() || ap.TTL.Sub(r.clk.Now()) < 23*time.Hour || ap.TTL.Sub(r.clk.Now()) > 25*time.Hour {
		t.Errorf("abandoned request backstop ttl = %v, want about 24h", ap.TTL)
	}
}

func TestPreviewFieldsEqualTheFieldsParsedFromTheFinalBytes(t *testing.T) {
	r := newRig(t, true)
	raw, err := mailmsg.Build(mailmsg.Message{From: "Me <me@example.com>", To: []string{"Ann <Ann@Example.com>"}, Cc: []string{"cy@example.com"},
		Bcc: []string{"dee@example.org"}, Subject: "Café", Body: "line1\nline2 ünï"}, r.clk.Now())
	if err != nil {
		t.Fatal(err)
	}
	d, _ := r.prov.CreateDraft(ctx, provider.DraftInput{Raw: raw})
	it := r.request(d.ID)
	pv := r.ap.Preview(it.RequestID)
	// Parse the stored snapshot independently.
	snap, err := r.svc.Seal.Open(ctx, r.approval(it.RequestID).Snapshot, envelope.Context{Owner: "pat", AccountID: r.acct.ID})
	if err != nil {
		t.Fatal(err)
	}
	want, err := mailmsg.Parse(snap)
	if err != nil {
		t.Fatal(err)
	}
	m := pv.Message
	if m == nil || m.From != "me@example.com" || m.Subject != want.Subject || m.Body != want.Body ||
		strings.Join(m.To, ",") != "ann@example.com" || strings.Join(m.Cc, ",") != "cy@example.com" || strings.Join(m.Bcc, ",") != "dee@example.org" {
		t.Fatalf("preview message = %+v, parsed = %+v", m, want)
	}
	if pv.AccountID != r.acct.ID || pv.FromAddress != r.acct.Address || pv.AccountStatus != "ok" || pv.SendEnabled == nil || !*pv.SendEnabled {
		t.Errorf("preview banner = %+v", pv)
	}
	if !bytes.Equal(snap, raw) {
		t.Error("the snapshot is not the draft's exact bytes")
	}
}

func TestSoftFlagsAndTrustChangesForNewRecipients(t *testing.T) {
	r := newRig(t, true)
	// A known recipient (written to before) and a trusted one produce no change.
	_ = r.st.PutContact(ctx, store.Contact{AccountID: r.acct.ID, Address: "known@example.com", Trust: store.TrustKnown})
	_ = r.st.PutContact(ctx, store.Contact{AccountID: r.acct.ID, Address: "trusted@example.com", Trust: store.TrustTrusted})
	var many []string
	for i := 0; i < 11; i++ {
		many = append(many, fmt.Sprintf("p%d@example.com", i))
	}
	raw, _ := mailmsg.Build(mailmsg.Message{From: r.acct.Address, To: append([]string{"known@example.com", "trusted@example.com", "stranger@elsewhere.org"}, many...),
		Bcc: []string{"hidden@elsewhere.org"}, Subject: "s", Body: "b"}, r.clk.Now())
	d, _ := r.prov.CreateDraft(ctx, provider.DraftInput{Raw: raw})
	it := r.request(d.ID)
	flags := map[string]bool{}
	for _, f := range it.SoftFlags {
		flags[f.Flag+":"+f.Address] = true
	}
	for _, want := range []string{"new_recipient:stranger@elsewhere.org", "external_domain:stranger@elsewhere.org", "bcc:hidden@elsewhere.org", "many_recipients:", "new_recipient:hidden@elsewhere.org"} {
		if !flags[want] {
			t.Errorf("missing soft flag %s in %v", want, it.SoftFlags)
		}
	}
	if flags["new_recipient:known@example.com"] || flags["new_recipient:trusted@example.com"] || flags["external_domain:known@example.com"] {
		t.Errorf("known and trusted same-domain recipients must not be flagged: %v", it.SoftFlags)
	}
	p, _ := api.ParsePayload(api.KindEmailSend, r.approval(it.RequestID).Payload)
	changed := map[string]bool{}
	for _, c := range p.Email.TrustChanges {
		changed[c.Address] = c.To == "trusted"
	}
	if !changed["stranger@elsewhere.org"] || !changed["hidden@elsewhere.org"] || changed["known@example.com"] || changed["trusted@example.com"] {
		t.Errorf("trust changes = %+v", p.Email.TrustChanges)
	}

	// After the send, the approved changes are applied.
	r.ap.Approve(it.RequestID)
	if out := r.status(it.RequestID); out.Status != store.SendSent {
		t.Fatalf("status = %+v", out)
	}
	if c, err := r.st.GetContact(ctx, r.acct.ID, "stranger@elsewhere.org"); err != nil || c.Trust != store.TrustTrusted {
		t.Errorf("stranger after send = %+v, %v", c, err)
	}
}

func TestABlockedRecipientRefusesTheRequest(t *testing.T) {
	r := newRig(t, true)
	_ = r.st.PutContact(ctx, store.Contact{AccountID: r.acct.ID, Address: "spam@example.com", Trust: store.TrustBlocked})
	_, err := r.svc.RequestSend(ctx, sendflow.RequestIn{Account: r.acct, FromAddress: r.acct.Address, DraftIDs: []string{r.draft("spam@example.com", "s", "b")}})
	if e := r.refusal(err); e.Code != sendflow.CodeBlockedRecip {
		t.Fatalf("err = %v", e)
	}
	if r.ap.Creates != 0 {
		t.Error("a blocked recipient must be refused before any approval exists")
	}
}

// ---- normalization and size ----

func TestNormalizationRejectsMultipartHTMLAndAttachmentDrafts(t *testing.T) {
	r := newRig(t, true)
	head := "From: me@example.com\r\nTo: a@example.com\r\nSubject: s\r\n"
	for name, tc := range map[string]struct{ raw, want string }{
		"attachment": {head + "Content-Type: multipart/mixed; boundary=B\r\n\r\n--B\r\nContent-Type: text/plain\r\n\r\nhi\r\n--B\r\nContent-Type: application/pdf\r\nContent-Disposition: attachment; filename=a.pdf\r\n\r\nx\r\n--B--\r\n",
			"draft has 1 attachment; remove it, the PoC sends plain text only"},
		"multipart": {head + "Content-Type: multipart/alternative; boundary=B\r\n\r\n--B\r\nContent-Type: text/plain\r\n\r\nhi\r\n--B\r\nContent-Type: text/html\r\n\r\n<b>hi</b>\r\n--B--\r\n", "multipart/alternative"},
		"html only": {head + "Content-Type: text/html\r\n\r\n<p>hi</p>", "HTML only"},
	} {
		d, _ := r.prov.CreateDraft(ctx, provider.DraftInput{Raw: []byte(tc.raw)})
		_, err := r.svc.RequestSend(ctx, sendflow.RequestIn{Account: r.acct, FromAddress: r.acct.Address, DraftIDs: []string{d.ID}})
		e := r.refusal(err)
		if e.Code != sendflow.CodeNotSendable || !strings.Contains(e.Msg, tc.want) {
			t.Errorf("%s: %v, want %q", name, e, tc.want)
		}
	}
	if r.ap.Creates != 0 {
		t.Error("no approval may exist for a draft that is not plain text")
	}
}

func TestADraftOver256KBFailsBeforeAnyApprovalExists(t *testing.T) {
	r := newRig(t, true)
	big := []byte("From: me@example.com\r\nTo: a@example.com\r\nSubject: s\r\n\r\n" + strings.Repeat("x", provider.MaxDraftRaw))
	d, _ := r.prov.CreateDraft(ctx, provider.DraftInput{Raw: big})
	_, err := r.svc.RequestSend(ctx, sendflow.RequestIn{Account: r.acct, FromAddress: r.acct.Address, DraftIDs: []string{d.ID}})
	if e := r.refusal(err); e.Code != sendflow.CodeDraftTooLarge {
		t.Fatalf("err = %v", e)
	}
	if r.ap.Creates != 0 {
		t.Fatal("an approval request was created for an oversized draft")
	}
	// Exactly at the limit is accepted: 256 KB itself is not "plus one byte".
	head := "From: me@example.com\r\nTo: a@example.com\r\nSubject: s\r\n\r\n"
	edge := []byte(head + strings.Repeat("x", provider.MaxDraftRaw-len(head)))
	if len(edge) != provider.MaxDraftRaw {
		t.Fatalf("edge draft is %d bytes", len(edge))
	}
	d2, _ := r.prov.CreateDraft(ctx, provider.DraftInput{Raw: edge})
	if _, err := r.svc.RequestSend(ctx, sendflow.RequestIn{Account: r.acct, FromAddress: r.acct.Address, DraftIDs: []string{d2.ID}}); err != nil {
		t.Fatalf("a draft of exactly 256 KB: %v", err)
	}
}

// ---- identity: from_address and send_enabled ----

func TestFromAddressMustMatchTheAccountExactly(t *testing.T) {
	r := newRig(t, true)
	id := r.draft("ann@example.com", "s", "b")
	for _, from := range []string{"Me", "acc_one", "other@example.com", "me@example.com.evil.net", "", "  "} {
		_, err := r.svc.RequestSend(ctx, sendflow.RequestIn{Account: r.acct, FromAddress: from, DraftIDs: []string{id}})
		if e := r.refusal(err); e.Code != sendflow.CodeFromMismatch {
			t.Errorf("from %q: err = %v, want from_mismatch", from, e)
		}
	}
	// A draft whose From header is another address is refused too.
	other := r.draftFor(r.prov, "someone-else@example.com", "ann@example.com", "s", "b")
	_, err := r.svc.RequestSend(ctx, sendflow.RequestIn{Account: r.acct, FromAddress: r.acct.Address, DraftIDs: []string{other}})
	if e := r.refusal(err); e.Code != sendflow.CodeFromMismatch {
		t.Errorf("draft From header: %v", e)
	}
	if r.ap.Creates != 0 {
		t.Error("a mismatch must create no approval")
	}
}

func TestSendRefusedWhileSendEnabledIsFalse(t *testing.T) {
	r := newRig(t, true)
	r.acct.SendEnabled = false
	if err := r.st.UpdateAccount(ctx, r.acct); err != nil {
		t.Fatal(err)
	}
	_, err := r.svc.RequestSend(ctx, sendflow.RequestIn{Account: r.acct, FromAddress: r.acct.Address, DraftIDs: []string{r.draft("a@example.com", "s", "b")}})
	if e := r.refusal(err); e.Code != sendflow.CodeSendDisabled {
		t.Fatalf("err = %v", e)
	}
	if r.ap.Creates != 0 {
		t.Error("no approval may be created while send is disabled")
	}
}

func TestSendEnabledTurnedOffAfterApprovalBlocksDispatch(t *testing.T) {
	r := newRig(t, true)
	id := r.approved(r.draft("a@example.com", "s", "b"))
	fresh, _ := r.st.GetAccount(ctx, "pat", r.acct.ID)
	fresh.SendEnabled = false
	if err := r.st.UpdateAccount(ctx, fresh); err != nil {
		t.Fatal(err)
	}
	out := r.status(id)
	if out.Status != sendflow.StatusBlocked || len(r.prov.SentMessages()) != 0 {
		t.Fatalf("status = %+v, sent %d", out, len(r.prov.SentMessages()))
	}
	if _, err := r.st.GetSend(ctx, id); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("a blocked dispatch must not claim: %v", err)
	}
	// Turning it back on lets the same approval go through.
	fresh.SendEnabled = true
	_ = r.st.UpdateAccount(ctx, fresh)
	if out := r.status(id); out.Status != store.SendSent {
		t.Fatalf("after re-enable: %+v", out)
	}
}

func TestAccountNotOKAtDispatchBlocks(t *testing.T) {
	r := newRig(t, true)
	id := r.approved(r.draft("a@example.com", "s", "b"))
	fresh, _ := r.st.GetAccount(ctx, "pat", r.acct.ID)
	fresh.Status = store.StatusNeedsReauth
	_ = r.st.UpdateAccount(ctx, fresh)
	if out := r.status(id); out.Status != sendflow.StatusBlocked || !strings.Contains(out.Note, "needs reauth") {
		t.Fatalf("status = %+v", out)
	}
	if len(r.prov.SentMessages()) != 0 {
		t.Fatal("sent from a needs_reauth account")
	}
}

func TestKillSwitchFlippedAfterApprovalBlocksDispatch(t *testing.T) {
	r := newRig(t, true)
	id := r.approved(r.draft("a@example.com", "s", "b"))
	if err := r.st.SetKillSwitch(ctx, true, "testing", r.clk.Now()); err != nil {
		t.Fatal(err)
	}
	out := r.status(id)
	if out.Status != sendflow.StatusBlocked || !strings.Contains(out.Note, "kill switch") {
		t.Fatalf("status = %+v", out)
	}
	if _, err := r.st.GetSend(ctx, id); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("a killed dispatch must not claim: %v", err)
	}
	if r.ap.ConsumeCalls != 0 || len(r.prov.SentMessages()) != 0 {
		t.Errorf("consume calls %d, sent %d", r.ap.ConsumeCalls, len(r.prov.SentMessages()))
	}
	// New requests are refused while it is on.
	_, err := r.svc.RequestSend(ctx, sendflow.RequestIn{Account: r.acct, FromAddress: r.acct.Address, DraftIDs: []string{r.draft("b@example.com", "s", "b")}})
	if e := r.refusal(err); e.Code != sendflow.CodeKillSwitch {
		t.Errorf("request while killed: %v", e)
	}
	_ = r.st.SetKillSwitch(ctx, false, "", r.clk.Now())
	if out := r.status(id); out.Status != store.SendSent {
		t.Fatalf("after the switch is off: %+v", out)
	}
}

// ---- receipts ----

func TestReceiptSubstitutionIsRejected(t *testing.T) {
	r := newRig(t, true)
	a := r.request(r.draft("a@example.com", "one", "b"))
	b := r.request(r.draft("b@example.com", "two", "b"))
	r.ap.Approve(a.RequestID)
	r.ap.Approve(b.RequestID)
	_, bSHA, _ := r.ap.Request(b.RequestID)
	now := r.clk.Now()
	for name, rc := range map[string]func() receipt.Receipt{
		"another request_id": func() receipt.Receipt { return r.ap.Receipt(a.RequestID) },
		"another payload": func() receipt.Receipt {
			return r.ap.SignFor(b.RequestID, api.KindEmailSend, strings.Repeat("0", 64), now, now.Add(15*time.Minute))
		},
		"another kind": func() receipt.Receipt {
			return r.ap.SignFor(b.RequestID, api.KindAccountSendEnable, bSHA, now, now.Add(15*time.Minute))
		},
		"expired": func() receipt.Receipt {
			return r.ap.SignFor(b.RequestID, api.KindEmailSend, bSHA, now.Add(-40*time.Minute), now.Add(-25*time.Minute))
		},
	} {
		r.ap.Tamper(b.RequestID, rc())
		out := r.status(b.RequestID)
		if out.Status != sendflow.StatusReceiptInvalid {
			t.Errorf("%s: status = %+v, want receipt_invalid", name, out)
		}
		if _, err := r.st.GetSend(ctx, b.RequestID); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("%s: a bad receipt must not claim: %v", name, err)
		}
	}
	if len(r.prov.SentMessages()) != 0 || r.ap.ConsumeCalls != 0 {
		t.Fatalf("sent %d, consumed %d", len(r.prov.SentMessages()), r.ap.ConsumeCalls)
	}
	// The genuine receipt still works.
	r.ap.Tamper(b.RequestID, r.ap.SignFor(b.RequestID, api.KindEmailSend, bSHA, now, now.Add(15*time.Minute)))
	if out := r.status(b.RequestID); out.Status != store.SendSent {
		t.Fatalf("genuine receipt: %+v", out)
	}
}

func TestAStoredPayloadThatWasEditedIsNotTrusted(t *testing.T) {
	r := newRig(t, true)
	id := r.approved(r.draft("a@example.com", "s", "b"))
	// The stored request no longer hashes to what was approved.
	ap := r.approval(id)
	forged := bytes.Replace(ap.Payload, []byte("a@example.com"), []byte("z@example.com"), 1)
	if bytes.Equal(forged, ap.Payload) {
		t.Fatal("test setup: payload did not change")
	}
	got, _, err := r.st.CreateApproval(ctx, store.Approval{ID: "forged", Owner: "pat", Kind: store.KindEmailSend, AccountID: ap.AccountID, IdempotencyKey: "k-forged",
		PayloadSHA256: ap.PayloadSHA256, Payload: forged, DraftID: ap.DraftID, RFC822SHA256: ap.RFC822SHA256, Snapshot: ap.Snapshot, Status: store.ApprovalPending,
		Created: r.clk.Now(), Expires: ap.Expires}, r.clk.Now())
	if err != nil || got.ID != "forged" {
		t.Fatal(err)
	}
	if out := r.status("forged"); out.Status == store.SendSent {
		t.Fatalf("a request whose payload does not match its hash was sent: %+v", out)
	}
}

// ---- idempotency, replay, concurrency ----

func TestReplayDoesNotResend(t *testing.T) {
	r := newRig(t, true)
	id := r.approved(r.draft("a@example.com", "s", "b"))
	first := r.status(id)
	consumes := r.ap.ConsumeCalls
	second := r.status(id)
	if first.Status != store.SendSent || second.Status != store.SendSent || second.ProviderMessageID != first.ProviderMessageID {
		t.Fatalf("first %+v second %+v", first, second)
	}
	if len(r.prov.SentMessages()) != 1 || r.ap.ConsumeCalls != consumes {
		t.Fatalf("sent %d; consume calls %d -> %d (a terminal ledger row must be returned without calling the approver)", len(r.prov.SentMessages()), consumes, r.ap.ConsumeCalls)
	}
}

func TestRequestingTheSameDraftAgainReturnsTheSameRequest(t *testing.T) {
	r := newRig(t, true)
	id := r.draft("a@example.com", "s", "b")
	a, b := r.request(id), r.request(id)
	if a.RequestID != b.RequestID || r.ap.Creates != 1 {
		t.Fatalf("%s vs %s, creates %d", a.RequestID, b.RequestID, r.ap.Creates)
	}
	// After it is sent the draft is discarded, so asking again finds no draft
	// and makes no second send.
	r.ap.Approve(a.RequestID)
	r.status(a.RequestID)
	_, err := r.svc.RequestSend(ctx, sendflow.RequestIn{Account: r.acct, FromAddress: r.acct.Address, DraftIDs: []string{id}})
	if err == nil || !strings.Contains(err.Error(), "draft_not_found") {
		t.Fatalf("after sent: err = %v, want draft_not_found", err)
	}
	if r.status(a.RequestID); len(r.prov.SentMessages()) != 1 {
		t.Fatal("sent twice")
	}
}

func TestADeniedOrExpiredRequestCanBeRequestedAgain(t *testing.T) {
	r := newRig(t, true)
	id := r.draft("a@example.com", "s", "b")
	first := r.request(id)
	r.ap.Deny(first.RequestID)
	second := r.request(id)
	if second.RequestID == first.RequestID || second.Status != "pending" {
		t.Fatalf("after deny: %+v", second)
	}
	r.clk.Advance(20 * time.Minute) // expires
	third := r.request(id)
	if third.RequestID == second.RequestID || third.Status != "pending" {
		t.Fatalf("after expiry: %+v", third)
	}
}

func TestTenConcurrentStatusCallsProduceExactlyOneSend(t *testing.T) {
	r := newRig(t, true)
	id := r.approved(r.draft("a@example.com", "s", "b"))
	var wg sync.WaitGroup
	statuses := make(chan string, 10)
	for range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out, err := r.svc.Status(ctx, "pat", id)
			if err != nil {
				statuses <- "error: " + err.Error()
				return
			}
			statuses <- out.Status
		}()
	}
	wg.Wait()
	close(statuses)
	for s := range statuses {
		if strings.HasPrefix(s, "error") {
			t.Errorf("a concurrent call errored: %s", s)
		}
	}
	if n := len(r.prov.SentMessages()); n != 1 {
		t.Fatalf("%d messages sent, want exactly 1", n)
	}
	if out := r.status(id); out.Status != store.SendSent {
		t.Fatalf("final status = %+v", out)
	}
	if n, _ := r.st.HourlyCount(ctx, r.acct.ID, r.clk.Now()); n != 1 {
		t.Errorf("hourly count = %d, want 1", n)
	}
}

func TestConcurrentSendsAtCapMinusOneProduceExactlyOneClaim(t *testing.T) {
	r := newRig(t, true)
	r.clk.t = time.Date(2026, 11, 25, 12, 50, 0, 0, time.UTC) // the hour turns inside the approvals' 15 minutes
	for range r.acct.HourlyCap - 1 {
		if err := r.st.ReserveCap(ctx, r.acct.ID, r.clk.Now(), r.acct.HourlyCap); err != nil {
			t.Fatal(err)
		}
	}
	ids := []string{r.approved(r.draft("a@example.com", "one", "b")), r.approved(r.draft("b@example.com", "two", "b"))}
	var wg sync.WaitGroup
	var rateLimited, sent atomic.Int32
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
				switch out.Status {
				case sendflow.StatusRateLimited:
					rateLimited.Add(1)
				case store.SendSent:
					sent.Add(1)
				}
			}()
		}
	}
	wg.Wait()
	if n := len(r.prov.SentMessages()); n != 1 {
		t.Fatalf("%d sent, want exactly one claim to win the last slot", n)
	}
	if rateLimited.Load() == 0 {
		t.Error("the losing request must report rate_limited")
	}
	if n, _ := r.st.HourlyCount(ctx, r.acct.ID, r.clk.Now()); n != r.acct.HourlyCap {
		t.Errorf("hourly count = %d, want the cap %d", n, r.acct.HourlyCap)
	}
	// Next hour the loser goes through.
	r.clk.Advance(11 * time.Minute)
	for _, id := range ids {
		r.status(id)
	}
	if n := len(r.prov.SentMessages()); n != 2 {
		t.Errorf("after the hour turned: %d sent, want 2", n)
	}
}

// ---- crash recovery ----

func TestCrashAfterClaimedResumesAtConsume(t *testing.T) {
	r := newRig(t, true)
	id := r.approved(r.draft("a@example.com", "s", "b"))
	// The earlier process claimed the row and died before consuming.
	if err := r.st.ClaimSend(ctx, store.SendRow{RequestID: id, Owner: "pat", AccountID: r.acct.ID, Created: r.clk.Now()}, r.acct.HourlyCap); err != nil {
		t.Fatal(err)
	}
	if r.ap.ConsumeCalls != 0 {
		t.Fatal("test setup")
	}
	out := r.status(id)
	if out.Status != store.SendSent || len(r.prov.SentMessages()) != 1 {
		t.Fatalf("status %+v, sent %d", out, len(r.prov.SentMessages()))
	}
	if r.ap.ConsumeCalls == 0 || r.ap.Claimant(id) != id {
		t.Errorf("the resumed call must consume as the request's own claimant (claimant %q)", r.ap.Claimant(id))
	}
	if n, _ := r.st.HourlyCount(ctx, r.acct.ID, r.clk.Now()); n != 1 {
		t.Errorf("the resumed claim must not take a second cap slot: %d", n)
	}
}

func TestCrashInDispatchingBecomesUnknownAndNeverResends(t *testing.T) {
	r := newRig(t, true)
	id := r.approved(r.draft("a@example.com", "s", "b"))
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(r.st.ClaimSend(ctx, store.SendRow{RequestID: id, Owner: "pat", AccountID: r.acct.ID, Created: r.clk.Now()}, r.acct.HourlyCap))
	_, err := r.st.AdvanceSend(ctx, id, store.SendClaimed, store.SendDispatching, "", r.clk.Now())
	must(err)

	// A call that is still in flight is left alone.
	if out := r.status(id); out.Status != sendflow.StatusDispatching {
		t.Fatalf("fresh dispatching row: %+v", out)
	}
	// Once it has been stale for long enough, it is judged crashed.
	r.clk.Advance(sendflow.DefaultStaleAfter + time.Second)
	out := r.status(id)
	if out.Status != store.SendUnknown {
		t.Fatalf("stale dispatching row: %+v", out)
	}
	for range 3 {
		if out := r.status(id); out.Status != store.SendUnknown {
			t.Fatalf("unknown must stay unknown: %+v", out)
		}
	}
	if len(r.prov.SentMessages()) != 0 {
		t.Fatal("a crashed dispatch was re-sent")
	}
}

func TestProviderAcceptedButNotRecordedBecomesUnknown(t *testing.T) {
	r := newRig(t, true)
	id := r.approved(r.draft("a@example.com", "s", "b"))
	// The ledger write that records `sent` fails, as a crash right after the
	// provider accepted the message would.
	fs := &failingAdvance{Store: r.st, to: store.SendSent}
	r.svc.Store = fs
	out, err := r.svc.Status(ctx, "pat", id)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.prov.SentMessages()) != 1 || out.Status != sendflow.StatusDispatching {
		t.Fatalf("sent %d, status %+v", len(r.prov.SentMessages()), out)
	}
	r.svc.Store = r.st // the process restarts with a healthy store
	r.clk.Advance(sendflow.DefaultStaleAfter + time.Second)
	if out := r.status(id); out.Status != store.SendUnknown {
		t.Fatalf("after the crash window: %+v", out)
	}
	if len(r.prov.SentMessages()) != 1 {
		t.Fatalf("%d sent, the message must never go out twice", len(r.prov.SentMessages()))
	}
}

type failingAdvance struct {
	store.Store
	to string
}

func (f *failingAdvance) AdvanceSend(ctx context.Context, id, from, to, msg string, now time.Time) (store.SendRow, error) {
	if to == f.to {
		return store.SendRow{}, errors.New("disk on fire")
	}
	return f.Store.AdvanceSend(ctx, id, from, to, msg, now)
}

// ---- consume claimant ----

func TestConsumeIsIdempotentForTheSameClaimantAndConflictsForAnother(t *testing.T) {
	r := newRig(t, true)
	it := r.request(r.draft("a@example.com", "s", "b"))
	r.ap.Approve(it.RequestID)
	if _, err := r.ap.Consume(ctx, it.RequestID, "claimant-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.ap.Consume(ctx, it.RequestID, "claimant-1"); err != nil {
		t.Errorf("the same claimant again: %v, want success", err)
	}
	if _, err := r.ap.Consume(ctx, it.RequestID, "claimant-2"); !errors.Is(err, api.ErrAlreadyConsumed) {
		t.Errorf("another claimant: %v, want ErrAlreadyConsumed", err)
	}
}

func TestAnApprovalConsumedByAnotherClaimantFailsTheSend(t *testing.T) {
	r := newRig(t, true)
	it := r.request(r.draft("a@example.com", "s", "b"))
	r.ap.Approve(it.RequestID)
	if _, err := r.ap.Consume(ctx, it.RequestID, "someone-else"); err != nil {
		t.Fatal(err)
	}
	// The approver already shows it consumed, so nothing is claimed.
	if out := r.status(it.RequestID); out.Status != sendflow.StatusAlreadyClaimed {
		t.Fatalf("status = %+v", out)
	}
	if len(r.prov.SentMessages()) != 0 {
		t.Fatal("sent an approval another claimant consumed")
	}
}

// ---- outcomes, snapshot retention ----

func TestOutcomesAndSnapshotRetention(t *testing.T) {
	snapshotGone := func(r *rig, id string) bool { return len(r.approval(id).Snapshot) == 0 }

	t.Run("sent", func(t *testing.T) {
		r := newRig(t, true)
		d := r.draft("a@example.com", "s", "b")
		id := r.approved(d)
		out := r.status(id)
		if out.Status != store.SendSent || out.ProviderMessageID == "" || out.SourceDraftRetained || out.SourceDraftID != "" {
			t.Fatalf("%+v", out)
		}
		if !snapshotGone(r, id) {
			t.Error("the snapshot must be deleted after sent")
		}
		if _, err := r.prov.GetDraftRaw(ctx, d); !errors.Is(err, provider.ErrNotFound) {
			t.Errorf("the unchanged source draft must be discarded after sent: %v", err)
		}
		if row, _ := r.st.GetSend(ctx, id); row.ProviderMessageID != out.ProviderMessageID {
			t.Errorf("the provider message id must be persisted with sent: %+v", row)
		}
	})
	t.Run("would_send", func(t *testing.T) {
		r := newRig(t, false)
		id := r.approved(r.draft("a@example.com", "s", "b"))
		out := r.status(id)
		if out.Status != store.SendWouldSend || !out.DryRun || len(r.prov.SentMessages()) != 0 {
			t.Fatalf("%+v sent %d", out, len(r.prov.SentMessages()))
		}
		if !snapshotGone(r, id) {
			t.Error("the snapshot must be deleted after would_send")
		}
		if _, err := r.st.GetContact(ctx, r.acct.ID, "a@example.com"); !errors.Is(err, store.ErrNotFound) {
			t.Error("a dry run must not enroll contacts")
		}
		if n, _ := r.st.HourlyCount(ctx, r.acct.ID, r.clk.Now()); n != 1 {
			t.Errorf("would_send keeps its cap slot: %d", n)
		}
	})
	t.Run("failed releases the cap slot", func(t *testing.T) {
		r := newRig(t, true)
		r.prov.SendErr = fmt.Errorf("%w: HTTP 400", provider.ErrRejected)
		id := r.approved(r.draft("a@example.com", "s", "b"))
		out := r.status(id)
		if out.Status != store.SendFailed || len(r.prov.SentMessages()) != 0 {
			t.Fatalf("%+v", out)
		}
		if !snapshotGone(r, id) {
			t.Error("the snapshot must be deleted after failed")
		}
		if n, _ := r.st.HourlyCount(ctx, r.acct.ID, r.clk.Now()); n != 0 {
			t.Errorf("a failed send must release its cap slot: %d", n)
		}
		// A failed send is final: calling again neither retries nor claims again.
		r.prov.SendErr = nil
		if again := r.status(id); again.Status != store.SendFailed || len(r.prov.SentMessages()) != 0 {
			t.Fatalf("retried a failed send: %+v", again)
		}
	})
	t.Run("unknown is never retried", func(t *testing.T) {
		r := newRig(t, true)
		r.prov.SendErr = errors.New("context deadline exceeded")
		id := r.approved(r.draft("a@example.com", "s", "b"))
		out := r.status(id)
		if out.Status != store.SendUnknown || !strings.Contains(out.Note, "Sent folder") {
			t.Fatalf("%+v", out)
		}
		r.prov.SendErr = nil
		if again := r.status(id); again.Status != store.SendUnknown || len(r.prov.SentMessages()) != 0 {
			t.Fatalf("retried an unknown send: %+v", again)
		}
		if n, _ := r.st.HourlyCount(ctx, r.acct.ID, r.clk.Now()); n != 1 {
			t.Errorf("an unknown send keeps its cap slot: %d", n)
		}
		if snapshotGone(r, id) {
			t.Error("an unknown request keeps its snapshot until the 24h backstop")
		}
		if ap := r.approval(id); ap.TTL.IsZero() {
			t.Error("the backstop ttl must be set")
		}
	})
	t.Run("denied", func(t *testing.T) {
		r := newRig(t, true)
		it := r.request(r.draft("a@example.com", "s", "b"))
		r.ap.Deny(it.RequestID)
		if out := r.status(it.RequestID); out.Status != sendflow.StatusDenied || !snapshotGone(r, it.RequestID) {
			t.Fatalf("%+v snapshot gone %v", out, snapshotGone(r, it.RequestID))
		}
	})
	t.Run("expired", func(t *testing.T) {
		r := newRig(t, true)
		it := r.request(r.draft("a@example.com", "s", "b"))
		r.clk.Advance(16 * time.Minute)
		if out := r.status(it.RequestID); out.Status != sendflow.StatusExpired || !snapshotGone(r, it.RequestID) {
			t.Fatalf("%+v snapshot gone %v", out, snapshotGone(r, it.RequestID))
		}
	})
	t.Run("pending keeps the snapshot", func(t *testing.T) {
		r := newRig(t, true)
		it := r.request(r.draft("a@example.com", "s", "b"))
		out := r.status(it.RequestID)
		if out.Status != sendflow.StatusPending || out.ApprovalURL == "" || snapshotGone(r, it.RequestID) {
			t.Fatalf("%+v", out)
		}
	})
}

func TestApproverDownReturnsPendingAndClaimsNothing(t *testing.T) {
	r := newRig(t, true)
	id := r.approved(r.draft("a@example.com", "s", "b"))
	r.ap.Down = true
	out := r.status(id)
	if out.Status != sendflow.StatusPending || !strings.Contains(out.Note, "could not be reached") {
		t.Fatalf("%+v", out)
	}
	if _, err := r.st.GetSend(ctx, id); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("nothing may be claimed while the approver is down: %v", err)
	}
	r.ap.Down = false
	if out := r.status(id); out.Status != store.SendSent {
		t.Fatalf("%+v", out)
	}
}

func TestStatusOfAnotherOwnersRequestIsNotFound(t *testing.T) {
	r := newRig(t, true)
	id := r.approved(r.draft("a@example.com", "s", "b"))
	_, err := r.svc.Status(ctx, "mallory", id)
	if e := r.refusal(err); e.Code != sendflow.CodeNotFound {
		t.Fatalf("%v", e)
	}
	if _, err := r.svc.Status(ctx, "pat", "nope"); err == nil {
		t.Fatal("unknown id must be not_found")
	}
}

// ---- batches (R8) ----

func (r *rig) batch(draftIDs ...string) sendflow.RequestOut {
	r.t.Helper()
	out, err := r.svc.RequestSend(ctx, sendflow.RequestIn{Account: r.acct, FromAddress: r.acct.Address, DraftIDs: draftIDs})
	if err != nil {
		r.t.Fatal(err)
	}
	return out
}

func TestBatchOfOneAccountHasOneApprovalAndEachItemIsDispatchedIndividually(t *testing.T) {
	r := newRig(t, true)
	out := r.batch(r.draft("a@example.com", "one", "b"), r.draft("b@example.com", "two", "b"), r.draft("c@example.com", "three", "b"))
	if !out.Batch || len(out.Items) != 3 || out.ApprovalURL == "" || out.Items[0].ApprovalURL == out.ApprovalURL {
		t.Fatalf("batch = %+v", out)
	}
	kind, _, _ := r.ap.Request(out.RequestID)
	if kind != api.KindEmailSendBatch {
		t.Fatalf("the batch request kind = %s", kind)
	}
	if st := r.status(out.RequestID); st.Status != sendflow.StatusPending {
		t.Fatalf("before approval: %+v", st)
	}
	r.ap.ApproveBatch(out.RequestID)
	st := r.status(out.RequestID)
	if st.Status != "done" || len(st.Items) != 3 {
		t.Fatalf("after approval: %+v", st)
	}
	for _, it := range st.Items {
		if it.Status != store.SendSent {
			t.Errorf("item %+v", it)
		}
		if _, err := r.st.GetSend(ctx, it.RequestID); err != nil {
			t.Errorf("each item needs its own ledger row: %v", err)
		}
	}
	if n := len(r.prov.SentMessages()); n != 3 {
		t.Fatalf("%d sent", n)
	}
	if n, _ := r.st.HourlyCount(ctx, r.acct.ID, r.clk.Now()); n != 3 {
		t.Errorf("each item takes its own cap slot: %d", n)
	}
	// An item can also be read on its own afterwards.
	if again := r.status(out.Items[0].RequestID); again.Status != store.SendSent {
		t.Errorf("%+v", again)
	}
}

func TestDroppingAnItemExcludesItFromTheReceiptAndTheSend(t *testing.T) {
	r := newRig(t, true)
	out := r.batch(r.draft("a@example.com", "one", "b"), r.draft("b@example.com", "two", "b"))
	dropped := out.Items[1].RequestID
	rc := r.ap.ApproveBatch(out.RequestID, dropped)
	_, droppedSHA, _ := r.ap.Request(dropped)

	st := r.status(out.RequestID)
	if st.Status != "done" {
		t.Fatalf("%+v", st)
	}
	byID := map[string]string{}
	for _, it := range st.Items {
		byID[it.RequestID] = it.Status
	}
	if byID[out.Items[0].RequestID] != store.SendSent || byID[dropped] != sendflow.StatusDenied {
		t.Fatalf("items = %v", byID)
	}
	if n := len(r.prov.SentMessages()); n != 1 {
		t.Fatalf("%d sent, the dropped message must not go out", n)
	}
	// The receipt covers the kept list only.
	_, batchSHA, _ := r.ap.Request(out.RequestID)
	if rc.Body.PayloadSHA256 != batchSHA {
		t.Error("the receipt must cover the rewritten batch payload")
	}
	// Even the dropped item's own status cannot be turned into a send.
	if got := r.status(dropped); got.Status != sendflow.StatusDenied || droppedSHA == "" {
		t.Fatalf("%+v", got)
	}
}

func TestBatchItemsAreClaimedIndividuallyAndTheCapAppliesAcrossTheBatch(t *testing.T) {
	r := newRig(t, true)
	r.clk.t = time.Date(2026, 11, 25, 12, 50, 0, 0, time.UTC)
	r.acct.HourlyCap = 2
	if err := r.st.UpdateAccount(ctx, r.acct); err != nil {
		t.Fatal(err)
	}
	out := r.batch(r.draft("a@example.com", "one", "b"), r.draft("b@example.com", "two", "b"), r.draft("c@example.com", "three", "b"))
	r.ap.ApproveBatch(out.RequestID)
	st := r.status(out.RequestID)
	got := map[string]int{}
	for _, it := range st.Items {
		got[it.Status]++
	}
	if got[store.SendSent] != 2 || got[sendflow.StatusRateLimited] != 1 {
		t.Fatalf("items = %v, want 2 sent and 1 rate_limited", got)
	}
	if st.Status == "done" {
		t.Error("a batch with a rate limited item is not done")
	}
	r.clk.Advance(11 * time.Minute)
	if st := r.status(out.RequestID); st.Status != "done" {
		t.Fatalf("next hour: %+v", st)
	}
	if n := len(r.prov.SentMessages()); n != 3 {
		t.Fatalf("%d sent", n)
	}
}

func TestBatchReceiptSubstitutionIsRejected(t *testing.T) {
	r := newRig(t, true)
	out := r.batch(r.draft("a@example.com", "one", "b"), r.draft("b@example.com", "two", "b"))
	r.ap.ApproveBatch(out.RequestID)
	// A receipt for some other request replaces the batch's.
	other := r.request(r.draft("z@example.com", "other", "b"))
	r.ap.Approve(other.RequestID)
	r.ap.Tamper(out.RequestID, r.ap.Receipt(other.RequestID))
	st := r.status(out.RequestID)
	for _, it := range st.Items {
		if it.Status != sendflow.StatusReceiptInvalid {
			t.Errorf("item %+v", it)
		}
	}
	if len(r.prov.SentMessages()) != 0 {
		t.Fatal("sent on a substituted batch receipt")
	}
}

func TestACrossAccountBatchIsRefusedByTheApprover(t *testing.T) {
	r := newRig(t, true)
	other, otherProv := r.addAccount("acc_two", "two@example.com")
	a := r.request(r.draft("a@example.com", "one", "b"))
	// Request a send from the second account, then try to batch both items under account one.
	raw, _ := mailmsg.Build(mailmsg.Message{From: other.Address, To: []string{"b@example.com"}, Subject: "x", Body: "b"}, r.clk.Now())
	d, _ := otherProv.CreateDraft(ctx, provider.DraftInput{Raw: raw})
	out, err := r.svc.RequestSend(ctx, sendflow.RequestIn{Account: other, FromAddress: other.Address, DraftIDs: []string{d.ID}})
	if err != nil {
		t.Fatal(err)
	}
	bp := api.BatchPayload{Owner: "pat", AccountID: r.acct.ID, FromAddress: r.acct.Address, Items: []api.BatchItem{
		{RequestID: a.RequestID, PayloadSHA256: r.approval(a.RequestID).PayloadSHA256},
		{RequestID: out.RequestID, PayloadSHA256: r.approval(out.RequestID).PayloadSHA256},
	}}
	raw2, _ := json.Marshal(bp)
	if _, _, err := r.ap.Create(ctx, api.CreateBody{IdempotencyKey: "x", Kind: api.KindEmailSendBatch, Payload: raw2, Preview: []byte(`{}`)}); err == nil || !strings.Contains(err.Error(), "cross_account_batch") {
		t.Fatalf("err = %v, want cross_account_batch", err)
	}
}
