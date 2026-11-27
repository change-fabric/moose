package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/change-fabric/moose/internal/accounts/envelope"
	"github.com/change-fabric/moose/internal/accounts/store"
	"github.com/change-fabric/moose/internal/calendar/tools"
	"github.com/change-fabric/moose/internal/config"
)

var devKey = base64.StdEncoding.EncodeToString([]byte(strings.Repeat("k", 32)))

func providerConfig(t *testing.T) config.Config {
	t.Helper()
	cfg := baseConfig(t)
	cfg.DevIdentity = "pat"
	cfg.Email.DevKey = devKey
	cfg.Email.GoogleClientID, cfg.Email.GoogleClientSecret = "gid", "gsecret"
	return cfg
}

func TestProvidersAreBuiltOnlyWhereStoredCredentialsCanBeOpened(t *testing.T) {
	ctx := context.Background()
	cfg := providerConfig(t)
	w, err := buildWiring(ctx, cfg, quiet, noAWS)
	if err != nil {
		t.Fatal(err)
	}
	defer w.store.Close()
	if w.providers == nil || len(w.cursorKey) != 32 {
		t.Fatalf("providers = %v, cursor key %d bytes: a dev key opens credentials", w.providers, len(w.cursorKey))
	}
	// Every instance derives the same cursor key, so a cursor survives a cold start.
	cfg2 := providerConfig(t)
	w2, err := buildWiring(ctx, cfg2, quiet, noAWS)
	if err != nil {
		t.Fatal(err)
	}
	defer w2.store.Close()
	if string(w.cursorKey) != string(w2.cursorKey) {
		t.Error("two instances derived different cursor keys")
	}
	// The key is bound to its purpose: it is not the dev key itself.
	if string(w.cursorKey) == devKey || strings.Contains(string(w.cursorKey), "gsecret") {
		t.Error("the cursor key is not derived")
	}

	// No key at all: no providers, and the tools say so rather than failing obscurely.
	bare := baseConfig(t)
	bare.DevIdentity = "pat"
	wb, err := buildWiring(ctx, bare, quiet, noAWS)
	if err != nil {
		t.Fatal(err)
	}
	defer wb.store.Close()
	if wb.providers != nil {
		t.Fatal("providers were built with no way to open credentials")
	}
	srv := httptest.NewServer(newHandler("t", wb.protect, registerTools(wb, bare)))
	defer srv.Close()
	cs, err := sdk.NewClient(&sdk.Implementation{Name: "c", Version: "1"}, nil).
		Connect(ctx, &sdk.StreamableClientTransport{Endpoint: srv.URL + "/calendar/mcp"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	res, err := cs.CallTool(ctx, &sdk.CallToolParams{Name: tools.ToolListCalendars})
	if err != nil || !res.IsError || !strings.Contains(textOf(res), "not configured") {
		t.Fatalf("call = %+v, %v; want a not configured error", res, err)
	}

	// A malformed dev key is an error at start, not at the first call.
	badKey := providerConfig(t)
	badKey.Email.DevKey = "not base64!"
	if _, err := buildWiring(ctx, badKey, quiet, noAWS); err == nil {
		t.Error("a malformed dev key was accepted")
	}
	shortKey := providerConfig(t)
	shortKey.Email.DevKey = base64.StdEncoding.EncodeToString([]byte("short"))
	if _, err := buildWiring(ctx, shortKey, quiet, noAWS); err == nil {
		t.Error("a short dev key was accepted")
	}
}

func textOf(res *sdk.CallToolResult) string {
	var b strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*sdk.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String()
}

// The whole read path: a tool call opens the stored credential, refreshes it at
// the token endpoint, and reads the Google Calendar API with the new token.
func TestListEventsReadsThroughTheStoredCredentialToTheProvider(t *testing.T) {
	ctx := context.Background()
	var tokenCalls int
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tokenCalls++
		_ = r.ParseForm()
		if r.PostForm.Get("refresh_token") != "the-refresh-token" {
			t.Errorf("refresh_token = %q", r.PostForm.Get("refresh_token"))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"at-1","token_type":"Bearer","expires_in":3600}`))
	}))
	defer tokenSrv.Close()
	var seen []string
	calSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Header.Get("Authorization")+" "+r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/calendarList"):
			_ = json.NewEncoder(w).Encode(map[string]any{"items": []map[string]any{{"id": "me@example.com", "summary": "Me", "primary": true, "accessRole": "owner", "timeZone": "Europe/Berlin"}}})
		case strings.HasSuffix(r.URL.Path, "/events"):
			_ = json.NewEncoder(w).Encode(map[string]any{"timeZone": "Europe/Berlin", "items": []map[string]any{{
				"id": "e1", "etag": `"1"`, "status": "confirmed", "summary": "Standup",
				"start":     map[string]any{"dateTime": "2026-11-25T09:00:00+01:00", "timeZone": "Europe/Berlin"},
				"end":       map[string]any{"dateTime": "2026-11-25T09:15:00+01:00", "timeZone": "Europe/Berlin"},
				"organizer": map[string]any{"email": "me@example.com", "self": true},
			}}})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer calSrv.Close()

	cfg := providerConfig(t)
	w, err := buildWiring(ctx, cfg, quiet, noAWS)
	if err != nil {
		t.Fatal(err)
	}
	defer w.store.Close()
	w.providers.GoogleCalendarBaseURL = calSrv.URL
	w.providers.Configs[store.ProviderGoogle].Endpoint.TokenURL = tokenSrv.URL

	key, _ := base64.StdEncoding.DecodeString(devKey)
	seal, err := envelope.NewLocal(key)
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := seal.Seal(ctx, []byte("the-refresh-token"), envelope.Context{Owner: "pat", AccountID: "acc_g"})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	err = w.store.CreateAccount(ctx, store.Account{
		Owner: "pat", ID: "acc_g", Provider: store.ProviderGoogle, ProviderUID: "me@example.com", Address: "me@example.com", Nickname: "Me",
		Status: store.StatusOK, HourlyCap: 10, RefreshToken: sealed, Created: now, Updated: now,
		Services: map[string]store.ServiceGrant{store.ServiceCalendar: {
			Scopes:    []string{"https://www.googleapis.com/auth/calendar.events", "https://www.googleapis.com/auth/calendar.readonly"},
			GrantedAt: now,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}

	srv := httptest.NewServer(newHandler("t", w.protect, registerTools(w, cfg)))
	defer srv.Close()
	cs, err := sdk.NewClient(&sdk.Implementation{Name: "c", Version: "1"}, nil).
		Connect(ctx, &sdk.StreamableClientTransport{Endpoint: srv.URL + "/calendar/mcp"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	res, err := cs.CallTool(ctx, &sdk.CallToolParams{Name: tools.ToolListEvents, Arguments: map[string]any{"start": "2026-11-25", "end": "2026-11-26"}})
	if err != nil || res.IsError {
		t.Fatalf("call = %v, %v", textOf(res), err)
	}
	out := textOf(res)
	if !strings.Contains(out, "Title: Standup") || !strings.Contains(out, "untrusted-content provenance") || !strings.Contains(out, `"organizer_trust":"self"`) {
		t.Errorf("result = %s", out)
	}
	if tokenCalls != 1 {
		t.Errorf("token endpoint called %d times", tokenCalls)
	}
	for _, s := range seen {
		if !strings.HasPrefix(s, "Bearer at-1 ") {
			t.Errorf("the calendar API saw %q, want the refreshed bearer token", s)
		}
	}
	if len(seen) != 2 {
		t.Errorf("calendar API calls = %v, want the calendar list and one events read", seen)
	}
}
