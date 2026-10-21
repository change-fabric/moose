package auth

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/change-fabric/moose/internal/audit"
	"github.com/change-fabric/moose/internal/store"
)

// Roles.
const (
	RoleAdmin  = "admin"
	RoleMember = "member"
)

// Unauthenticated routes.
const (
	HealthzPath   = "/healthz"
	BootstrapPath = "/v1/bootstrap/token"
)

// touchInterval throttles TouchToken writes per token.
const touchInterval = time.Minute

// Principal is the authenticated caller placed in the request context.
type Principal struct {
	Login    string   `json:"login"` // token owner: a tailnet login or tag:xyz
	Role     string   `json:"role"`
	TokenID  string   `json:"token_id"`
	Identity Identity `json:"identity"`
}

// IsAdmin reports whether the principal may use admin-only routes.
func (p Principal) IsAdmin() bool { return p.Role == RoleAdmin }

type principalKey struct{}
type identityKey struct{}

// PrincipalFrom returns the authenticated principal, if any.
func PrincipalFrom(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(principalKey{}).(Principal)
	return p, ok
}

// WithPrincipal attaches p to ctx (used by tests and internal callers).
func WithPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, principalKey{}, p)
}

// IdentityFrom returns the tailnet identity resolved for the request, if any.
func IdentityFrom(ctx context.Context) (Identity, bool) {
	id, ok := ctx.Value(identityKey{}).(Identity)
	return id, ok
}

// Authenticator implements the request-authentication algorithm and the
// bootstrap token route.
type Authenticator struct {
	Store         store.Store
	Identity      IdentitySource
	AllowedLogins []string // empty = any tailnet user
	AllowedTags   []string // empty = any tag
	Now           func() time.Time
	Logger        *slog.Logger

	mu      sync.Mutex
	touched map[string]time.Time
	wg      sync.WaitGroup
}

func (a *Authenticator) now() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now()
}

func (a *Authenticator) logger() *slog.Logger {
	if a.Logger != nil {
		return a.Logger
	}
	return slog.Default()
}

// Wait blocks until pending asynchronous TouchToken writes finish.
func (a *Authenticator) Wait() { a.wg.Wait() }

// WriteError writes the moose JSON error shape. A 401 always carries the
// Bearer challenge so MCP OAuth can be layered on later.
func WriteError(w http.ResponseWriter, status int, code, msg string) {
	if status == http.StatusUnauthorized {
		w.Header().Set("WWW-Authenticate", `Bearer realm="moose"`)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": code, "message": msg})
}

// fail writes an error and notes the code on the audit entry.
func fail(w http.ResponseWriter, r *http.Request, status int, code, msg string) {
	audit.FromContext(r.Context()).SetDetail(code)
	WriteError(w, status, code, msg)
}

// identify runs steps 1 and 2: resolve identity, then apply the allowlists.
func (a *Authenticator) identify(w http.ResponseWriter, r *http.Request) (Identity, bool) {
	if a.Identity == nil {
		fail(w, r, http.StatusUnauthorized, "unauthenticated", "no identity source configured")
		return Identity{}, false
	}
	id, err := a.Identity.Identify(r)
	if err != nil {
		fail(w, r, http.StatusUnauthorized, "unauthenticated", "tailnet identity could not be established")
		return Identity{}, false
	}
	if !a.allowed(id) {
		fail(w, r, http.StatusForbidden, "identity_not_allowed", "tailnet identity is not allowed to use moose")
		return Identity{}, false
	}
	return id, true
}

func (a *Authenticator) allowed(id Identity) bool {
	if id.Login != "" {
		return len(a.AllowedLogins) == 0 || slices.Contains(a.AllowedLogins, id.Login)
	}
	return len(id.Tags) > 0 && a.tagAllowed(id.Tags...)
}

func (a *Authenticator) tagAllowed(tags ...string) bool {
	if len(a.AllowedTags) == 0 {
		return true
	}
	for _, t := range tags {
		if slices.Contains(a.AllowedTags, t) {
			return true
		}
	}
	return false
}

// Middleware authenticates every request except GET /healthz and the
// bootstrap route (which only needs a tailnet identity; see Bootstrap).
func (a *Authenticator) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == HealthzPath && (r.Method == http.MethodGet || r.Method == http.MethodHead) {
			next.ServeHTTP(w, r)
			return
		}
		// Step 1-2: identity and allowlists.
		id, ok := a.identify(w, r)
		if !ok {
			return
		}
		ctx := context.WithValue(r.Context(), identityKey{}, id)
		if r.URL.Path == BootstrapPath && r.Method == http.MethodPost {
			audit.FromContext(ctx).SetActor(id.Login, "")
			next.ServeHTTP(w, r.WithContext(ctx))
			return
		}
		p, ok := a.authenticate(w, r.WithContext(ctx), id)
		if !ok {
			return
		}
		audit.FromContext(ctx).SetActor(p.Login, p.TokenID)
		a.touch(p.TokenID)
		next.ServeHTTP(w, r.WithContext(WithPrincipal(ctx, p)))
	})
}

// authenticate runs steps 3-6 and returns the principal for step 7.
func (a *Authenticator) authenticate(w http.ResponseWriter, r *http.Request, id Identity) (Principal, bool) {
	// Step 3: bearer PAT present and well formed.
	raw, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	raw = strings.TrimSpace(raw)
	if !ok || raw == "" {
		fail(w, r, http.StatusUnauthorized, "unauthenticated", "missing bearer token")
		return Principal{}, false
	}
	tokenID, err := ParsePAT(raw)
	if err != nil {
		fail(w, r, http.StatusUnauthorized, "unauthenticated", "malformed bearer token")
		return Principal{}, false
	}
	// Step 4: look up and verify. One message for every failure.
	tok, err := a.Store.GetToken(r.Context(), tokenID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		fail(w, r, http.StatusUnauthorized, "invalid_token", "token is invalid, revoked, or expired")
		return Principal{}, false
	case err != nil:
		a.logger().Error("auth: token lookup", "token", RedactPAT(raw), "err", err)
		fail(w, r, http.StatusServiceUnavailable, "store_unavailable", "state store unavailable")
		return Principal{}, false
	}
	if !VerifyPAT(raw, tok.Hash) || tok.Revoked || (tok.Expires != nil && !a.now().Before(*tok.Expires)) {
		fail(w, r, http.StatusUnauthorized, "invalid_token", "token is invalid, revoked, or expired")
		return Principal{}, false
	}
	// Step 5: owner cross-check against the tailnet identity.
	tagOwned := strings.HasPrefix(tok.Owner, "tag:")
	if tagOwned {
		if id.Login != "" || !slices.Contains(id.Tags, tok.Owner) {
			fail(w, r, http.StatusForbidden, "token_owner_mismatch", "token owner does not match the calling device")
			return Principal{}, false
		}
	} else if tok.Owner != id.Login {
		fail(w, r, http.StatusForbidden, "token_owner_mismatch", "token owner does not match the calling device")
		return Principal{}, false
	}
	// Step 6: user exists and is enabled; tags must be allowed instead.
	p := Principal{Login: tok.Owner, TokenID: tok.ID, Identity: id}
	if tagOwned {
		if !a.tagAllowed(tok.Owner) {
			fail(w, r, http.StatusForbidden, "identity_not_allowed", "tag is not allowed to use moose")
			return Principal{}, false
		}
		p.Role = RoleMember
		return p, true
	}
	u, err := a.Store.GetUser(r.Context(), tok.Owner)
	switch {
	case errors.Is(err, store.ErrNotFound):
		fail(w, r, http.StatusForbidden, "user_unknown", "token owner is not a moose user")
		return Principal{}, false
	case err != nil:
		a.logger().Error("auth: user lookup", "login", tok.Owner, "err", err)
		fail(w, r, http.StatusServiceUnavailable, "store_unavailable", "state store unavailable")
		return Principal{}, false
	case u.Disabled:
		fail(w, r, http.StatusForbidden, "user_disabled", "user is disabled")
		return Principal{}, false
	}
	p.Role = u.Role
	return p, true
}

// touch records LastUsed asynchronously, at most once per touchInterval per token.
func (a *Authenticator) touch(tokenID string) {
	now := a.now()
	a.mu.Lock()
	if a.touched == nil {
		a.touched = map[string]time.Time{}
	}
	if last, ok := a.touched[tokenID]; ok && now.Sub(last) < touchInterval {
		a.mu.Unlock()
		return
	}
	a.touched[tokenID] = now
	a.mu.Unlock()
	a.wg.Add(1)
	go func() {
		defer a.wg.Done()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := a.Store.TouchToken(ctx, tokenID, now); err != nil {
			a.logger().Warn("auth: touch token", "token_id", tokenID, "err", err)
		}
	}()
}

// RequireAdmin wraps an admin-only handler.
func RequireAdmin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, ok := PrincipalFrom(r.Context())
		if !ok {
			fail(w, r, http.StatusUnauthorized, "unauthenticated", "authentication required")
			return
		}
		if !p.IsAdmin() {
			fail(w, r, http.StatusForbidden, "forbidden", "admin role required")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// EnsureBootstrapAdmin creates login as admin when the users table is empty.
// It does nothing otherwise, and nothing when login is empty.
func EnsureBootstrapAdmin(ctx context.Context, s store.Store, login string, now time.Time) error {
	if login == "" {
		return nil
	}
	users, err := s.ListUsers(ctx)
	if err != nil {
		return err
	}
	if len(users) > 0 {
		return nil
	}
	return s.PutUser(ctx, store.User{Login: login, Role: RoleAdmin, Created: now.UTC()})
}

// BootstrapResponse is the body returned by POST /v1/bootstrap/token.
type BootstrapResponse struct {
	Token   string     `json:"token"`
	ID      string     `json:"id"`
	Owner   string     `json:"owner"`
	Name    string     `json:"name"`
	Expires *time.Time `json:"expires,omitempty"`
}

// Bootstrap serves POST /v1/bootstrap/token behind Middleware. It issues a
// first PAT to an existing, enabled user who owns zero tokens; the bootstrap
// admin is such a user after EnsureBootstrapAdmin, and members self-serve the
// same way (plan Q7). Once the caller owns a token the route answers 410.
func (a *Authenticator) Bootstrap() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		audit.FromContext(r.Context()).SetAction("token.bootstrap", "", "")
		if r.Method != http.MethodPost {
			fail(w, r, http.StatusMethodNotAllowed, "method_not_allowed", "use POST")
			return
		}
		id, ok := IdentityFrom(r.Context())
		if !ok {
			fail(w, r, http.StatusUnauthorized, "unauthenticated", "tailnet identity required")
			return
		}
		if id.Login == "" {
			fail(w, r, http.StatusForbidden, "bootstrap_forbidden", "tagged devices cannot bootstrap; ask an admin for a token")
			return
		}
		var body struct {
			Name string `json:"name"`
		}
		if r.Body != nil {
			if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body); err != nil && !errors.Is(err, io.EOF) {
				fail(w, r, http.StatusBadRequest, "invalid_request", "body must be JSON {\"name\":...}")
				return
			}
		}
		name := strings.TrimSpace(body.Name)
		if name == "" {
			name = "bootstrap"
		}
		ctx := r.Context()
		u, err := a.Store.GetUser(ctx, id.Login)
		switch {
		case errors.Is(err, store.ErrNotFound):
			fail(w, r, http.StatusForbidden, "bootstrap_forbidden", "caller is not a moose user")
			return
		case err != nil:
			fail(w, r, http.StatusServiceUnavailable, "store_unavailable", "state store unavailable")
			return
		case u.Disabled:
			fail(w, r, http.StatusForbidden, "user_disabled", "user is disabled")
			return
		}
		owned, err := a.Store.ListTokens(ctx, id.Login)
		if err != nil {
			fail(w, r, http.StatusServiceUnavailable, "store_unavailable", "state store unavailable")
			return
		}
		if len(owned) > 0 {
			fail(w, r, http.StatusGone, "bootstrap_used", "caller already owns a token; use POST /v1/tokens")
			return
		}
		plain, tok, err := NewToken(id.Login, name, DefaultTokenTTL, a.now())
		if err != nil {
			fail(w, r, http.StatusInternalServerError, "internal", "token generation failed")
			return
		}
		if err := a.Store.CreateToken(ctx, tok); err != nil {
			fail(w, r, http.StatusServiceUnavailable, "store_unavailable", "state store unavailable")
			return
		}
		audit.FromContext(ctx).SetActor(id.Login, tok.ID)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(BootstrapResponse{Token: plain, ID: tok.ID, Owner: tok.Owner, Name: tok.Name, Expires: tok.Expires})
	})
}
