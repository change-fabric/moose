package approver_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/change-fabric/moose/internal/approver/api"
	"github.com/change-fabric/moose/internal/approver/receipt"
	"github.com/change-fabric/moose/internal/approver/store"
	"github.com/change-fabric/moose/internal/approver/webauthn"
	"github.com/go-webauthn/webauthn/protocol"
)

// batchOf creates n pending email-send requests for a and a batch over them.
func (h *harness) batchOf(a acct, drafts ...string) (batch api.CreateResponse, items []api.CreateResponse, payload api.BatchPayload) {
	h.t.Helper()
	payload = api.BatchPayload{Owner: owner, AccountID: a.ID, FromAddress: a.Addr}
	for _, d := range drafts {
		c := h.create(sendBody(a, sendOpts{Draft: d, Body: "Body of " + d + "\n", To: []string{d + "@elsewhere.example"}}))
		items = append(items, c)
		payload.Items = append(payload.Items, api.BatchItem{RequestID: c.RequestID, PayloadSHA256: c.PayloadSHA256})
	}
	raw, _ := json.Marshal(payload)
	on := true
	pv, _ := json.Marshal(api.Preview{AccountID: a.ID, Nickname: a.Nick, FromAddress: a.Addr, DisplayName: a.Display, ColorIndex: a.Color, AccountStatus: "ok", SendEnabled: &on})
	batch = h.create(api.CreateBody{IdempotencyKey: "batch-" + strings.Join(drafts, "-"), Kind: api.KindEmailSendBatch, Payload: raw, Preview: pv})
	return
}

func TestBatchRejectsAnotherAccountsItems(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	mine := h.create(sendBody(work, sendOpts{Draft: "mine"}))
	theirs := h.create(sendBody(personal, sendOpts{Draft: "theirs"}))
	payload, _ := json.Marshal(api.BatchPayload{Owner: owner, AccountID: work.ID, FromAddress: work.Addr, Items: []api.BatchItem{
		{RequestID: mine.RequestID, PayloadSHA256: mine.PayloadSHA256},
		{RequestID: theirs.RequestID, PayloadSHA256: theirs.PayloadSHA256},
	}})
	on := true
	pv, _ := json.Marshal(api.Preview{AccountID: work.ID, Nickname: work.Nick, FromAddress: work.Addr, AccountStatus: "ok", SendEnabled: &on})
	_, _, err := h.client.Create(ctx, api.CreateBody{IdempotencyKey: "cross", Kind: api.KindEmailSendBatch, Payload: payload, Preview: pv})
	if code, ec := apiCode(err); code != http.StatusUnprocessableEntity || ec != "cross_account_batch" {
		t.Fatalf("a cross-account batch: %v, want 422 cross_account_batch", err)
	}
	// Other malformed batches.
	for name, items := range map[string][]api.BatchItem{
		"unknown item":    {{RequestID: "apr_nope", PayloadSHA256: strings.Repeat("a", 64)}},
		"wrong hash":      {{RequestID: mine.RequestID, PayloadSHA256: strings.Repeat("b", 64)}},
		"duplicate items": {{RequestID: mine.RequestID, PayloadSHA256: mine.PayloadSHA256}, {RequestID: mine.RequestID, PayloadSHA256: mine.PayloadSHA256}},
		"no items":        {},
	} {
		payload, _ := json.Marshal(api.BatchPayload{Owner: owner, AccountID: work.ID, FromAddress: work.Addr, Items: items})
		if _, _, err := h.client.Create(ctx, api.CreateBody{IdempotencyKey: "bad-" + name, Kind: api.KindEmailSendBatch, Payload: payload, Preview: pv}); err == nil {
			t.Errorf("%s: the batch was accepted", name)
		}
	}
	// A non-email-send request cannot ride in a batch.
	en := h.create(enableBody(work, "en"))
	payload, _ = json.Marshal(api.BatchPayload{Owner: owner, AccountID: work.ID, FromAddress: work.Addr, Items: []api.BatchItem{{RequestID: en.RequestID, PayloadSHA256: en.PayloadSHA256}}})
	if _, _, err := h.client.Create(ctx, api.CreateBody{IdempotencyKey: "enable-in-batch", Kind: api.KindEmailSendBatch, Payload: payload, Preview: pv}); err == nil {
		t.Error("an account-send-enable request was accepted as a batch item")
	}
}

func TestBatchPageShowsEveryMessageInFullWithOneBanner(t *testing.T) {
	h := newHarness(t)
	batch, _, _ := h.batchOf(work, "one", "two", "three")
	page := h.get("/approve/r/" + batch.RequestID)
	for _, want := range []string{"Approve 3 emails from one account", "Body of one", "Body of two", "Body of three", "one@elsewhere.example", "three@elsewhere.example"} {
		if !strings.Contains(page.Body, want) {
			t.Errorf("batch page lacks %q", want)
		}
	}
	if n := strings.Count(page.Body, `role="region"`); n != 1 {
		t.Errorf("the batch page has %d banners, want 1", n)
	}
	if n := strings.Count(page.Body, `type="checkbox"`); n != 3 {
		t.Errorf("%d include checkboxes, want one per message", n)
	}
	if n := strings.Count(page.Body, `id="confirm-from"`); n != 1 {
		t.Errorf("%d confirm buttons, want 1", n)
	}
}

func TestDroppingAnItemExcludesItFromTheReceipt(t *testing.T) {
	h := newHarness(t)
	h.enroll()
	ctx := context.Background()
	batch, items, payload := h.batchOf(work, "one", "two", "three")
	dropped := items[1].RequestID

	if r := h.tap(batch.RequestID); r.Code != http.StatusOK {
		t.Fatalf("tap: %d %s", r.Code, r.Body)
	}
	if r := h.decide(batch.RequestID, "approve", dropped); r.Code != http.StatusOK {
		t.Fatalf("approve with a drop: %d %s", r.Code, r.Body)
	}

	// The receipt covers the two kept items and nothing else.
	final := payload
	final.Items = []api.BatchItem{payload.Items[0], payload.Items[2]}
	finalRaw, _ := json.Marshal(final)
	finalSHA := canonSHA(t, finalRaw)
	st, err := h.client.Status(ctx, batch.RequestID)
	if err != nil || st.Status != "approved" || st.Receipt == nil {
		t.Fatalf("batch status: %+v, %v", st, err)
	}
	if st.PayloadSHA256 != finalSHA || st.Receipt.Body.PayloadSHA256 != finalSHA {
		t.Fatalf("the receipt covers %s, want the 2-item list %s", st.Receipt.Body.PayloadSHA256, finalSHA)
	}
	if err := receipt.Verify(h.pub, *st.Receipt, h.want(batch.RequestID, api.KindEmailSendBatch, finalSHA)); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(st.Payload), dropped) {
		t.Fatalf("the approved payload still lists the dropped item: %s", st.Payload)
	}
	// It does not verify against the original three-item list.
	if err := receipt.Verify(h.pub, *st.Receipt, h.want(batch.RequestID, api.KindEmailSendBatch, batch.PayloadSHA256)); err == nil {
		t.Fatal("the receipt verified against the original list that included the dropped item")
	}

	// The dropped item is denied, the kept ones are approved and carry the batch receipt.
	drop, _ := h.client.Status(ctx, dropped)
	if drop.Status != "denied" || drop.Receipt != nil {
		t.Fatalf("dropped item: %+v, want denied with no receipt", drop)
	}
	for _, kept := range []string{items[0].RequestID, items[2].RequestID} {
		s, _ := h.client.Status(ctx, kept)
		if s.Status != "approved" || s.BatchID != batch.RequestID || s.Receipt == nil || s.Receipt.Body.RequestID != batch.RequestID {
			t.Fatalf("kept item %s: %+v", kept, s)
		}
	}
	// Each kept item is consumed by itself, and a dropped one cannot be.
	if _, err := h.client.Consume(ctx, items[0].RequestID, "claim-1"); err != nil {
		t.Fatal(err)
	}
	if s, _ := h.client.Status(ctx, items[2].RequestID); s.Status != "approved" {
		t.Fatalf("consuming item 1 changed item 3 to %s", s.Status)
	}
	if _, err := h.client.Consume(ctx, dropped, "claim-2"); err == nil {
		t.Fatal("a dropped item was consumed")
	}
	if ok, _ := h.st.HasPreview(ctx, dropped); ok {
		t.Fatal("the dropped item's preview survived")
	}
}

func TestBatchAssertionIsBoundToTheFinalList(t *testing.T) {
	h := newHarness(t)
	h.enroll()
	ctx := context.Background()
	batch, items, _ := h.batchOf(work, "one", "two")
	h.tap(batch.RequestID)

	// A challenge bound to the original three-item digest cannot approve a batch
	// whose drop list changes the final list, and one bound to the wrong digest
	// cannot approve at all.
	for name, bind := range map[string]webauthn.Binding{
		"another digest":          {Purpose: webauthn.PurposeApprove, RequestID: batch.RequestID, PayloadSHA256: strings.Repeat("0", 64)},
		"drop without its digest": {Purpose: webauthn.PurposeApprove, RequestID: batch.RequestID, PayloadSHA256: batch.PayloadSHA256, Drops: []string{items[1].RequestID}},
	} {
		ca, err := h.wa.BeginAssertion(ctx, owner, bind)
		if err != nil {
			t.Fatal(err)
		}
		if r := h.post("/approve/r/"+batch.RequestID+"/finish", h.auth.Assert(ca)); r.Code != http.StatusForbidden {
			t.Errorf("%s: %d %s, want 403", name, r.Code, r.Body)
		}
	}
	if b, _ := h.st.GetRequest(ctx, batch.RequestID); b.Status != store.StatusPending {
		t.Fatalf("the batch moved to %s", b.Status)
	}

	// The honest path: begin with a drop, and the drop list lives in the stored
	// challenge, so the finish call cannot widen it.
	body, begin := h.assertionFor(batch.RequestID, "approve", items[1].RequestID)
	if body == nil {
		t.Fatalf("begin: %d %s", begin.Code, begin.Body)
	}
	if r := h.post("/approve/r/"+batch.RequestID+"/finish", body); r.Code != http.StatusOK {
		t.Fatalf("finish: %d %s", r.Code, r.Body)
	}
	if s, _ := h.client.Status(ctx, items[1].RequestID); s.Status != "denied" {
		t.Fatalf("the item dropped at begin is %s, want denied", s.Status)
	}
	if s, _ := h.client.Status(ctx, items[0].RequestID); s.Status != "approved" {
		t.Fatalf("the kept item is %s", s.Status)
	}
}

func TestBatchNeedsAtLeastOneItemAndKnownDrops(t *testing.T) {
	h := newHarness(t)
	h.enroll()
	batch, items, _ := h.batchOf(work, "one", "two")
	h.tap(batch.RequestID)
	if r := h.postJSON("/approve/r/"+batch.RequestID+"/begin", map[string]any{"action": "approve", "drop": []string{items[0].RequestID, items[1].RequestID}}); r.Code != http.StatusBadRequest {
		t.Fatalf("dropping every item: %d %s, want 400", r.Code, r.Body)
	}
	if r := h.postJSON("/approve/r/"+batch.RequestID+"/begin", map[string]any{"action": "approve", "drop": []string{"apr_unrelated"}}); r.Code != http.StatusBadRequest {
		t.Fatalf("dropping a stranger: %d %s, want 400", r.Code, r.Body)
	}
	// Denying the whole batch leaves the items alone to be decided separately.
	if r := h.decide(batch.RequestID, "deny"); r.Code != http.StatusOK {
		t.Fatalf("deny batch: %d %s", r.Code, r.Body)
	}
	if s, _ := h.client.Status(context.Background(), items[0].RequestID); s.Status != "pending" {
		t.Fatalf("denying the batch changed an item to %s", s.Status)
	}
}

func TestBatchNeedsTheFromTapAndPendingItems(t *testing.T) {
	h := newHarness(t)
	h.enroll()
	batch, items, _ := h.batchOf(work, "one", "two")
	if r := h.decide(batch.RequestID, "approve"); r.Code != http.StatusConflict {
		t.Fatalf("batch approve without the tap: %d %s", r.Code, r.Body)
	}
	// An item decided on its own makes the batch unapprovable.
	h.approveSend(items[0].RequestID)
	page := h.get("/approve/r/" + batch.RequestID)
	if !strings.Contains(page.Body, "Batch not available") || strings.Contains(page.Body, `id="approve"`) {
		t.Fatalf("a batch with a decided item must not be approvable: %s", page.Body)
	}
}

// ---- the queue ----

func (h *harness) queueLogin() *http.Cookie {
	h.t.Helper()
	begin := h.post("/approve/queue/login/begin", nil)
	if begin.Code != http.StatusOK {
		h.t.Fatalf("queue login begin: %d %s", begin.Code, begin.Body)
	}
	var ca protocol.CredentialAssertion
	if err := json.Unmarshal([]byte(begin.Body), &ca); err != nil {
		h.t.Fatal(err)
	}
	fin := h.post("/approve/queue/login/finish", h.auth.Assert(&ca))
	if fin.Code != http.StatusOK || len(fin.Cookies) != 1 {
		h.t.Fatalf("queue login finish: %d %s %v", fin.Code, fin.Body, fin.Cookies)
	}
	c := fin.Cookies[0]
	if !c.HttpOnly || !c.Secure || c.SameSite != http.SameSiteStrictMode {
		h.t.Fatalf("queue cookie flags: %+v", c)
	}
	return c
}

func TestQueueGroupsPendingApprovalsByAccount(t *testing.T) {
	h := newHarness(t)
	h.enroll()
	a1 := h.create(sendBody(work, sendOpts{Draft: "w1"}))
	h.advance(time.Second)
	h.create(sendBody(personal, sendOpts{Draft: "p1"}))
	h.advance(time.Second)
	a2 := h.create(sendBody(work, sendOpts{Draft: "w2"}))
	h.advance(time.Second)
	dash, _ := json.Marshal(api.DashboardSessionPayload{Owner: owner, ClientID: "c", ClientName: "Claude", RedirectURIHost: "claude.ai", TxnHash: sum("x")})
	h.create(api.CreateBody{IdempotencyKey: "dash", Kind: api.KindDashboardSession, Payload: dash})
	decided := h.create(sendBody(work, sendOpts{Draft: "w3"}))
	h.decide(decided.RequestID, "deny")

	// Signed out, the queue shows only the sign-in.
	r := h.get("/approve/queue")
	if r.Code != http.StatusUnauthorized || !strings.Contains(r.Body, "Sign in with passkey") || strings.Contains(r.Body, a1.RequestID) {
		t.Fatalf("queue without a session: %d %s", r.Code, r.Body)
	}
	if r := h.get("/approve/queue", &http.Cookie{Name: "moose_approver_queue", Value: "forged.value"}); r.Code != http.StatusUnauthorized {
		t.Fatalf("a forged queue cookie: %d", r.Code)
	}

	cookie := h.queueLogin()
	r = h.get("/approve/queue", cookie)
	if r.Code != http.StatusOK {
		t.Fatalf("queue: %d %s", r.Code, r.Body)
	}
	body := r.Body
	iWork, iHome, iNone := strings.Index(body, ">Work"), strings.Index(body, ">Personal"), strings.Index(body, "Not tied to an account")
	if iWork < 0 || iHome < 0 || iNone < 0 || !(iWork < iHome && iHome < iNone) {
		t.Fatalf("groups out of order or missing (work %d, personal %d, none %d)", iWork, iHome, iNone)
	}
	for _, id := range []string{a1.RequestID, a2.RequestID} {
		if !strings.Contains(body, `href="/approve/r/`+id+`"`) {
			t.Errorf("queue lacks a link to %s", id)
		}
	}
	if strings.Contains(body, decided.RequestID) {
		t.Error("a denied request is still queued")
	}
	// Work's two sends sit under one heading.
	workSection := body[iWork:iHome]
	if strings.Count(workSection, "/approve/r/apr_") != 2 {
		t.Errorf("the Work group holds %d requests, want 2", strings.Count(workSection, "/approve/r/apr_"))
	}
	if !strings.Contains(body, "alice@example.com") || !strings.Contains(body, "alice.doe@gmail.example") {
		t.Error("group headings omit the account addresses")
	}

	// The session lapses.
	h.advance(16 * time.Minute)
	if r := h.get("/approve/queue", cookie); r.Code != http.StatusUnauthorized {
		t.Fatalf("an expired queue session: %d", r.Code)
	}
}

func TestQueueLoginNeedsTheRightChallenge(t *testing.T) {
	h := newHarness(t)
	h.enroll()
	id := h.create(sendBody(work, sendOpts{})).RequestID
	// A challenge issued for approving a request does not sign in to the queue.
	body := h.rawAssertion(id, "x")
	if r := h.post("/approve/queue/login/finish", body); r.Code != http.StatusForbidden {
		t.Fatalf("a request challenge on the queue login: %d %s", r.Code, r.Body)
	}
}

// ---- store reads used by the queue ----

func TestQueueSkipsExpiredRequests(t *testing.T) {
	h := newHarness(t)
	h.enroll()
	h.create(sendBody(work, sendOpts{}))
	cookie := h.queueLogin()
	h.advance(store.RequestLifetime + time.Second)
	// The queue session (15 minutes) has also lapsed; sign in again.
	cookie = h.queueLogin()
	r := h.get("/approve/queue", cookie)
	if !strings.Contains(r.Body, "Nothing is waiting for approval") {
		t.Fatalf("an expired request is queued: %s", r.Body)
	}
}
