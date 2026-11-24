package oauthas

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/change-fabric/moose/internal/email/store"
)

const (
	maxRegisterBody  = 16 << 10
	maxRedirectURIs  = 5
	maxRedirectLen   = 2048
	maxClientNameLen = 60
)

// loopbackHost reports the two hosts RFC 8252 loopback matching applies to.
func loopbackHost(h string) bool { return h == "127.0.0.1" || h == "localhost" }

// checkRegistrable validates one redirect URI offered at registration: an
// absolute https URI, or an http URI on 127.0.0.1 or localhost, with no
// userinfo and no fragment.
func checkRegistrable(raw string) error {
	if raw == "" || len(raw) > maxRedirectLen || strings.TrimSpace(raw) != raw {
		return errors.New("redirect URI is empty, too long or has surrounding space")
	}
	if strings.ContainsFunc(raw, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) || r > unicode.MaxASCII }) {
		return errors.New("redirect URI must be plain ASCII with no whitespace or control characters")
	}
	u, err := url.Parse(raw)
	switch {
	case err != nil:
		return fmt.Errorf("redirect URI %q does not parse", raw)
	case u.Host == "" || u.Opaque != "":
		return fmt.Errorf("redirect URI %q is not absolute", raw)
	case u.User != nil:
		return fmt.Errorf("redirect URI %q must not carry credentials", raw)
	case u.Fragment != "" || strings.Contains(raw, "#"):
		return fmt.Errorf("redirect URI %q must not have a fragment", raw)
	case u.Scheme == "https":
		return nil
	case u.Scheme == "http" && loopbackHost(u.Hostname()):
		return nil
	}
	return fmt.Errorf("redirect URI %q must be https, or http on 127.0.0.1 or localhost", raw)
}

// MatchRedirect reports whether got is one of the registered redirect URIs.
// The match is exact. The one exception is RFC 8252 loopback: a registered
// http://127.0.0.1 or http://localhost URI also matches the same URI with any
// port, but never another host, scheme, path or query.
func MatchRedirect(registered []string, got string) bool {
	if checkRegistrable(got) != nil {
		return false
	}
	if slices.Contains(registered, got) {
		return true
	}
	g, err := url.Parse(got)
	if err != nil || g.Scheme != "http" || !loopbackHost(g.Hostname()) {
		return false
	}
	for _, reg := range registered {
		r, err := url.Parse(reg)
		if err != nil || r.Scheme != "http" || !loopbackHost(r.Hostname()) {
			continue
		}
		if r.Hostname() == g.Hostname() && r.EscapedPath() == g.EscapedPath() && r.RawQuery == g.RawQuery {
			return true
		}
	}
	return false
}

// cleanName makes a client-chosen name safe to show on the approval page:
// whitespace collapsed, control and format characters dropped, at most 60
// characters.
func cleanName(name string) string {
	var b strings.Builder
	for _, r := range name {
		switch {
		case unicode.IsSpace(r):
			b.WriteRune(' ')
		case unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || !unicode.IsPrint(r):
		default:
			b.WriteRune(r)
		}
	}
	out := strings.Join(strings.Fields(b.String()), " ")
	if utf8.RuneCountInString(out) > maxClientNameLen {
		out = string([]rune(out)[:maxClientNameLen])
	}
	if out == "" {
		return "Unnamed application"
	}
	return out
}

type registerRequest struct {
	ClientName              string   `json:"client_name"`
	RedirectURIs            []string `json:"redirect_uris"`
	TokenEndpointAuthMethod string   `json:"token_endpoint_auth_method"`
	GrantTypes              []string `json:"grant_types"`
	ResponseTypes           []string `json:"response_types"`
}

// register is RFC 7591 dynamic client registration for public clients.
func (s *Server) register(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST, OPTIONS")
		oauthError(w, http.StatusMethodNotAllowed, "invalid_request", "use POST")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxRegisterBody))
	if err != nil {
		oauthError(w, http.StatusRequestEntityTooLarge, "invalid_client_metadata", "registration body is too large")
		return
	}
	var req registerRequest
	if err := json.Unmarshal(body, &req); err != nil {
		oauthError(w, http.StatusBadRequest, "invalid_client_metadata", "body is not a JSON object")
		return
	}
	if n := len(req.RedirectURIs); n == 0 || n > maxRedirectURIs {
		oauthError(w, http.StatusBadRequest, "invalid_redirect_uri", fmt.Sprintf("give 1 to %d redirect_uris", maxRedirectURIs))
		return
	}
	seen := map[string]bool{}
	for _, u := range req.RedirectURIs {
		if err := checkRegistrable(u); err != nil {
			oauthError(w, http.StatusBadRequest, "invalid_redirect_uri", err.Error())
			return
		}
		if seen[u] {
			oauthError(w, http.StatusBadRequest, "invalid_redirect_uri", "duplicate redirect URI")
			return
		}
		seen[u] = true
	}
	if m := req.TokenEndpointAuthMethod; m != "" && m != "none" {
		oauthError(w, http.StatusBadRequest, "invalid_client_metadata", `only token_endpoint_auth_method "none" (public clients) is supported`)
		return
	}
	for _, g := range req.GrantTypes {
		if g != "authorization_code" && g != "refresh_token" {
			oauthError(w, http.StatusBadRequest, "invalid_client_metadata", "unsupported grant_type "+g)
			return
		}
	}
	for _, t := range req.ResponseTypes {
		if t != "code" {
			oauthError(w, http.StatusBadRequest, "invalid_client_metadata", "unsupported response_type "+t)
			return
		}
	}
	c := store.OAuthClient{
		ID: "mc_" + randomToken(18), Name: cleanName(req.ClientName),
		RedirectURIs: req.RedirectURIs, Created: s.d.Now().UTC(),
	}
	if err := s.d.Store.CreateOAuthClient(r.Context(), c); err != nil {
		s.d.Log.Error("oauthas: store client", "err", err)
		oauthError(w, http.StatusInternalServerError, "server_error", "could not store the registration")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"client_id":                  c.ID,
		"client_id_issued_at":        c.Created.Unix(),
		"client_name":                c.Name,
		"redirect_uris":              c.RedirectURIs,
		"grant_types":                []string{"authorization_code", "refresh_token"},
		"response_types":             []string{"code"},
		"token_endpoint_auth_method": "none",
		"scope":                      ScopeMCP,
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// oauthError writes an RFC 6749 section 5.2 style error body.
func oauthError(w http.ResponseWriter, status int, code, desc string) {
	writeJSON(w, status, map[string]string{"error": code, "error_description": desc})
}
