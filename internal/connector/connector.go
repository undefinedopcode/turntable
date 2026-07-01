// Package connector defines the extension point for queryable data sources.
//
// A Connector exposes one or more Datasets as typed rows. The core engine
// depends only on the interfaces here; individual connectors live under
// internal/connector/connectors/<name> and register with the Registry.
package connector

import (
	"context"

	"github.com/april/turntable/internal/engine"
	"github.com/april/turntable/internal/sql"
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
	Columns   []string    // projection pushdown; nil = all columns
	Predicate Expr        // filter pushdown; nil = none (see Expr below)
	Limit     *int        // optional row limit
	OrderBy   []OrderTerm // optional ordering hints

	// Aggregate, when non-nil, asks the connector to compute a grouped
	// aggregation natively and return the already-aggregated rows. The planner
	// only sets it after AggregatePusher.PushAggregate accepted the request, so
	// a connector that receives it has already agreed to FULLY apply the
	// grouping, the aggregates, and Aggregate.Predicate — the engine runs no
	// Aggregate or WHERE-Filter of its own above such a scan. See AggregateRequest.
	Aggregate *AggregateRequest
}

// AggregateRequest describes a grouped aggregation to compute at the source.
// It is produced by the planner from a `GROUP BY`/aggregate query over a single
// scan and handed to an AggregatePusher. Accepting it is an all-or-nothing
// contract: the connector must apply GroupBy, every Aggregate, and Predicate
// exactly, because the engine drops its own Aggregate and WHERE-Filter nodes for
// that scan (there are no raw rows left to re-aggregate or re-filter).
type AggregateRequest struct {
	GroupBy    []string      // group-by (breakdown) column names; all plain columns
	Aggregates []AggregateOp // the aggregate calculations to compute
	Predicate  Expr          // the WHERE to apply natively (nil = none)
}

// AggregateOp is one aggregate calculation in an AggregateRequest.
type AggregateOp struct {
	Func     string // aggregate name, upper-case (COUNT, SUM, AVG, MIN, MAX, …)
	Column   string // argument column; "" for COUNT(*)
	Distinct bool   // COUNT(DISTINCT col)
	Alias    string // output column name (the aggregate's SELECT alias or a synthetic name)
}

// AggregatePusher is an optional Connector capability. Given a proposed grouped
// aggregation, PushAggregate reports whether the connector can compute it
// natively and, if so, the schema of the aggregated rows Scan will then return
// (group-by columns followed by one column per AggregateOp, in request order).
// Returning ok=false declines the pushdown, leaving the aggregation to the
// engine over the connector's raw rows; sources that cannot produce raw rows
// (e.g. Honeycomb) should instead return an error explaining what is unsupported.
type AggregatePusher interface {
	PushAggregate(ctx context.Context, ds Dataset, agg AggregateRequest) (schema engine.Schema, ok bool, err error)
}

// OrderTerm describes a pushed-down ordering term.
type OrderTerm struct {
	Column string
	Desc   bool
}

// Expr is the pushdown predicate AST. It is an alias for the parser's Expr so
// connectors that understand predicates can work with it directly, while
// connectors that do not can leave Predicate nil.
type Expr = sql.Expr

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
