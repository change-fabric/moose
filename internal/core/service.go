package core

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
)

// Connector is the contract a data source implements. Package connectors
// re-exports it; it lives here so core does not depend on connectors.
type Connector interface {
	ID() string
	Health(ctx context.Context) error
	List(ctx context.Context, pageToken string, limit int) (SearchResult, error)
	Search(ctx context.Context, q SearchQuery) (SearchResult, error)
	Get(ctx context.Context, id string, maxBytes int64) (Document, error)
}

// Sentinel errors surfaced to transports.
var (
	ErrNotFound      = errors.New("not found or not shared with moose")
	ErrInvalidInput  = errors.New("invalid input")
	ErrUnknownSource = errors.New("unknown source")
)

// Defaults and limits.
const (
	DefaultLimit    = 20
	MaxLimit        = 100
	MaxQueryLen     = 256
	DefaultMaxBytes = 2 << 20
	redactedMarker  = "[REDACTED]"
)

// Describer is optionally implemented by connectors to name themselves and
// add health detail (for example the account to share content with).
type Describer interface {
	Name() string
	Kind() string
	Detail(ctx context.Context) string
}

// Service is the one core every surface (REST, MCP, CLI via REST) uses. It
// never touches HTTP.
type Service struct {
	connectors map[string]Connector
	order      []string
	redact     []*regexp.Regexp
	maxBytes   int64
}

// Options configure a Service.
type Options struct {
	RedactPatterns []string
	MaxBytes       int64
}

// NewService builds a Service. Invalid redaction patterns are an error that
// names the offending pattern.
func NewService(conns []Connector, opts Options) (*Service, error) {
	s := &Service{connectors: map[string]Connector{}, maxBytes: opts.MaxBytes}
	if s.maxBytes <= 0 {
		s.maxBytes = DefaultMaxBytes
	}
	for _, c := range conns {
		id := c.ID()
		if _, dup := s.connectors[id]; dup {
			return nil, fmt.Errorf("core: duplicate connector %q", id)
		}
		s.connectors[id] = c
		s.order = append(s.order, id)
	}
	sort.Strings(s.order)
	for _, p := range opts.RedactPatterns {
		re, err := regexp.Compile(p)
		if err != nil {
			return nil, fmt.Errorf("core: invalid redaction pattern %q: %w", p, err)
		}
		s.redact = append(s.redact, re)
	}
	return s, nil
}

// ListSources reports each connector with its live health.
func (s *Service) ListSources(ctx context.Context) []Source {
	out := make([]Source, 0, len(s.order))
	for _, id := range s.order {
		c := s.connectors[id]
		src := Source{ID: id, Kind: id, Name: id, Healthy: true}
		if d, ok := c.(Describer); ok {
			src.Name, src.Kind, src.Detail = d.Name(), d.Kind(), d.Detail(ctx)
		}
		if err := c.Health(ctx); err != nil {
			src.Healthy = false
			src.Detail = strings.TrimSpace(src.Detail + " " + err.Error())
		}
		out = append(out, src)
	}
	return out
}

// Search runs q against one source, or fans out across all connectors when
// q.Source is empty. A fan-out returns partial results if at least one
// connector succeeds; page tokens apply only to single-source searches.
func (s *Service) Search(ctx context.Context, q SearchQuery) (SearchResult, error) {
	q.Text = strings.TrimSpace(q.Text)
	if q.Text == "" || len(q.Text) > MaxQueryLen {
		return SearchResult{}, fmt.Errorf("%w: query must be 1..%d characters", ErrInvalidInput, MaxQueryLen)
	}
	q.Limit = clampLimit(q.Limit)

	if q.Source != "" {
		c, err := s.connector(q.Source)
		if err != nil {
			return SearchResult{}, err
		}
		res, err := c.Search(ctx, q)
		if err != nil {
			return SearchResult{}, err
		}
		s.redactRefs(res.Items)
		return res, nil
	}

	type result struct {
		res SearchResult
		err error
	}
	results := make([]result, len(s.order))
	var wg sync.WaitGroup
	for i, id := range s.order {
		wg.Add(1)
		go func(i int, c Connector) {
			defer wg.Done()
			sub := q
			sub.Source, sub.PageToken = c.ID(), ""
			r, err := c.Search(ctx, sub)
			results[i] = result{r, err}
		}(i, s.connectors[id])
	}
	wg.Wait()

	var merged SearchResult
	var errs []error
	for _, r := range results {
		if r.err != nil {
			errs = append(errs, r.err)
			continue
		}
		merged.Items = append(merged.Items, r.res.Items...)
	}
	if len(errs) > 0 && len(errs) == len(results) {
		return SearchResult{}, errors.Join(errs...)
	}
	sort.SliceStable(merged.Items, func(i, j int) bool {
		return merged.Items[i].Modified.After(merged.Items[j].Modified)
	})
	if len(merged.Items) > q.Limit {
		merged.Items = merged.Items[:q.Limit]
	}
	s.redactRefs(merged.Items)
	return merged, nil
}

// List returns top-level items from one source.
func (s *Service) List(ctx context.Context, source, pageToken string, limit int) (SearchResult, error) {
	c, err := s.connector(source)
	if err != nil {
		return SearchResult{}, err
	}
	res, err := c.List(ctx, pageToken, clampLimit(limit))
	if err != nil {
		return SearchResult{}, err
	}
	s.redactRefs(res.Items)
	return res, nil
}

// Get fetches one document with redaction applied.
func (s *Service) Get(ctx context.Context, source, id string) (Document, error) {
	if strings.TrimSpace(id) == "" {
		return Document{}, fmt.Errorf("%w: id is required", ErrInvalidInput)
	}
	c, err := s.connector(source)
	if err != nil {
		return Document{}, err
	}
	doc, err := c.Get(ctx, id, s.maxBytes)
	if err != nil {
		return Document{}, err
	}
	doc.Content = s.redactString(doc.Content)
	doc.Snippet = s.redactString(doc.Snippet)
	return doc, nil
}

func (s *Service) connector(id string) (Connector, error) {
	c, ok := s.connectors[id]
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrUnknownSource, id)
	}
	return c, nil
}

func (s *Service) redactRefs(items []DocRef) {
	for i := range items {
		items[i].Snippet = s.redactString(items[i].Snippet)
	}
}

func (s *Service) redactString(in string) string {
	for _, re := range s.redact {
		in = re.ReplaceAllString(in, redactedMarker)
	}
	return in
}

func clampLimit(n int) int {
	switch {
	case n <= 0:
		return DefaultLimit
	case n > MaxLimit:
		return MaxLimit
	}
	return n
}
