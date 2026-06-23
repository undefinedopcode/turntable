package sqlc

import (
	"context"
	"testing"

	"github.com/april/turntable/internal/connector"
	"github.com/april/turntable/internal/engine"
	"github.com/april/turntable/internal/sql"
)

func TestSQLiteResolveAndScan(t *testing.T) {
	ctx := context.Background()
	conn := New()

	// DSN for an in-memory SQLite database with a table.
	dsn := "file::memory:?cache=shared"
	db, _, _, err := openAndTable(connector.Dataset{
		Name:    "inventory",
		Source:  "inventory",
		Options: map[string]any{"driver": "sqlite", "dsn": dsn},
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	if _, err := db.Exec(`CREATE TABLE inventory (
		id INTEGER PRIMARY KEY,
		item TEXT,
		qty INTEGER,
		price REAL
	)`); err != nil {
		t.Fatalf("create table: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO inventory (id, item, qty, price) VALUES
		(1, 'hammer', 10, 12.99),
		(2, 'nails', 1000, 0.05),
		(3, 'saw', 5, 24.50),
		(4, 'tape', 50, 3.25)`); err != nil {
		t.Fatalf("insert: %v", err)
	}

	ds := connector.Dataset{
		Name:    "inventory",
		Source:  "inventory",
		Options: map[string]any{"driver": "sqlite", "dsn": dsn},
	}

	// Resolve schema.
	schema, err := conn.Resolve(ctx, ds)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(schema.Columns) != 4 {
		t.Fatalf("expected 4 columns, got %d: %+v", len(schema.Columns), schema.Columns)
	}
	if schema.Columns[0].Name != "id" {
		t.Fatalf("expected first column id, got %s", schema.Columns[0].Name)
	}

	// Scan with predicate and limit pushdown.
	limit := 2
	pred, _ := sql.ParseExpr(`qty > 5`)
	it, err := conn.Scan(ctx, connector.ScanRequest{
		Dataset:   ds,
		Columns:   []string{"id", "item", "qty", "price"},
		Predicate: pred,
		Limit:     &limit,
	})
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	defer it.Close()

	rows, err := engine.Materialize(ctx, it)
	if err != nil {
		t.Fatalf("materialize: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("expected 2 rows, got %d", len(rows))
	}

	// Verify pushed-down filter was applied by the DB.
	qtyIdx := schema.Index("qty")
	for _, r := range rows {
		if qtyIdx >= 0 {
			if n, _ := r.Values[qtyIdx].AsInt(); n <= 5 {
				t.Fatalf("row should have qty > 5: %+v", r.Values)
			}
		}
	}
}

func TestScanLimitNotPushedWhenPredicateUntranslatable(t *testing.T) {
	ctx := context.Background()
	conn := New()
	dsn := "file::memory:?cache=shared"
	db, _, _, err := openAndTable(connector.Dataset{
		Name: "widgets", Source: "widgets",
		Options: map[string]any{"driver": "sqlite", "dsn": dsn},
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close() // keep the shared in-memory DB alive for the duration

	if _, err := db.Exec(`CREATE TABLE widgets (id INTEGER PRIMARY KEY, item TEXT)`); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO widgets (id, item) VALUES (1,'hammer'),(2,'nails'),(3,'saw'),(4,'tape')`); err != nil {
		t.Fatalf("insert: %v", err)
	}

	ds := connector.Dataset{Name: "widgets", Source: "widgets", Options: map[string]any{"driver": "sqlite", "dsn": dsn}}
	// LOWER(item) is a function the translator can't push, so neither the WHERE
	// nor the LIMIT may be applied in-DB — the engine must see every row.
	limit := 2
	pred, _ := sql.ParseExpr(`LOWER(item) = 'hammer'`)
	it, err := conn.Scan(ctx, connector.ScanRequest{Dataset: ds, Predicate: pred, Limit: &limit})
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	defer it.Close()
	rows, err := engine.Materialize(ctx, it)
	if err != nil {
		t.Fatalf("materialize: %v", err)
	}
	if len(rows) != 4 {
		t.Fatalf("expected all 4 rows (no WHERE/LIMIT pushed), got %d", len(rows))
	}
}

func TestTranslateExprPushdown(t *testing.T) {
	tests := []struct {
		expr string
		want string
		ok   bool
	}{
		{`1 + 2`, `(1 + 2)`, true},
		{`name = "alice"`, `("name" = 'alice')`, true},
		{`qty > 5 AND price < 100`, `(("qty" > 5) AND ("price" < 100))`, true},
		{`name LIKE 'a%'`, `"name" LIKE 'a%'`, true},
		{`id IN (1, 2, 3)`, `"id" IN (1, 2, 3)`, true},
		{`id IS NULL`, `"id" IS NULL`, true},
		{`LOWER(name) = 'alice'`, "", false},
	}
	for _, tc := range tests {
		e, err := sql.ParseExpr(tc.expr)
		if err != nil {
			t.Fatalf("parse %q: %v", tc.expr, err)
		}
		got, ok := translateExpr(e, dialectFor("sqlite"))
		if ok != tc.ok {
			t.Errorf("translate %q: ok=%v, want %v", tc.expr, ok, tc.ok)
			continue
		}
		if ok && got != tc.want {
			t.Errorf("translate %q:\n got  %s\n want %s", tc.expr, got, tc.want)
		}
	}
}
