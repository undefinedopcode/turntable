package promc

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/april/turntable/internal/connector"
	"github.com/april/turntable/internal/engine"
)

// matrixJSON: two series with differing label sets (instance only on the
// first), out of fingerprint order, including a NaN sample.
const matrixJSON = `{
  "status": "success",
  "data": {
    "resultType": "matrix",
    "result": [
      {"metric": {"__name__": "flow", "station": "b"},
       "values": [[1767225600, "7.5"], [1767225660, "NaN"]]},
      {"metric": {"__name__": "flow", "station": "a", "instance": "s1:9100"},
       "values": [[1767225600, "1.5"]]}
    ]
  }
}`

func promServer(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return srv
}

func ds(url string, opts map[string]any) connector.Dataset {
	all := map[string]any{"url": url}
	for k, v := range opts {
		all[k] = v
	}
	return connector.Dataset{Name: "m", Source: url, Options: all}
}

func TestResolveAndScan(t *testing.T) {
	var gotPath, gotQuery, gotStep string
	srv := promServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotQuery = r.URL.Query().Get("query")
		gotStep = r.URL.Query().Get("step")
		w.Write([]byte(matrixJSON))
	})
	c := New()
	d := ds(srv.URL, map[string]any{"metric": "flow", "time_range": "3600", "step": "60"})

	schema, err := c.Resolve(context.Background(), d)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if gotPath != "/api/v1/query_range" || gotQuery != "flow" || gotStep != "60" {
		t.Errorf("request: path=%s query=%s step=%s", gotPath, gotQuery, gotStep)
	}
	// ts + sorted label union (__name__, instance, station) + value.
	want := []string{"ts", "__name__", "instance", "station", "value"}
	if len(schema.Columns) != len(want) {
		t.Fatalf("columns = %+v", schema.Columns)
	}
	for i, w := range want {
		if schema.Columns[i].Name != w {
			t.Errorf("col %d = %s, want %s", i, schema.Columns[i].Name, w)
		}
	}
	if schema.Columns[0].Type != engine.TypeTime || schema.Columns[4].Type != engine.TypeFloat {
		t.Errorf("types: %+v", schema.Columns)
	}

	it, err := c.Scan(context.Background(), connector.ScanRequest{Dataset: d})
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	rows, err := engine.Materialize(context.Background(), it)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 {
		t.Fatalf("rows = %d, want 3", len(rows))
	}
	// Series sort by fingerprint: station=a (with instance) first.
	r0 := rows[0].Values
	if s := r0[3].AsString(); s != "a" {
		t.Errorf("row0 station = %v, want a (fingerprint order)", r0[3].V)
	}
	if s := r0[2].AsString(); s != "s1:9100" {
		t.Errorf("row0 instance = %v", r0[2].V)
	}
	if f, _ := r0[4].AsFloat(); f != 1.5 {
		t.Errorf("row0 value = %v, want 1.5", r0[4].V)
	}
	ts, ok := r0[0].V.(time.Time)
	if !ok || !ts.Equal(time.Unix(1767225600, 0)) {
		t.Errorf("row0 ts = %v", r0[0].V)
	}
	// station=b rows: instance is NULL (label absent); NaN sample -> NULL value.
	if !rows[1].Values[2].IsNull() {
		t.Errorf("row1 instance = %v, want NULL", rows[1].Values[2].V)
	}
	if !rows[2].Values[4].IsNull() {
		t.Errorf("NaN sample value = %v, want NULL", rows[2].Values[4].V)
	}
}

func TestBearerAndQueryOption(t *testing.T) {
	var gotAuth, gotQuery string
	srv := promServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotQuery = r.URL.Query().Get("query")
		w.Write([]byte(`{"status":"success","data":{"resultType":"matrix","result":[]}}`))
	})
	c := New()
	d := ds(srv.URL, map[string]any{
		"query":  `rate(http_requests_total{job="api"}[5m])`,
		"bearer": "tok123",
	})
	if _, err := c.Resolve(context.Background(), d); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if gotAuth != "Bearer tok123" {
		t.Errorf("auth = %q", gotAuth)
	}
	if !strings.Contains(gotQuery, "rate(http_requests_total") {
		t.Errorf("query = %q", gotQuery)
	}
}

func TestPromErrors(t *testing.T) {
	srv := promServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"status":"error","errorType":"bad_data","error":"parse error at char 3"}`))
	})
	c := New()
	_, err := c.Resolve(context.Background(), ds(srv.URL, map[string]any{"metric": "x{"}))
	if err == nil || !strings.Contains(err.Error(), "parse error") {
		t.Errorf("err = %v, want the prom error surfaced", err)
	}
}

func TestResolveParams(t *testing.T) {
	// Missing url / missing selector.
	if _, err := resolveParams(connector.Dataset{Options: map[string]any{"metric": "up"}}); err == nil ||
		!strings.Contains(err.Error(), "url") {
		t.Errorf("missing url err = %v", err)
	}
	if _, err := resolveParams(connector.Dataset{Options: map[string]any{"url": "http://x"}}); err == nil ||
		!strings.Contains(err.Error(), "metric") {
		t.Errorf("missing selector err = %v", err)
	}
	// A qualified ref carries the selector in Source.
	p, err := resolveParams(connector.Dataset{
		Source:  "up",
		Options: map[string]any{"url": "http://x"},
	})
	if err != nil || p.query != "up" {
		t.Errorf("source selector: %v %q", err, p.query)
	}
	// Explicit window + auto step: 5000s / 250 = 20s.
	p, err = resolveParams(connector.Dataset{Options: map[string]any{
		"url": "http://x", "metric": "up",
		"start": "2026-01-01T00:00:00Z", "end": "2026-01-01T01:23:20Z",
	}})
	if err != nil {
		t.Fatal(err)
	}
	if p.step != 20*time.Second {
		t.Errorf("auto step = %v, want 20s", p.step)
	}
	if !p.start.Before(p.end) {
		t.Errorf("window: %v .. %v", p.start, p.end)
	}
	// Tiny window floors to the minimum step.
	p, _ = resolveParams(connector.Dataset{Options: map[string]any{
		"url": "http://x", "metric": "up", "time_range": "60",
	}})
	if p.step != minAutoStep {
		t.Errorf("floored step = %v, want %v", p.step, minAutoStep)
	}
	// A label named ts/value must not collide with the fixed columns.
	schema, keys := shape([]series{{Metric: map[string]string{"value": "x"}}})
	if schema.Columns[1].Name != "label_value" || keys[0] != "value" {
		t.Errorf("collision handling: %+v %v", schema.Columns, keys)
	}
}
