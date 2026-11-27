package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/change-fabric/moose/internal/accounts/store"
	"github.com/change-fabric/moose/internal/calendar/tools"
	"github.com/change-fabric/moose/internal/config"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

func noAWS(context.Context) (aws.Config, error) {
	return aws.Config{}, errors.New("aws must not be loaded")
}

func baseConfig(t *testing.T) config.Config {
	t.Helper()
	cfg := config.Default()
	cfg.Store.SQLitePath = filepath.Join(t.TempDir(), "calendar.db")
	cfg.Email = &config.Email{Provider: config.ProviderGmail, Calendar: config.EmailCalendar{Enabled: true}}
	return cfg
}

func TestHealthz(t *testing.T) {
	srv := httptest.NewServer(newHandler("t", nil, nil))
	defer srv.Close()
	res, err := http.Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	if res.StatusCode != 200 || string(b) != `{"ok":true}` {
		t.Fatalf("%d %q", res.StatusCode, b)
	}
}

func TestBareServerInitializesAtCalendarMCPWithNoTools(t *testing.T) {
	srv := httptest.NewServer(newHandler("1.2.3", nil, nil))
	defer srv.Close()
	ctx := context.Background()
	cs, err := sdk.NewClient(&sdk.Implementation{Name: "c", Version: "1"}, nil).
		Connect(ctx, &sdk.StreamableClientTransport{Endpoint: srv.URL + "/calendar/mcp"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	if info := cs.InitializeResult().ServerInfo; info.Name != "moose-calendar" || info.Version != "1.2.3" {
		t.Errorf("server info %+v", info)
	}
	res, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Tools) != 0 {
		t.Errorf("tools = %d, want 0", len(res.Tools))
	}
	// The email endpoint is not served here.
	for _, path := range []string{"/email/mcp", "/mcp"} {
		res, err := http.Post(srv.URL+path, "application/json", strings.NewReader("{}"))
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != http.StatusNotFound {
			t.Errorf("%s = %d, want 404 on the calendar server", path, res.StatusCode)
		}
	}
}

type bearer struct{ token string }

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	return http.DefaultTransport.RoundTrip(r)
}

func mintToken(t *testing.T, st store.Store, raw, owner, audience string) {
	t.Helper()
	err := st.CreateSession(context.Background(), store.Session{
		Hash: store.HashSecret(raw), Kind: store.SessionAccess, Owner: owner, ClientID: "c", Audience: audience, Scope: "mcp",
		Created: time.Now(), Expires: time.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
}

func protectedServer(t *testing.T) (*httptest.Server, *wiring, string) {
	t.Helper()
	cfg := baseConfig(t)
	var srv *httptest.Server
	// The audience is the public origin, which is the test server's own URL, so
	// the server is started first and wired second.
	var h http.Handler
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { h.ServeHTTP(w, r) }))
	t.Cleanup(srv.Close)
	cfg.Email.BaseURL = srv.URL
	w, err := buildWiring(context.Background(), cfg, quiet, noAWS)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { w.store.Close() })
	if w.protect == nil {
		t.Fatal("with a public origin the endpoint must be protected")
	}
	h = newHandler("t", w.protect, registerTools(w, cfg))
	return srv, w, srv.URL
}

func TestCalendarEndpointAcceptsOnlyCalendarTokens(t *testing.T) {
	srv, w, base := protectedServer(t)
	ctx := context.Background()
	now := time.Now()
	for _, a := range []store.Account{
		{Owner: "pat", ID: "acc_one", Provider: store.ProviderGoogle, ProviderUID: "a@example.com", Address: "a@example.com", Nickname: "One", Status: store.StatusOK, HourlyCap: 10,
			Services: map[string]store.ServiceGrant{store.ServiceCalendar: {Scopes: []string{"https://www.googleapis.com/auth/calendar.events", "https://www.googleapis.com/auth/calendar.readonly"}, GrantedAt: now}}},
		{Owner: "other", ID: "acc_two", Provider: store.ProviderGoogle, ProviderUID: "b@example.com", Address: "b@example.com", Nickname: "Two", Status: store.StatusOK, HourlyCap: 10},
	} {
		a.Created, a.Updated = now, now
		if err := w.store.CreateAccount(ctx, a); err != nil {
			t.Fatal(err)
		}
	}
	mintToken(t, w.store, "mat_calendar", "pat", base+"/calendar/mcp")
	mintToken(t, w.store, "mat_email", "pat", base+"/email/mcp")
	mintToken(t, w.store, "mat_none", "pat", "")

	status := func(token string) (int, string) {
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/calendar/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		return res.StatusCode, res.Header.Get("WWW-Authenticate")
	}
	if code, _ := status("mat_calendar"); code != 200 {
		t.Fatalf("calendar token = %d, want 200", code)
	}
	for name, tok := range map[string]string{"email token": "mat_email", "no audience": "mat_none", "garbage": "nope", "no token": ""} {
		code, challenge := status(tok)
		if code != 401 {
			t.Errorf("%s = %d, want 401", name, code)
		}
		if !strings.Contains(challenge, "/.well-known/oauth-protected-resource/calendar/mcp") {
			t.Errorf("%s challenge = %q, want the calendar resource metadata", name, challenge)
		}
	}

	// A calendar token reaches calendar_list_accounts, scoped to its owner.
	cs, err := sdk.NewClient(&sdk.Implementation{Name: "c", Version: "1"}, nil).
		Connect(ctx, &sdk.StreamableClientTransport{Endpoint: srv.URL + "/calendar/mcp", HTTPClient: &http.Client{Transport: bearer{"mat_calendar"}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	list, err := cs.ListTools(ctx, nil)
	if err != nil || len(list.Tools) != 12 {
		t.Fatalf("tools = %+v, %v", list, err)
	}
	for _, tl := range list.Tools {
		if !strings.HasPrefix(tl.Name, "calendar_") {
			t.Errorf("tool %q is not a calendar tool", tl.Name)
		}
	}
	res, err := cs.CallTool(ctx, &sdk.CallToolParams{Name: tools.ToolListAccounts})
	if err != nil || res.IsError {
		t.Fatalf("call = %+v, %v", res, err)
	}
	var text strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*sdk.TextContent); ok {
			text.WriteString(tc.Text)
		}
	}
	if !strings.Contains(text.String(), "a@example.com") || strings.Contains(text.String(), "b@example.com") || !strings.Contains(text.String(), `"granted"`) {
		t.Errorf("accounts = %s, want only pat's account with a granted calendar", text.String())
	}
}

func TestBuildWiringServesOpenOnlyInDevWithoutAnOrigin(t *testing.T) {
	cfg := baseConfig(t)
	cfg.DevIdentity = "pat"
	w, err := buildWiring(context.Background(), cfg, quiet, noAWS)
	if err != nil {
		t.Fatal(err)
	}
	defer w.store.Close()
	if w.protect != nil {
		t.Error("no public origin: the endpoint has no verifier")
	}
	// The dev identity is the tool owner there, and only there.
	d := registerTools(w, cfg)
	if d == nil {
		t.Fatal("no registrar")
	}
	srv := httptest.NewServer(newHandler("t", w.protect, d))
	defer srv.Close()
	ctx := context.Background()
	cs, err := sdk.NewClient(&sdk.Implementation{Name: "c", Version: "1"}, nil).
		Connect(ctx, &sdk.StreamableClientTransport{Endpoint: srv.URL + "/calendar/mcp"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	res, err := cs.CallTool(ctx, &sdk.CallToolParams{Name: tools.ToolListAccounts})
	if err != nil || res.IsError {
		t.Fatalf("dev call = %+v, %v", res, err)
	}

	// An unknown store kind is an error, not a silent default.
	bad := baseConfig(t)
	bad.Store.Kind = "carrier-pigeon"
	if _, err := buildWiring(context.Background(), bad, quiet, noAWS); err == nil {
		t.Error("an unknown store kind was accepted")
	}
}

func TestProviderGatingMatchesMooseEmail(t *testing.T) {
	e := config.Email{Providers: []string{config.MailProviderGoogle, config.MailProviderMicrosoft}}
	on := enabledProviders(e)
	if !on(store.ProviderGoogle) || on(store.ProviderMicrosoft) {
		t.Errorf("microsoft is listed but not enabled: google=%v microsoft=%v", on(store.ProviderGoogle), on(store.ProviderMicrosoft))
	}
	e.Microsoft.Enabled = true
	if !enabledProviders(e)(store.ProviderMicrosoft) || enabledProviders(e)("yahoo") {
		t.Error("gating wrong after enabling microsoft")
	}
}
