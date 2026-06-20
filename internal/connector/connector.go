// Package connector defines the extension point for queryable data sources.
//
// A Connector exposes one or more Datasets as typed rows. The core engine
// depends only on the interfaces here; individual connectors live under
// internal/connector/connectors/<name> and register with the Registry.
package connector

import (
	"context"

	"github.com/april/octoparser/internal/engine"
)

// Connector is the extension surface for any queryable source.
type Connector interface {
	// Name is the short prefix used in qualified table refs (e.g. "csv", "json").
	Name() string

	// Datasets lists datasets this connector currently exposes. Many file
	// connectors expose exactly one; DB connectors expose many.
	Datasets(ctx context.Context) ([]Dataset, error)

	// Resolve returns a typed schema for a dataset, possibly inferred.
	Resolve(ctx context.Context, ds Dataset) (engine.Schema, error)

	// Scan produces rows for a dataset, honoring any pushed-down request.
	// A connector may partially honor the request and the engine will apply
	// the residual predicate/projection/sort itself.
	Scan(ctx context.Context, req ScanRequest) (engine.RowIterator, error)
}

// Dataset identifies a single queryable relation within a connector.
// For file connectors, Name is typically the file path; for DB connectors it
// is a qualified table identifier.
type Dataset struct {
	// Name is the user-facing dataset name (e.g. "users", "./sales.csv").
	Name string

	// Source is the connector-specific locator string (DSN, path, URI, etc.).
	Source string

	// Options is connector-specific configuration (delimiter, region, etc.).
	Options map[string]any
}

// ScanRequest carries what the engine would like the connector to handle
// natively. Fields left at zero value mean "connector need not bother".
type ScanRequest struct {
	Dataset   Dataset
	Columns   []string       // projection pushdown; nil = all columns
	Predicate Expr           // filter pushdown; nil = none (see Expr below)
	Limit     *int           // optional row limit
	OrderBy   []OrderTerm    // optional ordering hints
}

// OrderTerm describes a pushed-down ordering term.
type OrderTerm struct {
	Column string
	Desc   bool
}

// Expr is a minimal expression marker for pushed-down predicates. The real
// expression AST lives in internal/sql/ast; connectors that want to interpret
// predicates will accept that concrete type via a type assertion. Keeping this
// interface here avoids an import cycle between connector and sql.
type Expr interface{ exprNode() }

// ScanResponse describes what the connector actually applied, so the engine
// can compute and apply the residual operators itself.
type ScanResponse struct {
	// AppliedPredicate is true if Predicate was fully honored. When false the
	// engine re-applies the original predicate in memory.
	AppliedPredicate bool
	// AppliedLimit is true if Limit was honored.
	AppliedLimit bool
	// AppliedOrderBy is true if OrderBy was honored.
	AppliedOrderBy bool
}