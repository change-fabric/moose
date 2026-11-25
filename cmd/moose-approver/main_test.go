package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/go-webauthn/webauthn/protocol"

	"github.com/change-fabric/moose/internal/approver/api"
	"github.com/change-fabric/moose/internal/approver/receipt"
	"github.com/change-fabric/moose/internal/approver/store"
	"github.com/change-fabric/moose/internal/approver/webauthn/webauthntest"
	"github.com/change-fabric/moose/internal/config"
)

var (
	devKey     = base64.StdEncoding.EncodeToString([]byte(strings.Repeat("k", 32)))
	hmacSecret = strings.Repeat("h", 40)
)

func noAWS(context.Context) (aws.Config, error) {
	return aws.Config{}, errors.New("AWS must not be loaded in dev")
}

func devConfig(t *testing.T) config.Config {
	t.Helper()
	return config.Config{
		Env: config.EnvDev, Store: config.Store{Kind: config.StoreSQLite, SQLitePath: filepath.Join(t.TempDir(), "approver.db")},
		Approver: &config.Approver{
			RPID: "moose.example.org", Origin: "https://moose.example.org", Owner: "pat",
			DevKey: devKey, HMACSecret: hmacSecret,
		},
	}
}

func newWiring(t *testing.T) *wiring {
	t.Helper()
	cfg := devConfig(t)
	if err := cfg.ValidateApproverServe(); err != nil {
		t.Fatal(err)
	}
	w, err := build(context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), noAWS)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = w.store.Close() })
	return w
}

func TestHealthzAndRoutes(t *testing.T) {
	w := newWiring(t)
	srv := httptest.NewServer(w.handler())
	defer srv.Close()
	res, err := http.Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != 200 || string(b) != `{"ok":true}` {
		t.Fatalf("healthz: %d %q", res.StatusCode, b)
	}
	// The service API is behind the MAC, the pages are not.
	for path, want := range map[string]int{
		"/approve/api/requests/apr_x": http.StatusUnauthorized,
		"/approve/r/apr_x":            http.StatusNotFound,
		"/approve/register/nope":      http.StatusNotFound,
		"/approve/queue":              http.StatusUnauthorized,
		"/approve/elsewhere":          http.StatusNotFound,
	} {
		res, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != want {
			t.Errorf("GET %s = %d, want %d", path, res.StatusCode, want)
		}
	}
}

func TestEnrollCommandPrintsASingleUseLinkThatRegistersAPasskey(t *testing.T) {
	w := newWiring(t)
	var out bytes.Buffer
	if err := runCommand(context.Background(), w, "enroll", &out); err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`https://moose\.example\.org(/approve/register/([A-Za-z0-9_-]{43}))`).FindStringSubmatch(out.String())
	if m == nil {
		t.Fatalf("no enrollment link in %q", out.String())
	}
	path, tok := m[1], m[2]
	// Only the hash of the token is stored.
	if _, err := w.store.PeekEnrollToken(context.Background(), store.Hash(tok), time.Now()); err != nil {
		t.Fatalf("the token is not stored by its hash: %v", err)
	}

	h := w.handler()
	do := func(method, p string, body []byte) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "https://moose.example.org"+p, bytes.NewReader(body))
		r.Header.Set("Origin", "https://moose.example.org")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		return rec
	}
	if rec := do(http.MethodGet, path, nil); rec.Code != 200 || !strings.Contains(rec.Body.String(), "Create passkey") {
		t.Fatalf("enroll page: %d", rec.Code)
	}
	begin := do(http.MethodPost, path+"/begin", nil)
	var creation protocol.CredentialCreation
	if err := json.Unmarshal(begin.Body.Bytes(), &creation); err != nil || begin.Code != 200 {
		t.Fatalf("begin: %d %s", begin.Code, begin.Body)
	}
	a := webauthntest.New("moose.example.org", "https://moose.example.org")
	if rec := do(http.MethodPost, path+"/finish", a.Register(&creation)); rec.Code != 200 {
		t.Fatalf("finish: %d %s", rec.Code, rec.Body)
	}
	if ok, _ := w.wa.HasCredentials(context.Background(), "pat"); !ok {
		t.Fatal("no passkey stored")
	}
	if rec := do(http.MethodGet, path, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("the link worked twice: %d", rec.Code)
	}
}

func TestPubkeyCommandPrintsTheKeyReceiptsVerifyWith(t *testing.T) {
	w := newWiring(t)
	var out bytes.Buffer
	if err := runCommand(context.Background(), w, "pubkey", &out); err != nil {
		t.Fatal(err)
	}
	rc, err := receipt.Sign(context.Background(), w.signer, receipt.Body{
		RequestID: "r", Kind: "k", PayloadSHA256: "p", ApprovedAt: time.Now(), ExpiresAt: time.Now().Add(time.Minute), RPID: "moose.example.org",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := receipt.Verify(out.Bytes(), rc, receipt.Want{RequestID: "r", Kind: "k", PayloadSHA256: "p", RPID: "moose.example.org", Now: time.Now()}); err != nil {
		t.Fatalf("the printed key does not verify the service's receipts: %v", err)
	}
}

func TestSweepCommandExpiresOverdueRequests(t *testing.T) {
	w := newWiring(t)
	ctx := context.Background()
	past := time.Now().Add(-time.Hour)
	_, _, err := w.store.CreateRequest(ctx, store.Request{
		ID: "apr_old", Owner: "pat", Kind: api.KindAccountSendEnable, IdempotencyKey: "k", PayloadSHA256: "s", Payload: json.RawMessage(`{}`),
		Created: past, Expires: past.Add(store.RequestLifetime),
	}, []byte(`{"x":1}`))
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := runCommand(ctx, w, "sweep", &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "expired 1 requests") {
		t.Fatalf("sweep output: %q", out.String())
	}
	if ok, _ := w.store.HasPreview(ctx, "apr_old"); ok {
		t.Fatal("the sweep left the preview")
	}
}

func TestUnknownCommand(t *testing.T) {
	w := newWiring(t)
	if err := runCommand(context.Background(), w, "frobnicate", io.Discard); err == nil {
		t.Fatal("an unknown command succeeded")
	}
}

func TestLambdaHandlerRunsTheSweepOnScheduledEvents(t *testing.T) {
	w := newWiring(t)
	ctx := context.Background()
	past := time.Now().Add(-time.Hour)
	_, _, _ = w.store.CreateRequest(ctx, store.Request{
		ID: "apr_old", Owner: "pat", Kind: api.KindAccountSendEnable, IdempotencyKey: "k", PayloadSHA256: "s", Payload: json.RawMessage(`{}`),
		Created: past, Expires: past.Add(store.RequestLifetime),
	}, []byte(`{}`))
	h := lambdaHandler(w, w.handler(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if _, err := h(ctx, json.RawMessage(`{"detail-type":"Scheduled Event","source":"aws.events"}`)); err != nil {
		t.Fatal(err)
	}
	if r, _ := w.store.GetRequest(ctx, "apr_old"); r.Status != store.StatusExpired {
		t.Fatalf("status = %s, want expired", r.Status)
	}
	// An API Gateway event is proxied to the HTTP handler.
	out, err := h(ctx, json.RawMessage(`{"version":"2.0","rawPath":"/healthz","requestContext":{"http":{"method":"GET","path":"/healthz"}},"headers":{}}`))
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := json.Marshal(out); !strings.Contains(string(b), `\"ok\":true`) {
		t.Fatalf("proxied response: %s", b)
	}
}

func TestBuildFailsWithoutLoadingAWSInDev(t *testing.T) {
	cfg := devConfig(t)
	cfg.Approver.HMACSecret = "short"
	if _, err := build(context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), noAWS); err == nil {
		t.Fatal("a short API secret was accepted")
	}
	cfg = devConfig(t)
	cfg.Approver.DevKey = ""
	cfg.Approver.KMSKeyID = "alias/seal"
	if _, err := build(context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), noAWS); err == nil || !strings.Contains(err.Error(), "aws config") {
		t.Fatalf("a KMS key must load AWS config: %v", err)
	}
}
