// Command moose-approver is moose's passkey approval service. It runs as an AWS
// Lambda behind an API Gateway HTTP API when AWS_LAMBDA_RUNTIME_API is set, and
// as a plain HTTP server on MOOSE_LISTEN otherwise (local development).
//
// Subcommands:
//
//	moose-approver                serve
//	moose-approver enroll         print a single-use link that enrolls a passkey
//	moose-approver sweep          expire overdue requests and delete their previews
//	moose-approver pubkey         print the receipt public key to pin in moose-email
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-lambda-go/lambda"
	"github.com/awslabs/aws-lambda-go-api-proxy/httpadapter"

	"github.com/change-fabric/moose/internal/approver/store"
	"github.com/change-fabric/moose/internal/config"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

const (
	serverName      = "moose-approver"
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
	if err := run(log, flag.Args(), os.Stdout); err != nil {
		log.Error(serverName+" exiting", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger, args []string, out io.Writer) error {
	cfg, err := config.Load(os.Getenv)
	if err != nil {
		return err
	}
	if err := cfg.ValidateApproverServe(); err != nil {
		return err
	}
	ctx := context.Background()
	w, err := build(ctx, cfg, log, defaultAWS)
	if err != nil {
		return err
	}
	defer w.store.Close()
	if len(args) > 0 {
		return runCommand(ctx, w, args[0], out)
	}
	h := w.handler()
	if os.Getenv("AWS_LAMBDA_RUNTIME_API") != "" {
		lambda.Start(lambdaHandler(w, h, log))
		return nil
	}
	if cfg.Listen == "" {
		return errors.New("set MOOSE_LISTEN to serve outside Lambda")
	}
	return serve(log, cfg.Listen, h)
}

func runCommand(ctx context.Context, w *wiring, cmd string, out io.Writer) error {
	switch cmd {
	case "enroll":
		tok := store.NewToken()
		err := w.store.CreateEnrollToken(ctx, store.EnrollToken{
			Hash: store.Hash(tok), Owner: w.cfg.Owner, Expires: time.Now().Add(store.EnrollTTL),
		})
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "Open this link on the device that holds your passkey. It works once and expires in %d minutes.\n%s/approve/register/%s\n",
			int(store.EnrollTTL.Minutes()), w.cfg.Origin, tok)
		return nil
	case "sweep":
		res, err := w.store.Sweep(ctx, w.cfg.Owner, time.Now())
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "expired %d requests, deleted %d previews of lapsed approvals\n", res.Expired, res.PreviewsDeleted)
		return nil
	case "pubkey":
		pem, err := w.publicKey(ctx)
		if err != nil {
			return err
		}
		_, err = out.Write(pem)
		return err
	}
	return fmt.Errorf("unknown command %q (want enroll, sweep or pubkey)", cmd)
}

// lambdaHandler serves API Gateway requests and, for an EventBridge scheduled
// event, runs the expiry sweep.
func lambdaHandler(w *wiring, h http.Handler, log *slog.Logger) func(context.Context, json.RawMessage) (any, error) {
	proxy := httpadapter.NewV2(h)
	return func(ctx context.Context, raw json.RawMessage) (any, error) {
		var probe struct {
			DetailType string `json:"detail-type"`
		}
		if json.Unmarshal(raw, &probe) == nil && probe.DetailType != "" {
			res, err := w.store.Sweep(ctx, w.cfg.Owner, time.Now())
			log.Info("sweep", "expired", res.Expired, "previews_deleted", res.PreviewsDeleted, "err", err)
			return res, err
		}
		var req events.APIGatewayV2HTTPRequest
		if err := json.Unmarshal(raw, &req); err != nil {
			return nil, err
		}
		return proxy.ProxyWithContext(ctx, req)
	}
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
