# Turntable — Design Document

## 1. Overview

Turntable is a command-line tool that lets you query **heterogeneous, independent
data sources** using a familiar **SQL-style query language**. Instead of writing
 bespoke scripts to parse JSON, slice CSVs, hit a database, or pull metrics from
an API, you write one SQL query and Turntable routes each referenced "table" to
the appropriate **connector**, retrieves the data, and executes the relational
operations (filter, project, join, aggregate, sort) in a unified in-memory
engine.

### Goals

- **One query language** across many data formats and sources.
- **Pluggable connectors** so the set of queryable sources is open-ended.
- **Cross-source joins** — join CSV against JSON against a Postgres table in a
  single query.
- **Zero-friction CLI** for ad-hoc data exploration (no long-running server).
- **Extensible** so community/third-party connectors (CloudWatch, Prometheus,
  REST APIs, etc.) can be added without modifying the core.

### Non-goals (v1)

- OLTP / transactional workloads.
- Replacing a full SQL engine. The SQL dialect is a pragmatic subset oriented
  toward read-only analytical queries over bounded datasets.
- Streaming/infinite sources (planned for later via cursor-based connectors).

---

## 2. Guiding Principles

1. **Everything is a table.** Every connector, regardless of backing format,
   exposes one or more named *datasets* as rows of typed columns. The query
   engine never needs to know whether a row came from a YAML file or a database.
2. **Push down when possible, fall back to in-memory.** Connectors may declare
   which predicates/projections they can evaluate natively (e.g. a SQL database
   connector pushes `WHERE`/`LIMIT` into the DB). Anything the connector can't
   handle is applied by the core engine in memory.
3. **Explicit schemas, inferred when absent.** Connectors either declare a
   schema up front or infer it from a sample. The engine works against a
   resolved schema so column references can be validated before execution.
4. **Bounded, predictable execution.** v1 targets datasets that fit in memory
   on the user's machine. This keeps the engine simple; performance work and
   spilling can come later.

---

## 3. High-Level Architecture

```
                ┌───────────────────────────────────────────────┐
   SQL text ──► │  Parser (lexer + AST)                          │
                └────────────────────┬──────────────────────────┘
                                     ▼
                ┌───────────────────────────────────────────────┐
                │  Planner / Validator                          │
                │  - resolves table refs → connectors            │
                │  - resolves/infers schemas                     │
                │  - validates columns, types                    │
                │  - decides pushdown per connector               │
                └────────────────────┬──────────────────────────┘
                                     ▼
                ┌───────────────────────────────────────────────┐
                │  Execution Engine (in-memory, vector-ish rows)│
                │  scan → filter → join → agg → proj → sort     │
                │     │ each stage may delegate to a connector    │
                └─────┴───────────────┬─────────────────────────┘
                                     ▼
                ┌───────────────────────────────────────────────┐
                │  Renderer (table / json / csv / ndjson)        │
                └───────────────────────────────────────────────┘

  Connectors (plugin-style, registered at startup):
  json | csv | yaml | excel | sql-db | (future) cloudwatch | prometheus | http | ...
```

### Component responsibilities

| Component        | Responsibility                                                        |
|------------------|-----------------------------------------------------------------------|
| **CLI**          | Arg parsing, config/profile loading, output format selection, REPL.   |
| **Parser**       | Tokenizes SQL text into an AST. Owns the supported SQL dialect.       |
| **Planner**      | Resolves identifiers to connectors+datasets, infers/merges schemas,  |
|                  | validates, computes a logical plan and per-connector pushdown.       |
| **Engine**        | Executes the logical plan over rows produced by connectors; applies  |
|                  | any operators not pushed down.                                         |
| **Connector**    | Knows how to enumerate datasets, resolve schema, and produce rows for  |
|                  | a given dataset, optionally honoring pushed-down predicates.         |
| **Renderer**     | Formats the final row set for the terminal or stdout.                |
| **Registry**     | Maps logical table names → connector instances. Drives discovery.    |

---

## 4. The SQL Dialect

A pragmatic, read-oriented subset of SQL. Familiar to anyone who knows ANSI SQL
but intentionally limited to keep the parser and planner tractable.

### Supported statement

```sql
SELECT  <select_list>
FROM    <table_ref>
[ JOIN  <table_ref> ON <expr> ]...
[ WHERE <expr> ]
[ GROUP BY <col>, ... [ HAVING <expr> ] ]
[ ORDER BY <expr> [ASC|DESC], ... ]
[ LIMIT <n> ] [ OFFSET <n> ]
```

- `select_list`: `*`, `col`, `expr AS alias`, aggregate functions
  (`COUNT`, `SUM`, `AVG`, `MIN`, `MAX`).
- `table_ref`: `name` or `name AS alias` or a subquery `( SELECT ... ) AS alias`.
  A `name` may be a connector-qualified path, e.g. `csv:./sales.csv` or
  `db:mydb.public.orders`.
- `JOIN`: inner and left; cross-source supported. Right/full outer deferred.
- `expr`: column refs, literals, arithmetic, string ops, comparisons, boolean
  ops (`AND/OR/NOT`), `IN`, `BETWEEN`, `LIKE`, `IS [NOT] NULL`, `CASE WHEN`,
  function calls (scalar functions provided by the engine, see §6).
- No DDL/DML. No `INSERT/UPDATE/DELETE`. Read-only by design.

### Identifier resolution

Unqualified `table` names are resolved via the **Registry** (see §5), which maps
them to a connector + dataset. Qualified forms let the user bypass the registry
and address a source directly:

```sql
SELECT * FROM json:./users.json            -- file connector shorthand
SELECT * FROM csv:./events.csv  AS e
SELECT * FROM excel:./report.xlsx sheet=Q1  -- excel sheet (v0.3)
JOIN  sql:postgres://.../orders o ON o.uid = e.user_id
```

---

## 5. Connector Model

A connector is an implementation of a small Go interface registered with the
core. The core depends only on this interface; connectors are isolated modules
under `internal/connectors/<name>`.

```go
// Connector is the extension point for any queryable source.
type Connector interface {
    // Name is the short prefix used in qualified table refs (e.g. "csv").
    Name() string

    // Datasets lists datasets this connector currently exposes. Many file
    // connectors expose exactly one (the file); DB connectors expose many.
    Datasets(ctx context.Context) ([]Dataset, error)

    // Resolve returns a typed schema for a dataset, possibly inferred.
    Resolve(ctx context.Context, ds Dataset) (Schema, error)

    // Scan produces rows for a dataset, honoring any pushed-down request.
    Scan(ctx context.Context, req ScanRequest) (RowIterator, error)
}

// ScanRequest carries what the engine would like the connector to handle
// natively. Anything left nil/unset means "connector need not bother".
type ScanRequest struct {
    Dataset     Dataset
    Columns     []string      // projection pushdown (nil = all)
    Predicate   Expr           // filter pushdown (nil = none); connector may
                              // partially honor and return the residual
    Limit       *int          // optional
    OrderBy     []OrderTerm   // optional; connector hints preferred order
}

// RowIterator streams typed rows. Connectors must support closing early.
type RowIterator interface {
    Next() (Row, bool, error) // returns (row, ok, err); ok=false at EOF
    Close() error
}
```

### Pushdown contract

- A connector returns a `ScanRequest` response describing **what it actually
  applied**. The engine records the *residual* predicate/projection/sort and
  applies them itself. This keeps connectors simple (they may implement zero
  pushdown and still be correct) while letting capable connectors (SQL DBs)
  push everything down for efficiency.
- File connectors (json/csv/yaml/excel) typically push down only `Columns` and `Limit`.

### Schema

```go
type Schema struct {
    Columns []Column
}
type Column struct {
    Name string
    Type Type        // int, float, string, bool, time, duration, bytes, any
    Nullable bool
}
```

- A `Type` of `any` means untyped/structured (nested objects/arrays). The engine
  permits dotted path access (`col.field`) and JSON-style indexing on `any`
  typed columns, resolved at planning time where possible.

### Registry & profiles

- At startup the CLI builds a **Registry** from a config file (`turntable.yaml`)
  and CLI flags. The config declares named sources, e.g.:

  ```yaml
  sources:
    users:
      connector: json
      path: ./data/users.json
    sales:
      connector: csv
      path: ./data/sales.csv
      delimiter: ","
    warehouse:
      connector: sql
      dsn: postgres://user@host:5432/db
      # schema discovery happens lazily
  ```

- A bare `FROM users` resolves via the registry; a qualified `FROM json:./x`
  creates an ephemeral, unregistered source for the query.

---

## 6. Query Engine

The engine consumes a validated logical plan and produces rows. Internally it
is a pull-based pipeline of operators, each consuming rows from its child:

```
Limit
 └─ Distinct
     └─ OrderBy
         └─ Project (select list + aliases)
             └─ Having
                 └─ Aggregate (group by)
                     └─ Filter (WHERE / residual predicates)
                         └─ Join
                             ├─ Scan(child A)
                             └─ Scan(child B)
```

- **Rows** are `[]Value` aligned to a known `Schema` at each stage. Values are
  strongly typed per `Type`; `any`-typed values hold nested structures.
- **Functions**: a small standard library of scalar functions (`COALESCE`,
  `LOWER/UPPER`, `SUBSTR`, `LEN`, `CAST`, `NOW`, `EXTRACT(part FROM ts)`,
  `COALESCE`, `JSON_EXTRACT(col, '$.path')`, regex `REGEXP_MATCH`, etc.) and the
  aggregates above. Extensible via a function registry.
- **Type coercion**: numeric widening and string<->time conversions follow
  predictable rules; ambiguous coercions are planner errors, not silent.
- **Joins**: hash join for equi-joins on in-memory sides; nested-loop fallback
  for non-equi joins. The planner picks build/probe sides by estimated row count
  when connectors provide cardinality hints.
- **Memory**: v1 materializes sides as needed; no spilling. A `--max-rows` /
  `--max-mem` guard prevents runaway scans; connectors and the engine both
  respect `LIMIT` early where possible.

---

## 7. CLI

```
turntable [flags] <query>
turntable -f query.sql
turntable --repl            # interactive mode with history + completion
```

Flags:

| Flag                     | Purpose                                                      |
|--------------------------|--------------------------------------------------------------|
| `-c, --config <path>`    | Profile/sources config (default `./turntable.yaml`).        |
| `-o, --output <fmt>`     | `table` (default), `csv`, `json`, `ndjson`, `yaml`, `raw`.   |
| `--header / --no-header` | Toggle header row for csv/table output.                      |
| `-s, --source <n=spec>` | Declare/override a source inline, e.g. `-s logs=yaml:./x`. |
| `--explain`              | Print the plan (pushdown per connector) instead of running. |
| `--strict`               | Fail on any unresolved column/type ambiguity.               |
| `-q, --quiet`           | Suppress timing/metadata, emit only results.                |
| `--repl`                 | Start interactive session.                                   |

Exit codes: `0` success with rows, `2` success zero rows, `1` error
(parse/plan/exec) with diagnostics on stderr.

### Examples

```bash
# Simple CSV query
turntable 'SELECT region, COUNT(*) AS n FROM sales WHERE amount > 100
            GROUP BY region ORDER BY n DESC LIMIT 10'

# Cross-source join: CSV orders + JSON users
turntable 'SELECT u.name, COUNT(o.id) AS orders
            FROM csv:./orders.csv o
            JOIN users u ON u.id = o.user_id
            GROUP BY u.name'

# Query a Postgres table, filter and project in-DB (pushdown)
turntable -c prod.yaml 'SELECT * FROM warehouse.public.events
            WHERE ts > NOW() - INTERVAL 1 DAY ORDER BY ts DESC LIMIT 50'
```

---

## 8. Project Layout

```
turntable/
├── DESIGN.md
├── README.md
├── go.mod
├── cmd/
│   └── turntable/
│       └── main.go              # CLI entrypoint, flag parsing, dispatch
├── internal/
│   ├── cli/                     # flag handling, repl, output wiring
│   ├── sql/
│   │   ├── lexer.go
│   │   ├── parser.go
│   │   ├── ast.go
│   │   └── parser_test.go
│   ├── plan/
│   │   ├── resolver.go           # table refs → connectors, schema resolve
│   │   ├── validate.go           # column/type validation
│   │   ├── pushdown.go           # per-connector pushdown negotiation
│   │   └── plan.go              # logical plan types
│   ├── engine/
│   │   ├── engine.go            # plan → row pipeline execution
│   │   ├── ops.go               # scan/filter/join/agg/project/sort/limit
│   │   ├── funcs.go             # scalar + aggregate function registry
│   │   └── types.go             # Value/Type/Schema/Row definitions
│   ├── connector/
│   │   ├── connector.go         # Connector/ScanRequest/RowIterator iface
│   │   ├── registry.go          # source registration + resolution
│   │   └── connectors/
│   │       ├── jsonc/           # json file connector
│   │       ├── csvc/            # csv connector
│   │       ├── yamlc/           # yaml connector
│   │       └── sqlc/            # SQL database connector (database/sql)
│   ├── render/
│   │   └── render.go            # table/csv/json/ndjson/yaml renderers
│   └── config/
│       └── config.go            # load/validate turntable.yaml
├── pkg/                         # (later) public, stable APIs for plugin authors
└── examples/
    └── turntable.yaml
```

The `internal/` boundary keeps the implementation details private in v1. Once
the connector interface and types stabilize, they graduate to `pkg/` so
out-of-tree connectors can be built without forking.

---

## 9. Extensibility: Future Connectors

The connector interface is the entire extension surface. Planned/later
connectors, all following the same contract:

- **CloudWatch Logs** — `connector: cloudwatchlogs`, dataset = log group; scan =
  `FilterLogEvents` with predicate pushdown mapped to filter patterns; emits
  parsed JSON rows where possible.
- **CloudWatch Metrics** — dataset = metric namespace; expose a synthetic schema
  of `(timestamp, metric, value, dims...)`; query-time ranges map to pushdown.
- **Prometheus** — instant/range vector queries wrapped as tables; `metric_name`
  as dataset, labels as columns.
- **HTTP/REST** — declarative mapping config: endpoint → dataset, response
  path → rows; pagination via cursor/offset config.
- **Parquet/Arrow** — columnar file sources; natural fit for larger datasets.
- **Git** — expose commits/diff stats/files as queryable tables.

Connectors may ship as separate Go modules and register via
`init()` + a plugin import in `cmd/turntable/main.go` (compile-time plugins),
with a runtime plugin mechanism (e.g. Yaegi or RPC) considered later.

---

## 10. Configuration

`turntable.yaml` (or via `--config`):

```yaml
sources:
  users:    { connector: json, path: ./data/users.json }
  sales:    { connector: csv,  path: ./data/sales.csv, delimiter: "," }
  warehouse:
    connector: sql
    driver: postgres
    dsn: postgres://user@host:5432/db?sslmode=require
  logs:
    connector: cloudwatchlogs
    region: us-east-1
    profile: prod
defaults:
  output: table
  max-rows: 1000000
```

- Credentials/DSNs may be sourced from env vars (`${ENV_VAR}` interpolation) to
  keep secrets out of the file.
- CLI `-s` flags and inline qualified refs override config sources.

---

## 11. Roadmap

**v0.1 — Core loop (MVP)**
- Lexer + parser for the supported dialect subset (SELECT/WHERE/JOIN/GROUP
  BY/ORDER BY/LIMIT, scalar funcs, basic aggregates).
- In-memory engine: scan, filter, project, inner/left join, group-by+agg,
  sort, limit.
- Connectors: `json`, `csv`, `yaml`.
- Renderers: `table`, `csv`, `json`, `ndjson`.
- Config file + CLI flags; `--explain`.

**v0.2 — SQL databases & pushdown** ✅
- `sql` connector over `database/sql` with predicate/limit/order pushdown.
- Schema discovery (`PRAGMA table_info`, `information_schema`, `DESCRIBE`);
  qualified `db.schema.table` refs.
- `${ENV_VAR}` interpolation in config DSNs.
- (Done in v0.3) `CASE WHEN`, richer string/time functions, `EXTRACT`.

**v0.3 — Ergonomics** ✅
- REPL with history, completion, `.tables` / `.schema <name>` introspection.
- Streaming/large-file handling (bounded memory via row-by-row render for
  csv/json/ndjson/yaml/raw; table buffers for column widths; `--max-rows`
  guard).
- `yaml` output; `--strict` mode (type-coercion failures become hard errors).
- `CASE WHEN` (searched & simple forms), `CAST(x AS type)`,
  `EXTRACT(field FROM src)`, `POSITION(sub IN str)`, FROM-less `SELECT <expr>`.
- Richer scalar functions: `LEFT/RIGHT`, `SPLIT_PART`, `REGEXP_REPLACE`,
  `REGEXP_MATCHES`, `REPEAT`, `REVERSE`, `INITCAP`, `LPAD/RPAD`,
  `DATE_TRUNC`, `DATE_ADD`, `AGE`, `TO_TIMESTAMP`, `DATE`, `STRFTIME`,
  `CURRENT_DATE`.

**v0.4+ — Ecosystem**
- CloudWatch logs/metrics, Prometheus, HTTP connectors.
- Public `pkg/` connector API + out-of-tree plugin story.
- Right/full outer joins, window functions, CTEs.
- Query/result caching; view definitions in config.

---

## 12. Testing Strategy

- **Parser**: golden files of SQL→AST; table-driven for error cases.
- **Engine**: fixture datasets (small JSON/CSV/YAML) + expected row outputs;
  property-style fuzzing of expression evaluation later.
- **Connectors**: each has its own testdata dir; integration tests use real
  files; the `sql` connector uses an in-memory SQLite for CI.
- **End-to-end**: CLI snapshot tests comparing rendered output for example
  queries in `examples/`.

---

## 13. Open Questions

1. **Plugin model** — compile-time imports vs runtime plugins vs RPC. Defer
   until the connector interface has been exercised by 2+ external authors.
2. **Nested data** — how rich should `any`-typed path access get? Start with
   dotted paths + array indexing; revisit if JSON-centric users need more.
3. **Large datasets** — exact threshold for "in memory" vs "streaming"; likely
   gated behind `--max-mem` and a streaming join strategy in v0.3.
4. **Dialect divergence** — how close to ANSI SQL to stay. Prefer staying
   predictable over clever; document deviations explicitly.