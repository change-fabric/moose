package oauthas

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/change-fabric/moose/internal/accounts/envelope"
	"github.com/change-fabric/moose/internal/accounts/store"
)

const maxTokenBody = 16 << 10

var verifierRE = regexp.MustCompile(`^[A-Za-z0-9._~-]{43,128}$`)

// token serves the authorization_code and refresh_token grants for public
// clients (no client secret; PKCE is the proof of possession).
func (s *Server) token(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST, OPTIONS")
		oauthError(w, http.StatusMethodNotAllowed, "invalid_request", "use POST")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxTokenBody)
	if err := r.ParseForm(); err != nil {
		oauthError(w, http.StatusBadRequest, "invalid_request", "body is not a form")
		return
	}
	// Parameters travel in the body only; a query string never counts.
	form := r.PostForm
	for _, k := range []string{"grant_type", "client_id", "code", "redirect_uri", "code_verifier", "refresh_token", "resource"} {
		if len(form[k]) > 1 {
			oauthError(w, http.StatusBadRequest, "invalid_request", "the parameter "+k+" appears more than once")
			return
		}
	}
	switch form.Get("grant_type") {
	case "authorization_code":
		s.codeGrant(w, r, form.Get)
	case "refresh_token":
		s.refreshGrant(w, r, form.Get)
	default:
		oauthError(w, http.StatusBadRequest, "unsupported_grant_type", "use authorization_code or refresh_token")
	}
}

type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int64  `json:"expires_in"`
	RefreshToken string `json:"refresh_token"`
	Scope        string `json:"scope"`
}

// codeGrant redeems an authorization code. The code is burned by the
// conditional redeem before any binding is checked, so a wrong verifier, client
// or redirect cannot be retried against the same code. A second redeem of a
// code revokes the tokens the first one produced.
func (s *Server) codeGrant(w http.ResponseWriter, r *http.Request, get func(string) string) {
	code, clientID, redirect, verifier := get("code"), get("client_id"), get("redirect_uri"), get("code_verifier")
	if code == "" || clientID == "" || redirect == "" || verifier == "" {
		oauthError(w, http.StatusBadRequest, "invalid_request", "code, client_id, redirect_uri and code_verifier are required")
		return
	}
	ctx := r.Context()
	now := s.d.Now()
	ac, err := s.d.Store.RedeemAuthCode(ctx, store.HashSecret(code), now)
	switch {
	case errors.Is(err, store.ErrAlreadyRedeemed):
		if rerr := s.d.Store.RevokeFamily(ctx, ac.FamilyID); rerr != nil {
			s.d.Log.Error("oauthas: revoke family after code replay", "err", rerr)
		}
		oauthError(w, http.StatusBadRequest, "invalid_grant", "the authorization code was already used")
		return
	case errors.Is(err, store.ErrNotFound), errors.Is(err, store.ErrExpired):
		oauthError(w, http.StatusBadRequest, "invalid_grant", "the authorization code is invalid or expired")
		return
	case err != nil:
		s.d.Log.Error("oauthas: redeem code", "err", err)
		oauthError(w, http.StatusInternalServerError, "server_error", "could not redeem the code")
		return
	}
	switch {
	case ac.ClientID != clientID:
		oauthError(w, http.StatusBadRequest, "invalid_grant", "the code was issued to another client")
		return
	case ac.RedirectURI != redirect:
		oauthError(w, http.StatusBadRequest, "invalid_grant", "redirect_uri does not match the authorization request")
		return
	case !verifierRE.MatchString(verifier) || !pkceMatches(verifier, ac.CodeChallenge):
		oauthError(w, http.StatusBadRequest, "invalid_grant", "code_verifier does not match the code_challenge")
		return
	}
	if res := get("resource"); res != "" && res != ac.Resource {
		oauthError(w, http.StatusBadRequest, "invalid_target", "resource does not match the authorization request")
		return
	}
	access, refresh := "mat_"+randomToken(32), ac.FamilyID+"."+randomToken(32)
	fam := store.RefreshFamily{
		ID: ac.FamilyID, Owner: ac.Owner, ClientID: ac.ClientID, Audience: ac.Resource,
		CurrentHash: store.HashSecret(refresh), Created: now.UTC(), LastRotated: now.UTC(),
		IdleExpires: now.Add(store.RefreshIdleTTL).UTC(), AbsExpires: now.Add(store.RefreshAbsoluteTTL).UTC(),
	}
	if err := s.d.Store.CreateRefreshFamily(ctx, fam); err != nil {
		s.d.Log.Error("oauthas: create family", "err", err)
		oauthError(w, http.StatusInternalServerError, "server_error", "could not issue tokens")
		return
	}
	if err := s.createAccess(ctx, ac.Owner, ac.ClientID, ac.FamilyID, ac.Resource, access, now); err != nil {
		s.d.Log.Error("oauthas: create access token", "err", err)
		oauthError(w, http.StatusInternalServerError, "server_error", "could not issue tokens")
		return
	}
	writeJSON(w, http.StatusOK, tokenResponse{
		AccessToken: access, TokenType: "Bearer", ExpiresIn: int64(AccessTTL / time.Second),
		RefreshToken: refresh, Scope: ScopeMCP,
	})
}

func pkceMatches(verifier, challenge string) bool {
	sum := sha256.Sum256([]byte(verifier))
	got := base64.RawURLEncoding.EncodeToString(sum[:])
	return subtle.ConstantTimeCompare([]byte(got), []byte(challenge)) == 1
}

func (s *Server) createAccess(ctx context.Context, owner, clientID, familyID, resource, access string, now time.Time) error {
	return s.d.Store.CreateSession(ctx, store.Session{
		Hash: store.HashSecret(access), Kind: store.SessionAccess, Owner: owner, ClientID: clientID,
		FamilyID: familyID, Audience: resource, Scope: ScopeMCP,
		Created: now.UTC(), Expires: now.Add(AccessTTL).UTC(),
	})
}

// gracePair is what the winner of a refresh race hands out, sealed on the family
// so a concurrent loser can be given the same pair once.
type gracePair struct {
	Access  string `json:"a"`
	Refresh string `json:"r"`
	Expires int64  `json:"e"`
}

func pairContext(owner, familyID string) envelope.Context {
	return envelope.Context{Owner: owner, AccountID: "oauth-family:" + familyID}
}

// refreshGrant rotates a refresh token. Each rotation is one conditional write
// on the family's current hash, so of two concurrent refreshes one wins. The
// loser, replaying the old token within the 10 second grace window, is given
// the winner's pair once; any other reuse revokes the whole family.
func (s *Server) refreshGrant(w http.ResponseWriter, r *http.Request, get func(string) string) {
	rt, clientID := get("refresh_token"), get("client_id")
	if rt == "" || clientID == "" {
		oauthError(w, http.StatusBadRequest, "invalid_request", "refresh_token and client_id are required")
		return
	}
	famID, _, ok := strings.Cut(rt, ".")
	if !ok || famID == "" {
		oauthError(w, http.StatusBadRequest, "invalid_grant", "the refresh token is invalid")
		return
	}
	ctx := r.Context()
	now := s.d.Now()
	fam, err := s.d.Store.GetRefreshFamily(ctx, famID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		oauthError(w, http.StatusBadRequest, "invalid_grant", "the refresh token is invalid")
		return
	case err != nil:
		s.d.Log.Error("oauthas: load family", "err", err)
		oauthError(w, http.StatusInternalServerError, "server_error", "could not refresh")
		return
	}
	if fam.ClientID != clientID {
		oauthError(w, http.StatusBadRequest, "invalid_grant", "the refresh token was issued to another client")
		return
	}
	if res := get("resource"); res != "" && res != fam.Audience {
		oauthError(w, http.StatusBadRequest, "invalid_target", "resource does not match the grant")
		return
	}
	if fam.Revoked {
		oauthError(w, http.StatusBadRequest, "invalid_grant", "the refresh token was revoked")
		return
	}

	access, refresh := "mat_"+randomToken(32), famID+"."+randomToken(32)
	expires := now.Add(AccessTTL)
	sealed, err := s.sealPair(ctx, fam, gracePair{Access: access, Refresh: refresh, Expires: expires.Unix()})
	if err != nil {
		s.d.Log.Error("oauthas: seal grace pair", "err", err)
		oauthError(w, http.StatusInternalServerError, "server_error", "could not refresh")
		return
	}
	// The access session exists before the rotation so a winner never hands
	// out a token that is not yet valid. A loser revokes its unused one.
	if err := s.createAccess(ctx, fam.Owner, fam.ClientID, famID, fam.Audience, access, now); err != nil {
		s.d.Log.Error("oauthas: create access token", "err", err)
		oauthError(w, http.StatusInternalServerError, "server_error", "could not refresh")
		return
	}
	discard := func() {
		if err := s.d.Store.RevokeSession(ctx, store.HashSecret(access)); err != nil && !errors.Is(err, store.ErrNotFound) {
			s.d.Log.Warn("oauthas: revoke unused access token", "err", err)
		}
	}
	out, err := s.d.Store.RotateRefreshWithPair(ctx, famID, store.HashSecret(rt), store.HashSecret(refresh), sealed, now)
	if err != nil {
		discard()
		s.d.Log.Error("oauthas: rotate refresh", "err", err)
		oauthError(w, http.StatusInternalServerError, "server_error", "could not refresh")
		return
	}
	switch out {
	case store.OutcomeRotated:
		writeJSON(w, http.StatusOK, tokenResponse{
			AccessToken: access, TokenType: "Bearer", ExpiresIn: int64(AccessTTL / time.Second),
			RefreshToken: refresh, Scope: ScopeMCP,
		})
	case store.OutcomeGraceReplay:
		discard()
		p, ok := s.winnersPair(ctx, famID, now)
		if !ok {
			oauthError(w, http.StatusBadRequest, "invalid_grant", "the refresh token was already used")
			return
		}
		writeJSON(w, http.StatusOK, tokenResponse{
			AccessToken: p.Access, TokenType: "Bearer", ExpiresIn: max(1, p.Expires-now.Unix()),
			RefreshToken: p.Refresh, Scope: ScopeMCP,
		})
	case store.OutcomeReused:
		discard()
		oauthError(w, http.StatusBadRequest, "invalid_grant", "the refresh token was already used; the grant is revoked")
	default: // revoked, expired
		discard()
		oauthError(w, http.StatusBadRequest, "invalid_grant", "the refresh token is no longer valid")
	}
}

func (s *Server) sealPair(ctx context.Context, fam store.RefreshFamily, p gracePair) ([]byte, error) {
	raw, err := json.Marshal(p)
	if err != nil {
		return nil, err
	}
	return s.d.Seal.Seal(ctx, raw, pairContext(fam.Owner, fam.ID))
}

// winnersPair reads back the pair the winning rotation stored.
func (s *Server) winnersPair(ctx context.Context, famID string, now time.Time) (gracePair, bool) {
	fam, err := s.d.Store.GetRefreshFamily(ctx, famID)
	if err != nil || fam.Revoked || len(fam.GracePair) == 0 {
		return gracePair{}, false
	}
	raw, err := s.d.Seal.Open(ctx, fam.GracePair, pairContext(fam.Owner, fam.ID))
	if err != nil {
		s.d.Log.Warn("oauthas: open grace pair", "err", err)
		return gracePair{}, false
	}
	var p gracePair
	if json.Unmarshal(raw, &p) != nil || p.Access == "" || p.Refresh == "" || p.Expires <= now.Unix() {
		return gracePair{}, false
	}
	return p, true
}
