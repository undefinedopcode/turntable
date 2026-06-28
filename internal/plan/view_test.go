package plan

import (
	"context"
	"testing"

	"github.com/april/turntable/internal/sql"
)

// registerView parses a query and registers it as a view, failing the test on
// error.
func registerView(t *testing.T, reg interface {
	RegisterView(string, sql.Statement, bool) error
}, name, query string) {
	t.Helper()
	q, err := sql.Parse(query)
	if err != nil {
		t.Fatal(err)
	}
	if err := reg.RegisterView(name, q, false); err != nil {
		t.Fatal(err)
	}
}

// TestViewMaterializedOncePerQuery: a view referenced twice in one query (self
// join) scans its source once — the externally-visible-CTE behavior.
func TestViewMaterializedOncePerQuery(t *testing.T) {
	var scans int
	reg := countingRegistry(t, &scans) // source c1 = {1,2,2}
	registerView(t, reg, "v", "SELECT n FROM c1")
	rows := runQuery(t, reg, "SELECT a.n FROM v AS a JOIN v AS b ON a.n = b.n")
	// self-join on n: 1×1 + 2×2 = 5 rows.
	if len(rows) != 5 {
		t.Fatalf("rows = %d, want 5", len(rows))
	}
	if scans != 1 {
		t.Errorf("c1 scanned %d times, want 1 (view materializes once per query)", scans)
	}
}

// TestViewFreshPerQuery: unlike a materialized view, a regular view re-runs its
// query on every query that references it (it is not cached across queries).
func TestViewFreshPerQuery(t *testing.T) {
	var scans int
	reg := countingRegistry(t, &scans)
	registerView(t, reg, "v", "SELECT n FROM c1")
	runQuery(t, reg, "SELECT n FROM v")
	runQuery(t, reg, "SELECT n FROM v")
	if scans != 2 {
		t.Errorf("c1 scanned %d times across two queries, want 2 (view is not cached across queries)", scans)
	}
}

// TestViewScopeIsolation: a view binds in the global scope (sources + views),
// not the referencing query's CTEs. An outer CTE named like the view's source
// must not leak into the view body.
func TestViewScopeIsolation(t *testing.T) {
	var scans int
	reg := countingRegistry(t, &scans) // real source c1 = {1,2,2}
	registerView(t, reg, "v", "SELECT n FROM c1")
	// The outer CTE c1 would shadow the source if it leaked into the view.
	rows := runQuery(t, reg, "WITH c1 AS (SELECT 99 AS n) SELECT n FROM v")
	if len(rows) != 3 {
		t.Fatalf("rows = %d, want 3 (view must read source c1, not the outer CTE)", len(rows))
	}
	for _, r := range rows {
		if n, _ := r.Values[0].AsInt(); n == 99 {
			t.Error("view leaked the outer CTE c1")
		}
	}
}

// TestViewRecursionRejected: a view that references itself is rejected at plan
// time rather than looping.
func TestViewRecursionRejected(t *testing.T) {
	var scans int
	reg := countingRegistry(t, &scans)
	registerView(t, reg, "v", "SELECT n FROM v")
	stmt, err := sql.Parse("SELECT n FROM v")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Build(context.Background(), stmt, reg); err == nil {
		t.Error("expected error for a self-referential view")
	}
}

// TestViewOfView: a view may reference another view.
func TestViewOfView(t *testing.T) {
	var scans int
	reg := countingRegistry(t, &scans)
	registerView(t, reg, "base", "SELECT n FROM c1")
	registerView(t, reg, "filtered", "SELECT n FROM base WHERE n > 1")
	rows := runQuery(t, reg, "SELECT n FROM filtered")
	if len(rows) != 2 { // c1 {1,2,2}, n>1 keeps the two 2s
		t.Fatalf("rows = %d, want 2", len(rows))
	}
}
