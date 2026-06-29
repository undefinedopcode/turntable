package pluginc

import (
	"context"
	"io"
	"os"
	"testing"

	"github.com/april/turntable/internal/connector"
	"github.com/april/turntable/internal/engine"
)

// TestMain doubles as the plugin fixture: when TT_PLUGIN_FIXTURE is set the test
// binary re-execs itself as a real plugin subprocess (speaking the stdio
// protocol over its own stdin/stdout) instead of running the test suite. This
// exercises the full process path — spawn, handshake, scan, shutdown — without
// shipping a separate fixture binary.
func TestMain(m *testing.M) {
	if os.Getenv("TT_PLUGIN_FIXTURE") != "" {
		(&fakePlugin{count: 5}).run(os.Stdin, os.Stdout)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func TestConnectorEndToEnd(t *testing.T) {
	t.Setenv("TT_PLUGIN_FIXTURE", "1") // inherited by the spawned subprocess
	pc := New([]string{os.Args[0]}, nil)
	pc.stderr = io.Discard
	defer pc.Close()

	ctx := context.Background()
	if err := pc.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	if pc.Name() != "fake" {
		t.Fatalf("Name() = %q, want fake", pc.Name())
	}

	ds, err := pc.Datasets(ctx)
	if err != nil {
		t.Fatalf("datasets: %v", err)
	}
	if len(ds) != 1 || ds[0].Name != "nums" {
		t.Fatalf("datasets = %+v", ds)
	}

	sch, err := pc.Resolve(ctx, connector.Dataset{Name: "nums"})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(sch.Columns) != 1 || sch.Columns[0].Name != "n" || sch.Columns[0].Type != engine.TypeInt {
		t.Fatalf("schema = %+v", sch)
	}

	it, err := pc.Scan(ctx, connector.ScanRequest{Dataset: connector.Dataset{Name: "nums"}})
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	defer it.Close()

	var got []int64
	for {
		row, ok, err := it.Next()
		if err != nil {
			t.Fatalf("next: %v", err)
		}
		if !ok {
			break
		}
		got = append(got, row.Values[0].V.(int64))
	}
	if len(got) != 5 {
		t.Fatalf("got %d rows, want 5: %v", len(got), got)
	}
	for i, v := range got {
		if v != int64(i) {
			t.Fatalf("row %d = %d", i, v)
		}
	}
}

func TestStartRejectsBadCommand(t *testing.T) {
	pc := New([]string{"/nonexistent/turntable-plugin-xyz"}, nil)
	pc.stderr = io.Discard
	if err := pc.Start(context.Background()); err == nil {
		t.Fatal("want error starting a nonexistent command")
	}
}
