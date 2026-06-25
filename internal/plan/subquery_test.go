package plan

import (
	"context"
	"testing"

	"github.com/april/turntable/internal/engine"
	"github.com/april/turntable/internal/sql"
)

// Subqueries planned end to end over rowsConn: s1 = {1,2,2}, s2 = {2,3}.

func nList(t *testing.T, q string) []int64 {
	t.Helper()
	rows := runQuery(t, rowsRegistry(t), q)
	out := make([]int64, len(rows))
	for i, r := range rows {
		out[i], _ = r.Values[0].AsInt()
	}
	return out
}

func TestCorrelatedExists(t *testing.T) {
	// s1 values matched in s2: n=2 (twice); n=1 has no match.
	got := nList(t, "SELECT n FROM s1 AS a WHERE EXISTS (SELECT 1 FROM s2 AS b WHERE b.n = a.n)")
	if len(got) != 2 || got[0] != 2 || got[1] != 2 {
		t.Fatalf("EXISTS = %v, want [2 2]", got)
	}
}

func TestCorrelatedNotExists(t *testing.T) {
	got := nList(t, "SELECT n FROM s1 AS a WHERE NOT EXISTS (SELECT 1 FROM s2 AS b WHERE b.n = a.n)")
	if len(got) != 1 || got[0] != 1 {
		t.Fatalf("NOT EXISTS = %v, want [1]", got)
	}
}

func TestCorrelatedScalarInSelect(t *testing.T) {
	// Count of s2 rows matching each s1 row: 1->0, 2->1, 2->1.
	rows := runQuery(t, rowsRegistry(t),
		"SELECT n, (SELECT COUNT(*) FROM s2 AS b WHERE b.n = a.n) AS c FROM s1 AS a")
	want := map[int64]int64{1: 0, 2: 1}
	for _, r := range rows {
		n, _ := r.Values[0].AsInt()
		c, _ := r.Values[1].AsInt()
		if c != want[n] {
			t.Errorf("n=%d count=%d, want %d", n, c, want[n])
		}
	}
}

func TestNonCorrelatedScalarInWhere(t *testing.T) {
	// MIN(s2) = 2; keep s1 rows >= 2 -> {2,2}.
	got := nList(t, "SELECT n FROM s1 WHERE n >= (SELECT MIN(n) FROM s2)")
	if len(got) != 2 {
		t.Fatalf("rows = %v, want two 2s", got)
	}
}

func TestCorrelatedScalarComparison(t *testing.T) {
	// MAX of matching s2 rows: n=2 -> 2, n=1 -> NULL (no match). Keep where >= 2.
	got := nList(t, "SELECT n FROM s1 AS a WHERE (SELECT MAX(b.n) FROM s2 AS b WHERE b.n = a.n) >= 2")
	if len(got) != 2 || got[0] != 2 {
		t.Fatalf("rows = %v, want [2 2]", got)
	}
}

func TestCorrelatedIn(t *testing.T) {
	// s1.n IN (s2.n where n >= 2) = IN {2,3} -> {2,2}.
	got := nList(t, "SELECT n FROM s1 AS a WHERE a.n IN (SELECT b.n FROM s2 AS b WHERE b.n >= 2)")
	if len(got) != 2 || got[0] != 2 || got[1] != 2 {
		t.Fatalf("correlated IN = %v, want [2 2]", got)
	}
}

func TestScalarSubqueryTooManyRowsErrors(t *testing.T) {
	reg := rowsRegistry(t)
	// s2 has two rows; using it as a scalar must error at execution time.
	stmt, _ := sql.Parse("SELECT n FROM s1 WHERE n > (SELECT b.n FROM s2 AS b)")
	p, err := Build(context.Background(), stmt, reg)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	it, _, err := Exec(context.Background(), p)
	if err == nil {
		_, err = engine.Materialize(context.Background(), it)
	}
	if err == nil {
		t.Fatal("expected error: scalar subquery returned more than one row")
	}
}

func TestSubqueryWithGroupByRejected(t *testing.T) {
	reg := rowsRegistry(t)
	stmt, _ := sql.Parse("SELECT n, COUNT(*) FROM s1 AS a WHERE EXISTS (SELECT 1 FROM s2 AS b WHERE b.n = a.n) GROUP BY n")
	if _, err := Build(context.Background(), stmt, reg); err == nil {
		t.Fatal("expected error: subquery combined with GROUP BY")
	}
}
