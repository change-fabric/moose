package connect

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"golang.org/x/oauth2"
)

// Provider names. They are the {provider} segment of the connect routes and
// match store.ProviderGoogle and store.ProviderMicrosoft.
const (
	ProviderGoogle    = "google"
	ProviderMicrosoft = "microsoft"
)

// ErrorCopy is what the callback page says when a provider answers with an
// error instead of a code. Href and Label add the one action the page offers;
// both empty means the default "Back to accounts".
type ErrorCopy struct {
	Title, Message string
	Href, Label    string
}

// Adapter is one OAuth provider's side of the connect flow: the client it
// authorizes with, how to read the mailbox profile from a fresh token, which
// requested permissions the user left out, and the provider's own wording.
type Adapter interface {
	Name() string
	// Label is the provider's display name on the page and its badge.
	Label() string
	OAuthConfig(services []string) *oauth2.Config
	// AuthOptions are the provider-specific authorization request parameters.
	// reconnectAddress is the mailbox being reconnected, empty for a new one.
	AuthOptions(reconnectAddress string) []oauth2.AuthCodeOption
	Profile(ctx context.Context, tok *oauth2.Token) (Profile, error)
	// MissingScopes returns the requested scopes absent from granted. An empty
	// granted list means the provider did not report scopes.
	MissingScopes(granted []string, services []string) []string
	// Grant returns the scopes to record for a connect: every non-identity scope
	// granted, and the subset that is the mail service's. An empty granted list
	// means the provider did not report scopes and the request is taken as given.
	Grant(granted []string) (all, mail []string)
	// CallbackError words a provider error answer (the error and
	// error_description query parameters).
	CallbackError(code, description string) ErrorCopy
	// Help is the page's "what happens when I connect" paragraph (HTML-safe text).
	Help() string
}

// Registry holds adapters by name, in registration order.
type Registry struct {
	byName map[string]Adapter
}

// NewRegistry returns a registry holding the given adapters.
func NewRegistry(adapters ...Adapter) *Registry {
	r := &Registry{byName: map[string]Adapter{}}
	for _, a := range adapters {
		r.Register(a)
	}
	return r
}

// Register adds or replaces an adapter under its name.
func (r *Registry) Register(a Adapter) { r.byName[a.Name()] = a }

// Get returns the adapter registered under name.
func (r *Registry) Get(name string) (Adapter, error) {
	a, ok := r.byName[name]
	if !ok {
		return nil, fmt.Errorf("connect: no adapter for provider %q", name)
	}
	return a, nil
}

// Names lists the registered provider names, sorted.
func (r *Registry) Names() []string {
	out := make([]string, 0, len(r.byName))
	for n := range r.byName {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// GoogleAdapter connects Gmail mailboxes.
type GoogleAdapter struct {
	Config    *oauth2.Config
	ProfileFn ProfileFunc
}

// NewGoogleAdapter wraps a Google web client (see GoogleConfig) and a profile lookup.
func NewGoogleAdapter(cfg *oauth2.Config, profile ProfileFunc) *GoogleAdapter {
	return &GoogleAdapter{Config: cfg, ProfileFn: profile}
}

// Name implements Adapter.
func (g *GoogleAdapter) Name() string { return ProviderGoogle }

// Label implements Adapter.
func (g *GoogleAdapter) Label() string { return "Google" }

// OAuthConfig implements Adapter. Google serves mail only, so services is ignored.
func (g *GoogleAdapter) OAuthConfig([]string) *oauth2.Config { return g.Config }

// AuthOptions implements Adapter: offline access with a forced consent screen,
// so a refresh token always comes back.
func (g *GoogleAdapter) AuthOptions(reconnectAddress string) []oauth2.AuthCodeOption {
	opts := []oauth2.AuthCodeOption{
		oauth2.AccessTypeOffline,
		oauth2.SetAuthURLParam("prompt", "consent"),
	}
	if reconnectAddress != "" {
		opts = append(opts, oauth2.SetAuthURLParam("login_hint", reconnectAddress))
	}
	return opts
}

// Profile implements Adapter.
func (g *GoogleAdapter) Profile(ctx context.Context, tok *oauth2.Token) (Profile, error) {
	return g.ProfileFn(ctx, g.Config.TokenSource(ctx, tok))
}

// MissingScopes implements Adapter. Google omits scope only when it equals
// what was requested, so an empty granted list is complete.
func (g *GoogleAdapter) MissingScopes(granted []string, _ []string) []string {
	if len(granted) == 0 {
		return nil
	}
	have := map[string]bool{}
	for _, sc := range granted {
		have[sc] = true
	}
	var missing []string
	for _, want := range []string{ScopeReadonly, ScopeCompose} {
		if !have[want] {
			missing = append(missing, strings.TrimPrefix(want, "https://www.googleapis.com/auth/"))
		}
	}
	return missing
}

// Grant implements Adapter: the two Gmail scopes, which MissingScopes has
// already required.
func (g *GoogleAdapter) Grant([]string) (all, mail []string) {
	return []string{ScopeReadonly, ScopeCompose}, []string{ScopeReadonly, ScopeCompose}
}

// CallbackError implements Adapter.
func (g *GoogleAdapter) CallbackError(code, _ string) ErrorCopy {
	return ErrorCopy{Title: "Google did not connect the account", Message: "Google answered: " + code + ". Nothing was saved."}
}

// Help implements Adapter.
func (g *GoogleAdapter) Help() string {
	return "Google asks for permission to read mail and manage drafts. Because this is your own private app, it first shows an unverified-app warning: click Advanced, then Go to moose.changefabric.org (unsafe), then allow both permissions."
}
