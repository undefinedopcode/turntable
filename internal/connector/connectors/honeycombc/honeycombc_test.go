package honeycombc

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/april/turntable/internal/connector"
	"github.com/april/turntable/internal/engine"
	"github.com/april/turntable/internal/plan"
	"github.com/april/turntable/internal/sql"
)

// fakeHoney serves canned responses keyed by "METHOD path" and records the last
// request body per key (so query-spec translation can be asserted).
type fakeHoney struct {
	resp   map[string]string // "METHOD path" -> JSON response
	errs   map[string]error  // "METHOD path" -> error to return (overrides resp)
	bodies map[string]any    // "METHOD path" -> decoded last request body
}

func (f *fakeHoney) do(ctx context.Context, method, path string, body any, v2 bool) ([]byte, error) {
	key := method + " " + path
	if e, ok := f.errs[key]; ok {
		return nil, e
	}
	if body != nil {
		if f.bodies == nil {
			f.bodies = map[string]any{}
		}
		// round-trip through JSON so assertions see the serialized shape.
		b, _ := json.Marshal(body)
		var decoded any
		_ = json.Unmarshal(b, &decoded)
		f.bodies[key] = decoded
	}
	r, ok := f.resp[key]
	if !ok {
		return nil, &notFound{key}
	}
	return []byte(r), nil
}

type notFound struct{ key string }

func (e *notFound) Error() string { return "no canned response for " + e.key }

func drain(t *testing.T, it engine.RowIterator) []engine.Row {
	t.Helper()
	rows, err := engine.Materialize(context.Background(), it)
	if err != nil {
		t.Fatalf("materialize: %v", err)
	}
	return rows
}

func TestMetaDatasetsScan(t *testing.T) {
	f := &fakeHoney{resp: map[string]string{
		"GET /1/datasets": `[
			{"name":"Prod","slug":"prod","regular_columns_count":42,"last_written_at":"2026-06-01T00:00:00Z"},
			{"name":"Staging","slug":"staging"}
		]`,
	}}
	c := newWithClient(f)
	ds := connector.Dataset{Source: "datasets", Options: map[string]any{"api_key": "k"}}
	sc, err := c.Resolve(context.Background(), ds)
	if err != nil {
		t.Fatal(err)
	}
	if sc.Columns[0].Name != "name" || sc.Columns[3].Name != "columns_count" {
		t.Fatalf("unexpected schema: %+v", sc.Columns)
	}
	rows := drain(t, mustScan(t, c, connector.ScanRequest{Dataset: ds}))
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(rows))
	}
	// columns_count is int; missing on row 2 -> NULL.
	if n, _ := rows[0].Values[3].AsInt(); n != 42 {
		t.Errorf("columns_count = %v, want 42", rows[0].Values[3].V)
	}
	if !rows[1].Values[3].IsNull() {
		t.Errorf("missing columns_count should be NULL")
	}
	if rows[0].Values[5].Type != engine.TypeTime {
		t.Errorf("last_written_at should coerce to time, got %v", rows[0].Values[5].Type)
	}
}

func TestEnvironmentsUnwrapsJSONAPI(t *testing.T) {
	f := &fakeHoney{resp: map[string]string{
		"GET /2/teams/myteam/environments": `{"data":[
			{"id":"e1","attributes":{"name":"prod","slug":"prod","color":"blue"}}
		]}`,
	}}
	c := newWithClient(f)
	ds := connector.Dataset{Source: "environments", Options: map[string]any{"management_key": "id:sec", "team": "myteam"}}
	rows := drain(t, mustScan(t, c, connector.ScanRequest{Dataset: ds}))
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	if rows[0].Values[0].V != "e1" || rows[0].Values[1].V != "prod" || rows[0].Values[4].V != "blue" {
		t.Errorf("unexpected env row: %+v", rows[0].Values)
	}
}

func TestEventsSchemaFromColumns(t *testing.T) {
	f := &fakeHoney{resp: map[string]string{
		"GET /1/columns/prod": `[
			{"key_name":"duration_ms","type":"float"},
			{"key_name":"service.name","type":"string"},
			{"key_name":"http.status_code","type":"integer"}
		]`,
	}}
	c := newWithClient(f)
	ds := connector.Dataset{Options: map[string]any{"api_key": "k", "dataset": "prod"}}
	sc, err := c.Resolve(context.Background(), ds)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]engine.Type{}
	for _, col := range sc.Columns {
		got[col.Name] = col.Type
	}
	if got["duration_ms"] != engine.TypeFloat || got["service.name"] != engine.TypeString || got["http.status_code"] != engine.TypeInt {
		t.Fatalf("unexpected event schema types: %+v", got)
	}
}

func TestNonAggregateEventsScanErrors(t *testing.T) {
	c := newWithClient(&fakeHoney{})
	ds := connector.Dataset{Options: map[string]any{"api_key": "k", "dataset": "prod"}}
	if _, err := c.Scan(context.Background(), connector.ScanRequest{Dataset: ds}); err == nil {
		t.Fatal("expected error scanning events without an aggregate")
	}
}

// TestEventsAggregatePushdownEndToEnd runs a real SQL aggregate query through the
// planner and engine with the connector registered, asserting: the engine's
// Aggregate/WHERE is elided (the connector receives the pushed aggregation), the
// query spec is translated correctly (calculations, breakdowns, filters), dotted
// attribute names resolve, and the aggregated rows come back typed.
func TestEventsAggregatePushdownEndToEnd(t *testing.T) {
	f := &fakeHoney{resp: map[string]string{
		"GET /1/columns/prod": `[
			{"key_name":"duration_ms","type":"float"},
			{"key_name":"service.name","type":"string"}
		]`,
		"POST /1/queries/prod":       `{"id":"q1"}`,
		"POST /1/query_results/prod": `{"id":"r1","complete":false}`,
		"GET /1/query_results/prod/r1": `{"id":"r1","complete":true,"data":{"results":[
			{"data":{"service.name":"api","COUNT":10,"AVG(duration_ms)":12.5}},
			{"data":{"service.name":"web","COUNT":3,"AVG(duration_ms)":4.0}}
		]}}`,
	}}
	c := newWithClient(f)

	reg := connector.NewRegistry()
	if err := reg.RegisterConnector(c); err != nil {
		t.Fatal(err)
	}
	ds := connector.Dataset{Name: "events", Options: map[string]any{"api_key": "k", "dataset": "prod"}}
	if err := reg.RegisterSource("events", c, ds); err != nil {
		t.Fatal(err)
	}

	q := `SELECT service.name, COUNT(*) AS n, AVG(duration_ms) AS avg_dur
	      FROM events
	      WHERE duration_ms > 100
	      GROUP BY service.name
	      HAVING COUNT(*) > 5
	      ORDER BY n DESC`
	stmt, err := sql.Parse(q)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	p, err := plan.Build(context.Background(), stmt, reg)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	it, schema, err := plan.Exec(context.Background(), p)
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	rows := drain(t, it)

	// Output schema types: service.name string, n int, avg_dur float.
	types := map[string]engine.Type{}
	for _, col := range schema.Columns {
		types[col.Name] = col.Type
	}
	if types["n"] != engine.TypeInt || types["avg_dur"] != engine.TypeFloat || types["service.name"] != engine.TypeString {
		t.Errorf("unexpected output types: %+v", types)
	}

	// HAVING COUNT(*) > 5 drops the web row (3); ORDER BY n DESC keeps api first.
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1 (HAVING should drop web)", len(rows))
	}
	if rows[0].Values[0].V != "api" {
		t.Errorf("row[0] service.name = %v, want api", rows[0].Values[0].V)
	}
	if n, _ := rows[0].Values[1].AsInt(); n != 10 {
		t.Errorf("row[0] n = %v, want 10", rows[0].Values[1].V)
	}
	if av, _ := rows[0].Values[2].AsFloat(); av != 12.5 {
		t.Errorf("row[0] avg_dur = %v, want 12.5", rows[0].Values[2].V)
	}

	// Verify the query spec we POSTed to Honeycomb.
	spec, _ := f.bodies["POST /1/queries/prod"].(map[string]any)
	if spec == nil {
		t.Fatal("no query spec captured")
	}
	bd, _ := spec["breakdowns"].([]any)
	if len(bd) != 1 || bd[0] != "service.name" {
		t.Errorf("breakdowns = %v, want [service.name]", spec["breakdowns"])
	}
	calcs, _ := spec["calculations"].([]any)
	if len(calcs) != 2 {
		t.Fatalf("calculations = %v, want 2", spec["calculations"])
	}
	c0 := calcs[0].(map[string]any)
	if c0["op"] != "COUNT" {
		t.Errorf("calc[0].op = %v, want COUNT", c0["op"])
	}
	c1 := calcs[1].(map[string]any)
	if c1["op"] != "AVG" || c1["column"] != "duration_ms" {
		t.Errorf("calc[1] = %v, want AVG(duration_ms)", c1)
	}
	filters, _ := spec["filters"].([]any)
	if len(filters) != 1 {
		t.Fatalf("filters = %v, want 1", spec["filters"])
	}
	fl := filters[0].(map[string]any)
	if fl["column"] != "duration_ms" || fl["op"] != ">" {
		t.Errorf("filter = %v, want duration_ms > …", fl)
	}
}

// TestEventsQuery403HintOnFreePlan: a 403 from the query POST (the paid-only
// Query Data API) is wrapped with an explanatory hint, not surfaced raw.
func TestEventsQuery403HintOnFreePlan(t *testing.T) {
	f := &fakeHoney{
		resp: map[string]string{
			"GET /1/columns/prod": `[{"key_name":"duration_ms","type":"float"}]`,
		},
		errs: map[string]error{
			"POST /1/queries/prod": &apiError{status: 403, body: "forbidden"},
		},
	}
	c := newWithClient(f)
	req := connector.ScanRequest{
		Dataset:   connector.Dataset{Options: map[string]any{"api_key": "k", "dataset": "prod"}},
		Aggregate: &connector.AggregateRequest{Aggregates: []connector.AggregateOp{{Func: "COUNT", Alias: "n"}}},
	}
	_, err := c.Scan(context.Background(), req)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "paid plan") || !strings.Contains(err.Error(), "403") {
		t.Errorf("error = %q, want a 403 hint mentioning a paid plan", err)
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
