package plan

import (
	"context"
	"testing"

	"github.com/april/turntable/internal/connector"
	"github.com/april/turntable/internal/engine"
)

// countingConn counts how many times Scan is called, so a test can prove a CTE
// referenced multiple times runs its underlying scan only once.
type countingConn struct {
	scans *int
	data  []int64
}

func (countingConn) Name() string { return "counting" }
func (countingConn) Datasets(ctx context.Context) ([]connector.Dataset, error) {
	return nil, nil
}
func (countingConn) Resolve(ctx context.Context, ds connector.Dataset) (engine.Schema, error) {
	return engine.Schema{Columns: []engine.Column{{Name: "n", Type: engine.TypeInt, Nullable: true}}}, nil
}
func (c countingConn) Scan(ctx context.Context, req connector.ScanRequest) (engine.RowIterator, error) {
	*c.scans++
	rows := make([]engine.Row, len(c.data))
	for i, v := range c.data {
		rows[i] = engine.Row{Values: []engine.Value{engine.IntVal(v)}}
	}
	return engine.NewSliceIter(rows), nil
}

func countingRegistry(t *testing.T, scans *int) *connector.Registry {
	t.Helper()
	conn := countingConn{scans: scans, data: []int64{1, 2, 2}}
	reg := connector.NewRegistry()
	_ = reg.RegisterConnector(conn)
	if err := reg.RegisterSource("c1", conn, connector.Dataset{Name: "c1"}); err != nil {
		t.Fatal(err)
	}
	return reg
}

// TestCTEMaterializedOnce proves a CTE referenced twice scans its source once.
func TestCTEMaterializedOnce(t *testing.T) {
	var scans int
	reg := countingRegistry(t, &scans)
	// x references c1; the body references x twice via UNION ALL.
	rows := runQuery(t, reg,
		"WITH x AS (SELECT n FROM c1) SELECT n FROM x UNION ALL SELECT n FROM x")
	if len(rows) != 6 { // c1 has 3 rows; x ∪all x = 6
		t.Fatalf("rows = %d, want 6", len(rows))
	}
	if scans != 1 {
		t.Errorf("c1 scanned %d times, want 1 (CTE should materialize once)", scans)
	}
}

// TestCTEMaterializedInJoin proves a CTE self-joined still scans once.
func TestCTEMaterializedInJoin(t *testing.T) {
	var scans int
	reg := countingRegistry(t, &scans)
	rows := runQuery(t, reg,
		"WITH x AS (SELECT n FROM c1) SELECT a.n FROM x AS a JOIN x AS b ON a.n = b.n")
	// x = {1,2,2}; self-join on n: 1×1=1, 2×2=4 (two 2s each side) → 5 rows.
	if len(rows) != 5 {
		t.Fatalf("rows = %d, want 5", len(rows))
	}
	if scans != 1 {
		t.Errorf("c1 scanned %d times, want 1", scans)
	}
}

// TestCTESingleRefStillOnce confirms a singly-referenced CTE also scans once
// (materialization adds buffering but no extra scan).
func TestCTESingleRefStillOnce(t *testing.T) {
	var scans int
	reg := countingRegistry(t, &scans)
	rows := runQuery(t, reg, "WITH x AS (SELECT n FROM c1) SELECT n FROM x WHERE n > 1")
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(rows))
	}
	if scans != 1 {
		t.Errorf("c1 scanned %d times, want 1", scans)
	}
}
