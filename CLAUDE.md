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
# in-process go-mysql-server + a SQL Server Docker container). Gated behind a
# build tag so the default suite stays pure-Go; the Postgres case downloads a
# server binary on first run. The SQL Server case needs Docker (no embeddable
# pure-Go server) and skips when the daemon is absent — set TURNTABLE_MSSQL_DSN
# to target an existing server instead, or TURNTABLE_MSSQL_IMAGE to pick the
# image. First run pulls a ~1.5 GB image, so use a generous -timeout.
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
   FROM-clause derived table, `TableFunc` for a FROM-clause set-returning
   function (`generate_series`, resolved at plan time to int/timestamp bounds via
   `buildTableFunc`; exec materializes it through `generateSeries`), and `SetOp`
   (a binary `Op`/`All`/`Left`/`Right` node) for `UNION`/`INTERSECT`/`EXCEPT`). `Build` takes a `sql.Statement` and
   dispatches on `*SelectStmt` vs `*SetOpStmt` (set operations) vs `*WithStmt`
   (CTEs); `buildSetOp` folds the flat branch list by precedence (INTERSECT
   tightest, UNION/EXCEPT left-associative) into binary `SetOp` nodes under the
   chain-level Sort/Limit — exec maps `UNION`→`ConcatIter`(+`DistinctIter`),
   `INTERSECT`→`IntersectIter`, `EXCEPT`→`ExceptIter` (`*All` keep multiset
   multiplicity). A `Subquery` passes its child plan's rows through
   under an alias, so `resolverFor`/`baseRelation` treat it like a Scan for
   column qualification. A column-alias list (`AS a(c1, c2, …)`,
   `TableRef.ColAliases`) reuses this: `buildTableRef` wraps the built source
   (via `buildTableRefRaw`) in a `Subquery` presenting the `renameColumns`'d
   schema — positional, so it works for base tables (which lose pushdown when
   renamed), derived tables, and table functions alike. `buildWith` registers each CTE in `buildCtx.ctes`, then
   `buildTableRef` resolves a bare name to a CTE (shadowing a registered source)
   before the registry. The CTE's plan is built **once** on its first reference
   (a `visiting` guard rejects recursion during that build) and shared via a
   `*cteMaterialization` (`cteEntry.mat`); each reference is a `Subquery` wrapping
   a `CTERef` that points at that shared materialization. At exec, the first
   `CTERef` pulled runs the CTE's plan to completion and buffers its rows
   (`cteMaterialization.ensure`); every reference replays the buffer via its own
   `cteReplayIter` cursor — so a multiply-referenced CTE executes (and hits its
   sources) once, and all references see a consistent snapshot. `--explain` tags
   the node `CTE <name> [materialized]`. **Regular views** (`CREATE VIEW`) reuse
   this exact machinery: their defining query is stored in the registry
   (`Registry.RegisterView`/`View`), and `buildTableRef` resolves a bare name to a
   view (after CTEs, before sources) via `buildView`, which expands it like a CTE
   — planned once per query (cached in `buildCtx.viewMat`), wrapped in
   `Subquery`+`CTERef{IsView:true}`. So a view re-runs on every query (always
   current) but materializes once *within* a query (an externally-visible CTE).
   A view body binds in the global scope, so `buildView` hides the referencing
   query's CTEs (`bc.ctes`) while planning it; recursion is rejected. Window functions (`f(...) OVER (...)`,
   `FuncCall.Over` set) get a `Window` node between the post-WHERE rows and the
   projection: `buildWindow` lifts each window call into an appended `$winN`
   column (mirroring the aggregate `$aggN` extraction via the shared
   `rewriteFuncs`), and the projection/ORDER BY reference those columns. The
   `WindowIter` materializes, partitions, orders, and computes per spec
   (ROW_NUMBER/RANK/DENSE_RANK/LAG/LEAD and aggregate windows). Aggregate windows
   support an explicit `ROWS`/`RANGE BETWEEN … AND …` frame (`WindowSpec.Frame`,
   parsed into `sql.WindowFrame`/`FrameBound`): ROWS uses physical offsets
   (`frameBoundIndex`) for moving averages; RANGE is value-based
   (`computeWindowRange` — peers share a frame, offsets are value windows;
   single ORDER BY, numeric offset, or a timestamp column with an `INTERVAL`
   offset for rolling time windows — `INTERVAL '…'` is an `sql.IntervalLit`
   evaluating to a `TypeDuration`, and `Arith` does time±duration / time−time). GROUPS errors; the default frame
   (whole partition, or running when ORDER BY'd) applies when none is given.
   Distribution window fns NTILE/PERCENT_RANK/CUME_DIST and the two-column stats
   CORR/COVAR_*/REGR_* (paired via AggSpec.Arg2, `computeRegr`) also live here.
   Time-series helpers: FIRST/LAST(value, ord) are two-arg aggregates
   (`computeFirstLast` — value at min/max ord, e.g. latest reading per station;
   also usable as window aggregates); LOCF(x) is a window function carrying
   the last non-NULL x forward — the gap-filling companion to a generate_series
   LEFT JOIN (recipe in DIALECT.md); DELTA(x) / RATE(counter, ts) are window
   functions for consecutive-row difference and per-second rate (RATE treats a
   counter drop as a reset: increase = new value). DATE_BIN's origin arg is
   optional (defaults to the Unix epoch, time_bucket-style).
   Window + GROUP BY in one query is rejected for now.
   A non-correlated `WHERE/HAVING x IN (SELECT ...)` is handled differently:
   `resolveInSubqueries` executes the subquery at build time and folds its one
   column into a literal `InExpr.List` (`valueToLiteral`), so the engine needs no
   subquery support and the folded list is even pushdown-eligible. (Build-time
   execution means `--explain` runs IN subqueries.) Other subqueries — `EXISTS`,
   scalar `(SELECT ...)`, and correlated `IN` — are lifted by `buildSubqueries`
   into an `Apply` node (one `$subN` column per subquery, like the aggregate/
   window `$aggN`/`$winN` extraction): the inner plan is built once with
   correlated qualified columns rewritten to `sql.OuterRef` (`rewriteCorrelated`,
   resolved against the outer scope). `execApply` streams the outer rows and, per
   row, re-executes each inner plan with the outer row bound on the context
   (`withOuter`/`outerFromCtx`); the engine's `Evaluator.Outer` resolves
   `OuterRef`. Non-correlated subqueries are memoized (run once). It is correct
   but `O(outer rows)`. The `Apply` sits below the WHERE `Filter` (and so below
   any later Aggregate/Window stage), so a subquery in **WHERE** composes with
   `GROUP BY`/aggregates/window — the `$subN` columns are consumed by the filter
   and ignored by the aggregate. Only subqueries in the **SELECT list / ORDER BY**
   (post-aggregation) are rejected when grouping/windowing — `buildSelect`
   distinguishes `whereHasSubquery` from `projHasSubquery`. Correlation is
   single-level. As an optimization,
   `decorrelateExists` (run before subquery detection) turns a top-level
   correlated `[NOT] EXISTS` over a single table with one equality correlation
   into a semi/anti `HashJoinIter` (`sql.JoinSemi`/`JoinAnti` — planner-only join
   kinds that emit each left row at most once, NULL keys never matching) — one
   hash pass instead of `O(rows)`, and it composes with GROUP BY since no
   subquery is left behind. `exec.go` lowers the tree into
   an `engine.RowIterator` pipeline (e.g. a `SetOp` → the matching
   concat/intersect/except iterator).
3. **`internal/engine`** — pull-based operator pipeline over `[]Value` rows
   aligned to a `Schema`. `ops.go` = operators, `eval.go` = expression
   evaluation, `funcs.go` = the scalar + aggregate **function registry**
   (`FuncRegistry`), `value.go`/`types.go` = the `Value`/`Type`/`Schema`/`Row`
   model. Joins: in-memory `HashJoinIter` supporting INNER/LEFT/RIGHT/FULL (plus
   planner-only SEMI/ANTI for decorrelated EXISTS). The planner's `splitJoin`
   divides `ON` into `a.x = b.y` equi-conjuncts (each a hash-key pair; multiple
   AND'd equalities → a **composite** bucket key) and a **residual** `sql.Expr`
   (everything else — non-equality, expression/constant equality, single-side
   conditions); the iterator hashes on the composite key and re-checks the
   residual per candidate pair via an `Evaluator` over the combined schema. With
   no equi-conjunct the key list is empty → all left rows share one bucket and it
   degenerates to a nested-loop join (`O(left × right)`). The left side is the
   build side, the right is streamed, and the unmatched side of an outer join is
   NULL-padded (a left row whose every keyed partner fails the residual still
   appears NULL-padded). `--explain` tags joins with `[N keys]`/`[residual]`/
   `[nested loop]`. **ASOF [LEFT] JOIN** (`sql.Join.Asof`, parsed as a
   non-reserved word via `startsAsofJoin` lookahead so identifiers named asof
   keep working): each left row matches at most the single nearest right row per
   the ON's one inequality, grouped by its equality conjuncts —
   `splitAsofJoin` normalizes to `engine.AsofSpec` (no residuals allowed) and
   `AsofJoinIter` materializes/groups/sorts the right side and binary-searches
   per left row; explain tag `ASOF … [on >=, N group key(s)]`.
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
  prefixes (`csv`, `sql`, …) → `Connector` instances. `RemoveSource` unregisters
  a name (used by `DROP MATERIALIZED VIEW`).
- `connectors/memc` is the odd one out: an in-memory `Schema`+`[]Row` store
  (`Put`/`Drop`/`Has`, `Populated` flag) that backs materialized views (see
  `internal/cli/matview.go`) rather than locating external data. It applies no
  pushdown.
- `connectors/<name>/` are the implementations, in four families:
  - **Plugin** (`pluginc`): not a data source itself but a generic proxy that
    runs an **external program** as a connector, forwarding each interface method
    (Datasets/Resolve/Scan) over stdio JSON-RPC 2.0 (Content-Length framing,
    method set initialize/datasets/resolve/scan/next/close/shutdown; `scan` opens
    a cursor that `next` drains in batches). Rows are positional JSON arrays
    decoded against the wire schema (`cell.go`); a `WHERE` is rendered to a narrow
    JSON predicate subset (`predicate.go`, best-effort — the engine always
    re-applies, so partial pushdown is safe). One `pluginc.Connector` owns one
    long-lived subprocess; `cli.go` `registerPluginSource` starts it (handshake +
    wildcard `dataset:"*"` expansion), registers the advertised name as a
    qualified-ref prefix, and `App.Close` (deferred in `Run`) tears the processes
    down. Config: `connector: plugin` + a `command:` list (`config.Source.Command`,
    `${ENV}`-interpolated; arbitrary exec, so deliberately *not* exposed via the
    web add-source UI). Protocol is specified in **PLUGINS.md**. Tests:
    pipe-based codec/iterator (`client_test.go`) + an exec-self real-subprocess
    e2e (`pluginc_test.go`, via `TestMain`). A **Go SDK** for plugin *authors*
    lives in a separate, dependency-free module `sdk/go` (package `ttplugin`,
    import `github.com/april/turntable/sdk/go/ttplugin`) — it mirrors this
    connector across the wire, implementing framing/dispatch/cursor/predicate-eval/
    cell-encoding so an author writes only `Plugin{Name, Datasets}` (each
    `Dataset` = a `Schema` + a `Rows` func); it auto-applies the pushed
    `WHERE`/`LIMIT` unless `ManualPushdown`. The reference plugins under
    `examples/plugins/` (`sysinfo` = env + Go-runtime stats, dependency-free;
    `procinfo` = the live process table via gopsutil; `k8s` = Kubernetes
    resources via client-go — flattened datasets for pods/deployments/
    statefulsets/daemonsets/nodes/services/namespaces/events, plus a generic
    `resource` dataset for any kind/CRD; auth via kubeconfig incl. AKS/EKS exec
    plugins; `context`/`kubeconfig`/`namespace` options) are each their **own
    module** using the SDK (kept separate so gopsutil/client-go never enter
    turntable's or the SDK's dep graph; they `replace`→`../../../sdk/go`
    locally). Build them with `examples/plugins/build.sh` — being separate
    modules, they are **not** compiled by `go build ./...` from the repo root.
  - **File** (`jsonc`, `csvc`, `yamlc`, `excelc`, `parquetc`): locate data by a
    local path; infer schema from a sample/footer; push down only columns/limit.
    `logc` is a plain-text log reader that **auto-detects** the format by
    sampling the first ~200 lines and picking the first of json / Apache-combined
    / common (CLF) / syslog / bracketed (`[time] [component] message`, e.g.
    pacman/ALPM) / logfmt / leveled whose parse ratio clears a threshold (else a
    `raw` line view). Each format yields a typed schema
    (status/bytes/pid→int, time→time; json/logfmt get a dynamic key-union schema).
    A `format` option forces one; a `pattern` option (a regexp with `(?P<name>…)`
    groups → columns) overrides detection. Line-oriented (no multi-line joining).
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
    Four drivers are compiled in (blank imports in `sqlc.go`): `sqlite`
    (`modernc.org/sqlite`, pure Go), `postgres` (`lib/pq`), `mysql`
    (`go-sql-driver/mysql`), `sqlserver` (`microsoft/go-mssqldb`). A `dialect`
    (in `sqlc.go`, keyed by driver name) abstracts the per-engine differences:
    identifier quoting (`quoteIdent` — backticks MySQL, `[brackets]` SQL Server,
    double quotes otherwise), bind placeholders (`placeholder` — `$N` Postgres,
    `@pN` SQL Server, `?` otherwise), and row-limit syntax (`usesTop` — SQL
    Server prepends `SELECT TOP (n)`, others append `LIMIT n`). `pushesLike`
    withholds LIKE/ILIKE pushdown for SQL Server (collation-dependent
    case-sensitivity could drop rows the engine must see). `buildScanQuery` is
    the pure (DB-free, unit-tested) query renderer. Thread the dialect through
    any new query-building code; do **not** hardcode quoting/placeholders/limits.
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
    aws-sdk-go-v2. `azmetricsc` is the Azure twin of `cwmetricsc`: option-driven
    Azure Monitor metrics for one resource (`resource`/`metric`/`aggregation`/
    `interval`/`timespan`/`dimension`), a fixed schema + one column per split
    dimension, Azure AD auth via `DefaultAzureCredential`. It wraps `armmonitor`
    behind a narrow `metricsAPI` returning normalized series (`client.go` adapts
    the SDK; the connector logic is SDK-free and fake-tested). No aggregate
    pushdown — the Azure API is pre-aggregated by `aggregation`+`interval`, so the
    engine does any further rollup. Two modes: per-resource (`resource`, via
    `armmonitor`) and **batch** (`resources` list + `region`, via `azmetrics`'
    data-plane `QueryResources` — many resources/call, chunked at 50, each row
    tagged by its own `resource`; `realClient` lazily builds+caches an
    armmonitor client per subscription and an azmetrics client per region).
    See `docs/azure-monitor-design.md`.
    `azrgraphc` (Azure Resource Graph) is fleet inventory across subscriptions via
    one KQL endpoint (`armresourcegraph`). Like `athenac`, it pushes WHERE/ORDER
    BY/LIMIT down — as KQL, via the shared **`azkql`** renderer (a pure, DB-free,
    unit-tested package — the KQL analogue of `sqlc`'s `buildScanQuery`, meant to
    be reused by a future Log Analytics connector). A `query` option carries raw
    KQL (no pushdown). Schemaless: schema inferred from a sample like `dynamodbc`
    (`Scan` calls `Resolve`; keys sorted for determinism; nested `tags`/
    `properties` stay `TypeAny`). Table via `table` option or the ref Source
    (`azrgraph:Resources`, default `Resources`); `subscriptions`/`top` options;
    paginates via the response `SkipToken`. Azure AD auth (`DefaultAzureCredential`,
    Reader). `azkql.Build` translates only push-safe predicate parts (top-level
    columns; `LIKE`→case-insensitive `contains`, a safe superset) and leaves the
    rest to the engine — pushing is always an optimization.
    `azlogsc` (Azure Monitor Logs / Log Analytics, the Azure twin of `cwlogsc`)
    also reuses `azkql`: it queries a workspace (`workspace` option) by `table`
    (or ref Source, `azlogs:AppRequests`) with WHERE/ORDER BY/LIMIT pushed as KQL,
    or a raw `query`. Log Analytics returns **typed columns** with the rows, so
    the schema is exact (no inference; `columnType` maps datetime/int/long/real/
    dynamic/… to engine types) and there is no pagination (one query, bounded by
    the KQL take cap + the `timespan`, default P1D). Wraps `azlogs` behind a
    narrow `logsAPI`; Azure AD auth (Log Analytics Reader).
    `dynamodbc` and `aztablesc` are schemaless entity stores (schema inferred
    from sampled items, like `jsonc`) with a `table="*"` wildcard via
    `DatasetsFor` + `expand{Dynamo,Azure}Tables` in `cli.go`. `aztablesc`
    (Azure Table Storage) supports two auth paths — `connection_string`, or
    `account`/`endpoint` for Azure AD via `DefaultAzureCredential` — and
    translates predicates to an OData `$filter` (`translateOData`).
    `honeycombc` (Honeycomb.io observability) exposes four datasets selected by a
    `kind` option (falling back to the ref Source, else `events`): three metadata
    tables flattened into rows like `trelloc` — `datasets` (v1 `/1/datasets`),
    `columns` (`/1/columns/{slug}`, needs `dataset=<slug>`), and `environments`
    (v2 Management API, JSON:API-shaped, needs `management_key`+`team`) — plus a
    per-dataset **`events`** table that is the connector's reason for the
    aggregate-pushdown machinery below. Honeycomb has **no raw-event read API**
    (every query is an aggregation over a time window), so `events` cannot return
    raw rows: it implements `connector.AggregatePusher` and a plain (non-aggregate)
    scan is an error. `PushAggregate` maps the planner's group-by→`breakdowns`,
    aggregates→`calculations` (COUNT/COUNT_DISTINCT/SUM/AVG/MIN/MAX/MEDIAN→P50),
    and WHERE→`filters` (`translateFilters`), returning the aggregated schema;
    `Scan` runs the async Query Data API (create query → create query_result →
    poll `complete`) and maps `data.results[].data` (keyed by breakdown name +
    `OP(column)`) back to rows. Auth: `api_key`→`X-Honeycomb-Team` (v1),
    `management_key`→Bearer (v2); both are `config.IsSensitive` (must be `${ENV}`)
    and fall back to `HONEYCOMB_API_KEY`/`HONEYCOMB_MANAGEMENT_KEY`. Dotted
    attribute names (`service.name`) — which SQL lexes as qualifier+name — resolve
    via a fallback in `engine.SchemaResolver`/`exprType` that matches a column
    literally named `<qualifier>.<name>`. `dataset="*"` expands to one events
    source per Honeycomb dataset via `DatasetsFor`+`expandHoneycombDatasets`.
    Time window from `time_range` (default 7200s) or `start_time`/`end_time`.
    Plan note: the events **Query Data API** is gated to paid Honeycomb plans, so
    on a free plan the query POST returns 403 (`enterpriseHint` wraps it with an
    explanation); the metadata datasets work on any plan.
    `awsconfigc` (AWS Config Advanced Query) is the AWS analogue of `azrgraphc`:
    account/region resource inventory (every type Config records) via Config's
    SQL `SELECT` surface. Table mode exposes a **fixed** top-level schema
    (`resourceId`/`resourceType`/`awsRegion`/`tags`/`configuration`/… — no
    inference, since Config's top-level shape is documented) and pushes WHERE
    (`=`/`IN`/`LIKE` on scalar top-level columns, `translate`/`buildWhere`) + LIMIT
    down as a Config `SELECT`; a raw `query` option carries a full Config `SELECT`
    (schema then inferred from a sample). `SelectResourceConfig`, or
    `SelectAggregateResourceConfig` when an `aggregator` option is set (multi-
    account); paginates via `NextToken`; results are JSON strings parsed to rows.
    Wraps aws-sdk-go-v2 `configservice` behind a narrow `configAPI` (fake-tested).
    Needs AWS Config enabled/recording. `region`/`profile`/`aggregator`/`top`
    options. See `examples/aws-config.md`.
    `awscostc` (AWS Cost Explorer) and `azcostc` (Azure Cost Management) are the
    cost twins — both option-driven like the metrics connectors (the APIs are
    pre-aggregated by metric+granularity+group-by, so no SQL-aggregate pushdown).
    `awscostc` (`GetCostAndUsage`, paginated) options `granularity`/`metric(s)`/
    `group_by` (`TYPE:KEY`, ≤2)/`start`/`end`; schema = `period_start`,
    `period_end`, a column per group-by, a column per metric, `currency`. `azcostc`
    (Cost Management Query API, `DefaultAzureCredential`) options `subscription`/
    `scope`/`metric`/`group_by`/`granularity`/`timeframe`/`start`/`end`; returns
    typed columns so the schema is exact (like `azlogsc`). Both wrap their SDK
    behind a narrow interface (fake-tested). See `examples/cost.md`.
    `athenac` is the odd one in this family: Athena *is* a SQL engine
    (Presto/Trino over S3, Glue catalog), so it pushes projection/predicate
    (`translateExpr`, Presto-flavored — double-quote idents, no ILIKE)/ORDER BY/
    LIMIT down as SQL (`buildQuery`) rather than filtering in memory. Schema is
    free via the Glue catalog (`GetTableMetadata`/`ListTableMetadata`, no query);
    only `Scan` runs a billed query — async: `StartQueryExecution` → poll
    `GetQueryExecution` → page `GetQueryResults` (whose first page carries a
    header row the iterator skips; cells are strings typed by the schema). Uses
    aws-sdk-go-**v2** like the other AWS connectors (not a `database/sql` driver,
    deliberately — that avoids dragging in aws-sdk-go v1). `table="*"` expands
    every table in the Athena `database` via `expandAthenaTables`. Needs an
    `output_location` (S3) unless the `workgroup` sets one.
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
    **Aggregate pushdown** (opt-in — `honeycombc`, which cannot return raw rows,
    and `sqlc`, as an optimization): a connector implementing
    `connector.AggregatePusher`
    can compute a whole `GROUP BY`/aggregate query at the source. When a
    single-scan aggregate query's connector is a pusher, `buildSelect` calls
    `buildPushedAggregate`, which extracts group-by→`AggregateRequest.GroupBy`
    (`[]AggregateGroup` — a plain column, or a **`DATE_BIN(stride, ts)` time
    bucket** via `bucketGroupExpr`: 2-arg form only, constant stride; the bucket
    expression's occurrences in SELECT/HAVING/ORDER BY are rewritten to its
    output column, and the request declines if any leftover expression still
    references a raw column),
    aggregates→`AggregateOp`s (via `pushAggExtractor`, the pushdown analogue of
    `aggExtractor` — plain-column args only, `COUNT(*)`/`COUNT(DISTINCT)`
    supported, scalar wrappers like `ROUND(AVG(x),2)` left for the engine's
    projection), and the WHERE→`AggregateRequest.Predicate`, then calls
    `PushAggregate(ctx, ds, req)`. `sqlc`'s implementation (`buildAggQuery`, a
    pure renderer like `buildScanQuery`) pushes COUNT/SUM/AVG/MIN/MAX + plain or
    bucketed group-bys + an *exactly*-translatable WHERE (a superset predicate
    like sqlite/mysql case-insensitive LIKE declines — no raw rows remain to
    refine); buckets render as epoch-floor arithmetic per dialect
    (`dialect.bucketExpr`; SQL Server declines, DATEDIFF int-overflow risk). On `ok=true` the `Scan` gains `.Aggregate` (its
    schema becomes the aggregated rows), and the engine emits **no** `Aggregate`
    and **no** WHERE-`Filter` for that scan — HAVING (`Filter`), ORDER BY (`Sort`)
    and the projection run above it over the aggregated rows. This is
    all-or-nothing: accepting the request means the connector fully applies
    grouping + aggregates + predicate. `ok=false` declines (the engine aggregates
    the connector's raw rows as usual); a non-nil error aborts planning (Honeycomb
    returns one for an unsupported op/filter since it has no raw-row fallback).

**`internal/cli`** wires it together. `cli.go` `NewApp()` registers all built-in
connectors and owns flag/config handling; `repl.go` is the interactive loop
(readline history, tab completion, dot-commands). **Views** are session
statements parsed in `internal/sql` (view keywords matched as non-reserved words
so columns named `view`/`data` still parse) and dispatched in `runQueryInto` /
the web `handleQuery` *before* `plan.Build` — not row-producing queries; they
return a `notice`. Two flavors:
 - **Materialized views** (`matview.go`): `CREATE/REFRESH/DROP MATERIALIZED VIEW`
   (`*CreateMatViewStmt`/`*RefreshMatViewStmt`/`*DropMatViewStmt`). `createMatView`
   plans (and, unless `WITH NO DATA`, executes) the query and buffers the rows in
   the `memc` connector (`App.mem`, an in-memory `Schema`+`[]Row` store under the
   `mem` prefix), registers the view as a source, and records the query in
   `App.matViews` so `REFRESH` can re-run it. The stored schema is normalized to
   unqualified column names (`viewSchema`/`unqualifyName`; duplicates rejected,
   like PostgreSQL).
 - **Regular views** (`view.go`): `CREATE [OR REPLACE] VIEW` / `DROP VIEW`
   (`*CreateViewStmt`/`*DropViewStmt`). `createViewCore` validates by planning,
   then stores the query in the registry (`RegisterView`); the planner expands
   references inline (see `buildView`) so the view re-runs per query but
   materializes once within one. `.tables` / `/api/sources` list both kinds (a
   regular view tagged `view`); `.schema` / `/api/schema` resolve a view's columns
   via `viewSchemaFor` (planning the query, since a view has no connector).
The web path shares the `*Core` methods via `sessionStmtResponse` in `serve.go`
(returns a `queryResponse.Notice`; the frontend shows a banner and refreshes the
source list). `serve.go` (`--serve`) is the
web UI — an HTTP server exposing the same parse/plan/exec path as a JSON API.
Endpoints: `GET/POST /api/query`, `GET /api/sources` (list) / `POST /api/sources`
(register at runtime, the web `.use` — goes through `registerSourceExpand`, so
wildcards and validation match), `GET /api/schema`, `GET /api/functions` (the
dialect's scalar/aggregate/keyword lists — same data as REPL `.functions` —
feeding editor autocomplete), `POST /api/loginfer` (analyze a log file path:
returns the recognized `logc` format + a parsed preview, or — for an
unrecognized file — inferred templates from `internal/loginfer`, a Drain-style
miner, each carrying a ready-to-use `pattern` regex; the add-source modal
auto-runs this when a log file is chosen and lets the user pick a template and
rename its columns, which rewrites the pattern), `GET/POST /api/dashboards` +
`GET/DELETE /api/dashboards/{slug}` (**dashboards/stories** — named panel lists
of markdown/table/pivot/chart/stat, one YAML file each under
`.turntable/dashboards/` (git-committable, NOT ignored — only `.turntable/data`
is), CRUD in `dashboard.go`; the server only stores definitions — the client
runs each panel's query through `/api/query`, substituting `{{var}}` toolbar
variables as quoted literals (`{{var:raw}}` raw); a panel's `view` is the
frontend `ViewConfig` (see below), and the results pane's **Pin** button
appends the current query+view as a panel — see `docs/dashboards-design.md`;
frontend: `DashboardView.tsx`, `PinModal.tsx`, `Markdown.tsx` a minimal
React-element markdown renderer, no innerHTML), and `POST /api/upload`
(multipart file → streamed to the persistent, project-relative `App.uploadDir` =
`.turntable/data` (gitignored), created in `serve()`; kept across restarts so a
file source saved to the config keeps resolving. Stored under the original
sanitized name, `-N`-suffixed on clash via `createUpload`; the client then
registers a file source at the returned relative path). The web add-source UI is a modal whose form adapts per
connector via `connectorSpecs.ts` (file connectors get an upload). Field routing
for both `.use` and the web form is shared via `applySourceField` in `cli.go`.
Results are row-capped (`--max-rows`, default 5000) and it binds to localhost by
default. The UI is **tabbed** (`TabBar.tsx` — each tab an independent query
workspace with its own editor text + result, persisted to localStorage by
`storage.ts`; the results-pane view config — table/chart/pivot mode plus each
view's settings — persists too, as a `ViewConfig` (`view.ts`) whose column refs
are **by name** so it survives schema drift, and which doubles as the panel
format for future dashboards — see `docs/dashboards-design.md`); each tab has a
CodeMirror editor (SQL highlighting + source/
column/function autocomplete), query history + saved queries (also localStorage),
and a results pane with three views: a table (client-side sort/filter, cell
copy/JSON-expand, CSV/JSON/NDJSON export via `export.ts`), a Chart.js chart
(`Chart.tsx`: bar/line/area/scatter/bubble/heatmap/pie plus node-link
graph/tree with PNG export, X column + multi-series Y toggles + a series-by
breakdown + client-side group-by-X aggregation count/sum/avg/min/max; a
**time-typed X** on line/area gets a real time axis — points become (epoch ms,
y) via `chartjs-adapter-date-fns`, uneven sampling/gaps render truthfully, and
LTTB decimation plots ALL rows instead of the 500-row raw cap; scatter/bubble
accept a time X too; chart extras: a Y chip cycles left axis → **right axis**
(dual-axis `y2`, mixed-unit series) → off, a **threshold** input draws
horizontal reference lines via `chartjs-plugin-annotation`, and a **band**
lo/hi column pair renders a translucent envelope fill (e.g. P10–P90) behind
line/area series — all persisted in the ViewConfig so they carry into
dashboard panels), and a
pivot table (`PivotTable.tsx` — row×column cross-tab of one measure, optional
cell heatmap colouring). The chart heatmap uses the `chartjs-chart-matrix`
plugin; the **graph/tree** types use `chartjs-chart-graph` (force-directed +
hierarchical layouts) + `chartjs-plugin-datalabels` for node labels — they map an
edge-list/parent-pointer result (a *node* column + a *parent/links-to* column,
e.g. `pid`/`ppid`) to nodes+edges via `nodesEdges` in `pivot.ts`, with a
synthetic root joining a forest into one tree and a node cap. Nodes can be
coloured by a column (categorical palette / numeric gradient, with a legend) and
sized by a measure. The graph is interactive: scroll/drag to zoom+pan
(`chartjs-plugin-zoom`, + a "reset view" button) and **click a node to drill
into its subtree** (`nodesEdges`' `focus` arg keeps only the focus node and its
forward-reachable descendants, lifting the cap; a breadcrumb clears it; the
focus auto-resets when the data or node/parent columns change). The whole graph
chart lives in a `React.memo` child (`GraphChart`) keyed by a per-render counter:
it builds chart data/options *fresh* each render (the force layout needs
react-chartjs-2's per-render update to run, and reused option objects corrupt the
controller) yet only re-renders on real input changes, so unrelated re-renders
(typing in the editor) never disturb or blank it. NB:
`chartjs-chart-graph` 4.3.5 needs chart.js pinned to ~4.4 (it breaks on 4.5's
option-sharing — see the note in `Chart.tsx`). The shared aggregation/pivot
primitives live in `pivot.ts` (used by both the chart and the pivot table).
The frontend is a **React + Vite + TypeScript** app under
`internal/cli/webui/` (source) built to `webui/dist/` (committed), embedded via
`//go:embed all:webui/dist` and served with `http.FileServerFS`. `go build`
needs no Node; after editing `webui/src/**` run `npm run build` (or `go generate
./internal/cli`) and commit the updated `dist/`. Dev: `npm run dev` (HMR on
:5173, proxies `/api` to the Go server). See `webui/README.md`. **`internal/config`** loads
`turntable.yaml` with `${ENV_VAR}` / `${VAR:-default}` interpolation (`Parse` →
`InterpolateSource`), and also reads a `.env` at startup (`LoadDotEnv`; real env
wins) so those refs resolve without manual exports.

**Secrets / runtime sources.** Sensitive connector fields (`config.IsSensitive`:
sql `dsn` except sqlite; http `bearer`; linear `api_key`/`bearer`; trello
`key`/`token`; azuredevops `pat`; azuretables `connection_string`) must be a sole
`${ENV_VAR}` reference, not a literal — enforced by `ValidateSourceSecrets` so
credentials never reach the config file. Runtime adds (`.use`, web
`/api/sources`) go through `App.registerRuntimeSource`: validate the *declared*
form, then `InterpolateSource` resolves the refs for the connector while the
`${VAR}` form is what's validated/persisted (note: pre-existing config sources
are interpolated at load and not re-validated). Optional persistence appends the
declared (secret-free) source to the config via `config.AppendSource` (a
`yaml.Node` round-trip preserving comments/formatting, block style, 2-space
indent; creates the file / replaces a same-named entry) — opt in with `.use …
save` or web `save:true`. `App.configPath` is the target (`-c`, else
`./turntable.yaml`). The web add-source modal marks sensitive fields with an
"env ref" badge + client-side `${VAR}` validation and a "Save to config file"
checkbox.

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
