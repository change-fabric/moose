package main

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"

	"github.com/change-fabric/moose/internal/approver/receipt"
	"github.com/change-fabric/moose/internal/config"
	"github.com/change-fabric/moose/internal/email/connect"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))


// noRedirect reports a redirect instead of following it.
var noRedirect = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

func noAWS(context.Context) (aws.Config, error) {
	return aws.Config{}, errors.New("aws must not be loaded")
}

func devConfig(t *testing.T) config.Config {
	t.Helper()
	cfg := config.Default()
	cfg.Store.SQLitePath = filepath.Join(t.TempDir(), "email.db")
	cfg.Email = &config.Email{
		Provider: config.ProviderGmail, BaseURL: "http://localhost:8080",
		DevKey:         base64.StdEncoding.EncodeToString([]byte(strings.Repeat("k", 32))),
		GoogleClientID: "id", GoogleClientSecret: "secret",
	}
	return cfg
}

func TestConnectIsOffWithoutBaseURL(t *testing.T) {
	cfg := devConfig(t)
	cfg.Email.BaseURL = ""
	w, err := buildConnect(context.Background(), cfg, quiet, noAWS)
	if err != nil || w != nil {
		t.Fatalf("buildConnect = %v, %v; want nil, nil", w, err)
	}
	if w, err := buildConnect(context.Background(), config.Default(), quiet, noAWS); err != nil || w != nil {
		t.Fatalf("no email section: %v, %v", w, err)
	}
}

func TestLocalConnectServesBehindASession(t *testing.T) {
	cfg := devConfig(t)
	w, err := buildConnect(context.Background(), cfg, quiet, noAWS) // sqlite + dev key + env creds: no AWS
	if err != nil {
		t.Fatal(err)
	}
	defer w.store.Close()
	srv := httptest.NewServer(newHandler("t", mount{connect.Prefix, w.connect.Handler()}))
	defer srv.Close()

	res, err := noRedirect.Get(srv.URL + connect.PagePath)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusSeeOther || res.Header.Get("Location") != connect.LoginPath {
		t.Fatalf("connect page without a session = %d %q, want a redirect to sign-in", res.StatusCode, res.Header.Get("Location"))
	}
	res, err = http.Get(srv.URL + "/healthz")
	if err != nil || res.StatusCode != 200 {
		t.Fatalf("healthz = %v, %v", res, err)
	}
	res.Body.Close()
}

func TestDevIdentitySignsTheConnectPageIn(t *testing.T) {
	cfg := devConfig(t)
	cfg.DevIdentity = "pat"
	w, err := buildConnect(context.Background(), cfg, quiet, noAWS)
	if err != nil {
		t.Fatal(err)
	}
	defer w.store.Close()
	srv := httptest.NewServer(w.connect.Handler())
	defer srv.Close()
	res, err := http.Get(srv.URL + connect.PagePath)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	if res.StatusCode != 200 || !strings.Contains(string(body), "Connected accounts") || !strings.Contains(string(body), "read-only for now") {
		t.Fatalf("page = %d %q", res.StatusCode, body)
	}

	// The dev identity is honoured only in dev.
	cfg.Env = config.EnvProd
	w2, err := buildConnect(context.Background(), cfg, quiet, noAWS)
	if err != nil {
		t.Fatal(err)
	}
	defer w2.store.Close()
	srv2 := httptest.NewServer(w2.connect.Handler())
	defer srv2.Close()
	res2, _ := noRedirect.Get(srv2.URL + connect.PagePath)
	res2.Body.Close()
	if res2.StatusCode != http.StatusSeeOther {
		t.Fatalf("prod page with a dev identity = %d, want a redirect to sign-in", res2.StatusCode)
	}
}

func TestAWSIsLoadedOnlyWhenConfigNeedsIt(t *testing.T) {
	cfg := devConfig(t)
	cfg.Email.DevKey, cfg.Email.KMSKeyID = "", "alias/moose"
	loaded := 0
	w, err := buildConnect(context.Background(), cfg, quiet, func(context.Context) (aws.Config, error) {
		loaded++
		return aws.Config{Region: "us-east-1"}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	defer w.store.Close()
	if loaded != 1 {
		t.Fatalf("aws config loaded %d times, want once for a KMS key", loaded)
	}

	cfg = devConfig(t)
	cfg.Store.Kind, cfg.Store.DynamoTable = config.StoreDynamo, "moose-email"
	if w, err := buildConnect(context.Background(), cfg, quiet, noAWS); err == nil || !strings.Contains(err.Error(), "aws config") {
		if w != nil {
			w.store.Close()
		}
		t.Fatalf("dynamo without aws: err = %v, want the aws load error", err)
	}
}

func TestGoogleCredentials(t *testing.T) {
	ctx := context.Background()
	const arn = "arn:aws:secretsmanager:us-east-1:1:secret:g"
	good := func(context.Context, string) ([]byte, error) {
		return []byte(`{"client_id":"i","client_secret":"s"}`), nil
	}
	id, sec, err := googleCredentials(ctx, config.Email{GoogleSecretARN: arn}, good)
	if err != nil || id != "i" || sec != "s" {
		t.Fatalf("from secret = %q %q %v", id, sec, err)
	}
	id, sec, err = googleCredentials(ctx, config.Email{GoogleClientID: "di", GoogleClientSecret: "ds"}, nil)
	if err != nil || id != "di" || sec != "ds" {
		t.Fatalf("from env = %q %q %v", id, sec, err)
	}
	for name, fn := range map[string]secretFunc{
		"read error":   func(context.Context, string) ([]byte, error) { return nil, errors.New("denied") },
		"not json":     func(context.Context, string) ([]byte, error) { return []byte("plain"), nil },
		"missing keys": func(context.Context, string) ([]byte, error) { return []byte(`{"client_id":"i"}`), nil },
	} {
		if _, _, err := googleCredentials(ctx, config.Email{GoogleSecretARN: arn}, fn); err == nil {
			t.Errorf("%s: no error", name)
		} else if strings.Contains(err.Error(), "plain") {
			t.Errorf("%s: error echoes the secret: %v", name, err)
		}
	}
	if _, _, err := googleCredentials(ctx, config.Email{GoogleSecretARN: arn}, nil); err == nil {
		t.Error("ARN without a reader accepted")
	}
}

func oauthConfig(t *testing.T) config.Config {
	t.Helper()
	signer, err := receipt.GenerateLocalSigner()
	if err != nil {
		t.Fatal(err)
	}
	pub, err := signer.PublicKeyPEM()
	if err != nil {
		t.Fatal(err)
	}
	cfg := devConfig(t)
	cfg.Email.Owner, cfg.Email.RPID, cfg.Email.ReceiptPublicKey = "pat", "localhost", string(pub)
	cfg.Email.ApproverSecret = strings.Repeat("s", 32)
	return cfg
}

func TestOAuthProtectsMCPAndServesTheAuthorizationServer(t *testing.T) {
	cfg := oauthConfig(t)
	w, err := buildConnect(context.Background(), cfg, quiet, noAWS)
	if err != nil || w == nil || w.oauth == nil {
		t.Fatalf("buildConnect = %v, %v", w, err)
	}
	defer w.store.Close()
	mounts := append([]mount{{connect.Prefix, w.connect.Handler()}}, oauthMounts(w.oauth)...)
	srv := httptest.NewServer(newHandlerAuth("t", w.oauth.Protect, registerTools(w, cfg), mounts...))
	defer srv.Close()

	res, err := http.Post(srv.URL+"/mcp", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusUnauthorized || !strings.Contains(res.Header.Get("WWW-Authenticate"), "/.well-known/oauth-protected-resource/mcp") {
		t.Fatalf("/mcp without a token = %d %q, want 401 pointing at the resource metadata", res.StatusCode, res.Header.Get("WWW-Authenticate"))
	}
	for _, p := range []string{"/.well-known/oauth-authorization-server", "/.well-known/oauth-protected-resource/mcp", "/.well-known/oauth-protected-resource"} {
		res, err = http.Get(srv.URL + p)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != 200 {
			t.Errorf("%s = %d, want 200", p, res.StatusCode)
		}
	}
	// register, authorize, poll and token are all routed to the server.
	res, err = http.Post(srv.URL+"/register", "application/json", strings.NewReader(`{"redirect_uris":["https://claude.ai/api/mcp/auth_callback"]}`))
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusCreated {
		t.Errorf("/register = %d", res.StatusCode)
	}
	for path, want := range map[string]int{"/authorize": 400, "/authorize/poll": 404, "/token": 405, connect.PagePath: 303} {
		res, err = noRedirect.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != want {
			t.Errorf("GET %s = %d, want %d", path, res.StatusCode, want)
		}
	}
}

func TestOAuthNeedsALongServiceSecret(t *testing.T) {
	cfg := oauthConfig(t)
	cfg.Email.ApproverSecret = "short"
	if _, err := buildConnect(context.Background(), cfg, quiet, noAWS); err == nil || !strings.Contains(err.Error(), "32 bytes") {
		t.Fatalf("err = %v, want a refusal of a short service secret", err)
	}
}

func TestMCPStaysOpenLocallyWithoutOAuth(t *testing.T) {
	w, err := buildConnect(context.Background(), devConfig(t), quiet, noAWS)
	if err != nil || w.oauth != nil {
		t.Fatalf("oauth must be off when email.owner and friends are unset: %v %v", w, err)
	}
	w.store.Close()
}
