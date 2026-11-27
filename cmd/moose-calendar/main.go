// Command moose-calendar is the moose calendar MCP server. It runs as an AWS
// Lambda behind the same API Gateway as moose-email when AWS_LAMBDA_RUNTIME_API
// is set, and as a plain HTTP server on MOOSE_LISTEN otherwise (local
// development). It serves /calendar/mcp for the accounts moose-email connected;
// the OAuth authorization server that issues its tokens stays in moose-email.
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

	"github.com/change-fabric/moose/internal/calendar/tools"
	"github.com/change-fabric/moose/internal/config"
	"github.com/change-fabric/moose/internal/mcpkit"
	"github.com/change-fabric/moose/internal/oauthas"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

const (
	serverName      = "moose-calendar"
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
	if err := cfg.ValidateCalendar(); err != nil {
		return err
	}
	w, err := buildWiring(context.Background(), cfg, log, defaultAWS)
	if err != nil {
		return err
	}
	defer w.store.Close()
	h := newHandler(version, w.protect, registerTools(w, cfg))
	if os.Getenv("AWS_LAMBDA_RUNTIME_API") != "" {
		lambda.Start(httpadapter.NewV2(h).ProxyWithContext)
		return nil
	}
	if cfg.Listen == "" {
		return errors.New("set MOOSE_LISTEN to serve outside Lambda")
	}
	return serve(log, cfg.Listen, h)
}

// registerTools adds the calendar tools. Without a public origin (local
// development) a request has no token, so the dev identity stands in as owner.
func registerTools(w *wiring, cfg config.Config) func(*sdk.Server) {
	d := tools.Deps{Store: w.tools, CursorKey: w.cursorKey}
	if w.providers != nil {
		d.Providers = w.providers
		d.Invites = w.invites
	}
	if w.protect == nil && cfg.Env == config.EnvDev {
		d.FallbackOwner = cfg.DevIdentity
	}
	return func(s *sdk.Server) { tools.Register(s, d) }
}

// newHandler serves /healthz and the MCP endpoint at /calendar/mcp, with protect
// wrapped around the endpoint (nil leaves it open, which only dev allows).
// register adds the tools; nil gives a bare MCP endpoint.
func newHandler(version string, protect func(http.Handler) http.Handler, register func(*sdk.Server)) http.Handler {
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
	mux.Handle(oauthas.CalendarMCPPath, mcp)
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
