// Package fake is an in-memory Connector for tests.
package fake

import (
	"context"
	"sort"
	"strings"
	"sync"

	"github.com/change-fabric/moose/internal/core"
)

// Connector serves a fixed set of documents from memory.
type Connector struct {
	IDValue   string
	HealthErr error
	SearchErr error

	mu   sync.Mutex
	docs map[string]core.Document
}

// New returns a fake connector with the given id and documents.
func New(id string, docs ...core.Document) *Connector {
	c := &Connector{IDValue: id, docs: map[string]core.Document{}}
	for _, d := range docs {
		c.Put(d)
	}
	return c
}

// Put adds or replaces a document.
func (c *Connector) Put(d core.Document) {
	c.mu.Lock()
	defer c.mu.Unlock()
	d.Source = c.IDValue
	c.docs[d.ID] = d
}

func (c *Connector) ID() string                   { return c.IDValue }
func (c *Connector) Health(context.Context) error { return c.HealthErr }

func (c *Connector) List(_ context.Context, _ string, limit int) (core.SearchResult, error) {
	return c.collect(func(core.Document) bool { return true }, limit), nil
}

func (c *Connector) Search(_ context.Context, q core.SearchQuery) (core.SearchResult, error) {
	if c.SearchErr != nil {
		return core.SearchResult{}, c.SearchErr
	}
	t := strings.ToLower(q.Text)
	return c.collect(func(d core.Document) bool {
		return strings.Contains(strings.ToLower(d.Title), t) || strings.Contains(strings.ToLower(d.Content), t)
	}, q.Limit), nil
}

func (c *Connector) Get(_ context.Context, id string, maxBytes int64) (core.Document, error) {
	c.mu.Lock()
	d, ok := c.docs[id]
	c.mu.Unlock()
	if !ok {
		return core.Document{}, core.ErrNotFound
	}
	if maxBytes > 0 && int64(len(d.Content)) > maxBytes {
		d.Content, d.Truncated = d.Content[:maxBytes], true
	}
	return d, nil
}

func (c *Connector) collect(keep func(core.Document) bool, limit int) core.SearchResult {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []core.DocRef
	for _, d := range c.docs {
		if keep(d) {
			ref := d.DocRef
			if ref.Snippet == "" {
				ref.Snippet = d.Content
			}
			out = append(out, ref)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Modified.After(out[j].Modified) })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return core.SearchResult{Items: out}
}
