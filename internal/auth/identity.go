package auth

import (
	"context"
	"crypto/subtle"
	"errors"
	"net/http"
	"strings"

	"github.com/change-fabric/moose/internal/config"
)

// Identity is the tailnet caller. A tagged node has an empty Login and its
// tags in Tags.
type Identity struct {
	Login string   `json:"login,omitempty"`
	Tags  []string `json:"tags,omitempty"`
	Node  string   `json:"node,omitempty"`
}

// Tagged reports whether this is a service (tagged) identity.
func (id Identity) Tagged() bool { return id.Login == "" && len(id.Tags) > 0 }

// ErrNoIdentity means the caller's tailnet identity could not be established.
// The middleware answers it with 401.
var ErrNoIdentity = errors.New("auth: caller identity unavailable")

// IdentitySource resolves the tailnet identity behind a request.
type IdentitySource interface {
	Identify(r *http.Request) (Identity, error)
}

// WhoIser is the part of the tsnet LocalClient moose needs, reduced to the
// fields it reads. The server package adapts *local.Client to it, which keeps
// the tailscale module out of this package and its tests.
type WhoIser interface {
	WhoIs(ctx context.Context, remoteAddr string) (Identity, error)
}

// TSNetIdentity asks the embedded tsnet node who owns the remote address.
type TSNetIdentity struct{ Client WhoIser }

func (s TSNetIdentity) Identify(r *http.Request) (Identity, error) {
	if s.Client == nil {
		return Identity{}, ErrNoIdentity
	}
	id, err := s.Client.WhoIs(r.Context(), r.RemoteAddr)
	if err != nil {
		return Identity{}, errors.Join(ErrNoIdentity, err)
	}
	if id.Login == "" && len(id.Tags) == 0 {
		return Identity{}, ErrNoIdentity
	}
	return id, nil
}

// Header names used by the AWS relay.
const (
	HeaderRelaySecret = "X-Moose-Relay-Secret"
	HeaderTSUserLogin = "Tailscale-User-Login"
	HeaderTSUserName  = "Tailscale-User-Name"
)

// HeaderIdentity trusts the identity headers that `tailscale serve` injects on
// the relay, but only when the request also carries the relay shared secret.
// Tagged callers get no serve headers and are rejected in v0 (plan Q2).
type HeaderIdentity struct{ Secret []byte }

func (s HeaderIdentity) Identify(r *http.Request) (Identity, error) {
	got := r.Header.Get(HeaderRelaySecret)
	if len(s.Secret) == 0 || got == "" || subtle.ConstantTimeCompare([]byte(got), s.Secret) != 1 {
		return Identity{}, ErrNoIdentity
	}
	login := strings.TrimSpace(r.Header.Get(HeaderTSUserLogin))
	if login == "" {
		return Identity{}, ErrNoIdentity
	}
	return Identity{Login: login, Node: r.Header.Get(HeaderTSUserName)}, nil
}

// DevIdentity treats every caller as one fixed login. Local development only.
type DevIdentity struct{ Login string }

// NewDevIdentity refuses to build a dev identity in prod or without a login.
func NewDevIdentity(env, login string) (DevIdentity, error) {
	if env == config.EnvProd {
		return DevIdentity{}, errors.New("auth: dev identity is refused when env is prod")
	}
	if login == "" {
		return DevIdentity{}, errors.New("auth: dev identity requires MOOSE_DEV_IDENTITY")
	}
	return DevIdentity{Login: login}, nil
}

func (s DevIdentity) Identify(*http.Request) (Identity, error) {
	if s.Login == "" {
		return Identity{}, ErrNoIdentity
	}
	return Identity{Login: s.Login, Node: "dev"}, nil
}
