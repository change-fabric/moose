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

	"github.com/change-fabric/moose/internal/config"
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
	h := newHandler(version)
	if os.Getenv("AWS_LAMBDA_RUNTIME_API") != "" {
		lambda.Start(httpadapter.NewV2(h).ProxyWithContext)
		return nil
	}
	if cfg.Listen == "" {
		return errors.New("set MOOSE_LISTEN to serve outside Lambda")
	}
	return serve(log, cfg.Listen, h)
}

// newHandler serves /healthz and the MCP endpoint. The MCP server has no tools
// yet; they arrive with the provider-backed tool phases.
func newHandler(version string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	mux.Handle(mcpkit.Path, mcpkit.Handler(mcpkit.NewServer(serverName, version, nil)))
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
