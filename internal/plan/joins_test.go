package plan

import (
	"testing"

	"github.com/april/turntable/internal/engine"
)

// Outer joins planned end to end over rowsConn: s1 = {1,2,2}, s2 = {2,3}.
// Joining on n: key 2 matches (two s1 rows × one s2 row = 2), s1's 1 and s2's 3
// are unmatched.

func joinRows(t *testing.T, kind string) []engine.Row {
	t.Helper()
	q := "SELECT a.n AS an, b.n AS bn FROM s1 AS a " + kind + " JOIN s2 AS b ON a.n = b.n"
	return runQuery(t, rowsRegistry(t), q)
}

func TestInnerJoinExec(t *testing.T) {
	if got := len(joinRows(t, "INNER")); got != 2 {
		t.Fatalf("inner rows = %d, want 2", got)
	}
}

func TestLeftJoinExec(t *testing.T) {
	rows := joinRows(t, "LEFT")
	if len(rows) != 3 { // 2 matches + unmatched left n=1
		t.Fatalf("left rows = %d, want 3", len(rows))
	}
	// The unmatched left row has a non-null an and a NULL bn.
	var unmatched int
	for _, r := range rows {
		if !r.Values[0].IsNull() && r.Values[1].IsNull() {
			unmatched++
		}
	}
	if unmatched != 1 {
		t.Errorf("left join unmatched-left rows = %d, want 1", unmatched)
	}
}

func TestRightJoinExec(t *testing.T) {
	rows := joinRows(t, "RIGHT")
	if len(rows) != 3 { // 2 matches + unmatched right n=3
		t.Fatalf("right rows = %d, want 3", len(rows))
	}
	// The unmatched right row (n=3) has NULL an and bn=3.
	var ok bool
	for _, r := range rows {
		if r.Values[0].IsNull() {
			if v, _ := r.Values[1].AsInt(); v == 3 {
				ok = true
			}
		}
	}
	if !ok {
		t.Error("right join: expected NULL-padded left for unmatched right n=3")
	}
}

func TestFullJoinExec(t *testing.T) {
	rows := joinRows(t, "FULL")
	if len(rows) != 4 { // 2 matches + unmatched left (1) + unmatched right (3)
		t.Fatalf("full rows = %d, want 4", len(rows))
	}
	var nullLeft, nullRight int
	for _, r := range rows {
		if r.Values[0].IsNull() {
			nullLeft++
		}
		if r.Values[1].IsNull() {
			nullRight++
		}
	}
	if nullLeft != 1 || nullRight != 1 {
		t.Errorf("full join: nullLeft=%d nullRight=%d, want 1 and 1", nullLeft, nullRight)
	}
}

func TestFullOuterKeywordExec(t *testing.T) {
	// The optional OUTER keyword parses and behaves identically.
	if got := len(joinRows(t, "FULL OUTER")); got != 4 {
		t.Fatalf("full outer rows = %d, want 4", got)
	}
}

// TestNonEquiJoinExec exercises a pure non-equality ON (no equi-key), which the
// planner lowers to a nested-loop join. s1 = {1,2,2}, s2 = {2,3}; a.n < b.n
// pairs: 1<{2,3}=2, 2<{3}=1, 2<{3}=1 → 4 rows.
func TestNonEquiJoinExec(t *testing.T) {
	reg := rowsRegistry(t)
	rows := runQuery(t, reg, "SELECT a.n AS an, b.n AS bn FROM s1 AS a JOIN s2 AS b ON a.n < b.n")
	if len(rows) != 4 {
		t.Fatalf("non-equi join rows = %d, want 4", len(rows))
	}
}

// TestResidualJoinExec exercises an equi-key plus a non-equi residual conjunct.
// s1 = {1,2,2}, s2 = {2,3}; a.n = b.n matches the two n=2 rows, and the residual
// a.n > 1 keeps both. With a.n > 5 the residual rejects all → 0.
func TestResidualJoinExec(t *testing.T) {
	reg := rowsRegistry(t)
	if rows := runQuery(t, reg, "SELECT a.n FROM s1 AS a JOIN s2 AS b ON a.n = b.n AND a.n > 1"); len(rows) != 2 {
		t.Fatalf("equi+residual rows = %d, want 2", len(rows))
	}
	if rows := runQuery(t, reg, "SELECT a.n FROM s1 AS a JOIN s2 AS b ON a.n = b.n AND a.n > 5"); len(rows) != 0 {
		t.Fatalf("equi+failing-residual rows = %d, want 0", len(rows))
	}
}

// TestLeftJoinResidualExec verifies a residual that rejects every keyed match
// still keeps the left rows, NULL-padded. s1 = {1,2,2}; with a.n = b.n AND
// a.n > 5 nothing matches, so all three left rows survive with a NULL right.
func TestLeftJoinResidualExec(t *testing.T) {
	reg := rowsRegistry(t)
	rows := runQuery(t, reg, "SELECT a.n AS an, b.n AS bn FROM s1 AS a LEFT JOIN s2 AS b ON a.n = b.n AND a.n > 5")
	if len(rows) != 3 {
		t.Fatalf("left join residual rows = %d, want 3", len(rows))
	}
	for _, r := range rows {
		if !r.Values[1].IsNull() {
			t.Errorf("expected NULL right column, got %v", r.Values[1])
		}
	}
}
