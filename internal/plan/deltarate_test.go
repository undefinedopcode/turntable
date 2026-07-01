package plan

import (
	"math"
	"testing"
)

func TestDeltaWindow(t *testing.T) {
	// Station b: d1 -> 4, d2 -> 5 (input order is d2 first; window ORDER BY ts).
	rows := runQuery(t, readingsRegistry(t),
		"SELECT ts, DELTA(value) OVER (ORDER BY ts) AS d "+
			"FROM readings WHERE station = 'b' ORDER BY ts")
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(rows))
	}
	if !rows[0].Values[1].IsNull() {
		t.Errorf("first delta = %v, want NULL", rows[0].Values[1].V)
	}
	if d, _ := rows[1].Values[1].AsFloat(); d != 1 {
		t.Errorf("second delta = %v, want 1", rows[1].Values[1].V)
	}
}

func TestRateWindowTimestamps(t *testing.T) {
	// b: 4 -> 5 over one day = 1/86400 per second.
	rows := runQuery(t, readingsRegistry(t),
		"SELECT ts, RATE(value, ts) OVER (ORDER BY ts) AS r "+
			"FROM readings WHERE station = 'b' ORDER BY ts")
	if !rows[0].Values[1].IsNull() {
		t.Errorf("first rate = %v, want NULL", rows[0].Values[1].V)
	}
	got, _ := rows[1].Values[1].AsFloat()
	if want := 1.0 / 86400; math.Abs(got-want) > 1e-12 {
		t.Errorf("rate = %v, want %v", got, want)
	}
}

func TestRateCounterReset(t *testing.T) {
	// Counter drops 10 -> 3: Prometheus semantics treat the drop as a restart
	// from zero, so the increase is the new value (3), not -7. Numeric axis:
	// rate is per unit of t.
	rows := runQuery(t, readingsRegistry(t),
		"SELECT t, RATE(v, t) OVER (ORDER BY t) AS r FROM "+
			"(SELECT 1 AS t, 10 AS v UNION ALL SELECT 2, 3 UNION ALL SELECT 4, 9) x "+
			"ORDER BY t")
	if len(rows) != 3 {
		t.Fatalf("rows = %d, want 3", len(rows))
	}
	if !rows[0].Values[1].IsNull() {
		t.Errorf("first rate = %v, want NULL", rows[0].Values[1].V)
	}
	if r, _ := rows[1].Values[1].AsFloat(); r != 3 {
		t.Errorf("reset rate = %v, want 3 (increase = new value)", rows[1].Values[1].V)
	}
	if r, _ := rows[2].Values[1].AsFloat(); r != 3 { // (9-3)/(4-2)
		t.Errorf("normal rate = %v, want 3", rows[2].Values[1].V)
	}
}

func TestDeltaNullPropagates(t *testing.T) {
	// Station c: 7 then NULL — a NULL on either side of the pair yields NULL.
	rows := runQuery(t, readingsRegistry(t),
		"SELECT ts, DELTA(value) OVER (ORDER BY ts) AS d "+
			"FROM readings WHERE station = 'c' ORDER BY ts")
	if !rows[1].Values[1].IsNull() {
		t.Errorf("delta over NULL = %v, want NULL", rows[1].Values[1].V)
	}
}
