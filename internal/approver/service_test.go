package approver_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/change-fabric/moose/internal/approver/api"
	"github.com/change-fabric/moose/internal/approver/receipt"
	"github.com/change-fabric/moose/internal/approver/store"
)

func apiCode(err error) (int, string) {
	var ae *api.Error
	if errors.As(err, &ae) {
		return ae.Status, ae.Code
	}
	return 0, ""
}

// ---- the approve path ----

func TestApprovePathWithVirtualAuthenticator(t *testing.T) {
	h := newHarness(t)
	h.enroll()
	body := sendBody(work, sendOpts{})
	created := h.create(body)
	ctx := context.Background()

	st, err := h.client.Status(ctx, created.RequestID)
	if err != nil || st.Status != "pending" || st.Receipt != nil {
		t.Fatalf("fresh request: %+v, %v", st, err)
	}
	if want := "https://moose.example.org/approve/r/" + created.RequestID; created.ApprovalURL != want {
		t.Fatalf("approval_url = %q, want %q", created.ApprovalURL, want)
	}
	if want := canonSHA(t, body.Payload); created.PayloadSHA256 != want {
		t.Fatalf("payload_sha256 = %s, want the sha of the canonical payload %s", created.PayloadSHA256, want)
	}

	// The page shows the exact message and the sender.
	page := h.get("/approve/r/" + created.RequestID)
	for _, want := range []string{
		"bob@elsewhere.example", "Friday", "Hello Bob,", "New recipient: bob@elsewhere.example",
		"alice@example.com", "Send from Work (alice@example.com)", sum("draft-1" + "Hello Bob,\n\nSee you Friday.\n"),
	} {
		if !strings.Contains(page.Body, want) {
			t.Errorf("approval page lacks %q", want)
		}
	}
	// A decision before the FROM tap is refused.
	if r := h.postJSON("/approve/r/"+created.RequestID+"/begin", map[string]any{"action": "approve"}); r.Code != http.StatusConflict || !strings.Contains(r.Body, "from_not_confirmed") {
		t.Fatalf("approve before the FROM tap: %d %s", r.Code, r.Body)
	}
	if r := h.tap(created.RequestID); r.Code != http.StatusOK {
		t.Fatalf("tap: %d %s", r.Code, r.Body)
	}
	if r := h.decide(created.RequestID, "approve"); r.Code != http.StatusOK {
		t.Fatalf("approve: %d %s", r.Code, r.Body)
	}

	st, err = h.client.Status(ctx, created.RequestID)
	if err != nil || st.Status != "approved" || st.Receipt == nil {
		t.Fatalf("after approval: %+v, %v", st, err)
	}
	want := h.want(created.RequestID, api.KindEmailSend, canonSHA(t, body.Payload))
	if err := receipt.Verify(h.pub, *st.Receipt, want); err != nil {
		t.Fatalf("receipt does not verify: %v", err)
	}
	if got := st.Receipt.Body.ExpiresAt.Sub(st.Receipt.Body.ApprovedAt); got != 15*time.Minute {
		t.Fatalf("receipt lifetime = %v, want 15m", got)
	}

	cons, err := h.client.Consume(ctx, created.RequestID, "email-req-1")
	if err != nil || cons.Status != "consumed" || cons.Receipt == nil {
		t.Fatalf("consume: %+v, %v", cons, err)
	}
	if ok, _ := h.st.HasPreview(ctx, created.RequestID); ok {
		t.Fatal("the preview survived consume")
	}
	if page := h.get("/approve/r/" + created.RequestID); !strings.Contains(page.Body, "Already used") || strings.Contains(page.Body, "Hello Bob") {
		t.Fatalf("a consumed request's page must not show the message: %s", page.Body)
	}
}

func TestReceiptPayloadsCoverAccountAndFromAddress(t *testing.T) {
	h := newHarness(t)
	h.enroll()
	for name, tc := range map[string]struct {
		body api.CreateBody
		kind string
	}{
		"email-send":          {sendBody(work, sendOpts{}), api.KindEmailSend},
		"account-send-enable": {enableBody(work, "enable-1"), api.KindAccountSendEnable},
	} {
		created := h.create(tc.body)
		h.approveSend(created.RequestID)
		st, err := h.client.Status(context.Background(), created.RequestID)
		if err != nil || st.Receipt == nil {
			t.Fatalf("%s: %+v, %v", name, st, err)
		}
		req, _ := h.st.GetRequest(context.Background(), created.RequestID)
		var payload map[string]any
		if err := json.Unmarshal(req.Payload, &payload); err != nil {
			t.Fatal(err)
		}
		if payload["account_id"] != work.ID || payload["from_address"] != work.Addr {
			t.Errorf("%s: payload %v lacks account_id and from_address", name, payload)
		}
		if st.Receipt.Body.PayloadSHA256 != api.SHA256(req.Payload) || st.Receipt.Body.Kind != tc.kind {
			t.Errorf("%s: the receipt does not cover the stored payload: %+v", name, st.Receipt.Body)
		}
		// A receipt for another account's payload does not verify for this one.
		other := strings.Replace(string(req.Payload), work.ID, personal.ID, 1)
		if err := receipt.Verify(h.pub, *st.Receipt, h.want(created.RequestID, tc.kind, canonSHA(t, []byte(other)))); !errors.Is(err, receipt.ErrMismatch) {
			t.Errorf("%s: receipt verified against another account's payload: %v", name, err)
		}
	}
}

func TestContactTrustBatchAndDashboardSession(t *testing.T) {
	h := newHarness(t)
	h.enroll()
	trust, _ := json.Marshal(api.ContactTrustPayload{Owner: owner, AccountID: work.ID, Changes: []api.TrustChange{{Address: "carol@x.example", To: "trusted"}, {Address: "dan@x.example", To: "trusted"}}})
	pv, _ := json.Marshal(api.Preview{Nickname: work.Nick})
	c1 := h.create(api.CreateBody{IdempotencyKey: "t1", Kind: api.KindContactTrustBatch, Payload: trust, Preview: pv})
	page := h.get("/approve/r/" + c1.RequestID)
	for _, want := range []string{"carol@x.example", "dan@x.example", "Trust these addresses"} {
		if !strings.Contains(page.Body, want) {
			t.Errorf("trust page lacks %q", want)
		}
	}
	// No FROM banner, so no tap is needed.
	if strings.Contains(page.Body, "Sending from") {
		t.Error("a contact-trust page shows the FROM banner")
	}
	if r := h.decide(c1.RequestID, "approve"); r.Code != http.StatusOK {
		t.Fatalf("approve trust batch: %d %s", r.Code, r.Body)
	}

	dash, _ := json.Marshal(api.DashboardSessionPayload{Owner: owner, ClientID: "client-1", ClientName: "<script>alert(1)</script>Claude", RedirectURIHost: "claude.ai", TxnHash: sum("handle")})
	c2 := h.create(api.CreateBody{IdempotencyKey: "d1", Kind: api.KindDashboardSession, Payload: dash})
	page = h.get("/approve/r/" + c2.RequestID)
	for _, want := range []string{"client-1", "claude.ai", "&lt;script&gt;alert(1)&lt;/script&gt;Claude"} {
		if !strings.Contains(page.Body, want) {
			t.Errorf("dashboard-session page lacks %q", want)
		}
	}
	if strings.Contains(page.Body, "<script>alert(1)") {
		t.Fatal("the client name was not escaped")
	}
	if r := h.decide(c2.RequestID, "approve"); r.Code != http.StatusOK {
		t.Fatalf("approve dashboard session: %d %s", r.Code, r.Body)
	}
	st, _ := h.client.Status(context.Background(), c2.RequestID)
	if err := receipt.Verify(h.pub, *st.Receipt, h.want(c2.RequestID, api.KindDashboardSession, canonSHA(t, dash))); err != nil {
		t.Fatal(err)
	}
}

// ---- WebAuthn rules over HTTP ----

func TestAssertionWithoutUserVerificationIsRejected(t *testing.T) {
	h := newHarness(t)
	h.enroll()
	id := h.create(sendBody(work, sendOpts{})).RequestID
	h.tap(id)
	h.auth.UserVerified = false
	if r := h.decide(id, "approve"); r.Code != http.StatusForbidden {
		t.Fatalf("assertion without UV: %d %s, want 403", r.Code, r.Body)
	}
	if req, _ := h.st.GetRequest(context.Background(), id); req.Status != store.StatusPending || req.Receipt != nil {
		t.Fatalf("request after a rejected assertion: %+v", req)
	}
	h.auth.UserVerified = true
	if r := h.decide(id, "approve"); r.Code != http.StatusOK {
		t.Fatalf("a good assertion after a bad one: %d %s", r.Code, r.Body)
	}
}

func TestChallengeReplayIsRejected(t *testing.T) {
	h := newHarness(t)
	h.enroll()
	id := h.create(sendBody(work, sendOpts{})).RequestID
	body, begin := h.assertionFor(id, "deny")
	if body == nil {
		t.Fatalf("begin: %d %s", begin.Code, begin.Body)
	}
	if r := h.post("/approve/r/"+id+"/finish", body); r.Code != http.StatusOK {
		t.Fatalf("deny: %d %s", r.Code, r.Body)
	}
	if r := h.post("/approve/r/"+id+"/finish", body); r.Code == http.StatusOK {
		t.Fatalf("a replayed assertion was accepted: %s", r.Body)
	}
}

func TestChallengeForRequestAIsRejectedOnRequestB(t *testing.T) {
	h := newHarness(t)
	h.enroll()
	a := h.create(sendBody(work, sendOpts{Draft: "a"})).RequestID
	b := h.create(sendBody(work, sendOpts{Draft: "b"})).RequestID
	h.tap(a)
	h.tap(b)
	body, begin := h.assertionFor(a, "approve")
	if body == nil {
		t.Fatalf("begin: %d %s", begin.Code, begin.Body)
	}
	if r := h.post("/approve/r/"+b+"/finish", body); r.Code != http.StatusForbidden {
		t.Fatalf("A's assertion on B: %d %s, want 403", r.Code, r.Body)
	}
	if req, _ := h.st.GetRequest(context.Background(), b); req.Status != store.StatusPending {
		t.Fatalf("B moved to %s", req.Status)
	}
	// The failed use burned the challenge: it no longer approves A either.
	if r := h.post("/approve/r/"+a+"/finish", body); r.Code == http.StatusOK {
		t.Fatal("a challenge survived a mismatched use")
	}
	if req, _ := h.st.GetRequest(context.Background(), a); req.Status != store.StatusPending {
		t.Fatalf("A moved to %s", req.Status)
	}
}

func TestAssertionWithoutRecordedFromConfirmationIsRejected(t *testing.T) {
	h := newHarness(t)
	h.enroll()
	created := h.create(sendBody(work, sendOpts{}))
	id := created.RequestID
	// Bypass the begin handler's own check: issue the challenge directly, as a
	// client that skipped the tap would try to.
	body := h.rawAssertion(id, created.PayloadSHA256)
	r := h.post("/approve/r/"+id+"/finish", body)
	if r.Code != http.StatusConflict || !strings.Contains(r.Body, "from_not_confirmed") {
		t.Fatalf("assertion without the FROM tap: %d %s", r.Code, r.Body)
	}
	if req, _ := h.st.GetRequest(context.Background(), id); req.Status != store.StatusPending || req.Receipt != nil {
		t.Fatalf("request after the refusal: %+v", req)
	}
	// An account-send-enable request needs the tap too.
	en := h.create(enableBody(work, "en")).RequestID
	if r := h.decide(en, "approve"); r.Code != http.StatusConflict {
		t.Fatalf("enable without the tap: %d %s", r.Code, r.Body)
	}
}

func TestEnrollmentTokenReuseAndExpiry(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	tok := store.NewToken()
	_ = h.st.CreateEnrollToken(ctx, store.EnrollToken{Hash: store.Hash(tok), Owner: owner, Expires: h.clock().Add(store.EnrollTTL)})
	h.enrollWith(tok, h.auth, http.StatusOK)

	// The same link a second time: the page, begin and finish all refuse.
	if r := h.get("/approve/register/" + tok); r.Code != http.StatusNotFound {
		t.Fatalf("a used token's page: %d", r.Code)
	}
	if r := h.post("/approve/register/"+tok+"/begin", nil); r.Code != http.StatusForbidden {
		t.Fatalf("a used token's begin: %d %s", r.Code, r.Body)
	}

	// An expired token.
	tok2 := store.NewToken()
	_ = h.st.CreateEnrollToken(ctx, store.EnrollToken{Hash: store.Hash(tok2), Owner: owner, Expires: h.clock().Add(store.EnrollTTL)})
	h.advance(store.EnrollTTL)
	if r := h.get("/approve/register/" + tok2); r.Code != http.StatusNotFound {
		t.Fatalf("an expired token's page: %d", r.Code)
	}
	if r := h.post("/approve/register/"+tok2+"/begin", nil); r.Code != http.StatusForbidden {
		t.Fatalf("an expired token's begin: %d", r.Code)
	}

	// Begin before expiry, finish after: the token is rechecked at finish.
	h.advance(time.Second)
	tok3 := store.NewToken()
	_ = h.st.CreateEnrollToken(ctx, store.EnrollToken{Hash: store.Hash(tok3), Owner: owner, Expires: h.clock().Add(store.EnrollTTL)})
	begin := h.post("/approve/register/"+tok3+"/begin", nil)
	if begin.Code != http.StatusOK {
		t.Fatalf("begin: %d %s", begin.Code, begin.Body)
	}
	h.advance(store.EnrollTTL)
	other := newAuthenticator()
	if r := h.post("/approve/register/"+tok3+"/finish", other.registerFrom(t, begin.Body)); r.Code == http.StatusOK {
		t.Fatal("a token that expired mid-ceremony was redeemed")
	}
	creds, _ := h.st.ListCredentials(ctx, owner)
	if len(creds) != 1 {
		t.Fatalf("%d passkeys stored, want only the first", len(creds))
	}
}

func TestStateChangingRequestsNeedTheSiteOrigin(t *testing.T) {
	h := newHarness(t)
	h.enroll()
	id := h.create(sendBody(work, sendOpts{})).RequestID
	for name, hdr := range map[string]map[string]string{
		"no origin":    {"Origin": ""},
		"other origin": {"Origin": "https://evil.example"},
	} {
		for _, path := range []string{"/from", "/begin", "/finish"} {
			if r := h.do(http.MethodPost, "/approve/r/"+id+path, []byte(`{"action":"deny"}`), hdr); r.Code != http.StatusForbidden {
				t.Errorf("%s on %s: %d, want 403", name, path, r.Code)
			}
		}
	}
}

func TestPagesSendStrictHeaders(t *testing.T) {
	h := newHarness(t)
	id := h.create(sendBody(work, sendOpts{})).RequestID
	r := h.get("/approve/r/" + id)
	csp := r.Header.Get("Content-Security-Policy")
	for _, want := range []string{"default-src 'none'", "script-src 'nonce-", "frame-ancestors 'none'", "connect-src 'self'"} {
		if !strings.Contains(csp, want) {
			t.Errorf("CSP %q lacks %q", csp, want)
		}
	}
	if strings.Contains(csp, "unsafe-inline") {
		t.Error("CSP allows unsafe-inline")
	}
	nonce := strings.Split(strings.Split(csp, "script-src 'nonce-")[1], "'")[0]
	if !strings.Contains(r.Body, `<script nonce="`+nonce+`">`) || !strings.Contains(r.Body, `<style nonce="`+nonce+`">`) {
		t.Error("the page's script and style do not carry the CSP nonce")
	}
	if r.Header.Get("Cache-Control") != "no-store" || r.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Errorf("headers: %v", r.Header)
	}
}

// ---- the service API ----

func TestCreateIsIdempotent(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	body := sendBody(work, sendOpts{Key: "same-key"})
	first, created, err := h.client.Create(ctx, body)
	if err != nil || !created {
		t.Fatalf("first create: created=%v err=%v", created, err)
	}
	second, created, err := h.client.Create(ctx, body)
	if err != nil || created {
		t.Fatalf("repeat create: created=%v err=%v, want 200 and no duplicate", created, err)
	}
	if first.RequestID != second.RequestID || first.ApprovalURL != second.ApprovalURL || first.PayloadSHA256 != second.PayloadSHA256 {
		t.Fatalf("repeat returned %+v, want %+v", second, first)
	}
	if open, _ := h.st.ListOpen(ctx, owner); len(open) != 1 {
		t.Fatalf("%d requests exist, want 1", len(open))
	}
	other := sendBody(work, sendOpts{Key: "same-key", Draft: "different-draft"})
	if _, _, err := h.client.Create(ctx, other); err == nil {
		t.Fatal("the same key with another payload was accepted")
	} else if code, ec := apiCode(err); code != http.StatusConflict || ec != "idempotency_conflict" {
		t.Fatalf("same key, other payload: %v", err)
	}
}

func TestConsumingTwiceGives409(t *testing.T) {
	h := newHarness(t)
	h.enroll()
	ctx := context.Background()
	id := h.create(sendBody(work, sendOpts{})).RequestID
	// Not yet approved.
	if _, err := h.client.Consume(ctx, id, "c1"); err == nil {
		t.Fatal("consumed a pending request")
	} else if code, _ := apiCode(err); code != http.StatusConflict {
		t.Fatalf("consume while pending: %v", err)
	}
	h.approveSend(id)
	if _, err := h.client.Consume(ctx, id, "c1"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.client.Consume(ctx, id, "c2"); !errors.Is(err, api.ErrAlreadyConsumed) {
		t.Fatalf("second consume by another claimant: %v, want 409", err)
	}
	if _, err := h.client.Consume(ctx, id, ""); !errors.Is(err, api.ErrAlreadyConsumed) {
		t.Fatalf("second consume with no claimant: %v, want 409", err)
	}
	// The same claimant retrying is idempotent (a crash between consume and the local write).
	if st, err := h.client.Consume(ctx, id, "c1"); err != nil || st.Status != "consumed" {
		t.Fatalf("same claimant again: %+v, %v, want 200", st, err)
	}
	// Raw: the second consume without a claimant is a 409 on the wire.
	r := h.rawAPI(http.MethodPost, api.Prefix+"requests/"+id+"/consume", []byte(`{}`), h.clock().Unix(), true)
	if r.Code != http.StatusConflict {
		t.Fatalf("raw second consume: %d %s", r.Code, r.Body)
	}
}

func TestStatusTransitionsIncludingExpired(t *testing.T) {
	h := newHarness(t)
	h.enroll()
	ctx := context.Background()
	status := func(id string) string {
		st, err := h.client.Status(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if st.Status != "approved" && st.Status != "consumed" && st.Receipt != nil {
			t.Fatalf("status %s carries a receipt", st.Status)
		}
		if (st.Status == "approved" || st.Status == "consumed") && st.Receipt == nil {
			t.Fatalf("status %s has no receipt", st.Status)
		}
		return st.Status
	}

	pending := h.create(sendBody(work, sendOpts{Draft: "p"})).RequestID
	denied := h.create(sendBody(work, sendOpts{Draft: "d"})).RequestID
	approved := h.create(sendBody(work, sendOpts{Draft: "a"})).RequestID
	consumed := h.create(sendBody(work, sendOpts{Draft: "c"})).RequestID
	expired := h.create(sendBody(work, sendOpts{Draft: "e"})).RequestID
	if r := h.decide(denied, "deny"); r.Code != http.StatusOK {
		t.Fatalf("deny: %d %s", r.Code, r.Body)
	}
	h.approveSend(approved)
	h.approveSend(consumed)
	if _, err := h.client.Consume(ctx, consumed, "x"); err != nil {
		t.Fatal(err)
	}
	for id, want := range map[string]string{pending: "pending", denied: "denied", approved: "approved", consumed: "consumed", expired: "pending"} {
		if got := status(id); got != want {
			t.Errorf("%s = %s, want %s", id, got, want)
		}
	}

	h.advance(store.RequestLifetime + time.Second)
	if got := status(pending); got != "expired" {
		t.Errorf("an undecided request after 15 minutes = %s, want expired", got)
	}
	if got := status(expired); got != "expired" {
		t.Errorf("= %s, want expired", got)
	}
	if got := status(denied); got != "denied" {
		t.Errorf("denied request changed to %s", got)
	}
	// An approval outlives the request window; its own receipt window rules.
	if got := status(approved); got != "approved" {
		t.Errorf("approved request after the request window = %s", got)
	}
	// An expired request cannot be decided and its page says so.
	if r := h.decide(pending, "approve"); r.Code != http.StatusConflict {
		t.Fatalf("deciding an expired request: %d %s", r.Code, r.Body)
	}
	if page := h.get("/approve/r/" + pending); !strings.Contains(page.Body, "Expired") || strings.Contains(page.Body, "Hello Bob") {
		t.Fatalf("expired page: %s", page.Body)
	}
	if ok, _ := h.st.HasPreview(ctx, pending); ok {
		t.Fatal("the preview of an expired request survived")
	}
	if _, err := h.client.Status(ctx, "apr_doesnotexist"); err == nil {
		t.Fatal("status of an unknown request succeeded")
	} else if code, _ := apiCode(err); code != http.StatusNotFound {
		t.Fatalf("unknown request: %v", err)
	}
}

func TestReceiptSubstitutionIsRejected(t *testing.T) {
	h := newHarness(t)
	h.enroll()
	ctx := context.Background()
	bodyA, bodyB := sendBody(work, sendOpts{Draft: "a"}), sendBody(work, sendOpts{Draft: "b"})
	a, b := h.create(bodyA).RequestID, h.create(bodyB).RequestID
	h.approveSend(a)
	h.approveSend(b)
	stA, _ := h.client.Status(ctx, a)
	rcpt := *stA.Receipt
	shaA, shaB := canonSHA(t, bodyA.Payload), canonSHA(t, bodyB.Payload)
	if err := receipt.Verify(h.pub, rcpt, h.want(a, api.KindEmailSend, shaA)); err != nil {
		t.Fatalf("the genuine pair must verify: %v", err)
	}
	cases := map[string]receipt.Want{
		"another request_id":     h.want(b, api.KindEmailSend, shaA),
		"another payload_sha256": h.want(a, api.KindEmailSend, shaB),
		"another kind":           h.want(a, api.KindAccountSendEnable, shaA),
	}
	for name, w := range cases {
		if err := receipt.Verify(h.pub, rcpt, w); !errors.Is(err, receipt.ErrMismatch) {
			t.Errorf("%s: %v, want ErrMismatch", name, err)
		}
	}
	// Moving A's receipt onto B's request is a forgery.
	forged := rcpt
	forged.Body.RequestID, forged.Body.PayloadSHA256 = b, shaB
	if err := receipt.Verify(h.pub, forged, h.want(b, api.KindEmailSend, shaB)); !errors.Is(err, receipt.ErrSignature) {
		t.Errorf("a receipt rewritten for B: %v, want ErrSignature", err)
	}
	// Expired.
	h.advance(store.ReceiptLifetime)
	if err := receipt.Verify(h.pub, rcpt, h.want(a, api.KindEmailSend, shaA)); !errors.Is(err, receipt.ErrExpired) {
		t.Errorf("an expired receipt: %v, want ErrExpired", err)
	}
	if _, err := h.client.Consume(ctx, a, "c"); err == nil {
		t.Error("an expired approval was consumed")
	} else if code, _ := apiCode(err); code != http.StatusGone {
		t.Errorf("consume after the receipt lapsed: %v", err)
	}
}

func TestPreviewIsGoneAfterDenyAndConsume(t *testing.T) {
	h := newHarness(t)
	h.enroll()
	ctx := context.Background()
	denied := h.create(sendBody(work, sendOpts{Draft: "d"})).RequestID
	used := h.create(sendBody(work, sendOpts{Draft: "u"})).RequestID
	h.decide(denied, "deny")
	h.approveSend(used)
	if ok, _ := h.st.HasPreview(ctx, used); !ok {
		t.Fatal("the preview must still exist between approval and consume")
	}
	if _, err := h.client.Consume(ctx, used, "c"); err != nil {
		t.Fatal(err)
	}
	for name, id := range map[string]string{"deny": denied, "consume": used} {
		if ok, _ := h.st.HasPreview(ctx, id); ok {
			t.Errorf("the preview survived %s", name)
		}
		req, _ := h.st.GetRequest(ctx, id)
		if _, err := h.st.Preview(ctx, req); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("Preview after %s: %v, want ErrNotFound", name, err)
		}
	}
}

// ---- HMAC ----

// rawAPI sends an API request with a MAC made at ts, tampering with nothing.
func (h *harness) rawAPI(method, path string, body []byte, ts int64, sign bool) resp {
	r := httptest.NewRequest(method, "https://moose.example.org"+path, strings.NewReader(string(body)))
	r.Header.Set(api.HeaderTimestamp, strconv.FormatInt(ts, 10))
	if sign {
		r.Header.Set(api.HeaderSignature, api.Sign(h.secret, method, path, ts, body))
	}
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, r)
	return resp{Code: rec.Code, Body: rec.Body.String()}
}

func TestServiceAPIRequiresAValidMAC(t *testing.T) {
	h := newHarness(t)
	id := h.create(sendBody(work, sendOpts{})).RequestID
	path := api.Prefix + "requests/" + id
	now := h.clock().Unix()

	if r := h.rawAPI(http.MethodGet, path, nil, now, true); r.Code != http.StatusOK {
		t.Fatalf("a valid MAC: %d %s", r.Code, r.Body)
	}
	for name, r := range map[string]resp{
		"no signature":     h.rawAPI(http.MethodGet, path, nil, now, false),
		"stale timestamp":  h.rawAPI(http.MethodGet, path, nil, now-int64(api.MaxSkew/time.Second)-1, true),
		"future timestamp": h.rawAPI(http.MethodGet, path, nil, now+int64(api.MaxSkew/time.Second)+1, true),
	} {
		if r.Code != http.StatusUnauthorized {
			t.Errorf("%s: %d, want 401", name, r.Code)
		}
	}
	// The edge of the window is accepted.
	if r := h.rawAPI(http.MethodGet, path, nil, now-int64(api.MaxSkew/time.Second), true); r.Code != http.StatusOK {
		t.Errorf("a timestamp 5 minutes old: %d, want 200", r.Code)
	}
	// A MAC covers the method, path and body: replaying it elsewhere fails.
	req := httptest.NewRequest(http.MethodGet, "https://moose.example.org"+path, nil)
	req.Header.Set(api.HeaderTimestamp, strconv.FormatInt(now, 10))
	req.Header.Set(api.HeaderSignature, api.Sign(h.secret, http.MethodGet, path, now, nil))
	other := httptest.NewRequest(http.MethodPost, "https://moose.example.org"+path+"/consume", nil)
	other.Header = req.Header.Clone()
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, other)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("a MAC replayed on another route: %d, want 401", rec.Code)
	}
	body := []byte(`{"idempotency_key":"k"}`)
	signed := api.Sign(h.secret, http.MethodPost, api.Prefix+"requests", now, body)
	tampered := httptest.NewRequest(http.MethodPost, "https://moose.example.org"+api.Prefix+"requests", strings.NewReader(`{"idempotency_key":"j"}`))
	tampered.Header.Set(api.HeaderTimestamp, strconv.FormatInt(now, 10))
	tampered.Header.Set(api.HeaderSignature, signed)
	rec = httptest.NewRecorder()
	h.handler.ServeHTTP(rec, tampered)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("a tampered body: %d, want 401", rec.Code)
	}
	wrongKey := &api.Client{BaseURL: origin, Secret: []byte(strings.Repeat("x", 40)), Now: h.clock, HTTP: &http.Client{Transport: handlerTransport{h: h.handler, calls: &h.calls}}}
	if _, err := wrongKey.Status(context.Background(), id); err == nil {
		t.Error("a client with another secret was served")
	} else if code, _ := apiCode(err); code != http.StatusUnauthorized {
		t.Errorf("another secret: %v", err)
	}
}

func TestClientRetriesOnceOnServerErrorsOnly(t *testing.T) {
	h := newHarness(t)
	id := h.create(sendBody(work, sendOpts{})).RequestID
	mk := func(fail int32) (*api.Client, *atomic.Int32) {
		var calls atomic.Int32
		return &api.Client{BaseURL: origin, Secret: h.secret, Now: h.clock,
			HTTP: &http.Client{Transport: handlerTransport{h: h.handler, calls: &calls, failFirst: fail}}}, &calls
	}
	ctx := context.Background()
	if c, calls := mk(1); true {
		if st, err := c.Status(ctx, id); err != nil || st.Status != "pending" || calls.Load() != 2 {
			t.Fatalf("one 500 then success: %+v, %v, %d calls; want a retry", st, err, calls.Load())
		}
	}
	if c, calls := mk(2); true {
		if _, err := c.Status(ctx, id); err == nil || calls.Load() != 2 {
			t.Fatalf("two 500s: err=%v after %d calls; want failure after exactly 1 retry", err, calls.Load())
		}
	}
	if c, calls := mk(0); true {
		if _, err := c.Status(ctx, "apr_nope"); err == nil || calls.Load() != 1 {
			t.Fatalf("a 404: err=%v after %d calls; a 4xx must not be retried", err, calls.Load())
		}
	}
	if c, calls := mk(1); true {
		// Creates retry too: the idempotency key makes that safe.
		body := sendBody(work, sendOpts{Key: "retry-key", Draft: "r"})
		first, _, err := c.Create(ctx, body)
		if err != nil || calls.Load() != 2 {
			t.Fatalf("create with a retry: %v, %d calls", err, calls.Load())
		}
		again, created, err := h.client.Create(ctx, body)
		if err != nil || created || again.RequestID != first.RequestID {
			t.Fatalf("the retried create must not duplicate: %+v created=%v %v", again, created, err)
		}
	}
}

func TestCreateValidatesPayloadsAndPreviews(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	good := sendBody(work, sendOpts{})
	mut := func(f func(map[string]any)) api.CreateBody {
		var c map[string]any
		_ = json.Unmarshal(good.Payload, &c)
		f(c)
		b := good
		b.Payload, _ = json.Marshal(c)
		return b
	}
	pv := func(f func(*api.Preview)) api.CreateBody {
		var p api.Preview
		_ = json.Unmarshal(good.Preview, &p)
		f(&p)
		b := good
		b.Preview, _ = json.Marshal(p)
		return b
	}
	off := false
	cases := map[string]struct {
		body api.CreateBody
		code string
	}{
		"unknown kind":         {api.CreateBody{IdempotencyKey: "k1", Kind: "wire-money", Payload: good.Payload}, "invalid_payload"},
		"another owner":        {mut(func(c map[string]any) { c["owner"] = "mallory" }), "invalid_payload"},
		"unknown payload key":  {mut(func(c map[string]any) { c["extra"] = 1 }), "invalid_payload"},
		"missing from_address": {mut(func(c map[string]any) { delete(c, "from_address") }), "invalid_payload"},
		"bad rfc822 hash":      {mut(func(c map[string]any) { c["rfc822_sha256"] = "nope" }), "invalid_payload"},
		"no recipients":        {mut(func(c map[string]any) { c["recipients"] = []string{} }), "invalid_payload"},
		"trust change to blocked": {mut(func(c map[string]any) {
			c["trust_changes"] = []map[string]string{{"address": "x@y.z", "to": "blocked"}}
		}), "invalid_payload"},
		"preview for another from":  {pv(func(p *api.Preview) { p.FromAddress = "mallory@example.com" }), "preview_mismatch"},
		"preview for another acct":  {pv(func(p *api.Preview) { p.AccountID = "acc_other" }), "preview_mismatch"},
		"preview From header":       {pv(func(p *api.Preview) { p.Message.From = "mallory@example.com" }), "preview_mismatch"},
		"preview hides a recipient": {pv(func(p *api.Preview) { p.Message.To = []string{"someone.else@example.com"} }), "preview_mismatch"},
		"preview adds a bcc":        {pv(func(p *api.Preview) { p.Message.Bcc = []string{"spy@example.com"} }), "preview_mismatch"},
		"send disabled (R11)":       {pv(func(p *api.Preview) { p.SendEnabled = &off }), "account_not_ready"},
		"needs reauth (R11)":        {pv(func(p *api.Preview) { p.AccountStatus = "needs_reauth" }), "account_not_ready"},
		"disconnected (R11)":        {pv(func(p *api.Preview) { p.AccountStatus = "disconnected" }), "account_not_ready"},
		"no message":                {pv(func(p *api.Preview) { p.Message = nil }), "invalid_preview"},
		"preview too large":         {pv(func(p *api.Preview) { p.Message.Body = strings.Repeat("x", 400_000) }), "preview_too_large"},
	}
	for name, tc := range cases {
		tc.body.IdempotencyKey = "case-" + strings.ReplaceAll(name, " ", "-")
		_, _, err := h.client.Create(ctx, tc.body)
		code, ec := apiCode(err)
		if code != http.StatusUnprocessableEntity || ec != tc.code {
			t.Errorf("%s: %v, want 422 %s", name, err, tc.code)
		}
	}
	if open, _ := h.st.ListOpen(ctx, owner); len(open) != 0 {
		t.Fatalf("%d requests were stored although every create was refused", len(open))
	}
	// A send-enable request for a disabled account is allowed: that is its purpose.
	en := enableBody(work, "en-1")
	if _, _, err := h.client.Create(ctx, en); err != nil {
		t.Fatalf("account-send-enable: %v", err)
	}
	// moose-email's connect page sends a flat map[string]string preview.
	flat := enableBody(work, "en-2")
	flat.Preview = []byte(`{"action":"Enable sending","nickname":"Work","from_address":"alice@example.com","account_id":"acc_work"}`)
	if _, _, err := h.client.Create(ctx, flat); err != nil {
		t.Fatalf("the connect page's preview shape: %v", err)
	}
}

func TestConcurrentApprovalsOfOneRequestYieldOneReceipt(t *testing.T) {
	h := newHarness(t)
	h.enroll()
	id := h.create(sendBody(work, sendOpts{})).RequestID
	h.tap(id)
	var bodies [][]byte
	for range 4 {
		b, begin := h.assertionFor(id, "approve")
		if b == nil {
			t.Fatalf("begin: %d %s", begin.Code, begin.Body)
		}
		bodies = append(bodies, b)
	}
	var ok atomic.Int32
	var wg sync.WaitGroup
	for _, b := range bodies {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if r := h.post("/approve/r/"+id+"/finish", b); r.Code == http.StatusOK {
				ok.Add(1)
			}
		}()
	}
	wg.Wait()
	if ok.Load() != 1 {
		t.Fatalf("%d concurrent approvals succeeded, want exactly 1", ok.Load())
	}
}
