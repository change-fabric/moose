package connect_test

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/change-fabric/moose/internal/accounts/connect"
	"github.com/change-fabric/moose/internal/accounts/envelope"
	"github.com/change-fabric/moose/internal/accounts/scopes"
	"github.com/change-fabric/moose/internal/accounts/store"
)

const (
	calEvents   = "https://www.googleapis.com/auth/calendar.events"
	calReadonly = "https://www.googleapis.com/auth/calendar.readonly"
)

func grantPath(id string) string { return "/connect/accounts/" + id + "/grant-calendar" }

// startGrant posts the Grant calendar form and returns the state the provider
// would echo back and the authorization URL the browser was sent to.
func (r *rig) startGrant(id string) (state string, loc *url.URL) {
	r.t.Helper()
	res, body := r.post(grantPath(id), nil)
	r.wantStatus(res, http.StatusSeeOther)
	loc, err := url.Parse(res.Header.Get("Location"))
	if err != nil || loc.Host != "accounts.example" {
		r.t.Fatalf("grant redirected to %q (%s)", res.Header.Get("Location"), body)
	}
	return loc.Query().Get("state"), loc
}

// finishGrant plays the provider: the signed-in identity is address and the
// token endpoint reports scope.
func (r *rig) finishGrant(state, address, scope string) (*http.Response, string) {
	r.t.Helper()
	r.mu.Lock()
	r.next++
	code := fmt.Sprintf("grant-code-%d", r.next)
	r.codes[code] = address
	r.scope = scope
	r.mu.Unlock()
	return r.callback(state, code)
}

// connectMail connects a mail account and returns it.
func (r *rig) connectMail(address string) store.Account {
	r.t.Helper()
	res, body := r.connectAccount(address, nil)
	r.wantStatus(res, http.StatusSeeOther)
	for _, a := range r.accounts() {
		if a.Address == address {
			return a
		}
	}
	r.t.Fatalf("no account for %s after connect: %s", address, body)
	return store.Account{}
}

func (r *rig) account(id string) store.Account {
	r.t.Helper()
	a, err := r.st.GetAccount(context.Background(), owner, id)
	if err != nil {
		r.t.Fatal(err)
	}
	return a
}

func (r *rig) openCredential(a store.Account) string {
	r.t.Helper()
	b, err := r.seal.Open(context.Background(), a.RefreshToken, envelope.Context{Owner: owner, AccountID: a.ID})
	if err != nil {
		r.t.Fatal(err)
	}
	return string(b)
}

const mailScopes = connect.ScopeReadonly + " " + connect.ScopeCompose

func TestGrantStartAsksForCalendarScopesIncrementally(t *testing.T) {
	r := newRig(t)
	a := r.connectMail("pat@gmail.example")
	_, loc := r.startGrant(a.ID)
	q := loc.Query()
	if got := q.Get("scope"); got != calEvents+" "+calReadonly {
		t.Errorf("scope = %q, want only the two calendar scopes", got)
	}
	if q.Get("include_granted_scopes") != "true" || q.Get("access_type") != "offline" || q.Get("prompt") != "consent" || q.Get("login_hint") != "pat@gmail.example" {
		t.Errorf("incremental consent params: %v", q)
	}
	if q.Get("code_challenge_method") != "S256" || len(q.Get("state")) < 40 {
		t.Errorf("PKCE or state too weak: %v", q)
	}
	if q.Get("redirect_uri") != r.app.URL+connect.CallbackPath {
		t.Errorf("redirect_uri = %q, want the registered Google callback", q.Get("redirect_uri"))
	}
}

func TestGrantBindsAccountProviderAndServiceInTheState(t *testing.T) {
	r := newRig(t)
	a := r.connectMail("pat@gmail.example")
	state, _ := r.startGrant(a.ID)
	st, err := r.st.TakeConnectState(context.Background(), store.HashSecret(state), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if st.GrantAccountID != a.ID || st.Provider != store.ProviderGoogle || st.Service != store.ServiceCalendar || st.Owner != owner || st.ReconnectID != "" {
		t.Fatalf("state = %+v", st)
	}
}

func TestGrantMergesCalendarAndKeepsMail(t *testing.T) {
	r := newRig(t)
	a := r.connectMail("pat@gmail.example")
	before := r.account(a.ID)
	state, _ := r.startGrant(a.ID)
	res, body := r.finishGrant(state, "pat@gmail.example", mailScopes+" "+calEvents+" "+calReadonly)
	r.wantStatus(res, http.StatusSeeOther)
	if loc := res.Header.Get("Location"); !strings.Contains(loc, "granted="+a.ID) || strings.Contains(loc, "partial") {
		t.Fatalf("redirect = %q (%s)", loc, body)
	}
	got := r.account(a.ID)
	cal := got.Services[store.ServiceCalendar]
	if strings.Join(cal.Scopes, " ") != calEvents+" "+calReadonly || cal.GrantedAt.IsZero() {
		t.Errorf("calendar grant = %+v", cal)
	}
	if m := got.Services[store.ServiceMail]; strings.Join(m.Scopes, " ") != strings.Join(before.Services[store.ServiceMail].Scopes, " ") {
		t.Errorf("mail grant changed: %+v", m)
	}
	for _, sc := range []string{connect.ScopeReadonly, connect.ScopeCompose, calEvents, calReadonly} {
		if !contains(got.Scopes, sc) {
			t.Errorf("account scopes %v lack %s", got.Scopes, sc)
		}
	}
	if got.Version <= before.Version || r.openCredential(got) != "1//refresh-for-pat@gmail.example" {
		t.Errorf("the credential was not rewritten with the token that covers both services (version %d -> %d)", before.Version, got.Version)
	}
	if got.Status != store.StatusOK || got.SendEnabled != before.SendEnabled || got.Nickname != before.Nickname {
		t.Errorf("grant changed unrelated fields: %+v", got)
	}
	if miss := scopes.Missing(scopes.Google, scopes.Calendar, got.ProviderUID, cal.Scopes); len(miss) != 0 {
		t.Errorf("missing = %v after a full grant", miss)
	}
	// The page reflects it and no longer offers the button.
	_, page := r.get(res.Header.Get("Location"))
	if !strings.Contains(page, "Calendar: granted") || strings.Contains(page, grantPath(a.ID)) || !strings.Contains(page, "Calendar access granted for") {
		t.Errorf("page after grant: %s", page)
	}
}

func TestGrantWithoutAReportedScopeTakesTheRequestAsGiven(t *testing.T) {
	r := newRig(t)
	a := r.connectMail("pat@gmail.example")
	state, _ := r.startGrant(a.ID)
	res, _ := r.finishGrant(state, "pat@gmail.example", "-")
	r.wantStatus(res, http.StatusSeeOther)
	if cal := r.account(a.ID).Services[store.ServiceCalendar]; len(cal.Scopes) != 2 {
		t.Errorf("grant = %+v, want both requested scopes", cal)
	}
}

func TestGrantSignedInAsADifferentAccountIsRejectedAndMailKeepsWorking(t *testing.T) {
	r := newRig(t)
	a := r.connectMail("pat@gmail.example")
	other := r.connectMail("someone@gmail.example")
	before, otherBefore := r.account(a.ID), r.account(other.ID)
	state, _ := r.startGrant(a.ID)
	res, body := r.finishGrant(state, "someone@gmail.example", mailScopes+" "+calEvents+" "+calReadonly)
	if res.StatusCode != http.StatusConflict || !strings.Contains(body, "Different Google account") || !strings.Contains(body, "pat@gmail.example") {
		t.Fatalf("callback = %d %s", res.StatusCode, body)
	}
	got := r.account(a.ID)
	if _, has := got.Services[store.ServiceCalendar]; has {
		t.Errorf("a calendar grant was stored for the wrong identity: %+v", got.Services)
	}
	if !bytes.Equal(got.RefreshToken, before.RefreshToken) || got.Version != before.Version || got.Status != store.StatusOK ||
		strings.Join(got.Scopes, " ") != strings.Join(before.Scopes, " ") {
		t.Errorf("the mail credential or record changed: before %+v after %+v", before, got)
	}
	o := r.account(other.ID)
	if _, has := o.Services[store.ServiceCalendar]; has || !bytes.Equal(o.RefreshToken, otherBefore.RefreshToken) {
		t.Errorf("the account that actually signed in was changed: %+v", o)
	}
	if n := len(r.accounts()); n != 2 {
		t.Errorf("%d accounts, want 2: a grant never creates one", n)
	}
	// The state was consumed: the same callback cannot be replayed.
	res, _ = r.callback(state, "anything")
	r.wantStatus(res, http.StatusBadRequest)
}

func TestGrantDeniedChangesNothing(t *testing.T) {
	r := newRig(t)
	a := r.connectMail("pat@gmail.example")
	before := r.account(a.ID)
	state, _ := r.startGrant(a.ID)
	res, body := r.get(connect.CallbackPath + "?" + url.Values{"state": {state}, "error": {"access_denied"}}.Encode())
	if res.StatusCode != http.StatusBadRequest || !strings.Contains(body, "calendar access was declined") || !strings.Contains(body, "Mail still works") {
		t.Fatalf("denial = %d %s", res.StatusCode, body)
	}
	got := r.account(a.ID)
	if _, has := got.Services[store.ServiceCalendar]; has || !bytes.Equal(got.RefreshToken, before.RefreshToken) || got.Version != before.Version {
		t.Errorf("denial changed the account: %+v", got)
	}
}

func TestGrantPartialRecordsOnlyWhatWasGranted(t *testing.T) {
	r := newRig(t)
	a := r.connectMail("pat@gmail.example")
	state, _ := r.startGrant(a.ID)
	res, _ := r.finishGrant(state, "pat@gmail.example", mailScopes+" "+calReadonly)
	r.wantStatus(res, http.StatusSeeOther)
	if loc := res.Header.Get("Location"); !strings.Contains(loc, "partial=1") {
		t.Errorf("redirect %q does not flag the partial grant", loc)
	}
	got := r.account(a.ID)
	cal := got.Services[store.ServiceCalendar]
	if len(cal.Scopes) != 1 || cal.Scopes[0] != calReadonly {
		t.Fatalf("calendar grant = %+v, want only calendar.readonly", cal)
	}
	if miss := scopes.Missing(scopes.Google, scopes.Calendar, got.ProviderUID, cal.Scopes); len(miss) != 1 || miss[0] != "calendar.events" {
		t.Errorf("missing = %v, want calendar.events", miss)
	}
	_, page := r.get(connect.PagePath)
	if !strings.Contains(page, "partly granted, missing calendar.events") || !strings.Contains(page, "Grant missing calendar access") {
		t.Errorf("page does not offer to finish the grant: %s", page)
	}
	// Finishing it merges onto what was already granted.
	state, _ = r.startGrant(a.ID)
	res, _ = r.finishGrant(state, "pat@gmail.example", mailScopes+" "+calEvents+" "+calReadonly)
	r.wantStatus(res, http.StatusSeeOther)
	cal = r.account(a.ID).Services[store.ServiceCalendar]
	if len(cal.Scopes) != 2 {
		t.Errorf("after finishing: %+v", cal)
	}
}

func TestGrantWithNoCalendarScopeStoresNothing(t *testing.T) {
	r := newRig(t)
	a := r.connectMail("pat@gmail.example")
	before := r.account(a.ID)
	state, _ := r.startGrant(a.ID)
	res, body := r.finishGrant(state, "pat@gmail.example", mailScopes)
	if res.StatusCode != http.StatusBadRequest || !strings.Contains(body, "No calendar permissions granted") {
		t.Fatalf("callback = %d %s", res.StatusCode, body)
	}
	got := r.account(a.ID)
	if _, has := got.Services[store.ServiceCalendar]; has || !bytes.Equal(got.RefreshToken, before.RefreshToken) {
		t.Errorf("account changed: %+v", got)
	}
}

func TestGrantRefusals(t *testing.T) {
	r := newRig(t)
	ok := r.connectMail("pat@gmail.example")
	reauth := r.seed("Old", "old@gmail.example", 3, store.StatusNeedsReauth)
	gone := r.seed("Gone", "gone@gmail.example", 4, store.StatusDisconnected)

	for name, tc := range map[string]struct {
		id   string
		want int
	}{
		"needs reauth": {reauth.ID, http.StatusConflict},
		"disconnected": {gone.ID, http.StatusConflict},
		"unknown":      {"acc_missing", http.StatusNotFound},
	} {
		res, _ := r.post(grantPath(tc.id), nil)
		if res.StatusCode != tc.want {
			t.Errorf("%s: grant = %d, want %d", name, res.StatusCode, tc.want)
		}
	}
	// No CSRF token, no session.
	res, _ := r.postRaw(grantPath(ok.ID), url.Values{}, nil)
	r.wantStatus(res, http.StatusForbidden)
	r.logout()
	res, _ = r.postRaw(grantPath(ok.ID), url.Values{"csrf": {"x"}}, nil)
	r.wantStatus(res, http.StatusUnauthorized)
}

func TestGrantIsUnavailableWhileCalendarIsOff(t *testing.T) {
	r := newRigCalendar(t, false)
	a := r.connectMail("pat@gmail.example")
	res, _ := r.post(grantPath(a.ID), nil)
	r.wantStatus(res, http.StatusNotFound)
	_, page := r.get(connect.PagePath)
	if strings.Contains(page, "Grant calendar") || strings.Contains(page, "Calendar:") {
		t.Errorf("page offers calendar while it is off: %s", page)
	}
}

func TestAnotherOwnersAccountCannotBeGranted(t *testing.T) {
	r := newRig(t)
	a := r.connectMail("pat@gmail.example")
	r.login("conner")
	// Swap the session cookie to conner's.
	res, _ := r.post(grantPath(a.ID), nil)
	if res.StatusCode != http.StatusNotFound {
		t.Errorf("grant of another owner's account = %d, want 404", res.StatusCode)
	}
}

func TestMailReconnectKeepsTheCalendarGrantAndAsksForGrantedScopes(t *testing.T) {
	r := newRig(t)
	a := r.connectMail("pat@gmail.example")
	state, _ := r.startGrant(a.ID)
	res, _ := r.finishGrant(state, "pat@gmail.example", mailScopes+" "+calEvents+" "+calReadonly)
	r.wantStatus(res, http.StatusSeeOther)

	// A mail reconnect keeps the calendar record, and its authorization request
	// carries include_granted_scopes so the new refresh token still covers it.
	state, loc := r.startConnect(url.Values{"account_id": {a.ID}})
	if loc.Query().Get("include_granted_scopes") != "true" {
		t.Errorf("reconnect request lacks include_granted_scopes: %v", loc.Query())
	}
	r.mu.Lock()
	r.codes["rc"] = "pat@gmail.example"
	r.scope = mailScopes
	r.mu.Unlock()
	res, _ = r.callback(state, "rc")
	r.wantStatus(res, http.StatusSeeOther)
	got := r.account(a.ID)
	if cal := got.Services[store.ServiceCalendar]; len(cal.Scopes) != 2 {
		t.Errorf("reconnect dropped the calendar grant: %+v", got.Services)
	}
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// ---- Microsoft ----

const (
	msMail = "https://graph.microsoft.com/Mail.ReadWrite https://graph.microsoft.com/Mail.Send https://graph.microsoft.com/User.Read"
	msCal  = "https://graph.microsoft.com/Calendars.ReadWrite https://graph.microsoft.com/Calendars.Read.Shared"
)

func (m *msRig) grantMS(id string) (state string, loc *url.URL, res *http.Response) {
	m.t.Helper()
	res, body := m.post(grantPath(id), nil)
	m.wantStatus(res, http.StatusSeeOther)
	loc, err := url.Parse(res.Header.Get("Location"))
	if err != nil || loc.Host != "login.microsoftonline.com" {
		m.t.Fatalf("grant redirected to %q (%s)", res.Header.Get("Location"), body)
	}
	return loc.Query().Get("state"), loc, res
}

func (m *msRig) finishMS(state string) (*http.Response, string) {
	m.t.Helper()
	m.mu.Lock()
	code := "grant-" + state[:8]
	m.codeOK[code] = true
	m.mu.Unlock()
	return m.get(connect.CallbackPathFor("microsoft") + "?" + url.Values{"state": {state}, "code": {code}}.Encode())
}

func TestMicrosoftGrantRequestsTheUnionOfScopes(t *testing.T) {
	m := newMSRig(t)
	m.connectMS(nil)
	a := m.accounts()[0]
	state, loc, _ := m.grantMS(a.ID)
	q := loc.Query()
	got := strings.Fields(q.Get("scope"))
	for _, want := range []string{"Calendars.ReadWrite", "Calendars.Read.Shared", "offline_access", "openid", "profile", "email", "User.Read"} {
		if !containsShort(got, want) {
			t.Errorf("scope %q lacks %s", q.Get("scope"), want)
		}
	}
	if !contains(got, "https://graph.microsoft.com/Mail.ReadWrite") || !contains(got, "https://graph.microsoft.com/Mail.Send") {
		t.Errorf("scope %q must also name the mail scopes the token has to keep covering", q.Get("scope"))
	}
	if q.Get("login_hint") != "Pat@Contoso.example" || q.Get("redirect_uri") != m.app.URL+"/connect/microsoft/callback" {
		t.Errorf("params: %v", q)
	}
	st, err := m.st.TakeConnectState(context.Background(), store.HashSecret(state), time.Now())
	if err != nil || st.GrantAccountID != a.ID || st.Provider != store.ProviderMicrosoft || st.Service != store.ServiceCalendar {
		t.Fatalf("state = %+v, %v", st, err)
	}
}

func TestMicrosoftGrantMergesAndChecksTheImmutableIdentity(t *testing.T) {
	m := newMSRig(t)
	m.connectMS(nil)
	a := m.accounts()[0]
	before := m.account(a.ID)

	// Signed in as another object id in the same tenant: rejected.
	m.oid = "99999999-dddd-eeee-ffff-444444444444"
	m.scope = msMail + " " + msCal + " openid profile email"
	state, _, _ := m.grantMS(a.ID)
	res, body := m.finishMS(state)
	if res.StatusCode != http.StatusConflict || !strings.Contains(body, "Different Microsoft account") {
		t.Fatalf("other identity = %d %s", res.StatusCode, body)
	}
	got := m.account(a.ID)
	if _, has := got.Services[store.ServiceCalendar]; has || !bytes.Equal(got.RefreshToken, before.RefreshToken) || got.Version != before.Version {
		t.Fatalf("a rejected grant changed the account: %+v", got)
	}

	// Same identity: both calendar scopes recorded, mail kept.
	m.oid = "33333333-dddd-eeee-ffff-444444444444"
	state, _, _ = m.grantMS(a.ID)
	res, body = m.finishMS(state)
	m.wantStatus(res, http.StatusSeeOther)
	got = m.account(a.ID)
	cal := got.Services[store.ServiceCalendar]
	if len(cal.Scopes) != 2 || len(got.Services[store.ServiceMail].Scopes) != 2 {
		t.Fatalf("services = %+v (%s)", got.Services, body)
	}
	if miss := scopes.Missing(scopes.Microsoft, scopes.Calendar, got.ProviderUID, cal.Scopes); len(miss) != 0 {
		t.Errorf("missing = %v", miss)
	}
	if got.Version <= before.Version {
		t.Errorf("credential not rewritten: version %d -> %d", before.Version, got.Version)
	}
}

func TestMicrosoftPersonalAccountIsNotAskedForSharedCalendars(t *testing.T) {
	m := newMSRig(t)
	m.tid = scopes.MicrosoftPersonalTenant
	m.connectMS(nil)
	a := m.accounts()[0]
	_, loc, _ := m.grantMS(a.ID)
	got := strings.Fields(loc.Query().Get("scope"))
	if !contains(got, "Calendars.ReadWrite") || contains(got, "Calendars.Read.Shared") {
		t.Errorf("personal account scopes = %v", got)
	}
	// ReadWrite alone is a complete grant there.
	state := loc.Query().Get("state")
	m.mu.Lock()
	m.scope = msMail + " https://graph.microsoft.com/Calendars.ReadWrite openid profile email"
	m.mu.Unlock()
	res, _ := m.finishMS(state)
	m.wantStatus(res, http.StatusSeeOther)
	if loc := res.Header.Get("Location"); strings.Contains(loc, "partial") {
		t.Errorf("a personal account with ReadWrite is a full grant, got %q", loc)
	}
}

func TestMicrosoftPartialGrantRecordsWhatWasGranted(t *testing.T) {
	m := newMSRig(t)
	m.connectMS(nil)
	a := m.accounts()[0]
	m.scope = msMail + " https://graph.microsoft.com/Calendars.ReadWrite openid profile email"
	state, _, _ := m.grantMS(a.ID)
	res, _ := m.finishMS(state)
	m.wantStatus(res, http.StatusSeeOther)
	if !strings.Contains(res.Header.Get("Location"), "partial=1") {
		t.Errorf("redirect %q does not flag the partial grant", res.Header.Get("Location"))
	}
	got := m.account(a.ID)
	cal := got.Services[store.ServiceCalendar]
	if len(cal.Scopes) != 1 || !strings.HasSuffix(cal.Scopes[0], "Calendars.ReadWrite") {
		t.Fatalf("grant = %+v", cal)
	}
	if miss := scopes.Missing(scopes.Microsoft, scopes.Calendar, got.ProviderUID, cal.Scopes); len(miss) != 1 || miss[0] != "Calendars.Read.Shared" {
		t.Errorf("missing = %v", miss)
	}
}

func TestMicrosoftConsentDeniedAndAdminRequired(t *testing.T) {
	m := newMSRig(t)
	m.connectMS(nil)
	a := m.accounts()[0]
	before := m.account(a.ID)
	state, _, _ := m.grantMS(a.ID)
	res, body := m.get(connect.CallbackPathFor("microsoft") + "?" + url.Values{"state": {state}, "error": {"access_denied"}}.Encode())
	if res.StatusCode != http.StatusBadRequest || !strings.Contains(body, "calendar access was declined") {
		t.Fatalf("denied = %d %s", res.StatusCode, body)
	}
	state, _, _ = m.grantMS(a.ID)
	res, body = m.get(connect.CallbackPathFor("microsoft") + "?" + url.Values{"state": {state}, "error": {"invalid_request"},
		"error_description": {"AADSTS65001: The user or administrator has not consented"}}.Encode())
	if res.StatusCode != http.StatusBadRequest || !strings.Contains(body, "administrator") {
		t.Fatalf("admin consent = %d %s", res.StatusCode, body)
	}
	got := m.account(a.ID)
	if _, has := got.Services[store.ServiceCalendar]; has || got.Version != before.Version {
		t.Errorf("account changed: %+v", got)
	}
}

func (m *msRig) account(id string) store.Account { return m.rig.account(id) }

// containsShort is contains ignoring the Graph resource prefix.
func containsShort(list []string, s string) bool {
	for _, v := range list {
		if scopes.Short(v) == s {
			return true
		}
	}
	return false
}
