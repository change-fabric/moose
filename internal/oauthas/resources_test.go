package oauthas_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/change-fabric/moose/internal/accounts/store"
	"github.com/change-fabric/moose/internal/oauthas"
)

var twoResources = []oauthas.Resource{oauthas.EmailResource, oauthas.CalendarResource}

// statusAt posts an MCP initialize to path with the bearer token.
func (e *env) statusAt(path, token string) int {
	e.t.Helper()
	req, _ := http.NewRequest(http.MethodPost, e.base+path, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`))
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

// codeFor runs /authorize for one resource and the dashboard sign-in, and
// returns the authorization code.
func (e *env) codeFor(clientID, challenge, resourcePath string) string {
	e.t.Helper()
	b := e.browser()
	res, body := e.get(b, e.authorizeURL(authParams{clientID: clientID, redirect: claudeRedir, challenge: challenge, state: "st-1", resource: e.base + resourcePath}))
	if res.StatusCode != 200 {
		e.t.Fatalf("authorize for %s = %d %s", resourcePath, res.StatusCode, body)
	}
	e.ap.approve(e.t, e.ap.last().id)
	status, p := e.poll(b)
	if status != 200 || p.Status != "approved" {
		e.t.Fatalf("poll = %d %+v", status, p)
	}
	u, _ := url.Parse(p.Redirect)
	return u.Query().Get("code")
}

// issueFor runs a whole login for one resource (named explicitly) and returns
// the client id and first token pair.
func (e *env) issueFor(resourcePath string) (clientID string, tok tokenOut) {
	e.t.Helper()
	clientID = e.register()
	verifier, challenge := pkce()
	f := codeForm(clientID, claudeRedir, e.codeFor(clientID, challenge, resourcePath), verifier)
	f.Set("resource", e.base+resourcePath)
	status, tok := e.token(f)
	if status != 200 {
		e.t.Fatalf("token for %s = %d %+v", resourcePath, status, tok)
	}
	return clientID, tok
}

func TestEachResourceIssuesItsOwnAudienceAndMetadata(t *testing.T) {
	e := newEnvWith(t, twoResources)
	for _, path := range []string{oauthas.MCPPath, oauthas.CalendarMCPPath} {
		res, body := e.get(http.DefaultClient, e.base+"/.well-known/oauth-protected-resource"+path)
		wantStatus(t, path+" metadata", res.StatusCode, 200)
		var m struct {
			Resource string   `json:"resource"`
			AS       []string `json:"authorization_servers"`
			Name     string   `json:"resource_name"`
		}
		if err := json.Unmarshal([]byte(body), &m); err != nil || m.Resource != e.base+path || len(m.AS) != 1 || m.AS[0] != e.base {
			t.Fatalf("%s metadata = %s", path, body)
		}
		if !strings.HasPrefix(m.Name, "moose ") {
			t.Errorf("%s resource_name = %q", path, m.Name)
		}
		_, tok := e.issueFor(path)
		sess, err := e.st.GetSession(context.Background(), store.HashSecret(tok.Access))
		if err != nil || sess.Audience != e.base+path {
			t.Fatalf("%s token audience = %q, %v", path, sess.Audience, err)
		}
	}
	// The bare metadata location answers for the default resource.
	_, body := e.get(http.DefaultClient, e.base+"/.well-known/oauth-protected-resource")
	if !strings.Contains(body, e.base+oauthas.MCPPath) {
		t.Errorf("bare PRM = %s, want the email resource", body)
	}
}

func TestTokenForOneResourceIsRejectedOnTheOther(t *testing.T) {
	e := newEnvWith(t, twoResources)
	_, emailTok := e.issueFor(oauthas.MCPPath)
	_, calTok := e.issueFor(oauthas.CalendarMCPPath)

	wantStatus(t, "email token at email", e.statusAt(oauthas.MCPPath, emailTok.Access), 200)
	wantStatus(t, "calendar token at calendar", e.statusAt(oauthas.CalendarMCPPath, calTok.Access), 200)
	wantStatus(t, "email token at calendar", e.statusAt(oauthas.CalendarMCPPath, emailTok.Access), 401)
	wantStatus(t, "calendar token at email", e.statusAt(oauthas.MCPPath, calTok.Access), 401)
	wantStatus(t, "no token at calendar", e.statusAt(oauthas.CalendarMCPPath, ""), 401)
}

func TestChallengePointsAtTheResourcesOwnMetadata(t *testing.T) {
	e := newEnvWith(t, twoResources)
	for _, path := range []string{oauthas.MCPPath, oauthas.CalendarMCPPath} {
		req, _ := http.NewRequest(http.MethodPost, e.base+path, strings.NewReader(`{}`))
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		got := res.Header.Get("WWW-Authenticate")
		if res.StatusCode != 401 || !strings.Contains(got, "/.well-known/oauth-protected-resource"+path) {
			t.Errorf("%s challenge = %d %q", path, res.StatusCode, got)
		}
	}
}

func TestRefreshAndCodeStayOnTheirResource(t *testing.T) {
	e := newEnvWith(t, twoResources)
	clientID, cal := e.issueFor(oauthas.CalendarMCPPath)

	// Refresh naming the other resource is refused, and the family still works.
	f := refreshForm(clientID, cal.Refresh)
	f.Set("resource", e.base+oauthas.MCPPath)
	status, out := e.token(f)
	wantError(t, "calendar refresh token for email", status, out, "invalid_target")
	f = refreshForm(clientID, cal.Refresh)
	f.Set("resource", e.base+oauthas.CalendarMCPPath)
	status, next := e.token(f)
	wantStatus(t, "calendar refresh for calendar", status, 200)
	sess, err := e.st.GetSession(context.Background(), store.HashSecret(next.Access))
	if err != nil || sess.Audience != e.base+oauthas.CalendarMCPPath {
		t.Fatalf("refreshed audience = %q, %v", sess.Audience, err)
	}
	wantStatus(t, "refreshed calendar token at email", e.statusAt(oauthas.MCPPath, next.Access), 401)

	// A code issued for calendar cannot be redeemed for email.
	verifier, challenge := pkce()
	f = codeForm(clientID, claudeRedir, e.codeFor(clientID, challenge, oauthas.CalendarMCPPath), verifier)
	f.Set("resource", e.base+oauthas.MCPPath)
	status, out = e.token(f)
	wantError(t, "calendar code redeemed for email", status, out, "invalid_target")
}

func TestAuthorizeResourceRules(t *testing.T) {
	e := newEnvWith(t, twoResources)
	clientID := e.register()
	_, challenge := pkce()

	// An unlisted resource and a path-only value are refused through the redirect.
	for _, res := range []string{"https://other.example/calendar/mcp", e.base + "/drive/mcp", "/calendar/mcp", e.base + "/calendar/mcp/"} {
		resp, err := e.browser().Get(e.authorizeURL(authParams{clientID: clientID, redirect: claudeRedir, challenge: challenge, state: "s", resource: res}))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		loc := resp.Header.Get("Location")
		if resp.StatusCode/100 != 3 || !strings.Contains(loc, "error=invalid_target") {
			t.Errorf("resource %q = %d %q, want a redirect with invalid_target", res, resp.StatusCode, loc)
		}
	}

	// Two different resources in one request are refused: a grant is for one.
	q := e.authorizeURL(authParams{clientID: clientID, redirect: claudeRedir, challenge: challenge, state: "s"})
	q += "&resource=" + url.QueryEscape(e.base+oauthas.MCPPath) + "&resource=" + url.QueryEscape(e.base+oauthas.CalendarMCPPath)
	resp, err := e.browser().Get(q)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if !strings.Contains(resp.Header.Get("Location"), "error=invalid_target") {
		t.Errorf("two resources = %d %q, want invalid_target", resp.StatusCode, resp.Header.Get("Location"))
	}

	// No resource means the default, email.
	_, tok := e.issue()
	sess, err := e.st.GetSession(context.Background(), store.HashSecret(tok.Access))
	if err != nil || sess.Audience != e.base+oauthas.MCPPath {
		t.Fatalf("default audience = %q, %v", sess.Audience, err)
	}
	wantStatus(t, "default token at calendar", e.statusAt(oauthas.CalendarMCPPath, tok.Access), 401)
}

func TestConsentPageNamesTheResource(t *testing.T) {
	e := newEnvWith(t, twoResources)
	clientID := e.register()
	_, challenge := pkce()
	_, body := e.get(e.browser(), e.authorizeURL(authParams{clientID: clientID, redirect: claudeRedir, challenge: challenge, state: "s", resource: e.base + oauthas.CalendarMCPPath}))
	if !strings.Contains(body, "moose calendar") || strings.Contains(body, "moose email") {
		t.Errorf("calendar login page does not name the calendar: %s", body)
	}
}

type statusRecorder struct {
	header http.Header
	code   int
}

func (r *statusRecorder) Header() http.Header         { return r.header }
func (r *statusRecorder) Write(b []byte) (int, error) { return len(b), nil }
func (r *statusRecorder) WriteHeader(c int)           { r.code = c }

func TestVerifierProtectsWithoutTheAuthorizationServer(t *testing.T) {
	e := newEnvWith(t, twoResources)
	_, cal := e.issueFor(oauthas.CalendarMCPPath)
	_, em := e.issueFor(oauthas.MCPPath)
	v := oauthas.NewVerifier(e.st, e.base, oauthas.CalendarResource, e.clock.Now)
	h := v.Protect(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) }))
	call := func(tok string) int {
		req, _ := http.NewRequest(http.MethodPost, "/calendar/mcp", nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		rec := &statusRecorder{header: http.Header{}, code: 200}
		h.ServeHTTP(rec, req)
		return rec.code
	}
	if got := call(cal.Access); got != 204 {
		t.Errorf("calendar token through a calendar verifier = %d", got)
	}
	if got := call(em.Access); got != 401 {
		t.Errorf("email token through a calendar verifier = %d, want 401", got)
	}
	e.clock.Advance(oauthas.AccessTTL + time.Minute)
	if got := call(cal.Access); got != 401 {
		t.Errorf("expired calendar token = %d, want 401", got)
	}
}

func TestNewRejectsBadResourceLists(t *testing.T) {
	for name, rs := range map[string][]oauthas.Resource{
		"duplicate path": {oauthas.EmailResource, oauthas.EmailResource},
		"no slash":       {{Path: "email/mcp"}},
		"trailing slash": {{Path: "/email/mcp/"}},
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			_, err := oauthas.New(oauthas.Deps{Store: e.st, Seal: mustSealer(t), Approver: e.ap, ReceiptKey: mustPub(t, e.ap), RPID: testRPID, Owner: "pat", BaseURL: e.base, Resources: rs})
			if err == nil {
				t.Errorf("New accepted %v", rs)
			}
		})
	}
}
