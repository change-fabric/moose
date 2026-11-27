package connect_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/oauth2"

	"github.com/change-fabric/moose/internal/accounts/connect"
	"github.com/change-fabric/moose/internal/accounts/envelope"
	"github.com/change-fabric/moose/internal/accounts/palette"
	"github.com/change-fabric/moose/internal/accounts/store"
	"github.com/change-fabric/moose/internal/accounts/store/sqlite"
	"github.com/change-fabric/moose/internal/email/provider"
)

const owner = "pat"

type fakeApprover struct {
	mu      sync.Mutex
	reqs    []connect.ApprovalRequest
	baseURL string
	err     error
	url     string // overrides the approval url
	badSHA  bool
}

func (f *fakeApprover) CreateRequest(_ context.Context, r connect.ApprovalRequest) (connect.ApprovalResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return connect.ApprovalResponse{}, f.err
	}
	f.reqs = append(f.reqs, r)
	sum := sha256.Sum256(r.Payload)
	sha := hex.EncodeToString(sum[:])
	if f.badSHA {
		sha = "deadbeef"
	}
	u := f.baseURL + "/approve/r/req-" + fmt.Sprint(len(f.reqs))
	if f.url != "" {
		u = f.url
	}
	return connect.ApprovalResponse{RequestID: "req-" + fmt.Sprint(len(f.reqs)), PayloadSHA256: sha, ApprovalURL: u, ExpiresAt: time.Now().Add(15 * time.Minute)}, nil
}

type rig struct {
	t        *testing.T
	app      *httptest.Server
	google   *httptest.Server
	st       store.Store
	seal     envelope.Sealer
	approver *fakeApprover
	client   *http.Client

	mu        sync.Mutex
	codes     map[string]string // auth code -> address
	scope     string            // scope the token endpoint reports ("-" omits it)
	noRefresh bool
	exchanges []url.Values
	next      int
}

func newRig(t *testing.T) *rig { return newRigCalendar(t, true) }

// newRigCalendar is newRig with the calendar service switched on or off.
func newRigCalendar(t *testing.T, calendar bool) *rig {
	t.Helper()
	r := &rig{t: t, codes: map[string]string{}, scope: connect.ScopeReadonly + " " + connect.ScopeCompose}
	r.google = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		_ = req.ParseForm()
		r.mu.Lock()
		defer r.mu.Unlock()
		r.exchanges = append(r.exchanges, req.PostForm)
		addr, ok := r.codes[req.PostForm.Get("code")]
		if !ok {
			http.Error(w, `{"error":"invalid_grant"}`, http.StatusBadRequest)
			return
		}
		body := map[string]any{"access_token": "at|" + addr, "token_type": "Bearer", "expires_in": 3600}
		if !r.noRefresh {
			body["refresh_token"] = "1//refresh-for-" + addr
		}
		if r.scope != "-" {
			body["scope"] = r.scope
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(body)
	}))
	t.Cleanup(r.google.Close)

	b, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "email.db"))
	if err != nil {
		t.Fatal(err)
	}
	r.st = store.New(b)
	t.Cleanup(func() { _ = r.st.Close() })
	if r.seal, err = envelope.NewLocal([]byte(strings.Repeat("k", 32))); err != nil {
		t.Fatal(err)
	}
	r.approver = &fakeApprover{}

	mux := http.NewServeMux()
	r.app = httptest.NewServer(mux)
	t.Cleanup(r.app.Close)
	r.approver.baseURL = r.app.URL

	cfg := connect.GoogleConfig("client-id", "client-secret", r.app.URL)
	cfg.Endpoint = oauth2.Endpoint{AuthURL: "https://accounts.example/auth", TokenURL: r.google.URL + "/token", AuthStyle: oauth2.AuthStyleInParams}
	srv, err := connect.New(connect.Deps{
		Store: r.st, Seal: r.seal, OAuth: cfg, BaseURL: r.app.URL, InsecureCookies: true,
		Approver: r.approver, Auth: connect.SessionAuth{Store: r.st}, CalendarEnabled: calendar,
		Profile: func(ctx context.Context, ts oauth2.TokenSource) (connect.Profile, error) {
			tok, err := ts.Token()
			if err != nil {
				return connect.Profile{}, err
			}
			addr := strings.TrimPrefix(tok.AccessToken, "at|")
			return connect.Profile{Address: addr, DisplayName: "Name of " + addr,
				Caps: provider.Capabilities{Threads: true, Labels: true, SearchDialect: provider.DialectGmail}}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	mux.Handle(connect.Prefix, srv.Handler())

	jar, _ := cookiejar.New(nil)
	r.client = &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	r.login(owner)
	return r
}

// login installs a dashboard session cookie for who.
func (r *rig) login(who string) {
	r.t.Helper()
	token := "session-token-" + who
	err := r.st.CreateSession(context.Background(), store.Session{
		Hash: store.HashSecret(token), Kind: store.SessionDashboard, Owner: who,
		Created: time.Now(), Expires: time.Now().Add(time.Hour)})
	if err != nil && !errors.Is(err, store.ErrConflict) {
		r.t.Fatal(err)
	}
	u, _ := url.Parse(r.app.URL)
	r.client.Jar.SetCookies(u, []*http.Cookie{{Name: connect.SessionCookie, Value: token, Path: "/"}})
}

func (r *rig) logout() {
	u, _ := url.Parse(r.app.URL)
	r.client.Jar.SetCookies(u, []*http.Cookie{{Name: connect.SessionCookie, Value: "", Path: "/", MaxAge: -1}})
}

func (r *rig) get(path string) (*http.Response, string) {
	r.t.Helper()
	res, err := r.client.Get(r.app.URL + path)
	if err != nil {
		r.t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res, string(b)
}

func (r *rig) csrf() string {
	r.t.Helper()
	r.get(connect.PagePath)
	u, _ := url.Parse(r.app.URL + connect.PagePath)
	for _, c := range r.client.Jar.Cookies(u) {
		if c.Name == "moose_csrf" {
			return c.Value
		}
	}
	r.t.Fatal("page set no csrf cookie")
	return ""
}

// post submits a form with a valid CSRF token unless form already carries one.
func (r *rig) post(path string, form url.Values) (*http.Response, string) {
	r.t.Helper()
	if form == nil {
		form = url.Values{}
	}
	if !form.Has("csrf") {
		form.Set("csrf", r.csrf())
	}
	return r.postRaw(path, form, nil)
}

func (r *rig) postRaw(path string, form url.Values, hdr map[string]string) (*http.Response, string) {
	r.t.Helper()
	req, _ := http.NewRequest("POST", r.app.URL+path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	res, err := r.client.Do(req)
	if err != nil {
		r.t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res, string(b)
}

func (r *rig) wantStatus(res *http.Response, want int) {
	r.t.Helper()
	if res.StatusCode != want {
		r.t.Fatalf("%s %s = %d, want %d", res.Request.Method, res.Request.URL.Path, res.StatusCode, want)
	}
}

// startConnect posts the start form and returns the state Google would echo back.
func (r *rig) startConnect(form url.Values) (state string, loc *url.URL) {
	r.t.Helper()
	res, body := r.post(connect.StartPath, form)
	r.wantStatus(res, http.StatusSeeOther)
	loc, err := url.Parse(res.Header.Get("Location"))
	if err != nil || loc.Host != "accounts.example" {
		r.t.Fatalf("start redirected to %q (%s)", res.Header.Get("Location"), body)
	}
	return loc.Query().Get("state"), loc
}

// connectAccount runs the whole flow for address and returns the callback response.
func (r *rig) connectAccount(address string, form url.Values) (*http.Response, string) {
	r.t.Helper()
	state, _ := r.startConnect(form)
	r.mu.Lock()
	r.next++
	code := fmt.Sprintf("code-%d", r.next)
	r.codes[code] = address
	r.mu.Unlock()
	return r.callback(state, code)
}

func (r *rig) callback(state, code string) (*http.Response, string) {
	return r.get(connect.CallbackPath + "?" + url.Values{"state": {state}, "code": {code}}.Encode())
}

func (r *rig) accounts() []store.Account {
	r.t.Helper()
	a, err := r.st.ListAccounts(context.Background(), owner)
	if err != nil {
		r.t.Fatal(err)
	}
	return a
}

func (r *rig) seed(nick, addr string, color int, status string) store.Account {
	r.t.Helper()
	a := store.Account{Owner: owner, ID: store.NewAccountID(), Provider: store.ProviderGoogle, ProviderUID: addr, Address: addr, Nickname: nick,
		ColorIndex: color, Status: status, HourlyCap: store.DefaultHourlyCap, Created: time.Now().Add(time.Duration(color) * time.Second),
		RefreshToken: []byte("sealed")}
	if err := r.st.CreateAccount(context.Background(), a); err != nil {
		r.t.Fatal(err)
	}
	return a
}

func TestEveryRouteNeedsADashboardSession(t *testing.T) {
	r := newRig(t)
	acct := r.seed("Work", "w@x.example", 0, store.StatusOK)
	csrf := r.csrf()
	hit := func(name string, do func() *http.Response) {
		res := do()
		signIn := res.StatusCode == http.StatusSeeOther && res.Header.Get("Location") == connect.LoginPath
		if res.StatusCode != http.StatusUnauthorized && !signIn {
			t.Errorf("%s without a session = %d %q, want 401 or a redirect to sign-in", name, res.StatusCode, res.Header.Get("Location"))
		}
	}
	for _, how := range []string{"no cookie", "access token kind", "revoked", "expired"} {
		r.logout()
		u, _ := url.Parse(r.app.URL)
		switch how {
		case "access token kind":
			_ = r.st.CreateSession(context.Background(), store.Session{Hash: store.HashSecret("acc"), Kind: store.SessionAccess, Owner: owner, Expires: time.Now().Add(time.Hour)})
			r.client.Jar.SetCookies(u, []*http.Cookie{{Name: connect.SessionCookie, Value: "acc", Path: "/"}})
		case "revoked":
			_ = r.st.CreateSession(context.Background(), store.Session{Hash: store.HashSecret("rev"), Kind: store.SessionDashboard, Owner: owner, Expires: time.Now().Add(time.Hour)})
			_ = r.st.RevokeSession(context.Background(), store.HashSecret("rev"))
			r.client.Jar.SetCookies(u, []*http.Cookie{{Name: connect.SessionCookie, Value: "rev", Path: "/"}})
		case "expired":
			_ = r.st.CreateSession(context.Background(), store.Session{Hash: store.HashSecret("old"), Kind: store.SessionDashboard, Owner: owner, Expires: time.Now().Add(-time.Minute)})
			r.client.Jar.SetCookies(u, []*http.Cookie{{Name: connect.SessionCookie, Value: "old", Path: "/"}})
		}
		hit(how+" page", func() *http.Response { res, _ := r.get(connect.PagePath); return res })
		hit(how+" callback", func() *http.Response { res, _ := r.get(connect.CallbackPath + "?state=x&code=y"); return res })
		for _, p := range []string{connect.StartPath, "/connect/accounts/" + acct.ID + "/enable-send",
			"/connect/accounts/" + acct.ID + "/disable-send", "/connect/accounts/" + acct.ID + "/disconnect"} {
			p := p
			hit(how+" "+p, func() *http.Response {
				res, _ := r.postRaw(p, url.Values{"csrf": {csrf}, "confirm": {"Work"}}, nil)
				return res
			})
		}
	}
	got, _ := r.st.GetAccount(context.Background(), owner, acct.ID)
	if got.Status != store.StatusOK || len(got.RefreshToken) == 0 {
		t.Fatalf("unauthenticated requests changed the account: %+v", got)
	}
}

func TestStartRedirectsToGoogleWithOfflineAccessAndPKCE(t *testing.T) {
	r := newRig(t)
	state, loc := r.startConnect(nil)
	q := loc.Query()
	if q.Get("client_id") != "client-id" || q.Get("redirect_uri") != r.app.URL+"/connect/google/callback" || q.Get("response_type") != "code" {
		t.Errorf("auth params: %v", q)
	}
	if got := q.Get("scope"); got != connect.ScopeReadonly+" "+connect.ScopeCompose {
		t.Errorf("scope = %q, want exactly gmail.readonly and gmail.compose", got)
	}
	if q.Get("access_type") != "offline" || q.Get("prompt") != "consent" {
		t.Errorf("access_type=%q prompt=%q, want offline and consent", q.Get("access_type"), q.Get("prompt"))
	}
	if q.Get("code_challenge_method") != "S256" || len(q.Get("code_challenge")) < 40 {
		t.Errorf("PKCE params missing: %v", q)
	}
	if len(state) < 40 {
		t.Errorf("state %q is too short to be unguessable", state)
	}
	// Only the hash of the state is stored.
	st, err := r.st.TakeConnectState(context.Background(), store.HashSecret(state), time.Now())
	if err != nil || st.Owner != owner || st.Verifier == "" {
		t.Fatalf("stored state = %+v, %v", st, err)
	}
}

func TestPostsNeedACSRFTokenAndSameOrigin(t *testing.T) {
	r := newRig(t)
	acct := r.seed("Work", "w@x.example", 0, store.StatusOK)
	good := r.csrf()
	for name, c := range map[string]struct {
		form url.Values
		hdr  map[string]string
	}{
		"missing token":  {url.Values{}, nil},
		"wrong token":    {url.Values{"csrf": {"nope"}}, nil},
		"foreign origin": {url.Values{"csrf": {good}}, map[string]string{"Origin": "https://evil.example"}},
		"cross-site":     {url.Values{"csrf": {good}}, map[string]string{"Sec-Fetch-Site": "cross-site"}},
	} {
		for _, p := range []string{connect.StartPath, "/connect/accounts/" + acct.ID + "/disable-send"} {
			if res, _ := r.postRaw(p, c.form, c.hdr); res.StatusCode != http.StatusForbidden {
				t.Errorf("%s on %s = %d, want 403", name, p, res.StatusCode)
			}
		}
	}
	if res, _ := r.postRaw("/connect/accounts/"+acct.ID+"/disable-send", url.Values{"csrf": {good}}, map[string]string{"Origin": r.app.URL, "Sec-Fetch-Site": "same-origin"}); res.StatusCode != http.StatusSeeOther {
		t.Errorf("same-origin post = %d, want 303", res.StatusCode)
	}
	u, _ := url.Parse(r.app.URL + connect.PagePath)
	for _, c := range r.client.Jar.Cookies(u) {
		if c.Name == "moose_csrf" && c.Value != good {
			t.Error("csrf token rotated on a later request")
		}
	}
}

func TestConnectStoresAReadOnlyAccount(t *testing.T) {
	r := newRig(t)
	res, _ := r.connectAccount("Pat@Example.com", nil)
	r.wantStatus(res, http.StatusSeeOther)
	accts := r.accounts()
	if len(accts) != 1 {
		t.Fatalf("%d accounts stored, want 1", len(accts))
	}
	a := accts[0]
	if !strings.HasPrefix(a.ID, "acc_") || a.Address != "Pat@Example.com" || a.ProviderUID != "pat@example.com" || a.Provider != store.ProviderGoogle ||
		a.Nickname != "pat" || a.DisplayName != "Name of Pat@Example.com" || a.ColorIndex != 0 || !a.IsDefault ||
		a.Status != store.StatusOK || a.SendEnabled || a.HourlyCap != store.DefaultHourlyCap || !a.Caps.Threads || a.Caps.SearchDialect != provider.DialectGmail {
		t.Fatalf("account = %+v", a)
	}
	if len(a.Scopes) != 2 || a.Scopes[0] != connect.ScopeReadonly || a.Scopes[1] != connect.ScopeCompose {
		t.Errorf("scopes = %v", a.Scopes)
	}
	if loc := res.Header.Get("Location"); loc != "/connect/google?connected="+a.ID {
		t.Errorf("redirect = %q", loc)
	}
	// The refresh token is sealed to {owner, account_id}: it opens only there.
	if strings.Contains(string(a.RefreshToken), "refresh-for") {
		t.Fatal("refresh token stored in the clear")
	}
	got, err := r.seal.Open(context.Background(), a.RefreshToken, envelope.Context{Owner: owner, AccountID: a.ID})
	if err != nil || string(got) != "1//refresh-for-Pat@Example.com" {
		t.Fatalf("sealed token = %q, %v", got, err)
	}
	if _, err := r.seal.Open(context.Background(), a.RefreshToken, envelope.Context{Owner: owner, AccountID: "acc_other"}); err == nil {
		t.Fatal("refresh token opened under another account id")
	}
	// The code was exchanged with the PKCE verifier and the tenant's client.
	ex := r.exchanges[len(r.exchanges)-1]
	if ex.Get("code_verifier") == "" || ex.Get("client_id") != "client-id" || ex.Get("client_secret") != "client-secret" || ex.Get("grant_type") != "authorization_code" {
		t.Errorf("token exchange form = %v", ex)
	}
}

func TestAccountsGetColoursInConnectOrderAndOneDefault(t *testing.T) {
	r := newRig(t)
	for _, addr := range []string{"a@x.example", "b@x.example", "c@x.example"} {
		if res, _ := r.connectAccount(addr, nil); res.StatusCode != http.StatusSeeOther {
			t.Fatalf("connect %s = %d", addr, res.StatusCode)
		}
	}
	accts := r.accounts()
	defaults := 0
	for i, a := range accts {
		if a.ColorIndex != i {
			t.Errorf("account %d colour_index = %d", i, a.ColorIndex)
		}
		if a.IsDefault {
			defaults++
		}
	}
	if len(accts) != 3 || defaults != 1 || !accts[0].IsDefault {
		t.Fatalf("accounts=%d defaults=%d first default=%v", len(accts), defaults, accts[0].IsDefault)
	}
}

func TestHandlesComeFromTheAddress(t *testing.T) {
	r := newRig(t)
	r.connectAccount("pat@one.example", url.Values{"nickname": {"Work"}}) // ignored: no user-defined labels
	r.connectAccount("pat@two.example", nil)
	r.connectAccount("pat@three.example", nil)
	var nicks []string
	for _, a := range r.accounts() {
		nicks = append(nicks, a.Nickname)
	}
	if strings.Join(nicks, ",") != "pat,pat-2,pat-3" {
		t.Fatalf("nicknames = %v, want pat,pat-2,pat-3 (a taken handle gets a numeric suffix)", nicks)
	}
}

func TestReconnectKeepsIdentity(t *testing.T) {
	r := newRig(t)
	r.connectAccount("pat@x.example", nil)
	r.connectAccount("kim@x.example", nil)
	orig := r.accounts()[0]
	if err := r.st.SetAccountCredential(context.Background(), orig.Owner, orig.ID, []byte("stale"), orig.Version); err != nil {
		t.Fatal(err)
	}
	orig.RefreshToken = []byte("stale")
	if err := r.st.UpdateAccount(context.Background(), orig.Owner, orig.ID, store.AccountPatch{Status: store.Ptr(store.StatusNeedsReauth)}); err != nil {
		t.Fatal(err)
	}
	// Through the Reconnect button.
	res, _ := r.connectAccount("pat@x.example", url.Values{"account_id": {orig.ID}})
	r.wantStatus(res, http.StatusSeeOther)
	got, _ := r.st.GetAccount(context.Background(), owner, orig.ID)
	if got.ID != orig.ID || got.ColorIndex != orig.ColorIndex || got.Nickname != orig.Nickname || got.Status != store.StatusOK || !got.IsDefault {
		t.Fatalf("after reconnect: %+v", got)
	}
	if pt, err := r.seal.Open(context.Background(), got.RefreshToken, envelope.Context{Owner: owner, AccountID: got.ID}); err != nil || !strings.Contains(string(pt), "pat@x.example") {
		t.Fatalf("token after reconnect: %q, %v", pt, err)
	}
	// Plain "connect" of the same mailbox reattaches to it too, even with another case.
	r.connectAccount("PAT@x.example", nil)
	if n := len(r.accounts()); n != 2 {
		t.Fatalf("%d accounts after connecting the same mailbox twice, want 2", n)
	}
	got, _ = r.st.GetAccount(context.Background(), owner, orig.ID)
	if got.ColorIndex != 0 || got.Nickname != orig.Nickname {
		t.Fatalf("identity changed: %+v", got)
	}
}

func TestReconnectKeepsSendSettingOnlyWhileConnected(t *testing.T) {
	r := newRig(t)
	r.connectAccount("pat@x.example", nil)
	a := r.accounts()[0]
	_ = r.st.UpdateAccount(context.Background(), a.Owner, a.ID, store.AccountPatch{SendEnabled: store.Ptr(true)})
	r.connectAccount("pat@x.example", url.Values{"account_id": {a.ID}})
	if got, _ := r.st.GetAccount(context.Background(), owner, a.ID); !got.SendEnabled {
		t.Fatal("a plain reconnect turned send off")
	}
	_ = r.st.DisconnectAccount(context.Background(), owner, a.ID, time.Now())
	r.connectAccount("pat@x.example", nil)
	got, _ := r.st.GetAccount(context.Background(), owner, a.ID)
	if got.ID != a.ID || got.Status != store.StatusOK || got.SendEnabled {
		t.Fatalf("reconnect after disconnect = %+v, want same id, ok, send still off", got)
	}
}

func TestReconnectToADifferentGoogleAccountIsRefused(t *testing.T) {
	r := newRig(t)
	r.connectAccount("pat@x.example", nil)
	orig := r.accounts()[0]
	before := orig.RefreshToken
	res, body := r.connectAccount("someone-else@x.example", url.Values{"account_id": {orig.ID}})
	r.wantStatus(res, http.StatusConflict)
	if !strings.Contains(body, "someone-else@x.example") || !strings.Contains(body, "different mailbox") {
		t.Errorf("error does not explain: %q", body)
	}
	accts := r.accounts()
	if len(accts) != 1 || string(accts[0].RefreshToken) != string(before) {
		t.Fatalf("a refused reconnect changed state: %d accounts", len(accts))
	}
	if res, _ := r.post(connect.StartPath, url.Values{"account_id": {"acc_unknown"}}); res.StatusCode != http.StatusNotFound {
		t.Errorf("reconnect of an unknown account = %d, want 404", res.StatusCode)
	}
}

func TestCallbackRejectsReusedUnknownAndForeignState(t *testing.T) {
	r := newRig(t)
	state, _ := r.startConnect(nil)
	r.codes["c1"] = "pat@x.example"
	if res, _ := r.callback(state, "c1"); res.StatusCode != http.StatusSeeOther {
		t.Fatalf("first callback = %d", res.StatusCode)
	}
	r.codes["c2"] = "pat@x.example"
	if res, body := r.callback(state, "c2"); res.StatusCode != http.StatusBadRequest || !strings.Contains(body, "expired or was already used") {
		t.Fatalf("replayed state = %d %q", res.StatusCode, body)
	}
	if res, _ := r.callback("never-issued", "c2"); res.StatusCode != http.StatusBadRequest {
		t.Fatalf("unknown state = %d", res.StatusCode)
	}
	if res, _ := r.get(connect.CallbackPath + "?code=c2"); res.StatusCode != http.StatusBadRequest {
		t.Fatalf("missing state = %d", res.StatusCode)
	}
	// A state started by one session cannot be completed by another owner's.
	state2, _ := r.startConnect(nil)
	r.login("conner")
	u, _ := url.Parse(r.app.URL)
	r.client.Jar.SetCookies(u, []*http.Cookie{{Name: connect.SessionCookie, Value: "session-token-conner", Path: "/"}})
	r.codes["c3"] = "conner@x.example"
	if res, _ := r.callback(state2, "c3"); res.StatusCode != http.StatusForbidden {
		t.Fatalf("foreign owner's callback = %d, want 403", res.StatusCode)
	}
	if n := len(r.accounts()); n != 1 {
		t.Fatalf("%d accounts for the first owner, want 1", n)
	}
	if other, _ := r.st.ListAccounts(context.Background(), "conner"); len(other) != 0 {
		t.Fatalf("foreign callback created an account: %+v", other)
	}
}

func TestIncompleteGrantsStoreNothing(t *testing.T) {
	r := newRig(t)
	cases := map[string]func(){
		"compose unticked":  func() { r.scope = connect.ScopeReadonly },
		"readonly unticked": func() { r.scope = connect.ScopeCompose },
		"no refresh token":  func() { r.noRefresh = true },
	}
	for name, setup := range cases {
		r.scope, r.noRefresh = connect.ScopeReadonly+" "+connect.ScopeCompose, false
		setup()
		res, body := r.connectAccount("pat@x.example", nil)
		if res.StatusCode < 400 {
			t.Errorf("%s: status %d, want an error", name, res.StatusCode)
		}
		if name != "no refresh token" && !strings.Contains(body, "Missing") {
			t.Errorf("%s: body does not name the missing permission: %q", name, body)
		}
		if n := len(r.accounts()); n != 0 {
			t.Fatalf("%s stored %d accounts", name, n)
		}
	}
	r.scope, r.noRefresh = "-", false // Google omitted scope: the requested scopes were granted
	if res, _ := r.connectAccount("pat@x.example", nil); res.StatusCode != http.StatusSeeOther {
		t.Errorf("omitted scope field = %d, want success", res.StatusCode)
	}
}

func TestGoogleDenialAndBadCode(t *testing.T) {
	r := newRig(t)
	state, _ := r.startConnect(nil)
	res, body := r.get(connect.CallbackPath + "?state=" + url.QueryEscape(state) + "&error=access_denied")
	if res.StatusCode != http.StatusBadRequest || !strings.Contains(body, "access_denied") || !strings.Contains(body, "Nothing was saved") {
		t.Fatalf("denied = %d %q", res.StatusCode, body)
	}
	state, _ = r.startConnect(nil)
	if res, _ := r.callback(state, "unknown-code"); res.StatusCode != http.StatusBadGateway {
		t.Fatalf("bad code = %d, want 502", res.StatusCode)
	}
	if n := len(r.accounts()); n != 0 {
		t.Fatalf("%d accounts after failures", n)
	}
}

func TestPageRowsAndControls(t *testing.T) {
	r := newRig(t)
	ok := r.seed("Alpha", "alpha@x.example", 0, store.StatusOK)
	sendOn := r.seed("Bravo", "bravo@x.example", 1, store.StatusOK)
	_ = r.st.UpdateAccount(context.Background(), sendOn.Owner, sendOn.ID, store.AccountPatch{SendEnabled: store.Ptr(true)})
	reauth := r.seed("Charlie", "charlie@x.example", 2, store.StatusNeedsReauth)
	delta := r.seed("Delta", "delta@x.example", 3, store.StatusDisconnected)
	_, body := r.get(connect.PagePath)

	pos := func(s string) int { return strings.Index(body, s) }
	if !(pos("charlie@") < pos("alpha@") && pos("alpha@") < pos("bravo@") && pos("bravo@") < pos("delta@")) || pos("charlie@") < 0 {
		t.Errorf("row order wrong: needs_reauth must sort first, then connect order")
	}
	for _, want := range []string{
		"alpha@x.example", palette.At(0).Name, palette.At(1).Name, palette.At(2).Name, palette.At(3).Name, // address and colour name per row
		"Needs reconnect", "Access verified", "Disconnected", "Not used by Claude yet",
		">Reconnect<",
		"/connect/accounts/" + ok.ID + "/enable-send", "/connect/accounts/" + sendOn.ID + "/disable-send",
		"/connect/accounts/" + reauth.ID + "/disconnect", "Type <b>alpha@x.example</b> to confirm",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("page lacks %q", want)
		}
	}
	for _, unwanted := range []string{
		"/connect/accounts/" + sendOn.ID + "/enable-send", // already enabled
		"/connect/accounts/" + reauth.ID + "/enable-send", // needs reconnect first
	} {
		if strings.Contains(body, unwanted) {
			t.Errorf("page offers %q", unwanted)
		}
	}
	if strings.Count(body, "Reconnect Alpha") != 0 {
		t.Error("a healthy account offers Reconnect")
	}
	if strings.Contains(body, "/connect/accounts/"+delta.ID+"/disconnect") {
		t.Error("a disconnected account offers Disconnect")
	}
}

func TestPageEscapesAddresses(t *testing.T) {
	r := newRig(t)
	r.seed("x", `<img src=x onerror=1>@x.example`, 0, store.StatusOK)
	_, body := r.get(connect.PagePath)
	if strings.Contains(body, "<img src=x") {
		t.Fatal("address rendered unescaped")
	}
}

func TestPageHasSecurityHeadersAndPhoneViewport(t *testing.T) {
	r := newRig(t)
	res, body := r.get(connect.PagePath)
	h := res.Header
	if h.Get("Cache-Control") != "no-store" || h.Get("X-Content-Type-Options") != "nosniff" || !strings.Contains(h.Get("Content-Security-Policy"), "frame-ancestors 'none'") {
		t.Errorf("headers: %v", h)
	}
	if !strings.Contains(body, `name="viewport"`) || !strings.Contains(body, `<html lang="en">`) {
		t.Error("page lacks viewport meta or lang")
	}
	var csrf *http.Cookie
	for _, c := range res.Cookies() {
		if c.Name == "moose_csrf" {
			csrf = c
		}
	}
	if csrf == nil || !csrf.HttpOnly || csrf.SameSite != http.SameSiteStrictMode || csrf.Path != "/connect/" {
		t.Errorf("csrf cookie = %+v", csrf)
	}
}

func TestDisconnectNeedsTheTypedAddress(t *testing.T) {
	r := newRig(t)
	a := r.seed("Work", "w@x.example", 0, store.StatusOK)
	_ = r.st.UpdateAccount(context.Background(), a.Owner, a.ID, store.AccountPatch{SendEnabled: store.Ptr(true)})
	path := "/connect/accounts/" + a.ID + "/disconnect"
	for _, wrong := range []string{"", "Work", "w@x", "w@x.example.com"} {
		res, _ := r.post(path, url.Values{"confirm": {wrong}})
		r.wantStatus(res, http.StatusBadRequest)
	}
	got, _ := r.st.GetAccount(context.Background(), owner, a.ID)
	if got.Status != store.StatusOK || len(got.RefreshToken) == 0 || !got.SendEnabled {
		t.Fatalf("a wrong confirmation changed the account: %+v", got)
	}
	res, _ := r.post(path, url.Values{"confirm": {" W@x.example "}})
	r.wantStatus(res, http.StatusSeeOther)
	got, _ = r.st.GetAccount(context.Background(), owner, a.ID)
	if got.Status != store.StatusDisconnected || len(got.RefreshToken) != 0 || got.SendEnabled || got.Nickname != "Work" {
		t.Fatalf("after disconnect: %+v", got)
	}
	if res, _ := r.post("/connect/accounts/acc_nope/disconnect", url.Values{"confirm": {"x"}}); res.StatusCode != http.StatusNotFound {
		t.Errorf("unknown account = %d, want 404", res.StatusCode)
	}
}

func TestAnotherOwnersAccountIsNotFound(t *testing.T) {
	r := newRig(t)
	other := store.Account{Owner: "conner", ID: store.NewAccountID(), Provider: store.ProviderGoogle, ProviderUID: "c@x.example", Address: "c@x.example",
		Nickname: "Mine", Status: store.StatusOK, RefreshToken: []byte("sealed")}
	if err := r.st.CreateAccount(context.Background(), other); err != nil {
		t.Fatal(err)
	}
	for _, act := range []string{"disconnect", "disable-send", "enable-send"} {
		res, _ := r.post("/connect/accounts/"+other.ID+"/"+act, url.Values{"confirm": {"Mine"}})
		r.wantStatus(res, http.StatusNotFound)
	}
	if got, _ := r.st.GetAccount(context.Background(), "conner", other.ID); got.Status != store.StatusOK {
		t.Fatalf("another owner's account changed: %+v", got)
	}
}

func TestEnableSendCreatesAnApprovalRequest(t *testing.T) {
	r := newRig(t)
	a := r.seed("Work", "Work@X.example", 0, store.StatusOK)
	res, _ := r.post("/connect/accounts/"+a.ID+"/enable-send", nil)
	r.wantStatus(res, http.StatusSeeOther)
	if loc := res.Header.Get("Location"); loc != r.app.URL+"/approve/r/req-1" {
		t.Fatalf("redirect = %q, want the approval page", loc)
	}
	if len(r.approver.reqs) != 1 {
		t.Fatalf("%d approval requests, want 1", len(r.approver.reqs))
	}
	req := r.approver.reqs[0]
	wantPayload := fmt.Sprintf(`{"account_id":%q,"from_address":"Work@X.example","owner":"pat"}`, a.ID)
	if req.Kind != "account-send-enable" || string(req.Payload) != wantPayload {
		t.Fatalf("request = %s %s, want account-send-enable %s", req.Kind, req.Payload, wantPayload)
	}
	if req.IdempotencyKey == "" || req.Preview["from_address"] != "Work@X.example" || req.Preview["nickname"] != "Work" {
		t.Errorf("request metadata = %+v", req)
	}
	ap, err := r.st.GetApproval(context.Background(), "req-1")
	if err != nil {
		t.Fatal(err)
	}
	if ap.Kind != store.KindAccountSendEnable || ap.AccountID != a.ID || ap.Owner != owner || ap.Status != store.ApprovalPending ||
		ap.PayloadSHA256 != fmt.Sprintf("%x", sha256.Sum256([]byte(wantPayload))) || string(ap.Payload) != wantPayload {
		t.Fatalf("stored approval = %+v", ap)
	}
	if got, _ := r.st.GetAccount(context.Background(), owner, a.ID); got.SendEnabled {
		t.Fatal("Enable send turned send on before any approval")
	}
	// A double click reuses the request.
	res, _ = r.post("/connect/accounts/"+a.ID+"/enable-send", nil)
	r.wantStatus(res, http.StatusSeeOther)
	if r.approver.reqs[0].IdempotencyKey != r.approver.reqs[len(r.approver.reqs)-1].IdempotencyKey {
		t.Error("two clicks used different idempotency keys")
	}
}

func TestEnableSendRefusals(t *testing.T) {
	r := newRig(t)
	reauth := r.seed("Stale", "s@x.example", 0, store.StatusNeedsReauth)
	if res, _ := r.post("/connect/accounts/"+reauth.ID+"/enable-send", nil); res.StatusCode != http.StatusConflict {
		t.Errorf("needs_reauth account = %d, want 409", res.StatusCode)
	}
	ok := r.seed("Fine", "f@x.example", 1, store.StatusOK)
	r.approver.err = errors.New("approver down")
	if res, _ := r.post("/connect/accounts/"+ok.ID+"/enable-send", nil); res.StatusCode != http.StatusBadGateway {
		t.Errorf("approver error = %d, want 502", res.StatusCode)
	}
	r.approver.err, r.approver.badSHA = nil, true
	if res, _ := r.post("/connect/accounts/"+ok.ID+"/enable-send", nil); res.StatusCode != http.StatusBadGateway {
		t.Errorf("approver hashing another payload = %d, want 502", res.StatusCode)
	}
	r.approver.badSHA, r.approver.url = false, "https://evil.example/approve/r/x"
	if res, _ := r.post("/connect/accounts/"+ok.ID+"/enable-send", nil); res.StatusCode != http.StatusBadGateway {
		t.Errorf("approval url on a foreign host = %d, want 502", res.StatusCode)
	}
	if got, _ := r.st.GetAccount(context.Background(), owner, ok.ID); got.SendEnabled {
		t.Fatal("send enabled without approval")
	}
}

func TestEnableSendWithoutApprover(t *testing.T) {
	r := newRig(t)
	cfg := connect.GoogleConfig("a", "b", r.app.URL)
	srv, err := connect.New(connect.Deps{Store: r.st, Seal: r.seal, OAuth: cfg, BaseURL: r.app.URL, InsecureCookies: true,
		Auth: connect.StaticOwner(owner), Profile: func(context.Context, oauth2.TokenSource) (connect.Profile, error) { return connect.Profile{}, nil }})
	if err != nil {
		t.Fatal(err)
	}
	a := r.seed("Work", "w@x.example", 0, store.StatusOK)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	jar, _ := cookiejar.New(nil)
	c := &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	res, _ := c.Get(ts.URL + connect.PagePath)
	body, _ := io.ReadAll(res.Body)
	if strings.Contains(string(body), "enable-send") || !strings.Contains(string(body), "read-only for now") {
		t.Error("page offers Enable send without an approver")
	}
	u, _ := url.Parse(ts.URL + connect.PagePath)
	var token string
	for _, ck := range jar.Cookies(u) {
		if ck.Name == "moose_csrf" {
			token = ck.Value
		}
	}
	// The test server's own origin differs from BaseURL, so use a matching Origin.
	req, _ := http.NewRequest("POST", ts.URL+"/connect/accounts/"+a.ID+"/enable-send", strings.NewReader(url.Values{"csrf": {token}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", r.app.URL)
	res, err = c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("enable-send without an approver = %d, want 503", res.StatusCode)
	}
}

func TestDisableSendAppliesDirectly(t *testing.T) {
	r := newRig(t)
	a := r.seed("Work", "w@x.example", 0, store.StatusOK)
	_ = r.st.UpdateAccount(context.Background(), a.Owner, a.ID, store.AccountPatch{SendEnabled: store.Ptr(true)})
	res, _ := r.post("/connect/accounts/"+a.ID+"/disable-send", nil)
	r.wantStatus(res, http.StatusSeeOther)
	if got, _ := r.st.GetAccount(context.Background(), owner, a.ID); got.SendEnabled {
		t.Fatal("send still enabled")
	}
	if len(r.approver.reqs) != 0 {
		t.Fatal("disable asked the approver for an approval")
	}
	res, _ = r.post("/connect/accounts/"+a.ID+"/disable-send", nil) // idempotent
	r.wantStatus(res, http.StatusSeeOther)
}

func TestNewRejectsIncompleteDeps(t *testing.T) {
	if _, err := connect.New(connect.Deps{}); err == nil {
		t.Fatal("New accepted empty deps")
	}
	r := newRig(t)
	_, err := connect.New(connect.Deps{Store: r.st, Seal: r.seal, OAuth: connect.GoogleConfig("a", "b", "x"), Auth: connect.StaticOwner("o"),
		Profile: func(context.Context, oauth2.TokenSource) (connect.Profile, error) { return connect.Profile{}, nil }, BaseURL: "not a url"})
	if err == nil {
		t.Fatal("New accepted a base url with no host")
	}
}

func TestGoogleConfigDefaults(t *testing.T) {
	c := connect.GoogleConfig("id", "secret", "https://moose.example.org/")
	if c.RedirectURL != "https://moose.example.org/connect/google/callback" {
		t.Errorf("redirect = %q", c.RedirectURL)
	}
	if len(c.Scopes) != 2 || c.Scopes[0] != connect.ScopeReadonly || c.Scopes[1] != connect.ScopeCompose {
		t.Errorf("scopes = %v", c.Scopes)
	}
}
