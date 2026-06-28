package plan

import (
	"testing"

	"github.com/april/turntable/internal/connector"
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
