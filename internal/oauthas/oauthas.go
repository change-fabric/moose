// Package oauthas is moose-email's OAuth 2.1 authorization server, written by
// hand and kept deliberately small: dynamic client registration, authorization
// code with PKCE S256, opaque access tokens and rotating refresh tokens, for
// public clients only.
//
// The login step is not a password form. /authorize creates a dashboard-session
// request with moose-approver and shows an interstitial page with a QR code
// that opens it on the owner's phone. The page polls /authorize/poll, and once
// a passkey approval receipt verifies, redirects the browser back to the client
// with an authorization code.
//
// It protects /mcp with the go-sdk's auth.RequireBearerToken (Protect) and
// serves RFC 9728 protected resource metadata through the go-sdk handler. The
// invariants every rule below must keep, each with a negative test:
//
//   - codes live 60 seconds, are stored hashed, are bound to client_id,
//     redirect_uri, the PKCE challenge and the resource, and are redeemed with
//     a conditional write; a second redeem fails and revokes the tokens issued
//     from the first;
//   - the resource (RFC 8707) must be https://<domain>/mcp, tokens record it as
//     their audience, and the bearer verifier rejects any other audience;
//   - redirect URIs match exactly, except that http://127.0.0.1 and
//     http://localhost match on any port (RFC 8252);
//   - the browser transaction is bound to a secret polling handle held in an
//     HttpOnly, Secure, SameSite=Lax cookie, and the dashboard approval payload
//     carries sha256(handle);
//   - access tokens last an hour; refresh tokens 30 days idle and 90 days
//     absolute per family, rotate on every use, and any reuse outside a 10
//     second grace window revokes the whole family.
package oauthas

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/oauthex"

	"github.com/change-fabric/moose/internal/accounts/envelope"
	"github.com/change-fabric/moose/internal/accounts/store"
	"github.com/change-fabric/moose/internal/approver/api"
)

// Routes served by Handler.
const (
	ASMetadataPath = "/.well-known/oauth-authorization-server"
	// PRMetadataPath is the RFC 9728 document; the path-suffixed form for the
	// /mcp resource is served too.
	PRMetadataPath = "/.well-known/oauth-protected-resource"
	RegisterPath   = "/register"
	AuthorizePath  = "/authorize"
	PollPath       = "/authorize/poll"
	TokenPath      = "/token"
	// MCPPath is the protected resource. Each moose module serves MCP under
	// its own prefix on the shared host, so a later module adds /drive/mcp.
	MCPPath = "/email/mcp"
)

// ScopeMCP is the one scope this server issues.
const ScopeMCP = "mcp"

// Lifetimes.
const (
	CodeTTL   = 60 * time.Second
	AccessTTL = time.Hour
	// TxnTTL matches the approver's request lifetime.
	TxnTTL = 15 * time.Minute
)

const txnCookie = "moose_oauth_txn"

// Approver is the part of the moose-approver service API this package uses.
// *api.Client implements it.
type Approver interface {
	Create(ctx context.Context, in api.CreateBody) (api.CreateResponse, bool, error)
	Status(ctx context.Context, id string) (api.StatusResponse, error)
	Consume(ctx context.Context, id, claimant string) (api.StatusResponse, error)
}

// Deps wires a Server.
type Deps struct {
	Store    store.Store
	Seal     envelope.Sealer
	Approver Approver
	// ReceiptKey is the pinned approver receipt public key (PEM).
	ReceiptKey []byte
	// RPID is the relying party id every receipt must carry.
	RPID string
	// Owner is the single tenant owner every token is issued to.
	Owner string
	// BaseURL is the public origin, https://<domain>. It is the issuer, and
	// BaseURL + /mcp is the one resource tokens are issued for.
	BaseURL string
	// InsecureCookies drops the Secure cookie flag, for http://localhost in dev.
	InsecureCookies bool
	Now             func() time.Time
	Log             *slog.Logger
}

// Server is the authorization server.
type Server struct {
	d        Deps
	issuer   string
	resource string
}

// New validates d and returns the server.
func New(d Deps) (*Server, error) {
	switch {
	case d.Store == nil || d.Seal == nil || d.Approver == nil:
		return nil, errors.New("oauthas: Store, Seal and Approver are required")
	case len(d.ReceiptKey) == 0 || d.RPID == "" || d.Owner == "":
		return nil, errors.New("oauthas: ReceiptKey, RPID and Owner are required")
	}
	u, err := url.Parse(d.BaseURL)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") || (u.Path != "" && u.Path != "/") {
		return nil, fmt.Errorf("oauthas: BaseURL %q is not an origin", d.BaseURL)
	}
	if d.Now == nil {
		d.Now = time.Now
	}
	if d.Log == nil {
		d.Log = slog.Default()
	}
	issuer := u.Scheme + "://" + u.Host
	return &Server{d: d, issuer: issuer, resource: issuer + MCPPath}, nil
}

// Resource is the canonical resource identifier, https://<domain>/mcp.
func (s *Server) Resource() string { return s.resource }

// Handler serves the metadata, registration, authorization and token routes.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	prm := auth.ProtectedResourceMetadataHandler(&oauthex.ProtectedResourceMetadata{
		Resource:               s.resource,
		AuthorizationServers:   []string{s.issuer},
		ScopesSupported:        []string{ScopeMCP},
		BearerMethodsSupported: []string{"header"},
		ResourceName:           "moose email",
	})
	mux.Handle(PRMetadataPath, prm)
	mux.Handle(PRMetadataPath+MCPPath, prm)
	mux.Handle(ASMetadataPath, cors(http.HandlerFunc(s.asMetadata)))
	mux.Handle(RegisterPath, cors(http.HandlerFunc(s.register)))
	mux.Handle(TokenPath, cors(http.HandlerFunc(s.token)))
	mux.HandleFunc("GET "+AuthorizePath, s.authorize)
	mux.HandleFunc("GET "+PollPath, s.poll)
	mux.HandleFunc("GET "+LoginPath, s.login)
	mux.HandleFunc("GET "+LoginPollPath, s.loginPoll)
	return noStore(mux)
}

// Protect wraps next, the MCP handler, in auth.RequireBearerToken. A request
// without a valid access token for this resource gets 401 with a
// WWW-Authenticate header pointing at the protected resource metadata.
func (s *Server) Protect(next http.Handler) http.Handler {
	return auth.RequireBearerToken(s.verify, &auth.RequireBearerTokenOptions{
		ResourceMetadataURL: s.issuer + PRMetadataPath + MCPPath,
		Scopes:              []string{ScopeMCP},
	})(next)
}

// verify hashes the opaque bearer token and looks it up. It accepts only a
// live, unrevoked access token whose audience is exactly this resource.
func (s *Server) verify(ctx context.Context, token string, _ *http.Request) (*auth.TokenInfo, error) {
	sess, err := s.d.Store.GetSession(ctx, store.HashSecret(token))
	switch {
	case errors.Is(err, store.ErrNotFound):
		return nil, fmt.Errorf("%w: unknown token", auth.ErrInvalidToken)
	case err != nil:
		return nil, err
	}
	switch {
	case sess.Kind != store.SessionAccess:
		return nil, fmt.Errorf("%w: not an access token", auth.ErrInvalidToken)
	case sess.Revoked:
		return nil, fmt.Errorf("%w: revoked", auth.ErrInvalidToken)
	case !s.d.Now().Before(sess.Expires):
		return nil, fmt.Errorf("%w: expired", auth.ErrInvalidToken)
	case sess.Audience != s.resource:
		return nil, fmt.Errorf("%w: wrong audience", auth.ErrInvalidToken)
	}
	return &auth.TokenInfo{
		Scopes:     strings.Fields(sess.Scope),
		Expiration: sess.Expires,
		UserID:     sess.Owner,
		Extra:      map[string]any{"client_id": sess.ClientID},
	}, nil
}

func (s *Server) asMetadata(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"issuer":                                         s.issuer,
		"authorization_endpoint":                         s.issuer + AuthorizePath,
		"token_endpoint":                                 s.issuer + TokenPath,
		"registration_endpoint":                          s.issuer + RegisterPath,
		"scopes_supported":                               []string{ScopeMCP},
		"response_types_supported":                       []string{"code"},
		"grant_types_supported":                          []string{"authorization_code", "refresh_token"},
		"code_challenge_methods_supported":               []string{"S256"},
		"token_endpoint_auth_methods_supported":          []string{"none"},
		"authorization_response_iss_parameter_supported": true,
	})
}

// cors lets browser-based MCP clients reach the JSON endpoints. They carry no
// cookies, so a wildcard origin exposes nothing.
func cors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Access-Control-Allow-Origin", "*")
		h.Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		h.Set("Access-Control-Allow-Headers", "Content-Type, Authorization, MCP-Protocol-Version")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func noStore(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Cache-Control", "no-store")
		h.Set("Pragma", "no-cache")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "same-origin")
		next.ServeHTTP(w, r)
	})
}

// randomToken returns n random bytes, base64url encoded.
func randomToken(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(fmt.Sprintf("oauthas: random: %v", err))
	}
	return base64.RawURLEncoding.EncodeToString(b)
}
