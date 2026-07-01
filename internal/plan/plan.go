// Package plan builds a validated logical plan from a parsed AST. It resolves
// table references to connectors via the Registry, infers/merges schemas,
// validates column/type references, and decides which operators run in the
// engine (file connectors do no pushdown in v0.1).
//
// The logical plan is a tree of plan.Node values. plan.Exec converts the tree
// into an engine.RowIterator pipeline.
package plan

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/april/turntable/internal/connector"
	"github.com/april/turntable/internal/engine"
	"github.com/april/turntable/internal/sql"
)

// Plan is a validated, executable logical plan.
type Plan struct {
	Root         Node
	OutputSchema engine.Schema
	Funcs        *engine.FuncRegistry
	// Strict makes type-coercion failures hard errors at execution time.
	Strict bool
}

// Node is a node in the logical plan tree.
type Node interface{ planNode() }

// Scan reads rows from a connector dataset.
//
// Predicate, Limit, and OrderBy are pushdown hints handed to the connector via
// the ScanRequest. They are an optimization only: the engine still applies its
// own Filter/Sort/Limit above the Scan, so a connector that ignores or partially
// honors them stays correct. They are set only for single-table scans (no joins)
// where pushing is safe — see buildSelect.
type Scan struct {
	Source    connector.Source
	Schema    engine.Schema
	Alias     string
	Predicate sql.Expr              // WHERE predicate to offer the connector (may be nil)
	Limit     *int                  // row limit to offer the connector (may be nil)
	OrderBy   []connector.OrderTerm // ordering hint (set only when every ORDER BY term is a plain column)

	// Aggregate is set when the connector (an AggregatePusher) accepted a grouped
	// aggregation: the Scan then returns already-aggregated rows conforming to
	// Schema, and the planner emits no Aggregate/WHERE-Filter above it. See
	// buildPushedAggregate.
	Aggregate *connector.AggregateRequest
}

// NoFrom is a synthetic single-row, zero-column relation for "SELECT <expr>"
// queries that have no FROM clause (e.g. scratch math in the REPL).
type NoFrom struct{}

// TableFunc is a set-returning function in FROM (currently generate_series),
// resolved at plan time to its concrete bounds — an integer or timestamp series
// of one column, under an alias.
type TableFunc struct {
	IsTime              bool
	IntStart, IntStop   int64
	IntStep             int64
	TimeStart, TimeStop time.Time
	TimeStep            time.Duration
	Schema              engine.Schema
	Alias               string
}

// Subquery is a derived table: a FROM-clause subquery whose child plan produces
// the rows, presented under an alias. It passes the child's rows through
// unchanged; the alias lets the outer query qualify the subquery's columns.
type Subquery struct {
	Child  Node
	Schema engine.Schema
	Alias  string
}

// CTERef references a materialized common table expression — or, when IsView is
// set, a CREATE VIEW expanded the same way. Every reference to the same CTE/view
// shares one *cteMaterialization, so its plan runs at most once per query (the
// first reference pulled triggers it) and its rows are replayed from an in-memory
// buffer for subsequent references — improving performance when used multiple
// times, and giving a consistent snapshot across all references within the
// query. A CTERef is always wrapped in a Subquery for per-reference column
// qualification.
type CTERef struct {
	Name   string
	Mat    *cteMaterialization
	IsView bool // a CREATE VIEW expansion rather than a WITH-clause CTE
}

// cteMaterialization is the shared, build-time plan plus exec-time row cache for
// one CTE. Plan/Schema are filled at plan time; rows/done/err are filled lazily
// the first time any reference is executed (the plan is single-use, so caching
// exec state on it is safe).
type cteMaterialization struct {
	Plan   Node
	Schema engine.Schema

	rows []engine.Row
	done bool
	err  error
}

// ensure runs the CTE's plan to completion once, caching the rows (and any
// error). Subsequent calls are no-ops. ctx/funcs/strict come from the first
// reference pulled; a CTE is never correlated to the enclosing query, so the
// particular caller does not matter.
func (m *cteMaterialization) ensure(ctx context.Context, funcs *engine.FuncRegistry, strict bool) error {
	if m.done {
		return m.err
	}
	m.done = true
	it, _, err := execNode(ctx, m.Plan, funcs, strict)
	if err != nil {
		m.err = err
		return err
	}
	m.rows, m.err = engine.Materialize(ctx, it)
	return m.err
}

// SetOp combines two query plans by UNION, INTERSECT, or EXCEPT (its branches
// must share a column count). All selects the multiset form (keep duplicates per
// the operator's rule) over the default distinct form. A chain of set operations
// is a left-/precedence-folded tree of these. Any final ORDER BY/LIMIT is layered
// above as Sort/Limit nodes.
type SetOp struct {
	Op     sql.SetOpKind
	All    bool
	Left   Node
	Right  Node
	Schema engine.Schema
}

// subqKind classifies a subquery expression lifted into an Apply column.
type subqKind int

const (
	subqExists subqKind = iota // EXISTS (...)
	subqScalar                 // (SELECT ...) used as a value
	subqIn                     // x IN (correlated SELECT ...)
)

// SubquerySpec is one subquery lifted into a $subN column by the Apply node.
type SubquerySpec struct {
	Name       string
	Kind       subqKind
	Inner      Node     // the subquery plan, built once (correlated refs are OuterRefs)
	Test       sql.Expr // subqIn only: the LHS, evaluated against the outer row
	Negate     bool     // subqIn only: NOT IN
	Correlated bool     // false → the value is the same for every row (memoized)
}

// Apply evaluates correlated/uncorrelated subquery expressions per outer row and
// appends each as a column (Schema = child schema + one column per spec). A
// projection/filter above references the appended columns by name.
type Apply struct {
	Child  Node
	Specs  []SubquerySpec
	Schema engine.Schema
}

// Window computes window-function columns and appends them to its child's rows
// (Schema is the child schema followed by one column per spec). A projection
// above references the appended columns by name.
type Window struct {
	Child  Node
	Specs  []engine.WindowSpec
	Schema engine.Schema
}

// Filter applies a residual predicate.
type Filter struct {
	Child     Node
	Predicate sql.Expr
}

// Project computes the select list.
type Project struct {
	Child    Node
	Outputs  []engine.ProjectedExpr
	Distinct bool
}

// Join combines two relations. The ON condition is split into zero or more
// equi-key pairs (LeftKeys/RightKeys, hashed) plus an optional Residual
// predicate evaluated per candidate pair; with no key pairs the join runs as a
// nested loop. LeftKeys and RightKeys are parallel.
type Join struct {
	Kind      sql.JoinKind
	Left      Node
	Right     Node
	LeftKeys  []engine.KeyExtractor
	RightKeys []engine.KeyExtractor
	Residual  sql.Expr // non-equi ON remainder, applied to each candidate pair (nil if none)
	Schema    engine.Schema
	Aliases   []engine.AliasRange // all contributing aliases in the combined schema
}

// Aggregate groups rows.
type Aggregate struct {
	Child  Node
	Keys   []sql.Expr
	Aggs   []engine.AggSpec
	Having sql.Expr
	Schema engine.Schema
}

// Sort orders rows.
type Sort struct {
	Child Node
	Terms []sql.OrderTerm
}

// Limit applies LIMIT/OFFSET.
type Limit struct {
	Child  Node
	Limit  *int
	Offset *int
}

func (*Scan) planNode()      {}
func (*Subquery) planNode()  {}
func (*CTERef) planNode()    {}
func (*SetOp) planNode()     {}
func (*Window) planNode()    {}
func (*Apply) planNode()     {}
func (*Filter) planNode()    {}
func (*Project) planNode()   {}
func (*Join) planNode()      {}
func (*Aggregate) planNode() {}
func (*Sort) planNode()      {}
func (*Limit) planNode()     {}
func (*NoFrom) planNode()    {}
func (*TableFunc) planNode() {}

// buildCtx carries planner state down the tree.
type buildCtx struct {
	ctx   context.Context
	reg   *connector.Registry
	funcs *engine.FuncRegistry
	ctes  map[string]*cteEntry // WITH clause table expressions, by name
	// viewMat caches each referenced view's materialization for this one build, so
	// a view used several times in a query is planned/run once (see buildView).
	viewMat map[string]*cteEntry
}

// cteEntry is a registered common table expression. visiting guards against a
// CTE referencing itself (recursive CTEs are not supported) during the one-time
// build; mat is the shared materialization, built on the first reference and
// reused by every later reference.
type cteEntry struct {
	query    sql.Statement
	visiting bool
	mat      *cteMaterialization
}

// Build resolves and validates a parsed statement (a SELECT or a UNION of
// SELECTs) into a Plan against the given Registry. Options adjust planning
// behavior (e.g. strict mode).
func Build(ctx context.Context, stmt sql.Statement, reg *connector.Registry, opts ...BuildOption) (*Plan, error) {
	if stmt == nil {
		return nil, fmt.Errorf("nil statement")
	}
	bc := &buildCtx{ctx: ctx, reg: reg, funcs: engine.NewFuncRegistry()}
	var (
		root   Node
		schema engine.Schema
		err    error
	)
	switch s := stmt.(type) {
	case *sql.SelectStmt:
		root, schema, err = bc.buildSelect(s)
	case *sql.SetOpStmt:
		root, schema, err = bc.buildSetOp(s)
	case *sql.WithStmt:
		root, schema, err = bc.buildWith(s)
	default:
		return nil, fmt.Errorf("unsupported statement %T", stmt)
	}
	if err != nil {
		return nil, err
	}
	p := &Plan{Root: root, OutputSchema: schema, Funcs: bc.funcs}
	for _, o := range opts {
		o(p)
	}
	return p, nil
}

// buildSetOp builds the plan for a chain of set operations. Branches must agree
// on column count; the first branch's output schema names the result. Operators
// fold by SQL precedence — INTERSECT binds tighter than UNION/EXCEPT, which are
// left-associative — into a tree of binary SetOp nodes. A trailing ORDER BY/LIMIT
// is layered above.
func (bc *buildCtx) buildSetOp(s *sql.SetOpStmt) (Node, engine.Schema, error) {
	if len(s.Selects) == 0 {
		return nil, engine.Schema{}, fmt.Errorf("empty set operation")
	}
	nodes := make([]Node, len(s.Selects))
	var outSchema engine.Schema
	for i, sel := range s.Selects {
		n, sch, err := bc.buildSelect(sel)
		if err != nil {
			return nil, engine.Schema{}, err
		}
		if i == 0 {
			outSchema = sch
		} else if len(sch.Columns) != len(outSchema.Columns) {
			return nil, engine.Schema{}, fmt.Errorf(
				"each set-operation branch must have the same number of columns (%d vs %d)",
				len(outSchema.Columns), len(sch.Columns))
		}
		nodes[i] = n
	}

	// Pass 1: fold INTERSECT (highest precedence) into the preceding term.
	terms := []Node{nodes[0]}
	var lowOps []sql.SetOpTerm
	for i, op := range s.Ops {
		if op.Kind == sql.SetIntersect {
			j := len(terms) - 1
			terms[j] = &SetOp{Op: sql.SetIntersect, All: op.All, Left: terms[j], Right: nodes[i+1], Schema: outSchema}
		} else {
			terms = append(terms, nodes[i+1])
			lowOps = append(lowOps, op)
		}
	}
	// Pass 2: fold the remaining UNION/EXCEPT left to right.
	root := terms[0]
	for i, op := range lowOps {
		root = &SetOp{Op: op.Kind, All: op.All, Left: root, Right: terms[i+1], Schema: outSchema}
	}

	if len(s.OrderBy) > 0 {
		root = &Sort{Child: root, Terms: s.OrderBy}
	}
	if s.Limit != nil || s.Offset != nil {
		root = &Limit{Child: root, Limit: s.Limit, Offset: s.Offset}
	}
	return root, outSchema, nil
}

// buildWith registers the WITH clause's CTEs (each in scope for the later CTEs
// and the body) and builds the body. Each CTE's plan is built once on first
// reference and shared (see buildCTE), so a CTE used twice is planned once and
// materialized once at run time.
func (bc *buildCtx) buildWith(s *sql.WithStmt) (Node, engine.Schema, error) {
	if bc.ctes == nil {
		bc.ctes = map[string]*cteEntry{}
	}
	for i := range s.CTEs {
		c := s.CTEs[i]
		if _, dup := bc.ctes[c.Name]; dup {
			return nil, engine.Schema{}, fmt.Errorf("duplicate CTE name %q", c.Name)
		}
		bc.ctes[c.Name] = &cteEntry{query: c.Query}
	}
	return bc.buildStatement(s.Body)
}

// buildStatement builds a SELECT or UNION statement node.
func (bc *buildCtx) buildStatement(stmt sql.Statement) (Node, engine.Schema, error) {
	switch s := stmt.(type) {
	case *sql.SelectStmt:
		return bc.buildSelect(s)
	case *sql.SetOpStmt:
		return bc.buildSetOp(s)
	default:
		return nil, engine.Schema{}, fmt.Errorf("unsupported statement %T", stmt)
	}
}

// buildCTE plans a reference to the named CTE, wrapping a shared CTERef in a
// Subquery node so its columns qualify under the reference alias. The CTE's plan
// is built once (on the first reference) and shared via the cteEntry, so it
// executes at most once per query and is replayed for later references. It
// guards against recursion during that one-time build.
func (bc *buildCtx) buildCTE(name, alias string) (Node, engine.Schema, error) {
	e := bc.ctes[name]
	if e.mat == nil {
		if e.visiting {
			return nil, engine.Schema{}, fmt.Errorf("recursive CTE %q is not supported", name)
		}
		e.visiting = true
		child, schema, err := bc.buildStatement(e.query)
		e.visiting = false
		if err != nil {
			return nil, engine.Schema{}, fmt.Errorf("CTE %q: %w", name, err)
		}
		e.mat = &cteMaterialization{Plan: child, Schema: schema}
	}
	return &Subquery{
		Child:  &CTERef{Name: name, Mat: e.mat},
		Schema: e.mat.Schema,
		Alias:  alias,
	}, e.mat.Schema, nil
}

// buildView expands a referenced view (CREATE VIEW) inline, mirroring buildCTE:
// the view's query is planned once per build (cached in bc.viewMat) and every
// reference shares the materialization, so a view used several times runs once
// and presents a consistent snapshot — an externally-visible CTE. The view binds
// in the global scope (sources + other views), not the referencing query's CTEs,
// so the outer WITH clause is hidden while planning the view body. A self- or
// cyclic reference is rejected via the visiting guard.
func (bc *buildCtx) buildView(name string, query sql.Statement, alias string) (Node, engine.Schema, error) {
	if bc.viewMat == nil {
		bc.viewMat = map[string]*cteEntry{}
	}
	e := bc.viewMat[name]
	if e == nil {
		e = &cteEntry{query: query}
		bc.viewMat[name] = e
	}
	if e.mat == nil {
		if e.visiting {
			return nil, engine.Schema{}, fmt.Errorf("view %q references itself (recursive views are not supported)", name)
		}
		e.visiting = true
		savedCtes := bc.ctes // a view does not see the referencing query's CTEs
		bc.ctes = nil
		child, schema, err := bc.buildStatement(e.query)
		bc.ctes = savedCtes
		e.visiting = false
		if err != nil {
			return nil, engine.Schema{}, fmt.Errorf("view %q: %w", name, err)
		}
		e.mat = &cteMaterialization{Plan: child, Schema: schema}
	}
	return &Subquery{
		Child:  &CTERef{Name: name, Mat: e.mat, IsView: true},
		Schema: e.mat.Schema,
		Alias:  alias,
	}, e.mat.Schema, nil
}

// BuildOption configures a Plan during Build.
type BuildOption func(*Plan)

// WithStrict enables strict (hard-error) type coercion for the plan.
func WithStrict() BuildOption {
	return func(p *Plan) { p.Strict = true }
}

// IfStrict returns WithStrict() when enabled is true, else nil options. A
// convenience for call sites that always spread the result.
func IfStrict(enabled bool) []BuildOption {
	if enabled {
		return []BuildOption{WithStrict()}
	}
	return nil
}

// resolveInSubqueries walks an expression and folds every non-correlated
// `x IN (SELECT ...)` into a literal value list by executing the subquery once.
// It mutates InExpr nodes in place; other shapes are traversed so nested INs are
// handled. A nil expression is a no-op.
func (bc *buildCtx) resolveInSubqueries(e sql.Expr) error {
	switch ex := e.(type) {
	case nil:
		return nil
	case *sql.BinaryOp:
		if err := bc.resolveInSubqueries(ex.Left); err != nil {
			return err
		}
		return bc.resolveInSubqueries(ex.Right)
	case *sql.UnaryOp:
		return bc.resolveInSubqueries(ex.Expr)
	case *sql.InExpr:
		if err := bc.resolveInSubqueries(ex.Expr); err != nil {
			return err
		}
		for _, it := range ex.List {
			if err := bc.resolveInSubqueries(it); err != nil {
				return err
			}
		}
		if ex.Subquery != nil {
			list, err := bc.evalSubqueryColumn(ex.Subquery)
			if err != nil {
				return err
			}
			ex.List = list
			ex.Subquery = nil
		}
		return nil
	case *sql.BetweenExpr:
		if err := bc.resolveInSubqueries(ex.Expr); err != nil {
			return err
		}
		if err := bc.resolveInSubqueries(ex.Low); err != nil {
			return err
		}
		return bc.resolveInSubqueries(ex.High)
	case *sql.LikeExpr:
		if err := bc.resolveInSubqueries(ex.Expr); err != nil {
			return err
		}
		return bc.resolveInSubqueries(ex.Pat)
	case *sql.IsNullExpr:
		return bc.resolveInSubqueries(ex.Expr)
	case *sql.FuncCall:
		for _, a := range ex.Args {
			if err := bc.resolveInSubqueries(a); err != nil {
				return err
			}
		}
		return nil
	case *sql.CaseExpr:
		for _, w := range ex.Whens {
			if err := bc.resolveInSubqueries(w.Cond); err != nil {
				return err
			}
			if err := bc.resolveInSubqueries(w.Then); err != nil {
				return err
			}
		}
		return bc.resolveInSubqueries(ex.Else)
	case *sql.CastExpr:
		return bc.resolveInSubqueries(ex.Expr)
	case *sql.ExtractExpr:
		return bc.resolveInSubqueries(ex.Source)
	}
	return nil
}

// evalSubqueryColumn executes an IN subquery and returns its single column's
// distinct values as literal expressions. The subquery must be non-correlated
// (it is planned and run independently) and produce exactly one column of a
// scalar type.
func (bc *buildCtx) evalSubqueryColumn(sub *sql.SelectStmt) ([]sql.Expr, error) {
	root, schema, err := bc.buildSelect(sub)
	if err != nil {
		return nil, fmt.Errorf("IN subquery: %w", err)
	}
	if len(schema.Columns) != 1 {
		return nil, fmt.Errorf("subquery in IN must return exactly one column, got %d", len(schema.Columns))
	}
	it, _, err := Exec(bc.ctx, &Plan{Root: root, OutputSchema: schema, Funcs: bc.funcs})
	if err != nil {
		return nil, fmt.Errorf("IN subquery: %w", err)
	}
	rows, err := engine.Materialize(bc.ctx, it)
	if err != nil {
		return nil, fmt.Errorf("IN subquery: %w", err)
	}
	list := make([]sql.Expr, 0, len(rows))
	seen := make(map[string]bool, len(rows))
	for _, r := range rows {
		if len(r.Values) == 0 {
			continue
		}
		v := r.Values[0]
		if v.IsNull() {
			continue // a NULL adds no matchable value to an IN list
		}
		// Dedup by string form; all values share one column type, so this is safe.
		key := v.AsString()
		if seen[key] {
			continue
		}
		seen[key] = true
		lit, err := valueToLiteral(v)
		if err != nil {
			return nil, err
		}
		list = append(list, lit)
	}
	return list, nil
}

// valueToLiteral converts a scalar engine value into the matching literal AST
// node. Non-scalar types (time, structured) are rejected rather than silently
// coerced, which would drop or mis-match rows.
func valueToLiteral(v engine.Value) (sql.Expr, error) {
	switch v.Type {
	case engine.TypeInt:
		n, _ := v.AsInt()
		return &sql.LitInt{V: n}, nil
	case engine.TypeFloat:
		f, _ := v.AsFloat()
		return &sql.LitFloat{V: f}, nil
	case engine.TypeString:
		return &sql.LitString{V: v.AsString()}, nil
	case engine.TypeBool:
		b, _ := v.AsBool()
		return &sql.LitBool{V: b}, nil
	default:
		return nil, fmt.Errorf("IN subquery: unsupported value type %s (supported: int, float, string, bool)", v.Type)
	}
}

// columnOrderTerms converts ORDER BY terms to connector order hints, but only
// if every term is a plain column reference (the connector OrderTerm carries
// just a column name + direction). ok is false if any term is an expression or
// alias, so the caller pushes the whole ORDER BY or none of it.
func columnOrderTerms(order []sql.OrderTerm) ([]connector.OrderTerm, bool) {
	out := make([]connector.OrderTerm, 0, len(order))
	for _, t := range order {
		cr, ok := t.Expr.(*sql.ColRef)
		if !ok {
			return nil, false
		}
		out = append(out, connector.OrderTerm{Column: cr.Name, Desc: t.Desc})
	}
	return out, true
}

// buildSelect builds the plan for a SELECT, returning the root node and the
// output schema (after projection).
func (bc *buildCtx) buildSelect(stmt *sql.SelectStmt) (Node, engine.Schema, error) {
	// 1. FROM + JOINs -> base relation schema with aliases.
	base, baseSchema, err := bc.buildFrom(stmt)
	if err != nil {
		return nil, engine.Schema{}, err
	}

	// Decorrelate top-level [NOT] EXISTS conjuncts into semi-/anti-joins (one
	// hash pass vs. the per-row Apply). Runs before subquery detection so a fully
	// decorrelated EXISTS leaves no subquery behind (and can coexist with GROUP
	// BY). The output schema is unchanged (the join emits only the left columns).
	base, err = bc.decorrelateExists(stmt, base, baseSchema)
	if err != nil {
		return nil, engine.Schema{}, err
	}

	// Determine if this is an aggregate query (GROUP BY present, or any
	// aggregate function in the select list). Needed for pushdown gating below.
	hasAgg := len(stmt.GroupBy) > 0
	if !hasAgg {
		for _, it := range stmt.Items.Items {
			if exprHasAgg(it.Expr) {
				hasAgg = true
				break
			}
		}
	}

	// Window functions in the select list or ORDER BY get their own stage.
	hasWindow := false
	for _, it := range stmt.Items.Items {
		if exprHasWindow(it.Expr) {
			hasWindow = true
			break
		}
	}
	if !hasWindow {
		for _, ot := range stmt.OrderBy {
			if exprHasWindow(ot.Expr) {
				hasWindow = true
				break
			}
		}
	}
	if hasWindow && hasAgg {
		return nil, engine.Schema{}, fmt.Errorf("window functions combined with GROUP BY/aggregates are not yet supported")
	}

	// Subquery expressions (EXISTS, scalar subqueries, correlated IN) are lifted
	// into an Apply node that computes a value column per outer row. The Apply
	// sits below the WHERE Filter (and so below any GROUP BY/aggregate/window
	// stage), so subqueries in WHERE compose with grouping/windows. Subqueries in
	// the SELECT list or ORDER BY are evaluated post-aggregation, which the
	// below-aggregate Apply can't express, so those stay unsupported when
	// combined with grouping/windows. (Runs before IN-folding so non-correlated
	// INs still fold to literals.)
	whereHasSubquery := exprHasSubquery(stmt.Where)
	projHasSubquery := false
	for _, it := range stmt.Items.Items {
		if !it.Star && exprHasSubquery(it.Expr) {
			projHasSubquery = true
			break
		}
	}
	if !projHasSubquery {
		for _, ot := range stmt.OrderBy {
			if exprHasSubquery(ot.Expr) {
				projHasSubquery = true
				break
			}
		}
	}
	if (hasAgg || hasWindow) && projHasSubquery {
		return nil, engine.Schema{}, fmt.Errorf("subqueries in the SELECT list or ORDER BY combined with GROUP BY/aggregates/window functions are not yet supported (subqueries in WHERE are supported)")
	}
	if whereHasSubquery || projHasSubquery {
		base, baseSchema, err = bc.buildSubqueries(stmt, base, baseSchema)
		if err != nil {
			return nil, engine.Schema{}, err
		}
	}

	// Fold any remaining (non-correlated) `x IN (SELECT ...)` in WHERE/HAVING into
	// a literal value list by executing the subquery once. Doing this before
	// pushdown means the resolved IN list can also be pushed to a capable
	// connector. The engine sees an ordinary IN-list predicate.
	if err := bc.resolveInSubqueries(stmt.Where); err != nil {
		return nil, engine.Schema{}, err
	}
	if err := bc.resolveInSubqueries(stmt.Having); err != nil {
		return nil, engine.Schema{}, err
	}

	// Resolve positional ORDER BY / GROUP BY references (e.g. `ORDER BY 2`) to
	// the matching select item before they reach Sort/Aggregate, so an integer
	// term sorts/groups by that output column instead of being a silent no-op.
	if err := resolveOrdinals(stmt); err != nil {
		return nil, engine.Schema{}, err
	}

	// Aggregate pushdown: a single-scan GROUP BY/aggregate query whose connector
	// is an AggregatePusher may compute the whole aggregation — group-by,
	// aggregates and WHERE — at the source. On success the Scan emits aggregated
	// rows and the engine adds no Aggregate or WHERE-Filter; HAVING/ORDER BY/LIMIT
	// and the projection run above it as usual.
	if hasAgg && !hasWindow {
		if scan, ok := base.(*Scan); ok {
			root, sch, pushed, err := bc.buildPushedAggregate(stmt, scan)
			if err != nil {
				return nil, engine.Schema{}, err
			}
			if pushed {
				if stmt.Limit != nil || stmt.Offset != nil {
					root = &Limit{Child: root, Limit: stmt.Limit, Offset: stmt.Offset}
				}
				return root, sch, nil
			}
		}
	}

	// 2. Pushdown hints: for a single-table scan (no joins) hand the WHERE
	//    predicate, and a safe LIMIT, to the connector. This is an optimization
	//    only — the engine's Filter/Limit below still run, so a connector that
	//    ignores or partially honors the request stays correct; capable
	//    connectors (sql databases, Azure Tables) just fetch fewer rows.
	if scan, ok := base.(*Scan); ok {
		scan.Predicate = stmt.Where
		// A LIMIT can only be pushed when nothing between the scan and the
		// engine's Limit changes the row count or order: no ORDER BY (needs all
		// rows to sort), no aggregation (needs all rows to group), no OFFSET.
		if stmt.Limit != nil && stmt.Offset == nil && len(stmt.OrderBy) == 0 && !hasAgg && !hasWindow {
			scan.Limit = stmt.Limit
		}
		// ORDER BY is offered to the connector only when every term is a plain
		// column (the connector OrderTerm is column+direction) and there is no
		// aggregation. It is a hint — the engine's Sort re-orders — so a
		// connector may honor it to choose which rows survive a cap.
		if len(stmt.OrderBy) > 0 && !hasAgg {
			if terms, ok := columnOrderTerms(stmt.OrderBy); ok {
				scan.OrderBy = terms
			}
		}
	}

	// 3. WHERE -> Filter (always applied by the engine; pushdown above is a
	//    superset optimization, so re-filtering here keeps results correct).
	if stmt.Where != nil {
		base = &Filter{Child: base, Predicate: stmt.Where}
	}

	var root Node
	var outSchema engine.Schema

	if hasAgg {
		// Aggregate, then (optionally) Sort over its output, then Project. The
		// aggregate builder rewrites ORDER BY/HAVING to reference aggregate output
		// columns, so a Sort by an aggregate or a scalar-of-aggregate works.
		aggNode, outs, projSchema, orderTerms, err := bc.buildAggregate(stmt, base, baseSchema)
		if err != nil {
			return nil, engine.Schema{}, err
		}
		projectBase := Node(aggNode)
		if len(orderTerms) > 0 {
			projectBase = &Sort{Child: projectBase, Terms: orderTerms}
		}
		root = &Project{Child: projectBase, Outputs: outs, Distinct: stmt.Distinct}
		outSchema = projSchema
	} else if hasWindow {
		// Window stage: compute window columns over the (post-WHERE) rows, then
		// Sort, then Project. Window calls in the select list and ORDER BY are
		// rewritten to reference the appended $winN columns.
		node, outs, projSchema, orderTerms, err := bc.buildWindow(stmt, base, baseSchema)
		if err != nil {
			return nil, engine.Schema{}, err
		}
		projectBase := node
		if len(orderTerms) > 0 {
			projectBase = &Sort{Child: projectBase, Terms: orderTerms}
		}
		root = &Project{Child: projectBase, Outputs: outs, Distinct: stmt.Distinct}
		outSchema = projSchema
	} else {
		// ORDER BY runs before Project so it can reference input columns that are
		// not in the select list.
		projectBase := base
		if len(stmt.OrderBy) > 0 {
			projectBase = &Sort{Child: projectBase, Terms: stmt.OrderBy}
		}
		outs, sch, err := bc.buildProjection(stmt, baseSchema)
		if err != nil {
			return nil, engine.Schema{}, err
		}
		root = &Project{Child: projectBase, Outputs: outs, Distinct: stmt.Distinct}
		outSchema = sch
	}

	// 6. LIMIT/OFFSET.
	if stmt.Limit != nil || stmt.Offset != nil {
		root = &Limit{Child: root, Limit: stmt.Limit, Offset: stmt.Offset}
	}

	return root, outSchema, nil
}

// buildFrom resolves the FROM table ref and any JOINs into a base node + the
// combined schema. Each side gets an alias (explicit or the table name).
func (bc *buildCtx) buildFrom(stmt *sql.SelectStmt) (Node, engine.Schema, error) {
	if stmt.NoFrom {
		// A single row with no columns; projections evaluate against an
		// empty row (literals/functions only, no column refs allowed).
		return &NoFrom{}, engine.Schema{}, nil
	}
	leftNode, leftSchema, leftAlias, err := bc.buildTableRef(stmt.From)
	if err != nil {
		return nil, engine.Schema{}, err
	}
	aliases := []engine.AliasRange{
		{Alias: leftAlias, Start: 0, End: len(leftSchema.Columns)},
	}
	schema := leftSchema
	for _, j := range stmt.Joins {
		rightNode, rightSchema, rightAlias, err := bc.buildTableRef(j.Ref)
		if err != nil {
			return nil, engine.Schema{}, err
		}
		lks, rks, residual, err := bc.splitJoin(j.On, schema, aliases, rightSchema, rightAlias)
		if err != nil {
			return nil, engine.Schema{}, err
		}
		combined := engine.Schema{Columns: append(append([]engine.Column{}, schema.Columns...), rightSchema.Columns...)}
		aliases = append(aliases, engine.AliasRange{
			Alias: rightAlias, Start: len(schema.Columns), End: len(combined.Columns),
		})
		leftNode = &Join{
			Kind: j.Kind, Left: leftNode, Right: rightNode,
			LeftKeys: lks, RightKeys: rks, Residual: residual, Schema: combined,
			Aliases: aliases,
		}
		schema = combined
		leftAlias = "" // only the first join split uses explicit left/right aliases
	}
	return leftNode, schema, nil
}

// buildTableRef resolves a single FROM/JOIN table reference into a plan node,
// applying an optional column-rename list (AS alias(c1, c2, …)) by presenting
// the source's rows through a Subquery under the renamed schema. The rename is
// positional, so it works uniformly for base tables, derived tables, and table
// functions (a base table loses predicate pushdown when renamed, which is fine —
// the engine re-applies the filter).
func (bc *buildCtx) buildTableRef(tr sql.TableRef) (Node, engine.Schema, string, error) {
	node, schema, alias, err := bc.buildTableRefRaw(tr)
	if err != nil || len(tr.ColAliases) == 0 {
		return node, schema, alias, err
	}
	renamed, err := renameColumns(schema, tr.ColAliases)
	if err != nil {
		return nil, engine.Schema{}, "", fmt.Errorf("column aliases for %q: %w", alias, err)
	}
	return &Subquery{Child: node, Schema: renamed, Alias: alias}, renamed, alias, nil
}

// renameColumns returns a copy of schema with its leading columns renamed to
// names (which must not outnumber the columns); trailing columns keep their
// original names.
func renameColumns(schema engine.Schema, names []string) (engine.Schema, error) {
	if len(names) > len(schema.Columns) {
		return engine.Schema{}, fmt.Errorf("%d column alias(es) but the source has %d column(s)", len(names), len(schema.Columns))
	}
	cols := make([]engine.Column, len(schema.Columns))
	copy(cols, schema.Columns)
	for i, n := range names {
		cols[i].Name = n
	}
	return engine.Schema{Columns: cols}, nil
}

// buildTableRefRaw resolves a FROM/JOIN reference into a Scan/Subquery/TableFunc
// node, before any column-rename list is applied.
func (bc *buildCtx) buildTableRefRaw(tr sql.TableRef) (Node, engine.Schema, string, error) {
	if tr.Func != nil {
		return bc.buildTableFunc(tr)
	}
	if tr.Subquery != nil {
		alias := tr.Alias
		if alias == "" {
			return nil, engine.Schema{}, "", fmt.Errorf("subquery in FROM must have an alias")
		}
		var (
			child  Node
			schema engine.Schema
			err    error
		)
		switch sub := tr.Subquery.(type) {
		case *sql.SelectStmt:
			child, schema, err = bc.buildSelect(sub)
		case *sql.SetOpStmt:
			child, schema, err = bc.buildSetOp(sub)
		default:
			return nil, engine.Schema{}, "", fmt.Errorf("unsupported subquery %T", sub)
		}
		if err != nil {
			return nil, engine.Schema{}, "", fmt.Errorf("subquery %q: %w", alias, err)
		}
		return &Subquery{Child: child, Schema: schema, Alias: alias}, schema, alias, nil
	}
	// A bare name may reference a WITH-clause CTE (which shadows a registered
	// source or view of the same name).
	if tr.Prefix == "" && tr.Name != "" && bc.ctes != nil {
		if _, ok := bc.ctes[tr.Name]; ok {
			alias := tr.Alias
			if alias == "" {
				alias = tr.Name
			}
			node, schema, err := bc.buildCTE(tr.Name, alias)
			if err != nil {
				return nil, engine.Schema{}, "", err
			}
			return node, schema, alias, nil
		}
	}
	// A bare name may reference a registered view, expanded inline like a CTE.
	if tr.Prefix == "" && tr.Name != "" {
		if q, ok := bc.reg.View(tr.Name); ok {
			alias := tr.Alias
			if alias == "" {
				alias = tr.Name
			}
			node, schema, err := bc.buildView(tr.Name, q, alias)
			if err != nil {
				return nil, engine.Schema{}, "", err
			}
			return node, schema, alias, nil
		}
	}
	src, err := resolveTableRef(bc.ctx, tr, bc.reg)
	if err != nil {
		return nil, engine.Schema{}, "", err
	}
	schema, err := src.Conn.Resolve(bc.ctx, src.Dataset)
	if err != nil {
		return nil, engine.Schema{}, "", fmt.Errorf("resolve schema for %q: %w", src.Name, err)
	}
	alias := tr.Alias
	if alias == "" {
		alias = tr.Name
		if alias == "" {
			alias = tr.Source
		}
	}
	return &Scan{Source: src, Schema: schema, Alias: alias}, schema, alias, nil
}

// buildTableFunc resolves a FROM-clause table function. generate_series(start,
// stop[, step]) produces a one-column relation: an integer series, or a
// timestamp series when start/stop are timestamps and step is an INTERVAL. The
// (constant) arguments are evaluated at plan time so the schema type is known.
func (bc *buildCtx) buildTableFunc(tr sql.TableRef) (Node, engine.Schema, string, error) {
	fc := tr.Func
	alias := tr.Alias
	if alias == "" {
		alias = strings.ToLower(fc.Name)
	}
	if !strings.EqualFold(fc.Name, "generate_series") {
		return nil, engine.Schema{}, "", fmt.Errorf("unknown table function %q", fc.Name)
	}
	if len(fc.Args) < 2 || len(fc.Args) > 3 {
		return nil, engine.Schema{}, "", fmt.Errorf("generate_series expects (start, stop[, step])")
	}
	ev := engine.Evaluator{Funcs: engine.NewFuncRegistry(), Resolve: func(string, string) int { return -1 }}
	vals := make([]engine.Value, len(fc.Args))
	for i, a := range fc.Args {
		v, err := ev.Eval(a, engine.Row{})
		if err != nil {
			return nil, engine.Schema{}, "", fmt.Errorf("generate_series argument %d: %w", i+1, err)
		}
		if v.IsNull() {
			return nil, engine.Schema{}, "", fmt.Errorf("generate_series arguments must not be NULL")
		}
		vals[i] = v
	}

	node := &TableFunc{Alias: alias}
	if vals[0].Type == engine.TypeTime {
		start, _ := vals[0].V.(time.Time)
		stop, ok := vals[1].V.(time.Time)
		if !ok {
			return nil, engine.Schema{}, "", fmt.Errorf("generate_series: stop must be a timestamp like start")
		}
		if len(fc.Args) != 3 {
			return nil, engine.Schema{}, "", fmt.Errorf("a timestamp generate_series needs an INTERVAL step")
		}
		step, ok := vals[2].V.(time.Duration)
		if !ok || step == 0 {
			return nil, engine.Schema{}, "", fmt.Errorf("generate_series step must be a non-zero INTERVAL")
		}
		node.IsTime, node.TimeStart, node.TimeStop, node.TimeStep = true, start, stop, step
		node.Schema = engine.Schema{Columns: []engine.Column{{Name: "value", Type: engine.TypeTime, Nullable: false}}}
	} else {
		start, ok1 := vals[0].AsInt()
		stop, ok2 := vals[1].AsInt()
		if !ok1 || !ok2 {
			return nil, engine.Schema{}, "", fmt.Errorf("generate_series start/stop must be integers or timestamps")
		}
		step := int64(1)
		if len(fc.Args) == 3 {
			s, ok := vals[2].AsInt()
			if !ok || s == 0 {
				return nil, engine.Schema{}, "", fmt.Errorf("generate_series step must be a non-zero integer")
			}
			step = s
		}
		node.IntStart, node.IntStop, node.IntStep = start, stop, step
		node.Schema = engine.Schema{Columns: []engine.Column{{Name: "value", Type: engine.TypeInt, Nullable: false}}}
	}
	return node, node.Schema, alias, nil
}

// splitJoin analyzes a JOIN ON expression. Each top-level AND conjunct of the
// form leftcol = rightcol (one column resolving to the already-combined left
// side, the other to the right side being joined) becomes a hash-key pair; every
// other conjunct — a non-equality comparison, an equality over expressions or
// literals, or one touching a single side — is collected into a residual
// predicate the engine evaluates on each candidate pair. With at least one key
// pair the join hashes on the composite key; with none it runs as a nested loop
// applying the residual to every pair. Key indices are into the combined schema
// (left) / the new right schema. leftKeys and rightKeys are returned in parallel.
func (bc *buildCtx) splitJoin(on sql.Expr, leftSchema engine.Schema, leftAliases []engine.AliasRange, rightSchema engine.Schema, rightAlias string) (leftKeys, rightKeys []engine.KeyExtractor, residual sql.Expr, err error) {
	var resid []sql.Expr
	for _, c := range splitConjuncts(on) {
		lIdx, rIdx, ok := bc.equiKey(c, leftSchema, leftAliases, rightSchema, rightAlias)
		if !ok {
			resid = append(resid, c)
			continue
		}
		li, ri := lIdx, rIdx
		leftKeys = append(leftKeys, func(row engine.Row) engine.Value {
			if li >= 0 && li < len(row.Values) {
				return row.Values[li]
			}
			return engine.Null()
		})
		rightKeys = append(rightKeys, func(row engine.Row) engine.Value {
			if ri >= 0 && ri < len(row.Values) {
				return row.Values[ri]
			}
			return engine.Null()
		})
	}
	if len(leftKeys) == 0 && len(resid) == 0 {
		return nil, nil, nil, fmt.Errorf("JOIN ON has no usable condition")
	}
	return leftKeys, rightKeys, andConjuncts(resid), nil
}

// equiKey reports whether conjunct c is `leftcol = rightcol` with the columns on
// opposite join sides and, if so, returns the left/right column indices.
func (bc *buildCtx) equiKey(c sql.Expr, leftSchema engine.Schema, leftAliases []engine.AliasRange, rightSchema engine.Schema, rightAlias string) (int, int, bool) {
	bin, ok := c.(*sql.BinaryOp)
	if !ok || bin.Op != "=" {
		return 0, 0, false
	}
	lIdx, rIdx, err := bc.classifyJoinOperands(bin.Left, bin.Right, leftSchema, leftAliases, rightSchema, rightAlias)
	if err != nil {
		return 0, 0, false
	}
	return lIdx, rIdx, true
}

// classifyJoinOperands determines which operand belongs to which side and
// returns the left-side column index and right-side column index.
func (bc *buildCtx) classifyJoinOperands(a, b sql.Expr, leftSchema engine.Schema, leftAliases []engine.AliasRange, rightSchema engine.Schema, rightAlias string) (int, int, error) {
	ai, aSide := bc.colSide(a, leftSchema, leftAliases, rightSchema, rightAlias)
	bi, bSide := bc.colSide(b, leftSchema, leftAliases, rightSchema, rightAlias)
	// Assign: one must be left, the other right.
	var lIdx, rIdx int = -1, -1
	switch {
	case aSide == "left" && bSide == "right":
		lIdx, rIdx = ai, bi
	case aSide == "right" && bSide == "left":
		lIdx, rIdx = bi, ai
	default:
		return -1, -1, fmt.Errorf("JOIN ON must compare a left column to a right column")
	}
	return lIdx, rIdx, nil
}

// colSide resolves a ColRef to a column index on either the left (combined)
// side or the right side, returning (index, "left"/"right"). Unqualified refs
// match against whichever side contains the column.
func (bc *buildCtx) colSide(expr sql.Expr, leftSchema engine.Schema, leftAliases []engine.AliasRange, rightSchema engine.Schema, rightAlias string) (int, string) {
	cr, ok := expr.(*sql.ColRef)
	if !ok {
		return -1, ""
	}
	if cr.Qualifier != "" {
		// Try the left combined side's aliases first.
		for _, ar := range leftAliases {
			if !strings.EqualFold(cr.Qualifier, ar.Alias) {
				continue
			}
			for i := ar.Start; i < ar.End && i < len(leftSchema.Columns); i++ {
				if strings.EqualFold(leftSchema.Columns[i].Name, cr.Name) {
					return i, "left"
				}
			}
		}
		if strings.EqualFold(cr.Qualifier, rightAlias) {
			if i := rightSchema.Index(cr.Name); i >= 0 {
				return i, "right"
			}
		}
		return -1, ""
	}
	if i := leftSchema.Index(cr.Name); i >= 0 {
		return i, "left"
	}
	if i := rightSchema.Index(cr.Name); i >= 0 {
		return i, "right"
	}
	return -1, ""
}

// resolveOrdinals rewrites positional references in GROUP BY and ORDER BY (a
// bare integer literal N, 1-based, refers to the N-th select item) into that
// item's expression. An out-of-range or `*` position is an error rather than a
// silent no-op.
func resolveOrdinals(stmt *sql.SelectStmt) error {
	items := stmt.Items.Items
	itemFor := func(clause string, n int) (sql.SelectItem, error) {
		if n < 1 || n > len(items) {
			return sql.SelectItem{}, fmt.Errorf("%s position %d is out of range (1..%d)", clause, n, len(items))
		}
		it := items[n-1]
		if it.Star {
			return sql.SelectItem{}, fmt.Errorf("%s position %d refers to '*'", clause, n)
		}
		return it, nil
	}
	for i, e := range stmt.GroupBy {
		li, ok := e.(*sql.LitInt)
		if !ok {
			continue
		}
		it, err := itemFor("GROUP BY", int(li.V))
		if err != nil {
			return err
		}
		stmt.GroupBy[i] = it.Expr
	}
	for i := range stmt.OrderBy {
		li, ok := stmt.OrderBy[i].Expr.(*sql.LitInt)
		if !ok {
			continue
		}
		it, err := itemFor("ORDER BY", int(li.V))
		if err != nil {
			return err
		}
		// Substitute the select item's expression. For an aggregate query
		// buildAggregate rewrites this (and the rest of ORDER BY) to reference the
		// aggregate output, so ordinals onto aggregates and scalars-of-aggregates
		// resolve too.
		stmt.OrderBy[i].Expr = it.Expr
	}
	return nil
}

// aggExtractor pulls aggregate calls out of expressions. Each call it finds is
// appended to specs/cols as an aggregate output column; rewrite() returns the
// expression with every aggregate replaced by a reference to that column, so a
// later projection can evaluate scalar wrappers (e.g. ROUND(STDDEV(x), 2)) over
// the aggregate's output. A top-level aggregate select item keeps its alias as
// the column name (so HAVING/ORDER BY can reference it); nested aggregates get a
// synthetic "$aggN" name a user identifier can never collide with.
type aggExtractor struct {
	specs []engine.AggSpec
	cols  []engine.Column
	in    engine.Schema // input schema, for inferring aggregate argument types
	n     int
}

func (x *aggExtractor) register(fc *sql.FuncCall, name string) sql.Expr {
	var arg, arg2 sql.Expr
	if len(fc.Args) >= 1 {
		arg = fc.Args[0]
	}
	if len(fc.Args) >= 2 {
		arg2 = fc.Args[1] // e.g. the STRING_AGG delimiter
	}
	x.specs = append(x.specs, engine.AggSpec{Func: fc.Name, Arg: arg, Arg2: arg2, Name: name, Distinct: fc.Distinct})
	x.cols = append(x.cols, engine.Column{Name: name, Type: aggregateType(fc, x.in), Nullable: true})
	return &sql.ColRef{Name: name}
}

func (x *aggExtractor) synth() string {
	name := fmt.Sprintf("$agg%d", x.n)
	x.n++
	return name
}

// rewrite returns a copy of e with each aggregate call replaced by a ColRef to
// its (synthetically named) output column. Aggregate arguments are left intact:
// they are evaluated by the AggregateIter over the input rows, not here.
func (x *aggExtractor) rewrite(e sql.Expr) sql.Expr {
	switch ex := e.(type) {
	case *sql.FuncCall:
		if engine.IsAggregate(ex.Name) && ex.Over == nil {
			return x.register(ex, x.synth())
		}
		args := make([]sql.Expr, len(ex.Args))
		for i, a := range ex.Args {
			args[i] = x.rewrite(a)
		}
		return &sql.FuncCall{Name: ex.Name, Args: args, Distinct: ex.Distinct, Over: ex.Over}
	case *sql.BinaryOp:
		return &sql.BinaryOp{Op: ex.Op, Left: x.rewrite(ex.Left), Right: x.rewrite(ex.Right)}
	case *sql.UnaryOp:
		return &sql.UnaryOp{Op: ex.Op, Expr: x.rewrite(ex.Expr)}
	case *sql.InExpr:
		list := make([]sql.Expr, len(ex.List))
		for i, v := range ex.List {
			list[i] = x.rewrite(v)
		}
		return &sql.InExpr{Expr: x.rewrite(ex.Expr), List: list, Subquery: ex.Subquery, Negate: ex.Negate}
	case *sql.BetweenExpr:
		return &sql.BetweenExpr{Expr: x.rewrite(ex.Expr), Low: x.rewrite(ex.Low), High: x.rewrite(ex.High), Negate: ex.Negate}
	case *sql.LikeExpr:
		return &sql.LikeExpr{Expr: x.rewrite(ex.Expr), Pat: x.rewrite(ex.Pat), Negate: ex.Negate, Insensitive: ex.Insensitive}
	case *sql.IsNullExpr:
		return &sql.IsNullExpr{Expr: x.rewrite(ex.Expr), Negate: ex.Negate}
	case *sql.CaseExpr:
		whens := make([]sql.CaseWhen, len(ex.Whens))
		for i, w := range ex.Whens {
			whens[i] = sql.CaseWhen{Cond: x.rewrite(w.Cond), Then: x.rewrite(w.Then)}
		}
		var els sql.Expr
		if ex.Else != nil {
			els = x.rewrite(ex.Else)
		}
		return &sql.CaseExpr{Whens: whens, Else: els}
	case *sql.CastExpr:
		return &sql.CastExpr{Expr: x.rewrite(ex.Expr), Type: ex.Type}
	case *sql.ExtractExpr:
		return &sql.ExtractExpr{Field: ex.Field, Source: x.rewrite(ex.Source)}
	case *sql.PositionExpr:
		return &sql.PositionExpr{Substr: x.rewrite(ex.Substr), Str: x.rewrite(ex.Str)}
	}
	// Literals and column refs are returned unchanged.
	return e
}

// buildAggregate constructs the Aggregate node plus the projection that runs on
// top of it. Aggregate calls anywhere in the SELECT list, HAVING, and ORDER BY
// are extracted into aggregate output columns; the remaining (aggregate-free)
// select items are the group keys. The Aggregate emits rows as
// [group keys..., aggregates...]; the returned projection restores SELECT-list
// order and evaluates any scalar wrappers around aggregates. The returned order
// terms are rewritten to reference the aggregate output and are applied (as a
// Sort) between the Aggregate and the projection by the caller.
func (bc *buildCtx) buildAggregate(stmt *sql.SelectStmt, base Node, baseSchema engine.Schema) (Node, []engine.ProjectedExpr, engine.Schema, []sql.OrderTerm, error) {
	ext := &aggExtractor{in: baseSchema}
	var keys []sql.Expr
	var keyCols []engine.Column
	var outs []engine.ProjectedExpr

	for _, it := range stmt.Items.Items {
		if it.Star {
			return nil, nil, engine.Schema{}, nil, fmt.Errorf("SELECT * not valid with aggregation; name columns explicitly")
		}
		name := it.As
		if name == "" {
			name = inferExprName(it.Expr)
		}
		switch {
		case isTopLevelAgg(it.Expr):
			// The whole item is an aggregate: name its column by the item's alias
			// so HAVING/ORDER BY referencing that alias resolve directly.
			ext.register(it.Expr.(*sql.FuncCall), name)
			outs = append(outs, engine.ProjectedExpr{Expr: &sql.ColRef{Name: name}, Name: name})
		case exprHasAgg(it.Expr):
			// A scalar expression wrapping one or more aggregates.
			outs = append(outs, engine.ProjectedExpr{Expr: ext.rewrite(it.Expr), Name: name})
		default:
			// No aggregate: a group key. The projection reads the key column back.
			keys = append(keys, it.Expr)
			keyCols = append(keyCols, engine.Column{Name: name, Type: exprType(it.Expr, baseSchema), Nullable: true})
			outs = append(outs, engine.ProjectedExpr{Expr: &sql.ColRef{Name: name}, Name: name})
		}
	}

	having := stmt.Having
	if having != nil {
		having = ext.rewrite(having)
	}
	var orderTerms []sql.OrderTerm
	for _, ot := range stmt.OrderBy {
		orderTerms = append(orderTerms, sql.OrderTerm{Expr: ext.rewrite(ot.Expr), Desc: ot.Desc})
	}

	// The AggregateIter emits [group keys..., aggregates...]; the intermediate
	// schema must follow that layout (not SELECT-list order) so name-based reads
	// land on the right columns.
	interSchema := engine.Schema{Columns: append(append([]engine.Column{}, keyCols...), ext.cols...)}
	// The projection runs over the aggregate output; with keys and aggregates now
	// typed, infer each output column's type over that intermediate schema (a
	// plain key/aggregate reference passes through, scalar wrappers like
	// ROUND(STDDEV(x), 2) resolve via the function library).
	outCols := make([]engine.Column, len(outs))
	for i := range outs {
		outs[i].Type = exprType(outs[i].Expr, interSchema)
		outCols[i] = engine.Column{Name: outs[i].Name, Type: outs[i].Type, Nullable: true}
	}
	node := &Aggregate{Child: base, Keys: keys, Aggs: ext.specs, Having: having, Schema: interSchema}
	return node, outs, engine.Schema{Columns: outCols}, orderTerms, nil
}

// buildPushedAggregate attempts to push a single-scan GROUP BY/aggregate query
// into the connector when it implements connector.AggregatePusher. On success it
// returns the plan rooted at the aggregating Scan — the connector returns
// already-aggregated rows, and HAVING (Filter), ORDER BY (Sort) and the SELECT
// projection are layered above it over those rows, with no engine Aggregate and
// no WHERE-Filter (the connector consumed the predicate). ok=false means "not
// pushable" and the caller falls back to the engine's own aggregation over raw
// rows. A non-nil error is a hard planning failure.
//
// It is structural-only: it pushes plain-column group-by/aggregate-args and lets
// PushAggregate decide whether the specific operations are supported. Scalar
// wrappers around aggregates (e.g. ROUND(AVG(x), 2)) still work — the wrapper is
// evaluated by the engine's projection over the aggregated rows.
func (bc *buildCtx) buildPushedAggregate(stmt *sql.SelectStmt, scan *Scan) (Node, engine.Schema, bool, error) {
	pusher, ok := scan.Source.Conn.(connector.AggregatePusher)
	if !ok {
		return nil, engine.Schema{}, false, nil
	}
	// Breakdowns: every GROUP BY term must be a plain column.
	var groupBy []string
	for _, g := range stmt.GroupBy {
		col, ok := pushColumnName(g, scan.Alias)
		if !ok {
			return nil, engine.Schema{}, false, nil
		}
		groupBy = append(groupBy, col)
	}
	// Extract aggregates from SELECT / HAVING / ORDER BY into calculations,
	// rewriting each aggregate call to a reference to its output column. Compute
	// into locals: rewriteFuncs is pure, so stmt is untouched until we commit —
	// leaving the fallback path an unmodified statement if we decline.
	ext := newPushAggExtractor(scan.Alias)
	newItems := make([]sql.SelectItem, len(stmt.Items.Items))
	for i, it := range stmt.Items.Items {
		if it.Star {
			return nil, engine.Schema{}, false, nil
		}
		// A top-level aggregate select item names its output column by the item's
		// alias, so HAVING/ORDER BY can reference that alias (as buildAggregate
		// does). Other aggregates (nested, or in HAVING/ORDER BY) get synthetic
		// $aggN names, de-duplicated across clauses.
		if fc, ok := it.Expr.(*sql.FuncCall); ok && isTopLevelAgg(it.Expr) && it.As != "" {
			it.Expr = ext.register(fc, it.As)
		} else {
			it.Expr = ext.rewrite(it.Expr)
		}
		newItems[i] = it
	}
	newHaving := stmt.Having
	if newHaving != nil {
		newHaving = ext.rewrite(newHaving)
	}
	newOrder := make([]sql.OrderTerm, len(stmt.OrderBy))
	for i, ot := range stmt.OrderBy {
		ot.Expr = ext.rewrite(ot.Expr)
		newOrder[i] = ot
	}
	if !ext.ok {
		return nil, engine.Schema{}, false, nil
	}

	req := connector.AggregateRequest{GroupBy: groupBy, Aggregates: ext.ops, Predicate: stmt.Where}
	schema, ok, err := pusher.PushAggregate(bc.ctx, scan.Source.Dataset, req)
	if err != nil {
		return nil, engine.Schema{}, false, err
	}
	if !ok {
		return nil, engine.Schema{}, false, nil
	}

	// Commit: the Scan now emits aggregated rows and owns the predicate.
	stmt.Items.Items = newItems
	stmt.Having = newHaving
	stmt.OrderBy = newOrder
	scan.Aggregate = &req
	scan.Schema = schema
	scan.Predicate = nil

	var node Node = scan
	if stmt.Having != nil {
		node = &Filter{Child: node, Predicate: stmt.Having}
	}
	if len(stmt.OrderBy) > 0 {
		node = &Sort{Child: node, Terms: stmt.OrderBy}
	}
	outs, projSchema, err := bc.buildProjection(stmt, schema)
	if err != nil {
		return nil, engine.Schema{}, false, err
	}
	root := &Project{Child: node, Outputs: outs, Distinct: stmt.Distinct}
	return root, projSchema, true, nil
}

// pushColumnName renders a column reference as the source column name for
// aggregate pushdown, reconstructing a dotted attribute name ("service.name")
// when the qualifier is not the scan's own alias. ok=false for non-columns.
func pushColumnName(e sql.Expr, alias string) (string, bool) {
	cr, ok := e.(*sql.ColRef)
	if !ok {
		return "", false
	}
	if cr.Qualifier == "" || strings.EqualFold(cr.Qualifier, alias) {
		return cr.Name, true
	}
	return cr.Qualifier + "." + cr.Name, true
}

// pushAggExtractor decomposes aggregate calls into connector.AggregateOp entries,
// rewriting each call to a ColRef on its ($aggN) output column — the pushdown
// analogue of aggExtractor. It sets ok=false on an aggregate it cannot express
// as a plain-column calculation (an expression argument or more than one
// positional argument). Identical aggregates are de-duplicated to one column.
type pushAggExtractor struct {
	alias string
	ops   []connector.AggregateOp
	seen  map[string]string // op key -> output column name
	n     int
	ok    bool
}

func newPushAggExtractor(alias string) *pushAggExtractor {
	return &pushAggExtractor{alias: alias, seen: map[string]string{}, ok: true}
}

// register turns one aggregate call into an AggregateOp (de-duplicated by
// op+distinct+column) and returns a reference to its output column. wantAlias, if
// non-empty, names a fresh op's output column (a top-level select item's alias);
// otherwise a synthetic $aggN name is used. Sets ok=false for an aggregate whose
// argument is not a plain column.
func (x *pushAggExtractor) register(fc *sql.FuncCall, wantAlias string) sql.Expr {
	var col string
	switch len(fc.Args) {
	case 0: // COUNT() — no column
	case 1:
		// COUNT(*) parses as a single "*" column ref: treat as no column.
		if cr, ok := fc.Args[0].(*sql.ColRef); ok && cr.Name == "*" && cr.Qualifier == "" {
			break
		}
		c, ok := pushColumnName(fc.Args[0], x.alias)
		if !ok {
			x.ok = false
			return fc
		}
		col = c
	default:
		x.ok = false
		return fc
	}
	key := strings.ToUpper(fc.Name) + "|" + strconv.FormatBool(fc.Distinct) + "|" + col
	if name, dup := x.seen[key]; dup {
		return &sql.ColRef{Name: name}
	}
	name := wantAlias
	if name == "" {
		name = fmt.Sprintf("$agg%d", x.n)
		x.n++
	}
	x.seen[key] = name
	x.ops = append(x.ops, connector.AggregateOp{
		Func: strings.ToUpper(fc.Name), Column: col, Distinct: fc.Distinct, Alias: name,
	})
	return &sql.ColRef{Name: name}
}

func (x *pushAggExtractor) rewrite(e sql.Expr) sql.Expr {
	return rewriteFuncs(e, func(fc *sql.FuncCall) sql.Expr {
		if !engine.IsAggregate(fc.Name) || fc.Over != nil {
			return nil
		}
		return x.register(fc, "")
	})
}

// isTopLevelAgg reports whether e is itself an aggregate call (vs. a scalar
// expression that merely contains one).
func isTopLevelAgg(e sql.Expr) bool {
	fc, ok := e.(*sql.FuncCall)
	return ok && fc.Over == nil && engine.IsAggregate(fc.Name)
}

// buildWindow constructs the Window node plus the projection above it. Window
// calls in the SELECT list and ORDER BY are extracted into appended $winN
// columns; the returned projection (and rewritten ORDER BY terms) reference
// them. A plain (non-window) select item is projected as its own expression
// over the window output, which still carries every base column.
func (bc *buildCtx) buildWindow(stmt *sql.SelectStmt, base Node, baseSchema engine.Schema) (Node, []engine.ProjectedExpr, engine.Schema, []sql.OrderTerm, error) {
	win := &winExtractor{in: baseSchema}
	var outs []engine.ProjectedExpr
	for _, it := range stmt.Items.Items {
		if it.Star {
			// Expand to the base relation's columns (window outputs are named
			// select items, not part of *).
			for _, c := range baseSchema.Columns {
				outs = append(outs, engine.ProjectedExpr{Expr: &sql.ColRef{Name: c.Name}, Name: c.Name})
			}
			continue
		}
		name := it.As
		if name == "" {
			name = inferExprName(it.Expr)
		}
		outs = append(outs, engine.ProjectedExpr{Expr: win.rewrite(it.Expr), Name: name})
	}
	var orderTerms []sql.OrderTerm
	for _, ot := range stmt.OrderBy {
		orderTerms = append(orderTerms, sql.OrderTerm{Expr: win.rewrite(ot.Expr), Desc: ot.Desc})
	}
	winSchema := engine.Schema{Columns: append(append([]engine.Column{}, baseSchema.Columns...), win.cols...)}
	// The projection runs over the window output (base columns + $winN); with the
	// window columns now typed, infer each output column's type over that schema.
	outCols := make([]engine.Column, len(outs))
	for i := range outs {
		outs[i].Type = exprType(outs[i].Expr, winSchema)
		outCols[i] = engine.Column{Name: outs[i].Name, Type: outs[i].Type, Nullable: true}
	}
	node := &Window{Child: base, Specs: win.specs, Schema: winSchema}
	return node, outs, engine.Schema{Columns: outCols}, orderTerms, nil
}

// splitConjuncts flattens a top-level AND chain into its conjuncts.
func splitConjuncts(e sql.Expr) []sql.Expr {
	if b, ok := e.(*sql.BinaryOp); ok && b.Op == "AND" {
		return append(splitConjuncts(b.Left), splitConjuncts(b.Right)...)
	}
	if e == nil {
		return nil
	}
	return []sql.Expr{e}
}

// andConjuncts rebuilds an AND chain (nil for an empty slice).
func andConjuncts(cs []sql.Expr) sql.Expr {
	if len(cs) == 0 {
		return nil
	}
	e := cs[0]
	for _, c := range cs[1:] {
		e = &sql.BinaryOp{Op: "AND", Left: e, Right: c}
	}
	return e
}

// baseAliasRanges returns the alias ranges of a (freshly built) base relation,
// for feeding the join-key splitter during decorrelation.
func baseAliasRanges(n Node, schema engine.Schema) []engine.AliasRange {
	switch x := baseRelation(n).(type) {
	case *Join:
		return x.Aliases
	case *Scan:
		return []engine.AliasRange{{Alias: x.Alias, Start: 0, End: len(schema.Columns)}}
	case *Subquery:
		return []engine.AliasRange{{Alias: x.Alias, Start: 0, End: len(schema.Columns)}}
	case *TableFunc:
		return []engine.AliasRange{{Alias: x.Alias, Start: 0, End: len(schema.Columns)}}
	}
	return []engine.AliasRange{{Alias: "", Start: 0, End: len(schema.Columns)}}
}

func isOuterCol(cr *sql.ColRef, inner map[string]bool, outer engine.Resolver) bool {
	return cr.Qualifier != "" && !inner[cr.Qualifier] && outer(cr.Qualifier, cr.Name) >= 0
}

func isInnerCol(cr *sql.ColRef, inner map[string]bool) bool {
	return cr.Qualifier == "" || inner[cr.Qualifier]
}

// anyOuterColRef reports whether e references any outer-scope column.
func anyOuterColRef(e sql.Expr, inner map[string]bool, outer engine.Resolver) bool {
	found := false
	mapExpr(e, func(n sql.Expr) sql.Expr {
		if cr, ok := n.(*sql.ColRef); ok && isOuterCol(cr, inner, outer) {
			found = true
		}
		return nil
	})
	return found
}

// correlationEq reports whether c is `outer.col = inner.col` (either order).
func correlationEq(c sql.Expr, inner map[string]bool, outer engine.Resolver) bool {
	b, ok := c.(*sql.BinaryOp)
	if !ok || b.Op != "=" {
		return false
	}
	l, lok := b.Left.(*sql.ColRef)
	r, rok := b.Right.(*sql.ColRef)
	if !lok || !rok {
		return false
	}
	return (isOuterCol(l, inner, outer) && isInnerCol(r, inner)) ||
		(isInnerCol(l, inner) && isOuterCol(r, inner, outer))
}

// decorrelateExists rewrites top-level `[NOT] EXISTS (correlated single-table
// subquery)` conjuncts in WHERE into semi-/anti-joins (one hash pass instead of
// re-running the subquery per outer row). Conjuncts it can't handle are left in
// WHERE for the Apply path. It returns the (possibly join-wrapped) base.
func (bc *buildCtx) decorrelateExists(stmt *sql.SelectStmt, base Node, baseSchema engine.Schema) (Node, error) {
	if stmt.Where == nil {
		return base, nil
	}
	var kept []sql.Expr
	for _, c := range splitConjuncts(stmt.Where) {
		joined, ok, err := bc.tryDecorrelate(c, base, baseSchema)
		if err != nil {
			return nil, err
		}
		if ok {
			base = joined
			continue
		}
		kept = append(kept, c)
	}
	stmt.Where = andConjuncts(kept)
	return base, nil
}

// tryDecorrelate converts one conjunct to a semi-/anti-join when it is a
// correlated EXISTS / NOT EXISTS over a single table with exactly one equality
// correlation. ok=false leaves it for the Apply path.
func (bc *buildCtx) tryDecorrelate(c sql.Expr, base Node, baseSchema engine.Schema) (Node, bool, error) {
	kind := sql.JoinSemi
	exists, ok := c.(*sql.ExistsExpr)
	if !ok {
		if u, isNot := c.(*sql.UnaryOp); isNot && u.Op == "NOT" {
			if exists, ok = u.Expr.(*sql.ExistsExpr); ok {
				kind = sql.JoinAnti
			}
		}
	}
	if !ok {
		return nil, false, nil
	}
	q := exists.Query
	// Only a simple single-table subquery (no joins/grouping/distinct/limit and
	// no aggregate or window in the select list) decorrelates cleanly.
	if len(q.Joins) > 0 || len(q.GroupBy) > 0 || q.Having != nil || q.Distinct ||
		q.Limit != nil || q.Offset != nil || q.NoFrom {
		return nil, false, nil
	}
	for _, it := range q.Items.Items {
		if !it.Star && (exprHasAgg(it.Expr) || exprHasWindow(it.Expr) || exprHasSubquery(it.Expr)) {
			return nil, false, nil
		}
	}

	inner := tableAliases(q)
	outerResolve := resolverFor(base, baseSchema)
	var corr sql.Expr
	var residual []sql.Expr
	for _, w := range splitConjuncts(q.Where) {
		switch {
		case correlationEq(w, inner, outerResolve):
			if corr != nil {
				return nil, false, nil // more than one correlation key: leave to Apply
			}
			corr = w
		case anyOuterColRef(w, inner, outerResolve):
			return nil, false, nil // a non-equality correlation: leave to Apply
		default:
			residual = append(residual, w)
		}
	}
	if corr == nil {
		return nil, false, nil // not correlated: cheap via memoized Apply
	}

	// Build the right side: the subquery's table, filtered by the residual.
	right, rightSchema, rightAlias, err := bc.buildTableRef(q.From)
	if err != nil {
		return nil, false, err
	}
	if res := andConjuncts(residual); res != nil {
		if scan, isScan := right.(*Scan); isScan {
			scan.Predicate = res // pushdown hint; engine re-applies below
		}
		right = &Filter{Child: right, Predicate: res}
	}

	leftAliases := baseAliasRanges(base, baseSchema)
	lks, rks, joinResid, err := bc.splitJoin(corr, baseSchema, leftAliases, rightSchema, rightAlias)
	if err != nil || len(lks) != 1 || joinResid != nil {
		return nil, false, nil // not a plain a.x = b.y key: leave to Apply
	}
	return &Join{
		Kind: kind, Left: base, Right: right, LeftKeys: lks, RightKeys: rks,
		Schema: baseSchema, Aliases: leftAliases,
	}, true, nil
}

// mapExpr deep-copies e, applying fn to every node first: a non-nil result
// replaces the node (its children are not descended into), and nil recurses.
// EXISTS/scalar-subquery nodes are treated as leaves (their inner Query is not
// walked here).
func mapExpr(e sql.Expr, fn func(sql.Expr) sql.Expr) sql.Expr {
	if e == nil {
		return nil
	}
	if r := fn(e); r != nil {
		return r
	}
	switch ex := e.(type) {
	case *sql.BinaryOp:
		return &sql.BinaryOp{Op: ex.Op, Left: mapExpr(ex.Left, fn), Right: mapExpr(ex.Right, fn)}
	case *sql.UnaryOp:
		return &sql.UnaryOp{Op: ex.Op, Expr: mapExpr(ex.Expr, fn)}
	case *sql.FuncCall:
		args := make([]sql.Expr, len(ex.Args))
		for i, a := range ex.Args {
			args[i] = mapExpr(a, fn)
		}
		return &sql.FuncCall{Name: ex.Name, Args: args, Distinct: ex.Distinct, Over: ex.Over}
	case *sql.InExpr:
		list := make([]sql.Expr, len(ex.List))
		for i, v := range ex.List {
			list[i] = mapExpr(v, fn)
		}
		return &sql.InExpr{Expr: mapExpr(ex.Expr, fn), List: list, Subquery: ex.Subquery, Negate: ex.Negate}
	case *sql.BetweenExpr:
		return &sql.BetweenExpr{Expr: mapExpr(ex.Expr, fn), Low: mapExpr(ex.Low, fn), High: mapExpr(ex.High, fn), Negate: ex.Negate}
	case *sql.LikeExpr:
		return &sql.LikeExpr{Expr: mapExpr(ex.Expr, fn), Pat: mapExpr(ex.Pat, fn), Negate: ex.Negate, Insensitive: ex.Insensitive}
	case *sql.IsNullExpr:
		return &sql.IsNullExpr{Expr: mapExpr(ex.Expr, fn), Negate: ex.Negate}
	case *sql.CaseExpr:
		whens := make([]sql.CaseWhen, len(ex.Whens))
		for i, w := range ex.Whens {
			whens[i] = sql.CaseWhen{Cond: mapExpr(w.Cond, fn), Then: mapExpr(w.Then, fn)}
		}
		var els sql.Expr
		if ex.Else != nil {
			els = mapExpr(ex.Else, fn)
		}
		return &sql.CaseExpr{Whens: whens, Else: els}
	case *sql.CastExpr:
		return &sql.CastExpr{Expr: mapExpr(ex.Expr, fn), Type: ex.Type}
	case *sql.ExtractExpr:
		return &sql.ExtractExpr{Field: ex.Field, Source: mapExpr(ex.Source, fn)}
	case *sql.PositionExpr:
		return &sql.PositionExpr{Substr: mapExpr(ex.Substr, fn), Str: mapExpr(ex.Str, fn)}
	}
	return e
}

// tableAliases returns the set of names a subquery's own FROM/JOIN tables can be
// referenced by (alias, else source name).
func tableAliases(q *sql.SelectStmt) map[string]bool {
	set := map[string]bool{}
	add := func(tr sql.TableRef) {
		a := tr.Alias
		if a == "" {
			a = tr.Name
		}
		if a == "" {
			a = tr.Source
		}
		if a != "" {
			set[a] = true
		}
	}
	add(q.From)
	for _, j := range q.Joins {
		add(j.Ref)
	}
	return set
}

// rewriteCorrelated rewrites, in place, every qualified column in q that refers
// to the outer scope (not one of q's own tables, but resolvable outside) into an
// OuterRef. It returns the number rewritten (0 means the subquery is not
// correlated). Correlated columns must be qualified with the outer table alias.
func rewriteCorrelated(q *sql.SelectStmt, outer engine.Resolver) int {
	inner := tableAliases(q)
	count := 0
	fn := func(e sql.Expr) sql.Expr {
		cr, ok := e.(*sql.ColRef)
		if !ok || cr.Qualifier == "" || inner[cr.Qualifier] {
			return nil
		}
		if outer(cr.Qualifier, cr.Name) >= 0 {
			count++
			return &sql.OuterRef{Qualifier: cr.Qualifier, Name: cr.Name}
		}
		return nil
	}
	q.Where = mapExpr(q.Where, fn)
	q.Having = mapExpr(q.Having, fn)
	for i := range q.Items.Items {
		if !q.Items.Items[i].Star {
			q.Items.Items[i].Expr = mapExpr(q.Items.Items[i].Expr, fn)
		}
	}
	for i := range q.Joins {
		q.Joins[i].On = mapExpr(q.Joins[i].On, fn)
	}
	for i := range q.GroupBy {
		q.GroupBy[i] = mapExpr(q.GroupBy[i], fn)
	}
	for i := range q.OrderBy {
		q.OrderBy[i].Expr = mapExpr(q.OrderBy[i].Expr, fn)
	}
	return count
}

// buildSubqueryPlan rewrites a subquery's correlated references against the outer
// scope, then builds its plan. It reports whether the subquery is correlated.
func (bc *buildCtx) buildSubqueryPlan(q *sql.SelectStmt, outer Node, outerSchema engine.Schema) (Node, bool, error) {
	n := rewriteCorrelated(q, resolverFor(outer, outerSchema))
	inner, _, err := bc.buildSelect(q)
	if err != nil {
		return nil, false, err
	}
	return inner, n > 0, nil
}

// subqExtractor lifts subquery expressions into Apply $subN columns, returning
// expressions that reference those columns.
type subqExtractor struct {
	bc          *buildCtx
	outer       Node
	outerSchema engine.Schema
	specs       []SubquerySpec
	cols        []engine.Column
	n           int
	err         error
}

func (x *subqExtractor) add(kind subqKind, inner Node, correlated bool, test sql.Expr, negate bool) sql.Expr {
	name := fmt.Sprintf("$sub%d", x.n)
	x.n++
	x.specs = append(x.specs, SubquerySpec{Name: name, Kind: kind, Inner: inner, Test: test, Negate: negate, Correlated: correlated})
	x.cols = append(x.cols, engine.Column{Name: name, Type: engine.TypeAny, Nullable: true})
	return &sql.ColRef{Name: name}
}

func (x *subqExtractor) rewrite(e sql.Expr) sql.Expr {
	return mapExpr(e, func(node sql.Expr) sql.Expr {
		if x.err != nil {
			return nil
		}
		switch s := node.(type) {
		case *sql.ExistsExpr:
			inner, corr, err := x.bc.buildSubqueryPlan(s.Query, x.outer, x.outerSchema)
			if err != nil {
				x.err = err
				return &sql.LitNull{}
			}
			return x.add(subqExists, inner, corr, nil, false)
		case *sql.ScalarSubquery:
			inner, corr, err := x.bc.buildSubqueryPlan(s.Query, x.outer, x.outerSchema)
			if err != nil {
				x.err = err
				return &sql.LitNull{}
			}
			return x.add(subqScalar, inner, corr, nil, false)
		case *sql.InExpr:
			if s.Subquery == nil {
				return nil
			}
			inner, corr, err := x.bc.buildSubqueryPlan(s.Subquery, x.outer, x.outerSchema)
			if err != nil {
				x.err = err
				return &sql.LitNull{}
			}
			if !corr {
				// A non-correlated IN is folded to a literal list elsewhere
				// (resolveInSubqueries), which is pushdown-eligible.
				return nil
			}
			return x.add(subqIn, inner, true, s.Expr, s.Negate)
		}
		return nil
	})
}

// buildSubqueries lifts subquery expressions in WHERE/SELECT/ORDER BY into an
// Apply node, rewriting those clauses to reference its $subN columns. If nothing
// is lifted (e.g. only a non-correlated IN), the base is returned unchanged.
func (bc *buildCtx) buildSubqueries(stmt *sql.SelectStmt, base Node, baseSchema engine.Schema) (Node, engine.Schema, error) {
	x := &subqExtractor{bc: bc, outer: base, outerSchema: baseSchema}
	if stmt.Where != nil {
		stmt.Where = x.rewrite(stmt.Where)
	}
	for i := range stmt.Items.Items {
		if !stmt.Items.Items[i].Star {
			stmt.Items.Items[i].Expr = x.rewrite(stmt.Items.Items[i].Expr)
		}
	}
	for i := range stmt.OrderBy {
		stmt.OrderBy[i].Expr = x.rewrite(stmt.OrderBy[i].Expr)
	}
	if x.err != nil {
		return nil, engine.Schema{}, x.err
	}
	if len(x.specs) == 0 {
		return base, baseSchema, nil
	}
	schema := engine.Schema{Columns: append(append([]engine.Column{}, baseSchema.Columns...), x.cols...)}
	return &Apply{Child: base, Specs: x.specs, Schema: schema}, schema, nil
}

// exprHasSubquery reports whether the expression contains a subquery node
// (EXISTS, a scalar subquery, or x IN (SELECT ...)).
func exprHasSubquery(e sql.Expr) bool {
	switch ex := e.(type) {
	case *sql.ExistsExpr, *sql.ScalarSubquery:
		return true
	case *sql.InExpr:
		if ex.Subquery != nil {
			return true
		}
		if exprHasSubquery(ex.Expr) {
			return true
		}
		for _, x := range ex.List {
			if exprHasSubquery(x) {
				return true
			}
		}
	case *sql.BinaryOp:
		return exprHasSubquery(ex.Left) || exprHasSubquery(ex.Right)
	case *sql.UnaryOp:
		return exprHasSubquery(ex.Expr)
	case *sql.BetweenExpr:
		return exprHasSubquery(ex.Expr) || exprHasSubquery(ex.Low) || exprHasSubquery(ex.High)
	case *sql.LikeExpr:
		return exprHasSubquery(ex.Expr) || exprHasSubquery(ex.Pat)
	case *sql.IsNullExpr:
		return exprHasSubquery(ex.Expr)
	case *sql.FuncCall:
		for _, a := range ex.Args {
			if exprHasSubquery(a) {
				return true
			}
		}
	case *sql.CaseExpr:
		for _, w := range ex.Whens {
			if exprHasSubquery(w.Cond) || exprHasSubquery(w.Then) {
				return true
			}
		}
		return ex.Else != nil && exprHasSubquery(ex.Else)
	case *sql.CastExpr:
		return exprHasSubquery(ex.Expr)
	case *sql.ExtractExpr:
		return exprHasSubquery(ex.Source)
	case *sql.PositionExpr:
		return exprHasSubquery(ex.Substr) || exprHasSubquery(ex.Str)
	}
	return false
}

// exprHasWindow reports whether the expression tree contains a window-function
// call (a FuncCall with an OVER clause).
func exprHasWindow(e sql.Expr) bool {
	switch ex := e.(type) {
	case *sql.FuncCall:
		if ex.Over != nil {
			return true
		}
		for _, a := range ex.Args {
			if exprHasWindow(a) {
				return true
			}
		}
	case *sql.BinaryOp:
		return exprHasWindow(ex.Left) || exprHasWindow(ex.Right)
	case *sql.UnaryOp:
		return exprHasWindow(ex.Expr)
	case *sql.InExpr:
		if exprHasWindow(ex.Expr) {
			return true
		}
		for _, x := range ex.List {
			if exprHasWindow(x) {
				return true
			}
		}
	case *sql.BetweenExpr:
		return exprHasWindow(ex.Expr) || exprHasWindow(ex.Low) || exprHasWindow(ex.High)
	case *sql.LikeExpr:
		return exprHasWindow(ex.Expr) || exprHasWindow(ex.Pat)
	case *sql.IsNullExpr:
		return exprHasWindow(ex.Expr)
	case *sql.CaseExpr:
		for _, w := range ex.Whens {
			if exprHasWindow(w.Cond) || exprHasWindow(w.Then) {
				return true
			}
		}
		return ex.Else != nil && exprHasWindow(ex.Else)
	case *sql.CastExpr:
		return exprHasWindow(ex.Expr)
	case *sql.ExtractExpr:
		return exprHasWindow(ex.Source)
	case *sql.PositionExpr:
		return exprHasWindow(ex.Substr) || exprHasWindow(ex.Str)
	}
	return false
}

// rewriteFuncs deep-copies e, replacing any FuncCall for which sub returns a
// non-nil node with that node (without descending into it); other nodes are
// rebuilt with rewritten children. Used to lift window calls into precomputed
// columns.
func rewriteFuncs(e sql.Expr, sub func(*sql.FuncCall) sql.Expr) sql.Expr {
	switch ex := e.(type) {
	case *sql.FuncCall:
		if r := sub(ex); r != nil {
			return r
		}
		args := make([]sql.Expr, len(ex.Args))
		for i, a := range ex.Args {
			args[i] = rewriteFuncs(a, sub)
		}
		return &sql.FuncCall{Name: ex.Name, Args: args, Distinct: ex.Distinct, Over: ex.Over}
	case *sql.BinaryOp:
		return &sql.BinaryOp{Op: ex.Op, Left: rewriteFuncs(ex.Left, sub), Right: rewriteFuncs(ex.Right, sub)}
	case *sql.UnaryOp:
		return &sql.UnaryOp{Op: ex.Op, Expr: rewriteFuncs(ex.Expr, sub)}
	case *sql.InExpr:
		list := make([]sql.Expr, len(ex.List))
		for i, v := range ex.List {
			list[i] = rewriteFuncs(v, sub)
		}
		return &sql.InExpr{Expr: rewriteFuncs(ex.Expr, sub), List: list, Subquery: ex.Subquery, Negate: ex.Negate}
	case *sql.BetweenExpr:
		return &sql.BetweenExpr{Expr: rewriteFuncs(ex.Expr, sub), Low: rewriteFuncs(ex.Low, sub), High: rewriteFuncs(ex.High, sub), Negate: ex.Negate}
	case *sql.LikeExpr:
		return &sql.LikeExpr{Expr: rewriteFuncs(ex.Expr, sub), Pat: rewriteFuncs(ex.Pat, sub), Negate: ex.Negate, Insensitive: ex.Insensitive}
	case *sql.IsNullExpr:
		return &sql.IsNullExpr{Expr: rewriteFuncs(ex.Expr, sub), Negate: ex.Negate}
	case *sql.CaseExpr:
		whens := make([]sql.CaseWhen, len(ex.Whens))
		for i, w := range ex.Whens {
			whens[i] = sql.CaseWhen{Cond: rewriteFuncs(w.Cond, sub), Then: rewriteFuncs(w.Then, sub)}
		}
		var els sql.Expr
		if ex.Else != nil {
			els = rewriteFuncs(ex.Else, sub)
		}
		return &sql.CaseExpr{Whens: whens, Else: els}
	case *sql.CastExpr:
		return &sql.CastExpr{Expr: rewriteFuncs(ex.Expr, sub), Type: ex.Type}
	case *sql.ExtractExpr:
		return &sql.ExtractExpr{Field: ex.Field, Source: rewriteFuncs(ex.Source, sub)}
	case *sql.PositionExpr:
		return &sql.PositionExpr{Substr: rewriteFuncs(ex.Substr, sub), Str: rewriteFuncs(ex.Str, sub)}
	}
	return e
}

// winExtractor lifts window-function calls out of expressions into Window output
// columns ($winN), returning expressions that reference those columns.
type winExtractor struct {
	specs []engine.WindowSpec
	cols  []engine.Column
	in    engine.Schema // input schema, for inferring window argument types
	n     int
}

func (x *winExtractor) rewrite(e sql.Expr) sql.Expr {
	return rewriteFuncs(e, func(fc *sql.FuncCall) sql.Expr {
		if fc.Over == nil {
			return nil
		}
		name := fmt.Sprintf("$win%d", x.n)
		x.n++
		x.specs = append(x.specs, engine.WindowSpec{
			Name:        name,
			Func:        fc.Name,
			Args:        fc.Args,
			PartitionBy: fc.Over.PartitionBy,
			OrderBy:     fc.Over.OrderBy,
			Frame:       fc.Over.Frame,
		})
		x.cols = append(x.cols, engine.Column{Name: name, Type: windowType(fc, x.in), Nullable: true})
		return &sql.ColRef{Name: name}
	})
}

// buildProjection expands a non-aggregate select list into engine.ProjectedExpr
// entries and computes the output schema. (Aggregate queries build their
// projection in buildAggregate, where aggregate calls are rewritten to column
// references first.)
func (bc *buildCtx) buildProjection(stmt *sql.SelectStmt, inSchema engine.Schema) ([]engine.ProjectedExpr, engine.Schema, error) {
	// Handle SELECT * (and alias.*).
	if len(stmt.Items.Items) == 1 && stmt.Items.Items[0].Star {
		var outs []engine.ProjectedExpr
		var cols []engine.Column
		for _, c := range inSchema.Columns {
			c := c
			outs = append(outs, engine.ProjectedExpr{
				Expr: &sql.ColRef{Name: c.Name},
				Name: c.Name,
				Type: c.Type,
			})
			cols = append(cols, c)
		}
		return outs, engine.Schema{Columns: cols}, nil
	}

	var outs []engine.ProjectedExpr
	var cols []engine.Column
	for _, it := range stmt.Items.Items {
		if it.Star {
			for _, c := range inSchema.Columns {
				c := c
				outs = append(outs, engine.ProjectedExpr{
					Expr: &sql.ColRef{Name: c.Name},
					Name: c.Name,
					Type: c.Type,
				})
				cols = append(cols, c)
			}
			continue
		}
		name := it.As
		if name == "" {
			name = inferExprName(it.Expr)
		}
		typ := exprType(it.Expr, inSchema)
		outs = append(outs, engine.ProjectedExpr{Expr: it.Expr, Name: name, Type: typ})
		cols = append(cols, engine.Column{Name: name, Type: typ, Nullable: true})
	}
	return outs, engine.Schema{Columns: cols}, nil
}

// exprType infers the result type of a projection expression against the input
// schema. It is best-effort and conservative: anything it cannot pin down with
// confidence yields TypeAny (the old blanket behaviour), so it only ever
// sharpens a column's type. It mirrors the engine's actual runtime evaluation
// (Arith, Cast, the function library, the aggregate/window operators) so the
// declared type matches the values produced.
func exprType(e sql.Expr, in engine.Schema) engine.Type {
	switch ex := e.(type) {
	case *sql.CastExpr:
		return castType(ex.Type)
	case *sql.ColRef:
		if i := in.Index(ex.Name); i >= 0 {
			return in.Columns[i].Type
		}
		// Dotted source attribute (service.name) lexed as qualifier+name: fall back
		// to a column literally named "<qualifier>.<name>", mirroring SchemaResolver.
		if ex.Qualifier != "" {
			if i := in.Index(ex.Qualifier + "." + ex.Name); i >= 0 {
				return in.Columns[i].Type
			}
		}
	case *sql.LitInt:
		return engine.TypeInt
	case *sql.LitFloat:
		return engine.TypeFloat
	case *sql.LitString:
		return engine.TypeString
	case *sql.LitBool:
		return engine.TypeBool
	case *sql.IntervalLit:
		return engine.TypeDuration
	case *sql.UnaryOp:
		return unaryType(ex, in)
	case *sql.BinaryOp:
		return binaryType(ex, in)
	case *sql.IsNullExpr, *sql.LikeExpr, *sql.InExpr, *sql.BetweenExpr:
		// Predicates evaluate to a boolean.
		return engine.TypeBool
	case *sql.ExtractExpr, *sql.PositionExpr:
		// EXTRACT(field FROM ts) and POSITION(a IN b) return integers.
		return engine.TypeInt
	case *sql.CaseExpr:
		return caseType(ex, in)
	case *sql.FuncCall:
		return funcCallType(ex, in)
	}
	return engine.TypeAny
}

// castType maps a CAST target type name to its engine.Type, mirroring the type
// names accepted by engine.Cast. Unknown names yield TypeAny.
func castType(typ string) engine.Type {
	switch strings.ToLower(typ) {
	case "int", "integer", "bigint":
		return engine.TypeInt
	case "float", "real", "double":
		return engine.TypeFloat
	case "string", "text", "varchar":
		return engine.TypeString
	case "bool", "boolean":
		return engine.TypeBool
	case "time", "timestamp", "datetime":
		return engine.TypeTime
	}
	return engine.TypeAny
}

// binaryType infers the type of a binary expression. Comparisons and logical
// connectives are boolean; arithmetic follows arithType.
func binaryType(ex *sql.BinaryOp, in engine.Schema) engine.Type {
	switch ex.Op {
	case "AND", "OR", "=", "<>", "<", "<=", ">", ">=":
		return engine.TypeBool
	case "+", "-", "*", "/":
		return arithType(ex.Op, exprType(ex.Left, in), exprType(ex.Right, in))
	}
	return engine.TypeAny
}

// arithType mirrors engine.Arith/temporalArith at the type level. int op int is
// int (except division, which may widen to float at runtime, so it is reported
// as float); any float operand yields float; temporal combinations follow the
// time/duration algebra. Unknown operands yield TypeAny.
func arithType(op string, l, r engine.Type) engine.Type {
	if isTemporal(l) || isTemporal(r) {
		switch {
		case op == "+" && l == engine.TypeTime && r == engine.TypeDuration,
			op == "+" && l == engine.TypeDuration && r == engine.TypeTime,
			op == "-" && l == engine.TypeTime && r == engine.TypeDuration:
			return engine.TypeTime
		case op == "-" && l == engine.TypeTime && r == engine.TypeTime:
			return engine.TypeDuration
		case (op == "+" || op == "-") && l == engine.TypeDuration && r == engine.TypeDuration:
			return engine.TypeDuration
		}
		return engine.TypeAny
	}
	if isNumeric(l) && isNumeric(r) {
		if op == "/" {
			// int/int is int only when it divides evenly (data-dependent); report
			// the widening type so the column type covers every row.
			return engine.TypeFloat
		}
		if l == engine.TypeInt && r == engine.TypeInt {
			return engine.TypeInt
		}
		return engine.TypeFloat
	}
	return engine.TypeAny
}

// unaryType infers the type of a unary expression: NOT is boolean; numeric
// negation preserves int/float (per engine.Negate).
func unaryType(ex *sql.UnaryOp, in engine.Schema) engine.Type {
	switch ex.Op {
	case "NOT":
		return engine.TypeBool
	case "-", "+":
		if t := exprType(ex.Expr, in); t == engine.TypeInt || t == engine.TypeFloat {
			return t
		}
	}
	return engine.TypeAny
}

// caseType infers a CASE's type by unifying every result branch (THENs + ELSE).
// A missing ELSE contributes NULL, which does not constrain the type.
func caseType(ex *sql.CaseExpr, in engine.Schema) engine.Type {
	branches := make([]sql.Expr, 0, len(ex.Whens)+1)
	for _, w := range ex.Whens {
		branches = append(branches, w.Then)
	}
	if ex.Else != nil {
		branches = append(branches, ex.Else)
	}
	return unifyExprs(branches, in)
}

// funcCallType infers the result type of a function call: a window function
// (Over set), an aggregate, or a scalar function.
func funcCallType(fc *sql.FuncCall, in engine.Schema) engine.Type {
	if fc.Over != nil {
		return windowType(fc, in)
	}
	if engine.IsAggregate(fc.Name) {
		return aggregateType(fc, in)
	}
	return scalarFuncType(fc, in)
}

// aggregateType infers an aggregate's result type, matching the engine's
// aggregate operator: COUNT/REGR_COUNT are integers, MIN/MAX/FIRST/LAST
// preserve their argument's type, STRING_AGG is a string, and every other
// aggregate computes a float.
func aggregateType(fc *sql.FuncCall, in engine.Schema) engine.Type {
	switch strings.ToUpper(fc.Name) {
	case "COUNT", "REGR_COUNT":
		return engine.TypeInt
	case "MIN", "MAX", "FIRST", "LAST":
		return firstArgType(fc, in)
	case "STRING_AGG":
		return engine.TypeString
	}
	return engine.TypeFloat
}

// windowType infers a window function's result type. The ranking/numbering
// functions are integers, the distribution functions are floats, and the value
// functions (LAG/LEAD/FIRST_VALUE/…) preserve their argument's type. An
// aggregate used as a window function follows aggregateType.
func windowType(fc *sql.FuncCall, in engine.Schema) engine.Type {
	switch strings.ToUpper(fc.Name) {
	case "ROW_NUMBER", "RANK", "DENSE_RANK", "NTILE":
		return engine.TypeInt
	case "PERCENT_RANK", "CUME_DIST":
		return engine.TypeFloat
	case "LAG", "LEAD", "FIRST_VALUE", "LAST_VALUE", "NTH_VALUE", "LOCF":
		return firstArgType(fc, in)
	}
	if engine.IsAggregate(fc.Name) {
		return aggregateType(fc, in)
	}
	return engine.TypeAny
}

// scalarFuncTypes maps scalar functions with a fixed result type to that type.
// Functions whose result type depends on their arguments (ABS, COALESCE,
// GREATEST, LEAST, NULLIF) are resolved in scalarFuncType instead.
var scalarFuncTypes = map[string]engine.Type{
	// String-returning.
	"LOWER": engine.TypeString, "UPPER": engine.TypeString, "SUBSTR": engine.TypeString,
	"SUBSTRING": engine.TypeString, "TRIM": engine.TypeString, "LTRIM": engine.TypeString,
	"RTRIM": engine.TypeString, "CONCAT": engine.TypeString, "REPLACE": engine.TypeString,
	"LEFT": engine.TypeString, "RIGHT": engine.TypeString, "SPLIT_PART": engine.TypeString,
	"REGEXP_REPLACE": engine.TypeString, "REGEXP_EXTRACT": engine.TypeString,
	"REGEXP_MATCHES": engine.TypeString, "EXTRACT_VALUE": engine.TypeString,
	"REPEAT": engine.TypeString, "REVERSE": engine.TypeString, "INITCAP": engine.TypeString,
	"LPAD": engine.TypeString, "RPAD": engine.TypeString, "STRFTIME": engine.TypeString,
	// Integer-returning.
	"LENGTH": engine.TypeInt, "LEN": engine.TypeInt, "STRPOS": engine.TypeInt,
	"INSTR": engine.TypeInt, "SIGN": engine.TypeInt, "WIDTH_BUCKET": engine.TypeInt,
	// Float-returning.
	"SQRT": engine.TypeFloat, "EXP": engine.TypeFloat, "LN": engine.TypeFloat,
	"LOG10": engine.TypeFloat, "LOG": engine.TypeFloat, "POWER": engine.TypeFloat,
	"POW": engine.TypeFloat, "MOD": engine.TypeFloat, "TRUNC": engine.TypeFloat,
	"ROUND": engine.TypeFloat, "FLOOR": engine.TypeFloat, "CEIL": engine.TypeFloat,
	"CEILING": engine.TypeFloat,
	// Time-returning.
	"NOW": engine.TypeTime, "CURRENT_TIMESTAMP": engine.TypeTime, "CURRENT_DATE": engine.TypeTime,
	"DATE_TRUNC": engine.TypeTime, "DATE_ADD": engine.TypeTime, "TO_TIMESTAMP": engine.TypeTime,
	"DATE": engine.TypeTime, "CONVERT_TZ": engine.TypeTime, "FROM_TZ": engine.TypeTime,
	"DATE_BIN": engine.TypeTime,
}

// scalarFuncType infers a scalar function's result type from scalarFuncTypes, or
// from its arguments for the type-preserving functions. Unknown functions (and
// AGE, whose result is a time or a duration depending on arity) yield TypeAny.
func scalarFuncType(fc *sql.FuncCall, in engine.Schema) engine.Type {
	if t, ok := scalarFuncTypes[strings.ToUpper(fc.Name)]; ok {
		return t
	}
	switch strings.ToUpper(fc.Name) {
	case "ABS", "NULLIF":
		if t := firstArgType(fc, in); t == engine.TypeInt || t == engine.TypeFloat || fc.Name == "NULLIF" {
			return t
		}
	case "COALESCE", "GREATEST", "LEAST":
		return unifyExprs(fc.Args, in)
	}
	return engine.TypeAny
}

// firstArgType returns the inferred type of a call's first argument, or TypeAny
// when it has none.
func firstArgType(fc *sql.FuncCall, in engine.Schema) engine.Type {
	if len(fc.Args) > 0 {
		return exprType(fc.Args[0], in)
	}
	return engine.TypeAny
}

// unifyExprs returns the common type of a set of expressions: their shared type
// if every expression resolves to the same concrete type, otherwise TypeAny.
func unifyExprs(exprs []sql.Expr, in engine.Schema) engine.Type {
	out := engine.TypeInvalid
	for _, e := range exprs {
		t := exprType(e, in)
		if t == engine.TypeAny || t == engine.TypeInvalid {
			return engine.TypeAny
		}
		if out == engine.TypeInvalid {
			out = t
		} else if out != t {
			return engine.TypeAny
		}
	}
	if out == engine.TypeInvalid {
		return engine.TypeAny
	}
	return out
}

// isNumeric reports whether t is an int or float.
func isNumeric(t engine.Type) bool { return t == engine.TypeInt || t == engine.TypeFloat }

// isTemporal reports whether t is a time or a duration.
func isTemporal(t engine.Type) bool { return t == engine.TypeTime || t == engine.TypeDuration }

// inferExprName derives a default column name for an expression.
func inferExprName(e sql.Expr) string {
	switch ex := e.(type) {
	case *sql.ColRef:
		if ex.Qualifier != "" {
			return ex.Qualifier + "." + ex.Name
		}
		return ex.Name
	case *sql.FuncCall:
		return strings.ToLower(ex.Name)
	case *sql.LitInt:
		return "int"
	case *sql.LitString:
		return "string"
	}
	return "expr"
}

// exprHasAgg reports whether the expression tree contains an aggregate call.
func exprHasAgg(e sql.Expr) bool {
	switch ex := e.(type) {
	case *sql.FuncCall:
		// A windowed aggregate (SUM(x) OVER (...)) is a window function, not a
		// grouping aggregate.
		if engine.IsAggregate(ex.Name) && ex.Over == nil {
			return true
		}
		for _, a := range ex.Args {
			if exprHasAgg(a) {
				return true
			}
		}
	case *sql.BinaryOp:
		return exprHasAgg(ex.Left) || exprHasAgg(ex.Right)
	case *sql.UnaryOp:
		return exprHasAgg(ex.Expr)
	case *sql.InExpr:
		if exprHasAgg(ex.Expr) {
			return true
		}
		for _, x := range ex.List {
			if exprHasAgg(x) {
				return true
			}
		}
	case *sql.BetweenExpr:
		return exprHasAgg(ex.Expr) || exprHasAgg(ex.Low) || exprHasAgg(ex.High)
	case *sql.LikeExpr:
		return exprHasAgg(ex.Expr) || exprHasAgg(ex.Pat)
	case *sql.IsNullExpr:
		return exprHasAgg(ex.Expr)
	case *sql.CaseExpr:
		for _, w := range ex.Whens {
			if exprHasAgg(w.Cond) || exprHasAgg(w.Then) {
				return true
			}
		}
		if ex.Else != nil && exprHasAgg(ex.Else) {
			return true
		}
	case *sql.CastExpr:
		return exprHasAgg(ex.Expr)
	case *sql.ExtractExpr:
		return exprHasAgg(ex.Source)
	case *sql.PositionExpr:
		return exprHasAgg(ex.Substr) || exprHasAgg(ex.Str)
	}
	return false
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
