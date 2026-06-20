package engine

import (
	"context"
	"fmt"
)

// Execute runs a logical plan node and returns a RowIterator. The plan types
// live in internal/plan to avoid a cycle; Execute accepts the concrete root
// via the Executer interface implemented there. This package's ops.go holds
// the operator implementations (filter/project/join/agg/sort/limit) used to
// compose the execution pipeline.
//
// The skeleton wires only a passthrough; full operators land in v0.1.

// PassthroughIter wraps a child iterator unchanged (used by the skeleton).
type PassthroughIter struct {
	inner RowIterator
}

func NewPassthroughIter(inner RowIterator) *PassthroughIter {
	return &PassthroughIter{inner: inner}
}

func (p *PassthroughIter) Next() (Row, bool, error) { return p.inner.Next() }
func (p *PassthroughIter) Close() error             { return p.inner.Close() }

// Materialize reads all rows from an iterator into a slice. Useful for small
// datasets and for tests. Respects context cancellation.
func Materialize(ctx context.Context, it RowIterator) ([]Row, error) {
	defer it.Close()
	var out []Row
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		r, ok, err := it.Next()
		if err != nil {
			return nil, err
		}
		if !ok {
			break
		}
		out = append(out, r)
	}
	return out, nil
}

// Run is a placeholder entrypoint; the real engine is implemented in v0.1.
func Run(ctx context.Context, plan any) (RowIterator, error) {
	return nil, fmt.Errorf("engine.Run not yet implemented (v0.1)")
}