// Package server composes auditing, authentication, the REST API, and the MCP
// endpoint into the one http.Handler both runtime shapes (moosed and the
// Lambda) serve.
package server

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/change-fabric/moose/internal/api"
	"github.com/change-fabric/moose/internal/audit"
	"github.com/change-fabric/moose/internal/auth"
	"github.com/change-fabric/moose/internal/core"
	moosemcp "github.com/change-fabric/moose/internal/mcp"
	"github.com/change-fabric/moose/internal/store"
)

// MCPPath is where the MCP streamable HTTP endpoint is mounted.
const MCPPath = "/mcp"

// Deps are everything the handler needs. Auth must have Store and Identity set.
type Deps struct {
	Service *core.Service
	Store   store.Store
	Auth    *auth.Authenticator
	Version string
	Logger  *slog.Logger
	Now     func() time.Time
	// MCP overrides the /mcp handler. Nil serves the moose MCP server over
	// Service; either way it sits behind the same auth.
	MCP http.Handler
}

// New builds the full handler: audit (outermost, so 401s are recorded) then
// auth then routing.
func New(d Deps) http.Handler {
	mux := http.NewServeMux()
	api.New(api.Deps{Service: d.Service, Store: d.Store, Version: d.Version, Now: d.Now}).Register(mux)
	mux.Handle("POST "+auth.BootstrapPath, d.Auth.Bootstrap())

	mcp := d.MCP
	if mcp == nil {
		mcp = moosemcp.Handler(moosemcp.NewServer(d.Service, d.Version))
	}
	mux.Handle(MCPPath, mcp)
	mux.Handle(MCPPath+"/", mcp)

	// Unknown paths get the JSON error shape instead of the mux's text body.
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		audit.FromContext(r.Context()).SetDetail("not_found")
		auth.WriteError(w, http.StatusNotFound, "not_found", "no such route")
	})

	return audit.Middleware(d.Store, audit.Options{Logger: d.Logger, Now: d.Now}, d.Auth.Middleware(mux))
}
