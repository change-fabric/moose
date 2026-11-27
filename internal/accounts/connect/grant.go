package connect

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"golang.org/x/oauth2"

	"github.com/change-fabric/moose/internal/accounts/envelope"
	"github.com/change-fabric/moose/internal/accounts/scopes"
	"github.com/change-fabric/moose/internal/accounts/store"
)

// Incremental consent: the "Grant calendar" button on an account. The attempt is
// one ConnectState bound to the account, the provider and the service. The
// callback accepts it only when the identity the provider returns is the
// account's own; otherwise it stores nothing and the mail credential stays as
// it was.

// grantCalendar starts a calendar consent for one account.
func (s *Server) grantCalendar(w http.ResponseWriter, r *http.Request) {
	owner, ok := s.ownerPost(w, r)
	if !ok {
		return
	}
	a, ok := s.account(w, r, owner)
	if !ok {
		return
	}
	if !s.d.CalendarEnabled {
		s.fail(w, http.StatusNotFound, "Calendar not available", "Calendar is not turned on for this moose.")
		return
	}
	ad, err := s.adapters.Get(accountProvider(a))
	if err != nil {
		s.fail(w, http.StatusNotFound, "Provider not available", "That account's provider is not available on this moose.")
		return
	}
	if a.Status != store.StatusOK {
		s.fail(w, http.StatusConflict, "Account not ready", fmt.Sprintf("%s is %s. Reconnect it before granting calendar access.", a.Address, strings.ReplaceAll(a.Status, "_", " ")))
		return
	}
	cfg, err := ad.GrantConfig(store.ServiceCalendar, a)
	if err != nil {
		s.fail(w, http.StatusConflict, "Calendar not supported", ad.Label()+" accounts cannot be granted calendar access here.")
		return
	}
	ctx := r.Context()
	state := randomToken(32)
	verifier := oauth2.GenerateVerifier()
	if err := s.d.Store.PutConnectState(ctx, store.ConnectState{
		Hash: store.HashSecret(state), Owner: owner, Verifier: verifier, Provider: ad.Name(),
		GrantAccountID: a.ID, Service: store.ServiceCalendar, Expires: s.d.Now().Add(store.ConnectStateTTL),
	}); err != nil {
		s.internal(w, "store connect state", err)
		return
	}
	opts := append([]oauth2.AuthCodeOption{oauth2.S256ChallengeOption(verifier)}, ad.GrantOptions(a.Address)...)
	http.Redirect(w, r, cfg.AuthCodeURL(state, opts...), http.StatusSeeOther)
}

// grantCallback finishes a calendar consent: exchange the code, check the
// signed-in identity is the account's, then merge what was granted.
func (s *Server) grantCallback(w http.ResponseWriter, r *http.Request, ad Adapter, st store.ConnectState, owner, code string) {
	ctx := r.Context()
	label := ad.Label()
	if st.Service != store.ServiceCalendar {
		s.fail(w, http.StatusBadRequest, "Connect link invalid", "This connect attempt named a service moose does not offer. Start again from the connect page.")
		return
	}
	acct, err := s.d.Store.GetAccount(ctx, owner, st.GrantAccountID)
	if errors.Is(err, store.ErrNotFound) {
		s.fail(w, http.StatusNotFound, "Account not found", "That account is no longer connected here. Nothing was changed.")
		return
	}
	if err != nil {
		s.internal(w, "get account", err)
		return
	}
	if accountProvider(acct) != ad.Name() {
		s.fail(w, http.StatusBadRequest, "Connect link invalid", "This connect attempt was started for a different provider. Start again from the connect page.")
		return
	}
	cfg, err := ad.GrantConfig(st.Service, acct)
	if err != nil {
		s.fail(w, http.StatusConflict, "Calendar not supported", label+" accounts cannot be granted calendar access here.")
		return
	}
	xctx := ctx
	if s.d.HTTPClient != nil {
		xctx = context.WithValue(ctx, oauth2.HTTPClient, s.d.HTTPClient)
	}
	tok, err := cfg.Exchange(xctx, code, oauth2.VerifierOption(st.Verifier))
	if err != nil {
		s.d.Log.Warn("connect: calendar token exchange failed", "provider", ad.Name(), "err", err.Error())
		s.fail(w, http.StatusBadGateway, label+" refused the code", "The authorization code could not be exchanged. Nothing was changed. Start again.")
		return
	}
	// Who signed in is checked before anything is stored.
	prof, err := ad.Profile(xctx, tok)
	if err != nil || prof.Address == "" {
		s.d.Log.Warn("connect: calendar profile lookup failed", "provider", ad.Name(), "err", fmt.Sprint(err))
		s.fail(w, http.StatusBadGateway, "Could not confirm the account", label+" accepted the code but the account could not be identified. Nothing was changed. Start again.")
		return
	}
	uid := prof.ProviderUID
	if uid == "" {
		uid = strings.ToLower(strings.TrimSpace(prof.Address))
	}
	if !strings.EqualFold(uid, acct.ProviderUID) {
		s.d.Log.Warn("connect: calendar consent for a different identity", "provider", ad.Name(), "account_id", acct.ID)
		s.fail(w, http.StatusConflict, "Different "+label+" account",
			fmt.Sprintf("You signed in to %s as %s, but %s (%s) is a different account. Nothing was changed and mail still works. Start again and sign in as %s.",
				label, prof.Address, acct.Nickname, acct.Address, acct.Address))
		return
	}
	var reported []string
	if raw, _ := tok.Extra("scope").(string); raw != "" {
		reported = strings.Fields(raw)
	}
	var granted []string
	if len(reported) == 0 {
		// The provider did not report scopes, which it does only when they equal
		// the request: take the request as given.
		granted = scopes.Required(ad.Name(), st.Service, acct.ProviderUID)
	} else {
		granted = scopes.Of(ad.Name(), st.Service, reported)
	}
	if len(granted) == 0 {
		s.fail(w, http.StatusBadRequest, "No calendar permissions granted",
			label+" did not grant any calendar permission, so nothing was changed. Mail still works. Grant calendar again and allow at least one.")
		return
	}
	now := s.d.Now().UTC()
	// The credential goes first, so a crash between the two writes leaves a token
	// that covers more than the record says, never less. The returned refresh
	// token covers every scope granted so far; without one the stored token stays.
	if tok.RefreshToken != "" {
		sealed, err := s.d.Seal.Seal(ctx, []byte(tok.RefreshToken), envelope.Context{Owner: owner, AccountID: acct.ID})
		if err != nil {
			s.internal(w, "seal credential", err)
			return
		}
		if err := s.writeCredential(ctx, owner, acct.ID, sealed); err != nil {
			s.internal(w, "write credential", err)
			return
		}
	}
	// Merge on a fresh read: a token refresh may have persisted scopes meanwhile.
	cur, err := s.d.Store.GetAccount(ctx, owner, acct.ID)
	if err != nil {
		s.internal(w, "get account", err)
		return
	}
	patch := store.AccountPatch{
		Scopes:   store.Ptr(scopes.Union(cur.Scopes, granted)),
		Services: store.Ptr(mergeServiceGrant(cur.Services, st.Service, granted, now)),
	}
	if err := s.d.Store.UpdateAccount(ctx, owner, acct.ID, patch); err != nil {
		s.internal(w, "save grant", err)
		return
	}
	dest := PagePath + "?granted=" + url.QueryEscape(acct.ID)
	if len(scopes.Missing(ad.Name(), st.Service, acct.ProviderUID, granted)) > 0 {
		dest += "&partial=1"
	}
	http.Redirect(w, r, dest, http.StatusSeeOther)
}

// mergeServiceGrant records granted for service on top of what the service
// already held, keeping every other service.
func mergeServiceGrant(have map[string]store.ServiceGrant, service string, granted []string, now time.Time) map[string]store.ServiceGrant {
	out := make(map[string]store.ServiceGrant, len(have)+1)
	for k, v := range have {
		out[k] = v
	}
	prev := out[service]
	out[service] = store.ServiceGrant{Scopes: scopes.Union(prev.Scopes, granted), GrantedAt: now}
	return out
}
