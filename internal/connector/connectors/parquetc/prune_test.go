package parquetc

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/april/turntable/internal/connector"
	"github.com/april/turntable/internal/engine"
	"github.com/april/turntable/internal/sql"
	"github.com/parquet-go/parquet-go"
)

type readingRow struct {
	TS   int64   `parquet:"ts,timestamp(microsecond)"`
	Site string  `parquet:"site"`
	Flow float64 `parquet:"flow"`
}

// writeGrouped writes 30 hourly readings across 3 row groups of 10 rows each,
// sorted by ts — so the groups carry disjoint time ranges, the layout a
// time-partitioned sensor dump naturally has.
func writeGrouped(t *testing.T) (string, time.Time) {
	t.Helper()
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	var rows []readingRow
	for i := 0; i < 30; i++ {
		rows = append(rows, readingRow{
			TS:   base.Add(time.Duration(i) * time.Hour).UnixMicro(),
			Site: []string{"a", "b"}[i%2],
			Flow: float64(i),
		})
	}
	path := filepath.Join(t.TempDir(), "readings.parquet")
	if err := parquet.WriteFile(path, rows, parquet.MaxRowsPerRowGroup(10)); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return path, base
}

func whereOf(t *testing.T, cond string) connector.Expr {
	t.Helper()
	stmt, err := sql.Parse("SELECT * FROM x WHERE " + cond)
	if err != nil {
		t.Fatalf("parse %q: %v", cond, err)
	}
	return stmt.(*sql.SelectStmt).Where
}

func scanCount(t *testing.T, path string, pred connector.Expr) int {
	t.Helper()
	it, err := New().Scan(context.Background(), connector.ScanRequest{
		Dataset:   connector.Dataset{Name: path, Source: path},
		Predicate: pred,
	})
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	defer it.Close()
	n := 0
	for {
		_, ok, err := it.Next()
		if err != nil {
			t.Fatalf("next: %v", err)
		}
		if !ok {
			return n
		}
		n++
	}
}

func TestPruneRowGroupsByTime(t *testing.T) {
	path, base := writeGrouped(t)

	// Sanity: a full scan crosses all row groups.
	if n := scanCount(t, path, nil); n != 30 {
		t.Fatalf("full scan = %d rows, want 30", n)
	}

	// ts >= hour 25 lives entirely in the last row group (rows 20..29): the
	// scan should return only that group's 10 rows (a superset of the 5
	// matching ones — the engine re-filters).
	cut := base.Add(25 * time.Hour).Format("2006-01-02 15:04:05")
	n := scanCount(t, path, whereOf(t, "ts >= '"+cut+"'"))
	if n != 10 {
		t.Errorf("ts >= h25 scanned %d rows, want 10 (one row group)", n)
	}

	// An equality on flow hits the middle group only.
	if n := scanCount(t, path, whereOf(t, "flow = 15")); n != 10 {
		t.Errorf("flow = 15 scanned %d rows, want 10", n)
	}

	// BETWEEN spanning two groups keeps exactly those two.
	if n := scanCount(t, path, whereOf(t, "flow BETWEEN 8 AND 12")); n != 20 {
		t.Errorf("flow BETWEEN 8 AND 12 scanned %d rows, want 20", n)
	}

	// A contradiction prunes everything.
	if n := scanCount(t, path, whereOf(t, "flow > 1000")); n != 0 {
		t.Errorf("flow > 1000 scanned %d rows, want 0", n)
	}

	// An unprunable conjunct (LIKE) is ignored but the prunable one still cuts;
	// an OR cannot prune at all and scans everything.
	if n := scanCount(t, path, whereOf(t, "site LIKE 'a%' AND flow >= 20")); n != 10 {
		t.Errorf("LIKE AND flow >= 20 scanned %d rows, want 10", n)
	}
	if n := scanCount(t, path, whereOf(t, "flow = 1 OR flow = 25")); n != 30 {
		t.Errorf("OR predicate scanned %d rows, want 30 (no pruning)", n)
	}
}

func TestExtractBounds(t *testing.T) {
	schema := engine.Schema{Columns: []engine.Column{
		{Name: "ts", Type: engine.TypeTime},
		{Name: "flow", Type: engine.TypeFloat},
	}}
	// Reversed operands flip; unparseable time literal declines; unknown
	// column declines.
	b := extractBounds(whereOf(t, "5 < flow AND ts >= '2026-01-01' AND nope = 1 AND ts <= 'not a time'"), schema)
	if len(b) != 2 {
		t.Fatalf("bounds = %+v, want 2", b)
	}
	if b[0].col != 1 || b[0].op != ">" {
		t.Errorf("flipped bound = %+v, want flow > 5", b[0])
	}
	if b[1].col != 0 || b[1].op != ">=" || b[1].val.Type != engine.TypeTime {
		t.Errorf("time bound = %+v", b[1])
	}
}
