// Package bootstrap turns the process environment into ready-to-serve
// dependencies: config, state store, connectors, core service, and the
// authenticator. moosed and the Lambda entrypoint share it.
package bootstrap

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/change-fabric/moose/internal/auth"
	"github.com/change-fabric/moose/internal/config"
	"github.com/change-fabric/moose/internal/connectors/gdrive"
	"github.com/change-fabric/moose/internal/core"
	"github.com/change-fabric/moose/internal/server"
	"github.com/change-fabric/moose/internal/store"
	"github.com/change-fabric/moose/internal/store/dynamo"
	"github.com/change-fabric/moose/internal/store/sqlite"
)

// Runtime is the assembled dependency set.
type Runtime struct {
	Config  config.Config
	Store   store.Store
	Service *core.Service
	Auth    *auth.Authenticator
	Logger  *slog.Logger
}

// Options tune FromEnv.
type Options struct {
	Getenv func(string) string
	Logger *slog.Logger
	// Identity overrides the configured identity source. moosed passes the
	// tsnet source here because only it owns the tsnet node.
	Identity auth.IdentitySource
	// DynamoClient is used when store.kind is dynamo. Nil is an error until
	// the AWS entrypoint supplies one.
	DynamoClient dynamo.API
}

// FromEnv loads config and builds the runtime. The caller closes Runtime.Store.
func FromEnv(ctx context.Context, opts Options) (*Runtime, error) {
	if opts.Getenv == nil {
		opts.Getenv = os.Getenv
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	cfg, err := config.Load(opts.Getenv)
	if err != nil {
		return nil, err
	}
	rt := &Runtime{Config: cfg, Logger: opts.Logger}

	if rt.Store, err = openStore(ctx, cfg, opts.DynamoClient); err != nil {
		return nil, err
	}
	fail := func(err error) (*Runtime, error) {
		_ = rt.Store.Close()
		return nil, err
	}
	if err := auth.EnsureBootstrapAdmin(ctx, rt.Store, cfg.BootstrapAdmin, time.Now()); err != nil {
		return fail(fmt.Errorf("bootstrap admin: %w", err))
	}

	conns, maxBytes, err := connectors(ctx, cfg, opts.Logger)
	if err != nil {
		return fail(err)
	}
	if rt.Service, err = core.NewService(conns, core.Options{RedactPatterns: cfg.Redaction.Patterns, MaxBytes: maxBytes}); err != nil {
		return fail(err)
	}

	idSrc := opts.Identity
	if idSrc == nil {
		if idSrc, err = identity(cfg); err != nil {
			return fail(err)
		}
	}
	rt.Auth = &auth.Authenticator{
		Store:         rt.Store,
		Identity:      idSrc,
		AllowedLogins: cfg.Identity.AllowedLogins,
		AllowedTags:   cfg.Identity.AllowedTags,
		Logger:        opts.Logger,
	}
	return rt, nil
}

// Handler builds the composed http.Handler for this runtime.
func (rt *Runtime) Handler(version string) http.Handler {
	return server.New(server.Deps{Service: rt.Service, Store: rt.Store, Auth: rt.Auth, Version: version, Logger: rt.Logger})
}

func openStore(ctx context.Context, cfg config.Config, ddb dynamo.API) (store.Store, error) {
	switch cfg.Store.Kind {
	case config.StoreSQLite:
		if dir := filepath.Dir(cfg.Store.SQLitePath); dir != "" {
			if err := os.MkdirAll(dir, 0o700); err != nil {
				return nil, fmt.Errorf("store: create %s: %w", dir, err)
			}
		}
		return sqlite.Open(ctx, cfg.Store.SQLitePath)
	case config.StoreDynamo:
		if ddb == nil {
			return nil, errors.New("store: dynamo requires an AWS client (use the Lambda entrypoint)")
		}
		return dynamo.New(ddb, cfg.Store.DynamoTable), nil
	}
	return nil, fmt.Errorf("store: unknown kind %q", cfg.Store.Kind)
}

func connectors(ctx context.Context, cfg config.Config, log *slog.Logger) ([]core.Connector, int64, error) {
	gc, ok := cfg.Connectors[gdrive.ID]
	if !ok || !gc.Enabled {
		return nil, 0, nil
	}
	var key []byte
	var keyErr error
	switch {
	case cfg.GDriveKeyFile != "":
		key, keyErr = os.ReadFile(cfg.GDriveKeyFile)
	case cfg.GDriveKeySecretARN != "":
		keyErr = errors.New("MOOSE_GDRIVE_KEY_SECRET_ARN is read by the Lambda entrypoint only")
	default:
		keyErr = errors.New("no service account key configured (set MOOSE_GDRIVE_KEY_FILE)")
	}
	if keyErr == nil {
		c, err := gdrive.New(ctx, key)
		if err == nil {
			return []core.Connector{c}, gc.MaxDocumentBytes, nil
		}
		keyErr = err
	}
	// Plan failure mode: start anyway, report gdrive unhealthy, answer 503.
	log.Warn("gdrive connector unavailable", "err", keyErr)
	return []core.Connector{Unavailable{IDValue: gdrive.ID, NameValue: "Google Drive", Err: keyErr}}, gc.MaxDocumentBytes, nil
}

func identity(cfg config.Config) (auth.IdentitySource, error) {
	switch cfg.Identity.Mode {
	case config.IdentityDev:
		return auth.NewDevIdentity(cfg.Env, cfg.DevIdentity)
	case config.IdentityTSNet:
		return nil, errors.New("identity.mode tsnet needs the tsnet node from moosed")
	case config.IdentityRelayHeaders:
		return nil, errors.New("identity.mode relay-headers needs the relay secret from the Lambda entrypoint")
	}
	return nil, fmt.Errorf("identity.mode %q is not supported here", cfg.Identity.Mode)
}

// Unavailable stands in for a connector whose credentials could not be
// loaded: health reports the reason and every data call is a 503.
type Unavailable struct {
	IDValue, NameValue string
	Err                error
}

func (u Unavailable) ID() string   { return u.IDValue }
func (u Unavailable) Name() string { return u.NameValue }
func (u Unavailable) Kind() string { return u.IDValue }
func (u Unavailable) Detail(context.Context) string {
	return "unavailable: " + u.Err.Error()
}
func (u Unavailable) Health(context.Context) error { return core.ErrUpstreamUnavailable }
func (u Unavailable) List(context.Context, string, int) (core.SearchResult, error) {
	return core.SearchResult{}, core.ErrUpstreamUnavailable
}
func (u Unavailable) Search(context.Context, core.SearchQuery) (core.SearchResult, error) {
	return core.SearchResult{}, core.ErrUpstreamUnavailable
}
func (u Unavailable) Get(context.Context, string, int64) (core.Document, error) {
	return core.Document{}, core.ErrUpstreamUnavailable
}
