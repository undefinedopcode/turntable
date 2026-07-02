# Turntable plugin connectors

A **plugin** is a standalone program that acts as a turntable connector. Turntable
launches it as a subprocess and exchanges JSON-RPC 2.0 messages over the
program's stdin/stdout. This lets you add connectors in any language, without
recompiling turntable — for example a program that reports live system state
(processes, sensors, queue depth) as a queryable relation.

A plugin implements the same contract as a built-in connector
(`Datasets`/`Resolve`/`Scan`), just across a process boundary. Everything
downstream — the planner, the engine, pushdown — is identical to a built-in.

**Use an SDK** — each implements everything in this document (framing,
dispatch, cursors, predicate evaluation, cell encoding), so you only declare
datasets and a row function:

- **Go**: `github.com/undefinedopcode/turntable-go-sdk/ttplugin`
  ([repo](https://github.com/undefinedopcode/turntable-go-sdk)) — see [Go SDK](#go-sdk)
- **Python**: [`sdk/python`](sdk/python/README.md)
  ([repo](https://github.com/undefinedopcode/turntable-python-sdk)) — a single stdlib-only module
- **Node.js**: [`sdk/node`](sdk/node/README.md)
  ([repo](https://github.com/undefinedopcode/turntable-node-sdk)) — a single dependency-free ES module

The protocol spec here remains the source of truth, and is what you implement
directly in any other language. The `sdk/` directories in this repo are the
canonical source; each standalone repo is published from them with
`./sdk/publish.sh` (a history-preserving `git subtree split`).

Reference plugins, one per SDK style, live under `examples/plugins/`:

| Plugin | Language | What it shows |
| ------ | -------- | ------------- |
| [`sysinfo`](examples/plugins/sysinfo/main.go) | Go | live env + Go-runtime stats, dependency-free |
| [`procinfo`](examples/plugins/procinfo/main.go) | Go | the live process table (gopsutil) |
| [`k8s`](examples/plugins/k8s) | Go | Kubernetes resources (client-go) |
| [`mqtt`](examples/plugins/mqtt/main.go) | Go | bounded MQTT broker snapshot (paho) — subscribe, collect for a window, return rows |
| [`pyfiles`](examples/plugins/pyfiles/pyfiles.py) | Python | a directory tree as a relation — no build step |
| [`nodeos`](examples/plugins/nodeos/nodeos.mjs) | Node.js | live OS state (cpus/net/host) — no build step |

Build the Go ones with `./examples/plugins/build.sh`; the Python/Node ones run
from source (`command: ["python3", ".../pyfiles.py"]`).

## Registering a plugin

Add a source with `connector: plugin` and a `command` (the executable plus
arguments):

```yaml
sources:
  sys:
    connector: plugin
    command: ["./examples/plugins/sysinfo"]   # or ["go", "run", "./examples/plugins/sysinfo"]
    options:
      dataset: "*"        # expose every dataset the plugin advertises
                          # (or a single dataset name; default: the source name)
```

- One subprocess backs all of a plugin's datasets.
- The plugin's advertised name also becomes a qualified-ref prefix, so
  `SELECT * FROM sysinfo:env` works in addition to the named source.
- `command` arguments support `${ENV_VAR}` interpolation like any other config
  string.
- In the REPL: `.use sys plugin command="./examples/plugins/sysinfo" dataset=*`
  (the `command` string is whitespace-split; use a config-file list for
  arguments that contain spaces).

### Trust model

A plugin is **arbitrary code that turntable executes**. Only configure plugins
you trust, from a config file you control. Plugin commands are deliberately *not*
accepted through the web add-source UI, which can register data sources but must
never launch processes. Treat a `command:` entry with the same care as a shell
command in your shell profile.

## Go SDK

The SDK module `github.com/undefinedopcode/turntable-go-sdk/ttplugin` is dependency-free
(standard library only), so importing it does not pull in turntable's own
dependency graph. It implements the entire protocol below — framing, dispatch,
scan cursors, predicate **evaluation**, limit, and cell encoding — leaving you to
declare datasets and a function that returns rows:

```go
package main

import (
	"os"
	"strings"

	"github.com/undefinedopcode/turntable-go-sdk/ttplugin"
)

func main() {
	ttplugin.Serve(ttplugin.Plugin{
		Name: "sysinfo",
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

- Return cells as plain Go values (`int`/`int64`, `float64`, `string`, `bool`,
  `time.Time`, `time.Duration`, `[]byte`, `nil`, or any JSON value for an `any`
  column); the SDK encodes them to the wire form below.
- By default the SDK **applies the pushed-down `WHERE` and `LIMIT`** to the rows
  you return, so you get predicate/limit pushdown for free. Set
  `Plugin.ManualPushdown` to handle them yourself (e.g. to push filters into a
  remote backend); the `Request` still carries the decoded `Predicate`/`Limit`,
  and `Predicate.Eval` is exported so you can reuse the evaluator.

A plugin needs nothing from turntable itself — the SDK is the only import. Each
example under `examples/plugins/` is its own module (like a real external plugin)
depending only on the SDK (and, for `procinfo`, gopsutil); they resolve the SDK
locally via a `replace` directive while in this repo.

## Python SDK

[`sdk/python/ttplugin.py`](sdk/python/ttplugin.py) is a single stdlib-only
module with the same shape — declare `Column`s, `Dataset`s with a rows
function, and `serve()`:

```python
import ttplugin

ttplugin.serve(ttplugin.Plugin(
    name="envinfo",
    datasets={
        "env": ttplugin.Dataset(
            columns=[ttplugin.Column("name", "string"),
                     ttplugin.Column("value", "string", nullable=True)],
            rows=lambda req: [[k, v] for k, v in os.environ.items()],
        ),
    },
))
```

Cells: int, float, str, bool, `datetime` (time), `timedelta` (duration),
`bytes`, `None`. Automatic `WHERE`/`LIMIT` application like the Go SDK
(`manual_pushdown=True` + `eval_predicate` to take over). Register with
`command: ["python3", "./envinfo.py"]` — no build step.

## Node.js SDK

[`sdk/node/ttplugin.js`](sdk/node/ttplugin.js) is a single dependency-free ES
module; rows functions may be async:

```js
import { serve } from "ttplugin"; // or a relative path to ttplugin.js

serve({
  name: "osinfo",
  datasets: {
    cpus: {
      columns: [{ name: "model", type: "string" }, { name: "speed_mhz", type: "int" }],
      rows: () => os.cpus().map((c) => [c.model, c.speed]),
    },
  },
});
```

Cells: number, string, boolean, `Date` (time), `Buffer` (bytes), `null`.
Automatic `WHERE`/`LIMIT` (`manualPushdown: true` + `evalPredicate` to take
over). Register with `command: ["node", "./osinfo.mjs"]`.

Both SDKs are held to the Go client's behavior by conformance tests
(`internal/connector/connectors/pluginc/sdkconform_test.go`) that drive the
fixtures under `testdata/` through the real subprocess path; they skip when
the interpreter is not installed.

Authors in other languages implement the wire protocol directly; the rest of this
document is that protocol.

## Transport

Messages are framed exactly like the Language Server Protocol: an ASCII header
block, a blank line, then the JSON payload.

```
Content-Length: 52\r\n
\r\n
{"jsonrpc":"2.0","id":1,"method":"initialize",...}
```

- Only `Content-Length` is required; other headers are ignored.
- **stdout** carries protocol messages only. Write diagnostics/logs to
  **stderr** — turntable forwards them to its own stderr.
- Requests are issued one at a time (turntable serializes them), so a plugin may
  process messages in a simple read-handle-write loop.

## Messages

All requests are JSON-RPC 2.0. A request has an `id`; turntable expects a
response with the same `id`. `shutdown` is a notification (no `id`, no response).

| Method       | Direction | Purpose                                  |
| ------------ | --------- | ---------------------------------------- |
| `initialize` | → plugin  | handshake: version, name, capabilities   |
| `datasets`   | → plugin  | list datasets (optional if advertised)   |
| `resolve`    | → plugin  | schema of one dataset                    |
| `scan`       | → plugin  | open a row cursor                         |
| `next`       | → plugin  | pull a batch of rows from a cursor       |
| `close`      | → plugin  | release a cursor                          |
| `shutdown`   | → plugin  | notification: exit cleanly               |

### `initialize`

```jsonc
// params
{ "protocolVersion": 1, "options": { "dataset": "*" } }
// result
{
  "protocolVersion": 1,
  "name": "sysinfo",
  "capabilities": { "predicatePushdown": true, "limitPushdown": true },
  "datasets": [ { "name": "env" }, { "name": "runtime" } ]   // optional
}
```

`options` is the source's `options` map verbatim. `capabilities` is how a plugin
opts into pushdown; every flag defaults to false. Advertising `datasets` here
lets turntable skip a separate `datasets` call.

Capabilities: `predicatePushdown`, `projectionPushdown`, `limitPushdown`,
`orderByPushdown`. Turntable only sends a hint for a capability the plugin
advertised.

### `datasets`

```jsonc
// params: {}
// result
{ "datasets": [ { "name": "env", "source": "", "options": {} } ] }
```

### `resolve`

```jsonc
// params
{ "dataset": { "name": "env", "options": {} } }
// result
{ "schema": { "columns": [
    { "name": "name",  "type": "string", "nullable": false },
    { "name": "value", "type": "string", "nullable": true }
] } }
```

### `scan`

```jsonc
// params
{
  "dataset":   { "name": "env", "options": { "root": "." } },  // the source's options
  "columns":   ["name"],          // projection hint (omitted unless capable)
  "limit":     100,               // omitted unless capable
  "predicate": { ...subset... },  // omitted unless capable; see below
  "orderBy":   [ { "column": "name", "desc": false } ]
}
// result
{
  "scanId": "1",
  "schema": { "columns": [ ... ] },           // same shape as resolve
  "applied": { "predicate": true, "limit": false }
}
```

`scan` opens a cursor and returns an id; it does **not** return rows. Return the
**full resolved schema** in `schema`, in resolve column order (projection
pushdown does not yet change the row shape turntable expects).

`applied` reports which hints the plugin honored. It is **advisory** — turntable
always keeps its own Filter/Sort/Limit above the scan, so anything the plugin
ignores or partially applies is corrected for free. A plugin that implements zero
pushdown is fully correct; pushdown is only an optimization to cut rows crossing
the pipe.

### `next`

```jsonc
// params
{ "scanId": "1", "maxRows": 1000 }
// result
{ "rows": [ ["PATH", "/usr/bin"], ["HOME", "/home/x"] ], "done": false }
```

Each row is a positional JSON array aligned to the scan schema. Turntable calls
`next` repeatedly until `done` is true. Cell encoding by column type:

| type       | JSON encoding                                            |
| ---------- | ------------------------------------------------------- |
| `int`      | number (or numeric string)                              |
| `float`    | number                                                  |
| `string`   | string                                                  |
| `bool`     | boolean                                                 |
| `time`     | RFC3339 string, e.g. `"2026-06-29T21:00:00Z"`           |
| `duration` | Go duration string (`"1h30m"`) or integer nanoseconds   |
| `bytes`    | base64 string                                           |
| `any`      | any JSON value (objects/arrays preserved)               |

A JSON `null` is SQL NULL for any column. A short row is NULL-padded; extra
cells are ignored.

### `close` / `shutdown`

```jsonc
// close  params: { "scanId": "1" }   result: {}
// shutdown is a notification: { "jsonrpc": "2.0", "method": "shutdown" }
```

On `shutdown` (or stdin EOF) the plugin should exit. Turntable sends `shutdown`,
closes stdin, and kills the process if it does not exit within a couple of
seconds.

## Predicate pushdown subset

When the plugin advertises `predicatePushdown` and turntable has a `WHERE` it can
express, it sends a `predicate` — a small JSON expression tree. This is a narrow
**subset** of turntable's SQL; anything it can't express is simply omitted, and
the engine applies it. **Pushing a partial predicate is always safe** because the
engine re-applies the full `WHERE` regardless.

Node kinds:

```jsonc
{ "kind": "compare", "op": "=", "column": "pid", "value": { "type": "int", "value": 42 } }
{ "kind": "and", "args": [ <node>, <node>, ... ] }
{ "kind": "or",  "args": [ <node>, <node>, ... ] }
{ "kind": "not", "arg": <node> }
{ "kind": "in", "column": "status", "values": [ <lit>, ... ], "negate": false }
{ "kind": "between", "column": "x", "low": <lit>, "high": <lit>, "negate": false }
{ "kind": "like", "column": "name", "pattern": "foo%", "insensitive": false, "negate": false }
{ "kind": "isnull", "column": "deleted_at", "negate": false }
```

- `op` is one of `=`, `<>`, `<`, `<=`, `>`, `>=` (comparisons are normalized to
  `column OP literal`).
- A literal is `{ "type": "int|float|string|bool|null", "value": ... }`.
- A plugin should handle the kinds it understands and **decline the whole
  predicate** (report `applied.predicate: false`, return unfiltered rows) if it
  meets a kind it doesn't — turntable then filters. Never silently drop rows.
- Only push `limit` once the predicate is fully applied; if you filtered
  partially, returning fewer rows than the limit would be wrong.

See `canHandle` / `evalPred` in the reference plugin for a compact evaluator.

## Versioning

`protocolVersion` is currently `1`. Turntable rejects a plugin advertising a
different version rather than risk misreading messages. New capabilities are
added as optional `capabilities` flags so older plugins keep working unchanged.
