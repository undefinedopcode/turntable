package plan

import (
	"context"
	"testing"

	"github.com/april/turntable/internal/sql"
)

// These exercise three dialect additions end to end over the shared rowsConn
// fixture (s1 = {1,2,2}, s2 = {2,3}).

func TestCountDistinctExec(t *testing.T) {
	reg := rowsRegistry(t)
	// s1 = {1,2,2}: COUNT(*)=3, COUNT(n)=3, COUNT(DISTINCT n)=2.
	rows := runQuery(t, reg, "SELECT COUNT(DISTINCT n) AS d, COUNT(n) AS c FROM s1")
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	if d, _ := rows[0].Values[0].AsInt(); d != 2 {
		t.Errorf("COUNT(DISTINCT n) = %v, want 2", rows[0].Values[0].V)
	}
	if c, _ := rows[0].Values[1].AsInt(); c != 3 {
		t.Errorf("COUNT(n) = %v, want 3", rows[0].Values[1].V)
	}
}

func TestSumDistinctExec(t *testing.T) {
	reg := rowsRegistry(t)
	// s1 = {1,2,2}: SUM=5, SUM(DISTINCT)=3.
	rows := runQuery(t, reg, "SELECT SUM(DISTINCT n) AS sd, SUM(n) AS s FROM s1")
	if sd, _ := rows[0].Values[0].AsFloat(); sd != 3 {
		t.Errorf("SUM(DISTINCT n) = %v, want 3", rows[0].Values[0].V)
	}
	if s, _ := rows[0].Values[1].AsFloat(); s != 5 {
		t.Errorf("SUM(n) = %v, want 5", rows[0].Values[1].V)
	}
}

func TestOrderByOrdinalPlain(t *testing.T) {
	reg := rowsRegistry(t)
	// s1 = {1,2,2}; ORDER BY 1 DESC must reorder to 2,2,1.
	rows := runQuery(t, reg, "SELECT n FROM s1 ORDER BY 1 DESC")
	if len(rows) != 3 {
		t.Fatalf("rows = %d, want 3", len(rows))
	}
	got := make([]int64, len(rows))
	for i, r := range rows {
		got[i], _ = r.Values[0].AsInt()
	}
	want := []int64{2, 2, 1}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order = %v, want %v", got, want)
		}
	}
}

func TestOrderByOrdinalAggregate(t *testing.T) {
	reg := rowsRegistry(t)
	// GROUP BY n over {1,2,2} -> (1,1),(2,2); ORDER BY 2 DESC puts the count-2
	// group (n=2) first.
	rows := runQuery(t, reg, "SELECT n, COUNT(*) AS c FROM s1 GROUP BY 1 ORDER BY 2 DESC")
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(rows))
	}
	if n, _ := rows[0].Values[0].AsInt(); n != 2 {
		t.Errorf("first group n = %v, want 2 (highest count)", rows[0].Values[0].V)
	}
}

func TestOrderByOrdinalOutOfRange(t *testing.T) {
	reg := rowsRegistry(t)
	stmt, _ := sql.Parse("SELECT n FROM s1 ORDER BY 3")
	if _, err := Build(context.Background(), stmt, reg); err == nil {
		t.Fatal("expected out-of-range ordinal error")
	}
}

func TestUnionInDerivedTable(t *testing.T) {
	reg := rowsRegistry(t)
	// (s1 UNION s2) dedups {1,2,2,2,3} -> {1,2,3}; outer filter n>1 -> {2,3}.
	rows := runQuery(t, reg,
		"SELECT n FROM (SELECT n FROM s1 UNION SELECT n FROM s2) AS u WHERE n > 1 ORDER BY 1")
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2 ({2,3})", len(rows))
	}
	if v, _ := rows[0].Values[0].AsInt(); v != 2 {
		t.Errorf("row0 = %v, want 2", rows[0].Values[0].V)
	}
	if v, _ := rows[1].Values[0].AsInt(); v != 3 {
		t.Errorf("row1 = %v, want 3", rows[1].Values[0].V)
	}
}

func TestUnionAllInDerivedTable(t *testing.T) {
	reg := rowsRegistry(t)
	// UNION ALL keeps all 5 rows; aggregate over the derived table.
	rows := runQuery(t, reg,
		"SELECT COUNT(*) AS c FROM (SELECT n FROM s1 UNION ALL SELECT n FROM s2) AS u")
	if c, _ := rows[0].Values[0].AsInt(); c != 5 {
		t.Errorf("count = %v, want 5", rows[0].Values[0].V)
	}
}
