package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestHealthz(t *testing.T) {
	srv := httptest.NewServer(newHandler("t"))
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

func TestMCPInitializesWithNoTools(t *testing.T) {
	srv := httptest.NewServer(newHandler("1.2.3"))
	defer srv.Close()
	ctx := context.Background()
	cs, err := sdk.NewClient(&sdk.Implementation{Name: "c", Version: "1"}, nil).
		Connect(ctx, &sdk.StreamableClientTransport{Endpoint: srv.URL + "/mcp"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	if info := cs.InitializeResult().ServerInfo; info.Name != "moose-email" || info.Version != "1.2.3" {
		t.Errorf("server info %+v", info)
	}
	res, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Tools) != 0 {
		t.Errorf("tools = %d, want 0", len(res.Tools))
	}
}
