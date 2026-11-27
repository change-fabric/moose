package connect_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
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
	"github.com/change-fabric/moose/internal/accounts/store"
	"github.com/change-fabric/moose/internal/accounts/store/sqlite"
	"github.com/change-fabric/moose/internal/email/provider"
)

// msRig serves the connect page with both providers over one stub that plays
// the Entra token endpoint and Microsoft Graph /me.
type msRig struct {
	*rig
	ms *httptest.Server

	mu     sync.Mutex
	scope  string // scope the token endpoint reports ("-" omits it)
	tid    string
	oid    string
	aud    string
	mail   string
	upn    string
	idTok  string // overrides the id_token when set
	exch   []url.Values
	codeOK map[string]bool
}

func jwt(claims map[string]any) string {
	b, _ := json.Marshal(claims)
	enc := base64.RawURLEncoding.EncodeToString
	return enc([]byte(`{"alg":"none"}`)) + "." + enc(b) + "."
}

func newMSRig(t *testing.T) *msRig {
	t.Helper()
	m := &msRig{
		scope: "https://graph.microsoft.com/Mail.ReadWrite https://graph.microsoft.com/Mail.Send https://graph.microsoft.com/User.Read openid profile email",
		tid:   "11111111-aaaa-bbbb-cccc-222222222222", oid: "33333333-dddd-eeee-ffff-444444444444", aud: "ms-client",
		mail: "Pat@Contoso.example", upn: "pat@contoso.onmicrosoft.com", codeOK: map[string]bool{},
	}
	m.ms = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		m.mu.Lock()
		defer m.mu.Unlock()
		switch {
		case req.Method == "POST" && req.URL.Path == "/token":
			_ = req.ParseForm()
			m.exch = append(m.exch, req.PostForm)
			if !m.codeOK[req.PostForm.Get("code")] {
				http.Error(w, `{"error":"invalid_grant"}`, http.StatusBadRequest)
				return
			}
			id := m.idTok
			if id == "" {
				id = jwt(map[string]any{"tid": m.tid, "oid": m.oid, "aud": m.aud})
			}
			body := map[string]any{"access_token": "at-ms", "token_type": "Bearer", "expires_in": 3600, "refresh_token": "rt-ms", "id_token": id}
			if m.scope != "-" {
				body["scope"] = m.scope
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(body)
		case req.Method == "GET" && req.URL.Path == "/v1.0/me":
			if req.Header.Get("Authorization") != "Bearer at-ms" {
				http.Error(w, "no", http.StatusUnauthorized)
				return
			}
			var mail any
			if m.mail != "" {
				mail = m.mail
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"mail": mail, "userPrincipalName": m.upn, "displayName": "Pat Taylor"})
		default:
			http.NotFound(w, req)
		}
	}))
	t.Cleanup(m.ms.Close)

	b, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "email.db"))
	if err != nil {
		t.Fatal(err)
	}
	r := &rig{t: t, codes: map[string]string{}}
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

	cfg := connect.MicrosoftConfig("ms-client", "ms-secret", r.app.URL)
	cfg.Endpoint.TokenURL = m.ms.URL + "/token"
	ad := connect.NewMicrosoftAdapter(cfg)
	ad.GraphBaseURL = m.ms.URL + "/v1.0"
	srv, err := connect.New(connect.Deps{
		Store: r.st, Seal: r.seal, Adapters: []connect.Adapter{ad}, BaseURL: r.app.URL, InsecureCookies: true,
		OAuth: connect.GoogleConfig("g-client", "g-secret", r.app.URL),
		Profile: func(context.Context, oauth2.TokenSource) (connect.Profile, error) {
			return connect.Profile{}, errors.New("google is not exercised here")
		},
		Approver: r.approver, Auth: connect.SessionAuth{Store: r.st},
	})
	if err != nil {
		t.Fatal(err)
	}
	mux.Handle(connect.Prefix, srv.Handler())
	jar, _ := cookiejar.New(nil)
	r.client = &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	r.login(owner)
	m.rig = r
	return m
}

func (m *msRig) start(form url.Values) (state string, loc *url.URL) {
	m.t.Helper()
	res, body := m.post(connect.StartPathFor("microsoft"), form)
	m.wantStatus(res, http.StatusSeeOther)
	loc, err := url.Parse(res.Header.Get("Location"))
	if err != nil || loc.Host != "login.microsoftonline.com" {
		m.t.Fatalf("start redirected to %q (%s)", res.Header.Get("Location"), body)
	}
	return loc.Query().Get("state"), loc
}

func (m *msRig) connectMS(form url.Values) (*http.Response, string) {
	m.t.Helper()
	state, _ := m.start(form)
	m.mu.Lock()
	code := "code-" + state[:8]
	m.codeOK[code] = true
	m.mu.Unlock()
	return m.get(connect.CallbackPathFor("microsoft") + "?" + url.Values{"state": {state}, "code": {code}}.Encode())
}

func TestMicrosoftStartUsesTheCommonAuthorityAndTheMailScopes(t *testing.T) {
	m := newMSRig(t)
	state, loc := m.start(nil)
	q := loc.Query()
	if loc.Path != "/common/oauth2/v2.0/authorize" {
		t.Errorf("authorize path = %q, want the /common authority", loc.Path)
	}
	if q.Get("client_id") != "ms-client" || q.Get("redirect_uri") != m.app.URL+"/connect/microsoft/callback" || q.Get("response_type") != "code" {
		t.Errorf("auth params: %v", q)
	}
	want := "Mail.ReadWrite Mail.Send offline_access openid profile email User.Read"
	if got := q.Get("scope"); got != want {
		t.Errorf("scope = %q, want %q", got, want)
	}
	if q.Get("prompt") != "select_account" || q.Has("access_type") {
		t.Errorf("prompt=%q access_type=%q; a new connection shows the account picker and Entra has no access_type", q.Get("prompt"), q.Get("access_type"))
	}
	if q.Get("code_challenge_method") != "S256" || len(q.Get("code_challenge")) < 40 || len(state) < 40 {
		t.Errorf("PKCE or state too weak: %v", q)
	}
	st, err := m.st.TakeConnectState(context.Background(), store.HashSecret(state), time.Now())
	if err != nil || st.Provider != store.ProviderMicrosoft || st.Owner != owner {
		t.Fatalf("stored state = %+v, %v", st, err)
	}
}

func TestMicrosoftConnectStoresAReadOnlyAccount(t *testing.T) {
	m := newMSRig(t)
	res, _ := m.connectMS(nil)
	m.wantStatus(res, http.StatusSeeOther)
	accts := m.accounts()
	if len(accts) != 1 {
		t.Fatalf("%d accounts, want 1", len(accts))
	}
	a := accts[0]
	if a.Provider != store.ProviderMicrosoft || a.ProviderUID != m.tid+"."+m.oid || a.Address != "Pat@Contoso.example" || a.DisplayName != "Pat Taylor" ||
		a.Status != store.StatusOK || a.SendEnabled || !a.Caps.ReadOnly || a.Caps.SearchDialect != provider.DialectGraph || !a.IsDefault {
		t.Fatalf("account = %+v", a)
	}
	if strings.Join(a.Scopes, " ") != "https://graph.microsoft.com/Mail.ReadWrite https://graph.microsoft.com/Mail.Send https://graph.microsoft.com/User.Read" {
		t.Errorf("scopes = %v", a.Scopes)
	}
	if g := a.Services[store.ServiceMail]; len(g.Scopes) != 2 {
		t.Errorf("mail grant = %+v, want the two Mail scopes", g)
	}
	got, err := m.seal.Open(context.Background(), a.RefreshToken, envelope.Context{Owner: owner, AccountID: a.ID})
	if err != nil || string(got) != "rt-ms" {
		t.Fatalf("sealed token = %q, %v", got, err)
	}
	ex := m.exch[len(m.exch)-1]
	if ex.Get("code_verifier") == "" || ex.Get("client_id") != "ms-client" || ex.Get("client_secret") != "ms-secret" {
		t.Errorf("token exchange form = %v", ex)
	}

	// The page badges the provider, reconnects through the provider's own route
	// and shows why sending cannot be turned on.
	_, body := m.get(connect.PagePath)
	for _, want := range []string{`<span class="badge">Microsoft</span>`, "Connect Microsoft account", "/connect/microsoft/start", "Read-only: sending is not available for Microsoft accounts yet", "stored Microsoft tokens"} {
		if !strings.Contains(body, want) {
			t.Errorf("page lacks %q", want)
		}
	}
	if strings.Contains(body, "/enable-send") {
		t.Error("a read-only account offers Enable sending")
	}
}

func TestMicrosoftAddressFallsBackToTheUserPrincipalName(t *testing.T) {
	m := newMSRig(t)
	m.mail = ""
	m.connectMS(nil)
	if a := m.accounts(); len(a) != 1 || a[0].Address != "pat@contoso.onmicrosoft.com" {
		t.Fatalf("accounts = %+v, want the UPN as address", a)
	}
}

func TestMicrosoftReconnectKeepsTheAccountAndRefusesAnotherIdentity(t *testing.T) {
	m := newMSRig(t)
	m.connectMS(nil)
	first := m.accounts()[0]

	// Same tid.oid, renamed mailbox: same account, new address.
	m.mail = "pat.new@contoso.example"
	if res, _ := m.connectMS(url.Values{"account_id": {first.ID}}); res.StatusCode != http.StatusSeeOther {
		t.Fatalf("reconnect = %d", res.StatusCode)
	}
	if a := m.accounts(); len(a) != 1 || a[0].ID != first.ID || a[0].Address != "pat.new@contoso.example" || a[0].Nickname != first.Nickname {
		t.Fatalf("accounts after reconnect = %+v", a)
	}

	// A different oid is a different mailbox: refused, nothing changed.
	m.oid = "99999999-dddd-eeee-ffff-444444444444"
	res, body := m.connectMS(url.Values{"account_id": {first.ID}})
	if res.StatusCode != http.StatusConflict || !strings.Contains(body, "Different Microsoft account") {
		t.Fatalf("other identity = %d %q", res.StatusCode, body)
	}
	if a := m.accounts(); len(a) != 1 || a[0].ProviderUID != m.tid+".33333333-dddd-eeee-ffff-444444444444" {
		t.Fatalf("accounts after refusal = %+v", a)
	}
	// Without a reconnect it is simply a second account.
	m.connectMS(nil)
	if n := len(m.accounts()); n != 2 {
		t.Fatalf("%d accounts, want 2", n)
	}
}

func TestMicrosoftIncompleteOrUnverifiableGrantsStoreNothing(t *testing.T) {
	cases := map[string]func(m *msRig){
		"mail send unticked":  func(m *msRig) { m.scope = "Mail.ReadWrite User.Read openid" },
		"mail write unticked": func(m *msRig) { m.scope = "Mail.Send User.Read openid" },
		"wrong audience":      func(m *msRig) { m.aud = "someone-elses-client" },
		"no tid":              func(m *msRig) { m.idTok = jwt(map[string]any{"oid": "x", "aud": "ms-client"}) },
		"not a jwt":           func(m *msRig) { m.idTok = "garbage" },
		"no address":          func(m *msRig) { m.mail, m.upn = "", "" },
	}
	for name, setup := range cases {
		m := newMSRig(t)
		setup(m)
		if res, _ := m.connectMS(nil); res.StatusCode < 400 {
			t.Errorf("%s: status %d, want an error", name, res.StatusCode)
		}
		if n := len(m.accounts()); n != 0 {
			t.Fatalf("%s stored %d accounts", name, n)
		}
	}
	m := newMSRig(t)
	m.scope = "-" // scope omitted: the request is taken as granted
	if res, _ := m.connectMS(nil); res.StatusCode != http.StatusSeeOther {
		t.Errorf("omitted scope = %d, want success", res.StatusCode)
	}
}

func TestMicrosoftErrorCopy(t *testing.T) {
	cases := []struct {
		name, query string
		want        []string
		link        bool
	}{
		{"admin approval 65001", "error=access_denied&error_description=AADSTS65001%3A+The+user+or+administrator+has+not+consented", []string{"needs to approve moose", "AADSTS65001"}, true},
		{"admin approval 90094", "error=access_denied&error_description=AADSTS90094%3A+The+grant+requires+admin+permission", []string{"needs to approve moose", "AADSTS90094"}, true},
		{"consent_required", "error=consent_required", []string{"needs to approve moose"}, true},
		{"denied", "error=access_denied&error_description=AADSTS65004%3A+The+user+declined", []string{"declined", "Nothing was saved"}, false},
		{"other", "error=server_error&error_description=AADSTS50000%3A+boom", []string{"server_error", "AADSTS50000", "Nothing was saved"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := newMSRig(t)
			state, _ := m.start(nil)
			res, body := m.get(connect.CallbackPathFor("microsoft") + "?state=" + url.QueryEscape(state) + "&" + tc.query)
			if res.StatusCode != http.StatusBadRequest {
				t.Fatalf("status %d, want 400", res.StatusCode)
			}
			for _, w := range tc.want {
				if !strings.Contains(body, w) {
					t.Errorf("body lacks %q: %s", w, body)
				}
			}
			link := "https://login.microsoftonline.com/organizations/v2.0/adminconsent?"
			if has := strings.Contains(body, link); has != tc.link {
				t.Errorf("admin consent link present = %v, want %v", has, tc.link)
			}
			if tc.link {
				for _, part := range []string{"client_id=ms-client", "redirect_uri=" + url.QueryEscape(m.app.URL+"/connect/microsoft/callback")} {
					if !strings.Contains(body, strings.ReplaceAll(part, "&", "&amp;")) {
						t.Errorf("admin consent link lacks %q", part)
					}
				}
			}
			if n := len(m.accounts()); n != 0 {
				t.Fatalf("%d accounts stored after an error", n)
			}
		})
	}
}

func TestAdminConsentAnswerNeedsNoSession(t *testing.T) {
	m := newMSRig(t)
	m.logout()
	res, body := m.get(connect.CallbackPathFor("microsoft") + "?admin_consent=True&tenant=t1")
	if res.StatusCode != http.StatusOK || !strings.Contains(body, "moose is approved") {
		t.Fatalf("approved = %d %q", res.StatusCode, body)
	}
	res, body = m.get(connect.CallbackPathFor("microsoft") + "?admin_consent=False&error=access_denied")
	if res.StatusCode != http.StatusOK || !strings.Contains(body, "was not approved") {
		t.Fatalf("not approved = %d %q", res.StatusCode, body)
	}
	// Without admin_consent a signed-out callback still goes to sign-in.
	if res, _ := m.get(connect.CallbackPathFor("microsoft") + "?code=x"); res.StatusCode != http.StatusSeeOther {
		t.Errorf("signed-out callback = %d, want redirect to sign-in", res.StatusCode)
	}
}

func TestAStateStartedForAnotherProviderIsRefused(t *testing.T) {
	m := newMSRig(t)
	state, _ := m.start(nil)
	res, body := m.get("/connect/google/callback?state=" + url.QueryEscape(state) + "&code=x")
	if res.StatusCode != http.StatusBadRequest || !strings.Contains(body, "different provider") {
		t.Fatalf("a Microsoft state on the Google callback = %d %q", res.StatusCode, body)
	}
	if n := len(m.accounts()); n != 0 {
		t.Fatalf("%d accounts stored", n)
	}
	// The state was consumed by the attempt, so it cannot be replayed on the right route.
	if res, _ := m.get(connect.CallbackPathFor("microsoft") + "?state=" + url.QueryEscape(state) + "&code=x"); res.StatusCode != http.StatusBadRequest {
		t.Errorf("replayed state = %d, want 400", res.StatusCode)
	}
}

func TestUnregisteredProvidersAreNotFoundAndSeededRowsSayTheyAreOff(t *testing.T) {
	g := newRig(t) // Google only: Microsoft is switched off
	if res, _ := g.post("/connect/microsoft/start", nil); res.StatusCode != http.StatusNotFound {
		t.Errorf("start for a switched-off provider = %d, want 404", res.StatusCode)
	}
	if res, _ := g.post("/connect/imap/start", nil); res.StatusCode != http.StatusNotFound {
		t.Errorf("start for an unknown provider = %d, want 404", res.StatusCode)
	}
	a := store.Account{Owner: owner, ID: store.NewAccountID(), Provider: store.ProviderMicrosoft, ProviderUID: "t.o", Address: "ms@contoso.example",
		Nickname: "ms", Status: store.StatusOK, HourlyCap: 10, Created: time.Now(), RefreshToken: []byte("sealed")}
	if err := g.st.CreateAccount(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	_, body := g.get(connect.PagePath)
	for _, want := range []string{"Provider turned off", `<span class="badge">Microsoft</span>`, "/connect/accounts/" + a.ID + "/disconnect"} {
		if !strings.Contains(body, want) {
			t.Errorf("page lacks %q", want)
		}
	}
	if strings.Contains(body, ">Reconnect<") || strings.Contains(body, "Connect Microsoft account") {
		t.Error("a switched-off provider offers connect or reconnect")
	}
	// The stored account is untouched.
	got, _ := g.st.GetAccount(context.Background(), owner, a.ID)
	if got.Status != store.StatusOK || string(got.RefreshToken) != "sealed" {
		t.Errorf("stored account changed: %+v", got)
	}
}

func TestReconnectIsRefusedOnTheWrongProvidersRoute(t *testing.T) {
	m := newMSRig(t)
	google := m.seed("Alpha", "alpha@x.example", 0, store.StatusNeedsReauth)
	res, body := m.post("/connect/microsoft/start", url.Values{"account_id": {google.ID}})
	if res.StatusCode != http.StatusConflict || !strings.Contains(body, "Reconnect it with Google") {
		t.Fatalf("a Google account on the Microsoft route = %d %q", res.StatusCode, body)
	}
	if res, body := m.post("/connect/microsoft/start", url.Values{"account_id": {"acc_missing"}}); res.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown account = %d %q", res.StatusCode, body)
	}
}

func TestMicrosoftConfigDefaults(t *testing.T) {
	c := connect.MicrosoftConfig("id", "sec", "https://moose.example.org/")
	if c.RedirectURL != "https://moose.example.org/connect/microsoft/callback" || c.Endpoint.TokenURL != "https://login.microsoftonline.com/common/oauth2/v2.0/token" ||
		c.Endpoint.AuthStyle != oauth2.AuthStyleInParams {
		t.Errorf("config = %+v", c)
	}
}
