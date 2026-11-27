package oauthas

import (
	"net/http"
	"net/url"
	"time"

	"github.com/change-fabric/moose/internal/accounts/store"
	"github.com/change-fabric/moose/internal/approver/api"
)

// Dashboard login. The Google connect page sits behind a dashboard session, and
// the only way to get one is the same passkey approval the OAuth login uses: a
// dashboard-session request to the approver, bound to a secret polling handle
// the browser holds in a cookie. When the receipt verifies, the poll mints the
// dashboard session instead of an authorization code.
const (
	// LoginPath starts a dashboard login; LoginPollPath is polled by its page.
	LoginPath     = "/connect/login"
	LoginPollPath = "/connect/login/poll"
	// DashboardCookie is the dashboard session cookie the connect page reads.
	DashboardCookie = "moose_session"
	// DashboardClientID names the dashboard in the approval the phone shows.
	// Dynamically registered clients never get this id.
	DashboardClientID = "moose-dashboard"
	// DashboardTTL is how long a dashboard session lasts.
	DashboardTTL = time.Hour
	// connectPath is where a completed login lands.
	connectPath = "/connect/google"

	loginCookie = "moose_login_txn"
)

// login starts a dashboard login and renders the page that waits for the
// owner's passkey approval on a phone.
func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	host := ""
	if u, err := url.Parse(s.issuer); err == nil {
		host = u.Host
	}
	handle := randomToken(32)
	hash := store.HashSecret(handle)
	payload, err := api.Canonical(mustJSON(api.DashboardSessionPayload{
		Owner: s.d.Owner, ClientID: DashboardClientID, ClientName: "moose dashboard (connect email accounts)",
		RedirectURIHost: host, TxnHash: hash,
	}))
	if err != nil {
		s.d.Log.Error("oauthas: canonical login payload", "err", err)
		s.errorPage(w, http.StatusInternalServerError, "Something went wrong", "Try again in a moment.")
		return
	}
	wantSHA := api.SHA256(payload)
	resp, _, err := s.d.Approver.Create(r.Context(), api.CreateBody{
		IdempotencyKey: "dashboard-txn-" + hash, Kind: api.KindDashboardSession, Payload: payload,
	})
	if err != nil || resp.PayloadSHA256 != wantSHA || resp.RequestID == "" || resp.ApprovalURL == "" {
		s.d.Log.Error("oauthas: create login approval", "err", err, "hash_matches", resp.PayloadSHA256 == wantSHA)
		s.errorPage(w, http.StatusBadGateway, "Approval is unavailable", "The approval service did not answer. Try again in a moment.")
		return
	}
	if err := s.d.Store.PutAuthTxn(r.Context(), store.AuthTxn{
		Hash: hash, Owner: s.d.Owner, ClientID: DashboardClientID, RedirectURI: connectPath,
		RequestID: resp.RequestID, PayloadSHA256: wantSHA, Expires: s.d.Now().Add(TxnTTL),
	}); err != nil {
		s.d.Log.Error("oauthas: store login txn", "err", err)
		s.errorPage(w, http.StatusInternalServerError, "Something went wrong", "Try again in a moment.")
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: loginCookie, Value: handle, Path: LoginPath, MaxAge: int(TxnTTL / time.Second),
		HttpOnly: true, Secure: !s.d.InsecureCookies, SameSite: http.SameSiteLaxMode,
	})
	s.waitPage(w, "moose dashboard", host, resp.ApprovalURL, "You are signing in to the moose dashboard to connect email accounts.", LoginPollPath)
}

// loginPoll completes a dashboard login once the approver has approved it and
// the receipt verifies. It answers with the connect page to go to and sets the
// dashboard session cookie, whose value exists only here and, hashed, in the
// store.
func (s *Server) loginPoll(w http.ResponseWriter, r *http.Request) {
	txn, ok := s.completeTxn(w, r, loginCookie, LoginPath, true)
	if !ok {
		return
	}
	now := s.d.Now()
	token := "mds_" + randomToken(32)
	if err := s.d.Store.CreateSession(r.Context(), store.Session{
		Hash: store.HashSecret(token), Kind: store.SessionDashboard, Owner: txn.Owner, ClientID: DashboardClientID,
		Created: now.UTC(), Expires: now.Add(DashboardTTL).UTC(),
	}); err != nil {
		s.d.Log.Error("oauthas: store dashboard session", "err", err)
		writeJSON(w, http.StatusInternalServerError, pollResponse{Status: "error"})
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: DashboardCookie, Value: token, Path: "/connect/", MaxAge: int(DashboardTTL / time.Second),
		HttpOnly: true, Secure: !s.d.InsecureCookies, SameSite: http.SameSiteLaxMode,
	})
	s.clearCookie(w, loginCookie, LoginPath)
	writeJSON(w, http.StatusOK, pollResponse{Status: "approved", Redirect: txn.RedirectURI})
}
