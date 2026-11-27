package connect

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/microsoft"

	"github.com/change-fabric/moose/internal/email/graph"
	"github.com/change-fabric/moose/internal/graphapi"
)

// Microsoft scopes requested at connect (plan decision 2). Moose code and
// passkey approvals stay the only send gate, so Mail.Send is requested up
// front. offline_access is what makes Entra return a refresh token.
const (
	ScopeMailReadWrite = "Mail.ReadWrite"
	ScopeMailSend      = "Mail.Send"
	ScopeUserRead      = "User.Read"
	ScopeOfflineAccess = "offline_access"
)

// MicrosoftScopes is the full connect request.
var MicrosoftScopes = []string{
	ScopeMailReadWrite, ScopeMailSend, ScopeOfflineAccess, "openid", "profile", "email", ScopeUserRead,
}

// MicrosoftCallbackPath is the redirect URI to register on the Entra app.
const MicrosoftCallbackPath = "/connect/microsoft/callback"

const graphScopePrefix = "https://graph.microsoft.com/"

// MicrosoftConfig builds the OAuth web client for the multi-tenant Entra app on
// the /common authority, which signs in work or school and personal accounts.
func MicrosoftConfig(clientID, clientSecret, baseURL string) *oauth2.Config {
	endpoint := microsoft.AzureADEndpoint("common")
	// Entra accepts the secret in the form body; naming it avoids oauth2's
	// probe-and-retry and matches what accountprov sends on refresh.
	endpoint.AuthStyle = oauth2.AuthStyleInParams
	return &oauth2.Config{
		ClientID:     clientID,
		ClientSecret: clientSecret,
		Endpoint:     endpoint,
		RedirectURL:  strings.TrimRight(baseURL, "/") + MicrosoftCallbackPath,
		Scopes:       append([]string(nil), MicrosoftScopes...),
	}
}

// MicrosoftAdapter connects Exchange Online and Outlook.com mailboxes.
type MicrosoftAdapter struct {
	Config *oauth2.Config
	// GraphBaseURL overrides the Graph root (tests). Empty means graphapi.DefaultBaseURL.
	GraphBaseURL string
}

// NewMicrosoftAdapter wraps a Microsoft web client (see MicrosoftConfig).
func NewMicrosoftAdapter(cfg *oauth2.Config) *MicrosoftAdapter { return &MicrosoftAdapter{Config: cfg} }

// Name implements Adapter.
func (m *MicrosoftAdapter) Name() string { return ProviderMicrosoft }

// Label implements Adapter.
func (m *MicrosoftAdapter) Label() string { return "Microsoft" }

// OAuthConfig implements Adapter. Mail is the only service so far, so services
// is ignored.
func (m *MicrosoftAdapter) OAuthConfig([]string) *oauth2.Config { return m.Config }

// AuthOptions implements Adapter. A new connection shows the account picker, so
// the owner chooses between a work and a personal sign-in; a reconnect hints
// the mailbox it expects. Entra returns a refresh token because of the
// offline_access scope, not an access_type parameter.
func (m *MicrosoftAdapter) AuthOptions(reconnectAddress string) []oauth2.AuthCodeOption {
	if reconnectAddress != "" {
		return []oauth2.AuthCodeOption{oauth2.SetAuthURLParam("login_hint", reconnectAddress)}
	}
	return []oauth2.AuthCodeOption{oauth2.SetAuthURLParam("prompt", "select_account")}
}

// shortScope drops the Graph resource prefix Entra sometimes puts on scopes.
func shortScope(sc string) string { return strings.TrimPrefix(sc, graphScopePrefix) }

// MissingScopes implements Adapter. Entra reports the granted scopes with the
// token; without the report the request is taken as given. Both Mail scopes and
// User.Read are needed.
func (m *MicrosoftAdapter) MissingScopes(granted []string, _ []string) []string {
	if len(granted) == 0 {
		return nil
	}
	have := map[string]bool{}
	for _, sc := range granted {
		have[strings.ToLower(shortScope(sc))] = true
	}
	var missing []string
	for _, want := range []string{ScopeMailReadWrite, ScopeMailSend, ScopeUserRead} {
		if !have[strings.ToLower(want)] {
			missing = append(missing, want)
		}
	}
	return missing
}

// identityScopes belong to no service and are not recorded as grants.
var identityScopes = map[string]bool{"openid": true, "profile": true, "email": true, "offline_access": true}

// Grant implements Adapter.
func (m *MicrosoftAdapter) Grant(granted []string) (all, mail []string) {
	if len(granted) == 0 {
		granted = []string{ScopeMailReadWrite, ScopeMailSend, ScopeUserRead}
	}
	for _, sc := range granted {
		short := shortScope(sc)
		if identityScopes[strings.ToLower(short)] {
			continue
		}
		all = append(all, sc)
		if strings.HasPrefix(strings.ToLower(short), "mail.") {
			mail = append(mail, sc)
		}
	}
	return all, mail
}

var aadstsCode = regexp.MustCompile(`AADSTS\d{4,7}`)

// adminConsentCodes mean the tenant needs an administrator to approve moose
// before any user can: no consent yet (65001), or the grant requires admin
// permission (90094).
var adminConsentCodes = map[string]bool{"AADSTS65001": true, "AADSTS90094": true}

// AdminConsentURL is the link a tenant administrator opens to approve moose for
// the whole tenant. "organizations" is the documented generic tenant for a
// link handed to an administrator whose tenant is not known yet; the
// admin_consent answer returns to the callback, which says so.
func (m *MicrosoftAdapter) AdminConsentURL() string {
	q := url.Values{
		"client_id":    {m.Config.ClientID},
		"scope":        {graphScopePrefix + ".default"},
		"redirect_uri": {m.Config.RedirectURL},
	}
	return "https://login.microsoftonline.com/organizations/v2.0/adminconsent?" + q.Encode()
}

// CallbackError implements Adapter.
func (m *MicrosoftAdapter) CallbackError(code, description string) ErrorCopy {
	aadsts := aadstsCode.FindString(description)
	switch {
	case adminConsentCodes[aadsts] || code == "consent_required":
		return ErrorCopy{
			Title: "Your organization needs to approve moose",
			Message: "Your Microsoft 365 administrator has to approve moose before anyone in the organization can connect (" +
				orDefault(aadsts, "admin approval required") + "). Open the admin consent link yourself if you are the administrator, " +
				"or send it to them, then connect again. Nothing was saved.",
			Href: m.AdminConsentURL(), Label: "Open admin consent link",
		}
	case code == "access_denied":
		return ErrorCopy{
			Title:   "Microsoft access was declined",
			Message: "The sign-in was cancelled or the permissions were declined, so Microsoft did not connect the account. Nothing was saved. Connect again and accept the permissions to continue.",
		}
	}
	msg := "Microsoft answered: " + code
	if aadsts != "" {
		msg += " (" + aadsts + ")"
	}
	return ErrorCopy{Title: "Microsoft did not connect the account", Message: msg + ". Nothing was saved."}
}

func orDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

// Help implements Adapter.
func (m *MicrosoftAdapter) Help() string {
	return "Microsoft asks for permission to read and manage mail, and to send mail. moose can only read for now. Work and school accounts may need an administrator to approve moose first; personal Outlook.com accounts approve it themselves."
}

// idClaims are the id_token claims moose reads.
type idClaims struct {
	Tid string          `json:"tid"`
	Oid string          `json:"oid"`
	Aud json.RawMessage `json:"aud"`
}

// parseIDToken reads the claims of the id_token the token endpoint returned.
// The token arrived straight from Entra over TLS in the code exchange, which
// OpenID Connect Core 3.1.3.7 accepts in place of signature validation, so the
// signature is not checked; the audience must still be this client.
func parseIDToken(raw, clientID string) (idClaims, error) {
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return idClaims{}, errors.New("id_token is not a JWT")
	}
	b, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return idClaims{}, errors.New("id_token payload is not base64url")
	}
	var c idClaims
	if err := json.Unmarshal(b, &c); err != nil {
		return idClaims{}, errors.New("id_token payload is not JSON")
	}
	var aud string
	_ = json.Unmarshal(c.Aud, &aud)
	if clientID != "" && aud != clientID {
		return idClaims{}, errors.New("id_token audience is not this client")
	}
	if c.Tid == "" || c.Oid == "" {
		return idClaims{}, errors.New("id_token lacks tid or oid")
	}
	return c, nil
}

// Profile implements Adapter. The identity is tid.oid from the id_token, which
// is stable across renames; the address is /me mail, falling back to the
// userPrincipalName when mail is empty (it can be for a personal account).
func (m *MicrosoftAdapter) Profile(ctx context.Context, tok *oauth2.Token) (Profile, error) {
	raw, _ := tok.Extra("id_token").(string)
	claims, err := parseIDToken(raw, m.Config.ClientID)
	if err != nil {
		return Profile{}, err
	}
	g := graphapi.New(oauth2.StaticTokenSource(tok))
	if m.GraphBaseURL != "" {
		g.BaseURL = m.GraphBaseURL
	}
	var me struct {
		Mail              string `json:"mail"`
		UserPrincipalName string `json:"userPrincipalName"`
		DisplayName       string `json:"displayName"`
	}
	q := url.Values{"$select": {"mail,userPrincipalName,displayName"}}
	if err := g.GetJSON(ctx, "/me", q, nil, &me); err != nil {
		return Profile{}, fmt.Errorf("read /me: %w", err)
	}
	addr := strings.TrimSpace(me.Mail)
	if addr == "" {
		addr = strings.TrimSpace(me.UserPrincipalName)
	}
	if addr == "" {
		return Profile{}, errors.New("/me has neither mail nor userPrincipalName")
	}
	return Profile{
		Address:     addr,
		DisplayName: me.DisplayName,
		ProviderUID: strings.ToLower(claims.Tid + "." + claims.Oid),
		Caps:        graph.New(g).Capabilities(),
	}, nil
}
