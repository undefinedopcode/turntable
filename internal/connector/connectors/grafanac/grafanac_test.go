package grafanac

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/april/turntable/internal/connector"
	"github.com/april/turntable/internal/engine"
)

// fakeAPI records requests and returns canned responses keyed by "METHOD path".
type fakeAPI struct {
	responses map[string]string // "GET /api/datasources" -> body
	status    map[string]int
	lastBody  map[string]json.RawMessage // path -> POST body seen
}

func (f *fakeAPI) do(ctx context.Context, method, path string, body any) ([]byte, int, error) {
	key := method + " " + path
	if body != nil {
		if f.lastBody == nil {
			f.lastBody = map[string]json.RawMessage{}
		}
		b, _ := json.Marshal(body)
		f.lastBody[path] = b
	}
	st := 200
	if f.status != nil {
		if s, ok := f.status[key]; ok {
			st = s
		}
	}
	resp, ok := f.responses[key]
	if !ok {
		return []byte(`{"message":"not found"}`), 404, nil
	}
	return []byte(resp), st, nil
}

func scanRows(t *testing.T, c *Connector, ds connector.Dataset) (engine.Schema, []engine.Row) {
	t.Helper()
	schema, err := c.Resolve(context.Background(), ds)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	it, err := c.Scan(context.Background(), connector.ScanRequest{Dataset: ds})
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	var rows []engine.Row
	for {
		r, ok, err := it.Next()
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		if !ok {
			break
		}
		rows = append(rows, r)
	}
	return schema, rows
}

func colIndex(s engine.Schema, name string) int {
	for i, c := range s.Columns {
		if c.Name == name {
			return i
		}
	}
	return -1
}

func TestDatasourcesList(t *testing.T) {
	api := &fakeAPI{responses: map[string]string{
		"GET /api/datasources": `[
			{"id":1,"uid":"prom-uid","name":"Prometheus","type":"prometheus","typeName":"Prometheus","url":"http://prom:9090","isDefault":true,"readOnly":false,"access":"proxy"},
			{"id":2,"uid":"pg-uid","name":"Postgres","type":"postgres","url":"pg:5432","isDefault":false,"database":"analytics","user":"ro"}
		]`,
	}}
	c := newWithClient(api)
	ds := connector.Dataset{Name: "grafana", Source: "datasources", Options: map[string]any{"url": "http://g"}}

	schema, rows := scanRows(t, c, ds)
	if len(rows) != 2 {
		t.Fatalf("want 2 rows, got %d", len(rows))
	}
	name := colIndex(schema, "name")
	uid := colIndex(schema, "uid")
	isDefault := colIndex(schema, "is_default")
	if got := rows[0].Values[name].AsString(); got != "Prometheus" {
		t.Errorf("row0 name = %q", got)
	}
	if got := rows[0].Values[uid].AsString(); got != "prom-uid" {
		t.Errorf("row0 uid = %q", got)
	}
	if b, _ := rows[0].Values[isDefault].AsBool(); !b {
		t.Errorf("row0 is_default should be true")
	}
	if b, _ := rows[1].Values[isDefault].AsBool(); b {
		t.Errorf("row1 is_default should be false")
	}
}

func TestQueryPrometheusFrames(t *testing.T) {
	// Two series -> two frames, each [Time, Value] with distinct labels.
	frames := `{"results":{"A":{"status":200,"frames":[
		{"schema":{"fields":[
			{"name":"Time","type":"time"},
			{"name":"Value","type":"number","typeInfo":{"frame":"float64"},"labels":{"job":"api","instance":"a"}}
		]},"data":{"values":[[1620000000000,1620000015000],[1.5,2.5]]}},
		{"schema":{"fields":[
			{"name":"Time","type":"time"},
			{"name":"Value","type":"number","typeInfo":{"frame":"float64"},"labels":{"job":"web","instance":"b"}}
		]},"data":{"values":[[1620000000000],[9.0]]}}
	]}}}`
	api := &fakeAPI{responses: map[string]string{
		"GET /api/datasources/name/Prometheus": `{"uid":"prom-uid","type":"prometheus"}`,
		"POST /api/ds/query":                   frames,
	}}
	c := newWithClient(api)
	ds := connector.Dataset{Name: "q", Options: map[string]any{
		"url": "http://g", "datasource": "Prometheus", "query": "up",
	}}

	schema, rows := scanRows(t, c, ds)
	// Columns: Time, Value, instance, job
	for _, want := range []string{"Time", "Value", "job", "instance"} {
		if colIndex(schema, want) < 0 {
			t.Fatalf("missing column %q in %+v", want, schema.Columns)
		}
	}
	if colIndex(schema, "Time") != 0 || colIndex(schema, "Value") != 1 {
		t.Errorf("field columns should come first: %+v", schema.Columns)
	}
	if len(rows) != 3 { // 2 samples + 1 sample
		t.Fatalf("want 3 rows, got %d", len(rows))
	}
	ti, vi, ji := colIndex(schema, "Time"), colIndex(schema, "Value"), colIndex(schema, "job")
	if rows[0].Values[ti].Type != engine.TypeTime {
		t.Errorf("Time column not a time value: %v", rows[0].Values[ti])
	}
	if f, _ := rows[0].Values[vi].AsFloat(); f != 1.5 {
		t.Errorf("row0 Value = %v, want 1.5", f)
	}
	if got := rows[0].Values[ji].AsString(); got != "api" {
		t.Errorf("row0 job = %q, want api", got)
	}
	if got := rows[2].Values[ji].AsString(); got != "web" {
		t.Errorf("row2 job = %q, want web", got)
	}

	// The query body should carry expr + range for a prometheus datasource.
	body := string(api.lastBody["/api/ds/query"])
	if !strings.Contains(body, `"expr":"up"`) {
		t.Errorf("query body missing expr: %s", body)
	}
	if !strings.Contains(body, `"range":true`) {
		t.Errorf("prometheus query should set range: %s", body)
	}
	if !strings.Contains(body, `"uid":"prom-uid"`) {
		t.Errorf("query body missing resolved uid: %s", body)
	}
}

func TestQuerySQLTable(t *testing.T) {
	frames := `{"results":{"A":{"status":200,"frames":[
		{"schema":{"fields":[
			{"name":"id","type":"number","typeInfo":{"frame":"int64"}},
			{"name":"name","type":"string"},
			{"name":"active","type":"boolean"}
		]},"data":{"values":[[1,2],["alice","bob"],[true,false]]}}
	]}}}`
	api := &fakeAPI{responses: map[string]string{
		"GET /api/datasources/name/pg": `{"uid":"pg-uid","type":"postgres"}`,
		"POST /api/ds/query":           frames,
	}}
	c := newWithClient(api)
	ds := connector.Dataset{Name: "q", Options: map[string]any{
		"url": "http://g", "datasource": "pg", "raw_sql": "SELECT id, name, active FROM users",
	}}

	schema, rows := scanRows(t, c, ds)
	if len(rows) != 2 {
		t.Fatalf("want 2 rows, got %d", len(rows))
	}
	idi := colIndex(schema, "id")
	if schema.Columns[idi].Type != engine.TypeInt {
		t.Errorf("id column should be int (int64 typeInfo), got %v", schema.Columns[idi].Type)
	}
	if n, _ := rows[0].Values[idi].AsInt(); n != 1 {
		t.Errorf("row0 id = %v", n)
	}
	if got := rows[1].Values[colIndex(schema, "name")].AsString(); got != "bob" {
		t.Errorf("row1 name = %q", got)
	}
	if b, _ := rows[0].Values[colIndex(schema, "active")].AsBool(); !b {
		t.Errorf("row0 active should be true")
	}

	body := string(api.lastBody["/api/ds/query"])
	if !strings.Contains(body, `"rawSql":"SELECT id, name, active FROM users"`) {
		t.Errorf("sql query body missing rawSql: %s", body)
	}
	if !strings.Contains(body, `"format":"table"`) {
		t.Errorf("sql query should default format=table: %s", body)
	}
}

func TestQueryNameFallsBackToUID(t *testing.T) {
	// datasource value isn't a name; the name lookup 404s, uid lookup succeeds.
	api := &fakeAPI{
		responses: map[string]string{
			"GET /api/datasources/uid/abc123": `{"uid":"abc123","type":"loki"}`,
			"POST /api/ds/query":              `{"results":{"A":{"frames":[]}}}`,
		},
		status: map[string]int{"GET /api/datasources/name/abc123": 404},
	}
	// name lookup returns the default 404 (no response entry).
	c := newWithClient(api)
	ds := connector.Dataset{Name: "q", Options: map[string]any{
		"url": "http://g", "datasource": "abc123", "expr": `{app="x"}`,
	}}
	if _, err := c.Resolve(context.Background(), ds); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	body := string(api.lastBody["/api/ds/query"])
	if !strings.Contains(body, `"uid":"abc123"`) {
		t.Errorf("expected uid fallback in body: %s", body)
	}
}

func TestQueryErrorSurfaced(t *testing.T) {
	api := &fakeAPI{responses: map[string]string{
		"GET /api/datasources/name/pg": `{"uid":"pg-uid","type":"postgres"}`,
		"POST /api/ds/query":           `{"results":{"A":{"status":500,"error":"syntax error at or near FROMX"}}}`,
	}}
	c := newWithClient(api)
	ds := connector.Dataset{Name: "q", Options: map[string]any{
		"url": "http://g", "datasource": "pg", "raw_sql": "SELECT FROMX",
	}}
	_, err := c.Scan(context.Background(), connector.ScanRequest{Dataset: ds})
	if err == nil || !strings.Contains(err.Error(), "syntax error") {
		t.Fatalf("expected query error surfaced, got %v", err)
	}
}

func TestMissingDatasourceOption(t *testing.T) {
	c := newWithClient(&fakeAPI{})
	ds := connector.Dataset{Name: "q", Options: map[string]any{"url": "http://g", "query": "up"}}
	_, err := c.Resolve(context.Background(), ds)
	if err == nil || !strings.Contains(err.Error(), "datasource") {
		t.Fatalf("expected datasource-required error, got %v", err)
	}
}
