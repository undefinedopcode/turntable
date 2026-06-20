// Package sqlc is the SQL database connector. It reads tables/views from a
// database via database/sql, with predicate/limit/order pushdown into the
// underlying query.
package sqlc

import (
	"context"
	"fmt"

	"github.com/april/octoparser/internal/connector"
	"github.com/april/octoparser/internal/engine"
)

type Connector struct {
	// Driver is the database/sql driver name (e.g. "postgres", "sqlite3").
	Driver string
	// DSN is the data source name.
	DSN string
}

func New(driver, dsn string) *Connector { return &Connector{Driver: driver, DSN: dsn} }

func (Connector) Name() string { return "sql" }

func (Connector) Datasets(ctx context.Context) ([]connector.Dataset, error) {
	// TODO(v0.2): enumerate schemas/tables via information_schema.
	return nil, nil
}

func (c *Connector) Resolve(ctx context.Context, ds connector.Dataset) (engine.Schema, error) {
	return engine.Schema{}, fmt.Errorf("sqlc.Resolve not yet implemented (v0.2)")
}

func (c *Connector) Scan(ctx context.Context, req connector.ScanRequest) (engine.RowIterator, error) {
	return nil, fmt.Errorf("sqlc.Scan not yet implemented (v0.2)")
}