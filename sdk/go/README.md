# turntable-go-sdk

The Go SDK for writing [turntable](https://github.com/undefinedopcode/turntable)
plugin connectors. A plugin is a standalone program that turntable launches as
a subprocess and drives over stdio with JSON-RPC 2.0; this module (package
`ttplugin`, dependency-free) implements all of the protocol plumbing —
framing, dispatch, scan cursors, predicate evaluation, and cell encoding — so
you only declare datasets and a function that produces rows:

```go
package main

import (
	"os"
	"strings"

	"github.com/undefinedopcode/turntable-go-sdk/ttplugin"
)

func main() {
	ttplugin.Serve(ttplugin.Plugin{
		Name: "envinfo",
		Datasets: map[string]ttplugin.Dataset{
			"env": {
				Schema: ttplugin.Schema{Columns: []ttplugin.Column{
					{Name: "name", Type: "string"},
					{Name: "value", Type: "string", Nullable: true},
				}},
				Rows: func(ttplugin.Request) (ttplugin.Rows, error) {
					var rows ttplugin.Rows
					for _, kv := range os.Environ() {
						k, v, _ := strings.Cut(kv, "=")
						rows = append(rows, ttplugin.Row{k, v})
					}
					return rows, nil
				},
			},
		},
	})
}
```

Build it and register it as a plugin source in `turntable.yaml`:

```yaml
sources:
  envinfo:
    connector: plugin
    command: ["./envinfo"]
    options: { dataset: "*" }
```

- Cells are plain Go values (`int`/`int64`, `float64`, `string`, `bool`,
  `time.Time`, `time.Duration`, `[]byte`, `nil` for NULL).
- The SDK applies the pushed-down `WHERE`/`LIMIT` to the rows you return, so
  you get pushdown for free; set `Plugin.ManualPushdown` to handle them
  yourself (the `Request` carries the decoded `Predicate`, whose `Eval` is
  exported).
- stdout carries protocol messages only — log to stderr.

The wire protocol spec and reference plugins (system state, Kubernetes, MQTT,
and more) live in the
[turntable repository](https://github.com/undefinedopcode/turntable/blob/main/PLUGINS.md).
Sibling SDKs: [Python](https://github.com/undefinedopcode/turntable-python-sdk),
[Node.js](https://github.com/undefinedopcode/turntable-node-sdk).

This repository is published from the turntable monorepo's `sdk/go` directory
(`git subtree split`); develop and file issues there.
