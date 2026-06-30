// Command procinfo is a turntable plugin connector that exposes the live process
// table as a queryable relation, built on the Go SDK
// (github.com/april/turntable/sdk/go/ttplugin) and gopsutil for cross-platform
// process enumeration.
//
// It demonstrates how little code a real plugin needs: the SDK handles the
// protocol and pushdown, gopsutil supplies the data, and this file is just a
// schema plus a row builder.
//
//	processes   one row per running process
//
// Build and register it (its own module; build from this directory):
//
//	go build -o bin/procinfo ./examples/plugins/procinfo
//	# turntable.yaml:
//	#   procinfo:
//	#     connector: plugin
//	#     command: ["/abs/path/bin/procinfo"]
//	#     options: {dataset: processes}
//
// Then, for example:
//
//	SELECT pid, name, mem_rss FROM processes ORDER BY mem_rss DESC LIMIT 10
package main

import (
	"os"
	"time"

	"github.com/april/turntable/sdk/go/ttplugin"
	"github.com/shirou/gopsutil/v4/process"
)

func main() {
	if err := ttplugin.Serve(ttplugin.Plugin{
		Name: "procinfo",
		Datasets: map[string]ttplugin.Dataset{
			"processes": {
				Schema: ttplugin.Schema{Columns: []ttplugin.Column{
					{Name: "pid", Type: "int"},
					{Name: "ppid", Type: "int", Nullable: true},
					{Name: "name", Type: "string", Nullable: true},
					{Name: "username", Type: "string", Nullable: true},
					{Name: "status", Type: "string", Nullable: true},
					{Name: "cpu_percent", Type: "float", Nullable: true},
					{Name: "mem_rss", Type: "int", Nullable: true},
					{Name: "threads", Type: "int", Nullable: true},
					{Name: "create_time", Type: "time", Nullable: true},
					{Name: "cmdline", Type: "string", Nullable: true},
				}},
				Rows: processRows,
			},
		},
	}); err != nil {
		os.Exit(1)
	}
}

// processRows enumerates the live process table. Per-process fields are
// best-effort: a process can exit, or be unreadable due to permissions, between
// enumeration and inspection, so individual field errors yield NULL rather than
// failing the whole scan. The SDK applies any pushed-down WHERE/LIMIT to the
// result.
func processRows(ttplugin.Request) (ttplugin.Rows, error) {
	procs, err := process.Processes()
	if err != nil {
		return nil, err
	}
	rows := make(ttplugin.Rows, 0, len(procs))
	for _, p := range procs {
		ppid, _ := p.Ppid()
		name, _ := p.Name()
		user, _ := p.Username()
		threads, _ := p.NumThreads()
		cpu, _ := p.CPUPercent()

		var status any
		if st, err := p.Status(); err == nil && len(st) > 0 {
			status = st[0]
		}
		var rss any
		if mi, err := p.MemoryInfo(); err == nil && mi != nil {
			rss = int64(mi.RSS)
		}
		var created any
		if ms, err := p.CreateTime(); err == nil && ms > 0 {
			created = time.UnixMilli(ms)
		}
		cmd, _ := p.Cmdline()

		rows = append(rows, ttplugin.Row{
			int(p.Pid), int(ppid), nullStr(name), nullStr(user), status,
			cpu, rss, int(threads), created, nullStr(cmd),
		})
	}
	return rows, nil
}

// nullStr maps an empty string to NULL so missing fields read as NULL, not "".
func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}
