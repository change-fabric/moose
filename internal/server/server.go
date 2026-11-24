// Package server composes auditing, authentication, and a connector's mounts
// into the one http.Handler both runtime shapes (moose-drive and the Lambda)
// serve. It knows nothing about any connector; each supplies its own mounts.
package server

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/change-fabric/moose/internal/audit"
	"github.com/change-fabric/moose/internal/auth"
	"github.com/change-fabric/moose/internal/store"
)

// Mount is one route a connector contributes to the server. Pattern is a
// net/http.ServeMux pattern. Every mount sits behind authentication and
// auditing; Admin additionally requires an admin principal.
type Mount struct {
	Pattern string
	Handler http.Handler
	Admin   bool
}

// Deps are everything the handler needs. Auth must have Store and Identity set.
type Deps struct {
	Store  store.Store
	Auth   *auth.Authenticator
	Logger *slog.Logger
	Now    func() time.Time
	// Mounts are the routes of the connector being served, for example
	// api.Mounts for Drive.
	Mounts []Mount
}

// New builds the full handler: audit (outermost, so 401s are recorded) then
// auth then routing.
func New(d Deps) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("POST "+auth.BootstrapPath, d.Auth.Bootstrap())
	for _, m := range d.Mounts {
		h := m.Handler
		if m.Admin {
			h = auth.RequireAdmin(h)
		}
		mux.Handle(m.Pattern, h)
	}

	// Unknown paths get the JSON error shape instead of the mux's text body.
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		audit.FromContext(r.Context()).SetDetail("not_found")
		auth.WriteError(w, http.StatusNotFound, "not_found", "no such route")
	})

	return audit.Middleware(d.Store, audit.Options{Logger: d.Logger, Now: d.Now}, d.Auth.Middleware(mux))
}
