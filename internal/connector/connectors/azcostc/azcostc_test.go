package azcostc

import (
	"context"
	"testing"

	"github.com/april/turntable/internal/connector"
	"github.com/april/turntable/internal/engine"
)

type fakeCost struct {
	cols      []costColumn
	rows      [][]any
	lastScope string
	lastDef   queryDef
}

func (f *fakeCost) query(ctx context.Context, scope string, def queryDef) ([]costColumn, [][]any, error) {
	f.lastScope = scope
	f.lastDef = def
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

func TestScopeShorthand(t *testing.T) {
	if got := scopeOf(map[string]any{"subscription": "abc"}); got != "/subscriptions/abc" {
		t.Errorf("subscription shorthand = %q", got)
	}
	if got := scopeOf(map[string]any{"scope": "/providers/Microsoft.Billing/billingAccounts/1"}); got != "/providers/Microsoft.Billing/billingAccounts/1" {
		t.Errorf("explicit scope = %q", got)
	}
}

func TestQueryDefFromOptions(t *testing.T) {
	def := queryFor(map[string]any{
		"metric":   "PreTaxCost",
		"group_by": "ServiceName, TAG:env",
		"start":    "2026-06-01",
		"end":      "2026-06-30",
	})
	if def.metric != "PreTaxCost" || def.metricAlias != "pretaxcost" {
		t.Errorf("metric/alias = %q/%q", def.metric, def.metricAlias)
	}
	if def.timeframe != "Custom" {
		t.Errorf("timeframe = %q, want Custom (start/end given)", def.timeframe)
	}
	if def.exportType != "ActualCost" {
		t.Errorf("type = %q, want ActualCost default", def.exportType)
	}
	if len(def.grouping) != 2 || def.grouping[0].name != "ServiceName" || def.grouping[0].isTag ||
		def.grouping[1].name != "env" || !def.grouping[1].isTag {
		t.Errorf("grouping = %+v", def.grouping)
	}
}

func TestScanTypedColumnsAndRows(t *testing.T) {
	f := &fakeCost{
		cols: []costColumn{
			{"cost", engine.TypeFloat},
			{"UsageDate", engine.TypeFloat}, // Azure returns the date as a number (YYYYMMDD)
			{"ServiceName", engine.TypeString},
			{"Currency", engine.TypeString},
		},
		rows: [][]any{
			{12.5, float64(20260601), "Virtual Machines", "USD"},
			{3.0, float64(20260601), "Storage", "USD"},
		},
	}
	c := newWithClient(f)
	ds := connector.Dataset{Options: map[string]any{"subscription": "abc", "group_by": "ServiceName", "granularity": "Daily"}}
	rows := drain(t, mustScan(t, c, connector.ScanRequest{Dataset: ds}))

	if f.lastScope != "/subscriptions/abc" {
		t.Errorf("scope = %q", f.lastScope)
	}
	if f.lastDef.granularity != "Daily" || f.lastDef.timeframe != "MonthToDate" {
		t.Errorf("def = %+v", f.lastDef)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(rows))
	}
	if v, _ := rows[0].Values[0].AsFloat(); v != 12.5 {
		t.Errorf("cost = %v, want 12.5", rows[0].Values[0].V)
	}
	if rows[0].Values[2].V != "Virtual Machines" {
		t.Errorf("service = %v", rows[0].Values[2].V)
	}
}

func TestScanRequiresScope(t *testing.T) {
	c := newWithClient(&fakeCost{})
	if _, err := c.Scan(context.Background(), connector.ScanRequest{Dataset: connector.Dataset{}}); err == nil {
		t.Fatal("expected error without subscription/scope")
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
