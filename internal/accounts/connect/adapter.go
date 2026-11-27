package connect

import (
	"context"
	"fmt"
	"strings"

	"golang.org/x/oauth2"
)

// ProviderGoogle is the canonical name of the Google adapter.
const ProviderGoogle = "google"

// Adapter is one OAuth provider's side of the connect flow: the client it
// authorizes with, how to read the mailbox profile from a fresh token, and
// which requested permissions the user left out.
type Adapter interface {
	Name() string
	OAuthConfig(services []string) *oauth2.Config
	Profile(ctx context.Context, tok *oauth2.Token) (Profile, error)
	// MissingScopes returns the requested scopes absent from granted. An empty
	// granted list means the provider did not report scopes.
	MissingScopes(granted []string, services []string) []string
}

// Registry holds adapters by name.
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

// OAuthConfig implements Adapter. Google serves mail only, so services is ignored.
func (g *GoogleAdapter) OAuthConfig([]string) *oauth2.Config { return g.Config }

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
