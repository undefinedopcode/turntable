// Package plan builds a validated logical plan from a parsed AST. It resolves
// table references to connectors via the Registry, infers/merges schemas,
// validates column/type references, and negotiates per-connector pushdown.
//
// This is a skeleton; full resolution and validation land in the v0.1
// milestone. For now it exposes the core types so the engine and CLI compile
// and the wiring can be demonstrated.
package plan

import (
	"context"
	"fmt"

	"github.com/april/octoparser/internal/connector"
	"github.com/april/octoparser/internal/engine"
	"github.com/april/octoparser/internal/sql"
)

// Plan is a validated, executable logical plan.
type Plan struct {
	Root      Node
	OutputSchema engine.Schema
}

// Node is a node in the logical plan tree.
type Node interface{ planNode() }

// Scan reads rows from a connector dataset.
type Scan struct {
	Source connector.Source
	Schema engine.Schema
}

// Filter applies a residual predicate.
type Filter struct {
	Child     Node
	Predicate sql.Expr
}

// Project computes the select list.
type Project struct {
	Child Node
	Items []sql.SelectItem
}

// Join combines two relations.
type Join struct {
	Kind     sql.JoinKind
	Left     Node
	Right    Node
	On       sql.Expr
	Schema   engine.Schema
}

// Aggregate groups rows.
type Aggregate struct {
	Child   Node
	Keys    []sql.Expr
	Aggs    []sql.FuncCall
	Having  sql.Expr
}

// Sort orders rows.
type Sort struct {
	Child   Node
	Terms   []sql.OrderTerm
}

// Limit applies LIMIT/OFFSET.
type Limit struct {
	Child  Node
	Limit  *int
	Offset *int
}

func (*Scan) planNode()     {}
func (*Filter) planNode()   {}
func (*Project) planNode()  {}
func (*Join) planNode()     {}
func (*Aggregate) planNode() {}
func (*Sort) planNode()     {}
func (*Limit) planNode()    {}

// Build resolves and validates a parsed SELECT into a Plan against the given
// Registry. This is a minimal implementation: it resolves the FROM table ref
// to a Scan node and returns it directly. Joins, projections, predicates,
// aggregation, and pushdown negotiation are completed in v0.1.
func Build(ctx context.Context, stmt *sql.SelectStmt, reg *connector.Registry) (*Plan, error) {
	if stmt == nil {
		return nil, fmt.Errorf("nil statement")
	}
	src, err := resolveTableRef(ctx, stmt.From, reg)
	if err != nil {
		return nil, err
	}
	schema, err := src.Conn.Resolve(ctx, src.Dataset)
	if err != nil {
		return nil, fmt.Errorf("resolve schema for %q: %w", src.Name, err)
	}
	root := &Scan{Source: src, Schema: schema}
	return &Plan{Root: root, OutputSchema: schema}, nil
}

func resolveTableRef(ctx context.Context, tr sql.TableRef, reg *connector.Registry) (connector.Source, error) {
	if tr.Subquery != nil {
		return connector.Source{}, fmt.Errorf("subqueries not yet supported")
	}
	if tr.Prefix != "" {
		return reg.ResolveQualified(ctx, tr.Prefix, tr.Source, nil)
	}
	if tr.Name == "" {
		return connector.Source{}, fmt.Errorf("empty table reference")
	}
	s, ok := reg.Resolve(tr.Name)
	if !ok {
		return connector.Source{}, fmt.Errorf("unknown source %q (not in config and not qualified)", tr.Name)
	}
	return s, nil
}