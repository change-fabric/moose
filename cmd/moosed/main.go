// Command moosed is the long-lived moose server. In dev it serves plain HTTP
// on MOOSE_LISTEN; otherwise it joins the tailnet with tsnet and serves HTTPS
// on :443, taking caller identity from tsnet WhoIs.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"tailscale.com/client/local"
	"tailscale.com/tsnet"

	"github.com/change-fabric/moose/internal/auth"
	"github.com/change-fabric/moose/internal/bootstrap"
	"github.com/change-fabric/moose/internal/config"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

const shutdownTimeout = 15 * time.Second

func main() {
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()
	if *showVersion {
		fmt.Println("moosed", version)
		return
	}
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	if err := run(log); err != nil {
		log.Error("moosed exiting", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	cfg, err := config.Load(os.Getenv)
	if err != nil {
		return err
	}

	var (
		ln    net.Listener
		idSrc auth.IdentitySource
		ts    *tsnet.Server
	)
	if cfg.Listen != "" {
		// Dev listener: plain HTTP, identity from config (dev mode).
		// There is no tsnet node to ask WhoIs here, so the default tsnet
		// mode falls back to MOOSE_DEV_IDENTITY (config refuses both in prod).
		if cfg.Identity.Mode == config.IdentityTSNet {
			dev, err := auth.NewDevIdentity(cfg.Env, cfg.DevIdentity)
			if err != nil {
				return fmt.Errorf("MOOSE_LISTEN without a tailnet: %w", err)
			}
			idSrc = dev
		}
		if ln, err = net.Listen("tcp", cfg.Listen); err != nil {
			return err
		}
	} else {
		if cfg.Identity.Mode != config.IdentityTSNet {
			return fmt.Errorf("without MOOSE_LISTEN moosed serves on the tailnet and needs identity.mode tsnet, got %q", cfg.Identity.Mode)
		}
		ts = &tsnet.Server{
			Hostname: cfg.Hostname,
			Dir:      cfg.TSNetStateDir,
			AuthKey:  cfg.TSAuthKey,
			Logf:     func(string, ...any) {},
		}
		defer ts.Close()
		if _, err := ts.Up(ctx); err != nil {
			return fmt.Errorf("tsnet up (if the auth key expired or was reused, issue a new one): %w", err)
		}
		if ln, err = ts.ListenTLS("tcp", ":443"); err != nil {
			return fmt.Errorf("tsnet listen: %w", err)
		}
		lc, err := ts.LocalClient()
		if err != nil {
			return fmt.Errorf("tsnet local client: %w", err)
		}
		idSrc = auth.TSNetIdentity{Client: whoIs{lc}}
	}
	defer ln.Close()

	rt, err := bootstrap.FromEnv(ctx, bootstrap.Options{Logger: log, Identity: idSrc})
	if err != nil {
		return err
	}
	defer rt.Store.Close()

	srv := &http.Server{
		Handler:           rt.Handler(version),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	log.Info("moosed serving", "version", version, "addr", ln.Addr().String(), "env", cfg.Env, "identity", cfg.Identity.Mode)

	select {
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	case <-ctx.Done():
		log.Info("moosed shutting down")
		sctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		if err := srv.Shutdown(sctx); err != nil {
			return fmt.Errorf("shutdown: %w", err)
		}
	}
	rt.Auth.Wait()
	log.Info("moosed stopped")
	return nil
}

// whoIs adapts the tsnet LocalClient to auth.WhoIser.
type whoIs struct{ lc *local.Client }

func (w whoIs) WhoIs(ctx context.Context, addr string) (auth.Identity, error) {
	res, err := w.lc.WhoIs(ctx, addr)
	if err != nil {
		return auth.Identity{}, err
	}
	if res.Node == nil {
		return auth.Identity{}, auth.ErrNoIdentity
	}
	id := auth.Identity{Node: res.Node.ComputedName}
	if res.Node.IsTagged() {
		id.Tags = append([]string(nil), res.Node.Tags...)
		return id, nil
	}
	if res.UserProfile == nil {
		return auth.Identity{}, auth.ErrNoIdentity
	}
	id.Login = res.UserProfile.LoginName
	return id, nil
}
