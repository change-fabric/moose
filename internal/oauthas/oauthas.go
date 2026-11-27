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
//   - the resource (RFC 8707) must be one of the configured resources (email at
//     https://<domain>/email/mcp, calendar at https://<domain>/calendar/mcp),
//     codes, refresh families and tokens are bound to that one resource and
//     record it as their audience, and the bearer verifier of each resource
//     rejects any other audience: a token for one resource is dead on the other;
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
	// MCPPath is the email resource's path, the default resource. Each moose
	// module serves MCP under its own prefix on the shared host.
	MCPPath = "/email/mcp"
	// CalendarMCPPath is the calendar resource's path.
	CalendarMCPPath = "/calendar/mcp"
)

// Resource is one protected MCP resource the authorization server issues
// tokens for. Each resource has its own audience, so a client connects to it
// with its own login (its own authorization and token exchange) and the token
// it gets is useless anywhere else.
type Resource struct {
	// Path is where the resource serves MCP, such as "/email/mcp".
	Path string
	// Name is shown on the consent page and in the metadata ("moose email").
	Name string
	// Audience is the canonical resource identifier, https://<domain><Path>. New
	// fills it in; set it only to override.
	Audience string
}

// EmailResource and CalendarResource are the two resources moose serves.
var (
	EmailResource    = Resource{Path: MCPPath, Name: "moose email"}
	CalendarResource = Resource{Path: CalendarMCPPath, Name: "moose calendar"}
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
	// BaseURL + each resource's Path is a resource tokens are issued for.
	BaseURL string
	// Resources are the protected resources. Empty means the email resource
	// alone. A /authorize request that names no resource gets the first.
	Resources []Resource
	// InsecureCookies drops the Secure cookie flag, for http://localhost in dev.
	InsecureCookies bool
	Now             func() time.Time
	Log             *slog.Logger
}

// Server is the authorization server.
type Server struct {
	d      Deps
	issuer string
	// resources are the configured resources with audiences filled in; the
	// first is the default.
	resources []Resource
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
	resources, err := resolveResources(issuer, d.Resources)
	if err != nil {
		return nil, err
	}
	return &Server{d: d, issuer: issuer, resources: resources}, nil
}

// resolveResources fills in audiences and validates the list: at least one,
// paths absolute and unique, audiences unique.
func resolveResources(issuer string, in []Resource) ([]Resource, error) {
	if len(in) == 0 {
		in = []Resource{EmailResource}
	}
	out := make([]Resource, 0, len(in))
	seenPath, seenAud := map[string]bool{}, map[string]bool{}
	for _, r := range in {
		if !strings.HasPrefix(r.Path, "/") || strings.HasSuffix(r.Path, "/") || strings.ContainsAny(r.Path, "?#") {
			return nil, fmt.Errorf("oauthas: resource path %q must start with / and have no trailing slash, query or fragment", r.Path)
		}
		if r.Audience == "" {
			r.Audience = issuer + r.Path
		}
		if r.Name == "" {
			r.Name = "moose"
		}
		if seenPath[r.Path] || seenAud[r.Audience] {
			return nil, fmt.Errorf("oauthas: resource %q is listed twice", r.Path)
		}
		seenPath[r.Path], seenAud[r.Audience] = true, true
		out = append(out, r)
	}
	return out, nil
}

// Resource is the default resource's canonical identifier, the audience of the
// first configured resource (https://<domain>/email/mcp unless configured).
func (s *Server) Resource() string { return s.resources[0].Audience }

// Resources lists the configured resources, default first.
func (s *Server) Resources() []Resource { return append([]Resource(nil), s.resources...) }

// resourceByAudience finds a configured resource by its canonical identifier.
func (s *Server) resourceByAudience(aud string) (Resource, bool) {
	for _, r := range s.resources {
		if r.Audience == aud {
			return r, true
		}
	}
	return Resource{}, false
}

// resourceName is the display name for an audience, "moose" for an unknown one.
func (s *Server) resourceName(aud string) string {
	if r, ok := s.resourceByAudience(aud); ok {
		return r.Name
	}
	return "moose"
}

// Handler serves the metadata, registration, authorization and token routes.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	// One RFC 9728 document per resource, mounted at its path-suffixed
	// location; the bare location answers for the default resource.
	for i, r := range s.resources {
		prm := auth.ProtectedResourceMetadataHandler(&oauthex.ProtectedResourceMetadata{
			Resource:               r.Audience,
			AuthorizationServers:   []string{s.issuer},
			ScopesSupported:        []string{ScopeMCP},
			BearerMethodsSupported: []string{"header"},
			ResourceName:           r.Name,
		})
		if i == 0 {
			mux.Handle(PRMetadataPath, prm)
		}
		mux.Handle(PRMetadataPath+r.Path, prm)
	}
	mux.Handle(ASMetadataPath, cors(http.HandlerFunc(s.asMetadata)))
	mux.Handle(RegisterPath, cors(http.HandlerFunc(s.register)))
	mux.Handle(TokenPath, cors(http.HandlerFunc(s.token)))
	mux.HandleFunc("GET "+AuthorizePath, s.authorize)
	mux.HandleFunc("GET "+PollPath, s.poll)
	mux.HandleFunc("GET "+LoginPath, s.login)
	mux.HandleFunc("GET "+LoginPollPath, s.loginPoll)
	return noStore(mux)
}

// Protect wraps next, the default resource's MCP handler, in
// auth.RequireBearerToken. See ProtectResource.
func (s *Server) Protect(next http.Handler) http.Handler {
	return s.ProtectResource(s.resources[0].Path, next)
}

// ProtectResource wraps next, the MCP handler of the resource served at path,
// in auth.RequireBearerToken. A request without a valid access token for that
// resource gets 401 with a WWW-Authenticate header pointing at the resource's
// protected resource metadata. It panics if path is not a configured resource,
// which is a wiring mistake.
func (s *Server) ProtectResource(path string, next http.Handler) http.Handler {
	for _, r := range s.resources {
		if r.Path == path {
			return NewVerifier(s.d.Store, s.issuer, r, s.d.Now).Protect(next)
		}
	}
	panic("oauthas: ProtectResource: no resource at " + path)
}

// Verifier checks bearer tokens for one resource. It needs only the store, so
// a server that is not the authorization server (the calendar Lambda) can
// protect its own endpoint with the tokens this one issued.
type Verifier struct {
	store    store.Store
	issuer   string
	resource Resource
	now      func() time.Time
}

// NewVerifier returns the verifier for resource, whose audience is
// issuer + Path unless Audience is set. now may be nil.
func NewVerifier(st store.Store, issuer string, resource Resource, now func() time.Time) *Verifier {
	issuer = strings.TrimRight(issuer, "/")
	if resource.Audience == "" {
		resource.Audience = issuer + resource.Path
	}
	if now == nil {
		now = time.Now
	}
	return &Verifier{store: st, issuer: issuer, resource: resource, now: now}
}

// Protect wraps next in auth.RequireBearerToken for this resource.
func (v *Verifier) Protect(next http.Handler) http.Handler {
	return auth.RequireBearerToken(v.verify, &auth.RequireBearerTokenOptions{
		ResourceMetadataURL: v.issuer + PRMetadataPath + v.resource.Path,
		Scopes:              []string{ScopeMCP},
	})(next)
}

// verify hashes the opaque bearer token and looks it up. It accepts only a
// live, unrevoked access token whose audience is exactly this resource.
func (v *Verifier) verify(ctx context.Context, token string, _ *http.Request) (*auth.TokenInfo, error) {
	sess, err := v.store.GetSession(ctx, store.HashSecret(token))
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
	case !v.now().Before(sess.Expires):
		return nil, fmt.Errorf("%w: expired", auth.ErrInvalidToken)
	case sess.Audience != v.resource.Audience:
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
