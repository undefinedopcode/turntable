# Design: MCP server

Status: **steps 1–3 implemented**. Step 1: `turntable mcp` subcommand in
`internal/cli/mcp.go` with the `query`, `list_sources`, `describe_source`,
and `list_functions` tools over stdio, tested in `mcp_test.go` via the SDK's
in-memory transport. Step 2: connector specs ported to Go (`connspec.go`, the
source of truth — served as `GET /api/connectors`, consumed by the web modal,
and driving `isFileConnector`), plus the `list_connectors`, `add_source`
(plugin rejected, secrets as `${ENV_VAR}`), and `remove_source` tools.
Step 3: `list/get/save/delete_dashboard` over the YAML store (validation shared
with the web POST via `saveDashboardChecked`) and `render_dashboard` over the
headless renderer (`renderDashboard` was already transport-neutral — no
factoring needed). Step 4 below remains future work.

Goal: expose turntable to MCP clients — Claude Code first — so an agent can
add data sources, explore schemas, construct and run queries, and build
dashboards conversationally. The motivating loop:

> "Here's a CSV of stations and a Postgres DSN — join them, find the
> anomalous flow readings from last night, and pin the analysis as a
> dashboard I can open in the web UI."

Everything an agent needs for that loop already exists behind the web API
(`serve.go`) and the REPL: `execStmt` (parse → plan → exec with a row cap),
`sessionStmtResponse` (views/matviews), `registerRuntimeSource` (validated,
secret-safe source adds), the dashboard YAML store (`dashboard.go`), and the
headless renderer (`dashrender.go`). The MCP server is a **third transport
over the same App methods** — no new engine or connector capability. Where
the web UI's frontend owns a piece of logic the agent needs (per-connector
field specs in `connectorSpecs.ts`), this design moves that piece server-side
rather than duplicating it.

## Shape

- **Entry point**: a `turntable mcp` subcommand, dispatched in `App.Run`
  exactly like `dashboard` (after flags/config/source registration, so `-c`,
  `.env` loading, `--max-rows`, and config-declared sources all apply). Runs
  an MCP server on **stdio** until EOF/signal. No HTTP transport in v1 —
  Claude Code launches local stdio servers directly, and stdio sidesteps the
  auth/exposure questions `--serve` has to answer with "localhost only".
- **Registration** (project `.mcp.json`):

  ```json
  {
    "mcpServers": {
      "turntable": {
        "command": "turntable",
        "args": ["mcp", "-c", "turntable.yaml"]
      }
    }
  }
  ```

- **Code layout**: `internal/cli/mcp.go` (+ `mcp_test.go`), a sibling of
  `serve.go`, so tool handlers call the unexported App methods directly. If
  it grows past a file or two, split into `internal/cli/mcpserver/` with the
  App passed in — but don't start there.
- **SDK**: the official `github.com/modelcontextprotocol/go-sdk` (`mcp`
  package). Pure Go, no CGO, gives us stdio transport, tool/resource
  registration with JSON-schema'd inputs, structured tool results, and an
  in-memory transport for tests. (Alternative: `mark3labs/mcp-go`, more
  widely deployed historically; the official SDK is the safer long-term bet
  and its API is the one Anthropic documents.) Remember the tidy caveat:
  `GOFLAGS=-tags=integration go mod tidy` when adding the dep.
- **Stdout discipline**: in MCP mode stdout carries only JSON-RPC frames.
  `App.Out` must not be written to; startup warnings already go to `a.Err`
  (stderr), which MCP clients capture as server logs. The subcommand should
  assert this by simply never using `a.Out`.
- **Concurrency**: one client, but requests can interleave. `serve.go` gets
  away without locking because the registry/matview maps are effectively
  written only from the single-user add-source path; the MCP server should
  take the cheap insurance of a per-App mutex around mutating tools
  (`add_source`, session statements, dashboard saves) since an agent *will*
  fire tool calls in parallel.

## Tools

Verbs an agent composes, not a REST mirror. All read tools set
`readOnlyHint`. Every result is one JSON text block (plus
`structuredContent` where the SDK makes it free) — predictable for the
model, trivial to test.

### Query

**`query`** — the centerpiece. Input:

| field      | type   | notes                                                        |
|------------|--------|--------------------------------------------------------------|
| `sql`      | string | required                                                     |
| `max_rows` | int    | optional; default **200**, capped at `--max-rows` (else 5000) |
| `explain`  | bool   | return the plan instead of executing                         |

Wraps `sql.Parse` → `sessionStmtResponse` (so `CREATE VIEW` / `CREATE
MATERIALIZED VIEW` / `REFRESH` / `DROP` work through the same tool and
return a `notice`, mirroring the web path) → `explainStmt` or `execStmt`.
Output is the `queryResponse` shape serve.go already defines: `columns`
(name/type/nullable), `rows` (positional arrays, `jsonValue` encoding),
`count`, `truncated`, `elapsed_ms`, `notice`/`explain`/`error`.

Two deliberate differences from the web API:

- **Default row cap of 200, not 5000.** Rows land in the model's context;
  5000 rows is a context bomb. `truncated: true` plus a hint string
  ("re-run with max_rows, or aggregate") teaches the agent to narrow with
  SQL instead of paging. The web UI's cap is a rendering guard; this one is
  a token budget.
- **Query errors are the tool result, not a protocol error** (same policy as
  the web API's HTTP-200-with-`error` field): a parse/plan/exec failure is
  information the agent should read and react to — set `isError` on the MCP
  result but keep the full message in-band.

**`list_functions`** — `{scalar: [...], aggregate: [...], keywords: [...]}`,
same data as `handleFunctions` / REPL `.functions`. This is the anti-drift
channel: agents get the live dialect surface rather than trusting training
data or a stale doc.

### Sources & schema

**`list_sources`** — `[{name, connector}]` including views tagged
`connector: "view"`, exactly `listSources`.

**`describe_source`** — input `{name}`; columns/types/nullable via
`Conn.Resolve`, view schemas via `viewSchemaFor`, file freshness via
`fileMeta`. The note from `handleSchema` applies: resolving may hit the
network, which is fine — it's on-demand.

**`list_connectors`** — the schema-discovery tool for `add_source`: every
registered connector with its field specs (key, label, required, sensitive,
enum options, file-or-option routing, free-text note). This is the one
genuinely new piece of work: those specs currently live only in
`webui/src/connectorSpecs.ts`. Port them to Go (`internal/cli/connspec.go`,
a `[]ConnectorSpec` literal) and make that the source of truth:

- the MCP tool serializes it;
- a new `GET /api/connectors` serves it to the web UI, whose
  `connectorSpecs.ts` shrinks to fetch + types (or is generated — either
  way, one list to maintain when a connector is added);
- `isFileConnector` in `repl.go` becomes derivable from `Spec.File`, killing
  a second parallel list.

Sensitive fields carry `sensitive: true` and the spec text an agent needs:
"pass a `${ENV_VAR}` reference, never a literal credential".

**`add_source`** — input `{name, connector, fields: {k: v}, save?: bool}`.
Routes fields through `applySourceField` and registers via
`registerRuntimeSource` — so `ValidateSourceSecrets` (literal credentials
rejected), `${ENV_VAR}` interpolation, and wildcard expansion (`table=*`,
`sheet=*`, `dataset=*`) behave identically to `.use` and the web modal.
Returns `{registered: [names...]}`. With `save: true`, also
`config.AppendSource` to `a.configPath` (declared, secret-free form) and
report the path.

One policy decision, resolved the same way as the web UI: **`connector:
plugin` is rejected** (`command:` is arbitrary exec). Yes, a Claude Code
agent can already run commands — but the MCP server shouldn't be the venue
where "add a data source" quietly escalates to "run a program", and other
MCP clients are not shells. Plugin sources declared in `turntable.yaml`
still load and are fully queryable; the agent can *edit the config* and say
so. If demand appears, a `turntable mcp --allow-plugin-sources` flag is the
opt-in, not a default.

**`remove_source`** — input `{name}`; `Reg.RemoveSource` (already exists for
`DROP MATERIALIZED VIEW`). Session-scoped adds are otherwise permanent for
the server's lifetime, and an agent iterating on wildcard expansions needs
an undo. Does not touch the config file.

### Dashboards

The server side of dashboards is a YAML file store; the MCP tools are that
store, verbatim — the same "server stores definitions, never runs panel
queries" split as the web API (`docs/dashboards-design.md`).

**`list_dashboards`** — `listDashboards()`: slug, name, description, panel
count, parse errors surfaced not hidden.

**`get_dashboard`** — full definition for a slug, panels and `view` configs
included, so an agent can read-modify-write.

**`save_dashboard`** — upsert keyed by slug (empty slug ⇒ `slugify(name)`),
through `Dashboard.validate()`. The input schema documents panel kinds
(markdown/table/pivot/chart/stat), `width: full|half`, `{{var}}`
substitution, and — importantly for authoring quality — the `view` object
being the frontend `ViewConfig` with **column-name (not index) references**.
A short worked example in the tool description (one markdown panel, one
chart panel with `x`/`series`) is worth more than schema prose here; agents
write correct ViewConfigs from one example. No `append_panel` convenience in
v1: get → append in-model → save is three calls and keeps the tool surface
small.

**`delete_dashboard`** — by slug; `destructiveHint`.

**`render_dashboard`** — input `{slug, out?, variables?: {k: v}}`; drives
the `dashrender.go` path (panel execution with variable overrides →
self-contained HTML) and returns the written file path. This closes the
loop headlessly: the agent builds a dashboard *and hands the user an
artifact* without anyone starting `--serve`. Requires factoring the render
core out of `dashboardCmd`'s CLI arg-parsing into a method both call —
small, and `dashrender_test.go` already tests the core.

### Log inference (v1.5, optional)

**`infer_log_format`** — input `{path}`; wraps the `handleLoginfer` logic:
detected format + column preview, or mined templates each carrying a
ready-to-use `pattern` regex. The agent flow — "here's a weird log file" →
infer → `add_source` with `pattern=` → query — is exactly the add-source
modal's flow, and everything is already factored (`logc.Detect`,
`logc.Sample`, `loginfer.Infer`). Cheap to add once the core tools exist;
not load-bearing for v1.

## Resources

Thin, and secondary to tools (tool support is universal across clients;
resource UX varies):

- **`turntable://dialect`** — DIALECT.md, embedded via `go:embed` at build
  time. The single highest-leverage context an agent can pin: the dialect is
  SQL-*like*, and the differences (connector refs in FROM, `ASOF JOIN`,
  `DATE_BIN`, window frames, LOCF/DELTA/RATE) are precisely what an agent
  will get wrong from generic SQL priors. Note DIALECT.md lives at the repo
  root, so the embed needs a small `//go:generate` copy or an `embed` from a
  doc package — decide at implementation; do not hand-duplicate the text.
- **`turntable://plugins`** — PLUGINS.md, same mechanism, for "write me a
  plugin" sessions (the agent authors against the SDKs; turntable runs the
  result from the config, not via MCP — see the plugin policy above).

Per-source schema resources (a `turntable://schema/{source}` template) are
redundant with `describe_source` and add client-compat surface; skip unless
a client-side reason appears. Prompts likewise: skip in v1.

## Security posture

- Queries are read-only by the engine's design (no DDL/DML), but they run
  with **this process's file and network access** — the same caveat
  `exposureNote` prints for `--serve`. Stdio keeps the transport
  single-user and local; there is no network listener.
- Secrets: `ValidateSourceSecrets` already guarantees credentials enter as
  `${ENV_VAR}` references. The MCP server adds one more reason that matters:
  tool inputs and outputs land in **conversation transcripts**. Literal
  secrets in an `add_source` call would be transcript-persisted; the
  validation rejecting them is now protecting the chat log, not just the
  config file. Tool descriptions should say this plainly.
- Query *results* can still contain sensitive data and will enter the
  transcript. That is inherent to the use case (the agent must see data to
  analyze it) and is the user's call when they connect a source; the spec
  just notes it — no server-side redaction in v1.
- Plugin sources: not creatable via MCP (above). File paths in `add_source`
  are not sandboxed (the CLI and REPL aren't either); the process's own
  permissions are the boundary.

## Testing

Mirror `serve_test.go`: table-driven tests in `mcp_test.go` against an
in-process client over the SDK's in-memory transport — register a CSV/JSON
fixture source, then exercise each tool: query happy path, parse error
in-band, truncation at `max_rows`, session statements returning notices,
`add_source` rejecting a literal secret and a `plugin` connector, dashboard
save→get→render round-trip. No subprocess, no network; stays inside the
default pure-Go suite.

## Sequencing

1. **Core query loop**: `turntable mcp` subcommand, SDK dep, stdio wiring;
   `query`, `list_sources`, `describe_source`, `list_functions`. Usable in
   Claude Code on day one against config-declared sources.
2. **Source management**: connector specs to Go (`list_connectors`, plus
   `GET /api/connectors` and the frontend consuming it), `add_source`,
   `remove_source`.
3. **Dashboards**: `list/get/save/delete_dashboard`; factor the dashrender
   core and add `render_dashboard`.
4. **Context & polish**: `turntable://dialect` and `turntable://plugins`
   resources, `infer_log_format`, tool-description examples tuned against
   real agent transcripts.

Each step ships independently; 1 is worth releasing alone.

## Out of scope (for now)

- HTTP/SSE transport, auth, multi-client — revisit if a remote/hosted use
  case appears; `--serve` remains the browser story.
- Server-side panel execution or dashboard screenshots via MCP — the
  render tool returns HTML; pixels are a client concern.
- Write access of any kind to source data (engine is read-only by design).
- MCP *client* support (turntable consuming other MCP servers as a
  connector). Interesting — an `mcpc` connector could query any MCP
  server's resources as relations — but it is a connector design, not part
  of this server.
