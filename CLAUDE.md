# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

Turntable is a CLI that queries heterogeneous data sources (JSON, CSV, YAML,
Excel, SQL databases) through one SQL-style dialect. Each `FROM` reference is
routed to a **connector**, which produces typed rows; the in-memory **engine**
runs the relational operators (filter, join, aggregate, project, sort) that the
connector did not push down. Read-only by design — no DDL/DML.

`DESIGN.md` is the authoritative design doc, but note it describes some
aspirational structure that does not match the tree yet (e.g. it lists
`plan/resolver.go`, `plan/validate.go`, `plan/pushdown.go` and a `pkg/`
directory — none exist; the real `plan` package is just `plan.go` + `exec.go`).
Trust the actual source over DESIGN.md when they disagree.

`DIALECT.md` is the user-facing SQL language reference (grammar, types,
functions). When you add/change a function or dialect feature, update it — and
the function list is also exposed live via the REPL `.functions` command
(`FuncRegistry.Names()` + `engine.Aggregates()`), so prefer pointing users there
over re-listing functions in prose that drifts.

## Commands

```bash
go build ./cmd/turntable          # build the `turntable` binary (pure Go, no CGO)
go test ./...                     # run all tests (pure-Go, no network/servers)
go test ./internal/sql/...        # test one package
go test -run TestParseSelect ./internal/sql   # run a single test by name
go vet ./...                      # vet

# SQL connector integration tests against real servers (embedded Postgres +
# in-process go-mysql-server). Gated behind a build tag so the default suite
# stays pure-Go; the Postgres case downloads a server binary on first run.
go test -tags integration ./internal/connector/connectors/sqlc/
# IMPORTANT: those deps are only referenced from integration-tagged files, so a
# plain `go mod tidy` PRUNES them. Always tidy with the tag:
GOFLAGS=-tags=integration go mod tidy

./examples/run.sh                 # run all demo queries end-to-end
./examples/run.sh 10              # run demo #10 only (SQL pushdown)
sqlite3 examples/data/inventory.db < examples/init_sqlite.sql   # (re)create demo DB
```

The SQLite driver is `modernc.org/sqlite` (pure Go) — builds and tests need no
CGO and no system SQLite. `sqlc` tests use in-memory SQLite, so they run in CI
without external services.

Run the tool from source during development with `go run ./cmd/turntable ...`.

## Request flow / architecture

A query moves through fixed stages, one package each:

1. **`internal/sql`** — `lexer.go` → `parser.go` → `ast.go`. Tokenizes and
   parses the SQL subset into an `Expr`/statement AST. Owns the dialect. A
   `scheme://...` run after an identifier lexes as a single `TKURL` token
   (terminating at whitespace/`,`/`)`/`;`/quote), so inline URL refs like
   `FROM http://host/data.json` parse: the scheme becomes the connector prefix
   (`http`/`https` both map to httpc) and the full URL becomes `TableRef.Source`.
   This also makes `sql:postgres://...` capture the whole DSN as the source.
2. **`internal/plan`** — `plan.go` resolves table refs to connectors via the
   `Registry`, infers/merges schemas, validates columns/types, and builds a
   tree of `plan.Node` (`Scan`, `Filter`, `Project`, `Join`, aggregate, etc.,
   plus `NoFrom` for `SELECT <expr>` with no FROM, `Subquery` for an aliased
   FROM-clause derived table, and `Union` for `UNION`/`UNION ALL`). `Build`
   takes a `sql.Statement` and dispatches on `*SelectStmt` vs `*SetOpStmt`
   (UNION); `buildSetOp` lays a `Union` under the union-level Sort/Limit. A
   `Subquery` passes its child plan's rows through under an alias, so
   `resolverFor`/`baseRelation` treat it like a Scan for column qualification.
   A `WHERE/HAVING x IN (SELECT ...)` is handled differently: `resolveInSubqueries`
   executes the (non-correlated) subquery at build time and folds its one column
   into a literal `InExpr.List` (`valueToLiteral`), so the engine needs no
   subquery support and the folded list is even pushdown-eligible. (Build-time
   execution means `--explain` runs IN subqueries.) `exec.go` lowers the tree
   into an `engine.RowIterator` pipeline (`Union` → `ConcatIter` + optional
   `DistinctIter`).
3. **`internal/engine`** — pull-based operator pipeline over `[]Value` rows
   aligned to a `Schema`. `ops.go` = operators, `eval.go` = expression
   evaluation, `funcs.go` = the scalar + aggregate **function registry**
   (`FuncRegistry`), `value.go`/`types.go` = the `Value`/`Type`/`Schema`/`Row`
   model. Joins: hash for equi-joins, nested-loop fallback.
4. **`internal/render`** — formats the final rows. `render.go` +
   `stream_test.go`; csv/json/ndjson/yaml/raw stream row-by-row (bounded
   memory), `table` buffers to compute column widths.

**`internal/connector`** is the extension surface, depended on only through
interfaces:

- `connector.go` defines `Connector` (`Name`/`Datasets`/`Resolve`/`Scan`),
  `Dataset`, `ScanRequest` (projection/predicate/limit/orderby pushdown), and
  `ScanResponse` (what the connector *actually applied* — the engine re-applies
  the residual). `Expr` is aliased from `internal/sql`.
- `registry.go` maps logical source names → `Source{Conn, Dataset}` and short
  prefixes (`csv`, `sql`, …) → `Connector` instances.
- `connectors/<name>/` are the implementations, in three families:
  - **File** (`jsonc`, `csvc`, `yamlc`, `excelc`, `parquetc`): locate data by a
    local path; infer schema from a sample/footer; push down only columns/limit.
    `claudelogsc` is a specialized local-JSONL reader for Claude Code transcripts
    (`~/.claude/projects/<slug>/*.jsonl`): text extraction from string-or-array
    `content`, a `path`/`project` option (or default to the cwd's project), and a
    `kind` option selecting one of three fixed schemas — `messages` (default; one
    row per message), `tools` (one row per `tool_use` block), or `tool_results`
    (one row per `tool_result` block; join `tool_use_id` to a tools-view
    `tool_id`). The iterator buffers the N rows a message produces. Its options
    route through
    `Dataset.Options`, not the file-path field, so it is **not** in
    `isFileConnector`.
  - **SQL** (`sqlc`): pushes `WHERE`/`ORDER BY`/`LIMIT` into the DB via
    `database/sql`; discovers schema via `PRAGMA`/`information_schema`/`DESCRIBE`.
    Three drivers are compiled in (blank imports in `sqlc.go`): `sqlite`
    (`modernc.org/sqlite`, pure Go), `postgres` (`lib/pq`), `mysql`
    (`go-sql-driver/mysql`). A `dialect` (in `sqlc.go`, keyed by driver name)
    abstracts the two things that actually differ per engine: identifier
    quoting (`quoteIdent` — backticks for MySQL, double quotes otherwise) and
    bind placeholders (`placeholder` — `$N` for Postgres, `?` otherwise). Thread
    the dialect through any new query-building code; do **not** hardcode quoting.
  - **URL/API** (`httpc`, `linearc`, `trelloc`, `cwlogsc`, `cwmetricsc`,
    `dynamodbc`, `aztablesc`): locate data by a URL and/or option keys (no local
    path). All reach the network through a small injected client interface so
    tests run with a fake, no credentials. `httpc` fetches a JSON doc; `linearc`
    queries the Linear GraphQL API; `trelloc` queries the Trello REST API with
    fixed datasets (boards/lists/cards/members, board-scoped where needed) and
    key+token auth via the `Authorization` header — its structure mirrors
    `linearc` (fixed schemas, `coerce`, field flattening); `azdevopsc` exposes
    Azure DevOps Boards `work_items` (PAT basic auth) and hides the two-step
    WIQL→batch-fetch API behind its injected `devopsAPI` — fields are namespaced
    (`System.Title`, `System.AssignedTo.displayName`) so columns use a
    multi-segment `path` like `linearc`; `cwlogsc`/`cwmetricsc`/`dynamodbc` wrap
    aws-sdk-go-v2.
    `dynamodbc` and `aztablesc` are schemaless entity stores (schema inferred
    from sampled items, like `jsonc`) with a `table="*"` wildcard via
    `DatasetsFor` + `expand{Dynamo,Azure}Tables` in `cli.go`. `aztablesc`
    (Azure Table Storage) supports two auth paths — `connection_string`, or
    `account`/`endpoint` for Azure AD via `DefaultAzureCredential` — and
    translates predicates to an OData `$filter` (`translateOData`).
    **Predicate/Limit/OrderBy pushdown:** wired in the planner. For a
    single-table scan (no joins), `buildSelect` sets `Scan.Predicate` (the
    WHERE); `Scan.Limit` when safe (no ORDER BY / aggregate / OFFSET); and
    `Scan.OrderBy` when every ORDER BY term is a plain column (`columnOrderTerms`).
    `execScan` passes all three in the `ScanRequest`. It is an **optimization
    only** — the engine's `Filter`/`Sort`/`Limit` nodes are always kept above the
    Scan, so a connector that ignores or partially honors the hints stays correct
    (sqlc/aztablesc filter at the source; sqlc and azdevopsc also order at the
    source; file & most API connectors ignore them and the engine does the work).
    A connector must push `Limit` only when it fully applied the predicate (see
    sqlc's `predicateHandled`); the planner withdraws the limit for
    ORDER BY/aggregate/OFFSET (so a connector honoring an ORDER BY hint must not
    also truncate). `--explain` annotates pushed scans, e.g.
    `Scan inv [pushdown: predicate, limit=3, order]`. There is no `ScanResponse`,
    so the engine cannot drop the redundant re-filter/re-sort — a future refinement.

**`internal/cli`** wires it together. `cli.go` `NewApp()` registers all built-in
connectors and owns flag/config handling; `repl.go` is the interactive loop
(readline history, tab completion, dot-commands); `serve.go` (`--serve`) is the
web UI — an HTTP server exposing the same parse/plan/exec path as a JSON API.
Endpoints: `GET/POST /api/query`, `GET /api/sources` (list) / `POST /api/sources`
(register at runtime, the web `.use` — goes through `registerSourceExpand`, so
wildcards and validation match), `GET /api/schema`, and `POST /api/upload`
(multipart file → streamed to a per-session temp dir `App.uploadDir`, created in
`serve()` and `RemoveAll`'d on shutdown; the client then registers a file source
at the returned path). The web add-source UI is a modal whose form adapts per
connector via `connectorSpecs.ts` (file connectors get an upload). Field routing
for both `.use` and the web form is shared via `applySourceField` in `cli.go`.
Results are row-capped (`--max-rows`, default 5000) and it binds to localhost by
default. The frontend is a **React + Vite + TypeScript** app under
`internal/cli/webui/` (source) built to `webui/dist/` (committed), embedded via
`//go:embed all:webui/dist` and served with `http.FileServerFS`. `go build`
needs no Node; after editing `webui/src/**` run `npm run build` (or `go generate
./internal/cli`) and commit the updated `dist/`. Dev: `npm run dev` (HMR on
:5173, proxies `/api` to the Go server). See `webui/README.md`. **`internal/config`** loads
`turntable.yaml` with `${ENV_VAR}` / `${VAR:-default}` interpolation.

## Pushdown contract (the core invariant)

A connector receives a `ScanRequest` and may honor any subset of it. It reports
what it applied via `ScanResponse`; the engine computes the **residual**
(predicate/projection/sort not applied) and runs it in memory. This means a
connector implementing zero pushdown is still correct — capable connectors just
go faster. When adding or changing a connector, never silently drop a requested
predicate: either apply it and set the corresponding `Applied*` flag, or leave
it for the engine.

## Adding a connector

1. New package under `internal/connector/connectors/<name>c/`, implementing
   `connector.Connector`.
2. Register it in `cli.go` `NewApp()` via `reg.RegisterConnector(<name>c.New())`.
3. Its `Name()` becomes both the qualified-ref prefix (`<name>:./path`) and the
   `connector:` value in `turntable.yaml`.
4. Add a `*_test.go` with its own testdata; mirror existing connectors. For
   API connectors, inject the network client behind an interface so tests use a
   fake (see `cwlogsc`/`cwmetricsc`) or an `httptest` server (see `httpc`).
5. If it is a local-file connector, add its name to `isFileConnector` in
   `repl.go` — that list controls whether the `.use` command treats `path=` as
   a file path (file connectors) or as a pass-through connector option (URL/API
   connectors, where e.g. `path` is httpc's JSON pointer). Config secrets and
   `${ENV_VAR}` work in any string option (`config.go` interpolates the
   `Options` map), and the `url:` config/`.use` field flows to `Dataset.Source`.

## Wildcard sources

A `sql` source with `table: "*"` (config) or `.use db sql ... table=*` (REPL)
enumerates every user table in the database and registers each under its own
name. The `excel` connector does the same with `sheet: "*"` across worksheets.
This expansion happens in `cli.go` (`registerSourceExpand`).

## Conventions

- Tests are table-driven Go tests colocated with the code (`*_test.go`); each
  connector keeps its own fixtures/testdata.
- The `internal/` boundary is deliberate — nothing is a public API yet. DESIGN.md
  plans a future `pkg/` graduation for the connector interface, but don't create
  it speculatively.
- Exit codes: `0` = success, `1` = error (parse/plan/exec). DESIGN.md mentions
  a `2` = zero-rows code, but that is not implemented — success is always `0`.
