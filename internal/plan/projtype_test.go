package plan

import (
	"context"
	"testing"

	"github.com/april/turntable/internal/engine"
	"github.com/april/turntable/internal/sql"
)

// execSchema builds and executes a query, returning the schema the exec path
// reports — the schema the web/render layers actually display. This is the path
// that previously forced every projected column to TypeAny (projectOutputSchema),
// so the tests assert against it rather than the plan-side OutputSchema.
func execSchema(t *testing.T, query string) engine.Schema {
	t.Helper()
	stmt, err := sql.Parse(query)
	if err != nil {
		t.Fatalf("parse %q: %v", query, err)
	}
	p, err := Build(context.Background(), stmt, testRegistry(t))
	if err != nil {
		t.Fatalf("build %q: %v", query, err)
	}
	it, schema, err := Exec(context.Background(), p)
	if err != nil {
		t.Fatalf("exec %q: %v", query, err)
	}
	it.Close()
	return schema
}

// TestProjectionColumnTypes verifies the projection sharpens output column types
// instead of defaulting every item to TypeAny: a plain column reference passes
// through the source type, and a CAST carries its target type.
func TestProjectionColumnTypes(t *testing.T) {
	cases := []struct {
		query string
		want  []engine.Type
	}{
		// fakeConn: x int, y string.
		{"SELECT x, y FROM t", []engine.Type{engine.TypeInt, engine.TypeString}},
		{"SELECT * FROM t", []engine.Type{engine.TypeInt, engine.TypeString}},
		{"SELECT CAST(y AS int) AS n FROM t", []engine.Type{engine.TypeInt}},
		{"SELECT CAST(x AS float) AS f FROM t", []engine.Type{engine.TypeFloat}},
		{"SELECT 1 AS a, 'hi' AS b, 2.5 AS c FROM t", []engine.Type{engine.TypeInt, engine.TypeString, engine.TypeFloat}},
		// Arithmetic: int+int stays int; division widens to float.
		{"SELECT x + 1 AS s FROM t", []engine.Type{engine.TypeInt}},
		{"SELECT x * 2 AS s FROM t", []engine.Type{engine.TypeInt}},
		{"SELECT x / 2 AS s FROM t", []engine.Type{engine.TypeFloat}},
		{"SELECT x + 1.5 AS s FROM t", []engine.Type{engine.TypeFloat}},
		// Comparisons / logical / predicates are boolean.
		{"SELECT x > 1 AS b FROM t", []engine.Type{engine.TypeBool}},
		{"SELECT x IS NULL AS b FROM t", []engine.Type{engine.TypeBool}},
		{"SELECT NOT (x > 1) AS b FROM t", []engine.Type{engine.TypeBool}},
		// Scalar functions by fixed return type.
		{"SELECT UPPER(y) AS u, LENGTH(y) AS n, SQRT(x) AS r FROM t", []engine.Type{engine.TypeString, engine.TypeInt, engine.TypeFloat}},
		// ABS preserves its argument's numeric type.
		{"SELECT ABS(x) AS a FROM t", []engine.Type{engine.TypeInt}},
		// CASE unifies its branches; mismatched branches fall back to any.
		{"SELECT CASE WHEN x > 0 THEN 1 ELSE 2 END AS c FROM t", []engine.Type{engine.TypeInt}},
		{"SELECT CASE WHEN x > 0 THEN 1 ELSE 'x' END AS c FROM t", []engine.Type{engine.TypeAny}},
		// COALESCE over mismatched types cannot be pinned down -> any.
		{"SELECT COALESCE(x, y) AS c FROM t", []engine.Type{engine.TypeAny}},
	}
	for _, c := range cases {
		got := execSchema(t, c.query).Columns
		if len(got) != len(c.want) {
			t.Fatalf("%s: got %d columns, want %d", c.query, len(got), len(c.want))
		}
		for i, w := range c.want {
			if got[i].Type != w {
				t.Errorf("%s: column %d type = %s, want %s", c.query, i, got[i].Type, w)
			}
		}
	}
}

// TestAggregateColumnTypes covers the aggregate projection path: COUNT is an
// integer, SUM/AVG are floats (the engine accumulates in float64), MIN/MAX
// preserve the argument type, a group key passes through, and a scalar wrapper
// over an aggregate resolves via the function library.
func TestAggregateColumnTypes(t *testing.T) {
	// fakeConn: x int, y string.
	q := `SELECT y, COUNT(*) AS n, SUM(x) AS s, AVG(x) AS a,
	             MIN(x) AS lo, MAX(x) AS hi, ROUND(AVG(x), 2) AS r
	      FROM t GROUP BY y`
	want := map[string]engine.Type{
		"y": engine.TypeString, "n": engine.TypeInt, "s": engine.TypeFloat,
		"a": engine.TypeFloat, "lo": engine.TypeInt, "hi": engine.TypeInt,
		"r": engine.TypeFloat,
	}
	for _, c := range execSchema(t, q).Columns {
		if w, ok := want[c.Name]; ok && c.Type != w {
			t.Errorf("column %q type = %s, want %s", c.Name, c.Type, w)
		}
	}
}

// TestWindowColumnTypes covers the window projection path: ROW_NUMBER/RANK are
// integers, an aggregate window (SUM) is a float, LAG preserves its argument's
// type, and base columns pass through.
func TestWindowColumnTypes(t *testing.T) {
	// fakeConn: x int, y string.
	q := `SELECT x,
	             ROW_NUMBER() OVER (ORDER BY x) AS rn,
	             RANK() OVER (ORDER BY x) AS rk,
	             SUM(x) OVER (PARTITION BY y) AS running,
	             LAG(x) OVER (ORDER BY x) AS prev
	      FROM t`
	want := map[string]engine.Type{
		"x": engine.TypeInt, "rn": engine.TypeInt, "rk": engine.TypeInt,
		"running": engine.TypeFloat, "prev": engine.TypeInt,
	}
	for _, c := range execSchema(t, q).Columns {
		if w, ok := want[c.Name]; ok && c.Type != w {
			t.Errorf("column %q type = %s, want %s", c.Name, c.Type, w)
		}
	}
}

// TestProjectionTypesThroughJoinAndCTE mirrors the real-world report: a CTE
// joined to a base table, with qualified refs and a CAST. Column types must
// survive the CTE materialization and the join's combined schema all the way
// through exec.
func TestProjectionTypesThroughJoinAndCTE(t *testing.T) {
	// fakeConn: x int, y string. Self-join t to a CTE over t.
	q := `WITH c AS (SELECT x, y FROM t)
	      SELECT x, y, c.y AS cy, CAST(y AS int) AS n
	      FROM t LEFT JOIN c ON c.x = t.x`
	want := map[string]engine.Type{
		"x": engine.TypeInt, "y": engine.TypeString,
		"cy": engine.TypeString, "n": engine.TypeInt,
	}
	for _, c := range execSchema(t, q).Columns {
		if w, ok := want[c.Name]; ok && c.Type != w {
			t.Errorf("column %q type = %s, want %s", c.Name, c.Type, w)
		}
	}
}
