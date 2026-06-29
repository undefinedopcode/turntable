# Turntable plugin connectors

A **plugin** is a standalone program that acts as a turntable connector. Turntable
launches it as a subprocess and exchanges JSON-RPC 2.0 messages over the
program's stdin/stdout. This lets you add connectors in any language, without
recompiling turntable — for example a program that reports live system state
(processes, sensors, queue depth) as a queryable relation.

A plugin implements the same contract as a built-in connector
(`Datasets`/`Resolve`/`Scan`), just across a process boundary. Everything
downstream — the planner, the engine, pushdown — is identical to a built-in.

A complete, dependency-free reference implementation lives in
[`examples/plugins/sysinfo`](examples/plugins/sysinfo/main.go); read it alongside
this spec.

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
  "dataset":   { "name": "env" },
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
