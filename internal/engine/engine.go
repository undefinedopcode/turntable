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

// AliasRange maps a table alias to the half-open [Start, End) interval of
// columns it contributes within a combined (joined) schema.
type AliasRange struct {
	Alias string
	Start int
	End   int
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
		// Fallback for source columns whose own name contains a dot (e.g.
		// Honeycomb attributes like "service.name"): SQL lexes such a reference as
		// qualifier="service", name="name", but no table alias matches. Match it
		// against a column literally named "<qualifier>.<name>". Harmless for
		// ordinary sources, whose column names never contain a dot.
		if qualifier != "" {
			dotted := qualifier + "." + name
			for i, c := range schema.Columns {
				if equalFold(c.Name, dotted) {
					return i
				}
			}
		}
		return -1
	}
}

// JoinResolver builds a Resolver for a joined (combined) schema. Each
// contributing alias has a column range. Unqualified names match the first
// column with that name across the whole combined schema.
func JoinResolver(schema Schema, aliases []AliasRange) Resolver {
	return func(qualifier, name string) int {
		if qualifier != "" {
			for _, ar := range aliases {
				if !equalFold(qualifier, ar.Alias) {
					continue
				}
				for i := ar.Start; i < ar.End && i < len(schema.Columns); i++ {
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