package plan

import (
	"context"
	"testing"

	"github.com/april/turntable/internal/connector"
	"github.com/april/turntable/internal/engine"
	"github.com/april/turntable/internal/sql"
)

// twoTableConn serves emp(id,name) and ord(emp_id) so correlated EXISTS can be
// exercised, including a NULL emp_id that must never match.
type twoTableConn struct{}

func (twoTableConn) Name() string { return "two" }
func (twoTableConn) Datasets(ctx context.Context) ([]connector.Dataset, error) {
	return nil, nil
}
func (twoTableConn) Resolve(ctx context.Context, ds connector.Dataset) (engine.Schema, error) {
	if ds.Name == "emp" {
		return engine.Schema{Columns: []engine.Column{
			{Name: "id", Type: engine.TypeInt, Nullable: true},
			{Name: "name", Type: engine.TypeString, Nullable: true},
		}}, nil
	}
	return engine.Schema{Columns: []engine.Column{{Name: "emp_id", Type: engine.TypeInt, Nullable: true}}}, nil
}
func (twoTableConn) Scan(ctx context.Context, req connector.ScanRequest) (engine.RowIterator, error) {
	if req.Dataset.Name == "emp" {
		return engine.NewSliceIter([]engine.Row{
			{Values: []engine.Value{engine.IntVal(1), engine.StringVal("Alice")}},
			{Values: []engine.Value{engine.IntVal(2), engine.StringVal("bob")}},
			{Values: []engine.Value{engine.IntVal(3), engine.StringVal("Carol")}},
			{Values: []engine.Value{engine.Null(), engine.StringVal("Nul")}}, // NULL id
		}), nil
	}
	// ord: emps 1 and 2 have orders; 3 and NULL do not.
	return engine.NewSliceIter([]engine.Row{
		{Values: []engine.Value{engine.IntVal(1)}},
		{Values: []engine.Value{engine.IntVal(2)}},
		{Values: []engine.Value{engine.IntVal(2)}},
	}), nil
}

func twoRegistry(t *testing.T) *connector.Registry {
	t.Helper()
	reg := connector.NewRegistry()
	_ = reg.RegisterConnector(twoTableConn{})
	for _, n := range []string{"emp", "ord"} {
		if err := reg.RegisterSource(n, twoTableConn{}, connector.Dataset{Name: n}); err != nil {
			t.Fatal(err)
		}
	}
	return reg
}

func names(t *testing.T, reg *connector.Registry, q string) []string {
	t.Helper()
	rows := runQuery(t, reg, q)
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = r.Values[0].AsString()
	}
	return out
}

func planFor(t *testing.T, reg *connector.Registry, q string) Node {
	t.Helper()
	stmt, err := sql.Parse(q)
	if err != nil {
		t.Fatal(err)
	}
	p, err := Build(context.Background(), stmt, reg)
	if err != nil {
		t.Fatal(err)
	}
	return p.Root
}

// hasJoinKind reports whether the plan tree contains a join of the given kind.
func hasJoinKind(n Node, kind sql.JoinKind) bool {
	switch x := n.(type) {
	case *Join:
		if x.Kind == kind {
			return true
		}
		return hasJoinKind(x.Left, kind) || hasJoinKind(x.Right, kind)
	case *Filter:
		return hasJoinKind(x.Child, kind)
	case *Project:
		return hasJoinKind(x.Child, kind)
	case *Sort:
		return hasJoinKind(x.Child, kind)
	case *Limit:
		return hasJoinKind(x.Child, kind)
	case *Aggregate:
		return hasJoinKind(x.Child, kind)
	case *Apply:
		return hasJoinKind(x.Child, kind)
	}
	return false
}

func TestDecorrelateExistsSemiJoin(t *testing.T) {
	reg := twoRegistry(t)
	q := "SELECT name FROM emp AS e WHERE EXISTS (SELECT 1 FROM ord AS o WHERE o.emp_id = e.id) ORDER BY name"
	if !hasJoinKind(planFor(t, reg, q), sql.JoinSemi) {
		t.Fatal("EXISTS did not decorrelate to a semi join")
	}
	got := names(t, reg, q)
	// emps 1,2 have orders; 3 and NULL do not.
	if len(got) != 2 || got[0] != "Alice" || got[1] != "bob" {
		t.Fatalf("semi join = %v, want [Alice bob]", got)
	}
}

func TestDecorrelateNotExistsAntiJoin(t *testing.T) {
	reg := twoRegistry(t)
	q := "SELECT name FROM emp AS e WHERE NOT EXISTS (SELECT 1 FROM ord AS o WHERE o.emp_id = e.id) ORDER BY name"
	if !hasJoinKind(planFor(t, reg, q), sql.JoinAnti) {
		t.Fatal("NOT EXISTS did not decorrelate to an anti join")
	}
	got := names(t, reg, q)
	// Carol has no orders; the NULL-id row also has no match (NULL never matches).
	if len(got) != 2 || got[0] != "Carol" || got[1] != "Nul" {
		t.Fatalf("anti join = %v, want [Carol Nul]", got)
	}
}

func TestDecorrelatedExistsMatchesApply(t *testing.T) {
	reg := twoRegistry(t)
	// The decorrelated EXISTS and the equivalent Apply-based scalar COUNT must
	// agree (cross-check of the two execution paths).
	semi := names(t, reg, "SELECT name FROM emp AS e WHERE EXISTS (SELECT 1 FROM ord AS o WHERE o.emp_id = e.id) ORDER BY name")
	apply := names(t, reg, "SELECT name FROM emp AS e WHERE (SELECT COUNT(*) FROM ord AS o WHERE o.emp_id = e.id) > 0 ORDER BY name")
	if len(semi) != len(apply) {
		t.Fatalf("semi %v vs apply %v", semi, apply)
	}
	for i := range semi {
		if semi[i] != apply[i] {
			t.Fatalf("row %d: semi %q vs apply %q", i, semi[i], apply[i])
		}
	}
}

func TestDecorrelateExistsWithGroupBy(t *testing.T) {
	reg := twoRegistry(t)
	// Decorrelation lets EXISTS coexist with GROUP BY (the Apply path rejects it).
	rows := runQuery(t, reg,
		"SELECT COUNT(*) AS n FROM emp AS e WHERE EXISTS (SELECT 1 FROM ord AS o WHERE o.emp_id = e.id)")
	if n, _ := rows[0].Values[0].AsInt(); n != 2 {
		t.Fatalf("count = %v, want 2", rows[0].Values[0].V)
	}
}

func TestNonDecorrelatableExistsUsesApply(t *testing.T) {
	reg := twoRegistry(t)
	// A non-equality correlation can't decorrelate; it stays on Apply (no semi
	// join) but still produces correct results.
	q := "SELECT name FROM emp AS e WHERE EXISTS (SELECT 1 FROM ord AS o WHERE o.emp_id > e.id) ORDER BY name"
	root := planFor(t, reg, q)
	if hasJoinKind(root, sql.JoinSemi) {
		t.Fatal("non-equality correlation should not decorrelate")
	}
	if !hasJoinKind(root, sql.JoinSemi) && !hasApply(root) {
		t.Fatal("expected an Apply node for the non-decorrelatable EXISTS")
	}
	// emp ids with some order emp_id > id: id=1 (orders 2,2 > 1) yes; id=2 (no
	// order > 2) no; id=3 no; NULL no.
	got := names(t, reg, q)
	if len(got) != 1 || got[0] != "Alice" {
		t.Fatalf("apply result = %v, want [Alice]", got)
	}
}

func hasApply(n Node) bool {
	switch x := n.(type) {
	case *Apply:
		return true
	case *Filter:
		return hasApply(x.Child)
	case *Project:
		return hasApply(x.Child)
	case *Sort:
		return hasApply(x.Child)
	case *Limit:
		return hasApply(x.Child)
	}
	return false
}
