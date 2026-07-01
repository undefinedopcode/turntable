package azlogsc

import (
	"context"
	"testing"

	"github.com/april/turntable/internal/connector"
	"github.com/april/turntable/internal/engine"
	"github.com/april/turntable/internal/sql"
)

// fakeLogs records the query it received and returns canned typed columns + rows.
type fakeLogs struct {
	cols     []logColumn
	rows     [][]any
	lastKQL  string
	lastWS   string
	lastSpan string
}

func (f *fakeLogs) query(ctx context.Context, workspace, kql, timespan string) ([]logColumn, [][]any, error) {
	f.lastKQL = kql
	f.lastWS = workspace
	f.lastSpan = timespan
	return f.cols, f.rows, nil
}

func drain(t *testing.T, it engine.RowIterator) []engine.Row {
	t.Helper()
	rows, err := engine.Materialize(context.Background(), it)
	if err != nil {
		t.Fatalf("materialize: %v", err)
	}
	return rows
}

func predicate(t *testing.T, where string) sql.Expr {
	t.Helper()
	stmt, err := sql.Parse("SELECT * FROM t WHERE " + where)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return stmt.(*sql.SelectStmt).Where
}

func sampleCols() []logColumn {
	return []logColumn{
		{"TimeGenerated", engine.TypeTime},
		{"Level", engine.TypeString},
		{"DurationMs", engine.TypeFloat},
		{"Count", engine.TypeInt},
	}
}

func TestResolveSchemaFromColumns(t *testing.T) {
	f := &fakeLogs{cols: sampleCols()}
	c := newWithClient(f)
	ds := connector.Dataset{Source: "AppRequests", Options: map[string]any{"workspace": "ws-1"}}
	sc, err := c.Resolve(context.Background(), ds)
	if err != nil {
		t.Fatal(err)
	}
	// Probe query bounded with take; schema mirrors the typed columns in order.
	if f.lastKQL != "AppRequests | take 1" {
		t.Errorf("probe kql = %q", f.lastKQL)
	}
	want := []struct {
		n string
		t engine.Type
	}{{"TimeGenerated", engine.TypeTime}, {"Level", engine.TypeString}, {"DurationMs", engine.TypeFloat}, {"Count", engine.TypeInt}}
	for i, w := range want {
		if sc.Columns[i].Name != w.n || sc.Columns[i].Type != w.t {
			t.Errorf("col %d = %+v, want %s/%v", i, sc.Columns[i], w.n, w.t)
		}
	}
}

func TestScanPushesKQLAndMapsTypedRows(t *testing.T) {
	f := &fakeLogs{
		cols: sampleCols(),
		rows: [][]any{
			{"2026-07-01T00:00:00Z", "Error", 12.5, float64(3)},
			{nil, "Info", nil, float64(1)},
		},
	}
	c := newWithClient(f)
	ds := connector.Dataset{Source: "AppRequests", Options: map[string]any{"workspace": "ws-1", "timespan": "PT6H"}}
	lim := 100
	rows := drain(t, mustScan(t, c, connector.ScanRequest{
		Dataset:   ds,
		Predicate: predicate(t, "Level = 'Error'"),
		Limit:     &lim,
	}))
	// Pushed KQL + timespan threaded through.
	want := `AppRequests | where Level == "Error" | take 100`
	if f.lastKQL != want {
		t.Errorf("kql = %q, want %q", f.lastKQL, want)
	}
	if f.lastWS != "ws-1" || f.lastSpan != "PT6H" {
		t.Errorf("workspace/timespan = %q/%q", f.lastWS, f.lastSpan)
	}
	// Typed row mapping: datetime -> time, real -> float, int, string.
	if rows[0].Values[0].Type != engine.TypeTime {
		t.Errorf("TimeGenerated should be time, got %v", rows[0].Values[0].Type)
	}
	if d, _ := rows[0].Values[2].AsFloat(); d != 12.5 {
		t.Errorf("DurationMs = %v, want 12.5", rows[0].Values[2].V)
	}
	if n, _ := rows[0].Values[3].AsInt(); n != 3 {
		t.Errorf("Count = %v, want 3", rows[0].Values[3].V)
	}
	// nulls preserved
	if !rows[1].Values[0].IsNull() || !rows[1].Values[2].IsNull() {
		t.Errorf("nil cells should be NULL")
	}
}

func TestRawQueryModeAndDefaults(t *testing.T) {
	f := &fakeLogs{cols: sampleCols()}
	c := newWithClient(f)
	raw := "AppRequests | summarize c = count() by Level"
	ds := connector.Dataset{Options: map[string]any{"workspace": "ws-1", "query": raw}}
	// Predicate ignored in raw mode; default timespan applied.
	_, err := c.Scan(context.Background(), connector.ScanRequest{Dataset: ds, Predicate: predicate(t, "Level = 'x'")})
	if err != nil {
		t.Fatal(err)
	}
	if f.lastKQL != raw {
		t.Errorf("raw kql = %q, want verbatim", f.lastKQL)
	}
	if f.lastSpan != "P1D" {
		t.Errorf("default timespan = %q, want P1D", f.lastSpan)
	}
}

func TestScanRequiresWorkspaceAndTable(t *testing.T) {
	c := newWithClient(&fakeLogs{cols: sampleCols()})
	// missing workspace
	if _, err := c.Scan(context.Background(), connector.ScanRequest{Dataset: connector.Dataset{Source: "AppRequests"}}); err == nil {
		t.Error("expected error without workspace")
	}
	// workspace but no table/query
	ds := connector.Dataset{Options: map[string]any{"workspace": "ws-1"}}
	if _, err := c.Scan(context.Background(), connector.ScanRequest{Dataset: ds}); err == nil {
		t.Error("expected error without table or query")
	}
}

func mustScan(t *testing.T, c *Connector, req connector.ScanRequest) engine.RowIterator {
	t.Helper()
	it, err := c.Scan(context.Background(), req)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	return it
}
