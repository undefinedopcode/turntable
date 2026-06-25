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
	"strings"

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
}

// NoFrom is a synthetic single-row, zero-column relation for "SELECT <expr>"
// queries that have no FROM clause (e.g. scratch math in the REPL).
type NoFrom struct{}

// Subquery is a derived table: a FROM-clause subquery whose child plan produces
// the rows, presented under an alias. It passes the child's rows through
// unchanged; the alias lets the outer query qualify the subquery's columns.
type Subquery struct {
	Child  Node
	Schema engine.Schema
	Alias  string
}

// Union concatenates the rows of its branches (which must share a column count).
// Distinct is true for UNION (dedupe the combined result) and false for
// UNION ALL. Any final ORDER BY/LIMIT is layered above as Sort/Limit nodes.
type Union struct {
	Branches []Node
	Schema   engine.Schema
	Distinct bool
}

// Filter applies a residual predicate.
type Filter struct {
	Child     Node
	Predicate sql.Expr
}

// Project computes the select list.
type Project struct {
	Child   Node
	Outputs []engine.ProjectedExpr
	Distinct bool
}

// Join combines two relations.
type Join struct {
	Kind     sql.JoinKind
	Left     Node
	Right    Node
	LeftKey  engine.KeyExtractor
	RightKey engine.KeyExtractor
	Schema   engine.Schema
	Aliases  []engine.AliasRange // all contributing aliases in the combined schema
}

// Aggregate groups rows.
type Aggregate struct {
	Child    Node
	Keys     []sql.Expr
	Aggs     []engine.AggSpec
	Having   sql.Expr
	Schema   engine.Schema
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
func (*Union) planNode()     {}
func (*Filter) planNode()    {}
func (*Project) planNode()   {}
func (*Join) planNode()      {}
func (*Aggregate) planNode() {}
func (*Sort) planNode()      {}
func (*Limit) planNode()     {}
func (*NoFrom) planNode()    {}

// buildCtx carries planner state down the tree.
type buildCtx struct {
	ctx  context.Context
	reg  *connector.Registry
	funcs *engine.FuncRegistry
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

// buildSetOp builds the plan for a UNION of SELECT branches. Branches must agree
// on column count; the first branch's output schema names the result. The union
// dedupes (UNION) unless every branch is joined with UNION ALL. A trailing
// ORDER BY/LIMIT is layered above the union.
func (bc *buildCtx) buildSetOp(s *sql.SetOpStmt) (Node, engine.Schema, error) {
	if len(s.Selects) == 0 {
		return nil, engine.Schema{}, fmt.Errorf("empty set operation")
	}
	branches := make([]Node, len(s.Selects))
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
				"each UNION branch must have the same number of columns (%d vs %d)",
				len(outSchema.Columns), len(sch.Columns))
		}
		branches[i] = n
	}
	// UNION dedupes; only stays UNION ALL if every connector is ALL.
	distinct := false
	for _, all := range s.All {
		if !all {
			distinct = true
			break
		}
	}
	var root Node = &Union{Branches: branches, Schema: outSchema, Distinct: distinct}
	if len(s.OrderBy) > 0 {
		root = &Sort{Child: root, Terms: s.OrderBy}
	}
	if s.Limit != nil || s.Offset != nil {
		root = &Limit{Child: root, Limit: s.Limit, Offset: s.Offset}
	}
	return root, outSchema, nil
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

	// Fold any `x IN (SELECT ...)` in WHERE/HAVING into a literal value list by
	// executing the (non-correlated) subquery once. Doing this before pushdown
	// and Filter construction means the resolved IN list can also be pushed to a
	// capable connector. The engine sees an ordinary IN-list predicate.
	if err := bc.resolveInSubqueries(stmt.Where); err != nil {
		return nil, engine.Schema{}, err
	}
	if err := bc.resolveInSubqueries(stmt.Having); err != nil {
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

	// Resolve positional ORDER BY / GROUP BY references (e.g. `ORDER BY 2`) to
	// the matching select item before they reach Sort/Aggregate, so an integer
	// term sorts/groups by that output column instead of being a silent no-op.
	if err := resolveOrdinals(stmt); err != nil {
		return nil, engine.Schema{}, err
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
		if stmt.Limit != nil && stmt.Offset == nil && len(stmt.OrderBy) == 0 && !hasAgg {
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
		aggNode, outs, projSchema, orderTerms, err := bc.buildAggregate(stmt, base)
		if err != nil {
			return nil, engine.Schema{}, err
		}
		projectBase := Node(aggNode)
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
		lk, rk, err := bc.splitJoinKeys(j.On, schema, aliases, rightSchema, rightAlias)
		if err != nil {
			return nil, engine.Schema{}, err
		}
		combined := engine.Schema{Columns: append(append([]engine.Column{}, schema.Columns...), rightSchema.Columns...)}
		aliases = append(aliases, engine.AliasRange{
			Alias: rightAlias, Start: len(schema.Columns), End: len(combined.Columns),
		})
		leftNode = &Join{
			Kind: j.Kind, Left: leftNode, Right: rightNode,
			LeftKey: lk, RightKey: rk, Schema: combined,
			Aliases: aliases,
		}
		schema = combined
		leftAlias = "" // only the first join split uses explicit left/right aliases
	}
	return leftNode, schema, nil
}

// buildTableRef resolves a single FROM/JOIN table reference into a Scan node.
func (bc *buildCtx) buildTableRef(tr sql.TableRef) (Node, engine.Schema, string, error) {
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

// splitJoinKeys takes an ON expression of the form a.x = b.y and returns two
// KeyExtractor closures. The left side is the already-combined relation (with
// aliases), the right side is the new table being joined. Key indices are into
// the combined schema on that side.
func (bc *buildCtx) splitJoinKeys(on sql.Expr, leftSchema engine.Schema, leftAliases []engine.AliasRange, rightSchema engine.Schema, rightAlias string) (engine.KeyExtractor, engine.KeyExtractor, error) {
	bin, ok := on.(*sql.BinaryOp)
	if !ok || bin.Op != "=" {
		return nil, nil, fmt.Errorf("JOIN ON must be a single equality a.x = b.y (compound predicates not yet supported)")
	}
	lIdx, rIdx, err := bc.classifyJoinOperands(bin.Left, bin.Right, leftSchema, leftAliases, rightSchema, rightAlias)
	if err != nil {
		return nil, nil, err
	}
	lk := func(row engine.Row) engine.Value {
		if lIdx >= 0 && lIdx < len(row.Values) {
			return row.Values[lIdx]
		}
		return engine.Null()
	}
	rk := func(row engine.Row) engine.Value {
		if rIdx >= 0 && rIdx < len(row.Values) {
			return row.Values[rIdx]
		}
		return engine.Null()
	}
	return lk, rk, nil
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
	x.cols = append(x.cols, engine.Column{Name: name, Type: engine.TypeAny, Nullable: true})
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
		if engine.IsAggregate(ex.Name) {
			return x.register(ex, x.synth())
		}
		args := make([]sql.Expr, len(ex.Args))
		for i, a := range ex.Args {
			args[i] = x.rewrite(a)
		}
		return &sql.FuncCall{Name: ex.Name, Args: args, Distinct: ex.Distinct}
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
func (bc *buildCtx) buildAggregate(stmt *sql.SelectStmt, base Node) (Node, []engine.ProjectedExpr, engine.Schema, []sql.OrderTerm, error) {
	ext := &aggExtractor{}
	var keys []sql.Expr
	var keyCols []engine.Column
	var outs []engine.ProjectedExpr
	var outCols []engine.Column

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
			keyCols = append(keyCols, engine.Column{Name: name, Type: engine.TypeAny, Nullable: true})
			outs = append(outs, engine.ProjectedExpr{Expr: &sql.ColRef{Name: name}, Name: name})
		}
		outCols = append(outCols, engine.Column{Name: name, Type: engine.TypeAny, Nullable: true})
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
	node := &Aggregate{Child: base, Keys: keys, Aggs: ext.specs, Having: having, Schema: interSchema}
	return node, outs, engine.Schema{Columns: outCols}, orderTerms, nil
}

// isTopLevelAgg reports whether e is itself an aggregate call (vs. a scalar
// expression that merely contains one).
func isTopLevelAgg(e sql.Expr) bool {
	fc, ok := e.(*sql.FuncCall)
	return ok && engine.IsAggregate(fc.Name)
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
				})
				cols = append(cols, c)
			}
			continue
		}
		name := it.As
		if name == "" {
			name = inferExprName(it.Expr)
		}
		outs = append(outs, engine.ProjectedExpr{Expr: it.Expr, Name: name})
		cols = append(cols, engine.Column{Name: name, Type: engine.TypeAny, Nullable: true})
	}
	return outs, engine.Schema{Columns: cols}, nil
}

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
		if engine.IsAggregate(ex.Name) {
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