package azmetricsc

import (
	"context"
	"testing"
	"time"

	"github.com/april/turntable/internal/connector"
	"github.com/april/turntable/internal/engine"
)

// fakeMetrics records the query it received and returns canned series.
type fakeMetrics struct {
	lastQuery    metricQuery
	lastResource string
	series       []metricSeries
}

func (f *fakeMetrics) list(ctx context.Context, resourceURI string, q metricQuery) ([]metricSeries, error) {
	f.lastResource = resourceURI
	f.lastQuery = q
	return f.series, nil
}

func drain(t *testing.T, it engine.RowIterator) []engine.Row {
	t.Helper()
	rows, err := engine.Materialize(context.Background(), it)
	if err != nil {
		t.Fatalf("materialize: %v", err)
	}
	return rows
}

func fptr(f float64) *float64 { return &f }

func TestResolveSchemaWithDimensions(t *testing.T) {
	c := New()
	ds := connector.Dataset{Options: map[string]any{"dimension": "node, pod"}}
	sc, err := c.Resolve(context.Background(), ds)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"timestamp", "resource", "metric", "aggregation", "value", "node", "pod"}
	if len(sc.Columns) != len(want) {
		t.Fatalf("cols = %d, want %d", len(sc.Columns), len(want))
	}
	for i, n := range want {
		if sc.Columns[i].Name != n {
			t.Errorf("col %d = %q, want %q", i, sc.Columns[i].Name, n)
		}
	}
	if sc.Columns[4].Type != engine.TypeFloat || sc.Columns[0].Type != engine.TypeTime {
		t.Errorf("value should be float and timestamp time")
	}
}

func TestScanRequiresResourceAndMetric(t *testing.T) {
	c := newWithClient(&fakeMetrics{})
	// missing resource
	if _, err := c.Scan(context.Background(), connector.ScanRequest{Dataset: connector.Dataset{Options: map[string]any{"metric": "Percentage CPU"}}}); err == nil {
		t.Error("expected error without resource")
	}
	// missing metric
	ds := connector.Dataset{Options: map[string]any{"resource": "/subscriptions/s/x"}}
	if _, err := c.Scan(context.Background(), connector.ScanRequest{Dataset: ds}); err == nil {
		t.Error("expected error without metric")
	}
}

func TestScanBuildsQueryAndRows(t *testing.T) {
	ts := time.Date(2026, 6, 30, 12, 0, 0, 0, time.UTC)
	fake := &fakeMetrics{series: []metricSeries{
		{
			metric:     "Percentage CPU",
			dimensions: map[string]string{"node": "aks-1"},
			points: []metricPoint{
				{ts: ts, val: fptr(42.5)},
				{ts: ts.Add(5 * time.Minute), val: nil}, // no data -> NULL value
			},
		},
		{
			metric:     "Percentage CPU",
			dimensions: map[string]string{"node": "aks-2"},
			points:     []metricPoint{{ts: ts, val: fptr(10)}},
		},
	}}
	c := newWithClient(fake)
	ds := connector.Dataset{Options: map[string]any{
		"resource":    "/subscriptions/abc/resourceGroups/rg/providers/Microsoft.ContainerService/managedClusters/aks1",
		"metric":      "Percentage CPU",
		"aggregation": "Average",
		"dimension":   "node",
		"interval":    "PT5M",
	}}
	rows := drain(t, mustScan(t, c, connector.ScanRequest{Dataset: ds}))

	// Query translation: aggregation lower-cased, dimension -> "node eq '*'" filter.
	if fake.lastQuery.aggregation != "average" {
		t.Errorf("aggregation = %q, want average", fake.lastQuery.aggregation)
	}
	if fake.lastQuery.filter != "node eq '*'" {
		t.Errorf("filter = %q, want node eq '*'", fake.lastQuery.filter)
	}
	if len(fake.lastQuery.metricNames) != 1 || fake.lastQuery.metricNames[0] != "Percentage CPU" {
		t.Errorf("metricNames = %v", fake.lastQuery.metricNames)
	}
	if fake.lastQuery.interval != "PT5M" {
		t.Errorf("interval = %q", fake.lastQuery.interval)
	}

	// Rows: 3 points across 2 series, columns [ts, resource, metric, agg, value, node].
	if len(rows) != 3 {
		t.Fatalf("rows = %d, want 3", len(rows))
	}
	r0 := rows[0].Values
	if r0[0].Type != engine.TypeTime {
		t.Errorf("timestamp not a time: %v", r0[0].Type)
	}
	if r0[2].V != "Percentage CPU" || r0[3].V != "Average" {
		t.Errorf("metric/agg = %v/%v", r0[2].V, r0[3].V)
	}
	if f, _ := r0[4].AsFloat(); f != 42.5 {
		t.Errorf("value = %v, want 42.5", r0[4].V)
	}
	if r0[5].V != "aks-1" {
		t.Errorf("node = %v, want aks-1", r0[5].V)
	}
	// second point has nil value -> NULL
	if !rows[1].Values[4].IsNull() {
		t.Errorf("missing value should be NULL")
	}
}

func TestSubscriptionFromURI(t *testing.T) {
	uri := "/subscriptions/1234-abcd/resourceGroups/rg/providers/Microsoft.Compute/virtualMachines/vm1"
	if got := subscriptionFromURI(uri); got != "1234-abcd" {
		t.Errorf("sub = %q, want 1234-abcd", got)
	}
	if got := subscriptionFromURI("not-an-arm-id"); got != "" {
		t.Errorf("sub = %q, want empty", got)
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
