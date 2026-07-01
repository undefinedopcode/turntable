package azrgraphc

import (
	"context"
	"fmt"
	"testing"

	"github.com/april/turntable/internal/connector"
	"github.com/april/turntable/internal/engine"
	"github.com/april/turntable/internal/sql"
)

// fakeGraph serves rows in small pages (to exercise SkipToken pagination) and
// records the last KQL / subscriptions it was asked for.
type fakeGraph struct {
	rows     []map[string]any
	lastKQL  string
	lastSubs []string
	calls    int
}

func (f *fakeGraph) query(ctx context.Context, subs []string, kql string, top int32, skip string) ([]map[string]any, string, error) {
	f.lastKQL = kql
	f.lastSubs = subs
	f.calls++
	start := 0
	if skip != "" {
		fmt.Sscanf(skip, "%d", &start)
	}
	end := start + 2 // force 2-row pages regardless of top, to test pagination
	if end > len(f.rows) {
		end = len(f.rows)
	}
	next := ""
	if end < len(f.rows) {
		next = fmt.Sprintf("%d", end)
	}
	return f.rows[start:end], next, nil
}

func sampleRows() []map[string]any {
	return []map[string]any{
		{"id": "/subscriptions/s/vm1", "name": "vm1", "type": "microsoft.compute/virtualmachines", "location": "eastus", "tags": map[string]any{"env": "prod"}},
		{"id": "/subscriptions/s/vm2", "name": "vm2", "type": "microsoft.compute/virtualmachines", "location": "westus", "tags": map[string]any{"env": "dev"}},
		{"id": "/subscriptions/s/aks1", "name": "aks1", "type": "microsoft.containerservice/managedclusters", "location": "eastus"},
	}
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

func TestResolveInfersSchema(t *testing.T) {
	c := newWithClient(&fakeGraph{rows: sampleRows()})
	sc, err := c.Resolve(context.Background(), connector.Dataset{Source: "Resources"})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]engine.Type{}
	for _, col := range sc.Columns {
		got[col.Name] = col.Type
	}
	// Columns are the sorted union; scalars typed, nested `tags` -> any.
	if got["id"] != engine.TypeString || got["location"] != engine.TypeString {
		t.Errorf("id/location should be string: %+v", got)
	}
	if got["tags"] != engine.TypeAny {
		t.Errorf("tags should be any, got %v", got["tags"])
	}
	// Deterministic order (sorted).
	want := []string{"id", "location", "name", "tags", "type"}
	for i, n := range want {
		if sc.Columns[i].Name != n {
			t.Errorf("col %d = %q, want %q", i, sc.Columns[i].Name, n)
		}
	}
}

func TestScanPaginatesAndMaps(t *testing.T) {
	fake := &fakeGraph{rows: sampleRows()}
	c := newWithClient(fake)
	ds := connector.Dataset{Source: "Resources"}
	rows := drain(t, mustScan(t, c, connector.ScanRequest{Dataset: ds}))
	if len(rows) != 3 {
		t.Fatalf("rows = %d, want 3 (2-row pages should have paginated)", len(rows))
	}
	// Default scan KQL: table + safety take.
	if fake.lastKQL != "Resources | take 5000" {
		t.Errorf("kql = %q", fake.lastKQL)
	}
	// name column (index 2 in sorted schema) of first row.
	if rows[0].Values[2].V != "vm1" {
		t.Errorf("row0 name = %v, want vm1", rows[0].Values[2].V)
	}
	// aks1 has no tags -> NULL in the tags column.
	if !rows[2].Values[3].IsNull() {
		t.Errorf("aks1 tags should be NULL")
	}
}

func TestScanPushesPredicateAndSubscriptions(t *testing.T) {
	fake := &fakeGraph{rows: sampleRows()}
	c := newWithClient(fake)
	ds := connector.Dataset{Source: "Resources", Options: map[string]any{"subscriptions": "sub-a, sub-b"}}
	lim := 10
	_, err := c.Scan(context.Background(), connector.ScanRequest{
		Dataset:   ds,
		Predicate: predicate(t, "type = 'microsoft.compute/virtualmachines'"),
		Limit:     &lim,
	})
	if err != nil {
		t.Fatal(err)
	}
	want := `Resources | where type == "microsoft.compute/virtualmachines" | take 10`
	if fake.lastKQL != want {
		t.Errorf("kql = %q, want %q", fake.lastKQL, want)
	}
	if len(fake.lastSubs) != 2 || fake.lastSubs[0] != "sub-a" || fake.lastSubs[1] != "sub-b" {
		t.Errorf("subs = %v, want [sub-a sub-b]", fake.lastSubs)
	}
}

func TestRawQueryMode(t *testing.T) {
	fake := &fakeGraph{rows: sampleRows()}
	c := newWithClient(fake)
	raw := "Resources | where type =~ 'microsoft.web/sites' | project name, location"
	ds := connector.Dataset{Options: map[string]any{"query": raw}}
	// A predicate is ignored in raw mode (the user owns the query).
	_, err := c.Scan(context.Background(), connector.ScanRequest{Dataset: ds, Predicate: predicate(t, "name = 'x'")})
	if err != nil {
		t.Fatal(err)
	}
	if fake.lastKQL != raw {
		t.Errorf("raw mode kql = %q, want verbatim %q", fake.lastKQL, raw)
	}
}

func TestTableResolution(t *testing.T) {
	if got := tableFor(connector.Dataset{Options: map[string]any{"table": "ResourceContainers"}}); got != "ResourceContainers" {
		t.Errorf("table option: got %q", got)
	}
	if got := tableFor(connector.Dataset{Source: "Resources"}); got != "Resources" {
		t.Errorf("ref source: got %q", got)
	}
	if got := tableFor(connector.Dataset{}); got != "Resources" {
		t.Errorf("default: got %q", got)
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
