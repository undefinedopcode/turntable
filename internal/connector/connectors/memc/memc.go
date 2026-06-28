// Package memc is an in-memory connector: a store of named, fully-materialized
// relations. It backs session-scoped materialized views (CREATE MATERIALIZED
// VIEW) — query results are buffered as a Table here and served back on Scan —
// and is a foundation for in-memory tables more generally.
//
// It applies no pushdown (Scan always returns every buffered row); the engine
// re-applies any predicate/projection/sort, per the pushdown contract.
package memc

import (
	"context"
	"fmt"
	"sync"

	"github.com/april/turntable/internal/connector"
	"github.com/april/turntable/internal/engine"
)

// Table is one materialized relation: a fixed schema and its buffered rows.
// Populated distinguishes a defined-but-empty view (CREATE … WITH NO DATA) from
// a genuinely empty result — an unpopulated table is unscannable until refreshed.
type Table struct {
	Schema    engine.Schema
	Rows      []engine.Row
	Populated bool
}

// Connector is a concurrency-safe store of named Tables.
type Connector struct {
	mu     sync.RWMutex
	tables map[string]*Table
}

// New returns an empty in-memory connector.
func New() *Connector { return &Connector{tables: map[string]*Table{}} }

// Name is the qualified-ref prefix ("mem").
func (*Connector) Name() string { return "mem" }

// Put inserts or replaces the table stored under name.
func (c *Connector) Put(name string, t *Table) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.tables[name] = t
}

// Drop removes the named table, reporting whether it existed.
func (c *Connector) Drop(name string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.tables[name]; !ok {
		return false
	}
	delete(c.tables, name)
	return true
}

// Has reports whether a table is stored under name.
func (c *Connector) Has(name string) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	_, ok := c.tables[name]
	return ok
}

func (c *Connector) get(name string) (*Table, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	t, ok := c.tables[name]
	return t, ok
}

// Datasets lists the stored tables.
func (c *Connector) Datasets(ctx context.Context) ([]connector.Dataset, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]connector.Dataset, 0, len(c.tables))
	for name := range c.tables {
		out = append(out, connector.Dataset{Name: name, Source: name})
	}
	return out, nil
}

// Resolve returns the stored schema for a table.
func (c *Connector) Resolve(ctx context.Context, ds connector.Dataset) (engine.Schema, error) {
	t, ok := c.get(ds.Name)
	if !ok {
		return engine.Schema{}, fmt.Errorf("materialized view %q not found", ds.Name)
	}
	return t.Schema, nil
}

// Scan returns the table's buffered rows. An unpopulated view (WITH NO DATA)
// errors until it is refreshed, mirroring PostgreSQL.
func (c *Connector) Scan(ctx context.Context, req connector.ScanRequest) (engine.RowIterator, error) {
	t, ok := c.get(req.Dataset.Name)
	if !ok {
		return nil, fmt.Errorf("materialized view %q not found", req.Dataset.Name)
	}
	if !t.Populated {
		return nil, fmt.Errorf("materialized view %q has not been populated (use REFRESH MATERIALIZED VIEW)", req.Dataset.Name)
	}
	return engine.NewSliceIter(t.Rows), nil
}
