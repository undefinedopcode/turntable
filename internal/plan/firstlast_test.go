package plan

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/april/turntable/internal/connector"
	"github.com/april/turntable/internal/engine"
	"github.com/april/turntable/internal/sql"
)

// readingsConn is a small sensor-style fixture for FIRST/LAST/LOCF: per-station
// time-series rows (station, ts, value), deliberately NOT in chronological
// order so ordering by ts (not input order) is what's under test.
//
//	a: d3 -> 3.0, d1 -> 1.0 (no d2 row: a time gap), plus a NULL-ts row (99.0)
//	b: d2 -> 5.0, d1 -> 4.0
//	c: d1 -> 7.0, d2 -> NULL (latest value is NULL)
//	d: d1 -> NULL, d2 -> 8.0 (leading NULL for LOCF)
type readingsConn struct{}

var (
	d1 = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	d2 = time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	d3 = time.Date(2026, 1, 3, 0, 0, 0, 0, time.UTC)
)

func (readingsConn) Name() string { return "readings" }
func (readingsConn) Datasets(ctx context.Context) ([]connector.Dataset, error) {
	return nil, nil
}
func (readingsConn) Resolve(ctx context.Context, ds connector.Dataset) (engine.Schema, error) {
	return engine.Schema{Columns: []engine.Column{
		{Name: "station", Type: engine.TypeString, Nullable: true},
		{Name: "ts", Type: engine.TypeTime, Nullable: true},
		{Name: "value", Type: engine.TypeFloat, Nullable: true},
	}}, nil
}
func (readingsConn) Scan(ctx context.Context, req connector.ScanRequest) (engine.RowIterator, error) {
	row := func(st string, ts engine.Value, v engine.Value) engine.Row {
		return engine.Row{Values: []engine.Value{engine.StringVal(st), ts, v}}
	}
	rows := []engine.Row{
		row("a", engine.TimeVal(d3), engine.FloatVal(3)),
		row("a", engine.TimeVal(d1), engine.FloatVal(1)),
		row("a", engine.Null(), engine.FloatVal(99)), // NULL ts: skipped by FIRST/LAST
		row("b", engine.TimeVal(d2), engine.FloatVal(5)),
		row("b", engine.TimeVal(d1), engine.FloatVal(4)),
		row("c", engine.TimeVal(d1), engine.FloatVal(7)),
		row("c", engine.TimeVal(d2), engine.Null()), // latest value is NULL
		row("d", engine.TimeVal(d1), engine.Null()), // leading NULL for LOCF
		row("d", engine.TimeVal(d2), engine.FloatVal(8)),
	}
	return engine.NewSliceIter(rows), nil
}

func readingsRegistry(t *testing.T) *connector.Registry {
	t.Helper()
	reg := connector.NewRegistry()
	_ = reg.RegisterConnector(readingsConn{})
	if err := reg.RegisterSource("readings", readingsConn{}, connector.Dataset{Name: "readings"}); err != nil {
		t.Fatal(err)
	}
	return reg
}

func TestFirstLastGrouped(t *testing.T) {
	rows := runQuery(t, readingsRegistry(t),
		"SELECT station, FIRST(value, ts) AS f, LAST(value, ts) AS l "+
			"FROM readings GROUP BY station ORDER BY station")
	if len(rows) != 4 {
		t.Fatalf("rows = %d, want 4", len(rows))
	}
	check := func(i int, st string, first, last engine.Value) {
		t.Helper()
		if got := rows[i].Values[0].AsString(); got != st {
			t.Fatalf("row%d station = %q, want %q", i, got, st)
		}
		for j, want := range []engine.Value{first, last} {
			got := rows[i].Values[j+1]
			if want.IsNull() != got.IsNull() || (!want.IsNull() && engine.Compare(got, want) != 0) {
				t.Errorf("%s col%d = %v, want %v", st, j+1, got.V, want.V)
			}
		}
	}
	// a: input order d3-first must not matter; the NULL-ts 99 row is skipped.
	check(0, "a", engine.FloatVal(1), engine.FloatVal(3))
	check(1, "b", engine.FloatVal(4), engine.FloatVal(5))
	// c: the latest row's value is NULL, and LAST reports it honestly.
	check(2, "c", engine.FloatVal(7), engine.Null())
	check(3, "d", engine.Null(), engine.FloatVal(8))
}

func TestFirstLastEmptyGroupAndType(t *testing.T) {
	// A global aggregate over zero rows yields one NULL row.
	rows := runQuery(t, readingsRegistry(t),
		"SELECT FIRST(value, ts) AS f FROM readings WHERE station = 'zzz'")
	if len(rows) != 1 || !rows[0].Values[0].IsNull() {
		t.Fatalf("empty-input FIRST = %+v, want one NULL row", rows)
	}
	// FIRST/LAST preserve the argument's type (here: time, like MIN/MAX).
	rows = runQuery(t, readingsRegistry(t),
		"SELECT LAST(ts, ts) AS latest FROM readings")
	if rows[0].Values[0].Type != engine.TypeTime {
		t.Fatalf("LAST(ts, ts) type = %v, want time", rows[0].Values[0].Type)
	}
	if tv, _ := rows[0].Values[0].V.(time.Time); !tv.Equal(d3) {
		t.Fatalf("LAST(ts, ts) = %v, want %v", rows[0].Values[0].V, d3)
	}
}

func TestFirstLastArityError(t *testing.T) {
	stmt, err := sql.Parse("SELECT FIRST(value) FROM readings")
	if err != nil {
		t.Fatal(err)
	}
	p, err := Build(context.Background(), stmt, readingsRegistry(t))
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	it, _, err := Exec(context.Background(), p)
	if err == nil {
		_, err = engine.Materialize(context.Background(), it)
	}
	if err == nil || !strings.Contains(err.Error(), "expects 2 args") {
		t.Fatalf("err = %v, want arity error", err)
	}
}

func TestFirstLastAsWindow(t *testing.T) {
	// LAST(value, ts) OVER (PARTITION BY station) attaches the station's most
	// recent reading to every row (NULL-ts rows are skipped inside the frame).
	rows := runQuery(t, readingsRegistry(t),
		"SELECT station, LAST(value, ts) OVER (PARTITION BY station) AS latest "+
			"FROM readings WHERE station IN ('a', 'b') ORDER BY station")
	want := map[string]float64{"a": 3, "b": 5}
	if len(rows) != 5 {
		t.Fatalf("rows = %d, want 5", len(rows))
	}
	for i, r := range rows {
		st := r.Values[0].AsString()
		got, _ := r.Values[1].AsFloat()
		if got != want[st] {
			t.Errorf("row%d (%s) latest = %v, want %v", i, st, r.Values[1].V, want[st])
		}
	}
}

func TestLOCFPartitioned(t *testing.T) {
	// LOCF fills NULLs from the previous non-NULL value in window order, per
	// partition: c's NULL at d2 takes 7 (carried), d's leading NULL stays NULL.
	rows := runQuery(t, readingsRegistry(t),
		"SELECT station, ts, LOCF(value) OVER (PARTITION BY station ORDER BY ts) AS filled "+
			"FROM readings WHERE station IN ('c', 'd') ORDER BY station, ts")
	type exp struct {
		st   string
		null bool
		v    float64
	}
	want := []exp{
		{"c", false, 7}, {"c", false, 7}, // d2 carried forward
		{"d", true, 0}, {"d", false, 8}, // leading NULL stays NULL
	}
	if len(rows) != len(want) {
		t.Fatalf("rows = %d, want %d", len(rows), len(want))
	}
	for i, w := range want {
		got := rows[i].Values[2]
		if got.IsNull() != w.null {
			t.Errorf("row%d filled null = %v, want %v", i, got.IsNull(), w.null)
			continue
		}
		if !w.null {
			if f, _ := got.AsFloat(); f != w.v {
				t.Errorf("row%d filled = %v, want %v", i, got.V, w.v)
			}
		}
	}
}

func TestGapFillRecipe(t *testing.T) {
	// The documented gap-filling recipe: a generate_series time spine LEFT JOIN'd
	// to the readings, LOCF carrying the last value across the gap. Station a has
	// readings at d1 (1.0) and d3 (3.0) — nothing at d2.
	rows := runQuery(t, readingsRegistry(t),
		"SELECT g.t, LOCF(r.value) OVER (ORDER BY g.t) AS filled "+
			"FROM generate_series(CAST('2026-01-01' AS timestamp), CAST('2026-01-03' AS timestamp), INTERVAL '1 day') AS g(t) "+
			"LEFT JOIN readings r ON g.t = r.ts AND r.station = 'a' "+
			"ORDER BY g.t")
	if len(rows) != 3 {
		t.Fatalf("rows = %d, want 3", len(rows))
	}
	want := []float64{1, 1, 3} // d2 has no reading; LOCF carries d1's 1.0
	for i, w := range want {
		if f, _ := rows[i].Values[1].AsFloat(); f != w {
			t.Errorf("row%d filled = %v, want %v", i, rows[i].Values[1].V, w)
		}
	}

	// The full documented shape: spine LEFT JOIN a bucketed derived table
	// (2-arg DATE_BIN, origin defaulting to the epoch).
	rows = runQuery(t, readingsRegistry(t),
		"SELECT g.t, r.avg_flow AS measured, LOCF(r.avg_flow) OVER (ORDER BY g.t) AS filled "+
			"FROM generate_series(CAST('2026-01-01' AS timestamp), CAST('2026-01-03' AS timestamp), INTERVAL '1 day') AS g(t) "+
			"LEFT JOIN (SELECT DATE_BIN('1 day', ts) AS bucket, AVG(value) AS avg_flow "+
			"FROM readings WHERE station = 'a' GROUP BY bucket) r ON g.t = r.bucket "+
			"ORDER BY g.t")
	if len(rows) != 3 {
		t.Fatalf("recipe rows = %d, want 3", len(rows))
	}
	if !rows[1].Values[1].IsNull() {
		t.Errorf("d2 measured = %v, want NULL (the gap stays visible)", rows[1].Values[1].V)
	}
	for i, w := range want {
		if f, _ := rows[i].Values[2].AsFloat(); f != w {
			t.Errorf("recipe row%d filled = %v, want %v", i, rows[i].Values[2].V, w)
		}
	}
}
