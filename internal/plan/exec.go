package plan

import (
	"context"
	"fmt"

	"github.com/april/turntable/internal/connector"
	"github.com/april/turntable/internal/engine"
	"github.com/april/turntable/internal/sql"
)

// maxSeriesRows bounds a generate_series materialization (a guard against a
// runaway range, not a product limit).
const maxSeriesRows = 10_000_000

// generateSeries materializes a TableFunc into one-column rows.
func generateSeries(n *TableFunc) ([]engine.Row, error) {
	var rows []engine.Row
	emit := func(v engine.Value) bool {
		rows = append(rows, engine.Row{Values: []engine.Value{v}})
		return len(rows) <= maxSeriesRows
	}
	if n.IsTime {
		for cur := n.TimeStart; !(n.TimeStep > 0 && cur.After(n.TimeStop)) && !(n.TimeStep < 0 && cur.Before(n.TimeStop)); cur = cur.Add(n.TimeStep) {
			if !emit(engine.TimeVal(cur)) {
				return nil, fmt.Errorf("generate_series exceeded %d rows", maxSeriesRows)
			}
		}
		return rows, nil
	}
	for cur := n.IntStart; !(n.IntStep > 0 && cur > n.IntStop) && !(n.IntStep < 0 && cur < n.IntStop); cur += n.IntStep {
		if !emit(engine.IntVal(cur)) {
			return nil, fmt.Errorf("generate_series exceeded %d rows", maxSeriesRows)
		}
	}
	return rows, nil
}

// Exec converts a Plan's root Node into an engine.RowIterator, threading the
// per-stage schema and alias so that column references resolve correctly.
//
// The executor returns the iterator plus the output schema of the root node.
func Exec(ctx context.Context, p *Plan) (engine.RowIterator, engine.Schema, error) {
	it, schema, err := execNode(ctx, p.Root, p.Funcs, p.Strict)
	if err != nil {
		return nil, engine.Schema{}, err
	}
	return it, schema, nil
}

// ctxKey types values stored on the exec context.
type ctxKey int

const outerKey ctxKey = iota

// withOuter binds an enclosing query's current row on the context so that the
// subquery plan executed under it can resolve correlated OuterRef expressions.
func withOuter(ctx context.Context, o engine.Outer) context.Context {
	return context.WithValue(ctx, outerKey, o)
}

// outerFromCtx returns the bound outer row (inactive at the top level).
func outerFromCtx(ctx context.Context) engine.Outer {
	if ctx == nil {
		return engine.Outer{}
	}
	if o, ok := ctx.Value(outerKey).(engine.Outer); ok {
		return o
	}
	return engine.Outer{}
}

// evalForChild builds an Evaluator over a child node's output, carrying any
// active outer binding for correlated subqueries.
func evalForChild(ctx context.Context, child Node, schema engine.Schema, funcs *engine.FuncRegistry, strict bool) engine.Evaluator {
	return engine.Evaluator{Resolve: resolverFor(child, schema), Funcs: funcs, Strict: strict, Outer: outerFromCtx(ctx)}
}

// execNode recursively executes a node, returning its iterator + output schema.
func execNode(ctx context.Context, n Node, funcs *engine.FuncRegistry, strict bool) (engine.RowIterator, engine.Schema, error) {
	switch node := n.(type) {
	case *Scan:
		return execScan(ctx, node)

	case *Subquery:
		// Run the child plan; its rows pass through under the subquery alias.
		child, _, err := execNode(ctx, node.Child, funcs, strict)
		if err != nil {
			return nil, engine.Schema{}, err
		}
		return child, node.Schema, nil

	case *CTERef:
		// Replay the shared materialization; the first reference pulled runs the
		// CTE's plan once and buffers its rows.
		return &cteReplayIter{ctx: ctx, m: node.Mat, funcs: funcs, strict: strict}, node.Mat.Schema, nil

	case *SetOp:
		left, _, err := execNode(ctx, node.Left, funcs, strict)
		if err != nil {
			return nil, engine.Schema{}, err
		}
		right, _, err := execNode(ctx, node.Right, funcs, strict)
		if err != nil {
			left.Close()
			return nil, engine.Schema{}, err
		}
		var it engine.RowIterator
		switch node.Op {
		case sql.SetUnion:
			it = engine.NewConcatIter([]engine.RowIterator{left, right})
			if !node.All {
				it = engine.NewDistinctIter(it)
			}
		case sql.SetIntersect:
			it = engine.NewIntersectIter(left, right, node.All)
		case sql.SetExcept:
			it = engine.NewExceptIter(left, right, node.All)
		default:
			left.Close()
			right.Close()
			return nil, engine.Schema{}, fmt.Errorf("unknown set operation %d", node.Op)
		}
		return it, node.Schema, nil

	case *NoFrom:
		// One empty row; a zero-column schema.
		return engine.NewSliceIter([]engine.Row{{}}), engine.Schema{}, nil

	case *TableFunc:
		rows, err := generateSeries(node)
		if err != nil {
			return nil, engine.Schema{}, err
		}
		return engine.NewSliceIter(rows), node.Schema, nil

	case *Filter:
		child, schema, err := execNode(ctx, node.Child, funcs, strict)
		if err != nil {
			return nil, engine.Schema{}, err
		}
		eval := evalForChild(ctx, node.Child, schema, funcs, strict)
		return engine.NewFilterIter(child, node.Predicate, eval), schema, nil

	case *Project:
		child, schema, err := execNode(ctx, node.Child, funcs, strict)
		if err != nil {
			return nil, engine.Schema{}, err
		}
		eval := evalForChild(ctx, node.Child, schema, funcs, strict)
		out := engine.NewProjectIter(child, node.Outputs, eval)
		outSchema := projectOutputSchema(node.Outputs)
		it := engine.RowIterator(out)
		if node.Distinct {
			it = engine.NewDistinctIter(it)
		}
		return it, outSchema, nil

	case *Sort:
		child, schema, err := execNode(ctx, node.Child, funcs, strict)
		if err != nil {
			return nil, engine.Schema{}, err
		}
		eval := evalForChild(ctx, node.Child, schema, funcs, strict)
		return engine.NewSortIter(child, node.Terms, eval), schema, nil

	case *Limit:
		child, schema, err := execNode(ctx, node.Child, funcs, strict)
		if err != nil {
			return nil, engine.Schema{}, err
		}
		off := 0
		if node.Offset != nil {
			off = *node.Offset
		}
		return engine.NewLimitIter(child, node.Limit, off), schema, nil

	case *Join:
		return execJoin(ctx, node, funcs, strict)

	case *Aggregate:
		return execAggregate(ctx, node, funcs, strict)

	case *Window:
		child, childSchema, err := execNode(ctx, node.Child, funcs, strict)
		if err != nil {
			return nil, engine.Schema{}, err
		}
		eval := evalForChild(ctx, node.Child, childSchema, funcs, strict)
		return engine.NewWindowIter(child, node.Specs, eval), node.Schema, nil

	case *Apply:
		return execApply(ctx, node, funcs, strict)
	}
	return nil, engine.Schema{}, fmt.Errorf("unknown plan node %T", n)
}

// resolverFor returns the appropriate engine.Resolver for reading columns from
// the output of a child node. For a Join it uses JoinResolver over all
// contributing alias ranges so qualified refs resolve even after multi-way
// joins. For a Scan it uses the table's alias; otherwise an unqualified
// resolver.
func resolverFor(child Node, schema engine.Schema) engine.Resolver {
	base := baseRelation(child)
	if j, ok := base.(*Join); ok {
		return engine.JoinResolver(schema, j.Aliases)
	}
	if s, ok := base.(*Scan); ok {
		return engine.SchemaResolver(schema, s.Alias)
	}
	if sub, ok := base.(*Subquery); ok {
		return engine.SchemaResolver(schema, sub.Alias)
	}
	return engine.SchemaResolver(schema, "")
}

// baseRelation returns the leftmost Join or Scan under a chain of
// single-child operators. It stops at the first multi-child or leaf node.
func baseRelation(n Node) Node {
	for {
		switch x := n.(type) {
		case *Filter:
			n = x.Child
		case *Sort:
			n = x.Child
		case *Limit:
			n = x.Child
		case *Project:
			n = x.Child
		case *Aggregate:
			n = x.Child
		case *Window:
			n = x.Child
		case *Apply:
			n = x.Child
		default:
			return n
		}
	}
}

// execScan calls the connector and returns a Scan iterator + schema. The
// Predicate/Limit pushdown hints (set by the planner for single-table scans)
// are passed through; the connector applies what it can and the engine's
// Filter/Limit re-apply the rest.
func execScan(ctx context.Context, node *Scan) (engine.RowIterator, engine.Schema, error) {
	it, err := node.Source.Conn.Scan(ctx, connector.ScanRequest{
		Dataset:   node.Source.Dataset,
		Predicate: node.Predicate,
		Limit:     node.Limit,
		OrderBy:   node.OrderBy,
		Aggregate: node.Aggregate,
	})
	if err != nil {
		return nil, engine.Schema{}, err
	}
	return it, node.Schema, nil
}

// execJoin executes a hash join: build the left iterator, stream the right,
// and merge row values. The output schema is node.Schema.
func execJoin(ctx context.Context, node *Join, funcs *engine.FuncRegistry, strict bool) (engine.RowIterator, engine.Schema, error) {
	left, leftSchema, err := execNode(ctx, node.Left, funcs, strict)
	if err != nil {
		return nil, engine.Schema{}, err
	}
	right, rightSchema, err := execNode(ctx, node.Right, funcs, strict)
	if err != nil {
		return nil, engine.Schema{}, err
	}
	leftWidth := len(leftSchema.Columns)
	rightWidth := len(rightSchema.Columns)
	// The residual predicate (non-equi ON remainder) resolves columns over the
	// combined [left..., right...] schema via the join's alias ranges.
	var residualEval engine.Evaluator
	if node.Residual != nil {
		residualEval = engine.Evaluator{
			Resolve: engine.JoinResolver(node.Schema, node.Aliases),
			Funcs:   funcs, Strict: strict, Outer: outerFromCtx(ctx),
		}
	}
	it := engine.NewHashJoinIter(left, right, node.LeftKeys, node.RightKeys, node.Residual, residualEval, node.Kind, leftWidth, rightWidth)
	return it, node.Schema, nil
}

// execAggregate runs the aggregate. The group-key/agg-arg expressions are
// evaluated against the child's output rows; HAVING is evaluated against the
// aggregate's own output schema.
func execAggregate(ctx context.Context, node *Aggregate, funcs *engine.FuncRegistry, strict bool) (engine.RowIterator, engine.Schema, error) {
	child, childSchema, err := execNode(ctx, node.Child, funcs, strict)
	if err != nil {
		return nil, engine.Schema{}, err
	}
	eval := evalForChild(ctx, node.Child, childSchema, funcs, strict)
	var havingEval engine.Evaluator
	if node.Having != nil {
		havingEval = engine.Evaluator{Resolve: engine.SchemaResolver(node.Schema, ""), Funcs: funcs, Strict: strict}
	}
	it := engine.NewAggregateIter(child, node.Keys, node.Aggs, node.Having, eval, havingEval, node.Schema)
	return it, node.Schema, nil
}

// execApply runs the subquery-apply node: for each child (outer) row it
// evaluates each subquery spec to a single value and appends it as a column.
func execApply(ctx context.Context, node *Apply, funcs *engine.FuncRegistry, strict bool) (engine.RowIterator, engine.Schema, error) {
	child, childSchema, err := execNode(ctx, node.Child, funcs, strict)
	if err != nil {
		return nil, engine.Schema{}, err
	}
	outerResolve := resolverFor(node.Child, childSchema)
	eval := evalForChild(ctx, node.Child, childSchema, funcs, strict)
	// run executes one spec's inner plan with the outer row bound, returning its
	// materialized rows.
	run := func(spec SubquerySpec, outer engine.Row) ([]engine.Row, error) {
		innerCtx := withOuter(ctx, engine.Outer{Row: outer, Resolve: outerResolve, Active: true})
		it, _, err := execNode(innerCtx, spec.Inner, funcs, strict)
		if err != nil {
			return nil, err
		}
		return engine.Materialize(innerCtx, it)
	}
	return &applyIter{child: child, specs: node.Specs, eval: eval, run: run, memo: map[string]engine.Value{}}, node.Schema, nil
}

// applyIter streams its child, appending one column per subquery spec.
type applyIter struct {
	child  engine.RowIterator
	specs  []SubquerySpec
	eval   engine.Evaluator
	run    func(SubquerySpec, engine.Row) ([]engine.Row, error)
	memo   map[string]engine.Value // cached values for non-correlated specs
	closed bool
}

func (a *applyIter) Next() (engine.Row, bool, error) {
	r, ok, err := a.child.Next()
	if err != nil || !ok {
		return engine.Row{}, ok, err
	}
	for _, spec := range a.specs {
		v, err := a.value(spec, r)
		if err != nil {
			return engine.Row{}, false, err
		}
		r.Values = append(r.Values, v)
	}
	return r, true, nil
}

func (a *applyIter) value(spec SubquerySpec, outer engine.Row) (engine.Value, error) {
	// A non-correlated subquery yields the same value for every row.
	if !spec.Correlated {
		if v, ok := a.memo[spec.Name]; ok {
			return v, nil
		}
	}
	rows, err := a.run(spec, outer)
	if err != nil {
		return engine.Value{}, err
	}
	v, err := a.reduce(spec, outer, rows)
	if err != nil {
		return engine.Value{}, err
	}
	if !spec.Correlated {
		a.memo[spec.Name] = v
	}
	return v, nil
}

func (a *applyIter) reduce(spec SubquerySpec, outer engine.Row, rows []engine.Row) (engine.Value, error) {
	switch spec.Kind {
	case subqExists:
		return engine.BoolVal(len(rows) > 0), nil
	case subqScalar:
		if len(rows) == 0 {
			return engine.Null(), nil
		}
		if len(rows) > 1 {
			return engine.Value{}, fmt.Errorf("scalar subquery returned more than one row")
		}
		if len(rows[0].Values) == 0 {
			return engine.Null(), nil
		}
		return rows[0].Values[0], nil
	case subqIn:
		tv, err := a.eval.Eval(spec.Test, outer)
		if err != nil {
			return engine.Value{}, err
		}
		found := false
		if !tv.IsNull() {
			for _, rr := range rows {
				if len(rr.Values) > 0 && !rr.Values[0].IsNull() && engine.Compare(tv, rr.Values[0]) == 0 {
					found = true
					break
				}
			}
		}
		return engine.BoolVal(found != spec.Negate), nil
	}
	return engine.Value{}, fmt.Errorf("unknown subquery kind %d", spec.Kind)
}

func (a *applyIter) Close() error {
	if a.closed {
		return nil
	}
	a.closed = true
	return a.child.Close()
}

// cteReplayIter yields a CTE's materialized rows. The first iterator (across all
// references) to be pulled triggers the one-time materialization; each iterator
// keeps its own cursor into the shared buffer, so references are independent
// passes over the same snapshot.
type cteReplayIter struct {
	ctx    context.Context
	m      *cteMaterialization
	funcs  *engine.FuncRegistry
	strict bool
	i      int
	closed bool
}

func (it *cteReplayIter) Next() (engine.Row, bool, error) {
	if err := it.m.ensure(it.ctx, it.funcs, it.strict); err != nil {
		return engine.Row{}, false, err
	}
	if it.i >= len(it.m.rows) {
		return engine.Row{}, false, nil
	}
	r := it.m.rows[it.i]
	it.i++
	return r, true, nil
}

func (it *cteReplayIter) Close() error {
	it.closed = true
	return nil
}

// projectOutputSchema builds the output schema from a projection's output list.
func projectOutputSchema(outs []engine.ProjectedExpr) engine.Schema {
	cols := make([]engine.Column, len(outs))
	for i, o := range outs {
		typ := o.Type
		if typ == engine.TypeInvalid {
			typ = engine.TypeAny
		}
		cols[i] = engine.Column{Name: o.Name, Type: typ, Nullable: true}
	}
	return engine.Schema{Columns: cols}
}