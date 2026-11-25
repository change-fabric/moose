package oauthas

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/change-fabric/moose/internal/approver/api"
	"github.com/change-fabric/moose/internal/approver/receipt"
	"github.com/change-fabric/moose/internal/email/store"
	"github.com/change-fabric/moose/internal/webui"
)

var challengeRE = regexp.MustCompile(`^[A-Za-z0-9_-]{43}$`)

const maxState = 512

// authorize starts a browser transaction. It validates the request, asks the
// approver for a dashboard-session approval bound to a fresh secret polling
// handle, hands the handle to the browser as a cookie, and renders the
// interstitial page that waits for the owner's passkey approval on a phone.
//
// Until the client and redirect URI are known good, errors are shown on this
// page and never redirected, so an unregistered redirect URI is never used.
func (s *Server) authorize(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	for _, k := range []string{"client_id", "redirect_uri", "response_type", "code_challenge", "code_challenge_method", "state", "scope"} {
		if len(q[k]) > 1 {
			s.errorPage(w, http.StatusBadRequest, "Invalid request", "The parameter "+k+" appears more than once.")
			return
		}
	}
	client, err := s.d.Store.GetOAuthClient(r.Context(), q.Get("client_id"))
	switch {
	case errors.Is(err, store.ErrNotFound):
		s.errorPage(w, http.StatusBadRequest, "Unknown application", "The application is not registered here.")
		return
	case err != nil:
		s.d.Log.Error("oauthas: load client", "err", err)
		s.errorPage(w, http.StatusInternalServerError, "Something went wrong", "Try again in a moment.")
		return
	}
	redirect := q.Get("redirect_uri")
	if !MatchRedirect(client.RedirectURIs, redirect) {
		s.errorPage(w, http.StatusBadRequest, "Redirect not allowed", "That redirect address is not registered for this application.")
		return
	}
	state := q.Get("state")
	fail := func(code, desc string) { s.redirectError(w, r, redirect, state, code, desc) }

	switch {
	case len(state) > maxState:
		fail("invalid_request", "state is too long")
		return
	case q.Get("response_type") != "code":
		fail("unsupported_response_type", "only response_type=code is supported")
		return
	case q.Get("code_challenge_method") != "S256":
		fail("invalid_request", "code_challenge_method must be S256")
		return
	case !challengeRE.MatchString(q.Get("code_challenge")):
		fail("invalid_request", "code_challenge must be a 43 character base64url S256 challenge")
		return
	}
	resource, err := s.resourceFrom(q["resource"])
	if err != nil {
		fail("invalid_target", err.Error())
		return
	}
	scope, err := parseScope(q.Get("scope"))
	if err != nil {
		fail("invalid_scope", err.Error())
		return
	}

	ru, _ := url.Parse(redirect) // MatchRedirect parsed it
	handle := randomToken(32)
	hash := store.HashSecret(handle)
	payload, err := api.Canonical(mustJSON(api.DashboardSessionPayload{
		Owner: s.d.Owner, ClientID: client.ID, ClientName: client.Name, RedirectURIHost: ru.Host, TxnHash: hash,
	}))
	if err != nil {
		s.d.Log.Error("oauthas: canonical payload", "err", err)
		s.errorPage(w, http.StatusInternalServerError, "Something went wrong", "Try again in a moment.")
		return
	}
	wantSHA := api.SHA256(payload)
	resp, _, err := s.d.Approver.Create(r.Context(), api.CreateBody{
		IdempotencyKey: "oauth-txn-" + hash, Kind: api.KindDashboardSession, Payload: payload,
	})
	if err != nil || resp.PayloadSHA256 != wantSHA || resp.RequestID == "" || resp.ApprovalURL == "" {
		s.d.Log.Error("oauthas: create approval", "err", err, "hash_matches", resp.PayloadSHA256 == wantSHA)
		s.errorPage(w, http.StatusBadGateway, "Approval is unavailable", "The approval service did not answer. Try again in a moment.")
		return
	}
	now := s.d.Now()
	if err := s.d.Store.PutAuthTxn(r.Context(), store.AuthTxn{
		Hash: hash, Owner: s.d.Owner, ClientID: client.ID, RedirectURI: redirect, CodeChallenge: q.Get("code_challenge"),
		Resource: resource, Scope: scope, State: state, RequestID: resp.RequestID, PayloadSHA256: wantSHA,
		Expires: now.Add(TxnTTL),
	}); err != nil {
		s.d.Log.Error("oauthas: store txn", "err", err)
		s.errorPage(w, http.StatusInternalServerError, "Something went wrong", "Try again in a moment.")
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: txnCookie, Value: handle, Path: AuthorizePath, MaxAge: int(TxnTTL / time.Second),
		HttpOnly: true, Secure: !s.d.InsecureCookies, SameSite: http.SameSiteLaxMode,
	})
	s.waitPage(w, client.Name, ru.Host, resp.ApprovalURL, "An application is asking to connect to your moose email.", PollPath)
}

// resourceFrom checks the RFC 8707 resource indicators. Absent means the one
// resource this server protects; anything else must equal it exactly.
func (s *Server) resourceFrom(vals []string) (string, error) {
	for _, v := range vals {
		if v != s.resource {
			return "", fmt.Errorf("resource must be %s", s.resource)
		}
	}
	return s.resource, nil
}

func parseScope(raw string) (string, error) {
	if strings.TrimSpace(raw) == "" {
		return ScopeMCP, nil
	}
	for _, sc := range strings.Fields(raw) {
		if sc != ScopeMCP {
			return "", fmt.Errorf("unsupported scope %q", sc)
		}
	}
	return ScopeMCP, nil
}

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

// redirectError sends the user agent back to the client with an OAuth error.
func (s *Server) redirectError(w http.ResponseWriter, r *http.Request, redirect, state, code, desc string) {
	u, err := url.Parse(redirect)
	if err != nil {
		s.errorPage(w, http.StatusBadRequest, "Invalid request", desc)
		return
	}
	q := u.Query()
	q.Set("error", code)
	q.Set("error_description", desc)
	if state != "" {
		q.Set("state", state)
	}
	q.Set("iss", s.issuer)
	u.RawQuery = q.Encode()
	http.Redirect(w, r, u.String(), http.StatusSeeOther)
}

// ---- poll ----

type pollResponse struct {
	Status   string `json:"status"`
	Redirect string `json:"redirect,omitempty"`
	Note     string `json:"note,omitempty"`
}

// poll reports where the dashboard approval stands. It only answers a browser
// holding the transaction's secret handle. Once the approver says approved, it
// verifies the signed receipt against the stored request, takes the
// transaction (only one poll can), consumes the approval, and answers with the
// redirect carrying a fresh single-use authorization code.
func (s *Server) poll(w http.ResponseWriter, r *http.Request) {
	txn, ok := s.completeTxn(w, r, txnCookie, AuthorizePath, false)
	if !ok {
		return
	}
	ctx, now := r.Context(), s.d.Now()
	code := "mac_" + randomToken(32)
	if err := s.d.Store.CreateAuthCode(ctx, store.AuthCode{
		Hash: store.HashSecret(code), Owner: txn.Owner, ClientID: txn.ClientID, RedirectURI: txn.RedirectURI,
		CodeChallenge: txn.CodeChallenge, Resource: txn.Resource, FamilyID: "fam_" + randomToken(16),
		Created: now.UTC(), Expires: now.Add(CodeTTL).UTC(),
	}); err != nil {
		s.d.Log.Error("oauthas: store code", "err", err)
		writeJSON(w, http.StatusInternalServerError, pollResponse{Status: "error"})
		return
	}
	u, err := url.Parse(txn.RedirectURI)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, pollResponse{Status: "error"})
		return
	}
	q := u.Query()
	q.Set("code", code)
	if txn.State != "" {
		q.Set("state", txn.State)
	}
	q.Set("iss", s.issuer)
	u.RawQuery = q.Encode()
	s.clearCookie(w, txnCookie, AuthorizePath)
	writeJSON(w, http.StatusOK, pollResponse{Status: "approved", Redirect: u.String()})
}

// completeTxn is the part of a poll the OAuth login and the dashboard login
// share. It writes the response itself and returns false unless the approver
// has approved the transaction named by the browser's cookie, the receipt
// verifies, this poll took the transaction and the approval was consumed.
// dashboard says which kind of transaction the endpoint serves, so a handle
// from one flow never completes the other.
func (s *Server) completeTxn(w http.ResponseWriter, r *http.Request, cookie, cookiePath string, dashboard bool) (store.AuthTxn, bool) {
	c, err := r.Cookie(cookie)
	if err != nil || c.Value == "" {
		writeJSON(w, http.StatusNotFound, pollResponse{Status: "unknown"})
		return store.AuthTxn{}, false
	}
	ctx := r.Context()
	now := s.d.Now()
	hash := store.HashSecret(c.Value)
	txn, err := s.d.Store.GetAuthTxn(ctx, hash, now)
	if errors.Is(err, store.ErrNotFound) || (err == nil && (txn.ClientID == DashboardClientID) != dashboard) {
		writeJSON(w, http.StatusNotFound, pollResponse{Status: "unknown"})
		return store.AuthTxn{}, false
	}
	if err != nil {
		s.d.Log.Error("oauthas: poll load txn", "err", err)
		writeJSON(w, http.StatusInternalServerError, pollResponse{Status: "error"})
		return store.AuthTxn{}, false
	}
	st, err := s.d.Approver.Status(ctx, txn.RequestID)
	if err != nil {
		s.d.Log.Warn("oauthas: poll approver status", "err", err)
		writeJSON(w, http.StatusOK, pollResponse{Status: "pending", Note: "approval service unreachable"})
		return store.AuthTxn{}, false
	}
	switch st.Status {
	case "pending":
		writeJSON(w, http.StatusOK, pollResponse{Status: "pending"})
		return store.AuthTxn{}, false
	case "denied", "expired":
		_, _ = s.d.Store.TakeAuthTxn(ctx, hash, now)
		s.clearCookie(w, cookie, cookiePath)
		writeJSON(w, http.StatusOK, pollResponse{Status: st.Status})
		return store.AuthTxn{}, false
	case "approved":
	default: // consumed, or anything unknown: this transaction cannot complete
		writeJSON(w, http.StatusConflict, pollResponse{Status: "rejected"})
		return store.AuthTxn{}, false
	}
	if st.RequestID != txn.RequestID || st.Receipt == nil {
		writeJSON(w, http.StatusForbidden, pollResponse{Status: "rejected"})
		return store.AuthTxn{}, false
	}
	if err := receipt.Verify(s.d.ReceiptKey, *st.Receipt, receipt.Want{
		RequestID: txn.RequestID, Kind: api.KindDashboardSession, PayloadSHA256: txn.PayloadSHA256,
		RPID: s.d.RPID, Now: now,
	}); err != nil {
		s.d.Log.Warn("oauthas: poll receipt rejected", "err", err)
		writeJSON(w, http.StatusForbidden, pollResponse{Status: "rejected"})
		return store.AuthTxn{}, false
	}
	if _, err := s.d.Store.TakeAuthTxn(ctx, hash, now); err != nil {
		// Another poll took it, or it expired.
		writeJSON(w, http.StatusNotFound, pollResponse{Status: "unknown"})
		return store.AuthTxn{}, false
	}
	if _, err := s.d.Approver.Consume(ctx, txn.RequestID, "oauth:"+hash); err != nil {
		s.d.Log.Warn("oauthas: consume approval", "err", err)
		s.clearCookie(w, cookie, cookiePath)
		writeJSON(w, http.StatusBadGateway, pollResponse{Status: "error", Note: "approval could not be completed; start again"})
		return store.AuthTxn{}, false
	}
	return txn, true
}

func (s *Server) clearCookie(w http.ResponseWriter, name, path string) {
	http.SetCookie(w, &http.Cookie{Name: name, Value: "", Path: path, MaxAge: -1,
		HttpOnly: true, Secure: !s.d.InsecureCookies, SameSite: http.SameSiteLaxMode})
}

// ---- pages ----

var pageTmpl = template.Must(template.New("wait").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>Sign in to moose</title>
<style>
` + webui.Tokens + webui.Base + `
body{margin:0;background:var(--bg);color:var(--fg);font:16px/1.5 system-ui,sans-serif}
main{max-width:30rem;margin:0 auto;padding:2rem 16px}
h1{font-size:1.4rem;margin:0 0 .5rem}
.card{background:var(--card);border:1px solid var(--line);border-radius:12px;padding:1rem;margin:1rem 0}
dl{margin:0;display:grid;grid-template-columns:auto 1fr;gap:.25rem 1rem}dt{color:var(--muted)}dd{margin:0;overflow-wrap:anywhere}
.qr{background:#fff;padding:12px;border-radius:8px;width:fit-content;margin:0 auto}.qr svg{display:block;width:220px;height:220px}
a{color:var(--accent)}#status{font-weight:600}
.btn{display:inline-flex;align-items:center;justify-content:center;min-height:44px;padding:0 1.1rem;border-radius:8px;background:var(--accent);color:var(--accent-ink);font-weight:600;text-decoration:none}
.here{display:none}
@media (max-width:40rem),(pointer:coarse){.here{display:block}.here .btn{width:100%}.qr,.scan{display:none}}
</style></head><body><main>
<h1>Approve this sign-in with your passkey</h1>
<p>{{.Lead}} Check the details, then approve with your passkey.</p>
<p class="here"><a class="btn" href="{{.URL}}" rel="noopener noreferrer" target="_blank">Approve on this device</a></p>
<div class="card"><dl><dt>Application</dt><dd>{{.Client}}</dd><dt>Returns to</dt><dd>{{.Host}}</dd></dl></div>
<div class="qr">{{.QR}}</div>
<p class="scan">On a computer? Scan the code with your phone, or <a href="{{.URL}}" rel="noopener noreferrer" target="_blank">open the approval page</a> on a device that holds your passkey.</p>
<p id="status" role="status" aria-live="polite">Waiting for approval</p>
<noscript><p>This page needs JavaScript to notice the approval. Enable it, then reload.</p></noscript>
<script nonce="{{.Nonce}}">
(function(){
  var el=document.getElementById('status');
  function stop(t){el.textContent=t;}
  function tick(){
    fetch('{{.Poll}}',{credentials:'same-origin',cache:'no-store'}).then(function(r){return r.json().catch(function(){return {};});}).then(function(j){
      if(j.status==='approved'&&j.redirect){stop('Approved. Returning to the application');window.location.replace(j.redirect);return;}
      if(j.status==='denied'){stop('The sign-in was denied. You can close this page.');return;}
      if(j.status==='expired'){stop('The approval expired. Start again from the application.');return;}
      if(j.status==='unknown'||j.status==='rejected'||j.status==='error'){stop('This sign-in can no longer complete. Start again from the application.');return;}
      setTimeout(tick,2000);
    }).catch(function(){setTimeout(tick,4000);});
  }
  setTimeout(tick,1500);
})();
</script></main></body></html>`))

var errTmpl = template.Must(template.New("err").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>{{.Title}}</title>
<style>` + webui.Tokens + webui.Base + `body{font:16px/1.5 system-ui,sans-serif;margin:0;background:var(--bg);color:var(--fg)}main{max-width:30rem;margin:0 auto;padding:2rem 16px}</style>
</head><body><main><h1>{{.Title}}</h1><p>{{.Detail}}</p></main></body></html>`))

func (s *Server) pageHeaders(w http.ResponseWriter, nonce string) {
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	csp := "default-src 'none'; style-src 'unsafe-inline'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'"
	if nonce != "" {
		csp += "; script-src 'nonce-" + nonce + "'; connect-src 'self'"
	}
	h.Set("Content-Security-Policy", csp)
	h.Set("X-Frame-Options", "DENY")
}

func (s *Server) waitPage(w http.ResponseWriter, client, host, approvalURL, lead, poll string) {
	nonce := randomToken(16)
	qr, err := qrSVG(approvalURL)
	if err != nil {
		s.d.Log.Error("oauthas: render qr", "err", err)
		s.errorPage(w, http.StatusInternalServerError, "Something went wrong", "Try again in a moment.")
		return
	}
	var buf bytes.Buffer
	err = pageTmpl.Execute(&buf, map[string]any{
		"Client": client, "Host": host, "URL": approvalURL, "QR": template.HTML(qr), //nolint:gosec // generated here from a bit matrix
		"Nonce": nonce, "Poll": poll, "Lead": lead,
	})
	if err != nil {
		s.d.Log.Error("oauthas: render page", "err", err)
		s.errorPage(w, http.StatusInternalServerError, "Something went wrong", "Try again in a moment.")
		return
	}
	s.pageHeaders(w, nonce)
	_, _ = w.Write(buf.Bytes())
}

func (s *Server) errorPage(w http.ResponseWriter, status int, title, detail string) {
	var buf bytes.Buffer
	_ = errTmpl.Execute(&buf, map[string]string{"Title": title, "Detail": detail})
	s.pageHeaders(w, "")
	w.WriteHeader(status)
	_, _ = w.Write(buf.Bytes())
}
