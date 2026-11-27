package accountprov_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/oauth2"

	"github.com/change-fabric/moose/internal/accounts/accountprov"
	"github.com/change-fabric/moose/internal/accounts/envelope"
	"github.com/change-fabric/moose/internal/accounts/store"
	"github.com/change-fabric/moose/internal/accounts/store/sqlite"
	"github.com/change-fabric/moose/internal/email/provider"
)

const (
	mailScopes = "Mail.ReadWrite Mail.Send"
	calScope   = "Calendars.ReadWrite"
	calShared  = "Calendars.Read.Shared"
	baseScopes = "email offline_access openid profile"
)

// entra stands in for the Microsoft token endpoint. It rotates the refresh
// token on every use, rejects any refresh token but the latest, and, like the
// real endpoint, mints a token only for the scopes that were both requested
// and consented.
type entra struct {
	mu        sync.Mutex
	rt        int
	consented map[string]bool
	requests  []refreshReq
	failWith  string // when set, every request answers 400 with this body
}

type refreshReq struct{ refresh, scope string }

func (e *entra) refreshToken() string { return fmt.Sprintf("rt-%d", e.rt) }

func (e *entra) consent(scopes ...string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, sc := range scopes {
		for _, f := range strings.Fields(sc) {
			e.consented[f] = true
		}
	}
}

func (e *entra) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	e.mu.Lock()
	defer e.mu.Unlock()
	req := refreshReq{r.PostForm.Get("refresh_token"), r.PostForm.Get("scope")}
	e.requests = append(e.requests, req)
	w.Header().Set("Content-Type", "application/json")
	if e.failWith != "" {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(e.failWith))
		return
	}
	if r.PostForm.Get("grant_type") != "refresh_token" || r.PostForm.Get("client_id") != "ms-client" {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"invalid_request"}`))
		return
	}
	if req.refresh != e.refreshToken() {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"invalid_grant","error_description":"AADSTS70000: stale refresh token"}`))
		return
	}
	var granted []string
	for _, sc := range strings.Fields(req.scope) {
		if e.consented[sc] {
			granted = append(granted, sc)
		}
	}
	sort.Strings(granted)
	e.rt++
	fmt.Fprintf(w, `{"access_token":"at-%d|%s","token_type":"Bearer","expires_in":3600,"refresh_token":%q,"scope":%q}`,
		e.rt, strings.Join(granted, ","), e.refreshToken(), strings.Join(granted, " "))
}

type msFixture struct {
	src   *accountprov.Source
	st    store.Store
	seal  envelope.Sealer
	ms    *entra
	acct  store.Account
	clock *struct{ t time.Time }
}

func newMSFixture(t *testing.T, services map[string]store.ServiceGrant) *msFixture {
	t.Helper()
	ctx := context.Background()
	st, err := sqlite.OpenStore(ctx, filepath.Join(t.TempDir(), "e.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	seal, err := envelope.NewLocal([]byte(strings.Repeat("k", 32)))
	if err != nil {
		t.Fatal(err)
	}
	ms := &entra{rt: 1, consented: map[string]bool{}}
	srv := httptest.NewServer(ms)
	t.Cleanup(srv.Close)
	sealed, err := seal.Seal(ctx, []byte("rt-1"), envelope.Context{Owner: "pat", AccountID: "acc_ms"})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 27, 9, 0, 0, 0, time.UTC)
	f := &msFixture{st: st, seal: seal, ms: ms, clock: &struct{ t time.Time }{now}}
	var all []string
	for _, g := range services {
		all = append(all, g.Scopes...)
	}
	f.acct = store.Account{Owner: "pat", ID: "acc_ms", Provider: store.ProviderMicrosoft, ProviderUID: "tid.oid", Address: "a@example.com",
		Nickname: "MS", Status: store.StatusOK, HourlyCap: 10, RefreshToken: sealed, Services: services, Scopes: all, Created: now, Updated: now}
	if err := st.CreateAccount(ctx, f.acct); err != nil {
		t.Fatal(err)
	}
	f.acct, _ = st.GetAccount(ctx, "pat", "acc_ms")
	f.src = &accountprov.Source{
		Store: st, Seal: seal,
		Configs: map[string]*oauth2.Config{store.ProviderMicrosoft: {
			ClientID: "ms-client", ClientSecret: "sec",
			Endpoint: oauth2.Endpoint{TokenURL: srv.URL, AuthStyle: oauth2.AuthStyleInParams},
		}},
		Now: func() time.Time { return f.clock.t },
	}
	return f
}

func grants(mail, cal []string) map[string]store.ServiceGrant {
	g := map[string]store.ServiceGrant{store.ServiceMail: {Scopes: mail}}
	if cal != nil {
		g[store.ServiceCalendar] = store.ServiceGrant{Scopes: cal}
	}
	return g
}

func (f *msFixture) storedCredential(t *testing.T) string {
	t.Helper()
	got, err := f.st.GetAccount(context.Background(), "pat", "acc_ms")
	if err != nil {
		t.Fatal(err)
	}
	pt, err := f.seal.Open(context.Background(), got.RefreshToken, envelope.Context{Owner: "pat", AccountID: "acc_ms"})
	if err != nil {
		t.Fatal(err)
	}
	return string(pt)
}

// expire moves the clock past the access token's lifetime, so the next Token
// call refreshes.
func (f *msFixture) expire() { f.clock.t = f.clock.t.Add(2 * time.Hour) }

func split(s string) []string { return strings.Fields(s) }

func TestMicrosoftRefreshRequestsTheUnionOfGrantedScopesWhenServicesAlternate(t *testing.T) {
	f := newMSFixture(t, grants(split(mailScopes), []string{calScope}))
	f.ms.consent(mailScopes, calScope, baseScopes)
	want := strings.Fields(mailScopes + " " + calScope + " " + baseScopes)
	sort.Strings(want)
	wantScope := strings.Join(want, " ")

	// Right after incremental calendar consent: alternate mail and calendar
	// calls, then force expiry and alternate again. The caller keeps passing the
	// same stale account value, as a long-lived provider closure does.
	for round := 0; round < 3; round++ {
		for _, service := range []string{store.ServiceMail, store.ServiceCalendar, store.ServiceMail, store.ServiceCalendar} {
			tok, err := f.src.Token(context.Background(), f.acct)
			if err != nil {
				t.Fatalf("round %d %s: %v", round, service, err)
			}
			needle := "Mail.ReadWrite"
			if service == store.ServiceCalendar {
				needle = calScope
			}
			if !strings.Contains(tok.AccessToken, needle) {
				t.Fatalf("round %d: a %s call got a token without %s: %s", round, service, needle, tok.AccessToken)
			}
		}
		f.expire()
	}
	if len(f.ms.requests) != 3 {
		t.Fatalf("token endpoint saw %d refreshes, want 3 (cache serves the alternating calls)", len(f.ms.requests))
	}
	for i, r := range f.ms.requests {
		if r.scope != wantScope {
			t.Fatalf("refresh %d requested scope %q, want the full union %q", i, r.scope, wantScope)
		}
		if r.refresh != fmt.Sprintf("rt-%d", i+1) {
			t.Fatalf("refresh %d used %q: the rotated token was not written back", i, r.refresh)
		}
	}
	if got := f.storedCredential(t); got != "rt-4" {
		t.Fatalf("stored credential = %q, want the latest rotation rt-4", got)
	}
	got, _ := f.st.GetAccount(context.Background(), "pat", "acc_ms")
	if len(got.Services[store.ServiceMail].Scopes) != 2 || len(got.Services[store.ServiceCalendar].Scopes) != 1 {
		t.Fatalf("granted scopes were lost: %+v", got.Services)
	}
	if got.Version != 3 {
		t.Fatalf("credential version = %d, want 3 (one per rotation)", got.Version)
	}
}

func TestMicrosoftIncrementalConsentIsPickedUpByTheNextRefresh(t *testing.T) {
	ctx := context.Background()
	f := newMSFixture(t, grants(split(mailScopes), nil))
	f.ms.consent(mailScopes, baseScopes)
	if _, err := f.src.Token(ctx, f.acct); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(f.ms.requests[0].scope, "Calendars") {
		t.Fatalf("a mail-only account asked for calendar scopes: %q", f.ms.requests[0].scope)
	}
	// The owner clicks "Grant calendar": the connect flow records the grant.
	f.ms.consent(calScope)
	svc := grants(split(mailScopes), []string{calScope})
	all := split(mailScopes + " " + calScope)
	if err := f.st.UpdateAccount(ctx, "pat", "acc_ms", store.AccountPatch{Services: &svc, Scopes: &all}); err != nil {
		t.Fatal(err)
	}
	f.expire()
	tok, err := f.src.Token(ctx, f.acct) // stale account value: still the mail-only copy
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(tok.AccessToken, calScope) || !strings.Contains(tok.AccessToken, "Mail.Send") {
		t.Fatalf("token after incremental consent covers %q, want mail and calendar", tok.AccessToken)
	}
	if !strings.Contains(f.ms.requests[1].scope, calScope) || !strings.Contains(f.ms.requests[1].scope, "Mail.ReadWrite") {
		t.Fatalf("second refresh asked for %q", f.ms.requests[1].scope)
	}
}

func TestMicrosoftPersistsTheScopesActuallyGranted(t *testing.T) {
	ctx := context.Background()
	f := newMSFixture(t, grants(split(mailScopes), []string{calScope, calShared}))
	// The tenant consented to read-write but not to shared-calendar access.
	f.ms.consent(mailScopes, calScope, baseScopes)
	if _, err := f.src.Token(ctx, f.acct); err != nil {
		t.Fatal(err)
	}
	got, _ := f.st.GetAccount(ctx, "pat", "acc_ms")
	cal := got.Services[store.ServiceCalendar].Scopes
	if len(cal) != 1 || cal[0] != calScope {
		t.Fatalf("calendar grant = %v, want only %s", cal, calScope)
	}
	if len(got.Services[store.ServiceMail].Scopes) != 2 {
		t.Fatalf("mail grant = %v", got.Services[store.ServiceMail].Scopes)
	}
	for _, sc := range got.Scopes {
		if sc == calShared {
			t.Fatalf("flat scopes still list the ungranted %s: %v", calShared, got.Scopes)
		}
	}
}

func TestMicrosoftRevokedCredentialCodesMoveTheAccountToNeedsReauth(t *testing.T) {
	for name, body := range map[string]string{
		"invalid_grant":        `{"error":"invalid_grant","error_description":"AADSTS50173: the grant was revoked"}`,
		"interaction_required": `{"error":"interaction_required","error_description":"AADSTS50076: MFA needed"}`,
		"AADSTS70008":          `{"error":"x","error_description":"AADSTS70008: The provided authorization code or refresh token has expired"}`,
		"AADSTS700082":         `{"error":"x","error_description":"AADSTS700082: The refresh token has expired due to inactivity"}`,
	} {
		t.Run(name, func(t *testing.T) {
			f := newMSFixture(t, grants(split(mailScopes), nil))
			f.ms.failWith = body
			if _, err := f.src.Token(context.Background(), f.acct); !errors.Is(err, provider.ErrAuth) {
				t.Fatalf("err = %v, want ErrAuth", err)
			}
			got, _ := f.st.GetAccount(context.Background(), "pat", "acc_ms")
			if got.Status != store.StatusNeedsReauth {
				t.Fatalf("status = %s, want needs_reauth", got.Status)
			}
		})
	}
}

func TestMicrosoftOtherFailuresAreNotAuthErrors(t *testing.T) {
	f := newMSFixture(t, grants(split(mailScopes), nil))
	f.ms.failWith = `{"error":"temporarily_unavailable","error_description":"AADSTS90033: try again"}`
	if _, err := f.src.Token(context.Background(), f.acct); err == nil || errors.Is(err, provider.ErrAuth) {
		t.Fatalf("err = %v, want a plain refresh error", err)
	}
	got, _ := f.st.GetAccount(context.Background(), "pat", "acc_ms")
	if got.Status != store.StatusOK {
		t.Fatalf("status = %s", got.Status)
	}
}

// flaky wraps a Store so a test can interleave another writer before the
// write-back, or make it fail.
type flaky struct {
	store.Store
	mu        sync.Mutex
	calls     int
	beforeSet func(owner, id string)
	failSet   error
}

func (s *flaky) SetAccountCredential(ctx context.Context, owner, id string, cred []byte, v int64) error {
	s.mu.Lock()
	s.calls++
	first := s.calls == 1
	s.mu.Unlock()
	if first && s.beforeSet != nil {
		s.beforeSet(owner, id)
	}
	if s.failSet != nil {
		return s.failSet
	}
	return s.Store.SetAccountCredential(ctx, owner, id, cred, v)
}

func TestWriteBackRetriesOnceAfterAVersionConflict(t *testing.T) {
	f := newMSFixture(t, grants(split(mailScopes), nil))
	f.ms.consent(mailScopes, baseScopes)
	w := &flaky{Store: f.st}
	w.beforeSet = func(owner, id string) {
		// Another process bumps the credential version between our read and
		// our write, with the same token content.
		cur, _ := f.st.GetAccount(context.Background(), owner, id)
		_ = f.st.SetAccountCredential(context.Background(), owner, id, cur.RefreshToken, cur.Version)
	}
	f.src.Store = w
	if _, err := f.src.Token(context.Background(), f.acct); err != nil {
		t.Fatal(err)
	}
	if w.calls != 2 {
		t.Fatalf("SetAccountCredential called %d times, want 2 (conflict, then one retry)", w.calls)
	}
	if got := f.storedCredential(t); got != "rt-2" {
		t.Fatalf("stored credential = %q, want the rotated rt-2", got)
	}
}

func TestWriteBackFailureDoesNotFailTheCall(t *testing.T) {
	f := newMSFixture(t, grants(split(mailScopes), nil))
	f.ms.consent(mailScopes, baseScopes)
	f.src.Store = &flaky{Store: f.st, failSet: errors.New("table unavailable")}
	tok, err := f.src.Token(context.Background(), f.acct)
	if err != nil || tok.AccessToken == "" {
		t.Fatalf("token = %v, err = %v: a failed write-back must not fail a good refresh", tok, err)
	}
	if got := f.storedCredential(t); got != "rt-1" {
		t.Fatalf("stored credential = %q, want unchanged rt-1", got)
	}
}

func TestWriteBackDoesNotReviveADisconnectedAccount(t *testing.T) {
	f := newMSFixture(t, grants(split(mailScopes), nil))
	f.ms.consent(mailScopes, baseScopes)
	w := &flaky{Store: f.st}
	w.beforeSet = func(owner, id string) {
		_ = f.st.DisconnectAccount(context.Background(), owner, id, time.Now())
	}
	f.src.Store = w
	if _, err := f.src.Token(context.Background(), f.acct); err != nil {
		t.Fatal(err)
	}
	got, _ := f.st.GetAccount(context.Background(), "pat", "acc_ms")
	if got.Status != store.StatusDisconnected || len(got.RefreshToken) != 0 {
		t.Fatalf("disconnected account was revived: %s, %d credential bytes", got.Status, len(got.RefreshToken))
	}
}

func TestGoogleRotatedRefreshTokenIsWrittenBack(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, 200, `{"access_token":"at-1","token_type":"Bearer","expires_in":3600,"refresh_token":"the-rotated-token"}`)
	if _, err := f.src.Token(ctx, f.acct); err != nil {
		t.Fatal(err)
	}
	got, err := f.st.GetAccount(ctx, "pat", "acc_one")
	if err != nil {
		t.Fatal(err)
	}
	if got.Version != 1 {
		t.Fatalf("version = %d, want 1", got.Version)
	}
	// newFixture's token endpoint insists on the original token, so reading the
	// store back is the check that the rotation landed.
	seal, _ := envelope.NewLocal([]byte(strings.Repeat("k", 32)))
	pt, err := seal.Open(ctx, got.RefreshToken, envelope.Context{Owner: "pat", AccountID: "acc_one"})
	if err != nil || string(pt) != "the-rotated-token" {
		t.Fatalf("stored credential = %q, %v", pt, err)
	}
}

func TestGoogleWithoutRotationWritesNothing(t *testing.T) {
	f := newFixture(t, 200, goodToken)
	if _, err := f.src.Token(context.Background(), f.acct); err != nil {
		t.Fatal(err)
	}
	got, _ := f.st.GetAccount(context.Background(), "pat", "acc_one")
	if got.Version != 0 {
		t.Fatalf("version = %d, want 0: an unchanged refresh token must not be rewritten", got.Version)
	}
}

// A Microsoft account yields the Graph mail client, which reads with the
// bearer token the refresh produced and reports read-only capabilities.
func TestMicrosoftAccountsGetTheGraphClient(t *testing.T) {
	f := newMSFixture(t, grants(split(mailScopes), nil))
	f.ms.consent(mailScopes, baseScopes)
	var auth string
	graphSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"value":[{"id":"AAFolderInbox=","displayName":"Inbox"}]}`))
	}))
	defer graphSrv.Close()
	f.src.GraphBaseURL = graphSrv.URL

	p, err := f.src.Provider(context.Background(), f.acct)
	if err != nil {
		t.Fatal(err)
	}
	if caps := p.Capabilities(); caps.SearchDialect != provider.DialectGraph || caps.ReadOnly || !caps.NativeDraftSend {
		t.Fatalf("capabilities = %+v, want Graph with native draft send and no read-only marker", caps)
	}
	boxes, err := p.ListMailboxes(context.Background())
	if err != nil || len(boxes) == 0 {
		t.Fatalf("ListMailboxes = %v, %v", boxes, err)
	}
	if !strings.HasPrefix(auth, "Bearer at-") {
		t.Errorf("Graph saw Authorization %q, want the refreshed token", auth)
	}
	if _, ok := p.(provider.NativeSender); !ok {
		t.Error("the Graph provider must implement provider.NativeSender")
	}
}

// Decision 11: a revoked credential at the token endpoint and a claims
// challenge from Graph both surface as provider.ErrAuth.
func TestMicrosoftAuthFailuresMapToErrAuth(t *testing.T) {
	f := newMSFixture(t, grants(split(mailScopes), nil))
	f.ms.failWith = `{"error":"interaction_required","error_description":"AADSTS50076: MFA required"}`
	p, err := f.src.Provider(context.Background(), f.acct)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.ListMailboxes(context.Background()); !errors.Is(err, provider.ErrAuth) {
		t.Fatalf("revoked credential = %v, want ErrAuth", err)
	}
	if got, _ := f.st.GetAccount(context.Background(), "pat", "acc_ms"); got.Status != store.StatusNeedsReauth {
		t.Errorf("status = %s, want needs_reauth", got.Status)
	}

	g := newMSFixture(t, grants(split(mailScopes), nil))
	g.ms.consent(mailScopes, baseScopes)
	graphSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("WWW-Authenticate", `Bearer realm="", authorization_uri="https://login.microsoftonline.com/common/oauth2/authorize", error="insufficient_claims", claims="eyJhY2Nlc3NfdG9rZW4iOnt9fQ=="`)
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"code":"InvalidAuthenticationToken","message":"Continuous access evaluation resulted in challenge"}}`))
	}))
	defer graphSrv.Close()
	g.src.GraphBaseURL = graphSrv.URL
	p, err = g.src.Provider(context.Background(), g.acct)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.ListMailboxes(context.Background()); !errors.Is(err, provider.ErrAuth) {
		t.Fatalf("claims challenge = %v, want ErrAuth", err)
	}
}
