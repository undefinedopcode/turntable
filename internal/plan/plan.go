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

	"github.com/april/octoparser/internal/connector"
	"github.com/april/octoparser/internal/engine"
	"github.com/april/octoparser/internal/sql"
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
type Scan struct {
	Source   connector.Source
	Schema   engine.Schema
	Alias    string
}

// NoFrom is a synthetic single-row, zero-column relation for "SELECT <expr>"
// queries that have no FROM clause (e.g. scratch math in the REPL).
type NoFrom struct{}

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

func (*Scan) planNode()     {}
func (*Filter) planNode()   {}
func (*Project) planNode()  {}
func (*Join) planNode()     {}
func (*Aggregate) planNode() {}
func (*Sort) planNode()     {}
func (*Limit) planNode()    {}
func (*NoFrom) planNode()   {}

// buildCtx carries planner state down the tree.
type buildCtx struct {
	ctx  context.Context
	reg  *connector.Registry
	funcs *engine.FuncRegistry
}

// Build resolves and validates a parsed SELECT into a Plan against the given
// Registry. Options adjust planning behavior (e.g. strict mode).
func Build(ctx context.Context, stmt *sql.SelectStmt, reg *connector.Registry, opts ...BuildOption) (*Plan, error) {
	if stmt == nil {
		return nil, fmt.Errorf("nil statement")
	}
	bc := &buildCtx{ctx: ctx, reg: reg, funcs: engine.NewFuncRegistry()}
	root, schema, err := bc.buildSelect(stmt)
	if err != nil {
		return nil, err
	}
	p := &Plan{Root: root, OutputSchema: schema, Funcs: bc.funcs}
	for _, o := range opts {
		o(p)
	}
	return p, nil
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

// buildSelect builds the plan for a SELECT, returning the root node and the
// output schema (after projection).
func (bc *buildCtx) buildSelect(stmt *sql.SelectStmt) (Node, engine.Schema, error) {
	// 1. FROM + JOINs -> base relation schema with aliases.
	base, baseSchema, err := bc.buildFrom(stmt)
	if err != nil {
		return nil, engine.Schema{}, err
	}

	// 2. WHERE -> Filter (engine-applied; no pushdown for file connectors in v0.1).
	if stmt.Where != nil {
		base = &Filter{Child: base, Predicate: stmt.Where}
	}

	// 3. Determine if this is an aggregate query (GROUP BY present, or any
	//    aggregate function in the select list).
	hasAgg := len(stmt.GroupBy) > 0
	if !hasAgg {
		for _, it := range stmt.Items.Items {
			if exprHasAgg(it.Expr) {
				hasAgg = true
				break
			}
		}
	}

	var projectBase Node
	var projectSchema engine.Schema

	if hasAgg {
		agg, schema, err := bc.buildAggregate(stmt, base, baseSchema)
		if err != nil {
			return nil, engine.Schema{}, err
		}
		projectBase = agg
		projectSchema = schema
	} else {
		projectBase = base
		projectSchema = baseSchema
	}

	// 4. ORDER BY (may reference output aliases or input columns; for v0.1 we
	//    resolve against the pre-projection schema when no aggregation, else
	//    against the aggregate output schema). We apply Sort before Project so
	//    it can reference input columns not in the select list. When there's an
	//    aggregate, the sort must reference aggregate output; we resolve there.
	if len(stmt.OrderBy) > 0 {
		projectBase = &Sort{Child: projectBase, Terms: stmt.OrderBy}
	}

	// 5. Project (select list).
	outs, outSchema, err := bc.buildProjection(stmt, projectSchema, hasAgg)
	if err != nil {
		return nil, engine.Schema{}, err
	}
	root := Node(&Project{Child: projectBase, Outputs: outs, Distinct: stmt.Distinct})
	// If DISTINCT, wrap with a distinct node conceptually; the engine Project
	// doesn't dedupe, so we rely on the renderer/cli if needed. v0.1: support
	// DISTINCT via a distinct wrapper applied in exec.

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
		return nil, engine.Schema{}, "", fmt.Errorf("subqueries not yet supported")
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

// buildAggregate constructs the Aggregate node and its output schema. In
// v0.1 the aggregate output schema mirrors the SELECT list: non-aggregate
// select items become group keys (in select-list order) and aggregate items
// become aggregate output columns. This lets SELECT aliases like
// `p.name AS product` flow through correctly.
func (bc *buildCtx) buildAggregate(stmt *sql.SelectStmt, base Node, baseSchema engine.Schema) (Node, engine.Schema, error) {
	var keys []sql.Expr
	var aggs []engine.AggSpec
	var outCols []engine.Column

	for _, it := range stmt.Items.Items {
		if it.Star {
			return nil, engine.Schema{}, fmt.Errorf("SELECT * not valid with aggregation; name columns explicitly")
		}
		fc, ok := it.Expr.(*sql.FuncCall)
		if ok && engine.IsAggregate(fc.Name) {
			var arg sql.Expr
			if len(fc.Args) == 1 {
				arg = fc.Args[0]
			}
			name := it.As
			if name == "" {
				name = inferExprName(it.Expr)
			}
			aggs = append(aggs, engine.AggSpec{Func: fc.Name, Arg: arg, Name: name})
			outCols = append(outCols, engine.Column{Name: name, Type: engine.TypeAny, Nullable: true})
			continue
		}
		name := it.As
		if name == "" {
			name = inferExprName(it.Expr)
		}
		keys = append(keys, it.Expr)
		outCols = append(outCols, engine.Column{Name: name, Type: engine.TypeAny, Nullable: true})
	}
	aggSchema := engine.Schema{Columns: outCols}
	node := &Aggregate{
		Child: base, Keys: keys, Aggs: aggs, Having: stmt.Having, Schema: aggSchema,
	}
	return node, aggSchema, nil
}

// buildProjection expands the select list into engine.ProjectedExpr entries
// and computes the output schema. When hasAgg is true the projection runs over
// the aggregate output schema: every select item must correspond to a column
// already present in inSchema (a group key or an aggregate alias). We emit a
// ColRef to that column rather than re-evaluating the (possibly aggregate)
// expression.
func (bc *buildCtx) buildProjection(stmt *sql.SelectStmt, inSchema engine.Schema, hasAgg bool) ([]engine.ProjectedExpr, engine.Schema, error) {
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
		if hasAgg {
			// Reference the aggregate output column by its name. The aggregate
			// builder emitted a column with this same name (alias or inferred).
			outs = append(outs, engine.ProjectedExpr{
				Expr: &sql.ColRef{Name: name},
				Name: name,
			})
		} else {
			outs = append(outs, engine.ProjectedExpr{Expr: it.Expr, Name: name})
		}
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