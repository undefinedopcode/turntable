package plan

import (
	"context"
	"testing"

	"github.com/april/turntable/internal/connector"
	"github.com/april/turntable/internal/sql"
)

func TestGenerateSeriesInt(t *testing.T) {
	reg := connector.NewRegistry() // no connector needed
	rows := runQuery(t, reg, "SELECT value FROM generate_series(1, 5)")
	if len(rows) != 5 {
		t.Fatalf("rows = %d, want 5", len(rows))
	}
	for i, w := range []int64{1, 2, 3, 4, 5} {
		if n, _ := rows[i].Values[0].AsInt(); n != w {
			t.Errorf("row %d = %v, want %d", i, rows[i].Values[0], w)
		}
	}
	// Explicit step (inclusive of the stop when it lands on a step).
	if got := runQuery(t, reg, "SELECT value FROM generate_series(0, 10, 5)"); len(got) != 3 {
		t.Errorf("stepped series = %d rows, want 3", len(got))
	}
	// Descending.
	if got := runQuery(t, reg, "SELECT value FROM generate_series(3, 1, -1)"); len(got) != 3 {
		t.Errorf("descending series = %d rows, want 3", len(got))
	}
}

func TestGenerateSeriesDate(t *testing.T) {
	reg := connector.NewRegistry()
	rows := runQuery(t, reg,
		"SELECT value FROM generate_series(CAST('2024-03-01' AS timestamp), CAST('2024-03-05' AS timestamp), INTERVAL '1 day')")
	if len(rows) != 5 {
		t.Fatalf("date series = %d rows, want 5 (inclusive)", len(rows))
	}
}

func TestColumnAliases(t *testing.T) {
	reg := connector.NewRegistry()
	// Rename the table function's "value" column to "n", reference both bare and
	// qualified.
	rows := runQuery(t, reg, "SELECT g.n FROM generate_series(1, 3) AS g(n) WHERE g.n > 1 ORDER BY n")
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(rows))
	}
	for i, w := range []int64{2, 3} {
		if n, _ := rows[i].Values[0].AsInt(); n != w {
			t.Errorf("row %d = %v, want %d", i, rows[i].Values[0], w)
		}
	}
	// Too many aliases is an error.
	stmt, err := sql.Parse("SELECT n FROM generate_series(1, 3) AS g(n, extra)")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Build(context.Background(), stmt, reg); err == nil {
		t.Error("expected error for more aliases than columns")
	}
}
