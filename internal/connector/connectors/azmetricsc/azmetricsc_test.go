package azmetricsc

import (
	"context"
	"strings"
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

	// batch capture
	batchSub    string
	batchRegion string
	batchIDs    []string
	batchSeries []metricSeries
}

func (f *fakeMetrics) list(ctx context.Context, resourceURI string, q metricQuery) ([]metricSeries, error) {
	f.lastResource = resourceURI
	f.lastQuery = q
	return f.series, nil
}

func (f *fakeMetrics) listBatch(ctx context.Context, subscription, region string, resourceIDs []string, q metricQuery) ([]metricSeries, error) {
	f.lastQuery = q
	f.batchSub = subscription
	f.batchRegion = region
	f.batchIDs = append(f.batchIDs, resourceIDs...)
	return f.batchSeries, nil
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
	want := []string{"timestamp", "resource", "metric", "unit", "aggregation", "value", "node", "pod"}
	if len(sc.Columns) != len(want) {
		t.Fatalf("cols = %d, want %d", len(sc.Columns), len(want))
	}
	for i, n := range want {
		if sc.Columns[i].Name != n {
			t.Errorf("col %d = %q, want %q", i, sc.Columns[i].Name, n)
		}
	}
	if sc.Columns[sc.Index("value")].Type != engine.TypeFloat || sc.Columns[0].Type != engine.TypeTime {
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
			unit:       "Percent",
			dimensions: map[string]string{"node": "aks-1"},
			points: []metricPoint{
				{ts: ts, vals: map[string]*float64{"average": fptr(42.5)}},
				{ts: ts.Add(5 * time.Minute), vals: map[string]*float64{"average": nil}}, // no data -> NULL value
			},
		},
		{
			metric:     "Percentage CPU",
			unit:       "Percent",
			dimensions: map[string]string{"node": "aks-2"},
			points:     []metricPoint{{ts: ts, vals: map[string]*float64{"average": fptr(10)}}},
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
	if len(fake.lastQuery.aggregations) != 1 || fake.lastQuery.aggregations[0] != "average" {
		t.Errorf("aggregations = %v, want [average]", fake.lastQuery.aggregations)
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

	// Rows: 3 points across 2 series, columns [ts, resource, metric, unit, agg, value, node].
	if len(rows) != 3 {
		t.Fatalf("rows = %d, want 3", len(rows))
	}
	sc := schemaFor(t, map[string]any{"dimension": "node"})
	r0 := rows[0].Values
	if r0[0].Type != engine.TypeTime {
		t.Errorf("timestamp not a time: %v", r0[0].Type)
	}
	if r0[sc.Index("metric")].V != "Percentage CPU" || r0[sc.Index("aggregation")].V != "average" {
		t.Errorf("metric/agg = %v/%v", r0[sc.Index("metric")].V, r0[sc.Index("aggregation")].V)
	}
	if r0[sc.Index("unit")].V != "Percent" {
		t.Errorf("unit = %v, want Percent", r0[sc.Index("unit")].V)
	}
	if f, _ := r0[sc.Index("value")].AsFloat(); f != 42.5 {
		t.Errorf("value = %v, want 42.5", r0[sc.Index("value")].V)
	}
	if r0[sc.Index("node")].V != "aks-1" {
		t.Errorf("node = %v, want aks-1", r0[sc.Index("node")].V)
	}
	// second point has nil value -> NULL
	if !rows[1].Values[sc.Index("value")].IsNull() {
		t.Errorf("missing value should be NULL")
	}
}

// schemaFor resolves the connector schema for the given options (column order is
// looked up by name so tests survive added columns).
func schemaFor(t *testing.T, opts map[string]any) engine.Schema {
	t.Helper()
	sc, err := New().Resolve(context.Background(), connector.Dataset{Options: opts})
	if err != nil {
		t.Fatal(err)
	}
	return sc
}

// TestScanMultipleAggregations: a comma-separated aggregation option emits one
// row per (point, aggregation) — a single Azure query returns them all.
func TestScanMultipleAggregations(t *testing.T) {
	ts := time.Date(2026, 6, 30, 12, 0, 0, 0, time.UTC)
	fake := &fakeMetrics{series: []metricSeries{{
		metric: "Percentage CPU", unit: "Percent",
		points: []metricPoint{{ts: ts, vals: map[string]*float64{"average": fptr(40), "maximum": fptr(90)}}},
	}}}
	ds := connector.Dataset{Options: map[string]any{
		"resource":    "/subscriptions/abc/x",
		"metric":      "Percentage CPU",
		"aggregation": "Average, Maximum",
	}}
	rows := drain(t, mustScan(t, newWithClient(fake), connector.ScanRequest{Dataset: ds}))
	if len(fake.lastQuery.aggregations) != 2 {
		t.Fatalf("aggregations = %v, want 2", fake.lastQuery.aggregations)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2 (one point × two aggregations)", len(rows))
	}
	sc := schemaFor(t, nil)
	got := map[string]float64{}
	for _, r := range rows {
		v, _ := r.Values[sc.Index("value")].AsFloat()
		got[r.Values[sc.Index("aggregation")].V.(string)] = v
	}
	if got["average"] != 40 || got["maximum"] != 90 {
		t.Errorf("agg values = %v, want average=40 maximum=90", got)
	}
}

func TestScanBatchMode(t *testing.T) {
	ts := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	fake := &fakeMetrics{batchSeries: []metricSeries{
		{resource: "/subscriptions/abc/resourceGroups/rg/providers/Microsoft.Compute/virtualMachines/vm1", metric: "Percentage CPU", points: []metricPoint{{ts: ts, vals: map[string]*float64{"maximum": fptr(30)}}}},
		{resource: "/subscriptions/abc/resourceGroups/rg/providers/Microsoft.Compute/virtualMachines/vm2", metric: "Percentage CPU", points: []metricPoint{{ts: ts, vals: map[string]*float64{"maximum": fptr(70)}}}},
	}}
	c := newWithClient(fake)
	ds := connector.Dataset{Options: map[string]any{
		"resources":   "/subscriptions/abc/resourceGroups/rg/providers/Microsoft.Compute/virtualMachines/vm1, /subscriptions/abc/resourceGroups/rg/providers/Microsoft.Compute/virtualMachines/vm2",
		"metric":      "Percentage CPU",
		"region":      "eastus",
		"aggregation": "Maximum",
	}}
	rows := drain(t, mustScan(t, c, connector.ScanRequest{Dataset: ds}))

	if fake.batchRegion != "eastus" {
		t.Errorf("region = %q, want eastus", fake.batchRegion)
	}
	if fake.batchSub != "abc" {
		t.Errorf("subscription = %q, want abc (parsed from resource id)", fake.batchSub)
	}
	if len(fake.batchIDs) != 2 {
		t.Errorf("batch ids = %v, want 2", fake.batchIDs)
	}
	if len(fake.lastQuery.aggregations) != 1 || fake.lastQuery.aggregations[0] != "maximum" {
		t.Errorf("aggregations = %v, want [maximum]", fake.lastQuery.aggregations)
	}
	// One row per resource; the resource column is each series' own resource.
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(rows))
	}
	sc := schemaFor(t, nil)
	if !strings.HasSuffix(rows[0].Values[sc.Index("resource")].V.(string), "/vm1") {
		t.Errorf("row0 resource = %v, want …/vm1", rows[0].Values[sc.Index("resource")].V)
	}
	if v, _ := rows[1].Values[sc.Index("value")].AsFloat(); v != 70 {
		t.Errorf("row1 value = %v, want 70", rows[1].Values[sc.Index("value")].V)
	}
}

func TestBatchModeRequiresRegion(t *testing.T) {
	c := newWithClient(&fakeMetrics{})
	ds := connector.Dataset{Options: map[string]any{
		"resources": "/subscriptions/abc/x/vm1",
		"metric":    "Percentage CPU",
	}}
	if _, err := c.Scan(context.Background(), connector.ScanRequest{Dataset: ds}); err == nil {
		t.Fatal("expected error: batch mode needs a region")
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
