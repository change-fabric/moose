// Package accountprov turns a stored account into a live provider.Provider.
//
// It owns the per-account access-token cache (R10): the sealed refresh token is
// opened and exchanged at most once per token lifetime however many calls
// arrive together, a revoked-credential answer (invalid_grant, and on Microsoft
// interaction_required, AADSTS70008, AADSTS700082) moves the account to
// needs_reauth, and nothing here ever logs or returns a token.
//
// A provider that rotates refresh tokens (Microsoft always does) has the new
// token written back through Store.SetAccountCredential, conditional on the
// credential version read with the old one. Microsoft refreshes also request
// the explicit union of the scopes the account has been granted across
// services, so a token minted after incremental calendar consent still covers
// mail and the reverse.
package accountprov

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.org/x/oauth2"
	"google.golang.org/api/option"

	"github.com/change-fabric/moose/internal/accounts/envelope"
	"github.com/change-fabric/moose/internal/accounts/store"
	"github.com/change-fabric/moose/internal/email/gmail"
	"github.com/change-fabric/moose/internal/email/provider"
)

// refreshSkew is how early a cached access token is treated as expired.
const refreshSkew = time.Minute

// microsoftBaseScopes are requested on every Microsoft refresh next to the
// service scopes. offline_access keeps the refresh token rotating.
var microsoftBaseScopes = []string{"offline_access", "openid", "profile", "email"}

// Source builds providers for accounts. The zero value is not usable: set
// Store, Seal and an OAuth config for each provider in use. It is safe for
// concurrent use.
type Source struct {
	Store store.Store
	Seal  envelope.Sealer
	// OAuth is the tenant's Google OAuth client; its token endpoint serves
	// Google refreshes. It is the entry for store.ProviderGoogle when Configs
	// has none.
	OAuth *oauth2.Config
	// Configs holds one OAuth client per provider name (store.ProviderGoogle,
	// store.ProviderMicrosoft).
	Configs map[string]*oauth2.Config
	// HTTPClient carries token endpoint calls. Nil means http.DefaultClient.
	HTTPClient *http.Client
	// GmailOptions are extra options for the Gmail client (tests pass
	// option.WithEndpoint).
	GmailOptions []option.ClientOption
	Now          func() time.Time
	// Log receives write-back failures. Nil discards them. It never receives a
	// token.
	Log *slog.Logger

	mu      sync.Mutex
	entries map[string]*entry
}

type entry struct {
	mu  sync.Mutex
	tok *oauth2.Token
}

// Provider returns the provider for account a. It never contacts the provider's
// token endpoint: the access token is fetched lazily by the first request that
// needs it.
func (s *Source) Provider(ctx context.Context, a store.Account) (provider.Provider, error) {
	switch a.Provider {
	case store.ProviderGoogle:
		opts := append([]option.ClientOption{option.WithTokenSource(tokenFunc(func() (*oauth2.Token, error) {
			return s.Token(ctx, a)
		}))}, s.GmailOptions...)
		return gmail.New(ctx, opts...)
	}
	return nil, fmt.Errorf("accountprov: unsupported provider %q", a.Provider)
}

type tokenFunc func() (*oauth2.Token, error)

func (f tokenFunc) Token() (*oauth2.Token, error) { return f() }

func (s *Source) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Source) logger() *slog.Logger {
	if s.Log != nil {
		return s.Log
	}
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func (s *Source) config(name string) (*oauth2.Config, error) {
	if c := s.Configs[name]; c != nil {
		return c, nil
	}
	if name == store.ProviderGoogle && s.OAuth != nil {
		return s.OAuth, nil
	}
	return nil, fmt.Errorf("accountprov: no OAuth client configured for provider %q", name)
}

func (s *Source) entry(id string) *entry {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.entries == nil {
		s.entries = map[string]*entry{}
	}
	e := s.entries[id]
	if e == nil {
		e = &entry{}
		s.entries[id] = e
	}
	return e
}

// Token returns a valid access token for a. Concurrent callers for one
// account share a single refresh: the first exchanges the refresh token and
// the rest find the fresh token in the cache. A refresh answered with a
// revoked-credential error marks the account needs_reauth and returns
// provider.ErrAuth. A rotated refresh token is written back before Token
// returns.
func (s *Source) Token(ctx context.Context, a store.Account) (*oauth2.Token, error) {
	e := s.entry(a.ID)
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.tok != nil && e.tok.AccessToken != "" && e.tok.Expiry.After(s.now().Add(refreshSkew)) {
		return e.tok, nil
	}
	if len(a.RefreshToken) == 0 {
		return nil, fmt.Errorf("%w: account has no stored credential", provider.ErrAuth)
	}
	a = s.freshest(ctx, a)
	cfg, err := s.config(a.Provider)
	if err != nil {
		return nil, err
	}
	refresh, err := s.Seal.Open(ctx, a.RefreshToken, envelope.Context{Owner: a.Owner, AccountID: a.ID})
	if err != nil {
		return nil, fmt.Errorf("accountprov: open stored credential: %w", err)
	}
	xctx := ctx
	if s.HTTPClient != nil {
		xctx = context.WithValue(ctx, oauth2.HTTPClient, s.HTTPClient)
	}
	var (
		tok     *oauth2.Token
		granted string
	)
	if a.Provider == store.ProviderMicrosoft {
		tok, granted, err = refreshWithScopes(xctx, cfg, string(refresh), requestScopes(a), s.now())
	} else {
		tok, err = cfg.TokenSource(xctx, &oauth2.Token{RefreshToken: string(refresh)}).Token()
	}
	if err != nil {
		var re *oauth2.RetrieveError
		if errors.As(err, &re) && revoked(re) {
			s.markReauth(ctx, a)
			return nil, fmt.Errorf("%w: the provider no longer accepts this account's credential", provider.ErrAuth)
		}
		return nil, fmt.Errorf("accountprov: refresh: %w", err)
	}
	if tok.RefreshToken != "" && tok.RefreshToken != string(refresh) {
		s.writeBack(ctx, a, tok.RefreshToken)
	}
	if a.Provider == store.ProviderMicrosoft && granted != "" {
		s.persistGranted(ctx, a, granted)
	}
	e.tok = tok
	return tok, nil
}

// freshest returns a with the stored account's current grants and, when the
// stored credential is newer than the caller's copy, its credential too. A
// rotation written by an earlier refresh is thereby used instead of the stale
// token in a long-lived account value, and a service granted since the caller
// read the account is included in the scope union. It falls back to a when the
// store cannot say.
func (s *Source) freshest(ctx context.Context, a store.Account) store.Account {
	cur, err := s.Store.GetAccount(ctx, a.Owner, a.ID)
	if err != nil {
		return a
	}
	a.Services, a.Scopes = cur.Services, cur.Scopes
	if cur.Version > a.Version && len(cur.RefreshToken) > 0 {
		a.RefreshToken, a.Version = cur.RefreshToken, cur.Version
	}
	return a
}

// revoked reports whether a token endpoint error means the credential is gone
// and the owner must reconnect: invalid_grant, and on Entra interaction_required
// and the AADSTS70008 (expired) and AADSTS700082 (inactive too long) codes.
func revoked(re *oauth2.RetrieveError) bool {
	switch re.ErrorCode {
	case "invalid_grant", "interaction_required":
		return true
	}
	body := string(re.Body)
	for _, marker := range []string{"invalid_grant", "interaction_required", "AADSTS70008", "AADSTS700082"} {
		if strings.Contains(body, marker) {
			return true
		}
	}
	return false
}

// writeBack stores a rotated refresh token. The write is conditional on the
// credential version read with the old token; on a conflict it reloads and
// tries once more. A failure is logged and never fails the call: the access
// token just minted is good, and the worst case is a needs_reauth on a later
// refresh.
func (s *Source) writeBack(ctx context.Context, a store.Account, refresh string) {
	sealed, err := s.Seal.Seal(ctx, []byte(refresh), envelope.Context{Owner: a.Owner, AccountID: a.ID})
	if err != nil {
		s.logger().Warn("accountprov: seal rotated credential failed", "account", a.ID, "err", err.Error())
		return
	}
	version := a.Version
	for attempt := 0; attempt < 2; attempt++ {
		err = s.Store.SetAccountCredential(ctx, a.Owner, a.ID, sealed, version)
		if !errors.Is(err, store.ErrConflict) {
			break
		}
		cur, gerr := s.Store.GetAccount(ctx, a.Owner, a.ID)
		if gerr != nil || cur.Status == store.StatusDisconnected {
			return // disconnected meanwhile: do not revive it
		}
		version = cur.Version
	}
	if err != nil {
		s.logger().Warn("accountprov: credential write-back failed", "account", a.ID, "err", err.Error())
	}
}

// markReauth records needs_reauth on the stored account. A failure to record
// it is not fatal: the caller still gets ErrAuth, and the next call retries.
func (s *Source) markReauth(ctx context.Context, a store.Account) {
	cur, err := s.Store.GetAccount(ctx, a.Owner, a.ID)
	if err != nil || cur.Status == store.StatusDisconnected {
		return
	}
	_ = s.Store.UpdateAccount(ctx, a.Owner, a.ID, store.AccountPatch{
		Status: store.Ptr(store.StatusNeedsReauth), LastError: store.Ptr("invalid_grant")})
}

// requestScopes is the explicit scope set for a Microsoft refresh: every scope
// the account has been granted in any service, plus the base OIDC scopes. A
// refresh that named only one service's scopes would mint a token the other
// service rejects.
func requestScopes(a store.Account) []string {
	set := map[string]bool{}
	for _, sc := range microsoftBaseScopes {
		set[sc] = true
	}
	for _, g := range a.Services {
		for _, sc := range g.Scopes {
			set[sc] = true
		}
	}
	for _, sc := range a.Scopes {
		set[sc] = true
	}
	out := make([]string, 0, len(set))
	for sc := range set {
		out = append(out, sc)
	}
	sort.Strings(out)
	return out
}

// refreshWithScopes performs a refresh_token grant that names scopes, which
// oauth2.Config.TokenSource cannot. It returns the token and the scope string
// the provider reports as granted.
func refreshWithScopes(ctx context.Context, cfg *oauth2.Config, refresh string, scopes []string, now time.Time) (*oauth2.Token, string, error) {
	form := url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refresh},
		"scope":         {strings.Join(scopes, " ")},
	}
	if cfg.Endpoint.AuthStyle != oauth2.AuthStyleInHeader {
		form.Set("client_id", cfg.ClientID)
		if cfg.ClientSecret != "" {
			form.Set("client_secret", cfg.ClientSecret)
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.Endpoint.TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if cfg.Endpoint.AuthStyle == oauth2.AuthStyleInHeader {
		req.SetBasicAuth(url.QueryEscape(cfg.ClientID), url.QueryEscape(cfg.ClientSecret))
	}
	client := http.DefaultClient
	if c, ok := ctx.Value(oauth2.HTTPClient).(*http.Client); ok && c != nil {
		client = c
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, "", err
	}
	var out struct {
		AccessToken  string      `json:"access_token"`
		TokenType    string      `json:"token_type"`
		RefreshToken string      `json:"refresh_token"`
		ExpiresIn    json.Number `json:"expires_in"`
		Scope        string      `json:"scope"`
		Error        string      `json:"error"`
		Description  string      `json:"error_description"`
	}
	_ = json.Unmarshal(body, &out)
	if resp.StatusCode/100 != 2 || out.AccessToken == "" {
		return nil, "", &oauth2.RetrieveError{Response: resp, Body: body, ErrorCode: out.Error, ErrorDescription: out.Description}
	}
	tok := &oauth2.Token{AccessToken: out.AccessToken, TokenType: out.TokenType, RefreshToken: out.RefreshToken}
	if n, err := out.ExpiresIn.Int64(); err == nil && n > 0 {
		tok.Expiry = now.Add(time.Duration(n) * time.Second)
	}
	return tok, out.Scope, nil
}

// serviceOf maps a Microsoft scope to the service it belongs to, or "" for the
// OIDC and profile scopes that belong to none.
func serviceOf(scope string) string {
	scope = strings.TrimPrefix(scope, "https://graph.microsoft.com/")
	switch {
	case strings.HasPrefix(scope, "Mail."):
		return store.ServiceMail
	case strings.HasPrefix(scope, "Calendars."):
		return store.ServiceCalendar
	}
	return ""
}

// persistGranted records the scopes a Microsoft refresh reported as granted,
// per service, so the stored grant is the actual one rather than what was
// asked for. It writes only when something changed.
func (s *Source) persistGranted(ctx context.Context, a store.Account, granted string) {
	perService := map[string][]string{}
	var all []string
	for _, sc := range strings.Fields(granted) {
		all = append(all, sc)
		if svc := serviceOf(sc); svc != "" {
			perService[svc] = append(perService[svc], sc)
		}
	}
	sort.Strings(all)
	next := map[string]store.ServiceGrant{}
	for svc, g := range a.Services {
		scopes := perService[svc]
		sort.Strings(scopes)
		next[svc] = store.ServiceGrant{Scopes: scopes, GrantedAt: g.GrantedAt}
	}
	for svc, scopes := range perService {
		if _, ok := next[svc]; !ok {
			sort.Strings(scopes)
			next[svc] = store.ServiceGrant{Scopes: scopes, GrantedAt: s.now().UTC()}
		}
	}
	if sameScopes(a.Scopes, all) && sameServices(a.Services, next) {
		return
	}
	if err := s.Store.UpdateAccount(ctx, a.Owner, a.ID, store.AccountPatch{Scopes: &all, Services: &next}); err != nil {
		s.logger().Warn("accountprov: persisting granted scopes failed", "account", a.ID, "err", err.Error())
	}
}

func sameScopes(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	x := append([]string(nil), a...)
	sort.Strings(x)
	y := append([]string(nil), b...)
	sort.Strings(y)
	for i := range x {
		if x[i] != y[i] {
			return false
		}
	}
	return true
}

func sameServices(a, b map[string]store.ServiceGrant) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		w, ok := b[k]
		if !ok || !sameScopes(v.Scopes, w.Scopes) {
			return false
		}
	}
	return true
}
