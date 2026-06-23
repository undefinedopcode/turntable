package aztablesc

import (
	"context"
	"testing"

	"github.com/april/turntable/internal/connector"
	"github.com/april/turntable/internal/engine"
	tsql "github.com/april/turntable/internal/sql"
)

// fakeTables is an in-memory tablesAPI. It records the filter/limit it was
// asked for so pushdown can be asserted.
type fakeTables struct {
	entities   []map[string]any
	tables     []string
	lastFilter string
	lastLimit  int
}

func (f *fakeTables) listEntities(ctx context.Context, table, filter string, limit int) ([]map[string]any, error) {
	f.lastFilter = filter
	f.lastLimit = limit
	items := f.entities
	if limit > 0 && limit < len(items) {
		items = items[:limit]
	}
	return items, nil
}

func (f *fakeTables) listTables(ctx context.Context) ([]string, error) { return f.tables, nil }

func ds(opts map[string]any) connector.Dataset {
	if opts == nil {
		opts = map[string]any{"table": "t"}
	}
	return connector.Dataset{Name: "t", Source: "t", Options: opts}
}

func drain(t *testing.T, it engine.RowIterator) []engine.Row {
	t.Helper()
	rows, err := engine.Materialize(context.Background(), it)
	if err != nil {
		t.Fatalf("materialize: %v", err)
	}
	return rows
}

func TestResolveInfersUnionSchema(t *testing.T) {
	fake := &fakeTables{entities: []map[string]any{
		{"PartitionKey": "p", "RowKey": "1", "name": "ada"},
		{"PartitionKey": "p", "RowKey": "2", "name": "bea", "region": "emea"},
	}}
	c := newWithClient(fake)
	schema, err := c.Resolve(context.Background(), ds(nil))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"PartitionKey", "RowKey", "name", "region"}
	if len(schema.Columns) != len(want) {
		t.Fatalf("cols = %d, want %d (%+v)", len(schema.Columns), len(want), schema.Columns)
	}
	for i, n := range want {
		if schema.Columns[i].Name != n {
			t.Errorf("col %d = %q, want %q", i, schema.Columns[i].Name, n)
		}
	}
}

func TestScanAlignsRowsAndNullsMissing(t *testing.T) {
	fake := &fakeTables{entities: []map[string]any{
		{"RowKey": "1", "name": "ada", "region": "emea"},
		{"RowKey": "2", "name": "bea"}, // missing region -> NULL
	}}
	c := newWithClient(fake)
	it, err := c.Scan(context.Background(), connector.ScanRequest{Dataset: ds(nil)})
	if err != nil {
		t.Fatal(err)
	}
	rows := drain(t, it)
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(rows))
	}
	// Columns sorted: RowKey(0), name(1), region(2).
	if rows[0].Values[1].V != "ada" {
		t.Errorf("row0 name = %v", rows[0].Values[1].V)
	}
	if !rows[1].Values[2].IsNull() {
		t.Errorf("row1 region = %+v, want NULL", rows[1].Values[2])
	}
}

func TestScanLimitWithoutPredicate(t *testing.T) {
	fake := &fakeTables{entities: []map[string]any{
		{"RowKey": "1"}, {"RowKey": "2"}, {"RowKey": "3"}, {"RowKey": "4"},
	}}
	c := newWithClient(fake)
	two := 2
	it, err := c.Scan(context.Background(), connector.ScanRequest{Dataset: ds(nil), Limit: &two})
	if err != nil {
		t.Fatal(err)
	}
	if rows := drain(t, it); len(rows) != 2 {
		t.Fatalf("rows = %d, want 2 (limit honored)", len(rows))
	}
	if fake.lastLimit != 2 {
		t.Errorf("pushed limit = %d, want 2", fake.lastLimit)
	}
}

func TestScanPushesODataFilter(t *testing.T) {
	fake := &fakeTables{entities: []map[string]any{{"qty": 10.0, "name": "alice"}}}
	c := newWithClient(fake)
	pred, err := tsql.ParseExpr(`qty > 5 AND name = 'alice'`)
	if err != nil {
		t.Fatal(err)
	}
	five := 5
	_, err = c.Scan(context.Background(), connector.ScanRequest{Dataset: ds(nil), Predicate: pred, Limit: &five})
	if err != nil {
		t.Fatal(err)
	}
	if want := "((qty gt 5) and (name eq 'alice'))"; fake.lastFilter != want {
		t.Errorf("pushed filter:\n got  %s\n want %s", fake.lastFilter, want)
	}
	// A fully-translated predicate makes pushing the limit safe.
	if fake.lastLimit != 5 {
		t.Errorf("pushed limit = %d, want 5", fake.lastLimit)
	}
}

func TestScanUntranslatablePredicateNotPushed(t *testing.T) {
	fake := &fakeTables{entities: []map[string]any{{"name": "ada"}}}
	c := newWithClient(fake)
	pred, err := tsql.ParseExpr(`name LIKE 'a%'`) // LIKE has no OData equivalent here
	if err != nil {
		t.Fatal(err)
	}
	three := 3
	if _, err := c.Scan(context.Background(), connector.ScanRequest{Dataset: ds(nil), Predicate: pred, Limit: &three}); err != nil {
		t.Fatal(err)
	}
	if fake.lastFilter != "" {
		t.Errorf("filter = %q, want empty (untranslatable predicate)", fake.lastFilter)
	}
	// Limit must NOT be pushed: the engine has to see every matching row first.
	if fake.lastLimit != maxItems {
		t.Errorf("pushed limit = %d, want maxItems (no limit push under unhandled predicate)", fake.lastLimit)
	}
}

func TestTranslateOData(t *testing.T) {
	cases := []struct {
		expr string
		want string
		ok   bool
	}{
		{`qty >= 5`, "(qty ge 5)", true},
		{`name = 'a''b'`, "(name eq 'a''b')", true}, // quote escaping
		{`id IN (1, 2, 3)`, "(id eq 1 or id eq 2 or id eq 3)", true},
		{`qty BETWEEN 5 AND 10`, "(qty ge 5 and qty le 10)", true},
		{`active = true`, "(active eq true)", true},
		{`name LIKE 'a%'`, "", false},    // unsupported
		{`id IS NULL`, "", false},        // unsupported
		{`LOWER(name) = 'x'`, "", false}, // function unsupported
	}
	for _, tc := range cases {
		e, err := tsql.ParseExpr(tc.expr)
		if err != nil {
			t.Fatalf("parse %q: %v", tc.expr, err)
		}
		got, ok := translateOData(e)
		if ok != tc.ok {
			t.Errorf("translate %q: ok=%v, want %v", tc.expr, ok, tc.ok)
			continue
		}
		if ok && got != tc.want {
			t.Errorf("translate %q:\n got  %s\n want %s", tc.expr, got, tc.want)
		}
	}
}

func TestDatasetsForEnumerates(t *testing.T) {
	fake := &fakeTables{tables: []string{"users", "events"}}
	c := newWithClient(fake)
	got, err := c.DatasetsFor(context.Background(), connector.Dataset{Options: map[string]any{"account": "acct"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("datasets = %d, want 2", len(got))
	}
	for _, d := range got {
		if d.Options["table"] != d.Name {
			t.Errorf("dataset %q table option = %v", d.Name, d.Options["table"])
		}
		if d.Options["account"] != "acct" {
			t.Errorf("dataset %q lost account option", d.Name)
		}
	}
}

func TestMissingTable(t *testing.T) {
	c := newWithClient(&fakeTables{})
	if _, err := c.Resolve(context.Background(), connector.Dataset{}); err == nil {
		t.Fatal("expected error when no table is given")
	}
}

func TestMissingAuthBuildsError(t *testing.T) {
	// With no injected client and no auth options, the real client build fails.
	c := New()
	if _, err := c.Resolve(context.Background(), connector.Dataset{Options: map[string]any{"table": "t"}}); err == nil {
		t.Fatal("expected error when no auth option is provided")
	}
}
