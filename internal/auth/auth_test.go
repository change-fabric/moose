package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/change-fabric/moose/internal/audit"
	"github.com/change-fabric/moose/internal/store"
	"github.com/change-fabric/moose/internal/store/sqlite"
)

func TestPATRoundTrip(t *testing.T) {
	tok, id, err := GeneratePAT(nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(tok, PATPrefix+id+"_") || len(id) != idLen {
		t.Fatalf("token %q id %q", tok, id)
	}
	got, err := ParsePAT(tok)
	if err != nil || got != id {
		t.Fatalf("ParsePAT = %q, %v", got, err)
	}
	if !VerifyPAT(tok, HashPAT(tok)) || VerifyPAT(tok+"x", HashPAT(tok)) {
		t.Fatal("VerifyPAT mismatch")
	}
	if r := RedactPAT(tok); r != PATPrefix+id+"_***" || strings.Contains(r, tok[len(PATPrefix)+idLen+1:]) {
		t.Fatalf("RedactPAT = %q", r)
	}
}

func TestParsePATSecretWithUnderscore(t *testing.T) {
	tok := PATPrefix + "abcdefgh2345_" + "ab_cd-ef"
	if id, err := ParsePAT(tok); err != nil || id != "abcdefgh2345" {
		t.Fatalf("got %q, %v", id, err)
	}
}

func TestParsePATMalformed(t *testing.T) {
	for _, s := range []string{"", "moose_pat_", "moose_pat_short_x", "ghp_abc", "moose_pat_ABCDEFGH2345_x", "moose_pat_abcdefgh2345_", "moose_pat_abcdefgh2345_!!"} {
		if _, err := ParsePAT(s); err == nil {
			t.Errorf("ParsePAT(%q) accepted", s)
		}
	}
}

func TestNewDevIdentityRefusedInProd(t *testing.T) {
	if _, err := NewDevIdentity("prod", "me@example.com"); err == nil {
		t.Fatal("dev identity allowed in prod")
	}
	if _, err := NewDevIdentity("dev", ""); err == nil {
		t.Fatal("dev identity allowed without login")
	}
}

type fakeWhoIs map[string]Identity

func (f fakeWhoIs) WhoIs(_ context.Context, addr string) (Identity, error) {
	id, ok := f[addr]
	if !ok {
		return Identity{}, ErrNoIdentity
	}
	return id, nil
}

type harness struct {
	t     *testing.T
	st    store.Store
	now   time.Time
	toks  map[string]string // name -> plaintext
	audit []store.AuditEvent
}

const (
	alice   = "alice@example.com"
	bob     = "bob@example.com"
	mallory = "mallory@example.com"
	admin   = "admin@example.com"
	relayS  = "relay-secret-value"
)

func newHarness(t *testing.T) *harness {
	t.Helper()
	ctx := context.Background()
	st, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "moose.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	h := &harness{t: t, st: st, now: time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC), toks: map[string]string{}}
	if err := EnsureBootstrapAdmin(ctx, st, admin, h.now); err != nil {
		t.Fatal(err)
	}
	for _, u := range []store.User{
		{Login: alice, Role: RoleMember, Created: h.now},
		{Login: bob, Role: RoleMember, Created: h.now},
		{Login: mallory, Role: RoleMember, Created: h.now, Disabled: true},
	} {
		if err := st.PutUser(ctx, u); err != nil {
			t.Fatal(err)
		}
	}
	h.mint("alice", alice, DefaultTokenTTL, false)
	h.mint("alice-revoked", alice, DefaultTokenTTL, true)
	h.mint("alice-expired", alice, -1, false) // expiry in the past, set below
	h.mint("ci", "tag:ci", 0, false)
	h.mint("blocked-tag", "tag:other", 0, false)
	h.mint("mallory", mallory, DefaultTokenTTL, false)
	h.mint("ghost", "ghost@example.com", DefaultTokenTTL, false)
	return h
}

func (h *harness) mint(name, owner string, ttl time.Duration, revoked bool) {
	h.t.Helper()
	ctx := context.Background()
	plain, tok, err := NewToken(owner, name, ttl, h.now)
	if err != nil {
		h.t.Fatal(err)
	}
	if ttl < 0 {
		past := h.now.Add(-time.Hour)
		tok.Expires = &past
	}
	if err := h.st.CreateToken(ctx, tok); err != nil {
		h.t.Fatal(err)
	}
	if revoked {
		if err := h.st.RevokeToken(ctx, tok.ID); err != nil {
			h.t.Fatal(err)
		}
	}
	h.toks[name] = plain
}

func (h *harness) AppendAudit(_ context.Context, e store.AuditEvent) error {
	h.audit = append(h.audit, e)
	return nil
}

func (h *harness) handler(src IdentitySource) (http.Handler, *Authenticator) {
	a := &Authenticator{
		Store:         h.st,
		Identity:      src,
		AllowedLogins: []string{admin, alice, bob, mallory, "ghost@example.com"},
		AllowedTags:   []string{"tag:ci"},
		Now:           func() time.Time { return h.now },
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`{"ok":true}`)) })
	mux.Handle("POST /v1/bootstrap/token", a.Bootstrap())
	mux.HandleFunc("GET /v1/whoami", func(w http.ResponseWriter, r *http.Request) {
		p, _ := PrincipalFrom(r.Context())
		_ = json.NewEncoder(w).Encode(p)
	})
	mux.Handle("GET /v1/users", RequireAdmin(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})))
	return audit.Middleware(h, audit.Options{}, a.Middleware(mux)), a
}

func TestMiddleware(t *testing.T) {
	tsnet := TSNetIdentity{Client: fakeWhoIs{
		"100.64.0.1:1": {Login: alice, Node: "alice-laptop"},
		"100.64.0.2:1": {Login: bob, Node: "bob-laptop"},
		"100.64.0.3:1": {Tags: []string{"tag:ci"}, Node: "ci-runner"},
		"100.64.0.4:1": {Login: "eve@example.com", Node: "eve"},
		"100.64.0.5:1": {Tags: []string{"tag:other"}, Node: "other"},
		"100.64.0.6:1": {Login: mallory},
		"100.64.0.7:1": {Login: "ghost@example.com"},
		"100.64.0.8:1": {Login: admin},
	}}
	relay := HeaderIdentity{Secret: []byte(relayS)}

	tests := []struct {
		name       string
		src        IdentitySource
		method     string
		path       string
		remote     string
		token      string // harness token name, or raw value when prefixed "raw:"
		headers    map[string]string
		wantStatus int
		wantCode   string
		wantChall  bool
	}{
		{name: "healthz needs nothing", src: tsnet, path: "/healthz", remote: "203.0.113.9:1", wantStatus: 200},
		{name: "unknown tailnet node 401", src: tsnet, path: "/v1/whoami", remote: "203.0.113.9:1", token: "alice", wantStatus: 401, wantCode: "unauthenticated", wantChall: true},
		{name: "disallowed login 403", src: tsnet, path: "/v1/whoami", remote: "100.64.0.4:1", token: "alice", wantStatus: 403, wantCode: "identity_not_allowed"},
		{name: "disallowed tag 403", src: tsnet, path: "/v1/whoami", remote: "100.64.0.5:1", token: "blocked-tag", wantStatus: 403, wantCode: "identity_not_allowed"},
		{name: "missing token 401 with WWW-Authenticate", src: tsnet, path: "/v1/whoami", remote: "100.64.0.1:1", wantStatus: 401, wantCode: "unauthenticated", wantChall: true},
		{name: "non-bearer scheme 401", src: tsnet, path: "/v1/whoami", remote: "100.64.0.1:1", headers: map[string]string{"Authorization": "Basic Zm9vOmJhcg=="}, wantStatus: 401, wantChall: true},
		{name: "malformed token 401", src: tsnet, path: "/v1/whoami", remote: "100.64.0.1:1", token: "raw:moose_pat_nope", wantStatus: 401, wantChall: true},
		{name: "unknown token id 401", src: tsnet, path: "/v1/whoami", remote: "100.64.0.1:1", token: "raw:moose_pat_aaaaaaaaaaaa_c2VjcmV0", wantStatus: 401, wantCode: "invalid_token", wantChall: true},
		{name: "hash mismatch 401", src: tsnet, path: "/v1/whoami", remote: "100.64.0.1:1", token: "tamper:alice", wantStatus: 401, wantCode: "invalid_token", wantChall: true},
		{name: "revoked 401", src: tsnet, path: "/v1/whoami", remote: "100.64.0.1:1", token: "alice-revoked", wantStatus: 401, wantCode: "invalid_token", wantChall: true},
		{name: "expired 401", src: tsnet, path: "/v1/whoami", remote: "100.64.0.1:1", token: "alice-expired", wantStatus: 401, wantCode: "invalid_token", wantChall: true},
		{name: "owner mismatch 403", src: tsnet, path: "/v1/whoami", remote: "100.64.0.2:1", token: "alice", wantStatus: 403, wantCode: "token_owner_mismatch"},
		{name: "user token from tagged node 403", src: tsnet, path: "/v1/whoami", remote: "100.64.0.3:1", token: "alice", wantStatus: 403, wantCode: "token_owner_mismatch"},
		{name: "tag token from user node 403", src: tsnet, path: "/v1/whoami", remote: "100.64.0.1:1", token: "ci", wantStatus: 403, wantCode: "token_owner_mismatch"},
		{name: "tag-owned token from tagged node 200", src: tsnet, path: "/v1/whoami", remote: "100.64.0.3:1", token: "ci", wantStatus: 200},
		{name: "disabled user 403", src: tsnet, path: "/v1/whoami", remote: "100.64.0.6:1", token: "mallory", wantStatus: 403, wantCode: "user_disabled"},
		{name: "unknown user 403", src: tsnet, path: "/v1/whoami", remote: "100.64.0.7:1", token: "ghost", wantStatus: 403, wantCode: "user_unknown"},
		{name: "valid user token 200", src: tsnet, path: "/v1/whoami", remote: "100.64.0.1:1", token: "alice", wantStatus: 200},
		{name: "admin route as member 403", src: tsnet, path: "/v1/users", remote: "100.64.0.1:1", token: "alice", wantStatus: 403, wantCode: "forbidden"},
		{name: "relay header without secret 401", src: relay, path: "/v1/whoami", token: "alice", headers: map[string]string{HeaderTSUserLogin: alice}, wantStatus: 401, wantCode: "unauthenticated", wantChall: true},
		{name: "relay header wrong secret 401", src: relay, path: "/v1/whoami", token: "alice", headers: map[string]string{HeaderTSUserLogin: alice, HeaderRelaySecret: "nope"}, wantStatus: 401, wantChall: true},
		{name: "relay secret without login 401", src: relay, path: "/v1/whoami", token: "ci", headers: map[string]string{HeaderRelaySecret: relayS}, wantStatus: 401, wantChall: true},
		{name: "relay header with secret 200", src: relay, path: "/v1/whoami", token: "alice", headers: map[string]string{HeaderTSUserLogin: alice, HeaderRelaySecret: relayS}, wantStatus: 200},
		{name: "dev identity 200", src: DevIdentity{Login: alice}, path: "/v1/whoami", token: "alice", wantStatus: 200},
		{name: "bootstrap unknown user 403", src: tsnet, method: "POST", path: BootstrapPath, remote: "100.64.0.7:1", wantStatus: 403, wantCode: "bootstrap_forbidden"},
		{name: "bootstrap tagged node 403", src: tsnet, method: "POST", path: BootstrapPath, remote: "100.64.0.3:1", wantStatus: 403, wantCode: "bootstrap_forbidden"},
		{name: "bootstrap user with token 410", src: tsnet, method: "POST", path: BootstrapPath, remote: "100.64.0.1:1", wantStatus: 410, wantCode: "bootstrap_used"},
		{name: "bootstrap disallowed login 403", src: tsnet, method: "POST", path: BootstrapPath, remote: "100.64.0.4:1", wantStatus: 403, wantCode: "identity_not_allowed"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			srv, a := h.handler(tc.src)
			method := tc.method
			if method == "" {
				method = http.MethodGet
			}
			req := httptest.NewRequest(method, tc.path, nil)
			if tc.remote != "" {
				req.RemoteAddr = tc.remote
			}
			switch {
			case strings.HasPrefix(tc.token, "raw:"):
				req.Header.Set("Authorization", "Bearer "+strings.TrimPrefix(tc.token, "raw:"))
			case strings.HasPrefix(tc.token, "tamper:"):
				req.Header.Set("Authorization", "Bearer "+h.toks[strings.TrimPrefix(tc.token, "tamper:")]+"A")
			case tc.token != "":
				req.Header.Set("Authorization", "Bearer "+h.toks[tc.token])
			}
			for k, v := range tc.headers {
				req.Header.Set(k, v)
			}
			rr := httptest.NewRecorder()
			srv.ServeHTTP(rr, req)
			a.Wait()
			if rr.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d; body %s", rr.Code, tc.wantStatus, rr.Body)
			}
			if got := rr.Header().Get("WWW-Authenticate"); tc.wantChall != (got == `Bearer realm="moose"`) {
				t.Fatalf("WWW-Authenticate = %q, want challenge %v", got, tc.wantChall)
			}
			if tc.wantCode != "" {
				var body map[string]string
				_ = json.Unmarshal(rr.Body.Bytes(), &body)
				if body["error"] != tc.wantCode {
					t.Fatalf("error = %q, want %q", body["error"], tc.wantCode)
				}
			}
			if len(h.audit) != 1 || h.audit[0].Status != tc.wantStatus {
				t.Fatalf("audit = %+v", h.audit)
			}
			if tc.wantStatus == 200 && tc.path == "/v1/whoami" {
				var p Principal
				if err := json.Unmarshal(rr.Body.Bytes(), &p); err != nil || p.TokenID == "" || h.audit[0].Actor != p.Login {
					t.Fatalf("principal %+v err %v audit %+v", p, err, h.audit[0])
				}
			}
			if strings.Contains(rr.Body.String(), "moose_pat_") {
				t.Fatal("response leaked a token")
			}
		})
	}
}

func TestBootstrapFlow(t *testing.T) {
	h := newHarness(t)
	srv, a := h.handler(DevIdentity{Login: admin})

	post := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, BootstrapPath, bytes.NewBufferString(`{"name":"laptop"}`))
		rr := httptest.NewRecorder()
		srv.ServeHTTP(rr, req)
		return rr
	}
	rr := post()
	if rr.Code != http.StatusCreated {
		t.Fatalf("first bootstrap = %d %s", rr.Code, rr.Body)
	}
	var resp BootstrapResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Owner != admin || resp.Name != "laptop" || resp.Expires == nil {
		t.Fatalf("resp = %+v", resp)
	}

	// The issued token authenticates and carries the admin role.
	req := httptest.NewRequest(http.MethodGet, "/v1/users", nil)
	req.Header.Set("Authorization", "Bearer "+resp.Token)
	rr = httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("admin route = %d %s", rr.Code, rr.Body)
	}

	// Bootstrap route returns 410 after the first token.
	if rr := post(); rr.Code != http.StatusGone {
		t.Fatalf("second bootstrap = %d, want 410", rr.Code)
	}

	a.Wait()
	tok, err := h.st.GetToken(context.Background(), resp.ID)
	if err != nil || tok.LastUsed == nil {
		t.Fatalf("token after use: %+v %v", tok, err)
	}
}

func TestEnsureBootstrapAdminOnlyWhenEmpty(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	if err := EnsureBootstrapAdmin(ctx, h.st, "other@example.com", h.now); err != nil {
		t.Fatal(err)
	}
	if _, err := h.st.GetUser(ctx, "other@example.com"); err == nil {
		t.Fatal("bootstrap admin created on a non-empty users table")
	}
	u, err := h.st.GetUser(ctx, admin)
	if err != nil || u.Role != RoleAdmin {
		t.Fatalf("admin = %+v %v", u, err)
	}
}

func TestTouchThrottle(t *testing.T) {
	h := newHarness(t)
	srv, a := h.handler(DevIdentity{Login: alice})
	call := func() {
		req := httptest.NewRequest(http.MethodGet, "/v1/whoami", nil)
		req.Header.Set("Authorization", "Bearer "+h.toks["alice"])
		rr := httptest.NewRecorder()
		srv.ServeHTTP(rr, req)
		if rr.Code != 200 {
			t.Fatalf("status %d", rr.Code)
		}
		a.Wait()
	}
	id, _ := ParsePAT(h.toks["alice"])
	lastUsed := func() time.Time {
		tok, err := h.st.GetToken(context.Background(), id)
		if err != nil || tok.LastUsed == nil {
			t.Fatalf("%+v %v", tok, err)
		}
		return *tok.LastUsed
	}
	call()
	first := lastUsed()
	h.now = h.now.Add(30 * time.Second)
	call()
	if !lastUsed().Equal(first) {
		t.Fatal("touched again within a minute")
	}
	h.now = h.now.Add(time.Minute)
	call()
	if !lastUsed().After(first) {
		t.Fatal("not touched after a minute")
	}
}
