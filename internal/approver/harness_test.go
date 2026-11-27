package approver_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-webauthn/webauthn/protocol"

	"github.com/change-fabric/moose/internal/accounts/envelope"
	"github.com/change-fabric/moose/internal/accounts/store/sqlite"
	"github.com/change-fabric/moose/internal/approver/api"
	"github.com/change-fabric/moose/internal/approver/pages"
	"github.com/change-fabric/moose/internal/approver/receipt"
	"github.com/change-fabric/moose/internal/approver/store"
	"github.com/change-fabric/moose/internal/approver/webauthn"
	"github.com/change-fabric/moose/internal/approver/webauthn/webauthntest"
)

const (
	rpID   = "moose.example.org"
	origin = "https://moose.example.org"
	owner  = "pat"
)

var t0 = time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)

// acct is a connected account as moose-email would describe it.
type acct struct {
	ID, Nick, Addr, Display string
	Color                   int
}

var (
	work     = acct{"acc_work", "Work", "alice@example.com", "Alice Doe", 0}
	personal = acct{"acc_home", "Personal", "alice.doe@gmail.example", "Alice", 1}
)

type harness struct {
	t       *testing.T
	st      *store.Store
	handler http.Handler
	client  *api.Client
	secret  []byte
	signer  *receipt.LocalSigner
	pub     []byte
	auth    *webauthntest.Authenticator
	wa      *webauthn.Service
	now     atomic.Int64
	// transport counts and can fail API calls.
	calls atomic.Int32
}

func (h *harness) clock() time.Time        { return time.Unix(h.now.Load(), 0).UTC() }
func (h *harness) advance(d time.Duration) { h.now.Add(int64(d / time.Second)) }

type handlerTransport struct {
	h     http.Handler
	calls *atomic.Int32
	// fail, if set, answers the nth call (1-based) with a 500 for n <= failFirst.
	failFirst int32
}

func (t handlerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	n := t.calls.Add(1)
	rec := httptest.NewRecorder()
	if n <= t.failFirst {
		http.Error(rec, `{"error":"boom"}`, http.StatusInternalServerError)
	} else {
		t.h.ServeHTTP(rec, r)
	}
	return rec.Result(), nil
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	b, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "approver.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = b.Close() })
	key := make([]byte, 32)
	_, _ = rand.Read(key)
	seal, err := envelope.NewLocal(key)
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{t: t}
	h.now.Store(t0.Unix())
	h.st = store.NewWithClock(b, seal, h.clock)
	h.signer, err = receipt.GenerateLocalSigner()
	if err != nil {
		t.Fatal(err)
	}
	h.pub, _ = h.signer.PublicKeyPEM()
	h.secret = []byte(strings.Repeat("s3cret-", 8))
	h.wa, err = webauthn.New(webauthn.Config{RPID: rpID, Origin: origin}, h.st, h.clock)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.Handle(api.Prefix, (&api.Server{Store: h.st, Owner: owner, BaseURL: origin, Secret: h.secret, Now: h.clock}).Handler())
	mux.Handle("/approve/", (&pages.Server{
		Store: h.st, WebAuthn: h.wa, Signer: h.signer, RPID: rpID, Origin: origin, Owner: owner,
		Secret: h.secret, Now: h.clock,
	}).Handler())
	h.handler = mux
	h.client = &api.Client{
		BaseURL: origin, Secret: h.secret, Now: h.clock,
		HTTP: &http.Client{Transport: handlerTransport{h: mux, calls: &h.calls}},
	}
	h.auth = webauthntest.New(rpID, origin)
	return h
}

// ---- page requests ----

type resp struct {
	Code    int
	Body    string
	Cookies []*http.Cookie
	Header  http.Header
}

func (h *harness) do(method, path string, body []byte, hdr map[string]string) resp {
	h.t.Helper()
	r := httptest.NewRequest(method, "https://moose.example.org"+path, bytes.NewReader(body))
	if method == http.MethodPost {
		r.Header.Set("Origin", origin)
		r.Header.Set("Content-Type", "application/json")
	}
	for k, v := range hdr {
		if v == "" {
			r.Header.Del(k)
		} else {
			r.Header.Set(k, v)
		}
	}
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, r)
	res := rec.Result()
	return resp{res.StatusCode, rec.Body.String(), res.Cookies(), res.Header}
}

func (h *harness) get(path string, cookies ...*http.Cookie) resp {
	h.t.Helper()
	r := httptest.NewRequest(http.MethodGet, "https://moose.example.org"+path, nil)
	for _, c := range cookies {
		r.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, r)
	res := rec.Result()
	return resp{res.StatusCode, rec.Body.String(), res.Cookies(), res.Header}
}

func (h *harness) post(path string, body []byte) resp { return h.do(http.MethodPost, path, body, nil) }

func (h *harness) postJSON(path string, v any) resp {
	b, _ := json.Marshal(v)
	return h.post(path, b)
}

// enroll runs the full first-passkey enrollment with the virtual authenticator.
func (h *harness) enroll() {
	h.t.Helper()
	tok := store.NewToken()
	if err := h.st.CreateEnrollToken(context.Background(), store.EnrollToken{Hash: store.Hash(tok), Owner: owner, Expires: h.clock().Add(store.EnrollTTL)}); err != nil {
		h.t.Fatal(err)
	}
	h.enrollWith(tok, h.auth, http.StatusOK)
}

func (h *harness) enrollWith(tok string, a *webauthntest.Authenticator, wantFinish int) {
	h.t.Helper()
	if r := h.get("/approve/register/" + tok); r.Code != http.StatusOK {
		h.t.Fatalf("register page: %d %s", r.Code, r.Body)
	}
	begin := h.post("/approve/register/"+tok+"/begin", nil)
	if begin.Code != http.StatusOK {
		h.t.Fatalf("register begin: %d %s", begin.Code, begin.Body)
	}
	var creation protocol.CredentialCreation
	if err := json.Unmarshal([]byte(begin.Body), &creation); err != nil {
		h.t.Fatal(err)
	}
	if got := creation.Response.AuthenticatorSelection.UserVerification; got != protocol.VerificationRequired {
		h.t.Fatalf("registration userVerification = %q, want required", got)
	}
	fin := h.post("/approve/register/"+tok+"/finish", a.Register(&creation))
	if fin.Code != wantFinish {
		h.t.Fatalf("register finish: %d %s, want %d", fin.Code, fin.Body, wantFinish)
	}
}

// ---- requests ----

func sum(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

func sib(a acct) api.Sibling {
	return api.Sibling{AccountID: a.ID, Nickname: a.Nick, Address: a.Addr, DisplayName: a.Display}
}

type sendOpts struct {
	Key      string
	Draft    string
	To       []string
	Siblings []api.Sibling
	Status   string
	Disabled bool
	Last     *api.LastSent
	Body     string
	// Semantic marks a provider that re-serialises on send (Microsoft).
	Semantic bool
}

// sendBody builds an email-send create body for account a.
func sendBody(a acct, o sendOpts) api.CreateBody {
	if o.Draft == "" {
		o.Draft = "draft-1"
	}
	if o.Key == "" {
		o.Key = "key-" + a.ID + "-" + o.Draft
	}
	if len(o.To) == 0 {
		o.To = []string{"bob@elsewhere.example"}
	}
	if o.Status == "" {
		o.Status = "ok"
	}
	if o.Body == "" {
		o.Body = "Hello Bob,\n\nSee you Friday.\n"
	}
	on := !o.Disabled
	payload, _ := json.Marshal(api.EmailSendPayload{
		Owner: owner, AccountID: a.ID, FromAddress: a.Addr, DraftID: o.Draft, RFC822SHA256: sum(o.Draft + o.Body),
		Recipients: o.To, SoftFlags: []api.SoftFlag{{Flag: "new_recipient", Address: o.To[0]}},
		TrustChanges: []api.TrustChange{{Address: o.To[0], To: "trusted"}},
	})
	preview, _ := json.Marshal(api.Preview{
		AccountID: a.ID, Nickname: a.Nick, FromAddress: a.Addr, DisplayName: a.Display, ColorIndex: a.Color,
		AccountStatus: o.Status, SendEnabled: &on, Siblings: o.Siblings, LastSent: o.Last,
		Message: &api.Message{From: a.Addr, To: o.To, Subject: "Friday", Body: o.Body}, SemanticMatch: o.Semantic,
	})
	return api.CreateBody{IdempotencyKey: o.Key, Kind: api.KindEmailSend, Payload: payload, Preview: preview}
}

func enableBody(a acct, key string) api.CreateBody {
	payload, _ := json.Marshal(api.SendEnablePayload{Owner: owner, AccountID: a.ID, FromAddress: a.Addr})
	preview, _ := json.Marshal(api.Preview{
		Action: "Enable sending", AccountID: a.ID, Nickname: a.Nick, FromAddress: a.Addr, DisplayName: a.Display, ColorIndex: a.Color,
	})
	return api.CreateBody{IdempotencyKey: key, Kind: api.KindAccountSendEnable, Payload: payload, Preview: preview}
}

func (h *harness) create(body api.CreateBody) api.CreateResponse {
	h.t.Helper()
	resp, _, err := h.client.Create(context.Background(), body)
	if err != nil {
		h.t.Fatalf("create %s: %v", body.Kind, err)
	}
	return resp
}

func (h *harness) tap(id string) resp {
	h.t.Helper()
	return h.post("/approve/r/"+id+"/from", nil)
}

// assertionFor runs begin for the action and answers it with the virtual
// authenticator, returning the assertion body.
func (h *harness) assertionFor(id, action string, drops ...string) (body []byte, begin resp) {
	h.t.Helper()
	begin = h.postJSON("/approve/r/"+id+"/begin", map[string]any{"action": action, "drop": drops})
	if begin.Code != http.StatusOK {
		return nil, begin
	}
	var ca protocol.CredentialAssertion
	if err := json.Unmarshal([]byte(begin.Body), &ca); err != nil {
		h.t.Fatal(err)
	}
	if ca.Response.UserVerification != protocol.VerificationRequired {
		h.t.Fatalf("assertion userVerification = %q, want required", ca.Response.UserVerification)
	}
	return h.auth.Assert(&ca), begin
}

// decide runs a full ceremony and returns the finish response.
func (h *harness) decide(id, action string, drops ...string) resp {
	h.t.Helper()
	body, begin := h.assertionFor(id, action, drops...)
	if body == nil {
		return begin
	}
	return h.post("/approve/r/"+id+"/finish", body)
}

// approveSend taps FROM then approves with a passkey.
func (h *harness) approveSend(id string) {
	h.t.Helper()
	if r := h.tap(id); r.Code != http.StatusOK {
		h.t.Fatalf("tap: %d %s", r.Code, r.Body)
	}
	if r := h.decide(id, "approve"); r.Code != http.StatusOK {
		h.t.Fatalf("approve: %d %s", r.Code, r.Body)
	}
}

func canonSHA(t *testing.T, payload []byte) string {
	t.Helper()
	c, err := api.Canonical(payload)
	if err != nil {
		t.Fatal(err)
	}
	return api.SHA256(c)
}

func (h *harness) want(id, kind, payloadSHA string) receipt.Want {
	return receipt.Want{RequestID: id, Kind: kind, PayloadSHA256: payloadSHA, RPID: rpID, Now: h.clock()}
}

// rawAssertion issues an approve challenge straight from the WebAuthn service,
// skipping the begin handler's own checks, and answers it.
func (h *harness) rawAssertion(id, payloadSHA string) []byte {
	h.t.Helper()
	ca, err := h.wa.BeginAssertion(context.Background(), owner, webauthn.Binding{Purpose: webauthn.PurposeApprove, RequestID: id, PayloadSHA256: payloadSHA})
	if err != nil {
		h.t.Fatal(err)
	}
	return h.auth.Assert(ca)
}

// otherAuth is a second authenticator, for finishing a ceremony another began.
type otherAuth struct{ *webauthntest.Authenticator }

func newAuthenticator() otherAuth { return otherAuth{webauthntest.New(rpID, origin)} }

func (o otherAuth) registerFrom(t *testing.T, beginBody string) []byte {
	t.Helper()
	var creation protocol.CredentialCreation
	if err := json.Unmarshal([]byte(beginBody), &creation); err != nil {
		t.Fatal(err)
	}
	return o.Register(&creation)
}
