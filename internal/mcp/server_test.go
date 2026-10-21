package mcp_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/change-fabric/moose/internal/auth"
	"github.com/change-fabric/moose/internal/connectors/fake"
	"github.com/change-fabric/moose/internal/core"
	"github.com/change-fabric/moose/internal/server"
	"github.com/change-fabric/moose/internal/store/sqlite"
)

const me = "me@example.com"

type bearer struct {
	token string
	base  http.RoundTripper
}

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	return b.base.RoundTrip(r)
}

// setup starts the full server and returns its URL and a fresh PAT.
func setup(t *testing.T) (string, string) {
	t.Helper()
	ctx := context.Background()
	st, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "moose.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := auth.EnsureBootstrapAdmin(ctx, st, me, time.Now()); err != nil {
		t.Fatal(err)
	}
	conn := fake.New("gdrive",
		core.Document{DocRef: core.DocRef{ID: "doc1", Title: "Roadmap", MimeType: "text/plain", URL: "https://example.com/doc1", Modified: time.Now()}, Content: "the moose roadmap", ContentType: "text/plain"},
		core.Document{DocRef: core.DocRef{ID: "doc2", Title: "Budget", MimeType: "text/csv", Modified: time.Now()}, Content: "a,b\n1,2", ContentType: "text/csv"},
	)
	svc, err := core.NewService([]core.Connector{conn}, core.Options{})
	if err != nil {
		t.Fatal(err)
	}
	a := &auth.Authenticator{Store: st, Identity: auth.DevIdentity{Login: me}}
	srv := httptest.NewServer(server.New(server.Deps{Service: svc, Store: st, Auth: a, Version: "test"}))
	t.Cleanup(func() { srv.Close(); a.Wait() })

	resp, err := http.Post(srv.URL+auth.BootstrapPath, "application/json", bytes.NewBufferString(`{"name":"mcp"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var boot auth.BootstrapResponse
	if err := json.NewDecoder(resp.Body).Decode(&boot); err != nil || boot.Token == "" {
		t.Fatalf("bootstrap: status %d err %v", resp.StatusCode, err)
	}
	return srv.URL, boot.Token
}

func connect(t *testing.T, url, token string) *sdk.ClientSession {
	t.Helper()
	client := sdk.NewClient(&sdk.Implementation{Name: "moose-test", Version: "test"}, nil)
	tr := &sdk.StreamableClientTransport{
		Endpoint:   url + server.MCPPath,
		HTTPClient: &http.Client{Transport: bearer{token: token, base: http.DefaultTransport}},
		MaxRetries: -1,
	}
	cs, err := client.Connect(context.Background(), tr, nil)
	if err != nil {
		t.Fatalf("initialize: %v", err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

func text(r *sdk.CallToolResult) string {
	var b strings.Builder
	for _, c := range r.Content {
		if tc, ok := c.(*sdk.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String()
}

func structured(t *testing.T, r *sdk.CallToolResult, out any) {
	t.Helper()
	raw, err := json.Marshal(r.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, out); err != nil {
		t.Fatalf("structured %s: %v", raw, err)
	}
}

func TestMCP(t *testing.T) {
	url, tok := setup(t)
	cs := connect(t, url, tok)
	ctx := context.Background()

	if got := cs.InitializeResult().ServerInfo.Name; got != "moose" {
		t.Fatalf("server name = %q", got)
	}

	tools, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tl := range tools.Tools {
		names = append(names, tl.Name)
	}
	sort.Strings(names)
	if strings.Join(names, ",") != "get_document,list_sources,search" {
		t.Fatalf("tools = %v", names)
	}

	t.Run("list_sources", func(t *testing.T) {
		r, err := cs.CallTool(ctx, &sdk.CallToolParams{Name: "list_sources", Arguments: map[string]any{}})
		if err != nil || r.IsError {
			t.Fatalf("err %v result %s", err, text(r))
		}
		var out struct {
			Sources []struct {
				ID      string
				Healthy bool
			}
		}
		structured(t, r, &out)
		if len(out.Sources) != 1 || out.Sources[0].ID != "gdrive" || !out.Sources[0].Healthy {
			t.Fatalf("sources = %+v", out)
		}
	})

	t.Run("search", func(t *testing.T) {
		r, err := cs.CallTool(ctx, &sdk.CallToolParams{Name: "search", Arguments: map[string]any{"query": "roadmap"}})
		if err != nil || r.IsError {
			t.Fatalf("err %v result %s", err, text(r))
		}
		var out struct {
			Items []core.DocRef `json:"items"`
		}
		structured(t, r, &out)
		if len(out.Items) != 1 || out.Items[0].ID != "doc1" {
			t.Fatalf("items = %+v", out)
		}
		// Schema rejects an empty query.
		r, err = cs.CallTool(ctx, &sdk.CallToolParams{Name: "search", Arguments: map[string]any{"query": ""}})
		if err == nil && !r.IsError {
			t.Fatal("empty query accepted")
		}
	})

	t.Run("get_document", func(t *testing.T) {
		r, err := cs.CallTool(ctx, &sdk.CallToolParams{Name: "get_document", Arguments: map[string]any{"id": "doc1"}})
		if err != nil || r.IsError {
			t.Fatalf("err %v result %s", err, text(r))
		}
		var doc core.Document
		structured(t, r, &doc)
		if doc.Content != "the moose roadmap" || doc.Title != "Roadmap" {
			t.Fatalf("doc = %+v", doc)
		}
		if tx := text(r); !strings.Contains(tx, "Title: Roadmap") || !strings.Contains(tx, "the moose roadmap") {
			t.Fatalf("text = %q", tx)
		}

		r, err = cs.CallTool(ctx, &sdk.CallToolParams{Name: "get_document", Arguments: map[string]any{"id": "missing"}})
		if err != nil {
			t.Fatalf("not found is a protocol error: %v", err)
		}
		if !r.IsError || !strings.HasPrefix(text(r), "not_found") {
			t.Fatalf("missing doc result = %+v %q", r, text(r))
		}
	})
}

func TestMCPRequiresBearer(t *testing.T) {
	url, _ := setup(t)
	body := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"x","version":"1"}}}`
	req, _ := http.NewRequest(http.MethodPost, url+server.MCPPath, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if got := resp.Header.Get("WWW-Authenticate"); !strings.Contains(got, `Bearer realm="moose"`) {
		t.Fatalf("WWW-Authenticate = %q", got)
	}

	// The SDK client fails to initialize without the header too.
	client := sdk.NewClient(&sdk.Implementation{Name: "moose-test", Version: "test"}, nil)
	if _, err := client.Connect(context.Background(), &sdk.StreamableClientTransport{Endpoint: url + server.MCPPath, MaxRetries: -1}, nil); err == nil {
		t.Fatal("connect without bearer succeeded")
	}
}
