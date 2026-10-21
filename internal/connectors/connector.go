// Package connectors defines the interface every data source implements and
// a small registry used to assemble a core.Service.
package connectors

import (
	"fmt"
	"sort"

	"github.com/change-fabric/moose/internal/core"
)

// Connector is a live, pass-through view of one data source. It is declared
// in core (so core does not import this package) and aliased here, where
// connector implementations look for it.
type Connector = core.Connector

// ErrNotFound is returned when a document does not exist or is not shared
// with moose. Callers must not distinguish the two cases.
var ErrNotFound = core.ErrNotFound

// Registry holds connectors by ID.
type Registry struct{ byID map[string]Connector }

// NewRegistry returns an empty registry.
func NewRegistry() *Registry { return &Registry{byID: map[string]Connector{}} }

// Register adds c, failing on a duplicate or empty ID.
func (r *Registry) Register(c Connector) error {
	id := c.ID()
	if id == "" {
		return fmt.Errorf("connectors: empty connector id")
	}
	if _, dup := r.byID[id]; dup {
		return fmt.Errorf("connectors: duplicate connector id %q", id)
	}
	r.byID[id] = c
	return nil
}

// All returns the registered connectors sorted by ID.
func (r *Registry) All() []Connector {
	out := make([]Connector, 0, len(r.byID))
	for _, c := range r.byID {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID() < out[j].ID() })
	return out
}
