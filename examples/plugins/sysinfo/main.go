// Command sysinfo is a reference turntable plugin connector built on the Go SDK
// (github.com/april/turntable/sdk/go/ttplugin). The SDK handles all of the
// JSON-RPC protocol plumbing — framing, dispatch, scan cursors, predicate
// evaluation, limit, cell encoding — so this program only declares its datasets
// and the functions that produce rows.
//
// It exposes live system state:
//
//	env       one row per environment variable (name, value)
//	runtime   a single row of live Go-runtime stats (recomputed every scan)
//
// Build and register it (see PLUGINS.md):
//
//	go build -o bin/sysinfo ./examples/plugins/sysinfo   # from this module dir
//	# turntable.yaml:
//	#   sysinfo:
//	#     connector: plugin
//	#     command: ["/abs/path/bin/sysinfo"]
//	#     options: {dataset: "*"}
//
// Compare this file with git history to see how much the SDK removes: the
// hand-rolled version was ~470 lines of mostly-boilerplate.
package main

import (
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/april/turntable/sdk/go/ttplugin"
)

func main() {
	if err := ttplugin.Serve(ttplugin.Plugin{
		Name: "sysinfo",
		Datasets: map[string]ttplugin.Dataset{
			"env": {
				Schema: ttplugin.Schema{Columns: []ttplugin.Column{
					{Name: "name", Type: "string"},
					{Name: "value", Type: "string", Nullable: true},
				}},
				Rows: envRows,
			},
			"runtime": {
				Schema: ttplugin.Schema{Columns: []ttplugin.Column{
					{Name: "goroutines", Type: "int"},
					{Name: "cpus", Type: "int"},
					{Name: "alloc_bytes", Type: "int"},
					{Name: "heap_objects", Type: "int"},
					{Name: "now", Type: "time"},
				}},
				Rows: runtimeRows,
			},
		},
	}); err != nil {
		os.Exit(1)
	}
}

// envRows lists the process environment. The SDK applies any pushed-down WHERE
// (e.g. name = 'PATH') and LIMIT to what we return.
func envRows(ttplugin.Request) (ttplugin.Rows, error) {
	environ := os.Environ()
	rows := make(ttplugin.Rows, 0, len(environ))
	for _, kv := range environ {
		k, v, _ := strings.Cut(kv, "=")
		rows = append(rows, ttplugin.Row{k, v})
	}
	return rows, nil
}

// runtimeRows reports live Go-runtime stats; recomputed on every scan, so each
// query sees current values.
func runtimeRows(ttplugin.Request) (ttplugin.Rows, error) {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return ttplugin.Rows{{
		runtime.NumGoroutine(),
		runtime.NumCPU(),
		int64(m.Alloc),
		int64(m.HeapObjects),
		time.Now(),
	}}, nil
}
