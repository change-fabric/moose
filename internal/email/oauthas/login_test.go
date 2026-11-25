package oauthas_test

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/change-fabric/moose/internal/email/connect"
	"github.com/change-fabric/moose/internal/email/oauthas"
	"github.com/change-fabric/moose/internal/email/store"
)

func (e *env) loginPoll(c *http.Client) (int, pollResult) {
	e.t.Helper()
	res, body := e.get(c, e.base+oauthas.LoginPollPath)
	var p pollResult
	_ = json.Unmarshal([]byte(body), &p)
	return res.StatusCode, p
}

func TestDashboardCookieNameMatchesTheConnectPage(t *testing.T) {
	if oauthas.DashboardCookie != connect.SessionCookie {
		t.Fatalf("login sets %q but the connect page reads %q", oauthas.DashboardCookie, connect.SessionCookie)
	}
}

func TestDashboardLoginMintsASessionTheConnectPageAccepts(t *testing.T) {
	e := newEnv(t)
	b := e.browser()
	res, body := e.get(b, e.base+oauthas.LoginPath)
	wantStatus(t, "login page", res.StatusCode, 200)
	if !strings.Contains(body, `\/connect\/login\/poll`) { // html/template escapes the JS string
		t.Fatalf("the page does not poll %s: %s", oauthas.LoginPollPath, body)
	}
	pl := e.ap.payload(t, e.ap.last().id)
	if pl.ClientID != oauthas.DashboardClientID || pl.Owner != "pat" || pl.ClientName == "" || pl.RedirectURIHost == "" {
		t.Fatalf("approval payload = %+v", pl)
	}
	var handle *http.Cookie
	u, _ := url.Parse(e.base + oauthas.LoginPath)
	for _, c := range b.Jar.Cookies(u) {
		if c.Name == "moose_login_txn" {
			handle = c
		}
	}
	if handle == nil || pl.TxnHash != store.HashSecret(handle.Value) {
		t.Fatalf("txn_hash must be sha256 of the browser's handle: %+v", pl)
	}

	// Pending: no session yet.
	status, p := e.loginPoll(b)
	if status != 200 || p.Status != "pending" {
		t.Fatalf("pending poll = %d %+v", status, p)
	}
	e.ap.approve(t, e.ap.last().id)
	status, p = e.loginPoll(b)
	if status != 200 || p.Status != "approved" || p.Redirect != "/connect/google" {
		t.Fatalf("approved poll = %d %+v", status, p)
	}
	var sess *http.Cookie
	cu, _ := url.Parse(e.base + "/connect/google")
	for _, c := range b.Jar.Cookies(cu) {
		if c.Name == oauthas.DashboardCookie {
			sess = c
		}
	}
	if sess == nil {
		t.Fatal("no dashboard session cookie")
	}
	// The cookie is the same thing connect.SessionAuth accepts.
	req, _ := http.NewRequest("GET", e.base+"/connect/google", nil)
	req.AddCookie(sess)
	owner, ok := connect.SessionAuth{Store: e.st, Now: e.clock.Now}.Owner(req)
	if !ok || owner != "pat" {
		t.Fatalf("SessionAuth = %q, %v", owner, ok)
	}
	// It expires.
	e.clock.Advance(oauthas.DashboardTTL + time.Second)
	if _, ok := (connect.SessionAuth{Store: e.st, Now: e.clock.Now}).Owner(req); ok {
		t.Fatal("dashboard session outlived its TTL")
	}
	// A login completes once.
	if status, _ := e.loginPoll(b); status != 404 {
		t.Fatalf("second poll = %d, want 404", status)
	}
}

func TestDashboardLoginSessionCookieAttributes(t *testing.T) {
	e := newEnv(t)
	b := e.browser()
	e.get(b, e.base+oauthas.LoginPath)
	e.ap.approve(t, e.ap.last().id)
	res, _ := e.get(b, e.base+oauthas.LoginPollPath)
	var c *http.Cookie
	for _, k := range res.Cookies() {
		if k.Name == oauthas.DashboardCookie {
			c = k
		}
	}
	if c == nil || !c.HttpOnly || c.SameSite != http.SameSiteLaxMode || c.Path != "/connect/" || c.MaxAge <= 0 || len(c.Value) < 40 {
		t.Fatalf("cookie = %+v", c)
	}
}

func TestDashboardLoginPollNeedsItsOwnCookie(t *testing.T) {
	e := newEnv(t)
	b := e.browser()
	e.get(b, e.base+oauthas.LoginPath)
	e.ap.approve(t, e.ap.last().id)
	if res, body := e.get(e.browser(), e.base+oauthas.LoginPollPath); res.StatusCode != 404 || strings.Contains(body, "approved") {
		t.Fatalf("poll with no cookie = %d %s", res.StatusCode, body)
	}
}

func TestOAuthPollCannotCompleteADashboardLoginOrTheReverse(t *testing.T) {
	e := newEnv(t)
	// Take the dashboard handle and present it to the OAuth poll under that cookie's name.
	b := e.browser()
	e.get(b, e.base+oauthas.LoginPath)
	e.ap.approve(t, e.ap.last().id)
	u, _ := url.Parse(e.base + oauthas.LoginPath)
	var handle string
	for _, c := range b.Jar.Cookies(u) {
		if c.Name == "moose_login_txn" {
			handle = c.Value
		}
	}
	req, _ := http.NewRequest("GET", e.base+oauthas.PollPath, nil)
	req.AddCookie(&http.Cookie{Name: "moose_oauth_txn", Value: handle})
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 404 {
		t.Fatalf("OAuth poll with a dashboard handle = %d, want 404", res.StatusCode)
	}

	// And an OAuth handle presented to the dashboard poll.
	clientID := e.register()
	_, challenge := pkce()
	ob := e.browser()
	e.get(ob, e.authorizeURL(authParams{clientID: clientID, redirect: claudeRedir, challenge: challenge}))
	e.ap.approve(t, e.ap.last().id)
	var oh string
	au, _ := url.Parse(e.base + oauthas.AuthorizePath)
	for _, c := range ob.Jar.Cookies(au) {
		if c.Name == "moose_oauth_txn" {
			oh = c.Value
		}
	}
	req, _ = http.NewRequest("GET", e.base+oauthas.LoginPollPath, nil)
	req.AddCookie(&http.Cookie{Name: "moose_login_txn", Value: oh})
	res, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 404 {
		t.Fatalf("dashboard poll with an OAuth handle = %d, want 404", res.StatusCode)
	}
}

func TestDashboardLoginReceiptForAnotherTransactionIsRejected(t *testing.T) {
	e := newEnv(t)
	a, b := e.browser(), e.browser()
	e.get(a, e.base+oauthas.LoginPath)
	e.get(b, e.base+oauthas.LoginPath)
	idB := e.ap.last().id
	e.ap.approve(t, idB)
	// A's browser is still pending: B's approval does not help it.
	if _, p := e.loginPoll(a); p.Status != "pending" {
		t.Fatalf("A's poll = %+v", p)
	}
	if _, p := e.loginPoll(b); p.Status != "approved" {
		t.Fatalf("B's poll = %+v", p)
	}
}

func TestDashboardLoginFailsClosedWhenTheApproverIsDown(t *testing.T) {
	e := newEnv(t)
	e.ap.failCreate = true
	res, _ := e.get(e.browser(), e.base+oauthas.LoginPath)
	wantStatus(t, "login with the approver down", res.StatusCode, 502)
}

func TestDashboardLoginDeniedAndExpired(t *testing.T) {
	for _, st := range []string{"denied", "expired"} {
		e := newEnv(t)
		b := e.browser()
		e.get(b, e.base+oauthas.LoginPath)
		e.ap.setStatus(e.ap.last().id, st)
		status, p := e.loginPoll(b)
		if status != 200 || p.Status != st || p.Redirect != "" {
			t.Errorf("%s poll = %d %+v", st, status, p)
		}
	}
}
