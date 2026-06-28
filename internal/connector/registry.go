package connector

import (
	"context"
	"fmt"
	"sync"
)

// Registry maps logical table names to Connector + Dataset pairs, and to
// Connector instances by short prefix (for qualified refs like "csv:./x").
type Registry struct {
	mu sync.RWMutex

	// sources maps a logical name (from config or -s flag) to a resolved entry.
	sources map[string]Source

	// connectors maps short prefix (e.g. "csv") to a Connector instance.
	connectors map[string]Connector
}

// Source is a registry entry tying a logical name to a connector + dataset.
type Source struct {
	Name     string
	Conn     Connector
	Dataset  Dataset
}

// NewRegistry returns an empty Registry.
func NewRegistry() *Registry {
	return &Registry{
		sources:    make(map[string]Source),
		connectors: make(map[string]Connector),
	}
}

// RegisterConnector registers a Connector instance by its short prefix.
func (r *Registry) RegisterConnector(c Connector) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	name := c.Name()
	if _, ok := r.connectors[name]; ok {
		return fmt.Errorf("connector %q already registered", name)
	}
	r.connectors[name] = c
	return nil
}

// RegisterConnectorAs registers an already-constructed Connector under an
// additional prefix (alias). This lets one connector answer to several
// qualified-ref schemes — e.g. the http connector serving both "http" and
// "https" URL refs.
func (r *Registry) RegisterConnectorAs(prefix string, c Connector) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.connectors[prefix]; ok {
		return fmt.Errorf("connector %q already registered", prefix)
	}
	r.connectors[prefix] = c
	return nil
}

// Connector returns the connector registered under prefix, or nil.
func (r *Registry) Connector(prefix string) Connector {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.connectors[prefix]
}

// Connectors returns all registered connectors (for introspection/.tables).
func (r *Registry) Connectors() []Connector {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Connector, 0, len(r.connectors))
	for _, c := range r.connectors {
		out = append(out, c)
	}
	return out
}

// RegisterSource binds a logical name to a connector + dataset.
func (r *Registry) RegisterSource(name string, conn Connector, ds Dataset) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.sources[name]; ok {
		return fmt.Errorf("source %q already registered", name)
	}
	r.sources[name] = Source{Name: name, Conn: conn, Dataset: ds}
	return nil
}

// RemoveSource unregisters a logical name, reporting whether it existed. Used
// to drop session-scoped sources such as materialized views.
func (r *Registry) RemoveSource(name string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.sources[name]; !ok {
		return false
	}
	delete(r.sources, name)
	return true
}

// Resolve looks up a logical name. The returned ok is false if not found.
func (r *Registry) Resolve(name string) (Source, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	s, ok := r.sources[name]
	return s, ok
}

// Sources returns all registered sources (for .tables introspection).
func (r *Registry) Sources() []Source {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Source, 0, len(r.sources))
	for _, s := range r.sources {
		out = append(out, s)
	}
	return out
}

// ResolveQualified resolves a qualified ref like "csv:./sales.csv" by
// dispatching to the connector for the prefix. The connector decides whether
// the source string identifies a valid dataset.
func (r *Registry) ResolveQualified(ctx context.Context, prefix, source string, options map[string]any) (Source, error) {
	c := r.Connector(prefix)
	if c == nil {
		return Source{}, fmt.Errorf("no connector registered for prefix %q", prefix)
	}
	ds := Dataset{Name: source, Source: source, Options: options}
	return Source{Name: source, Conn: c, Dataset: ds}, nil
}