// Package api serves the moose REST surface (/v1/...). Handlers are thin
// adapters over core.Service and store.Store; authentication and auditing are
// applied around them by internal/server.
package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/change-fabric/moose/internal/audit"
	"github.com/change-fabric/moose/internal/auth"
	"github.com/change-fabric/moose/internal/connectors/gdrive"
	"github.com/change-fabric/moose/internal/core"
	"github.com/change-fabric/moose/internal/store"
)

// Limits for request handling.
const (
	maxBodyBytes      = 16 << 10
	defaultAuditLimit = 100
	maxAuditLimit     = 1000
	defaultAuditSince = 24 * time.Hour
)

// Deps are the collaborators the REST handlers need.
type Deps struct {
	Service *core.Service
	Store   store.Store
	Version string
	Now     func() time.Time
}

// API holds the REST handlers.
type API struct{ d Deps }

// New returns the REST handlers over d.
func New(d Deps) *API {
	if d.Now == nil {
		d.Now = time.Now
	}
	return &API{d: d}
}

// Register mounts every route except POST /v1/bootstrap/token, which the
// server wires to auth.Authenticator.Bootstrap.
func (a *API) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /healthz", a.healthz)
	mux.HandleFunc("GET /v1/whoami", a.whoami)
	mux.HandleFunc("GET /v1/sources", a.sources)
	mux.HandleFunc("GET /v1/search", a.search)
	mux.HandleFunc("GET /v1/documents/{source}/{id...}", a.getDocument)
	mux.HandleFunc("GET /v1/list", a.list)
	mux.HandleFunc("POST /v1/tokens", a.createToken)
	mux.HandleFunc("GET /v1/tokens", a.listTokens)
	mux.HandleFunc("DELETE /v1/tokens/{id}", a.revokeToken)
	mux.Handle("POST /v1/users", auth.RequireAdmin(http.HandlerFunc(a.addUser)))
	mux.Handle("GET /v1/users", auth.RequireAdmin(http.HandlerFunc(a.listUsers)))
	mux.Handle("GET /v1/audit", auth.RequireAdmin(http.HandlerFunc(a.listAudit)))
}

// Response shapes. Store records carry no JSON tags and a token hash, so the
// API never encodes them directly.

// WhoAmI is the GET /v1/whoami body.
type WhoAmI struct {
	Login    string        `json:"login"`
	Role     string        `json:"role"`
	TokenID  string        `json:"token_id"`
	Identity auth.Identity `json:"identity"`
}

// TokenInfo describes a token without its secret or hash.
type TokenInfo struct {
	ID       string     `json:"id"`
	Owner    string     `json:"owner"`
	Name     string     `json:"name"`
	Created  time.Time  `json:"created"`
	LastUsed *time.Time `json:"last_used,omitempty"`
	Expires  *time.Time `json:"expires,omitempty"`
	Revoked  bool       `json:"revoked"`
}

// CreatedToken is the POST /v1/tokens body; Token is shown once.
type CreatedToken struct {
	TokenInfo
	Token string `json:"token"`
}

// UserInfo is a moose user.
type UserInfo struct {
	Login    string    `json:"login"`
	Role     string    `json:"role"`
	Created  time.Time `json:"created"`
	Disabled bool      `json:"disabled"`
}

// AuditInfo is one audit event.
type AuditInfo struct {
	At        time.Time `json:"at"`
	RequestID string    `json:"request_id"`
	Actor     string    `json:"actor,omitempty"`
	TokenID   string    `json:"token_id,omitempty"`
	Surface   string    `json:"surface"`
	Action    string    `json:"action"`
	Source    string    `json:"source,omitempty"`
	DocID     string    `json:"doc_id,omitempty"`
	Status    int       `json:"status"`
	LatencyMS int64     `json:"latency_ms"`
	Detail    string    `json:"detail,omitempty"`
}

// SourceInfo is one entry of GET /v1/sources.
type SourceInfo struct {
	ID      string `json:"id"`
	Kind    string `json:"kind"`
	Name    string `json:"name"`
	Healthy bool   `json:"healthy"`
	Detail  string `json:"detail,omitempty"`
}

// ResultPage is a search or list result.
type ResultPage struct {
	Items         []core.DocRef `json:"items"`
	NextPageToken string        `json:"next_page_token,omitempty"`
}

func tokenInfo(t store.Token) TokenInfo {
	return TokenInfo{ID: t.ID, Owner: t.Owner, Name: t.Name, Created: t.Created, LastUsed: t.LastUsed, Expires: t.Expires, Revoked: t.Revoked}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, r *http.Request, status int, code, msg string) {
	audit.FromContext(r.Context()).SetDetail(code)
	auth.WriteError(w, status, code, msg)
}

// writeErr maps core, connector, and store errors to the plan's status table.
func writeErr(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, core.ErrNotFound), errors.Is(err, store.ErrNotFound):
		fail(w, r, http.StatusNotFound, "not_found", core.ErrNotFound.Error())
	case errors.Is(err, core.ErrInvalidInput), errors.Is(err, core.ErrUnknownSource):
		fail(w, r, http.StatusBadRequest, "invalid_request", err.Error())
	case errors.Is(err, gdrive.ErrDocumentTooLarge):
		fail(w, r, http.StatusRequestEntityTooLarge, "document_too_large", "document is too large to export; open its URL instead")
	case errors.Is(err, gdrive.ErrUpstreamUnavailable), errors.Is(err, core.ErrUpstreamUnavailable):
		fail(w, r, http.StatusServiceUnavailable, "upstream_unavailable", "data source unavailable, try again later")
	default:
		fail(w, r, http.StatusInternalServerError, "internal", "internal error")
	}
}

func principal(w http.ResponseWriter, r *http.Request) (auth.Principal, bool) {
	p, ok := auth.PrincipalFrom(r.Context())
	if !ok {
		fail(w, r, http.StatusUnauthorized, "unauthenticated", "authentication required")
	}
	return p, ok
}

func intParam(r *http.Request, name string, def int) (int, error) {
	v := r.URL.Query().Get(name)
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return 0, errors.New(name + " must be a non-negative integer")
	}
	return n, nil
}

func decode(w http.ResponseWriter, r *http.Request, dst any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil && !errors.Is(err, io.EOF) {
		fail(w, r, http.StatusBadRequest, "invalid_request", "invalid JSON body: "+err.Error())
		return false
	}
	return true
}

func setAction(r *http.Request, action, source, docID string) {
	audit.FromContext(r.Context()).SetAction(action, source, docID)
}

func (a *API) healthz(w http.ResponseWriter, r *http.Request) {
	setAction(r, "healthz", "", "")
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "version": a.d.Version})
}

func (a *API) whoami(w http.ResponseWriter, r *http.Request) {
	setAction(r, "whoami", "", "")
	p, ok := principal(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, WhoAmI{Login: p.Login, Role: p.Role, TokenID: p.TokenID, Identity: p.Identity})
}

func (a *API) sources(w http.ResponseWriter, r *http.Request) {
	setAction(r, "list_sources", "", "")
	out := []SourceInfo{}
	for _, s := range a.d.Service.ListSources(r.Context()) {
		out = append(out, SourceInfo{ID: s.ID, Kind: s.Kind, Name: s.Name, Healthy: s.Healthy, Detail: s.Detail})
	}
	writeJSON(w, http.StatusOK, map[string]any{"sources": out})
}

func page(res core.SearchResult) ResultPage {
	items := res.Items
	if items == nil {
		items = []core.DocRef{}
	}
	return ResultPage{Items: items, NextPageToken: res.NextPageToken}
}

func (a *API) search(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	setAction(r, "search", q.Get("source"), "")
	limit, err := intParam(r, "limit", 0)
	if err != nil {
		fail(w, r, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	text := q.Get("q")
	if strings.TrimSpace(text) == "" || len(text) > core.MaxQueryLen {
		fail(w, r, http.StatusBadRequest, "invalid_request", "q is required, 1..256 characters")
		return
	}
	res, err := a.d.Service.Search(r.Context(), core.SearchQuery{Text: text, Source: q.Get("source"), Limit: limit, PageToken: q.Get("page_token")})
	if err != nil {
		writeErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, page(res))
}

func (a *API) list(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	source := q.Get("source")
	if source == "" {
		source = gdrive.ID
	}
	setAction(r, "list", source, "")
	limit, err := intParam(r, "limit", 0)
	if err != nil {
		fail(w, r, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	res, err := a.d.Service.List(r.Context(), source, q.Get("page_token"), limit)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, page(res))
}

func (a *API) getDocument(w http.ResponseWriter, r *http.Request) {
	source, id := r.PathValue("source"), r.PathValue("id")
	setAction(r, "get_document", source, id)
	doc, err := a.d.Service.Get(r.Context(), source, id)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, doc)
}

func (a *API) createToken(w http.ResponseWriter, r *http.Request) {
	setAction(r, "token.create", "", "")
	p, ok := principal(w, r)
	if !ok {
		return
	}
	var body struct {
		Name          string `json:"name"`
		Owner         string `json:"owner"`
		ExpiresInDays *int   `json:"expires_in_days"`
	}
	if !decode(w, r, &body) {
		return
	}
	body.Name, body.Owner = strings.TrimSpace(body.Name), strings.TrimSpace(body.Owner)
	if body.Name == "" {
		fail(w, r, http.StatusBadRequest, "invalid_request", "name is required")
		return
	}
	if body.Owner == "" {
		body.Owner = p.Login
	}
	if body.Owner != p.Login && !p.IsAdmin() {
		fail(w, r, http.StatusForbidden, "forbidden", "creating a token for another owner requires admin")
		return
	}
	ttl := auth.DefaultTokenTTL
	if body.ExpiresInDays != nil {
		switch d := *body.ExpiresInDays; {
		case d < 0:
			fail(w, r, http.StatusBadRequest, "invalid_request", "expires_in_days must not be negative")
			return
		case d == 0:
			if !p.IsAdmin() {
				fail(w, r, http.StatusForbidden, "forbidden", "tokens without expiry require admin")
				return
			}
			ttl = 0
		default:
			ttl = time.Duration(d) * 24 * time.Hour
		}
	}
	ctx := r.Context()
	if !strings.HasPrefix(body.Owner, "tag:") {
		u, err := a.d.Store.GetUser(ctx, body.Owner)
		if errors.Is(err, store.ErrNotFound) {
			fail(w, r, http.StatusBadRequest, "invalid_request", "owner is not a moose user; add it first")
			return
		} else if err != nil {
			writeErr(w, r, err)
			return
		} else if u.Disabled {
			fail(w, r, http.StatusBadRequest, "invalid_request", "owner is disabled")
			return
		}
	}
	plain, tok, err := auth.NewToken(body.Owner, body.Name, ttl, a.d.Now())
	if err != nil {
		writeErr(w, r, err)
		return
	}
	if err := a.d.Store.CreateToken(ctx, tok); err != nil {
		writeErr(w, r, err)
		return
	}
	setAction(r, "", "", tok.ID)
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusCreated, CreatedToken{TokenInfo: tokenInfo(tok), Token: plain})
}

func (a *API) listTokens(w http.ResponseWriter, r *http.Request) {
	setAction(r, "token.list", "", "")
	p, ok := principal(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	owner := p.Login
	if q.Get("all") == "1" || q.Get("owner") != "" {
		if !p.IsAdmin() {
			fail(w, r, http.StatusForbidden, "forbidden", "listing other owners' tokens requires admin")
			return
		}
		owner = q.Get("owner")
		if q.Get("all") == "1" {
			owner = ""
		}
	}
	toks, err := a.d.Store.ListTokens(r.Context(), owner)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	out := make([]TokenInfo, 0, len(toks))
	for _, t := range toks {
		out = append(out, tokenInfo(t))
	}
	writeJSON(w, http.StatusOK, map[string]any{"tokens": out})
}

func (a *API) revokeToken(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	setAction(r, "token.revoke", "", id)
	p, ok := principal(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	tok, err := a.d.Store.GetToken(ctx, id)
	// Non-admins get 404 for tokens they do not own, so ids do not leak.
	if errors.Is(err, store.ErrNotFound) || (err == nil && tok.Owner != p.Login && !p.IsAdmin()) {
		fail(w, r, http.StatusNotFound, "not_found", "token not found")
		return
	} else if err != nil {
		writeErr(w, r, err)
		return
	}
	if err := a.d.Store.RevokeToken(ctx, id); err != nil {
		writeErr(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *API) addUser(w http.ResponseWriter, r *http.Request) {
	setAction(r, "user.add", "", "")
	var body struct {
		Login string `json:"login"`
		Role  string `json:"role"`
	}
	if !decode(w, r, &body) {
		return
	}
	body.Login = strings.TrimSpace(body.Login)
	if body.Role == "" {
		body.Role = auth.RoleMember
	}
	if body.Login == "" || strings.HasPrefix(body.Login, "tag:") {
		fail(w, r, http.StatusBadRequest, "invalid_request", "login is required and must be a tailnet user login")
		return
	}
	if body.Role != auth.RoleAdmin && body.Role != auth.RoleMember {
		fail(w, r, http.StatusBadRequest, "invalid_request", "role must be admin or member")
		return
	}
	setAction(r, "", "", body.Login)
	ctx := r.Context()
	u := store.User{Login: body.Login, Role: body.Role, Created: a.d.Now().UTC()}
	if prev, err := a.d.Store.GetUser(ctx, body.Login); err == nil {
		u.Created, u.Disabled = prev.Created, prev.Disabled
	} else if !errors.Is(err, store.ErrNotFound) {
		writeErr(w, r, err)
		return
	}
	if err := a.d.Store.PutUser(ctx, u); err != nil {
		writeErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, UserInfo{Login: u.Login, Role: u.Role, Created: u.Created, Disabled: u.Disabled})
}

func (a *API) listUsers(w http.ResponseWriter, r *http.Request) {
	setAction(r, "user.list", "", "")
	users, err := a.d.Store.ListUsers(r.Context())
	if err != nil {
		writeErr(w, r, err)
		return
	}
	out := make([]UserInfo, 0, len(users))
	for _, u := range users {
		out = append(out, UserInfo{Login: u.Login, Role: u.Role, Created: u.Created, Disabled: u.Disabled})
	}
	writeJSON(w, http.StatusOK, map[string]any{"users": out})
}

func (a *API) listAudit(w http.ResponseWriter, r *http.Request) {
	setAction(r, "audit.list", "", "")
	q := r.URL.Query()
	since := a.d.Now().Add(-defaultAuditSince)
	if v := q.Get("since"); v != "" {
		t, err := parseSince(v, a.d.Now())
		if err != nil {
			fail(w, r, http.StatusBadRequest, "invalid_request", err.Error())
			return
		}
		since = t
	}
	limit, err := intParam(r, "limit", defaultAuditLimit)
	if err != nil {
		fail(w, r, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	if limit == 0 {
		limit = defaultAuditLimit
	}
	limit = min(limit, maxAuditLimit)
	evs, err := a.d.Store.ListAudit(r.Context(), since, limit)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	out := make([]AuditInfo, 0, len(evs))
	for _, e := range evs {
		out = append(out, AuditInfo{At: e.At, RequestID: e.RequestID, Actor: e.Actor, TokenID: e.TokenID, Surface: e.Surface,
			Action: e.Action, Source: e.Source, DocID: e.DocID, Status: e.Status, LatencyMS: e.LatencyMS, Detail: e.Detail})
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": out})
}

// parseSince accepts an RFC 3339 time or a Go duration ("24h") before now.
func parseSince(v string, now time.Time) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return t, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil || d < 0 {
		return time.Time{}, errors.New("since must be RFC 3339 or a duration like 24h")
	}
	return now.Add(-d), nil
}
