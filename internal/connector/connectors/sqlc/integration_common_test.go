//go:build integration

// Package sqlc integration tests exercise the connector against real database
// servers (embedded Postgres and an in-process MySQL-compatible server). They
// are gated behind the "integration" build tag so the default `go test ./...`
// run stays pure-Go and network-free:
//
//	go test -tags integration ./internal/connector/connectors/sqlc/
package sqlc

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/april/turntable/internal/connector"
	"github.com/april/turntable/internal/engine"
	tsql "github.com/april/turntable/internal/sql"
)

// seedInventory waits for the server to accept connections, then creates and
// populates the inventory table. priceType lets each dialect name its float
// column (DOUBLE PRECISION for Postgres, DOUBLE for MySQL).
func seedInventory(t *testing.T, driver, dsn, priceType string) {
	t.Helper()
	db, err := sql.Open(driver, dsn)
	if err != nil {
		t.Fatalf("open %s: %v", driver, err)
	}
	defer db.Close()

	// The server may take a moment to start listening.
	var pingErr error
	for i := 0; i < 50; i++ {
		if pingErr = db.Ping(); pingErr == nil {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if pingErr != nil {
		t.Fatalf("%s never became ready: %v", driver, pingErr)
	}

	mustExec(t, db, "DROP TABLE IF EXISTS inventory")
	mustExec(t, db, fmt.Sprintf(
		"CREATE TABLE inventory (id INT PRIMARY KEY, item VARCHAR(64), qty INT, price %s)", priceType))
	mustExec(t, db, `INSERT INTO inventory (id, item, qty, price) VALUES
		(1, 'hammer', 10, 12.99),
		(2, 'nails', 1000, 0.05),
		(3, 'saw', 5, 24.50),
		(4, 'tape', 50, 3.25)`)
}

func mustExec(t *testing.T, db *sql.DB, q string) {
	t.Helper()
	if _, err := db.Exec(q); err != nil {
		t.Fatalf("exec %q: %v", q, err)
	}
}

// checkConnector runs the same assertions against any backing database: schema
// discovery, table enumeration, and predicate/limit pushdown through Scan.
func checkConnector(t *testing.T, driver, dsn string) {
	t.Helper()
	ctx := context.Background()
	conn := New()
	opts := map[string]any{"driver": driver, "dsn": dsn}
	ds := connector.Dataset{Name: "inventory", Source: "inventory", Options: opts}

	// Resolve discovers the schema.
	schema, err := conn.Resolve(ctx, ds)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(schema.Columns) != 4 {
		t.Fatalf("expected 4 columns, got %d: %+v", len(schema.Columns), schema.Columns)
	}
	if schema.Columns[0].Name != "id" {
		t.Fatalf("expected first column id, got %q", schema.Columns[0].Name)
	}

	// DatasetsFor enumerates the user tables.
	tables, err := conn.DatasetsFor(ctx, connector.Dataset{Options: opts})
	if err != nil {
		t.Fatalf("DatasetsFor: %v", err)
	}
	var found bool
	for _, d := range tables {
		if d.Name == "inventory" {
			found = true
		}
	}
	if !found {
		t.Fatalf("inventory not enumerated, got %+v", tables)
	}

	// Scan pushes the predicate and limit into the database.
	limit := 2
	pred, err := tsql.ParseExpr(`qty > 5`)
	if err != nil {
		t.Fatalf("parse predicate: %v", err)
	}
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
		t.Fatalf("expected 2 rows (limit), got %d", len(rows))
	}
	qtyIdx := schema.Index("qty")
	for _, r := range rows {
		if n, _ := r.Values[qtyIdx].AsInt(); n <= 5 {
			t.Fatalf("pushed-down predicate failed: row has qty <= 5: %+v", r.Values)
		}
	}
}
