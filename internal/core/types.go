// Package core holds moose's transport-agnostic domain types and Service.
package core

import "time"

// Source describes a connected data source.
type Source struct {
	ID      string `json:"id"`
	Kind    string `json:"kind"`
	Name    string `json:"name"`
	Healthy bool   `json:"healthy"`
	Detail  string `json:"detail,omitempty"`
}

// DocRef identifies a document in a connector.
type DocRef struct {
	Source   string    `json:"source"`
	ID       string    `json:"id"`
	Title    string    `json:"title"`
	MimeType string    `json:"mime_type"`
	URL      string    `json:"url"`
	Modified time.Time `json:"modified"`
	Snippet  string    `json:"snippet,omitempty"`
}

// Content types a Document may carry.
const (
	ContentTypePlain        = "text/plain"
	ContentTypeCSV          = "text/csv"
	ContentTypeMarkdown     = "text/markdown"
	ContentTypeMetadataOnly = "metadata-only"
)

// Document is a DocRef plus its text content.
type Document struct {
	DocRef
	Content     string `json:"content"`
	ContentType string `json:"content_type"`
	Truncated   bool   `json:"truncated"`
}

// SearchQuery is a search across one source, or all when Source is empty.
type SearchQuery struct {
	Text      string `json:"text"`
	Source    string `json:"source,omitempty"`
	Limit     int    `json:"limit,omitempty"`
	PageToken string `json:"page_token,omitempty"`
}

// SearchResult is one page of results.
type SearchResult struct {
	Items         []DocRef `json:"items"`
	NextPageToken string   `json:"next_page_token,omitempty"`
}
