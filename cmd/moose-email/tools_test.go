package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/change-fabric/moose/internal/accounts/connect"
	"github.com/change-fabric/moose/internal/accounts/store"
)

type bearer struct{ token string }

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	return http.DefaultTransport.RoundTrip(r)
}

// A bearer token minted for an owner reaches email_list_accounts through the
// real handler stack, and sees only that owner's accounts.
func TestListAccountsThroughTheProtectedEndpoint(t *testing.T) {
	cfg := oauthConfig(t)
	ctx := context.Background()
	w, err := buildConnect(ctx, cfg, quiet, noAWS)
	if err != nil || w == nil || w.oauth == nil {
		t.Fatalf("buildConnect = %v, %v", w, err)
	}
	defer w.store.Close()
	now := time.Now().UTC()
	for _, a := range []store.Account{
		{Owner: "pat", ID: "acc_one", Provider: "gmail", ProviderUID: "a@example.com", Address: "a@example.com", Nickname: "Work", Status: store.StatusOK, HourlyCap: 10},
		{Owner: "someone", ID: "acc_two", Provider: "gmail", ProviderUID: "b@example.com", Address: "b@example.com", Nickname: "Theirs", Status: store.StatusOK, HourlyCap: 10},
	} {
		a.Created, a.Updated = now, now
		if err := w.store.CreateAccount(ctx, a); err != nil {
			t.Fatal(err)
		}
	}
	resource := w.oauth.Resource()
	for _, tok := range []struct{ raw, owner string }{{"tok-pat", "pat"}, {"tok-someone", "someone"}} {
		err := w.store.CreateSession(ctx, store.Session{
			Hash: store.HashSecret(tok.raw), Kind: store.SessionAccess, Owner: tok.owner, ClientID: "c", FamilyID: "f",
			Audience: resource, Scope: "mcp", Created: now, Expires: now.Add(time.Hour),
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	mounts := append([]mount{{connect.Prefix, w.connect.Handler()}}, oauthMounts(w.oauth)...)
	srv := httptest.NewServer(newHandlerAuth("t", w.oauth.Protect, registerTools(w, cfg), mounts...))
	defer srv.Close()

	list := func(token string) []string {
		t.Helper()
		cs, err := sdk.NewClient(&sdk.Implementation{Name: "c", Version: "1"}, nil).Connect(ctx, &sdk.StreamableClientTransport{
			Endpoint: srv.URL + "/email/mcp", HTTPClient: &http.Client{Transport: bearer{token}},
		}, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer cs.Close()
		res, err := cs.CallTool(ctx, &sdk.CallToolParams{Name: "email_list_accounts"})
		if err != nil || res.IsError {
			t.Fatalf("call = %+v, %v", res, err)
		}
		b, _ := json.Marshal(res.StructuredContent)
		var out struct {
			Accounts []struct {
				AccountID string `json:"account_id"`
			} `json:"accounts"`
		}
		if err := json.Unmarshal(b, &out); err != nil {
			t.Fatal(err)
		}
		var ids []string
		for _, a := range out.Accounts {
			ids = append(ids, a.AccountID)
		}
		return ids
	}
	if ids := list("tok-pat"); len(ids) != 1 || ids[0] != "acc_one" {
		t.Errorf("pat sees %v", ids)
	}
	if ids := list("tok-someone"); len(ids) != 1 || ids[0] != "acc_two" {
		t.Errorf("someone sees %v", ids)
	}
}

// The dashboard login must be routed to the authorization server, ahead of the
// connect page's own /connect/ prefix, or the owner cannot sign in to connect Google.
func TestDashboardLoginIsMountedOnTheAuthorizationServer(t *testing.T) {
	w, err := buildConnect(context.Background(), oauthConfig(t), quiet, noAWS)
	if err != nil || w == nil || w.oauth == nil {
		t.Fatalf("buildConnect = %v, %v", w, err)
	}
	defer w.store.Close()
	got := map[string]bool{}
	for _, m := range oauthMounts(w.oauth) {
		got[m.pattern] = true
	}
	for _, p := range []string{"/connect/login", "/connect/login/poll"} {
		if !got[p] {
			t.Errorf("oauthMounts lacks %s", p)
		}
	}
}
