package azcostc

import (
	"context"
	"fmt"
	"testing"

	"github.com/april/turntable/internal/connector"
	"github.com/april/turntable/internal/engine"
)

type fakeCost struct {
	cols      []costColumn
	rows      [][]any
	lastScope string
	lastDef   queryDef

	// pages, when set, models a multi-page result: each element is one page's
	// rows, returned in order with a synthetic NextLink until the last page. It
	// takes precedence over rows. links records the nextLink each call received.
	pages [][][]any
	links []string
}

func (f *fakeCost) query(ctx context.Context, scope string, def queryDef, nextLink string) ([]costColumn, [][]any, string, error) {
	f.lastScope = scope
	f.lastDef = def
	f.links = append(f.links, nextLink)
	if f.pages != nil {
		// nextLink of the form "page:N" selects the page; "" is page 0.
		idx := 0
		if nextLink != "" {
			fmt.Sscanf(nextLink, "page:%d", &idx)
		}
		next := ""
		if idx+1 < len(f.pages) {
			next = fmt.Sprintf("page:%d", idx+1)
		}
		return f.cols, f.pages[idx], next, nil
	}
	return f.cols, f.rows, "", nil
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

// TestScanFollowsNextLink verifies the scan walks the NextLink chain and
// concatenates every page's rows (the Query API caps a single response at ~5000).
func TestScanFollowsNextLink(t *testing.T) {
	f := &fakeCost{
		cols: []costColumn{{"cost", engine.TypeFloat}, {"ServiceName", engine.TypeString}},
		pages: [][][]any{
			{{1.0, "A"}, {2.0, "B"}},
			{{3.0, "C"}},
			{{4.0, "D"}, {5.0, "E"}},
		},
	}
	c := newWithClient(f)
	ds := connector.Dataset{Options: map[string]any{"subscription": "abc"}}
	rows := drain(t, mustScan(t, c, connector.ScanRequest{Dataset: ds}))

	if len(rows) != 5 {
		t.Fatalf("rows = %d, want 5 (all pages concatenated)", len(rows))
	}
	// First page's first row and last page's last row both present, in order.
	if v, _ := rows[0].Values[0].AsFloat(); v != 1.0 {
		t.Errorf("row[0] cost = %v, want 1.0", rows[0].Values[0].V)
	}
	if rows[4].Values[1].V != "E" {
		t.Errorf("row[4] service = %v, want E", rows[4].Values[1].V)
	}
	// Three calls: the first with no link, then each page's synthetic NextLink.
	wantLinks := []string{"", "page:1", "page:2"}
	if len(f.links) != len(wantLinks) {
		t.Fatalf("made %d calls, want %d", len(f.links), len(wantLinks))
	}
	for i, want := range wantLinks {
		if f.links[i] != want {
			t.Errorf("call %d nextLink = %q, want %q", i, f.links[i], want)
		}
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
