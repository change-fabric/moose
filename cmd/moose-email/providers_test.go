package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/change-fabric/moose/internal/accounts/connect"
	"github.com/change-fabric/moose/internal/accounts/oauthwire"
	"github.com/change-fabric/moose/internal/accounts/store"
	"github.com/change-fabric/moose/internal/config"
)

func pageBody(t *testing.T, w *wiring) string {
	t.Helper()
	srv := httptest.NewServer(w.connect.Handler())
	defer srv.Close()
	res, err := http.Get(srv.URL + connect.PagePath)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	if res.StatusCode != 200 {
		t.Fatalf("page = %d %s", res.StatusCode, b)
	}
	return string(b)
}

func msConfig(t *testing.T, providers ...string) config.Config {
	cfg := devConfig(t)
	cfg.DevIdentity = "pat"
	cfg.Email.Providers = providers
	cfg.Email.Microsoft = config.EmailMicrosoft{Enabled: true, ClientID: "ms-id", ClientSecret: "ms-secret"}
	return cfg
}

// Microsoft is off unless it is listed and enabled: the default config offers
// Google only, and listing it without enabling it changes nothing.
func TestMicrosoftIsOffByDefault(t *testing.T) {
	for name, mutate := range map[string]func(*config.Config){
		"default":         func(*config.Config) {},
		"listed, not on":  func(c *config.Config) { c.Email.Providers = []string{"google", "microsoft"} },
		"on, not listed":  func(c *config.Config) { c.Email.Microsoft.Enabled = true },
		"on, not listed2": func(c *config.Config) { c.Email.Providers = []string{"google"}; c.Email.Microsoft.Enabled = true },
	} {
		cfg := devConfig(t)
		cfg.DevIdentity = "pat"
		mutate(&cfg)
		w, err := buildConnect(context.Background(), cfg, quiet, noAWS)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		body := pageBody(t, w)
		if strings.Contains(body, "Connect Microsoft account") || !strings.Contains(body, "Connect Google account") {
			t.Errorf("%s: page offers the wrong providers", name)
		}
		if _, ok := w.providers.Configs[store.ProviderMicrosoft]; ok {
			t.Errorf("%s: a Microsoft OAuth client is configured", name)
		}
		w.store.Close()
	}
}

func TestMicrosoftEnabledRegistersItsAdapterAndClient(t *testing.T) {
	cfg := msConfig(t, "google", "microsoft")
	w, err := buildConnect(context.Background(), cfg, quiet, noAWS)
	if err != nil {
		t.Fatal(err)
	}
	defer w.store.Close()
	body := pageBody(t, w)
	for _, want := range []string{"Connect Google account", "Connect Microsoft account", "/connect/microsoft/start"} {
		if !strings.Contains(body, want) {
			t.Errorf("page lacks %q", want)
		}
	}
	ms := w.providers.Configs[store.ProviderMicrosoft]
	if ms == nil || ms.ClientID != "ms-id" || ms.RedirectURL != "http://localhost:8080/connect/microsoft/callback" ||
		ms.Endpoint.TokenURL != "https://login.microsoftonline.com/common/oauth2/v2.0/token" {
		t.Fatalf("microsoft client = %+v", ms)
	}
	if w.providers.Configs[store.ProviderGoogle] == nil {
		t.Error("google client missing")
	}
}

func TestMicrosoftOnlyDeploymentNeedsNoGoogleClient(t *testing.T) {
	cfg := msConfig(t, "microsoft")
	cfg.Email.GoogleClientID, cfg.Email.GoogleClientSecret = "", ""
	w, err := buildConnect(context.Background(), cfg, quiet, noAWS)
	if err != nil {
		t.Fatal(err)
	}
	defer w.store.Close()
	body := pageBody(t, w)
	if strings.Contains(body, "Connect Google account") || !strings.Contains(body, "Connect Microsoft account") {
		t.Errorf("page offers the wrong providers")
	}
	if err := cfg.ValidateEmail(); err != nil {
		t.Errorf("config rejected: %v", err)
	}
}

// The tools see an account of a switched-off provider as provider_disabled
// while the connect page and token refresh see the real record; enabling the
// provider restores it.
func TestSwitchedOffProviderAccountsAreHiddenFromTheTools(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name string
		cfg  func(*testing.T) config.Config
		want string
	}{
		{"off", func(t *testing.T) config.Config { c := devConfig(t); c.DevIdentity = "pat"; return c }, store.StatusProviderDisabled},
		{"on", func(t *testing.T) config.Config { return msConfig(t, "google", "microsoft") }, store.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w, err := buildConnect(ctx, tc.cfg(t), quiet, noAWS)
			if err != nil {
				t.Fatal(err)
			}
			defer w.store.Close()
			now := time.Now().UTC()
			if err := w.store.CreateAccount(ctx, store.Account{Owner: "pat", ID: "acc_ms", Provider: store.ProviderMicrosoft, ProviderUID: "t.o",
				Address: "m@contoso.example", Nickname: "ms", Status: store.StatusOK, HourlyCap: 10, RefreshToken: []byte("sealed"), Created: now, Updated: now}); err != nil {
				t.Fatal(err)
			}
			got, err := w.tools.GetAccount(ctx, "pat", "acc_ms")
			if err != nil || got.Status != tc.want {
				t.Fatalf("tools see %q (%v), want %q", got.Status, err, tc.want)
			}
			raw, _ := w.store.GetAccount(ctx, "pat", "acc_ms")
			if raw.Status != store.StatusOK || string(raw.RefreshToken) != "sealed" {
				t.Fatalf("stored account changed: %+v", raw)
			}
		})
	}
}

func TestMicrosoftCredentialsFromTheSecretOrTheEnvironment(t *testing.T) {
	ctx := context.Background()
	e := config.Email{Microsoft: config.EmailMicrosoft{ClientID: "id", ClientSecret: "dev-secret"}}
	if id, sec, err := oauthwire.MicrosoftCredentials(ctx, e, nil); err != nil || id != "id" || sec != "dev-secret" {
		t.Fatalf("dev = %q %q %v", id, sec, err)
	}
	e = config.Email{Microsoft: config.EmailMicrosoft{ClientID: "id", ClientSecretRef: "arn:m"}}
	reader := func(body string) oauthwire.SecretFunc {
		return func(_ context.Context, arn string) ([]byte, error) {
			if arn != "arn:m" {
				return nil, errors.New("wrong arn")
			}
			return []byte(body), nil
		}
	}
	for name, tc := range map[string]struct{ body, want string }{
		"json": {`{"client_secret":"from-json"}`, "from-json"},
		"bare": {" from-bare\n", "from-bare"},
	} {
		if id, sec, err := oauthwire.MicrosoftCredentials(ctx, e, reader(tc.body)); err != nil || id != "id" || sec != tc.want {
			t.Errorf("%s = %q %q %v", name, id, sec, err)
		}
	}
	for name, body := range map[string]string{"empty": "", "json without secret": `{"client_id":"x"}`, "broken json": `{`} {
		if _, _, err := oauthwire.MicrosoftCredentials(ctx, e, reader(body)); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
	if _, _, err := oauthwire.MicrosoftCredentials(ctx, e, nil); err == nil {
		t.Error("no secret reader: want an error")
	}
}
