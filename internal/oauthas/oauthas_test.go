package oauthas_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/change-fabric/moose/internal/accounts/envelope"
	"github.com/change-fabric/moose/internal/accounts/store"
	emailsqlite "github.com/change-fabric/moose/internal/accounts/store/sqlite"
	"github.com/change-fabric/moose/internal/approver/api"
	"github.com/change-fabric/moose/internal/approver/receipt"
	"github.com/change-fabric/moose/internal/mcpkit"
	"github.com/change-fabric/moose/internal/oauthas"
)

const (
	testRPID     = "moose.example.org"
	claudeRedir  = "https://claude.ai/api/mcp/auth_callback"
	loopbackRedr = "http://127.0.0.1/callback"
)

// ---- fake approver ----

type fakeReq struct {
	id, kind, sha, status, claimant string
	payload                         []byte
	rcpt                            *receipt.Receipt
	expires                         time.Time
}

// fakeApprover is the moose-approver service API with a test hook that approves
// a request by signing a real receipt.
type fakeApprover struct {
	mu         sync.Mutex
	signer     *receipt.LocalSigner
	now        func() time.Time
	reqs       map[string]*fakeReq
	byKey      map[string]string
	order      []string
	swap       map[string]string // request id -> request id whose receipt Status returns instead
	creates    int
	failCreate bool
}

func newFakeApprover(t *testing.T, now func() time.Time) *fakeApprover {
	t.Helper()
	signer, err := receipt.GenerateLocalSigner()
	if err != nil {
		t.Fatal(err)
	}
	return &fakeApprover{signer: signer, now: now, reqs: map[string]*fakeReq{}, byKey: map[string]string{}, swap: map[string]string{}}
}

func (f *fakeApprover) Create(_ context.Context, in api.CreateBody) (api.CreateResponse, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.creates++
	if f.failCreate {
		return api.CreateResponse{}, false, &api.Error{Status: 503, Code: "down"}
	}
	canon, err := api.Canonical(in.Payload)
	if err != nil {
		return api.CreateResponse{}, false, err
	}
	if id, ok := f.byKey[in.IdempotencyKey]; ok {
		r := f.reqs[id]
		return api.CreateResponse{RequestID: id, PayloadSHA256: r.sha, ApprovalURL: "https://" + testRPID + "/approve/r/" + id, ExpiresAt: r.expires}, false, nil
	}
	id := fmt.Sprintf("req_%d", len(f.order)+1)
	r := &fakeReq{id: id, kind: in.Kind, sha: api.SHA256(canon), status: "pending", payload: canon, expires: f.now().Add(15 * time.Minute)}
	f.reqs[id], f.byKey[in.IdempotencyKey] = r, id
	f.order = append(f.order, id)
	return api.CreateResponse{RequestID: id, PayloadSHA256: r.sha, ApprovalURL: "https://" + testRPID + "/approve/r/" + id, ExpiresAt: r.expires}, true, nil
}

func (f *fakeApprover) Status(_ context.Context, id string) (api.StatusResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.reqs[id]
	if !ok {
		return api.StatusResponse{}, &api.Error{Status: 404, Code: "not_found"}
	}
	out := api.StatusResponse{RequestID: r.id, Kind: r.kind, Status: r.status, PayloadSHA256: r.sha, ExpiresAt: r.expires}
	if r.status == "approved" || r.status == "consumed" {
		out.Receipt = r.rcpt
		if other, ok := f.swap[id]; ok {
			out.Receipt = f.reqs[other].rcpt
		}
	}
	return out, nil
}

func (f *fakeApprover) Consume(ctx context.Context, id, claimant string) (api.StatusResponse, error) {
	f.mu.Lock()
	r, ok := f.reqs[id]
	switch {
	case !ok:
		f.mu.Unlock()
		return api.StatusResponse{}, &api.Error{Status: 404, Code: "not_found"}
	case r.status == "consumed" && r.claimant != claimant:
		f.mu.Unlock()
		return api.StatusResponse{}, api.ErrAlreadyConsumed
	case r.status != "approved" && r.status != "consumed":
		f.mu.Unlock()
		return api.StatusResponse{}, &api.Error{Status: 409, Code: "not_approved"}
	}
	r.status, r.claimant = "consumed", claimant
	f.mu.Unlock()
	return f.Status(ctx, id)
}

// approve signs a receipt for the request as the approver would after a passkey tap.
func (f *fakeApprover) approve(t *testing.T, id string) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	r := f.reqs[id]
	rc, err := receipt.Sign(context.Background(), f.signer, receipt.Body{
		RequestID: r.id, Kind: r.kind, PayloadSHA256: r.sha, ApprovedAt: f.now(), ExpiresAt: r.expires, RPID: testRPID,
	})
	if err != nil {
		t.Fatal(err)
	}
	r.rcpt, r.status = &rc, "approved"
}

func (f *fakeApprover) setStatus(id, status string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reqs[id].status = status
}

func (f *fakeApprover) last() *fakeReq {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.reqs[f.order[len(f.order)-1]]
}

func (f *fakeApprover) payload(t *testing.T, id string) api.DashboardSessionPayload {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	var p api.DashboardSessionPayload
	if err := json.Unmarshal(f.reqs[id].payload, &p); err != nil {
		t.Fatal(err)
	}
	return p
}

// ---- environment ----

type testClock struct{ ns atomic.Int64 }

func (c *testClock) Now() time.Time          { return time.Unix(0, c.ns.Load()).UTC() }
func (c *testClock) Advance(d time.Duration) { c.ns.Add(int64(d)) }

type env struct {
	t     *testing.T
	srv   *httptest.Server
	base  string
	st    store.Store
	ap    *fakeApprover
	as    *oauthas.Server
	clock *testClock
	log   *bytes.Buffer
	logMu sync.Mutex
}

type lockedWriter struct {
	e *env
}

func (w lockedWriter) Write(p []byte) (int, error) {
	w.e.logMu.Lock()
	defer w.e.logMu.Unlock()
	return w.e.log.Write(p)
}

func newEnv(t *testing.T) *env { return newEnvWith(t, nil) }

// newEnvWith builds the environment with the given resources (nil is the
// default, email alone) and serves a protected MCP endpoint at each.
func newEnvWith(t *testing.T, resources []oauthas.Resource) *env {
	t.Helper()
	e := &env{t: t, clock: &testClock{}, log: &bytes.Buffer{}}
	e.clock.ns.Store(time.Now().UnixNano())
	st, err := emailsqlite.OpenStore(context.Background(), filepath.Join(t.TempDir(), "email.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	e.st = st
	e.ap = newFakeApprover(t, e.clock.Now)
	seal, err := envelope.NewLocal([]byte(strings.Repeat("k", 32)))
	if err != nil {
		t.Fatal(err)
	}
	pub, err := e.ap.signer.PublicKeyPEM()
	if err != nil {
		t.Fatal(err)
	}
	var h http.Handler
	e.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { h.ServeHTTP(w, r) }))
	t.Cleanup(e.srv.Close)
	e.base = e.srv.URL
	e.as, err = oauthas.New(oauthas.Deps{
		Store: st, Seal: seal, Approver: e.ap, ReceiptKey: pub, RPID: testRPID, Owner: "pat",
		BaseURL: e.base, InsecureCookies: true, Now: e.clock.Now, Resources: resources,
		Log: slog.New(slog.NewTextHandler(lockedWriter{e}, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.Handle("/", e.as.Handler())
	mux.Handle(oauthas.MCPPath, e.as.Protect(mcpkit.Handler(mcpkit.NewServer("moose-email", "t", nil))))
	for _, r := range e.as.Resources() {
		if r.Path != oauthas.MCPPath {
			mux.Handle(r.Path, e.as.ProtectResource(r.Path, mcpkit.Handler(mcpkit.NewServer("moose-other", "t", nil))))
		}
	}
	h = mux
	return e
}

func noFollow(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

// browser returns a client with its own cookie jar that never follows redirects.
func (e *env) browser() *http.Client {
	jar, _ := cookiejar.New(nil)
	return &http.Client{Jar: jar, CheckRedirect: noFollow}
}

func (e *env) register(redirects ...string) string {
	e.t.Helper()
	return e.registerNamed("Claude", redirects...)
}

func (e *env) registerNamed(name string, redirects ...string) string {
	e.t.Helper()
	if len(redirects) == 0 {
		redirects = []string{claudeRedir, loopbackRedr}
	}
	body, _ := json.Marshal(map[string]any{"client_name": name, "redirect_uris": redirects, "token_endpoint_auth_method": "none"})
	res, err := http.Post(e.base+"/register", "application/json", bytes.NewReader(body))
	if err != nil {
		e.t.Fatal(err)
	}
	defer res.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(res.Body).Decode(&out)
	if res.StatusCode != 201 {
		e.t.Fatalf("register = %d %v", res.StatusCode, out)
	}
	return out["client_id"].(string)
}

func pkce() (verifier, challenge string) {
	verifier = strings.Repeat("v", 20) + base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprint(time.Now().UnixNano())))
	for len(verifier) < 43 {
		verifier += "x"
	}
	sum := sha256.Sum256([]byte(verifier))
	return verifier, base64.RawURLEncoding.EncodeToString(sum[:])
}

type authParams struct {
	clientID, redirect, challenge, state, resource, method string
	omitChallenge                                          bool
}

func (e *env) authorizeURL(p authParams) string {
	q := url.Values{}
	q.Set("response_type", "code")
	q.Set("client_id", p.clientID)
	q.Set("redirect_uri", p.redirect)
	if !p.omitChallenge {
		q.Set("code_challenge", p.challenge)
	}
	if p.method == "" {
		p.method = "S256"
	}
	q.Set("code_challenge_method", p.method)
	if p.state != "" {
		q.Set("state", p.state)
	}
	if p.resource != "" {
		q.Set("resource", p.resource)
	}
	return e.base + "/authorize?" + q.Encode()
}

func (e *env) get(c *http.Client, u string) (*http.Response, string) {
	e.t.Helper()
	res, err := c.Get(u)
	if err != nil {
		e.t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res, string(b)
}

type pollResult struct {
	Status   string `json:"status"`
	Redirect string `json:"redirect"`
}

func (e *env) poll(c *http.Client) (int, pollResult) {
	e.t.Helper()
	res, body := e.get(c, e.base+"/authorize/poll")
	var p pollResult
	_ = json.Unmarshal([]byte(body), &p)
	return res.StatusCode, p
}

// login runs /authorize, approves the dashboard request and polls to the redirect,
// returning the authorization code and the browser's cookie handle.
func (e *env) login(clientID, redirect, challenge string) (code string, b *http.Client) {
	e.t.Helper()
	b = e.browser()
	res, body := e.get(b, e.authorizeURL(authParams{clientID: clientID, redirect: redirect, challenge: challenge, state: "st-1"}))
	if res.StatusCode != 200 {
		e.t.Fatalf("authorize = %d %s", res.StatusCode, body)
	}
	e.ap.approve(e.t, e.ap.last().id)
	status, p := e.poll(b)
	if status != 200 || p.Status != "approved" {
		e.t.Fatalf("poll = %d %+v", status, p)
	}
	u, err := url.Parse(p.Redirect)
	if err != nil {
		e.t.Fatal(err)
	}
	if u.Query().Get("state") != "st-1" || u.Query().Get("iss") != e.base {
		e.t.Fatalf("redirect %s lost state or iss", p.Redirect)
	}
	return u.Query().Get("code"), b
}

type tokenOut struct {
	Access  string `json:"access_token"`
	Type    string `json:"token_type"`
	Expires int64  `json:"expires_in"`
	Refresh string `json:"refresh_token"`
	Scope   string `json:"scope"`
	Error   string `json:"error"`
	Desc    string `json:"error_description"`
}

func (e *env) token(form url.Values) (int, tokenOut) {
	e.t.Helper()
	res, err := http.PostForm(e.base+"/token", form)
	if err != nil {
		e.t.Fatal(err)
	}
	defer res.Body.Close()
	var out tokenOut
	_ = json.NewDecoder(res.Body).Decode(&out)
	return res.StatusCode, out
}

func codeForm(clientID, redirect, code, verifier string) url.Values {
	return url.Values{"grant_type": {"authorization_code"}, "client_id": {clientID}, "redirect_uri": {redirect}, "code": {code}, "code_verifier": {verifier}}
}

func refreshForm(clientID, rt string) url.Values {
	return url.Values{"grant_type": {"refresh_token"}, "client_id": {clientID}, "refresh_token": {rt}}
}

// issue runs the whole login and code exchange and returns the first token pair.
func (e *env) issue() (clientID string, tok tokenOut) {
	e.t.Helper()
	clientID = e.register()
	verifier, challenge := pkce()
	code, _ := e.login(clientID, claudeRedir, challenge)
	status, tok := e.token(codeForm(clientID, claudeRedir, code, verifier))
	if status != 200 {
		e.t.Fatalf("token = %d %+v", status, tok)
	}
	return clientID, tok
}

func (e *env) mcpStatus(token string) int {
	e.t.Helper()
	req, _ := http.NewRequest(http.MethodPost, e.base+"/email/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	res.Body.Close()
	return res.StatusCode
}

func wantStatus(t *testing.T, what string, got, want int) {
	t.Helper()
	if got != want {
		t.Fatalf("%s = %d, want %d", what, got, want)
	}
}

func wantError(t *testing.T, what string, status int, out tokenOut, code string) {
	t.Helper()
	if status != 400 || out.Error != code || out.Access != "" || out.Refresh != "" {
		t.Fatalf("%s = %d %+v, want 400 %s and no tokens", what, status, out, code)
	}
}

// ---- the full flow ----

func TestFullFlowRegisterAuthorizeApproveTokenMCPRefreshReuse(t *testing.T) {
	e := newEnv(t)

	// /mcp is closed without a token, and points at the resource metadata.
	res, err := http.Post(e.base+"/email/mcp", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	wantStatus(t, "unauthenticated /mcp", res.StatusCode, 401)
	if wa := res.Header.Get("WWW-Authenticate"); !strings.Contains(wa, e.base+"/.well-known/oauth-protected-resource/email/mcp") {
		t.Fatalf("WWW-Authenticate = %q, want the resource metadata URL", wa)
	}

	clientID := e.register()
	verifier, challenge := pkce()
	code, _ := e.login(clientID, claudeRedir, challenge)

	status, tok := e.token(codeForm(clientID, claudeRedir, code, verifier))
	wantStatus(t, "token", status, 200)
	if tok.Type != "Bearer" || tok.Expires != 3600 || tok.Scope != "mcp" || tok.Access == "" || tok.Refresh == "" {
		t.Fatalf("token response = %+v", tok)
	}

	// The real MCP client connects with the bearer token.
	cs := e.connectMCP(tok.Access)
	if info := cs.InitializeResult().ServerInfo; info.Name != "moose-email" {
		t.Errorf("server info %+v", info)
	}
	cs.Close()

	// Refresh rotates both tokens.
	status, tok2 := e.token(refreshForm(clientID, tok.Refresh))
	wantStatus(t, "refresh", status, 200)
	if tok2.Access == tok.Access || tok2.Refresh == tok.Refresh || tok2.Expires != 3600 {
		t.Fatalf("refresh did not rotate: %+v", tok2)
	}
	wantStatus(t, "new access token at /mcp", e.mcpStatus(tok2.Access), 200)

	// Reusing the revoked (rotated-away) token after the grace window revokes the family.
	e.clock.Advance(store.RefreshGrace + time.Second)
	status, bad := e.token(refreshForm(clientID, tok.Refresh))
	wantError(t, "reuse after grace", status, bad, "invalid_grant")
	wantStatus(t, "family access token after reuse", e.mcpStatus(tok2.Access), 401)
	status, bad = e.token(refreshForm(clientID, tok2.Refresh))
	wantError(t, "current refresh after family revoke", status, bad, "invalid_grant")
}

func (e *env) connectMCP(token string) *sdk.ClientSession {
	e.t.Helper()
	hc := &http.Client{Transport: bearer{token, http.DefaultTransport}}
	cs, err := sdk.NewClient(&sdk.Implementation{Name: "c", Version: "1"}, nil).
		Connect(context.Background(), &sdk.StreamableClientTransport{Endpoint: e.base + "/email/mcp", HTTPClient: hc}, nil)
	if err != nil {
		e.t.Fatal(err)
	}
	return cs
}

type bearer struct {
	token string
	next  http.RoundTripper
}

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	return b.next.RoundTrip(r)
}

// ---- metadata ----

func TestMetadata(t *testing.T) {
	e := newEnv(t)
	for _, p := range []string{"/.well-known/oauth-protected-resource", "/.well-known/oauth-protected-resource/email/mcp"} {
		res, body := e.get(http.DefaultClient, e.base+p)
		wantStatus(t, p, res.StatusCode, 200)
		var m struct {
			Resource string   `json:"resource"`
			AS       []string `json:"authorization_servers"`
		}
		if err := json.Unmarshal([]byte(body), &m); err != nil || m.Resource != e.base+"/email/mcp" || len(m.AS) != 1 || m.AS[0] != e.base {
			t.Fatalf("%s = %s", p, body)
		}
	}
	res, body := e.get(http.DefaultClient, e.base+"/.well-known/oauth-authorization-server")
	wantStatus(t, "AS metadata", res.StatusCode, 200)
	var m map[string]any
	if err := json.Unmarshal([]byte(body), &m); err != nil {
		t.Fatal(err)
	}
	for k, want := range map[string]string{
		"issuer": e.base, "authorization_endpoint": e.base + "/authorize", "token_endpoint": e.base + "/token",
		"registration_endpoint": e.base + "/register",
	} {
		if m[k] != want {
			t.Errorf("%s = %v, want %s", k, m[k], want)
		}
	}
	if cm, _ := m["code_challenge_methods_supported"].([]any); len(cm) != 1 || cm[0] != "S256" {
		t.Errorf("code_challenge_methods_supported = %v, want only S256", m["code_challenge_methods_supported"])
	}
}

// ---- registration ----

func TestRegisterRejectsUnsafeRedirects(t *testing.T) {
	e := newEnv(t)
	for _, bad := range []string{
		"http://evil.example.org/cb",      // http off loopback
		"myapp://callback",                // custom scheme
		"https://claude.ai/cb#frag",       // fragment
		"https://user:pw@claude.ai/cb",    // credentials
		"/relative/path",                  // not absolute
		"http://[::1]/cb",                 // only 127.0.0.1 and localhost
		"javascript:alert(1)",             // not a redirect
		"https://claude.ai/cb with space", // whitespace
	} {
		body, _ := json.Marshal(map[string]any{"client_name": "x", "redirect_uris": []string{bad}})
		res, err := http.Post(e.base+"/register", "application/json", bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != 400 {
			t.Errorf("register %q = %d, want 400", bad, res.StatusCode)
		}
	}
	for _, body := range []string{`{}`, `{"redirect_uris":[]}`, `not json`,
		`{"redirect_uris":["https://a.example/cb"],"token_endpoint_auth_method":"client_secret_basic"}`,
		`{"redirect_uris":["https://a.example/cb"],"grant_types":["password"]}`} {
		res, err := http.Post(e.base+"/register", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != 400 {
			t.Errorf("register %s = %d, want 400", body, res.StatusCode)
		}
	}
}

// ---- redirect URIs ----

func TestMatchRedirectIsExactExceptLoopbackPorts(t *testing.T) {
	reg := []string{claudeRedir, "http://127.0.0.1/callback", "http://localhost/cb?x=1"}
	for _, ok := range []string{
		claudeRedir,
		"http://127.0.0.1:53124/callback",
		"http://127.0.0.1/callback",
		"http://localhost:8080/cb?x=1",
	} {
		if !oauthas.MatchRedirect(reg, ok) {
			t.Errorf("MatchRedirect(%q) = false, want true", ok)
		}
	}
	for _, bad := range []string{
		claudeRedir + "/",           // suffix
		claudeRedir + "x",           // suffix
		claudeRedir + "?a=b",        // extra query
		"https://claude.ai/api/mcp", // prefix
		"https://claude.ai/api/mcp/auth_callbac",
		"https://claude.ai.evil.example/api/mcp/auth_callback", // different host
		"https://evil.claude.ai/api/mcp/auth_callback",         // different host
		"https://claude.ai@evil.example/api/mcp/auth_callback", // userinfo
		"http://claude.ai/api/mcp/auth_callback",               // other scheme
		"HTTPS://claude.ai/api/mcp/auth_callback",              // not an exact string
		"https://claude.ai:444/api/mcp/auth_callback",          // port on a non-loopback host
		"http://127.0.0.1:53124/callback/",                     // loopback suffix
		"http://127.0.0.1:53124/other",                         // loopback other path
		"http://localhost:53124/callback",                      // other loopback host
		"http://127.0.0.1.evil.example:53124/callback",         // prefix host
		"http://localhost.evil.example:8080/cb?x=1",            // prefix host
		"http://localhost:8080/cb",                             // query dropped
		"http://localhost:8080/cb?x=2",                         // query changed
		"http://127.0.0.1:53124/callback#frag",                 // fragment
		"https://127.0.0.1:53124/callback",                     // https is not the registered scheme
		"http://127.0.0.1:abc/callback",                        // not a port
		"http://user@127.0.0.1:53124/callback",                 // userinfo
		"",
	} {
		if oauthas.MatchRedirect(reg, bad) {
			t.Errorf("MatchRedirect(%q) = true, want false", bad)
		}
	}
}

func TestAuthorizeNeverRedirectsToAnUnregisteredURI(t *testing.T) {
	e := newEnv(t)
	clientID := e.register()
	_, challenge := pkce()
	for _, redir := range []string{claudeRedir + "/x", "https://evil.example/cb", "http://127.0.0.1:1/other", ""} {
		res, _ := e.get(e.browser(), e.authorizeURL(authParams{clientID: clientID, redirect: redir, challenge: challenge}))
		if res.StatusCode != 400 || res.Header.Get("Location") != "" {
			t.Errorf("redirect_uri %q: status %d location %q, want 400 and no redirect", redir, res.StatusCode, res.Header.Get("Location"))
		}
	}
	if n := e.ap.creates; n != 0 {
		t.Errorf("%d approvals were created for rejected requests", n)
	}
	res, _ := e.get(e.browser(), e.authorizeURL(authParams{clientID: "mc_unknown", redirect: claudeRedir, challenge: challenge}))
	wantStatus(t, "unknown client", res.StatusCode, 400)
}

func TestAuthorizeAcceptsLoopbackPortFromRegisteredLoopback(t *testing.T) {
	e := newEnv(t)
	clientID := e.register()
	_, challenge := pkce()
	res, _ := e.get(e.browser(), e.authorizeURL(authParams{clientID: clientID, redirect: "http://127.0.0.1:61234/callback", challenge: challenge}))
	wantStatus(t, "loopback with a port", res.StatusCode, 200)
	if h := e.ap.payload(t, e.ap.last().id).RedirectURIHost; h != "127.0.0.1:61234" {
		t.Errorf("approval shows redirect host %q", h)
	}
}

func TestAuthorizeRejectsBadRequestsByRedirectingTheError(t *testing.T) {
	e := newEnv(t)
	clientID := e.register()
	_, challenge := pkce()
	cases := map[string]authParams{
		"plain pkce":        {clientID: clientID, redirect: claudeRedir, challenge: challenge, method: "plain"},
		"no challenge":      {clientID: clientID, redirect: claudeRedir, omitChallenge: true},
		"short challenge":   {clientID: clientID, redirect: claudeRedir, challenge: "abc"},
		"foreign resource":  {clientID: clientID, redirect: claudeRedir, challenge: challenge, resource: "https://other.example/mcp"},
		"resource w/o path": {clientID: clientID, redirect: claudeRedir, challenge: challenge, resource: e.base},
	}
	for name, p := range cases {
		res, _ := e.get(e.browser(), e.authorizeURL(p))
		loc, err := url.Parse(res.Header.Get("Location"))
		if res.StatusCode != http.StatusSeeOther || err != nil || loc.Host != "claude.ai" || loc.Query().Get("error") == "" {
			t.Errorf("%s: status %d location %q, want a redirect carrying an error", name, res.StatusCode, res.Header.Get("Location"))
		}
	}
	if e.ap.creates != 0 {
		t.Errorf("%d approvals created for bad requests", e.ap.creates)
	}
	u := e.authorizeURL(authParams{clientID: clientID, redirect: claudeRedir, challenge: challenge}) + "&scope=admin"
	res, _ := e.get(e.browser(), u)
	if loc := res.Header.Get("Location"); !strings.Contains(loc, "invalid_scope") {
		t.Errorf("unknown scope: %d %q", res.StatusCode, loc)
	}
	u = e.authorizeURL(authParams{clientID: clientID, redirect: claudeRedir, challenge: challenge}) + "&redirect_uri=https://evil.example/cb"
	res, _ = e.get(e.browser(), u)
	wantStatus(t, "duplicate redirect_uri", res.StatusCode, 400)
}

// ---- authorization code invariants ----

func TestCodeWrongClientRedirectOrVerifierIsRefusedAndBurnsTheCode(t *testing.T) {
	e := newEnv(t)
	clientID := e.register()
	other := e.register()
	verifier, challenge := pkce()

	for name, form := range map[string]func(code string) url.Values{
		"wrong client_id":    func(c string) url.Values { return codeForm(other, claudeRedir, c, verifier) },
		"wrong redirect_uri": func(c string) url.Values { return codeForm(clientID, loopbackRedr, c, verifier) },
		"wrong verifier":     func(c string) url.Values { return codeForm(clientID, claudeRedir, c, strings.Repeat("w", 50)) },
		"short verifier":     func(c string) url.Values { return codeForm(clientID, claudeRedir, c, "short") },
		"wrong resource": func(c string) url.Values {
			f := codeForm(clientID, claudeRedir, c, verifier)
			f.Set("resource", "https://other.example/mcp")
			return f
		},
	} {
		code, _ := e.login(clientID, claudeRedir, challenge)
		status, out := e.token(form(code))
		if status != 400 || out.Access != "" || out.Refresh != "" || (out.Error != "invalid_grant" && out.Error != "invalid_target") {
			t.Errorf("%s = %d %+v, want 400 invalid_grant and no tokens", name, status, out)
		}
		// The code is burned: the right parameters no longer work.
		status, out = e.token(codeForm(clientID, claudeRedir, code, verifier))
		wantError(t, name+": retry with the right parameters", status, out, "invalid_grant")
	}
}

func TestCodeDoubleRedeemFailsAndRevokesTheTokensFromTheFirst(t *testing.T) {
	e := newEnv(t)
	clientID := e.register()
	verifier, challenge := pkce()
	code, _ := e.login(clientID, claudeRedir, challenge)

	status, first := e.token(codeForm(clientID, claudeRedir, code, verifier))
	wantStatus(t, "first redeem", status, 200)
	wantStatus(t, "first access token", e.mcpStatus(first.Access), 200)

	status, second := e.token(codeForm(clientID, claudeRedir, code, verifier))
	wantError(t, "second redeem", status, second, "invalid_grant")
	wantStatus(t, "access token after the replay", e.mcpStatus(first.Access), 401)
	status, out := e.token(refreshForm(clientID, first.Refresh))
	wantError(t, "refresh after the replay", status, out, "invalid_grant")
}

func TestCodeExpiresAfterSixtySecondsAndIsStoredHashed(t *testing.T) {
	e := newEnv(t)
	clientID := e.register()
	verifier, challenge := pkce()

	code, _ := e.login(clientID, claudeRedir, challenge)
	e.clock.Advance(oauthas.CodeTTL - time.Second)
	status, _ := e.token(codeForm(clientID, claudeRedir, code, verifier))
	wantStatus(t, "redeem at 59s", status, 200)

	code, _ = e.login(clientID, claudeRedir, challenge)
	// The store holds only the hash: the raw code is not a key.
	if _, err := e.st.RedeemAuthCode(context.Background(), code, e.clock.Now()); err == nil {
		t.Fatal("the raw code works as a store key; codes must be stored hashed")
	}
	e.clock.Advance(oauthas.CodeTTL + time.Second)
	status, out := e.token(codeForm(clientID, claudeRedir, code, verifier))
	wantError(t, "redeem at 61s", status, out, "invalid_grant")
}

func TestTokenEndpointRefusesMalformedRequests(t *testing.T) {
	e := newEnv(t)
	clientID := e.register()
	verifier, challenge := pkce()
	code, _ := e.login(clientID, claudeRedir, challenge)

	res, err := http.Get(e.base + "/token?" + codeForm(clientID, claudeRedir, code, verifier).Encode())
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	wantStatus(t, "GET /token", res.StatusCode, 405)

	// Parameters in the query string do not count.
	res, err = http.Post(e.base+"/token?"+codeForm(clientID, claudeRedir, code, verifier).Encode(), "application/x-www-form-urlencoded", nil)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	wantStatus(t, "query-only parameters", res.StatusCode, 400)

	status, out := e.token(url.Values{"grant_type": {"password"}, "username": {"a"}, "password": {"b"}})
	if status != 400 || out.Error != "unsupported_grant_type" {
		t.Errorf("password grant = %d %+v", status, out)
	}
	f := codeForm(clientID, claudeRedir, code, verifier)
	f.Add("code", "second")
	status, out = e.token(f)
	if status != 400 || out.Error != "invalid_request" {
		t.Errorf("duplicate code = %d %+v", status, out)
	}
	// The valid code was untouched by those attempts.
	status, _ = e.token(codeForm(clientID, claudeRedir, code, verifier))
	wantStatus(t, "redeem after malformed attempts", status, 200)
}

// ---- resource and audience ----

func TestTokenForAnotherResourceIsRejectedAtMCP(t *testing.T) {
	e := newEnv(t)
	_, tok := e.issue()
	wantStatus(t, "token for this resource", e.mcpStatus(tok.Access), 200)

	raw := "mat_minted-for-another-resource"
	err := e.st.CreateSession(context.Background(), store.Session{
		Hash: store.HashSecret(raw), Kind: store.SessionAccess, Owner: "pat", ClientID: "c",
		Audience: "https://other.example/mcp", Scope: "mcp", Created: time.Now(), Expires: time.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	wantStatus(t, "token minted for another resource", e.mcpStatus(raw), 401)

	noAud := "mat_no-audience"
	_ = e.st.CreateSession(context.Background(), store.Session{
		Hash: store.HashSecret(noAud), Kind: store.SessionAccess, Owner: "pat", Scope: "mcp",
		Created: time.Now(), Expires: time.Now().Add(time.Hour),
	})
	wantStatus(t, "token with no audience", e.mcpStatus(noAud), 401)

	dash := "dashboard-cookie-value"
	_ = e.st.CreateSession(context.Background(), store.Session{
		Hash: store.HashSecret(dash), Kind: store.SessionDashboard, Owner: "pat", Audience: e.base + "/email/mcp",
		Scope: "mcp", Created: time.Now(), Expires: time.Now().Add(time.Hour),
	})
	wantStatus(t, "a dashboard session used as a bearer token", e.mcpStatus(dash), 401)

	expired := "mat_expired"
	_ = e.st.CreateSession(context.Background(), store.Session{
		Hash: store.HashSecret(expired), Kind: store.SessionAccess, Owner: "pat", Audience: e.base + "/email/mcp",
		Scope: "mcp", Created: time.Now().Add(-2 * time.Hour), Expires: time.Now().Add(-time.Hour),
	})
	wantStatus(t, "expired token", e.mcpStatus(expired), 401)
	wantStatus(t, "garbage token", e.mcpStatus("nope"), 401)
	wantStatus(t, "no token", e.mcpStatus(""), 401)
}

func TestResourceIsBoundThroughTheGrant(t *testing.T) {
	e := newEnv(t)
	clientID := e.register()
	verifier, challenge := pkce()

	// An explicit, correct resource works and is what the token is audienced to.
	b := e.browser()
	res, body := e.get(b, e.authorizeURL(authParams{clientID: clientID, redirect: claudeRedir, challenge: challenge, resource: e.base + "/email/mcp"}))
	if res.StatusCode != 200 {
		t.Fatalf("authorize with the right resource = %d %s", res.StatusCode, body)
	}
	e.ap.approve(t, e.ap.last().id)
	_, p := e.poll(b)
	u, _ := url.Parse(p.Redirect)
	f := codeForm(clientID, claudeRedir, u.Query().Get("code"), verifier)
	f.Set("resource", e.base+"/email/mcp")
	status, tok := e.token(f)
	wantStatus(t, "token with the matching resource", status, 200)
	sess, err := e.st.GetSession(context.Background(), store.HashSecret(tok.Access))
	if err != nil || sess.Audience != e.base+"/email/mcp" {
		t.Fatalf("session audience = %q, %v", sess.Audience, err)
	}

	// A refresh naming another resource is refused.
	f = refreshForm(clientID, tok.Refresh)
	f.Set("resource", "https://other.example/mcp")
	status, out := e.token(f)
	if status != 400 || out.Error != "invalid_target" {
		t.Errorf("refresh with a foreign resource = %d %+v", status, out)
	}
}

// ---- browser transaction binding ----

func TestPollNeedsTheTransactionCookie(t *testing.T) {
	e := newEnv(t)
	clientID := e.register()
	_, challenge := pkce()
	owner := e.browser()
	res, _ := e.get(owner, e.authorizeURL(authParams{clientID: clientID, redirect: claudeRedir, challenge: challenge}))
	wantStatus(t, "authorize", res.StatusCode, 200)
	e.ap.approve(t, e.ap.last().id)

	// No cookie: nothing.
	res, body := e.get(e.browser(), e.base+"/authorize/poll")
	if res.StatusCode != 404 || strings.Contains(body, "code=") || strings.Contains(body, "redirect\"") {
		t.Fatalf("poll with no cookie = %d %s", res.StatusCode, body)
	}
	// A wrong cookie: nothing.
	req, _ := http.NewRequest("GET", e.base+"/authorize/poll", nil)
	req.AddCookie(&http.Cookie{Name: "moose_oauth_txn", Value: "guess"})
	r2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b2, _ := io.ReadAll(r2.Body)
	r2.Body.Close()
	if r2.StatusCode != 404 || strings.Contains(string(b2), "code=") {
		t.Fatalf("poll with a wrong cookie = %d %s", r2.StatusCode, b2)
	}
	// The approver is not even asked for a request the poller cannot name.
	status, p := e.poll(owner)
	if status != 200 || p.Status != "approved" || p.Redirect == "" {
		t.Fatalf("the owning browser's poll = %d %+v", status, p)
	}
	// A transaction completes once.
	status, _ = e.poll(owner)
	wantStatus(t, "second poll after completion", status, 404)
}

func TestTransactionCookieAttributes(t *testing.T) {
	e := newEnv(t)
	clientID := e.register()
	_, challenge := pkce()
	secure, err := oauthas.New(oauthas.Deps{
		Store: e.st, Seal: mustSealer(t), Approver: e.ap, ReceiptKey: mustPub(t, e.ap), RPID: testRPID, Owner: "pat",
		BaseURL: "https://moose.example.org", Now: e.clock.Now,
	})
	if err != nil {
		t.Fatal(err)
	}
	// A client registered through the first server is visible to the second: same store.
	rec := httptest.NewRecorder()
	q := url.Values{"response_type": {"code"}, "client_id": {clientID}, "redirect_uri": {claudeRedir},
		"code_challenge": {challenge}, "code_challenge_method": {"S256"}}
	secure.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "https://moose.example.org/authorize?"+q.Encode(), nil))
	if rec.Code != 200 {
		t.Fatalf("authorize = %d %s", rec.Code, rec.Body)
	}
	var c *http.Cookie
	for _, k := range rec.Result().Cookies() {
		if k.Name == "moose_oauth_txn" {
			c = k
		}
	}
	if c == nil {
		t.Fatal("no transaction cookie")
	}
	if !c.HttpOnly || !c.Secure || c.SameSite != http.SameSiteLaxMode || c.Path != "/authorize" || c.MaxAge <= 0 {
		t.Fatalf("cookie = %+v, want HttpOnly, Secure, SameSite=Lax, scoped to /authorize and expiring", c)
	}
	if len(c.Value) < 40 {
		t.Fatalf("polling handle %q is too short to be a secret", c.Value)
	}
	if got := e.ap.payload(t, e.ap.last().id).TxnHash; got != store.HashSecret(c.Value) {
		t.Fatalf("payload txn_hash = %s, want sha256(handle)", got)
	}
	if strings.Contains(rec.Body.String(), c.Value) {
		t.Fatal("the polling handle is rendered into the page")
	}
}

func TestReceiptForAnotherTransactionIsRejected(t *testing.T) {
	e := newEnv(t)
	clientID := e.register()
	_, challenge := pkce()
	a, b := e.browser(), e.browser()
	e.get(a, e.authorizeURL(authParams{clientID: clientID, redirect: claudeRedir, challenge: challenge}))
	idA := e.ap.last().id
	e.get(b, e.authorizeURL(authParams{clientID: clientID, redirect: claudeRedir, challenge: challenge}))
	idB := e.ap.last().id
	if idA == idB {
		t.Fatal("two transactions share one approval")
	}
	pa, pb := e.ap.payload(t, idA), e.ap.payload(t, idB)
	if pa.TxnHash == pb.TxnHash {
		t.Fatal("two transactions share a txn_hash")
	}

	// Only B was approved, but the approver (or an attacker in front of it)
	// answers A's status with B's receipt.
	e.ap.approve(t, idB)
	e.ap.approve(t, idA)
	e.ap.mu.Lock()
	e.ap.swap[idA] = idB
	e.ap.mu.Unlock()
	status, p := e.poll(a)
	if status != 403 || p.Status != "rejected" || p.Redirect != "" {
		t.Fatalf("poll with another transaction's receipt = %d %+v", status, p)
	}
	// A receipt that is not even signed by the pinned key is rejected too.
	other, _ := receipt.GenerateLocalSigner()
	e.ap.mu.Lock()
	delete(e.ap.swap, idA)
	rc, _ := receipt.Sign(context.Background(), other, receipt.Body{
		RequestID: idA, Kind: api.KindDashboardSession, PayloadSHA256: e.ap.reqs[idA].sha,
		ApprovedAt: e.clock.Now(), ExpiresAt: e.clock.Now().Add(time.Minute), RPID: testRPID,
	})
	e.ap.reqs[idA].rcpt = &rc
	e.ap.mu.Unlock()
	status, p = e.poll(a)
	if status != 403 || p.Redirect != "" {
		t.Fatalf("poll with a receipt from another key = %d %+v", status, p)
	}
	// A receipt for the wrong rp_id or an expired one is rejected as well.
	e.ap.mu.Lock()
	rc, _ = receipt.Sign(context.Background(), e.ap.signer, receipt.Body{
		RequestID: idA, Kind: api.KindDashboardSession, PayloadSHA256: e.ap.reqs[idA].sha,
		ApprovedAt: e.clock.Now(), ExpiresAt: e.clock.Now().Add(time.Minute), RPID: "evil.example",
	})
	e.ap.reqs[idA].rcpt = &rc
	e.ap.mu.Unlock()
	if status, p = e.poll(a); status != 403 || p.Redirect != "" {
		t.Fatalf("poll with a receipt for another rp_id = %d %+v", status, p)
	}
	// B's own browser completes with B's own receipt.
	status, p = e.poll(b)
	if status != 200 || p.Status != "approved" {
		t.Fatalf("B's poll = %d %+v", status, p)
	}
}

func TestPollWhilePendingDeniedAndExpired(t *testing.T) {
	e := newEnv(t)
	clientID := e.register()
	_, challenge := pkce()

	b := e.browser()
	e.get(b, e.authorizeURL(authParams{clientID: clientID, redirect: claudeRedir, challenge: challenge}))
	if status, p := e.poll(b); status != 200 || p.Status != "pending" || p.Redirect != "" {
		t.Fatalf("pending poll = %d %+v", status, p)
	}
	e.ap.setStatus(e.ap.last().id, "denied")
	if status, p := e.poll(b); status != 200 || p.Status != "denied" || p.Redirect != "" {
		t.Fatalf("denied poll = %d %+v", status, p)
	}
	if status, _ := e.poll(b); status != 404 {
		t.Fatalf("poll after denial = %d, want 404", status)
	}

	b = e.browser()
	e.get(b, e.authorizeURL(authParams{clientID: clientID, redirect: claudeRedir, challenge: challenge}))
	e.clock.Advance(oauthas.TxnTTL + time.Second)
	if status, _ := e.poll(b); status != 404 {
		t.Fatalf("poll after the transaction expired = %d, want 404", status)
	}

	// A request the approver already consumed for someone else cannot complete.
	b = e.browser()
	e.get(b, e.authorizeURL(authParams{clientID: clientID, redirect: claudeRedir, challenge: challenge}))
	id := e.ap.last().id
	e.ap.approve(t, id)
	e.ap.setStatus(id, "consumed")
	if status, p := e.poll(b); status != 409 || p.Redirect != "" {
		t.Fatalf("poll of a consumed approval = %d %+v", status, p)
	}
}

func TestAuthorizeFailsClosedWhenTheApproverIsDown(t *testing.T) {
	e := newEnv(t)
	clientID := e.register()
	_, challenge := pkce()
	e.ap.failCreate = true
	b := e.browser()
	res, _ := e.get(b, e.authorizeURL(authParams{clientID: clientID, redirect: claudeRedir, challenge: challenge}))
	wantStatus(t, "authorize with the approver down", res.StatusCode, 502)
	for _, c := range res.Cookies() {
		if c.Name == "moose_oauth_txn" {
			t.Fatal("a transaction cookie was set although no approval exists")
		}
	}
}

func TestConsentPayloadAndPageShowTheClient(t *testing.T) {
	e := newEnv(t)
	clientID := e.registerNamed("<script>alert(1)</script>  Evil‮ Client", claudeRedir)
	_, challenge := pkce()
	res, body := e.get(e.browser(), e.authorizeURL(authParams{clientID: clientID, redirect: claudeRedir, challenge: challenge}))
	wantStatus(t, "authorize", res.StatusCode, 200)

	pl := e.ap.payload(t, e.ap.last().id)
	if pl.Owner != "pat" || pl.ClientID != clientID || pl.RedirectURIHost != "claude.ai" || pl.TxnHash == "" {
		t.Fatalf("payload = %+v", pl)
	}
	if strings.ContainsRune(pl.ClientName, '‮') {
		t.Errorf("client name %q keeps a bidi control character", pl.ClientName)
	}
	if strings.Contains(body, "<script>alert(1)") {
		t.Fatal("the client name is rendered unescaped")
	}
	if !strings.Contains(body, "&lt;script&gt;") || !strings.Contains(body, "claude.ai") {
		t.Errorf("page does not show the escaped client name and redirect host:\n%s", body)
	}
	csp := res.Header.Get("Content-Security-Policy")
	if !strings.Contains(csp, "script-src 'nonce-") || strings.Contains(csp, "script-src 'unsafe-inline'") {
		t.Errorf("CSP = %q", csp)
	}
	if !strings.Contains(body, "<svg") || !strings.Contains(body, "/approve/r/req_1") {
		t.Errorf("page lacks the QR code or the approval link")
	}
	if res.Header.Get("Cache-Control") != "no-store" {
		t.Errorf("Cache-Control = %q", res.Header.Get("Cache-Control"))
	}
}

// ---- lifetimes and refresh rotation ----

func TestTokenLifetimes(t *testing.T) {
	e := newEnv(t)
	clientID, tok := e.issue()
	if tok.Expires != 3600 {
		t.Errorf("expires_in = %d, want 3600", tok.Expires)
	}
	sess, err := e.st.GetSession(context.Background(), store.HashSecret(tok.Access))
	if err != nil {
		t.Fatal(err)
	}
	if d := sess.Expires.Sub(sess.Created); d != time.Hour {
		t.Errorf("access token lifetime = %v", d)
	}
	famID, _, _ := strings.Cut(tok.Refresh, ".")
	fam, err := e.st.GetRefreshFamily(context.Background(), famID)
	if err != nil {
		t.Fatal(err)
	}
	if d := fam.IdleExpires.Sub(fam.Created); d != 30*24*time.Hour {
		t.Errorf("refresh idle lifetime = %v, want 30 days", d)
	}
	if d := fam.AbsExpires.Sub(fam.Created); d != 90*24*time.Hour {
		t.Errorf("refresh absolute lifetime = %v, want 90 days", d)
	}
	if fam.CurrentHash != store.HashSecret(tok.Refresh) || fam.ClientID != clientID {
		t.Errorf("family = %+v", fam)
	}

	// The access token stops working at an hour (verified against the server clock).
	e.clock.Advance(time.Hour + time.Second)
	if _, err := e.st.GetSession(context.Background(), store.HashSecret(tok.Access)); err != nil {
		t.Fatal(err)
	}

	// 30 days idle ends the family; keeping it warm hits the 90 day ceiling.
	e.clock.Advance(30 * 24 * time.Hour)
	status, out := e.token(refreshForm(clientID, tok.Refresh))
	wantError(t, "refresh after 30 idle days", status, out, "invalid_grant")
}

func TestRefreshAbsoluteLifetime(t *testing.T) {
	e := newEnv(t)
	clientID, tok := e.issue()
	rt := tok.Refresh
	for i := 0; i < 3; i++ {
		e.clock.Advance(25 * 24 * time.Hour) // under the idle window each time
		status, out := e.token(refreshForm(clientID, rt))
		wantStatus(t, fmt.Sprintf("refresh at day %d", 25*(i+1)), status, 200)
		rt = out.Refresh
	}
	e.clock.Advance(25 * 24 * time.Hour) // day 100, past the 90 day ceiling
	status, out := e.token(refreshForm(clientID, rt))
	wantError(t, "refresh past the absolute lifetime", status, out, "invalid_grant")
}

func TestConcurrentRefreshOneWinsLoserGetsTheWinnersPairOnce(t *testing.T) {
	e := newEnv(t)
	clientID, tok := e.issue()

	const n = 2
	outs := make([]tokenOut, n)
	codes := make([]int, n)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			codes[i], outs[i] = e.token(refreshForm(clientID, tok.Refresh))
		}()
	}
	close(start)
	wg.Wait()
	for i := range n {
		if codes[i] != 200 || outs[i].Access == "" || outs[i].Refresh == "" {
			t.Fatalf("refresh %d = %d %+v; both concurrent refreshes should succeed", i, codes[i], outs[i])
		}
	}
	if outs[0].Refresh != outs[1].Refresh || outs[0].Access != outs[1].Access {
		t.Fatalf("the loser must get the winner's pair, got %+v and %+v", outs[0], outs[1])
	}
	// The family is intact, and the shared pair is live.
	wantStatus(t, "the shared access token", e.mcpStatus(outs[0].Access), 200)
	status, next := e.token(refreshForm(clientID, outs[0].Refresh))
	wantStatus(t, "rotating the shared refresh token", status, 200)
	if next.Refresh == outs[0].Refresh {
		t.Fatal("refresh token did not rotate")
	}
}

func TestRefreshReplayWithinGraceIsServedOnceThenRevokes(t *testing.T) {
	e := newEnv(t)
	clientID, tok := e.issue()
	status, win := e.token(refreshForm(clientID, tok.Refresh))
	wantStatus(t, "winner", status, 200)

	e.clock.Advance(5 * time.Second)
	status, replay := e.token(refreshForm(clientID, tok.Refresh))
	wantStatus(t, "replay inside the grace window", status, 200)
	if replay.Refresh != win.Refresh || replay.Access != win.Access {
		t.Fatalf("replay got %+v, want the winner's pair %+v", replay, win)
	}

	status, again := e.token(refreshForm(clientID, tok.Refresh))
	wantError(t, "second replay", status, again, "invalid_grant")
	wantStatus(t, "winner's access token after the family revoke", e.mcpStatus(win.Access), 401)
	status, out := e.token(refreshForm(clientID, win.Refresh))
	wantError(t, "winner's refresh token after the family revoke", status, out, "invalid_grant")
}

func TestRefreshReuseAfterGraceWindowRevokesTheFamily(t *testing.T) {
	e := newEnv(t)
	clientID, tok := e.issue()
	status, win := e.token(refreshForm(clientID, tok.Refresh))
	wantStatus(t, "winner", status, 200)

	e.clock.Advance(store.RefreshGrace + time.Second)
	status, out := e.token(refreshForm(clientID, tok.Refresh))
	wantError(t, "reuse after the grace window", status, out, "invalid_grant")
	wantStatus(t, "access token of the revoked family", e.mcpStatus(win.Access), 401)
	status, out = e.token(refreshForm(clientID, win.Refresh))
	wantError(t, "current refresh token of the revoked family", status, out, "invalid_grant")
}

func TestRefreshRefusesAnotherClientAndGarbage(t *testing.T) {
	e := newEnv(t)
	clientID, tok := e.issue()
	other := e.register()
	status, out := e.token(refreshForm(other, tok.Refresh))
	wantError(t, "another client's refresh", status, out, "invalid_grant")
	for _, rt := range []string{"garbage", "fam_unknown.secret", ".x", tok.Refresh + "x"} {
		status, out = e.token(refreshForm(clientID, rt))
		wantError(t, "refresh "+rt, status, out, "invalid_grant")
	}
	// A forged secret on a real family revokes it, like any other reuse.
	status, out = e.token(refreshForm(clientID, tok.Refresh+"x"))
	wantError(t, "forged secret", status, out, "invalid_grant")
	status, out = e.token(refreshForm(clientID, tok.Refresh))
	wantError(t, "genuine token after a forged presentation", status, out, "invalid_grant")
}

// ---- logging ----

func TestLogsHoldNoSecrets(t *testing.T) {
	e := newEnv(t)
	clientID := e.register()
	verifier, challenge := pkce()
	code, b := e.login(clientID, claudeRedir, challenge)
	var handle string
	u, _ := url.Parse(e.base + "/authorize")
	for _, c := range b.Jar.Cookies(u) {
		handle = c.Value
	}
	_, tok := e.token(codeForm(clientID, claudeRedir, code, verifier))
	e.token(codeForm(clientID, claudeRedir, code, verifier)) // replay: logs an error path
	e.clock.Advance(store.RefreshGrace + time.Second)
	e.token(refreshForm(clientID, tok.Refresh))

	e.logMu.Lock()
	logs := e.log.String()
	e.logMu.Unlock()
	for name, secret := range map[string]string{"code": code, "verifier": verifier, "access token": tok.Access,
		"refresh token": tok.Refresh, "polling handle": handle, "approval url": "/approve/r/"} {
		if secret != "" && strings.Contains(logs, secret) {
			t.Errorf("logs contain the %s", name)
		}
	}
}

func mustSealer(t *testing.T) envelope.Sealer {
	t.Helper()
	s, err := envelope.NewLocal([]byte(strings.Repeat("k", 32)))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func mustPub(t *testing.T, f *fakeApprover) []byte {
	t.Helper()
	pub, err := f.signer.PublicKeyPEM()
	if err != nil {
		t.Fatal(err)
	}
	return pub
}
