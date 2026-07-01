package sqlc

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/april/turntable/internal/connector"
	"github.com/april/turntable/internal/engine"
	"github.com/april/turntable/internal/plan"
	oparseSQL "github.com/april/turntable/internal/sql"
)

func TestBuildAggQueryDialects(t *testing.T) {
	agg := connector.AggregateRequest{
		GroupBy: []connector.AggregateGroup{
			{Column: "station", Alias: "station"},
			{Column: "ts", Stride: 5 * time.Minute, Alias: "bucket"},
		},
		Aggregates: []connector.AggregateOp{
			{Func: "AVG", Column: "flow", Alias: "avg_flow"},
			{Func: "COUNT", Column: "", Alias: "n"},
		},
	}
	table := tableRef{name: "readings"}

	cases := []struct {
		driver string
		want   []string // substrings that must appear
		ok     bool
	}{
		{"sqlite", []string{
			`datetime((CAST(strftime('%s', "ts") AS INTEGER) / 300) * 300, 'unixepoch') AS "bucket"`,
			`AVG("flow") AS "avg_flow"`, `COUNT(*) AS "n"`, `GROUP BY "station", datetime(`,
		}, true},
		{"postgres", []string{`to_timestamp(floor(extract(epoch from "ts") / 300) * 300) AS "bucket"`}, true},
		{"mysql", []string{"from_unixtime(floor(unix_timestamp(`ts`) / 300) * 300) AS `bucket`"}, true},
		{"sqlserver", nil, false}, // DATEDIFF int overflow risk: declined
	}
	for _, c := range cases {
		q, ok := buildAggQuery(agg, table, dialectFor(c.driver))
		if ok != c.ok {
			t.Errorf("%s: ok = %v, want %v (q=%s)", c.driver, ok, c.ok, q)
			continue
		}
		for _, w := range c.want {
			if !strings.Contains(q, w) {
				t.Errorf("%s query missing %q:\n%s", c.driver, w, q)
			}
		}
	}
}

func TestBuildAggQueryPredicate(t *testing.T) {
	table := tableRef{name: "t"}
	agg := connector.AggregateRequest{
		GroupBy:    []connector.AggregateGroup{{Column: "k", Alias: "k"}},
		Aggregates: []connector.AggregateOp{{Func: "COUNT", Alias: "n"}},
	}

	// An exactly-translatable predicate is rendered into the WHERE.
	where, err := oparseSQL.Parse("SELECT 1 FROM x WHERE flow > 5 AND station = 'a'")
	if err != nil {
		t.Fatal(err)
	}
	agg.Predicate = where.(*oparseSQL.SelectStmt).Where
	q, ok := buildAggQuery(agg, table, dialectFor("sqlite"))
	if !ok || !strings.Contains(q, `WHERE (("flow" > 5) AND ("station" = 'a'))`) {
		t.Errorf("predicate not rendered: ok=%v q=%s", ok, q)
	}

	// A case-sensitive LIKE on sqlite is only a superset filter — after
	// aggregation the engine cannot refine it, so the whole request declines.
	likeStmt, err := oparseSQL.Parse("SELECT 1 FROM x WHERE station LIKE 'N-%'")
	if err != nil {
		t.Fatal(err)
	}
	agg.Predicate = likeStmt.(*oparseSQL.SelectStmt).Where
	if _, ok := buildAggQuery(agg, table, dialectFor("sqlite")); ok {
		t.Error("inexact LIKE predicate must decline aggregate pushdown")
	}
	// Postgres LIKE is exact, so the same request pushes there.
	if _, ok := buildAggQuery(agg, table, dialectFor("postgres")); !ok {
		t.Error("exact LIKE on postgres should push")
	}
}

func TestAggregateSchemaValidation(t *testing.T) {
	base := engine.Schema{Columns: []engine.Column{
		{Name: "station", Type: engine.TypeString},
		{Name: "ts", Type: engine.TypeTime},
		{Name: "flow", Type: engine.TypeFloat},
	}}
	ok := func(agg connector.AggregateRequest) bool {
		_, k := aggregateSchema(agg, base)
		return k
	}
	grp := func(g ...connector.AggregateGroup) connector.AggregateRequest {
		return connector.AggregateRequest{GroupBy: g, Aggregates: []connector.AggregateOp{{Func: "COUNT", Alias: "n"}}}
	}
	if !ok(grp(connector.AggregateGroup{Column: "station", Alias: "station"})) {
		t.Error("plain column should be accepted")
	}
	if !ok(grp(connector.AggregateGroup{Column: "ts", Stride: time.Hour, Alias: "b"})) {
		t.Error("whole-second bucket should be accepted")
	}
	if ok(grp(connector.AggregateGroup{Column: "ts", Stride: 500 * time.Millisecond, Alias: "b"})) {
		t.Error("sub-second stride must decline")
	}
	if ok(grp(connector.AggregateGroup{Column: "nope", Alias: "nope"})) {
		t.Error("unknown column must decline")
	}
	if ok(connector.AggregateRequest{Aggregates: []connector.AggregateOp{{Func: "MEDIAN", Column: "flow", Alias: "m"}}}) {
		t.Error("MEDIAN must decline (engine computes it)")
	}
	// Output typing: bucket -> time, AVG -> float, MIN preserves column type.
	schema, k := aggregateSchema(connector.AggregateRequest{
		GroupBy: []connector.AggregateGroup{{Column: "ts", Stride: time.Hour, Alias: "b"}},
		Aggregates: []connector.AggregateOp{
			{Func: "AVG", Column: "flow", Alias: "a"},
			{Func: "MIN", Column: "station", Alias: "m"},
		},
	}, base)
	if !k || schema.Columns[0].Type != engine.TypeTime ||
		schema.Columns[1].Type != engine.TypeFloat || schema.Columns[2].Type != engine.TypeString {
		t.Errorf("schema = %+v", schema.Columns)
	}
}

// TestSQLiteDateBinPushdownEndToEnd drives a DATE_BIN GROUP BY through the
// planner against a real in-memory SQLite table and checks (a) the plan pushed
// the aggregation into the Scan (no engine Aggregate node) and (b) the bucket
// boundaries and aggregates match the engine's DATE_BIN semantics.
func TestSQLiteDateBinPushdownEndToEnd(t *testing.T) {
	ctx := context.Background()
	dsn := "file:aggpushtest?mode=memory&cache=shared"
	ds := connector.Dataset{Name: "readings", Source: "readings",
		Options: map[string]any{"driver": "sqlite", "dsn": dsn}}
	db, _, _, err := openAndTable(ds)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close() // hold the shared in-memory DB open across scans

	if _, err := db.Exec(`CREATE TABLE readings (station TEXT, ts TIMESTAMP, flow REAL)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO readings VALUES
		('a', '2026-01-01 10:05:00', 1.0),
		('a', '2026-01-01 10:35:00', 3.0),
		('a', '2026-01-01 11:10:00', 5.0),
		('b', '2026-01-01 10:20:00', 7.0)`); err != nil {
		t.Fatal(err)
	}

	reg := connector.NewRegistry()
	conn := New()
	_ = reg.RegisterConnector(conn)
	if err := reg.RegisterSource("readings", conn, ds); err != nil {
		t.Fatal(err)
	}

	q := "SELECT station, DATE_BIN('1 hour', ts) AS bucket, AVG(flow) AS avg_flow, COUNT(*) AS n " +
		"FROM readings WHERE flow > 0 GROUP BY station, DATE_BIN('1 hour', ts) " +
		"ORDER BY station, bucket"
	stmt, err := oparseSQL.Parse(q)
	if err != nil {
		t.Fatal(err)
	}
	p, err := plan.Build(ctx, stmt, reg)
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	// The tree must contain a Scan carrying the aggregate and no engine
	// Aggregate node.
	var sawAggScan, sawEngineAgg bool
	var walk func(n plan.Node)
	walk = func(n plan.Node) {
		switch node := n.(type) {
		case *plan.Scan:
			sawAggScan = sawAggScan || node.Aggregate != nil
		case *plan.Aggregate:
			sawEngineAgg = true
			walk(node.Child)
		case *plan.Project:
			walk(node.Child)
		case *plan.Filter:
			walk(node.Child)
		case *plan.Sort:
			walk(node.Child)
		case *plan.Limit:
			walk(node.Child)
		}
	}
	walk(p.Root)
	if !sawAggScan || sawEngineAgg {
		t.Fatalf("pushdown did not happen: aggScan=%v engineAgg=%v", sawAggScan, sawEngineAgg)
	}

	it, schema, err := plan.Exec(ctx, p)
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	rows, err := engine.Materialize(ctx, it)
	if err != nil {
		t.Fatalf("materialize: %v", err)
	}
	if schema.Columns[1].Name != "bucket" || schema.Columns[1].Type != engine.TypeTime {
		t.Errorf("bucket column = %+v, want time", schema.Columns[1])
	}
	type want struct {
		station, bucket string
		avg             float64
		n               int64
	}
	wants := []want{
		{"a", "2026-01-01 10:00:00", 2, 2},
		{"a", "2026-01-01 11:00:00", 5, 1},
		{"b", "2026-01-01 10:00:00", 7, 1},
	}
	if len(rows) != len(wants) {
		t.Fatalf("rows = %d, want %d: %+v", len(rows), len(wants), rows)
	}
	for i, w := range wants {
		if got := rows[i].Values[0].AsString(); got != w.station {
			t.Errorf("row%d station = %q, want %q", i, got, w.station)
		}
		if got := rows[i].Values[1].AsString(); got != w.bucket {
			t.Errorf("row%d bucket = %q, want %q", i, got, w.bucket)
		}
		if got, _ := rows[i].Values[2].AsFloat(); got != w.avg {
			t.Errorf("row%d avg = %v, want %v", i, rows[i].Values[2].V, w.avg)
		}
		if got, _ := rows[i].Values[3].AsInt(); got != w.n {
			t.Errorf("row%d n = %v, want %v", i, rows[i].Values[3].V, w.n)
		}
	}

	// An unpushable aggregate (MEDIAN) falls back to engine aggregation over
	// the raw rows and still answers correctly.
	q2 := "SELECT station, MEDIAN(flow) AS med FROM readings GROUP BY station ORDER BY station"
	stmt2, err := oparseSQL.Parse(q2)
	if err != nil {
		t.Fatal(err)
	}
	p2, err := plan.Build(ctx, stmt2, reg)
	if err != nil {
		t.Fatalf("build fallback: %v", err)
	}
	it2, _, err := plan.Exec(ctx, p2)
	if err != nil {
		t.Fatalf("exec fallback: %v", err)
	}
	rows2, err := engine.Materialize(ctx, it2)
	if err != nil {
		t.Fatalf("materialize fallback: %v", err)
	}
	if len(rows2) != 2 {
		t.Fatalf("fallback rows = %d, want 2", len(rows2))
	}
	if m, _ := rows2[0].Values[1].AsFloat(); m != 3 { // a: median(1,3,5)
		t.Errorf("fallback median = %v, want 3", rows2[0].Values[1].V)
	}
}
