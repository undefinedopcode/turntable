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
