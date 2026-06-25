package plan

import (
	"context"
	"testing"

	"github.com/april/turntable/internal/sql"
)

// INTERSECT/EXCEPT over rowsConn: s1 = {1,2,2}, s2 = {2,3}.

func setVals(t *testing.T, q string) []int64 {
	t.Helper()
	rows := runQuery(t, rowsRegistry(t), q)
	out := make([]int64, len(rows))
	for i, r := range rows {
		out[i], _ = r.Values[0].AsInt()
	}
	return out
}

func TestIntersectDistinct(t *testing.T) {
	// common distinct value is {2}.
	got := setVals(t, "SELECT n FROM s1 INTERSECT SELECT n FROM s2")
	if len(got) != 1 || got[0] != 2 {
		t.Fatalf("INTERSECT = %v, want [2]", got)
	}
}

func TestIntersectAll(t *testing.T) {
	// min(count_s1(2)=2, count_s2(2)=1) = 1 copy of 2.
	got := setVals(t, "SELECT n FROM s1 INTERSECT ALL SELECT n FROM s2")
	if len(got) != 1 || got[0] != 2 {
		t.Fatalf("INTERSECT ALL = %v, want [2]", got)
	}
}

func TestExceptDistinct(t *testing.T) {
	// s1 distinct values not in s2: {1}.
	got := setVals(t, "SELECT n FROM s1 EXCEPT SELECT n FROM s2 ORDER BY 1")
	if len(got) != 1 || got[0] != 1 {
		t.Fatalf("EXCEPT = %v, want [1]", got)
	}
}

func TestExceptAll(t *testing.T) {
	// multiplicities: 1 -> max(0,1-0)=1, 2 -> max(0,2-1)=1.
	got := setVals(t, "SELECT n FROM s1 EXCEPT ALL SELECT n FROM s2 ORDER BY 1")
	if len(got) != 2 || got[0] != 1 || got[1] != 2 {
		t.Fatalf("EXCEPT ALL = %v, want [1 2]", got)
	}
}

func TestSetOpPrecedence(t *testing.T) {
	// INTERSECT binds tighter: s1 UNION (s2 INTERSECT s2) = s1 UNION s2.
	got := setVals(t, "SELECT n FROM s1 UNION SELECT n FROM s2 INTERSECT SELECT n FROM s2 ORDER BY 1")
	if len(got) != 3 || got[0] != 1 || got[1] != 2 || got[2] != 3 {
		t.Fatalf("precedence result = %v, want [1 2 3]", got)
	}
}

func TestExceptThenUnionLeftAssoc(t *testing.T) {
	// (s1 EXCEPT s2) UNION s2 = {1} ∪ {2,3} = {1,2,3}.
	got := setVals(t, "SELECT n FROM s1 EXCEPT SELECT n FROM s2 UNION SELECT n FROM s2 ORDER BY 1")
	if len(got) != 3 {
		t.Fatalf("result = %v, want 3 rows {1,2,3}", got)
	}
}

func TestSetOpColumnCountMismatch(t *testing.T) {
	reg := rowsRegistry(t)
	stmt, _ := sql.Parse("SELECT n FROM s1 INTERSECT SELECT n, n FROM s2")
	if _, err := Build(context.Background(), stmt, reg); err == nil {
		t.Fatal("expected a column-count mismatch error")
	}
}
