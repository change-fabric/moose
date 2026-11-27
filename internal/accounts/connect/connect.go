// Package connect serves the connect page, /connect/google: the one hosted page
// moose-email has besides login. It runs one OAuth web flow per provider
// adapter (Google: offline access, gmail.readonly and gmail.compose; Microsoft:
// the /common authority and the mail scopes), always with state and PKCE, at
// /connect/{provider}/start and /connect/{provider}/callback. It stores each
// connected mailbox as an account that is read-only until send is enabled, and
// lists every account with a provider badge and the controls R9 asks for.
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
	Prefix = "/connect/"
	// PagePath is the accounts page. It keeps its Google-era address so
	// bookmarks work; it lists every provider's accounts.
	PagePath = "/connect/google"
	// StartPath and CallbackPath are the Google routes. They are the
	// {provider}=google case of StartPathFor and CallbackPathFor, so the
	// redirect URI already registered with Google is unchanged and needs no
	// redirect.
	StartPath    = "/connect/google/start"
	CallbackPath = "/connect/google/callback"
)

// StartPathFor is the route that starts a connect for a provider.
func StartPathFor(provider string) string { return Prefix + provider + "/start" }

// CallbackPathFor is the OAuth redirect route for a provider.
func CallbackPathFor(provider string) string { return Prefix + provider + "/callback" }

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
	LoginPath = "/connect/login"
)

// Profile is what moose learns about a freshly authorized mailbox.
type Profile struct {
	Address     string
	DisplayName string
	// ProviderUID is the provider's stable identity for the mailbox (Microsoft:
	// tid.oid). Empty means the lowercased address is the identity (Google).
	ProviderUID string
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
	// OAuth is the Google web client; build it with GoogleConfig. Together with
	// Profile it registers the Google adapter; leave both nil to offer other
	// providers only.
	OAuth   *oauth2.Config
	Profile ProfileFunc
	// Adapters are further providers (see NewMicrosoftAdapter). Only providers
	// named here, or Google through OAuth, can be connected or reconnected.
	Adapters []Adapter
	Auth     Authenticator
	Approver Approver // nil leaves Enable send unavailable
	// CalendarEnabled offers Grant calendar on each account (email.calendar.enabled).
	CalendarEnabled bool
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
	// order is the registered provider names in display order: Google first.
	order   []string
	origin  string
	baseURL *url.URL
}

// New validates deps and returns the server.
func New(d Deps) (*Server, error) {
	switch {
	case d.Store == nil || d.Seal == nil || d.Auth == nil:
		return nil, errors.New("connect: Store, Seal, OAuth, Profile and Auth are required")
	case (d.OAuth == nil) != (d.Profile == nil):
		return nil, errors.New("connect: Store, Seal, OAuth, Profile and Auth are required")
	case d.OAuth == nil && len(d.Adapters) == 0:
		return nil, errors.New("connect: at least one provider is required: OAuth and Profile, or Adapters")
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
	adapters := NewRegistry()
	if d.OAuth != nil {
		adapters.Register(NewGoogleAdapter(d.OAuth, d.Profile))
	}
	for _, a := range d.Adapters {
		adapters.Register(a)
	}
	return &Server{d: d, adapters: adapters, order: adapters.Names(), baseURL: u, origin: u.Scheme + "://" + u.Host}, nil
}

// Handler serves every connect route. Mount it at Prefix.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+PagePath, s.page)
	mux.HandleFunc("POST "+Prefix+"{provider}/start", s.start)
	mux.HandleFunc("GET "+Prefix+"{provider}/callback", s.callback)
	mux.HandleFunc("POST "+Prefix+"accounts/{id}/enable-send", s.enableSend)
	mux.HandleFunc("POST "+Prefix+"accounts/{id}/disable-send", s.disableSend)
	mux.HandleFunc("POST "+Prefix+"accounts/{id}/grant-calendar", s.grantCalendar)
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

// adapterFor resolves the {provider} segment of a connect route. A provider
// that is not registered (unknown, or switched off in config) is not found.
func (s *Server) adapterFor(w http.ResponseWriter, r *http.Request) (Adapter, bool) {
	ad, err := s.adapters.Get(r.PathValue("provider"))
	if err != nil {
		s.fail(w, http.StatusNotFound, "Provider not available", "That account provider is not available on this moose.")
		return nil, false
	}
	return ad, true
}

func (s *Server) start(w http.ResponseWriter, r *http.Request) {
	owner, ok := s.ownerPost(w, r)
	if !ok {
		return
	}
	ad, ok := s.adapterFor(w, r)
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
		a, ok := findByID(accts, id)
		if !ok || a.Owner != owner {
			s.fail(w, http.StatusNotFound, "Account not found", "That account is not connected here.")
			return
		}
		if accountProvider(a) != ad.Name() {
			s.fail(w, http.StatusConflict, "Wrong provider", fmt.Sprintf("%s is a %s account. Reconnect it with %s.", a.Address, providerLabel(s.adapters, accountProvider(a)), providerLabel(s.adapters, accountProvider(a))))
			return
		}
		st.ReconnectID = id
	}
	st.Provider = ad.Name()
	state := randomToken(32)
	verifier := oauth2.GenerateVerifier()
	st.Hash, st.Verifier = store.HashSecret(state), verifier
	st.Expires = s.d.Now().Add(store.ConnectStateTTL)
	if err := s.d.Store.PutConnectState(ctx, st); err != nil {
		s.internal(w, "store connect state", err)
		return
	}
	hint := ""
	if a, ok := findByID(accts, st.ReconnectID); ok {
		hint = a.Address
	}
	opts := append([]oauth2.AuthCodeOption{oauth2.S256ChallengeOption(verifier)}, ad.AuthOptions(hint)...)
	http.Redirect(w, r, ad.OAuthConfig(nil).AuthCodeURL(state, opts...), http.StatusSeeOther)
}

func (s *Server) callback(w http.ResponseWriter, r *http.Request) {
	ad, ok := s.adapterFor(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	if ad.Name() == ProviderMicrosoft && q.Get("admin_consent") != "" && q.Get("state") == "" {
		// The tenant administrator's browser, not the owner's: a static page with
		// no session and nothing stored.
		s.adminConsentAnswer(w, q)
		return
	}
	owner, ok := s.owner(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
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
	if stProvider(st) != ad.Name() {
		s.fail(w, http.StatusBadRequest, "Connect link invalid", "This connect attempt was started for a different provider. Start again from the connect page.")
		return
	}
	label := ad.Label()
	if e := q.Get("error"); e != "" {
		ec := ad.CallbackError(e, q.Get("error_description"))
		if st.GrantAccountID != "" && e == "access_denied" {
			ec = ErrorCopy{Title: label + " calendar access was declined",
				Message: "The permissions were declined, so nothing changed. Mail still works exactly as before. Use Grant calendar again to retry."}
		}
		if ec.Href != "" {
			s.failWith(w, http.StatusBadRequest, ec.Title, ec.Message, ec.Href, ec.Label)
			return
		}
		s.fail(w, http.StatusBadRequest, ec.Title, ec.Message)
		return
	}
	code := q.Get("code")
	if code == "" {
		s.fail(w, http.StatusBadRequest, "Connect link invalid", label+" returned no authorization code.")
		return
	}
	if st.GrantAccountID != "" {
		s.grantCallback(w, r, ad, st, owner, code)
		return
	}
	xctx := ctx
	if s.d.HTTPClient != nil {
		xctx = context.WithValue(ctx, oauth2.HTTPClient, s.d.HTTPClient)
	}
	tok, err := ad.OAuthConfig(nil).Exchange(xctx, code, oauth2.VerifierOption(st.Verifier))
	if err != nil {
		s.d.Log.Warn("connect: token exchange failed", "provider", ad.Name(), "err", err.Error())
		s.fail(w, http.StatusBadGateway, label+" refused the code", "The authorization code could not be exchanged. Start again.")
		return
	}
	rawScope, _ := tok.Extra("scope").(string)
	if missing := ad.MissingScopes(strings.Fields(rawScope), nil); len(missing) > 0 {
		s.fail(w, http.StatusBadRequest, "Permissions missing",
			"moose needs every permission it asked for on the "+label+" screen. Missing: "+strings.Join(missing, ", ")+". Nothing was saved.")
		return
	}
	if tok.RefreshToken == "" {
		s.fail(w, http.StatusBadGateway, "No refresh token", label+" did not return a refresh token. Remove moose from your "+label+" account's third-party access and connect again.")
		return
	}
	prof, err := ad.Profile(xctx, tok)
	if err != nil || prof.Address == "" {
		s.d.Log.Warn("connect: profile lookup failed", "provider", ad.Name(), "err", fmt.Sprint(err))
		s.fail(w, http.StatusBadGateway, "Could not read the mailbox", label+" accepted the code but the mailbox profile could not be read. Start again.")
		return
	}
	all, mail := ad.Grant(strings.Fields(rawScope))
	acct, err := s.saveAccount(ctx, owner, ad, st, prof, tok.RefreshToken, all, mail)
	switch {
	case errors.Is(err, errWrongAccount):
		s.fail(w, http.StatusConflict, "Different "+label+" account", err.Error())
		return
	case err != nil:
		s.internal(w, "save account", err)
		return
	}
	http.Redirect(w, r, PagePath+"?connected="+url.QueryEscape(acct.ID), http.StatusSeeOther)
}

var errWrongAccount = errors.New("wrong account")

// stProvider is the provider a connect state was started for; a state from
// before the field existed is Google's.
func stProvider(st store.ConnectState) string {
	if st.Provider == "" {
		return ProviderGoogle
	}
	return st.Provider
}

// accountProvider is an account's provider; the zero value is Google.
func accountProvider(a store.Account) string {
	if a.Provider == "" {
		return ProviderGoogle
	}
	return a.Provider
}

// providerLabel is a provider's display name, or its raw name when no adapter
// is registered for it (a provider switched off in config).
func providerLabel(r *Registry, name string) string {
	if ad, err := r.Get(name); err == nil {
		return ad.Label()
	}
	if name == ProviderMicrosoft {
		return "Microsoft"
	}
	return strings.ToUpper(name[:1]) + name[1:]
}

// adminConsentAnswer is what the administrator sees after approving (or not)
// on the Microsoft consent screen.
func (s *Server) adminConsentAnswer(w http.ResponseWriter, q url.Values) {
	if strings.EqualFold(q.Get("admin_consent"), "true") {
		s.failWith(w, http.StatusOK, "moose is approved", "Your organization's administrator approved moose. The person connecting a mailbox can now connect it again from the moose accounts page.", PagePath, "Go to moose accounts")
		return
	}
	s.failWith(w, http.StatusOK, "moose was not approved", "The administrator consent was not granted, so people in this organization still cannot connect. Nothing was changed.", PagePath, "Go to moose accounts")
}

// writeCredential stores a freshly sealed credential, re-reading the version
// and trying once more if another writer (a token refresh) got there first.
func (s *Server) writeCredential(ctx context.Context, owner, id string, sealed []byte) error {
	for attempt := 0; ; attempt++ {
		cur, err := s.d.Store.GetAccount(ctx, owner, id)
		if err != nil {
			return err
		}
		err = s.d.Store.SetAccountCredential(ctx, owner, id, sealed, cur.Version)
		if errors.Is(err, store.ErrConflict) && attempt == 0 {
			continue
		}
		return err
	}
}

// mergeMailGrant records the mail grant while keeping any other service.
func mergeMailGrant(have map[string]store.ServiceGrant, scopes []string, now time.Time) map[string]store.ServiceGrant {
	out := make(map[string]store.ServiceGrant, len(have)+1)
	for k, v := range have {
		out[k] = v
	}
	out[store.ServiceMail] = store.ServiceGrant{Scopes: scopes, GrantedAt: now}
	return out
}

// saveAccount creates the account or reattaches the same Google identity to its
// existing one, so a reconnect keeps the id, colour and nickname.
func (s *Server) saveAccount(ctx context.Context, owner string, ad Adapter, st store.ConnectState, p Profile, refresh string, allScopes, mailScopes []string) (store.Account, error) {
	name := ad.Name()
	uid := p.ProviderUID
	if uid == "" {
		uid = strings.ToLower(strings.TrimSpace(p.Address))
	}
	accts, err := s.d.Store.ListAccounts(ctx, owner)
	if err != nil {
		return store.Account{}, err
	}
	var existing *store.Account
	for i := range accts {
		if accts[i].ProviderUID == uid && accountProvider(accts[i]) == name {
			existing = &accts[i]
		}
	}
	if existing == nil && p.ProviderUID == "" {
		// Only an identity that is the address itself can be matched by address.
		if a, err := s.d.Store.FindAccountByAddress(ctx, owner, p.Address); err == nil && accountProvider(a) == name {
			existing = &a
		}
	}
	if st.ReconnectID != "" {
		target, ok := findByID(accts, st.ReconnectID)
		if !ok {
			return store.Account{}, fmt.Errorf("account %s is no longer connected", st.ReconnectID)
		}
		if existing == nil || existing.ID != target.ID {
			return store.Account{}, fmt.Errorf("%w: you signed in to %s as %s, but %s (%s) belongs to a different mailbox. Nothing was changed.",
				errWrongAccount, ad.Label(), p.Address, target.Nickname, target.Address)
		}
	}
	now := s.d.Now().UTC()
	if existing != nil {
		sealed, err := s.d.Seal.Seal(ctx, []byte(refresh), envelope.Context{Owner: owner, AccountID: existing.ID})
		if err != nil {
			return store.Account{}, err
		}
		// The credential goes first, then the profile and status, so a crash
		// between the two leaves the account needing reauth rather than "ok"
		// with a stale credential.
		if err := s.writeCredential(ctx, owner, existing.ID, sealed); err != nil {
			return store.Account{}, err
		}
		patch := store.AccountPatch{
			Status: store.Ptr(store.StatusOK), LastError: store.Ptr(""),
			Address: store.Ptr(p.Address), ProviderUID: store.Ptr(uid), Caps: store.Ptr(p.Caps), Scopes: store.Ptr(allScopes),
			Services: store.Ptr(mergeMailGrant(existing.Services, mailScopes, now)),
		}
		if p.DisplayName != "" {
			patch.DisplayName = store.Ptr(p.DisplayName)
		}
		if err := s.d.Store.UpdateAccount(ctx, owner, existing.ID, patch); err != nil {
			return store.Account{}, err
		}
		return s.d.Store.GetAccount(ctx, owner, existing.ID)
	}
	a := store.Account{
		Owner: owner, ID: store.NewAccountID(), Provider: name, ProviderUID: uid, Address: p.Address,
		DisplayName: p.DisplayName, ColorIndex: len(accts), Status: store.StatusOK,
		SendEnabled: false, HourlyCap: store.DefaultHourlyCap, Caps: p.Caps, Scopes: allScopes,
		Services:  mergeMailGrant(nil, mailScopes, now),
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
		if err := s.d.Store.UpdateAccount(r.Context(), owner, a.ID, store.AccountPatch{SendEnabled: store.Ptr(false)}); err != nil {
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
