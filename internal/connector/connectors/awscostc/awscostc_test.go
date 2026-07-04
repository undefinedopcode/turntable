package awscostc

import (
	"context"
	"testing"
	"time"

	"github.com/april/turntable/internal/connector"
	"github.com/april/turntable/internal/engine"
)

type fakeCost struct {
	lastReq costRequest
	results []costResult
}

func (f *fakeCost) get(ctx context.Context, req costRequest) ([]costResult, error) {
	f.lastReq = req
	return f.results, nil
}

func drain(t *testing.T, it engine.RowIterator) []engine.Row {
	t.Helper()
	rows, err := engine.Materialize(context.Background(), it)
	if err != nil {
		t.Fatalf("materialize: %v", err)
	}
	return rows
}

func TestColumnName(t *testing.T) {
	cases := map[string]string{
		"SERVICE": "service", "REGION": "region", "UnblendedCost": "unblended_cost",
		"AmortizedCost": "amortized_cost", "Environment": "environment", "UsageQuantity": "usage_quantity",
	}
	for in, want := range cases {
		if got := columnName(in); got != want {
			t.Errorf("columnName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSchemaFromOptions(t *testing.T) {
	c := New()
	ds := connector.Dataset{Options: map[string]any{
		"group_by": "SERVICE, TAG:Environment",
		"metrics":  "UnblendedCost, UsageQuantity",
	}}
	sc, _ := c.Resolve(context.Background(), ds)
	want := []string{
		"period_start", "period_end", "service", "environment",
		"unblended_cost", "usage_quantity",
		"unblended_cost_unit", "usage_quantity_unit", "estimated",
	}
	if len(sc.Columns) != len(want) {
		t.Fatalf("cols = %d, want %d: %+v", len(sc.Columns), len(want), sc.Columns)
	}
	for i, n := range want {
		if sc.Columns[i].Name != n {
			t.Errorf("col %d = %q, want %q", i, sc.Columns[i].Name, n)
		}
	}
}

func TestScanBuildsRequestAndRows(t *testing.T) {
	start := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	end := time.Date(2026, 6, 2, 0, 0, 0, 0, time.UTC)
	fake := &fakeCost{results: []costResult{
		{start: start, end: end, groups: []string{"AmazonEC2"}, amounts: map[string]float64{"UnblendedCost": 12.34}, units: map[string]string{"UnblendedCost": "USD"}, estimated: true},
		{start: start, end: end, groups: []string{"AmazonS3"}, amounts: map[string]float64{"UnblendedCost": 1.5}, units: map[string]string{"UnblendedCost": "USD"}},
	}}
	c := newWithClient(fake)
	ds := connector.Dataset{Options: map[string]any{
		"group_by":    "SERVICE",
		"granularity": "DAILY",
		"start":       "2026-06-01",
		"end":         "2026-06-08",
	}}
	rows := drain(t, mustScan(t, c, connector.ScanRequest{Dataset: ds}))

	// Request translation.
	if fake.lastReq.granularity != "DAILY" || fake.lastReq.start != "2026-06-01" || fake.lastReq.end != "2026-06-08" {
		t.Errorf("request = %+v", fake.lastReq)
	}
	if len(fake.lastReq.groupBy) != 1 || fake.lastReq.groupBy[0].typ != "DIMENSION" || fake.lastReq.groupBy[0].key != "SERVICE" {
		t.Errorf("groupBy = %+v", fake.lastReq.groupBy)
	}
	if len(fake.lastReq.metrics) != 1 || fake.lastReq.metrics[0] != "UnblendedCost" {
		t.Errorf("metrics = %v (want default UnblendedCost)", fake.lastReq.metrics)
	}

	// Rows: [period_start, period_end, service, unblended_cost, unblended_cost_unit, estimated].
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(rows))
	}
	if rows[0].Values[0].Type != engine.TypeTime {
		t.Errorf("period_start should be time")
	}
	if rows[0].Values[2].V != "AmazonEC2" {
		t.Errorf("service = %v, want AmazonEC2", rows[0].Values[2].V)
	}
	if v, _ := rows[0].Values[3].AsFloat(); v != 12.34 {
		t.Errorf("cost = %v, want 12.34", rows[0].Values[3].V)
	}
	// The per-metric unit column (was a single mislabeled "currency").
	if rows[0].Values[4].V != "USD" {
		t.Errorf("unblended_cost_unit = %v, want USD", rows[0].Values[4].V)
	}
	// estimated flag carried through from ResultByTime.Estimated.
	if b, _ := rows[0].Values[5].AsBool(); !b {
		t.Errorf("row 0 estimated should be true")
	}
	if b, _ := rows[1].Values[5].AsBool(); b {
		t.Errorf("row 1 estimated should be false")
	}
}

func TestTagGroupByParsing(t *testing.T) {
	got := groupBys(map[string]any{"group_by": "TAG:team, SERVICE"})
	if len(got) != 2 || got[0].typ != "TAG" || got[0].key != "team" || got[1].typ != "DIMENSION" || got[1].key != "SERVICE" {
		t.Errorf("groupBys = %+v", got)
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
