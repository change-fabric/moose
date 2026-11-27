// Package connect serves the Google connect page, /connect/google: the one
// hosted page moose-email has besides login. It runs the Google OAuth web flow
// (offline access, scopes gmail.readonly and gmail.compose, state and PKCE),
// stores each connected mailbox as an account that is read-only until send is
// enabled, and lists every account with the controls R9 asks for.
//
// Enabling send needs a passkey approval, so the Enable send button creates an
// `account-send-enable` request with the approver and sends the browser there.
// Disabling send, reconnecting and disconnecting apply directly.
//
// Everything is behind a dashboard session; every POST also checks a
// double-submit CSRF token and the request origin.
package connect

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"

	"github.com/change-fabric/moose/internal/accounts/envelope"
	"github.com/change-fabric/moose/internal/accounts/store"
	"github.com/change-fabric/moose/internal/email/provider"
)

// Paths served by Handler. Mount the handler at Prefix.
const (
	Prefix       = "/connect/"
	PagePath     = "/connect/google"
	StartPath    = "/connect/google/start"
	CallbackPath = "/connect/google/callback"
)

// Google scopes. Send is gated in code, so compose is requested up front.
const (
	ScopeReadonly = "https://www.googleapis.com/auth/gmail.readonly"
	ScopeCompose  = "https://www.googleapis.com/auth/gmail.compose"
)

// SessionCookie names the dashboard session cookie the approver sets.
const SessionCookie = "moose_session"

const (
	csrfCookie = "moose_csrf"
	// LoginPath is the dashboard sign-in, served by the authorization server.
	LoginPath     = "/connect/login"
	providerGmail = "gmail"
)

// Profile is what moose learns about a freshly authorized mailbox.
type Profile struct {
	Address     string
	DisplayName string
	Caps        provider.Capabilities
}

// ProfileFunc looks up the mailbox behind a token source.
type ProfileFunc func(ctx context.Context, ts oauth2.TokenSource) (Profile, error)

// Authenticator resolves the signed-in owner of a request.
type Authenticator interface {
	Owner(r *http.Request) (owner string, ok bool)
}

// ApprovalRequest is what the connect page asks the approver to create.
type ApprovalRequest struct {
	IdempotencyKey string
	Kind           string
	// Payload is the canonical JSON the approval signs over.
	Payload []byte
	// Preview is shown on the approval page.
	Preview map[string]string
}

// ApprovalResponse is the approver's answer.
type ApprovalResponse struct {
	RequestID     string
	PayloadSHA256 string
	ApprovalURL   string
	ExpiresAt     time.Time
}

// Approver creates approval requests. The HMAC client for moose-approver
// implements it; the connect page only needs this one call.
type Approver interface {
	CreateRequest(ctx context.Context, req ApprovalRequest) (ApprovalResponse, error)
}

// Deps wires a Server.
type Deps struct {
	Store store.Store
	Seal  envelope.Sealer
	// OAuth is the Google web client; build it with GoogleConfig.
	OAuth    *oauth2.Config
	Profile  ProfileFunc
	Auth     Authenticator
	Approver Approver // nil leaves Enable send unavailable
	// BaseURL is the public origin, used for origin checks and to confine approval redirects.
	BaseURL string
	// InsecureCookies drops the Secure cookie flag, for http://localhost in dev.
	InsecureCookies bool
	// HTTPClient is used for the token exchange (tests point it at a stub).
	HTTPClient *http.Client
	Now        func() time.Time
	Log        *slog.Logger
}

// GoogleConfig builds the OAuth web client for a tenant's Google credentials.
func GoogleConfig(clientID, clientSecret, baseURL string) *oauth2.Config {
	return &oauth2.Config{
		ClientID:     clientID,
		ClientSecret: clientSecret,
		Endpoint:     google.Endpoint,
		RedirectURL:  strings.TrimRight(baseURL, "/") + CallbackPath,
		Scopes:       []string{ScopeReadonly, ScopeCompose},
	}
}

// Server is the connect page.
type Server struct {
	d        Deps
	adapters *Registry
	origin   string
	baseURL  *url.URL
}

// New validates deps and returns the server.
func New(d Deps) (*Server, error) {
	switch {
	case d.Store == nil || d.Seal == nil || d.OAuth == nil || d.Profile == nil || d.Auth == nil:
		return nil, errors.New("connect: Store, Seal, OAuth, Profile and Auth are required")
	}
	u, err := url.Parse(d.BaseURL)
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("connect: BaseURL %q is not an origin", d.BaseURL)
	}
	if d.Now == nil {
		d.Now = time.Now
	}
	if d.Log == nil {
		d.Log = slog.Default()
	}
	adapters := NewRegistry(NewGoogleAdapter(d.OAuth, d.Profile))
	return &Server{d: d, adapters: adapters, baseURL: u, origin: u.Scheme + "://" + u.Host}, nil
}

// Handler serves every connect route. Mount it at Prefix.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+PagePath, s.page)
	mux.HandleFunc("POST "+StartPath, s.start)
	mux.HandleFunc("GET "+CallbackPath, s.callback)
	mux.HandleFunc("POST "+Prefix+"accounts/{id}/enable-send", s.enableSend)
	mux.HandleFunc("POST "+Prefix+"accounts/{id}/disable-send", s.disableSend)
	mux.HandleFunc("POST "+Prefix+"accounts/{id}/disconnect", s.disconnect)
	return secure(mux)
}

func secure(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Cache-Control", "no-store")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "same-origin")
		h.Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; base-uri 'none'; frame-ancestors 'none'")
		next.ServeHTTP(w, r)
	})
}

func (s *Server) owner(w http.ResponseWriter, r *http.Request) (string, bool) {
	owner, ok := s.d.Auth.Owner(r)
	if !ok || owner == "" {
		// A plain page load goes straight to sign-in, which returns here once
		// approved. A form post cannot be replayed, so it explains instead.
		if r.Method == http.MethodGet {
			http.Redirect(w, r, LoginPath, http.StatusSeeOther)
			return "", false
		}
		s.failWith(w, http.StatusUnauthorized, "Your session ended",
			"Sign in again with your passkey, then retry. Nothing was changed.", LoginPath, "Sign in")
		return "", false
	}
	return owner, true
}

// ownerPost authenticates a mutating request and checks CSRF and origin.
func (s *Server) ownerPost(w http.ResponseWriter, r *http.Request) (string, bool) {
	owner, ok := s.owner(w, r)
	if !ok {
		return "", false
	}
	if !s.csrfOK(r) {
		s.fail(w, http.StatusForbidden, "Request refused", "The form token was missing or did not match. Reload the page and try again.")
		return "", false
	}
	return owner, true
}

func (s *Server) csrfOK(r *http.Request) bool {
	// Referrer-Policy is same-origin, so a same-origin form post carries a real
	// Origin; "null" (or any other value) is refused.
	if o := r.Header.Get("Origin"); o != "" && o != s.origin {
		s.d.Log.Warn("connect: csrf refused", "reason", "origin", "origin", o)
		return false
	}
	if sf := r.Header.Get("Sec-Fetch-Site"); sf != "" && sf != "same-origin" && sf != "none" {
		return false
	}
	c, err := r.Cookie(csrfCookie)
	if err != nil || c.Value == "" {
		s.d.Log.Warn("connect: csrf refused", "reason", "no cookie")
		return false
	}
	return subtle.ConstantTimeCompare([]byte(c.Value), []byte(r.PostFormValue("csrf"))) == 1
}

func (s *Server) cookie(name, value string, maxAge int, sameSite http.SameSite) *http.Cookie {
	return &http.Cookie{Name: name, Value: value, Path: Prefix, MaxAge: maxAge, HttpOnly: true,
		Secure: !s.d.InsecureCookies, SameSite: sameSite}
}

func randomToken(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(fmt.Sprintf("connect: random: %v", err))
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// ---- start and callback ----

func (s *Server) start(w http.ResponseWriter, r *http.Request) {
	owner, ok := s.ownerPost(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	accts, err := s.d.Store.ListAccounts(ctx, owner)
	if err != nil {
		s.internal(w, "list accounts", err)
		return
	}
	st := store.ConnectState{Owner: owner}
	if id := r.PostFormValue("account_id"); id != "" {
		if a, ok := findByID(accts, id); !ok || a.Owner != owner {
			s.fail(w, http.StatusNotFound, "Account not found", "That account is not connected here.")
			return
		}
		st.ReconnectID = id
	}
	state := randomToken(32)
	verifier := oauth2.GenerateVerifier()
	st.Hash, st.Verifier = store.HashSecret(state), verifier
	st.Expires = s.d.Now().Add(store.ConnectStateTTL)
	if err := s.d.Store.PutConnectState(ctx, st); err != nil {
		s.internal(w, "store connect state", err)
		return
	}
	opts := []oauth2.AuthCodeOption{
		oauth2.AccessTypeOffline,
		oauth2.S256ChallengeOption(verifier),
		oauth2.SetAuthURLParam("prompt", "consent"), // always return a refresh token
	}
	if a, ok := findByID(accts, st.ReconnectID); ok {
		opts = append(opts, oauth2.SetAuthURLParam("login_hint", a.Address))
	}
	ad, err := s.adapters.Get(ProviderGoogle)
	if err != nil {
		s.internal(w, "connect adapter", err)
		return
	}
	http.Redirect(w, r, ad.OAuthConfig(nil).AuthCodeURL(state, opts...), http.StatusSeeOther)
}

func (s *Server) callback(w http.ResponseWriter, r *http.Request) {
	owner, ok := s.owner(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	q := r.URL.Query()
	state := q.Get("state")
	if state == "" {
		s.fail(w, http.StatusBadRequest, "Connect link invalid", "The connect request had no state. Start again from the connect page.")
		return
	}
	st, err := s.d.Store.TakeConnectState(ctx, store.HashSecret(state), s.d.Now())
	if errors.Is(err, store.ErrNotFound) {
		s.fail(w, http.StatusBadRequest, "Connect link expired", "That connect link expired or was already used. Start again from the connect page.")
		return
	}
	if err != nil {
		s.internal(w, "take connect state", err)
		return
	}
	if st.Owner != owner {
		s.fail(w, http.StatusForbidden, "Wrong session", "This connect attempt was started by a different session.")
		return
	}
	if e := q.Get("error"); e != "" {
		s.fail(w, http.StatusBadRequest, "Google did not connect the account", "Google answered: "+e+". Nothing was saved.")
		return
	}
	code := q.Get("code")
	if code == "" {
		s.fail(w, http.StatusBadRequest, "Connect link invalid", "Google returned no authorization code.")
		return
	}
	ad, err := s.adapters.Get(ProviderGoogle)
	if err != nil {
		s.internal(w, "connect adapter", err)
		return
	}
	xctx := ctx
	if s.d.HTTPClient != nil {
		xctx = context.WithValue(ctx, oauth2.HTTPClient, s.d.HTTPClient)
	}
	tok, err := ad.OAuthConfig(nil).Exchange(xctx, code, oauth2.VerifierOption(st.Verifier))
	if err != nil {
		s.d.Log.Warn("connect: token exchange failed", "err", err.Error())
		s.fail(w, http.StatusBadGateway, "Google refused the code", "The authorization code could not be exchanged. Start again.")
		return
	}
	rawScope, _ := tok.Extra("scope").(string)
	if missing := ad.MissingScopes(strings.Fields(rawScope), nil); len(missing) > 0 {
		s.fail(w, http.StatusBadRequest, "Permissions missing",
			"moose needs both Gmail permissions: read mail and manage drafts. Missing: "+strings.Join(missing, ", ")+". Nothing was saved.")
		return
	}
	if tok.RefreshToken == "" {
		s.fail(w, http.StatusBadGateway, "No refresh token", "Google did not return a refresh token. Remove moose from your Google account's third-party access and connect again.")
		return
	}
	prof, err := ad.Profile(xctx, tok)
	if err != nil || prof.Address == "" {
		s.d.Log.Warn("connect: profile lookup failed", "err", fmt.Sprint(err))
		s.fail(w, http.StatusBadGateway, "Could not read the mailbox", "Google accepted the code but the mailbox profile could not be read. Start again.")
		return
	}
	acct, err := s.saveAccount(ctx, owner, st, prof, tok.RefreshToken)
	switch {
	case errors.Is(err, errWrongAccount):
		s.fail(w, http.StatusConflict, "Different Google account", err.Error())
		return
	case err != nil:
		s.internal(w, "save account", err)
		return
	}
	http.Redirect(w, r, PagePath+"?connected="+url.QueryEscape(acct.ID), http.StatusSeeOther)
}

var errWrongAccount = errors.New("wrong account")

// saveAccount creates the account or reattaches the same Google identity to its
// existing one, so a reconnect keeps the id, colour and nickname.
func (s *Server) saveAccount(ctx context.Context, owner string, st store.ConnectState, p Profile, refresh string) (store.Account, error) {
	uid := strings.ToLower(strings.TrimSpace(p.Address))
	accts, err := s.d.Store.ListAccounts(ctx, owner)
	if err != nil {
		return store.Account{}, err
	}
	var existing *store.Account
	for i := range accts {
		if accts[i].ProviderUID == uid {
			existing = &accts[i]
		}
	}
	if existing == nil {
		if a, err := s.d.Store.FindAccountByAddress(ctx, owner, p.Address); err == nil {
			existing = &a
		}
	}
	if st.ReconnectID != "" {
		target, ok := findByID(accts, st.ReconnectID)
		if !ok {
			return store.Account{}, fmt.Errorf("account %s is no longer connected", st.ReconnectID)
		}
		if existing == nil || existing.ID != target.ID {
			return store.Account{}, fmt.Errorf("%w: you signed in to Google as %s, but %s (%s) belongs to a different mailbox. Nothing was changed.",
				errWrongAccount, p.Address, target.Nickname, target.Address)
		}
	}
	now := s.d.Now().UTC()
	scopes := []string{ScopeReadonly, ScopeCompose}
	if existing != nil {
		sealed, err := s.d.Seal.Seal(ctx, []byte(refresh), envelope.Context{Owner: owner, AccountID: existing.ID})
		if err != nil {
			return store.Account{}, err
		}
		a := *existing
		a.RefreshToken, a.Status, a.LastError = sealed, store.StatusOK, ""
		a.Address, a.ProviderUID, a.Caps, a.Scopes = p.Address, uid, p.Caps, scopes
		if p.DisplayName != "" {
			a.DisplayName = p.DisplayName
		}
		return a, s.d.Store.UpdateAccount(ctx, a)
	}
	a := store.Account{
		Owner: owner, ID: store.NewAccountID(), Provider: providerGmail, ProviderUID: uid, Address: p.Address,
		DisplayName: p.DisplayName, ColorIndex: len(accts), Status: store.StatusOK,
		SendEnabled: false, HourlyCap: store.DefaultHourlyCap, Caps: p.Caps, Scopes: scopes,
		IsDefault: len(accts) == 0, Created: now, Updated: now,
	}
	if a.RefreshToken, err = s.d.Seal.Seal(ctx, []byte(refresh), envelope.Context{Owner: owner, AccountID: a.ID}); err != nil {
		return store.Account{}, err
	}
	base := st.Nickname
	if base == "" {
		base = defaultNickname(p.Address)
	}
	for i := 1; i <= 50; i++ {
		a.Nickname = suffixed(base, i)
		err = s.d.Store.CreateAccount(ctx, a)
		if !errors.Is(err, store.ErrNicknameTaken) || st.Nickname != "" {
			return a, err
		}
	}
	return store.Account{}, store.ErrNicknameTaken
}

// defaultNickname derives a nickname from the local part of an address.
func defaultNickname(address string) string {
	local, _, _ := strings.Cut(address, "@")
	var b strings.Builder
	for _, r := range strings.ToLower(local) {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == '.' {
			b.WriteRune(r)
		}
	}
	out := b.String()
	if len(out) > 24 {
		out = out[:24]
	}
	if out == "" {
		return "account"
	}
	return out
}

// suffixed returns base for i == 1 and base-i otherwise, within 24 characters.
func suffixed(base string, i int) string {
	if i == 1 {
		return base
	}
	suf := fmt.Sprintf("-%d", i)
	if len(base)+len(suf) > 24 {
		base = base[:24-len(suf)]
	}
	return base + suf
}

func findByID(accts []store.Account, id string) (store.Account, bool) {
	for _, a := range accts {
		if a.ID == id {
			return a, true
		}
	}
	return store.Account{}, false
}

// ---- account actions ----

func (s *Server) account(w http.ResponseWriter, r *http.Request, owner string) (store.Account, bool) {
	a, err := s.d.Store.GetAccount(r.Context(), owner, r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		s.fail(w, http.StatusNotFound, "Account not found", "That account is not connected here.")
		return store.Account{}, false
	}
	if err != nil {
		s.internal(w, "get account", err)
		return store.Account{}, false
	}
	return a, true
}

func (s *Server) disableSend(w http.ResponseWriter, r *http.Request) {
	owner, ok := s.ownerPost(w, r)
	if !ok {
		return
	}
	a, ok := s.account(w, r, owner)
	if !ok {
		return
	}
	if a.SendEnabled {
		a.SendEnabled = false
		if err := s.d.Store.UpdateAccount(r.Context(), a); err != nil {
			s.internal(w, "disable send", err)
			return
		}
	}
	http.Redirect(w, r, PagePath+"?sendoff="+url.QueryEscape(a.ID), http.StatusSeeOther)
}

func (s *Server) disconnect(w http.ResponseWriter, r *http.Request) {
	owner, ok := s.ownerPost(w, r)
	if !ok {
		return
	}
	a, ok := s.account(w, r, owner)
	if !ok {
		return
	}
	if !strings.EqualFold(strings.TrimSpace(r.PostFormValue("confirm")), a.Address) {
		s.fail(w, http.StatusBadRequest, "Address did not match",
			fmt.Sprintf("To disconnect %s, type its address exactly. Nothing was changed.", a.Address))
		return
	}
	if err := s.d.Store.DisconnectAccount(r.Context(), owner, a.ID, s.d.Now()); err != nil {
		s.internal(w, "disconnect", err)
		return
	}
	http.Redirect(w, r, PagePath+"?disconnected="+url.QueryEscape(a.ID), http.StatusSeeOther)
}

func (s *Server) enableSend(w http.ResponseWriter, r *http.Request) {
	owner, ok := s.ownerPost(w, r)
	if !ok {
		return
	}
	a, ok := s.account(w, r, owner)
	if !ok {
		return
	}
	switch {
	case a.SendEnabled:
		http.Redirect(w, r, PagePath, http.StatusSeeOther)
		return
	case a.Status != store.StatusOK:
		s.fail(w, http.StatusConflict, "Account not ready", fmt.Sprintf("%s is %s. Reconnect it before enabling send.", a.Address, strings.ReplaceAll(a.Status, "_", " ")))
		return
	case s.d.Approver == nil:
		s.fail(w, http.StatusServiceUnavailable, "Approver not configured", "Enabling send needs a passkey approval and this deployment has no approver configured.")
		return
	}
	resp, err := RequestSendEnable(r.Context(), s.d.Store, s.d.Approver, a, s.d.Now())
	if err != nil {
		s.d.Log.Warn("connect: enable-send request failed", "account_id", a.ID, "err", err.Error())
		s.fail(w, http.StatusBadGateway, "Approval not created", "The approver could not create the request. Try again in a moment.")
		return
	}
	dest, err := url.Parse(resp.ApprovalURL)
	if err != nil || dest.Host != s.baseURL.Host || (dest.Scheme != "https" && dest.Scheme != s.baseURL.Scheme) {
		s.d.Log.Warn("connect: approver returned a foreign approval url", "account_id", a.ID)
		s.fail(w, http.StatusBadGateway, "Approval not created", "The approver returned an unexpected address.")
		return
	}
	http.Redirect(w, r, resp.ApprovalURL, http.StatusSeeOther)
}
