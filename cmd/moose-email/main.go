// Command moose-email is the moose email MCP server. It runs as an AWS Lambda
// behind an API Gateway HTTP API when AWS_LAMBDA_RUNTIME_API is set, and as a
// plain HTTP server on MOOSE_LISTEN otherwise (local development).
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

	"github.com/aws/aws-lambda-go/lambda"
	"github.com/awslabs/aws-lambda-go-api-proxy/httpadapter"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/change-fabric/moose/internal/config"
	"github.com/change-fabric/moose/internal/email/connect"
	"github.com/change-fabric/moose/internal/email/oauthas"
	"github.com/change-fabric/moose/internal/email/tools"
	"github.com/change-fabric/moose/internal/mcpkit"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

const (
	serverName      = "moose-email"
	shutdownTimeout = 15 * time.Second
)

func main() {
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()
	if *showVersion {
		fmt.Println(serverName, version)
		return
	}
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	if err := run(log); err != nil {
		log.Error(serverName+" exiting", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	cfg, err := config.Load(os.Getenv)
	if err != nil {
		return err
	}
	if err := cfg.ValidateEmail(); err != nil {
		return err
	}
	w, err := buildConnect(context.Background(), cfg, log, defaultAWS)
	if err != nil {
		return err
	}
	var (
		mounts   []mount
		protect  func(http.Handler) http.Handler
		register func(*sdk.Server)
	)
	if w != nil {
		defer w.store.Close()
		mounts = append(mounts, mount{connect.Prefix, w.connect.Handler()})
		if w.oauth != nil {
			mounts = append(mounts, oauthMounts(w.oauth)...)
			protect = w.oauth.Protect
		}
		register = registerTools(w, cfg)
	}
	h := newHandlerAuth(version, protect, register, mounts...)
	if os.Getenv("AWS_LAMBDA_RUNTIME_API") != "" {
		lambda.Start(httpadapter.NewV2(h).ProxyWithContext)
		return nil
	}
	if cfg.Listen == "" {
		return errors.New("set MOOSE_LISTEN to serve outside Lambda")
	}
	return serve(log, cfg.Listen, h)
}

// registerTools adds the email tools. Without the OAuth server (local
// development) a request has no token, so the dev identity stands in as owner.
func registerTools(w *wiring, cfg config.Config) func(*sdk.Server) {
	d := tools.Deps{Store: w.store, Providers: w.providers, Attach: w.attach, CursorKey: w.cursorKey}
	if w.oauth == nil && cfg.Env == config.EnvDev {
		d.FallbackOwner = cfg.DevIdentity
	}
	return func(s *sdk.Server) { tools.Register(s, d) }
}

// oauthMounts routes the authorization server's paths to it. They match the
// API Gateway route map: /.well-known/*, /register, /authorize, /authorize/poll
// and /token, plus the dashboard login under /connect/login.
func oauthMounts(as *oauthas.Server) []mount {
	h := as.Handler()
	var ms []mount
	for _, p := range []string{"/.well-known/", oauthas.RegisterPath, oauthas.AuthorizePath, oauthas.AuthorizePath + "/", oauthas.TokenPath, oauthas.LoginPath, oauthas.LoginPollPath} {
		ms = append(ms, mount{p, h})
	}
	return ms
}

// newHandler serves /healthz, the MCP endpoint and any extra mounts (the Google
// connect page), with /mcp open. It is the local development shape; a deployment
// that runs the OAuth server goes through newHandlerAuth.
func newHandler(version string, mounts ...mount) http.Handler {
	return newHandlerAuth(version, nil, nil, mounts...)
}

// newHandlerAuth is newHandler with protect wrapped around /mcp, which is
// auth.RequireBearerToken when the OAuth server is configured. register adds the
// tools; it is nil for a bare MCP endpoint.
func newHandlerAuth(version string, protect func(http.Handler) http.Handler, register func(*sdk.Server), mounts ...mount) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	var opts *sdk.ServerOptions
	if register != nil {
		opts = tools.ServerOptions()
	}
	srv := mcpkit.NewServer(serverName, version, opts)
	if register != nil {
		register(srv)
	}
	var mcp http.Handler = mcpkit.Handler(srv)
	if protect != nil {
		mcp = protect(mcp)
	}
	mux.Handle(oauthas.MCPPath, mcp)
	for _, m := range mounts {
		mux.Handle(m.pattern, m.handler)
	}
	return mux
}

func serve(log *slog.Logger, addr string, h http.Handler) error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	srv := &http.Server{Handler: h, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 2 * time.Minute}
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	log.Info(serverName+" serving", "version", version, "addr", ln.Addr().String())
	select {
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	case <-ctx.Done():
		sctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		if err := srv.Shutdown(sctx); err != nil {
			return fmt.Errorf("shutdown: %w", err)
		}
	}
	return nil
}
