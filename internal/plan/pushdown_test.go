package plan

import (
	"context"
	"testing"

	"github.com/april/turntable/internal/connector"
	"github.com/april/turntable/internal/engine"
	"github.com/april/turntable/internal/sql"
)

// fakeConn is a minimal connector for planner tests: a fixed two-column schema
// and an empty scan. It records nothing — the tests inspect the plan tree.
type fakeConn struct{}

func (fakeConn) Name() string { return "fake" }
func (fakeConn) Datasets(ctx context.Context) ([]connector.Dataset, error) {
	return nil, nil
}
func (fakeConn) Resolve(ctx context.Context, ds connector.Dataset) (engine.Schema, error) {
	return engine.Schema{Columns: []engine.Column{
		{Name: "x", Type: engine.TypeInt, Nullable: true},
		{Name: "y", Type: engine.TypeString, Nullable: true},
	}}, nil
}
func (fakeConn) Scan(ctx context.Context, req connector.ScanRequest) (engine.RowIterator, error) {
	return engine.NewSliceIter(nil), nil
}

func testRegistry(t *testing.T) *connector.Registry {
	t.Helper()
	reg := connector.NewRegistry()
	_ = reg.RegisterConnector(fakeConn{})
	for _, name := range []string{"t", "u"} {
		if err := reg.RegisterSource(name, fakeConn{}, connector.Dataset{Name: name}); err != nil {
			t.Fatalf("register %q: %v", name, err)
		}
	}
	return reg
}

// firstScan returns the (single) Scan node in a plan tree, or nil.
func firstScan(n Node) *Scan {
	switch node := n.(type) {
	case *Scan:
		return node
	case *Filter:
		return firstScan(node.Child)
	case *Project:
		return firstScan(node.Child)
	case *Sort:
		return firstScan(node.Child)
	case *Aggregate:
		return firstScan(node.Child)
	case *Limit:
		return firstScan(node.Child)
	}
	return nil
}

func buildPlan(t *testing.T, query string) *Plan {
	t.Helper()
	stmt, err := sql.Parse(query)
	if err != nil {
		t.Fatalf("parse %q: %v", query, err)
	}
	p, err := Build(context.Background(), stmt.(*sql.SelectStmt), testRegistry(t))
	if err != nil {
		t.Fatalf("build %q: %v", query, err)
	}
	return p
}

func TestPushdownSingleTablePredicateAndLimit(t *testing.T) {
	p := buildPlan(t, "SELECT * FROM t WHERE x > 1 LIMIT 5")
	scan := firstScan(p.Root)
	if scan == nil {
		t.Fatal("no Scan node found")
	}
	if scan.Predicate == nil {
		t.Error("expected WHERE predicate pushed to Scan")
	}
	if scan.Limit == nil || *scan.Limit != 5 {
		t.Errorf("expected LIMIT 5 pushed to Scan, got %v", scan.Limit)
	}
	// The engine still re-applies the predicate as a Filter above the Scan.
	if !hasFilter(p.Root) {
		t.Error("expected engine Filter retained above Scan (safety net)")
	}
}

func TestPushdownLimitWithdrawnUnderOrderBy(t *testing.T) {
	// ORDER BY needs all rows before LIMIT, so the limit must NOT be pushed.
	p := buildPlan(t, "SELECT * FROM t WHERE x > 1 ORDER BY y LIMIT 5")
	scan := firstScan(p.Root)
	if scan.Predicate == nil {
		t.Error("predicate should still be pushed under ORDER BY")
	}
	if scan.Limit != nil {
		t.Errorf("LIMIT must not be pushed under ORDER BY, got %v", scan.Limit)
	}
}

func TestPushdownLimitWithdrawnUnderAggregate(t *testing.T) {
	p := buildPlan(t, "SELECT COUNT(*) AS n FROM t WHERE x > 1 LIMIT 5")
	scan := firstScan(p.Root)
	if scan.Limit != nil {
		t.Errorf("LIMIT must not be pushed under aggregation, got %v", scan.Limit)
	}
}

func TestPushdownLimitWithdrawnUnderOffset(t *testing.T) {
	p := buildPlan(t, "SELECT * FROM t WHERE x > 1 LIMIT 5 OFFSET 10")
	scan := firstScan(p.Root)
	if scan.Limit != nil {
		t.Errorf("LIMIT must not be pushed with OFFSET, got %v", scan.Limit)
	}
}

func TestPushdownDisabledForJoins(t *testing.T) {
	// With a join the WHERE may span tables; nothing should be pushed to either
	// scan. firstScan reaches the left scan via the Join's Left child — but our
	// walker doesn't descend Joins, so assert via the Join directly.
	p := buildPlan(t, "SELECT * FROM t JOIN u ON t.x = u.x WHERE t.y > 1 LIMIT 5")
	j := firstJoin(p.Root)
	if j == nil {
		t.Fatal("expected a Join node")
	}
	for _, side := range []Node{j.Left, j.Right} {
		if s, ok := side.(*Scan); ok {
			if s.Predicate != nil || s.Limit != nil {
				t.Errorf("join scan %q should have no pushdown, got pred=%v limit=%v", s.Alias, s.Predicate, s.Limit)
			}
		}
	}
}

func hasFilter(n Node) bool {
	switch node := n.(type) {
	case *Filter:
		return true
	case *Project:
		return hasFilter(node.Child)
	case *Sort:
		return hasFilter(node.Child)
	case *Aggregate:
		return hasFilter(node.Child)
	case *Limit:
		return hasFilter(node.Child)
	}
	return false
}

func firstJoin(n Node) *Join {
	switch node := n.(type) {
	case *Join:
		return node
	case *Filter:
		return firstJoin(node.Child)
	case *Project:
		return firstJoin(node.Child)
	case *Sort:
		return firstJoin(node.Child)
	case *Aggregate:
		return firstJoin(node.Child)
	case *Limit:
		return firstJoin(node.Child)
	}
	return nil
}
