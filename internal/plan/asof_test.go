package plan

import (
	"context"
	"strings"
	"testing"

	"github.com/april/turntable/internal/sql"
)

// ASOF joins planned end to end. The derived tables keep the fixtures inline:
// e = events (k, ts), r = readings (k, ts2, v).
const asofEvents = "(SELECT 1 AS k, 5 AS ts UNION ALL SELECT 1, 2 UNION ALL SELECT 2, 5) e"
const asofReadings = "(SELECT 1 AS k, 1 AS ts2, 10 AS v UNION ALL SELECT 1, 4, 40 UNION ALL SELECT 2, 6, 60) r"

func TestAsofJoinInner(t *testing.T) {
	// For each event, the latest reading at or before it, per key k.
	// e(1,5) -> r(1,4)=40; e(1,2) -> r(1,1)=10; e(2,5): only r(2,6) is later -> dropped.
	rows := runQuery(t, rowsRegistry(t),
		"SELECT e.k, e.ts, r.v FROM "+asofEvents+
			" ASOF JOIN "+asofReadings+
			" ON e.ts >= r.ts2 AND e.k = r.k ORDER BY e.k, e.ts")
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2 (inner drops the unmatched event)", len(rows))
	}
	if v, _ := rows[0].Values[2].AsInt(); v != 10 {
		t.Errorf("event (1,2) v = %v, want 10", rows[0].Values[2].V)
	}
	if v, _ := rows[1].Values[2].AsInt(); v != 40 {
		t.Errorf("event (1,5) v = %v, want 40", rows[1].Values[2].V)
	}
}

func TestAsofJoinLeft(t *testing.T) {
	rows := runQuery(t, rowsRegistry(t),
		"SELECT e.k, e.ts, r.v FROM "+asofEvents+
			" ASOF LEFT JOIN "+asofReadings+
			" ON e.ts >= r.ts2 AND e.k = r.k ORDER BY e.k, e.ts")
	if len(rows) != 3 {
		t.Fatalf("rows = %d, want 3 (LEFT keeps the unmatched event)", len(rows))
	}
	if !rows[2].Values[2].IsNull() {
		t.Errorf("event (2,5) v = %v, want NULL (no reading at/before it)", rows[2].Values[2].V)
	}
}

func TestAsofJoinReversedOperandsAndDirections(t *testing.T) {
	// r.ts2 <= e.ts is the same condition with the operands swapped.
	rows := runQuery(t, rowsRegistry(t),
		"SELECT e.ts, r.v FROM "+asofEvents+
			" ASOF JOIN "+asofReadings+
			" ON r.ts2 <= e.ts AND e.k = r.k ORDER BY e.ts")
	if len(rows) != 2 {
		t.Fatalf("swapped operands: rows = %d, want 2", len(rows))
	}
	// <= matches the other direction: the earliest reading at or after the event.
	// e(1,2) -> r(1,4)=40; e(1,5): none at/after -> dropped; e(2,5) -> r(2,6)=60.
	rows = runQuery(t, rowsRegistry(t),
		"SELECT e.k, e.ts, r.v FROM "+asofEvents+
			" ASOF JOIN "+asofReadings+
			" ON e.ts <= r.ts2 AND e.k = r.k ORDER BY e.k, e.ts")
	if len(rows) != 2 {
		t.Fatalf("<= rows = %d, want 2", len(rows))
	}
	if v, _ := rows[0].Values[2].AsInt(); v != 40 {
		t.Errorf("<= event (1,2) v = %v, want 40", rows[0].Values[2].V)
	}
	if v, _ := rows[1].Values[2].AsInt(); v != 60 {
		t.Errorf("<= event (2,5) v = %v, want 60", rows[1].Values[2].V)
	}
}

func TestAsofJoinStrictInequality(t *testing.T) {
	// With an exact-timestamp reading present, >= takes it and > takes the prior one.
	events := "(SELECT 1 AS k, 4 AS ts) e"
	rows := runQuery(t, rowsRegistry(t),
		"SELECT r.v FROM "+events+" ASOF JOIN "+asofReadings+
			" ON e.ts >= r.ts2 AND e.k = r.k")
	if v, _ := rows[0].Values[0].AsInt(); v != 40 {
		t.Errorf(">= at equal ts v = %v, want 40", rows[0].Values[0].V)
	}
	rows = runQuery(t, rowsRegistry(t),
		"SELECT r.v FROM "+events+" ASOF JOIN "+asofReadings+
			" ON e.ts > r.ts2 AND e.k = r.k")
	if v, _ := rows[0].Values[0].AsInt(); v != 10 {
		t.Errorf("> at equal ts v = %v, want 10 (strictly before)", rows[0].Values[0].V)
	}
}

func TestAsofJoinNoEqualityKeys(t *testing.T) {
	// Equality conjuncts are optional: one global group.
	rows := runQuery(t, rowsRegistry(t),
		"SELECT e.ts, r.v FROM (SELECT 5 AS ts) e ASOF JOIN "+asofReadings+
			" ON e.ts >= r.ts2")
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	if v, _ := rows[0].Values[1].AsInt(); v != 40 { // greatest ts2 <= 5 is 4
		t.Errorf("v = %v, want 40", rows[0].Values[1].V)
	}
}

func TestAsofJoinTimestamps(t *testing.T) {
	// A time-typed axis: for each day in the spine, the latest reading of
	// station a at or before it (readings at d1=1.0 and d3=3.0).
	rows := runQuery(t, readingsRegistry(t),
		"SELECT g.t, r.value FROM "+
			"generate_series(CAST('2026-01-01' AS timestamp), CAST('2026-01-03' AS timestamp), INTERVAL '1 day') AS g(t) "+
			"ASOF JOIN (SELECT ts, value FROM readings WHERE station = 'a') r "+
			"ON g.t >= r.ts ORDER BY g.t")
	want := []float64{1, 1, 3}
	if len(rows) != 3 {
		t.Fatalf("rows = %d, want 3", len(rows))
	}
	for i, w := range want {
		if f, _ := rows[i].Values[1].AsFloat(); f != w {
			t.Errorf("day%d value = %v, want %v", i+1, rows[i].Values[1].V, w)
		}
	}
}

func TestAsofJoinErrors(t *testing.T) {
	reg := rowsRegistry(t)
	cases := []struct{ q, wantErr string }{
		{"SELECT * FROM " + asofEvents + " ASOF JOIN " + asofReadings + " ON e.k = r.k",
			"needs an inequality"},
		{"SELECT * FROM " + asofEvents + " ASOF JOIN " + asofReadings + " ON e.ts >= r.ts2 AND e.ts <= r.ts2",
			"exactly one inequality"},
		{"SELECT * FROM " + asofEvents + " ASOF JOIN " + asofReadings + " ON e.ts >= r.ts2 AND e.k + 1 = r.k",
			"supports column equalities"},
	}
	for _, c := range cases {
		stmt, err := sql.Parse(c.q)
		if err != nil {
			t.Fatalf("parse %q: %v", c.q, err)
		}
		_, err = Build(context.Background(), stmt, reg)
		if err == nil || !strings.Contains(err.Error(), c.wantErr) {
			t.Errorf("%q: err = %v, want contains %q", c.q, err, c.wantErr)
		}
	}
	// ASOF RIGHT JOIN is a parse error.
	if _, err := sql.Parse("SELECT * FROM a ASOF RIGHT JOIN b ON a.t >= b.t"); err == nil ||
		!strings.Contains(err.Error(), "ASOF") {
		t.Errorf("ASOF RIGHT JOIN err = %v, want ASOF restriction", err)
	}
}
