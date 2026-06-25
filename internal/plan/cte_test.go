package plan

import (
	"context"
	"testing"

	"github.com/april/turntable/internal/sql"
)

// CTEs are planned over the shared rowsConn fixture: s1 = {1,2,2}, s2 = {2,3}.

func TestCTEBasic(t *testing.T) {
	reg := rowsRegistry(t)
	rows := runQuery(t, reg, "WITH x AS (SELECT n FROM s1) SELECT n FROM x WHERE n > 1")
	if len(rows) != 2 { // s1 {1,2,2}, n>1 -> two 2s
		t.Fatalf("rows = %d, want 2", len(rows))
	}
}

func TestCTEFeedsAggregate(t *testing.T) {
	reg := rowsRegistry(t)
	rows := runQuery(t, reg, "WITH x AS (SELECT n FROM s1) SELECT COUNT(*) AS c, SUM(n) AS s FROM x")
	if c, _ := rows[0].Values[0].AsInt(); c != 3 {
		t.Errorf("count = %v, want 3", rows[0].Values[0].V)
	}
	if s, _ := rows[0].Values[1].AsFloat(); s != 5 {
		t.Errorf("sum = %v, want 5", rows[0].Values[1].V)
	}
}

func TestCTEChainedReference(t *testing.T) {
	reg := rowsRegistry(t)
	// b references a; the body references b.
	rows := runQuery(t, reg,
		"WITH a AS (SELECT n FROM s1), b AS (SELECT n FROM a WHERE n > 1) SELECT n FROM b")
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(rows))
	}
	for _, r := range rows {
		if v, _ := r.Values[0].AsInt(); v != 2 {
			t.Errorf("value = %v, want 2", r.Values[0].V)
		}
	}
}

func TestCTEReferencedTwice(t *testing.T) {
	reg := rowsRegistry(t)
	// Each reference builds its own iterator; a UNION ALL of the same CTE twice
	// yields 2x the rows.
	rows := runQuery(t, reg,
		"WITH x AS (SELECT n FROM s1) SELECT n FROM x UNION ALL SELECT n FROM x")
	if len(rows) != 6 { // 3 + 3
		t.Fatalf("rows = %d, want 6", len(rows))
	}
}

func TestCTEBodyUnion(t *testing.T) {
	reg := rowsRegistry(t)
	rows := runQuery(t, reg,
		"WITH x AS (SELECT n FROM s1) SELECT n FROM x UNION SELECT n FROM s2")
	// distinct of {1,2,2} ∪ {2,3} = {1,2,3}
	if len(rows) != 3 {
		t.Fatalf("rows = %d, want 3", len(rows))
	}
}

func TestCTEShadowsSource(t *testing.T) {
	reg := rowsRegistry(t)
	// A CTE named s1 shadows the registered source s1; here it exposes s2's rows.
	rows := runQuery(t, reg, "WITH s1 AS (SELECT n FROM s2) SELECT n FROM s1")
	if len(rows) != 2 { // s2 = {2,3}
		t.Fatalf("rows = %d, want 2 (the CTE, not the source)", len(rows))
	}
}

func TestCTERecursionRejected(t *testing.T) {
	reg := rowsRegistry(t)
	stmt, _ := sql.Parse("WITH r AS (SELECT n FROM r) SELECT n FROM r")
	if _, err := Build(context.Background(), stmt, reg); err == nil {
		t.Fatal("expected error for a recursive CTE")
	}
}

func TestCTEDuplicateNameRejected(t *testing.T) {
	reg := rowsRegistry(t)
	stmt, _ := sql.Parse("WITH a AS (SELECT n FROM s1), a AS (SELECT n FROM s2) SELECT n FROM a")
	if _, err := Build(context.Background(), stmt, reg); err == nil {
		t.Fatal("expected error for duplicate CTE name")
	}
}
