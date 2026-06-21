package plan

import (
	"context"
	"fmt"

	"github.com/april/turntable/internal/connector"
	"github.com/april/turntable/internal/engine"
)

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

// execNode recursively executes a node, returning its iterator + output schema.
func execNode(ctx context.Context, n Node, funcs *engine.FuncRegistry, strict bool) (engine.RowIterator, engine.Schema, error) {
	switch node := n.(type) {
	case *Scan:
		return execScan(ctx, node)

	case *NoFrom:
		// One empty row; a zero-column schema.
		return engine.NewSliceIter([]engine.Row{{}}), engine.Schema{}, nil

	case *Filter:
		child, schema, err := execNode(ctx, node.Child, funcs, strict)
		if err != nil {
			return nil, engine.Schema{}, err
		}
		eval := engine.Evaluator{Resolve: resolverFor(node.Child, schema), Funcs: funcs, Strict: strict}
		return engine.NewFilterIter(child, node.Predicate, eval), schema, nil

	case *Project:
		child, schema, err := execNode(ctx, node.Child, funcs, strict)
		if err != nil {
			return nil, engine.Schema{}, err
		}
		eval := engine.Evaluator{Resolve: resolverFor(node.Child, schema), Funcs: funcs, Strict: strict}
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
		eval := engine.Evaluator{Resolve: resolverFor(node.Child, schema), Funcs: funcs, Strict: strict}
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
		default:
			return n
		}
	}
}

// execScan calls the connector and returns a Scan iterator + schema.
func execScan(ctx context.Context, node *Scan) (engine.RowIterator, engine.Schema, error) {
	it, err := node.Source.Conn.Scan(ctx, connector.ScanRequest{
		Dataset: node.Source.Dataset,
	})
	if err != nil {
		return nil, engine.Schema{}, err
	}
	return it, node.Schema, nil
}

// execJoin executes a hash join: build the left iterator, stream the right,
// and merge row values. The output schema is node.Schema.
func execJoin(ctx context.Context, node *Join, funcs *engine.FuncRegistry, strict bool) (engine.RowIterator, engine.Schema, error) {
	left, _, err := execNode(ctx, node.Left, funcs, strict)
	if err != nil {
		return nil, engine.Schema{}, err
	}
	right, rightSchema, err := execNode(ctx, node.Right, funcs, strict)
	if err != nil {
		return nil, engine.Schema{}, err
	}
	rightWidth := len(rightSchema.Columns)
	it := engine.NewHashJoinIter(left, right, node.LeftKey, node.RightKey, node.Kind, rightWidth)
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
	eval := engine.Evaluator{Resolve: resolverFor(node.Child, childSchema), Funcs: funcs, Strict: strict}
	var havingEval engine.Evaluator
	if node.Having != nil {
		havingEval = engine.Evaluator{Resolve: engine.SchemaResolver(node.Schema, ""), Funcs: funcs, Strict: strict}
	}
	it := engine.NewAggregateIter(child, node.Keys, node.Aggs, node.Having, eval, havingEval, node.Schema)
	return it, node.Schema, nil
}

// projectOutputSchema builds the output schema from a projection's output list.
func projectOutputSchema(outs []engine.ProjectedExpr) engine.Schema {
	cols := make([]engine.Column, len(outs))
	for i, o := range outs {
		cols[i] = engine.Column{Name: o.Name, Type: engine.TypeAny, Nullable: true}
	}
	return engine.Schema{Columns: cols}
}