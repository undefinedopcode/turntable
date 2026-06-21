package engine

import (
	"context"
)

// Materialize reads all rows from an iterator into a slice. Useful for small
// datasets and for tests. Respects context cancellation; a nil context is
// treated as non-cancellable.
func Materialize(ctx context.Context, it RowIterator) ([]Row, error) {
	defer it.Close()
	var out []Row
	for {
		if ctx != nil {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
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

// SchemaResolver builds a Resolver for a schema. The alias, when non-empty,
// must match a registered alias; unqualified names match any column. When
// multiple columns share a name, the first match wins. (The planner should
// reject ambiguous unqualified references.)
func SchemaResolver(schema Schema, alias string) Resolver {
	return func(qualifier, name string) int {
		for i, c := range schema.Columns {
			if qualifier != "" {
				// qualified: match alias + column name
				if equalFold(qualifier, alias) && equalFold(c.Name, name) {
					return i
				}
				continue
			}
			if equalFold(c.Name, name) {
				return i
			}
		}
		return -1
	}
}

// JoinResolver builds a Resolver for a joined (combined) schema. The qualifier
// is matched against leftAlias (columns [0, leftWidth)) or rightAlias (columns
// [leftWidth, end)). Unqualified names match the first column with that name.
func JoinResolver(schema Schema, leftAlias string, leftWidth int, rightAlias string) Resolver {
	return func(qualifier, name string) int {
		if qualifier != "" {
			if equalFold(qualifier, leftAlias) {
				for i := 0; i < leftWidth && i < len(schema.Columns); i++ {
					if equalFold(schema.Columns[i].Name, name) {
						return i
					}
				}
			}
			if equalFold(qualifier, rightAlias) {
				for i := leftWidth; i < len(schema.Columns); i++ {
					if equalFold(schema.Columns[i].Name, name) {
						return i
					}
				}
			}
			return -1
		}
		for i, c := range schema.Columns {
			if equalFold(c.Name, name) {
				return i
			}
		}
		return -1
	}
}