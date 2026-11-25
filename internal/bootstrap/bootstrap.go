// Package bootstrap turns the process environment into the dependencies every
// server shares: config, state store, and the authenticator. Server-specific
// wiring (for Drive, internal/drive/wiring) is built on the Runtime by each cmd.
package bootstrap

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/change-fabric/moose/internal/auth"
	"github.com/change-fabric/moose/internal/config"
	"github.com/change-fabric/moose/internal/store"
	"github.com/change-fabric/moose/internal/store/dynamo"
	"github.com/change-fabric/moose/internal/store/sqlite"
)

// Runtime is the assembled dependency set.
type Runtime struct {
	Config config.Config
	Store  store.Store
	Auth   *auth.Authenticator
	Logger *slog.Logger
	// Secret resolves a secret ARN, or is nil when the entrypoint has none.
	Secret func(ctx context.Context, arn string) ([]byte, error)
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
	// Secret resolves a secret ARN to its value. The Lambda entrypoint
	// supplies a Secrets Manager reader; without it ARN-based settings fail.
	Secret func(ctx context.Context, arn string) ([]byte, error)
	// Validate checks the calling server's own config section, for example
	// config.Config.ValidateDrive. It runs before anything is opened.
	Validate func(config.Config) error
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
	if opts.Validate != nil {
		if err := opts.Validate(cfg); err != nil {
			return nil, err
		}
	}
	rt := &Runtime{Config: cfg, Logger: opts.Logger, Secret: opts.Secret}

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

	idSrc := opts.Identity
	if idSrc == nil {
		if idSrc, err = identity(ctx, cfg, opts.Secret); err != nil {
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

func identity(ctx context.Context, cfg config.Config, secret func(context.Context, string) ([]byte, error)) (auth.IdentitySource, error) {
	switch cfg.Identity.Mode {
	case config.IdentityDev:
		return auth.NewDevIdentity(cfg.Env, cfg.DevIdentity)
	case config.IdentityTSNet:
		return nil, errors.New("identity.mode tsnet needs the tsnet node from moosed")
	case config.IdentityRelayHeaders:
		if secret != nil && cfg.RelaySecretARN != "" {
			v, err := secret(ctx, cfg.RelaySecretARN)
			if err != nil {
				return nil, fmt.Errorf("relay secret: %w", err)
			}
			if len(v) == 0 {
				return nil, errors.New("relay secret is empty (put a value with aws secretsmanager put-secret-value)")
			}
			return auth.HeaderIdentity{Secret: v}, nil
		}
		return nil, errors.New("identity.mode relay-headers needs the relay secret from the Lambda entrypoint")
	}
	return nil, fmt.Errorf("identity.mode %q is not supported here", cfg.Identity.Mode)
}
