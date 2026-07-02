package pluginc

// SDK conformance: drive the Python and Node plugin SDKs (sdk/python,
// sdk/node) through the real connector — spawn, handshake, resolve, scan with
// predicate/limit pushdown, cell decoding, shutdown. The fixtures under
// testdata/ declare identical datasets, so both SDKs are held to the same
// assertions. Skips (rather than fails) when the interpreter is not installed,
// keeping the default suite dependency-free.

import (
	"context"
	"io"
	"os/exec"
	"testing"
	"time"

	"github.com/april/turntable/internal/connector"
	"github.com/april/turntable/internal/engine"
	"github.com/april/turntable/internal/sql"
)

func TestPythonSDKConformance(t *testing.T) {
	runSDKConformance(t, "python3", "testdata/conform.py")
}

func TestNodeSDKConformance(t *testing.T) {
	runSDKConformance(t, "node", "testdata/conform.mjs")
}

func runSDKConformance(t *testing.T, interpreter, script string) {
	t.Helper()
	if _, err := exec.LookPath(interpreter); err != nil {
		t.Skipf("%s not installed; skipping SDK conformance", interpreter)
	}

	pc := New([]string{interpreter, script}, nil)
	pc.stderr = io.Discard
	defer pc.Close()

	ctx := context.Background()
	if err := pc.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	if pc.Name() != "conform" {
		t.Fatalf("Name() = %q, want conform", pc.Name())
	}

	ds, err := pc.Datasets(ctx)
	if err != nil || len(ds) != 1 || ds[0].Name != "vals" {
		t.Fatalf("datasets = %+v, %v", ds, err)
	}

	sch, err := pc.Resolve(ctx, connector.Dataset{Name: "vals"})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	wantTypes := []engine.Type{
		engine.TypeInt, engine.TypeFloat, engine.TypeString,
		engine.TypeBool, engine.TypeTime,
	}
	if len(sch.Columns) != 6 {
		t.Fatalf("schema = %+v", sch.Columns)
	}
	for i, wt := range wantTypes {
		if sch.Columns[i].Type != wt {
			t.Errorf("col %d (%s) type = %v, want %v", i, sch.Columns[i].Name, sch.Columns[i].Type, wt)
		}
	}

	// Full scan: all rows, cells decoded (time as time.Time, NULLs preserved).
	rows := scanAll(t, pc, connector.ScanRequest{Dataset: connector.Dataset{Name: "vals"}})
	if len(rows) != 3 {
		t.Fatalf("rows = %d, want 3", len(rows))
	}
	if v, ok := rows[0].Values[0].AsInt(); !ok || v != 1 {
		t.Errorf("row0 i = %+v, want 1", rows[0].Values[0])
	}
	ts, ok := rows[0].Values[4].V.(time.Time)
	if rows[0].Values[4].Type != engine.TypeTime || !ok ||
		!ts.Equal(time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)) {
		t.Errorf("row0 t = %+v, want 2026-01-01T10:00:00Z", rows[0].Values[4])
	}
	if !rows[2].Values[1].IsNull() || !rows[2].Values[4].IsNull() {
		t.Errorf("row2 NULLs not preserved: %+v", rows[2].Values)
	}

	// Predicate pushdown: the SDK filters before rows cross the pipe.
	// i >= 2 AND s LIKE 'a%'  ->  only (3, aloe).
	stmt, err := sql.Parse("SELECT * FROM vals WHERE i >= 2 AND s LIKE 'a%'")
	if err != nil {
		t.Fatal(err)
	}
	pred := stmt.(*sql.SelectStmt).Where
	rows = scanAll(t, pc, connector.ScanRequest{
		Dataset:   connector.Dataset{Name: "vals"},
		Predicate: pred,
	})
	if len(rows) != 1 {
		t.Fatalf("filtered rows = %d, want 1", len(rows))
	}
	if s := rows[0].Values[2].AsString(); s != "aloe" {
		t.Errorf("filtered row s = %q, want aloe", s)
	}

	// Limit pushdown.
	two := 2
	rows = scanAll(t, pc, connector.ScanRequest{
		Dataset: connector.Dataset{Name: "vals"},
		Limit:   &two,
	})
	if len(rows) != 2 {
		t.Fatalf("limited rows = %d, want 2", len(rows))
	}

	// Options ride the dataset object (the bug the Python SDK surfaced): a
	// second scan against an unknown dataset errors cleanly instead of hanging.
	if _, err := pc.Scan(ctx, connector.ScanRequest{Dataset: connector.Dataset{Name: "nope"}}); err == nil {
		t.Error("scan of unknown dataset should error")
	}
}

func scanAll(t *testing.T, pc *Connector, req connector.ScanRequest) []engine.Row {
	t.Helper()
	it, err := pc.Scan(context.Background(), req)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	defer it.Close()
	var out []engine.Row
	for {
		row, ok, err := it.Next()
		if err != nil {
			t.Fatalf("next: %v", err)
		}
		if !ok {
			return out
		}
		out = append(out, row)
	}
}
