package plan

import (
	"context"
	"testing"

	"github.com/april/turntable/internal/connector"
	"github.com/april/turntable/internal/engine"
	"github.com/april/turntable/internal/sql"
)

// rowsConn is a connector returning fixed single-column ("n", int) rows, so
// subquery/union execution can be tested end to end. The rows are keyed by the
// dataset source name.
type rowsConn struct {
	data map[string][]int64
}

func (rowsConn) Name() string { return "rows" }
func (rowsConn) Datasets(ctx context.Context) ([]connector.Dataset, error) {
	return nil, nil
}
func (rowsConn) Resolve(ctx context.Context, ds connector.Dataset) (engine.Schema, error) {
	return engine.Schema{Columns: []engine.Column{{Name: "n", Type: engine.TypeInt, Nullable: true}}}, nil
}
func (c rowsConn) Scan(ctx context.Context, req connector.ScanRequest) (engine.RowIterator, error) {
	vals := c.data[req.Dataset.Name]
	rows := make([]engine.Row, len(vals))
	for i, v := range vals {
		rows[i] = engine.Row{Values: []engine.Value{engine.IntVal(v)}}
	}
	return engine.NewSliceIter(rows), nil
}

func rowsRegistry(t *testing.T) *connector.Registry {
	t.Helper()
	conn := rowsConn{data: map[string][]int64{
		"s1": {1, 2, 2},
		"s2": {2, 3},
	}}
	reg := connector.NewRegistry()
	_ = reg.RegisterConnector(conn)
	for _, n := range []string{"s1", "s2"} {
		if err := reg.RegisterSource(n, conn, connector.Dataset{Name: n}); err != nil {
			t.Fatal(err)
		}
	}
	return reg
}

func runQuery(t *testing.T, reg *connector.Registry, q string) []engine.Row {
	t.Helper()
	stmt, err := sql.Parse(q)
	if err != nil {
		t.Fatalf("parse %q: %v", q, err)
	}
	p, err := Build(context.Background(), stmt, reg)
	if err != nil {
		t.Fatalf("build %q: %v", q, err)
	}
	it, _, err := Exec(context.Background(), p)
	if err != nil {
		t.Fatalf("exec %q: %v", q, err)
	}
	rows, err := engine.Materialize(context.Background(), it)
	if err != nil {
		t.Fatalf("materialize %q: %v", q, err)
	}
	return rows
}

func TestSubqueryExec(t *testing.T) {
	reg := rowsRegistry(t)
	// Subquery passes its rows through; outer filter applies.
	rows := runQuery(t, reg, "SELECT n FROM (SELECT n FROM s1) AS x WHERE n > 1")
	if len(rows) != 2 { // s1 = {1,2,2}; n>1 keeps the two 2s
		t.Fatalf("rows = %d, want 2", len(rows))
	}
	for _, r := range rows {
		if v, _ := r.Values[0].AsInt(); v != 2 {
			t.Errorf("value = %v, want 2", r.Values[0].V)
		}
	}
}

func TestSubqueryQualifiedColumn(t *testing.T) {
	reg := rowsRegistry(t)
	rows := runQuery(t, reg, "SELECT x.n FROM (SELECT n FROM s1) AS x")
	if len(rows) != 3 {
		t.Fatalf("rows = %d, want 3", len(rows))
	}
}

func TestSubqueryRequiresAlias(t *testing.T) {
	reg := rowsRegistry(t)
	stmt, _ := sql.Parse("SELECT n FROM (SELECT n FROM s1)")
	if _, err := Build(context.Background(), stmt, reg); err == nil {
		t.Fatal("expected error: subquery in FROM must have an alias")
	}
}

func TestUnionDistinctVsAll(t *testing.T) {
	reg := rowsRegistry(t)
	// s1 = {1,2,2}, s2 = {2,3}
	if rows := runQuery(t, reg, "SELECT n FROM s1 UNION SELECT n FROM s2"); len(rows) != 3 {
		t.Fatalf("UNION rows = %d, want 3 (distinct {1,2,3})", len(rows))
	}
	if rows := runQuery(t, reg, "SELECT n FROM s1 UNION ALL SELECT n FROM s2"); len(rows) != 5 {
		t.Fatalf("UNION ALL rows = %d, want 5", len(rows))
	}
}

func TestUnionOrderByLimit(t *testing.T) {
	reg := rowsRegistry(t)
	rows := runQuery(t, reg, "SELECT n FROM s1 UNION ALL SELECT n FROM s2 ORDER BY n DESC LIMIT 2")
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(rows))
	}
	// Highest two of {1,2,2,2,3} are 3 then 2.
	if v, _ := rows[0].Values[0].AsInt(); v != 3 {
		t.Errorf("row0 = %v, want 3", rows[0].Values[0].V)
	}
	if v, _ := rows[1].Values[0].AsInt(); v != 2 {
		t.Errorf("row1 = %v, want 2", rows[1].Values[0].V)
	}
}

func TestInSubqueryExec(t *testing.T) {
	reg := rowsRegistry(t)
	// s1 = {1,2,2}, s2 = {2,3}. n IN (SELECT n FROM s2) keeps the 2s.
	rows := runQuery(t, reg, "SELECT n FROM s1 WHERE n IN (SELECT n FROM s2)")
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2 (the two 2s)", len(rows))
	}
	for _, r := range rows {
		if v, _ := r.Values[0].AsInt(); v != 2 {
			t.Errorf("value = %v, want 2", r.Values[0].V)
		}
	}
}

func TestNotInSubqueryExec(t *testing.T) {
	reg := rowsRegistry(t)
	// s1 = {1,2,2}; NOT IN (SELECT n FROM s2={2,3}) keeps only the 1.
	rows := runQuery(t, reg, "SELECT n FROM s1 WHERE n NOT IN (SELECT n FROM s2)")
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	if v, _ := rows[0].Values[0].AsInt(); v != 1 {
		t.Errorf("value = %v, want 1", rows[0].Values[0].V)
	}
}

func TestInSubqueryEmptyResult(t *testing.T) {
	reg := rowsRegistry(t)
	// Subquery filtered to nothing -> IN () matches no rows; NOT IN () matches all.
	if rows := runQuery(t, reg, "SELECT n FROM s1 WHERE n IN (SELECT n FROM s2 WHERE n > 99)"); len(rows) != 0 {
		t.Fatalf("IN empty: rows = %d, want 0", len(rows))
	}
	if rows := runQuery(t, reg, "SELECT n FROM s1 WHERE n NOT IN (SELECT n FROM s2 WHERE n > 99)"); len(rows) != 3 {
		t.Fatalf("NOT IN empty: rows = %d, want 3", len(rows))
	}
}

func TestInSubqueryMultiColumnError(t *testing.T) {
	reg := rowsRegistry(t)
	stmt, _ := sql.Parse("SELECT n FROM s1 WHERE n IN (SELECT n, n FROM s2)")
	if _, err := Build(context.Background(), stmt, reg); err == nil {
		t.Fatal("expected single-column error for multi-column IN subquery")
	}
}

func TestUnionColumnCountMismatch(t *testing.T) {
	reg := rowsRegistry(t)
	// Second branch projects two columns; counts differ.
	stmt, _ := sql.Parse("SELECT n FROM s1 UNION SELECT n, n FROM s2")
	if _, err := Build(context.Background(), stmt, reg); err == nil {
		t.Fatal("expected column-count mismatch error")
	}
}
