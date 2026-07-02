package plan

// Qualified star (alias.*) expansion and standard-SQL output naming for
// qualified column refs (SELECT o.amount → column "amount"). Uses the
// emp/ord fixture from decorrelate_test.go.

import (
	"context"
	"strings"
	"testing"

	"github.com/april/turntable/internal/connector"
	"github.com/april/turntable/internal/engine"
	"github.com/april/turntable/internal/sql"
)

// runQuerySchema is runQuery, plus the output schema.
func runQuerySchema(t *testing.T, reg *connector.Registry, q string) (engine.Schema, []engine.Row) {
	t.Helper()
	stmt, err := sql.Parse(q)
	if err != nil {
		t.Fatalf("parse %q: %v", q, err)
	}
	p, err := Build(context.Background(), stmt, reg)
	if err != nil {
		t.Fatalf("build %q: %v", q, err)
	}
	it, schema, err := Exec(context.Background(), p)
	if err != nil {
		t.Fatalf("exec %q: %v", q, err)
	}
	rows, err := engine.Materialize(context.Background(), it)
	if err != nil {
		t.Fatalf("materialize %q: %v", q, err)
	}
	return schema, rows
}

func schemaNames(s engine.Schema) []string {
	out := make([]string, len(s.Columns))
	for i, c := range s.Columns {
		out[i] = c.Name
	}
	return out
}

func TestQualifiedStarSingleTable(t *testing.T) {
	schema, rows := runQuerySchema(t, twoRegistry(t), "SELECT e.* FROM emp AS e")
	if got := schemaNames(schema); len(got) != 2 || got[0] != "id" || got[1] != "name" {
		t.Fatalf("columns = %v, want [id name]", got)
	}
	if len(rows) != 4 {
		t.Fatalf("rows = %d, want 4", len(rows))
	}
}

func TestQualifiedStarJoin(t *testing.T) {
	// e.* expands to just emp's columns; ord's column is added explicitly.
	schema, rows := runQuerySchema(t, twoRegistry(t),
		"SELECT e.*, o.emp_id FROM emp AS e JOIN ord AS o ON e.id = o.emp_id")
	if got := schemaNames(schema); len(got) != 3 || got[0] != "id" || got[1] != "name" || got[2] != "emp_id" {
		t.Fatalf("columns = %v, want [id name emp_id]", got)
	}
	if len(rows) != 3 { // emp 1 has one order, emp 2 has two
		t.Fatalf("rows = %d, want 3", len(rows))
	}
}

func TestQualifiedStarRightSide(t *testing.T) {
	schema, _ := runQuerySchema(t, twoRegistry(t),
		"SELECT o.* FROM emp AS e JOIN ord AS o ON e.id = o.emp_id")
	if got := schemaNames(schema); len(got) != 1 || got[0] != "emp_id" {
		t.Fatalf("columns = %v, want [emp_id]", got)
	}
}

func TestQualifiedStarUnknownAlias(t *testing.T) {
	stmt, err := sql.Parse("SELECT x.* FROM emp AS e")
	if err != nil {
		t.Fatal(err)
	}
	_, err = Build(context.Background(), stmt, twoRegistry(t))
	if err == nil || !strings.Contains(err.Error(), `unknown table alias "x"`) {
		t.Fatalf("err = %v, want unknown table alias", err)
	}
}

func TestQualifiedStarInView(t *testing.T) {
	// The regression that surfaced this work: a view body using e.* must plan
	// (and run) like the inline query does.
	reg := twoRegistry(t)
	body, err := sql.Parse("SELECT e.* FROM emp AS e")
	if err != nil {
		t.Fatal(err)
	}
	if err := reg.RegisterView("empv", body, false); err != nil {
		t.Fatal(err)
	}
	schema, rows := runQuerySchema(t, reg, "SELECT name FROM empv WHERE id = 1")
	if got := schemaNames(schema); len(got) != 1 || got[0] != "name" {
		t.Fatalf("columns = %v, want [name]", got)
	}
	if len(rows) != 1 || rows[0].Values[0].AsString() != "Alice" {
		t.Fatalf("rows = %v, want [Alice]", rows)
	}
}

func TestUnqualifiedOutputName(t *testing.T) {
	schema, _ := runQuerySchema(t, twoRegistry(t), "SELECT e.name FROM emp AS e")
	if got := schemaNames(schema); got[0] != "name" {
		t.Fatalf("column = %q, want name (qualifier dropped from output)", got[0])
	}
}

func TestUnqualifiedOutputNameThroughSubquery(t *testing.T) {
	// The clean name makes the derived table's columns referenceable outside.
	schema, rows := runQuerySchema(t, twoRegistry(t),
		"SELECT s.name FROM (SELECT e.name FROM emp AS e) AS s WHERE s.name = 'Alice'")
	if got := schemaNames(schema); got[0] != "name" {
		t.Fatalf("column = %q, want name", got[0])
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
}

func TestQualifiedGroupByOrderByStillResolve(t *testing.T) {
	// Group keys are now named without their qualifier; qualified references in
	// HAVING/ORDER BY are rewritten to the key's output column.
	schema, rows := runQuerySchema(t, twoRegistry(t),
		"SELECT e.name, COUNT(*) AS n FROM emp AS e GROUP BY e.name HAVING e.name <> 'Nul' ORDER BY e.name")
	if got := schemaNames(schema); got[0] != "name" || got[1] != "n" {
		t.Fatalf("columns = %v, want [name n]", got)
	}
	if len(rows) != 3 {
		t.Fatalf("rows = %d, want 3", len(rows))
	}
	if rows[0].Values[0].AsString() != "Alice" {
		t.Fatalf("first row = %v, want Alice first", rows[0].Values)
	}
}

// dottedConn exposes a column whose real name contains a dot (a Honeycomb-style
// attribute), which must keep its dotted name in the output.
type dottedConn struct{}

func (dottedConn) Name() string                                        { return "dotted" }
func (dottedConn) Datasets(context.Context) ([]connector.Dataset, error) { return nil, nil }
func (dottedConn) Resolve(context.Context, connector.Dataset) (engine.Schema, error) {
	return engine.Schema{Columns: []engine.Column{
		{Name: "service.name", Type: engine.TypeString, Nullable: true},
	}}, nil
}
func (dottedConn) Scan(context.Context, connector.ScanRequest) (engine.RowIterator, error) {
	return engine.NewSliceIter([]engine.Row{
		{Values: []engine.Value{engine.StringVal("api")}},
	}), nil
}

func TestDottedAttributeNameKept(t *testing.T) {
	reg := connector.NewRegistry()
	_ = reg.RegisterConnector(dottedConn{})
	if err := reg.RegisterSource("events", dottedConn{}, connector.Dataset{Name: "events"}); err != nil {
		t.Fatal(err)
	}
	// service is not a FROM alias, so service.name is a dotted source attribute
	// and the dot stays in the output column name.
	schema, rows := runQuerySchema(t, reg, "SELECT service.name FROM events")
	if got := schemaNames(schema); got[0] != "service.name" {
		t.Fatalf("column = %q, want service.name kept dotted", got[0])
	}
	if len(rows) != 1 || rows[0].Values[0].AsString() != "api" {
		t.Fatalf("rows = %v", rows)
	}
}
