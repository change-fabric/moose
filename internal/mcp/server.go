// Package mcp exposes core.Service as an MCP server (list_sources, search,
// get_document) over stateless, JSON-response streamable HTTP. Authentication
// and auditing wrap the handler in internal/server.
package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/change-fabric/moose/internal/audit"
	"github.com/change-fabric/moose/internal/connectors/gdrive"
	"github.com/change-fabric/moose/internal/core"
)

// Tool names.
const (
	ToolListSources = "list_sources"
	ToolSearch      = "search"
	ToolGetDocument = "get_document"
	defaultSource   = gdrive.ID
)

// SourceOut is one entry of the list_sources output.
type SourceOut struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Healthy bool   `json:"healthy"`
	Detail  string `json:"detail,omitempty"`
}

// ListSourcesOut is the list_sources output.
type ListSourcesOut struct {
	Sources []SourceOut `json:"sources"`
}

// SearchIn is the search input.
type SearchIn struct {
	Query     string `json:"query"`
	Source    string `json:"source,omitempty"`
	Limit     int    `json:"limit,omitempty"`
	PageToken string `json:"page_token,omitempty"`
}

// SearchOut is the search output.
type SearchOut struct {
	Items         []core.DocRef `json:"items"`
	NextPageToken string        `json:"next_page_token,omitempty"`
}

// GetDocumentIn is the get_document input.
type GetDocumentIn struct {
	Source string `json:"source,omitempty"`
	ID     string `json:"id"`
}

func ptr[T any](v T) *T { return &v }

func sourceSchema(withDefault bool) *jsonschema.Schema {
	s := &jsonschema.Schema{Type: "string", Enum: []any{defaultSource}}
	if withDefault {
		s.Default = json.RawMessage(strconv.Quote(defaultSource))
	}
	return s
}

func searchSchema() *jsonschema.Schema {
	return &jsonschema.Schema{
		Type: "object",
		Properties: map[string]*jsonschema.Schema{
			"query":      {Type: "string", MinLength: ptr(1), MaxLength: ptr(core.MaxQueryLen), Description: "Words to match in file names and content"},
			"source":     sourceSchema(false),
			"limit":      {Type: "integer", Minimum: ptr(1.0), Maximum: ptr(float64(core.MaxLimit)), Default: json.RawMessage(`20`)},
			"page_token": {Type: "string"},
		},
		Required: []string{"query"},
	}
}

func getDocumentSchema() *jsonschema.Schema {
	return &jsonschema.Schema{
		Type: "object",
		Properties: map[string]*jsonschema.Schema{
			"source": sourceSchema(true),
			"id":     {Type: "string", MinLength: ptr(1)},
		},
		Required: []string{"id"},
	}
}

// NewServer builds the MCP server over svc.
func NewServer(svc *core.Service, version string) *sdk.Server {
	if version == "" {
		version = "dev"
	}
	s := sdk.NewServer(&sdk.Implementation{Name: "moose", Version: version}, nil)

	sdk.AddTool(s, &sdk.Tool{
		Name:        ToolListSources,
		Description: "List connected data sources and the account to share content with.",
		InputSchema: &jsonschema.Schema{Type: "object"},
	}, func(ctx context.Context, _ *sdk.CallToolRequest, _ struct{}) (*sdk.CallToolResult, ListSourcesOut, error) {
		audit.FromContext(ctx).SetAction(ToolListSources, "", "")
		out := ListSourcesOut{Sources: []SourceOut{}}
		for _, src := range svc.ListSources(ctx) {
			out.Sources = append(out.Sources, SourceOut{ID: src.ID, Name: src.Name, Healthy: src.Healthy, Detail: src.Detail})
		}
		return nil, out, nil
	})

	sdk.AddTool(s, &sdk.Tool{
		Name:        ToolSearch,
		Description: "Search connected data sources for documents whose names or content match the query.",
		InputSchema: searchSchema(),
	}, func(ctx context.Context, _ *sdk.CallToolRequest, in SearchIn) (*sdk.CallToolResult, SearchOut, error) {
		audit.FromContext(ctx).SetAction(ToolSearch, in.Source, "")
		res, err := svc.Search(ctx, core.SearchQuery{Text: in.Query, Source: in.Source, Limit: in.Limit, PageToken: in.PageToken})
		if err != nil {
			return toolError(ctx, err), SearchOut{}, nil
		}
		items := res.Items
		if items == nil {
			items = []core.DocRef{}
		}
		return nil, SearchOut{Items: items, NextPageToken: res.NextPageToken}, nil
	})

	sdk.AddTool(s, &sdk.Tool{
		Name:        ToolGetDocument,
		Description: "Fetch one document's text content by source and id.",
		InputSchema: getDocumentSchema(),
	}, func(ctx context.Context, _ *sdk.CallToolRequest, in GetDocumentIn) (*sdk.CallToolResult, core.Document, error) {
		if in.Source == "" {
			in.Source = defaultSource
		}
		audit.FromContext(ctx).SetAction(ToolGetDocument, in.Source, in.ID)
		doc, err := svc.Get(ctx, in.Source, in.ID)
		if err != nil {
			return toolError(ctx, err), core.Document{}, nil
		}
		return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: documentText(doc)}}}, doc, nil
	})
	return s
}

// Handler serves srv at /mcp as stateless streamable HTTP with JSON responses,
// which Lambda needs and the Mac Mini tolerates.
func Handler(srv *sdk.Server) http.Handler {
	return sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return srv },
		&sdk.StreamableHTTPOptions{Stateless: true, JSONResponse: true})
}

func documentText(d core.Document) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Title: %s\nURL: %s\nModified: %s\nTruncated: %t\n\n", d.Title, d.URL, d.Modified.UTC().Format(time.RFC3339), d.Truncated)
	if d.ContentType == core.ContentTypeMetadataOnly {
		b.WriteString("(no text content; open the URL)")
	} else {
		b.WriteString(d.Content)
	}
	return b.String()
}

// toolError maps errors to an IsError result carrying the REST error code.
func toolError(ctx context.Context, err error) *sdk.CallToolResult {
	code, msg := "internal", "internal error"
	switch {
	case errors.Is(err, core.ErrNotFound):
		code, msg = "not_found", core.ErrNotFound.Error()
	case errors.Is(err, core.ErrInvalidInput), errors.Is(err, core.ErrUnknownSource):
		code, msg = "invalid_request", err.Error()
	case errors.Is(err, gdrive.ErrDocumentTooLarge):
		code, msg = "document_too_large", "document is too large to export; open its URL instead"
	case errors.Is(err, gdrive.ErrUpstreamUnavailable), errors.Is(err, core.ErrUpstreamUnavailable):
		code, msg = "upstream_unavailable", "data source unavailable, try again later"
	}
	audit.FromContext(ctx).SetDetail(code)
	return &sdk.CallToolResult{IsError: true, Content: []sdk.Content{&sdk.TextContent{Text: code + ": " + msg}}}
}
