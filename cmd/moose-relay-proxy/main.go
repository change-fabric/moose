// Command moose-relay-proxy runs on the AWS relay EC2 behind `tailscale serve`.
// It forwards every request to the private API Gateway through the execute-api
// VPC endpoint and adds the relay shared secret, so the Lambda trusts the
// Tailscale identity headers that serve injected.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

const secretHeader = "X-Moose-Relay-Secret"

func main() {
	listen := flag.String("listen", envOr("MOOSE_RELAY_LISTEN", "127.0.0.1:8080"), "listen address")
	upstream := flag.String("upstream", os.Getenv("MOOSE_RELAY_UPSTREAM"), "API base URL, e.g. https://<api>-<vpce>.execute-api.<region>.vpce.amazonaws.com/v0")
	hostHdr := flag.String("host", os.Getenv("MOOSE_RELAY_HOST"), "Host header to send (defaults to the upstream host)")
	secretFile := flag.String("secret-file", os.Getenv("MOOSE_RELAY_SECRET_FILE"), "file holding the relay shared secret")
	flag.Parse()

	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	if err := run(*listen, *upstream, *hostHdr, *secretFile, log); err != nil {
		log.Error("moose-relay-proxy exiting", "err", err)
		os.Exit(1)
	}
}

func run(listen, upstream, host, secretFile string, log *slog.Logger) error {
	if upstream == "" || secretFile == "" {
		return errors.New("upstream and secret-file are required")
	}
	raw, err := os.ReadFile(secretFile)
	if err != nil {
		return fmt.Errorf("read secret: %w", err)
	}
	secret := strings.TrimSpace(string(raw))
	if secret == "" {
		return errors.New("relay secret is empty")
	}
	h, err := newProxy(upstream, host, secret)
	if err != nil {
		return err
	}
	srv := &http.Server{Addr: listen, Handler: h, ReadHeaderTimeout: 10 * time.Second}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()
	log.Info("moose-relay-proxy serving", "addr", listen, "upstream", upstream)
	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// newProxy builds the reverse proxy: rewrite to upstream (keeping its stage
// path prefix), set Host, and replace any client-sent secret with ours.
func newProxy(upstream, host, secret string) (http.Handler, error) {
	u, err := url.Parse(upstream)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("bad upstream %q", upstream)
	}
	if host == "" {
		host = u.Host
	}
	return &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(u)
			pr.Out.Host = host
			pr.Out.Header.Set(secretHeader, secret)
		},
	}, nil
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
