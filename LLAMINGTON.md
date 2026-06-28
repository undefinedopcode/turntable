## About project Turntable

- Turntable is a command-line tool that queries **heterogeneous, independent data sources** (JSON, CSV, YAML, Excel, SQL databases) using a single, familiar **SQL-style query language**. Instead of bespoke scripts to parse files or hit databases, you write one SQL query and Turntable routes each referenced "table" to the appropriate connector, retrieves the data, and runs the relational operations (filter, project, join, aggregate, sort, limit) in a unified in-memory engine.
- Features identified:
  - Pragmatic read-only SQL dialect: `SELECT`/`FROM`/`JOIN`/`WHERE`/`GROUP BY`/`HAVING`/`ORDER BY`/`LIMIT`/`OFFSET`, aggregates (`COUNT`, `SUM`, `AVG`, `MIN`, `MAX`), `DISTINCT`, `LIKE`, `IN`, `BETWEEN`, `IS NULL`.
  - Expression layer (v0.3): `CASE WHEN` (searched & simple), `CAST(x AS type)`, `EXTRACT(field FROM src)`, `POSITION(sub IN str)`, FROM-less `SELECT <expr>`.
  - Rich scalar/aggregate function library: string (`LEFT`, `RIGHT`, `POSITION`, `SPLIT_PART`, `REGEXP_REPLACE`, `REGEXP_MATCHES`, `REPEAT`, `REVERSE`, `INITCAP`, `LPAD`/`RPAD`, `LOWER`/`UPPER`, `LENGTH`, `SUBSTR`, `TRIM` family, `CONCAT`, `REPLACE`), numeric (`ABS`, `ROUND`, `FLOOR`, `CEIL`), time (`EXTRACT`, `DATE_TRUNC`, `DATE_ADD`, `AGE`, `TO_TIMESTAMP`, `DATE`, `STRFTIME`, `CURRENT_DATE`, `NOW`), and `COALESCE`.
  - Pluggable connector model with a `Connector` interface + `Registry`: `json`, `csv`, `yaml`, `excel` (`.xlsx` via excelize), and `sql` (`database/sql`, SQLite/Postgres/MySQL).
  - **Predicate / limit / order pushdown** into SQL databases; anything a connector can't handle is applied in-memory by the engine.
  - **Cross-source joins** — join CSV against JSON against a Postgres/SQLite table in one query (inner + left join via hash join).
  - Interactive REPL with line editing, history (`~/.turntable_history`), tab completion, and dot-commands (`.tables`, `.use`, `.schema`, `.output`, `.explain`, `.strict`, `.help`, `.quit`).
  - Runtime source registration via `.use` and `table: "*"` / `sheet: "*"` wildcard expansion (register every user table / worksheet at once).
  - Output formats: `table`, `csv`, `json`, `ndjson`, `yaml`, `raw`.
  - **Streaming result rendering** (bounded memory, row-by-row for csv/json/ndjson/yaml/raw) plus a `--max-rows` safety cap.
  - `--strict` mode (type-coercion failures become hard errors instead of `NULL`).
  - `--explain` to show the logical plan and per-connector pushdown without running.
  - YAML config file (`turntable.yaml`) with `${ENV_VAR}` / `${VAR:-default}` interpolation for credentials.
  - Inline qualified sources (e.g. `csv:./events.csv`) and `-s` overrides, no config required.

## Language

- **Go** (module `github.com/april/turntable`, Go 1.26.3). Standard library plus `gopkg.in/yaml.v3`, `github.com/xuri/excelize/v2` (Excel), `github.com/chzyer/readline` (REPL), and `modernc.org/sqlite` (pure-Go SQLite driver).

## Folder structure

```
turntable/
├── cmd/
│   └── turntable/
│       └── main.go                 # CLI entrypoint
├── internal/
│   ├── cli/
│   │   ├── cli.go                  # App: flag handling, wiring, source registration, query execution
│   │   ├── cli_test.go             # end-to-end CLI tests (temp data + config)
│   │   ├── repl.go                 # interactive REPL, dot-commands, completion, history
│   │   └── repl_test.go
│   ├── config/
│   │   ├── config.go               # turntable.yaml loader + env-var interpolation
│   │   └── config_test.go
│   ├── sql/
│   │   ├── lexer.go                # SQL tokenizer (tokens, keywords)
│   │   ├── ast.go                  # AST node types (SelectStmt, expr nodes)
│   │   ├── parser.go               # recursive-descent parser → AST
│   │   └── parser_test.go
│   ├── plan/
│   │   ├── plan.go                 # logical plan nodes + Build() (resolve, validate, pushdown)
│   │   └── exec.go                 # plan → RowIterator execution
│   ├── engine/
│   │   ├── types.go                # Type, Value, Column, Schema, Row, RowIterator
│   │   ├── value.go                # Value helpers / Compare / Arith
│   │   ├── eval.go                 # Evaluator: expression evaluation, CAST, EXTRACT, LIKE
│   │   ├── ops.go                  # iterator operators: filter/project/sort/limit/hash-join/aggregate/distinct
│   │   ├── funcs.go                # scalar + aggregate function registry
│   │   ├── engine.go               # Materialize, schema resolvers
│   │   ├── eval_test.go
│   │   └── ops_test.go
│   ├── connector/
│   │   ├── connector.go            # Connector / Dataset / ScanRequest / ScanResponse interfaces
│   │   ├── registry.go             # Registry: connector + source registration & resolution
│   │   ├── helpers.go              # shared connector helpers
│   │   └── connectors/
│   │       ├── jsonc/jsonc.go      # JSON file connector
│   │       ├── csvc/csvc.go        # CSV file connector
│   │       ├── yamlc/yamlc.go      # YAML file connector
│   │       ├── excelc/excelc.go    # Excel .xlsx connector
│   │       ├── excelc/excelc_test.go
│   │       ├── sqlc/sqlc.go        # SQL DB connector (database/sql), schema discovery, pushdown
│   │       ├── sqlc/sqlc_test.go
│   │       └── sqlc/datasets_test.go
│   └── render/
│       ├── render.go               # table/csv/json/ndjson/yaml/raw renderers (buffered + streaming)
│       └── stream_test.go
├── examples/
│   ├── turntable.yaml              # sample config
│   ├── run.sh                      # demo script (15 example queries)
│   ├── init_sqlite.sql             # SQLite demo DB schema/seed
│   └── data/                       # sample data: customers.json, orders.csv, products.yaml, inventory.db, regions.xlsx
├── README.md
├── DESIGN.md
├── go.mod
└── go.sum
```

## How to build

**Build command**: `go build ./cmd/turntable`
**Clean command**: `go clean` (or remove the produced `turntable` binary)

## How to test

**Test command**: `go test ./...`

Tests are standard Go `*_test.go` files using the `testing` package. The `sqlc` connector tests use an in-memory SQLite; the CLI tests build temp data files + config in `t.TempDir()` and assert on rendered output.

## Required tools

**Needed tools**: Go toolchain (≥ 1.26.3). For the SQLite example demo: `sqlite3` CLI (`sqlite3 examples/data/inventory.db < examples/init_sqlite.sql`). Optional: Postgres/MySQL for live SQL connector use.

## Entry point

- `cmd/turntable/main.go` — sets up a signal-cancelled context, constructs `cli.NewApp()`, and calls `app.Run(ctx, os.Args[1:])`.

## Packages: github.com/april/turntable

- **cmd/turntable/main.go**
  - Program entrypoint. Wires signal handling (`os.Interrupt`, `SIGTERM`) and delegates to `internal/cli.App`.
  - Key symbols: `main()`.

- **internal/cli**
  - Flag parsing, config/source wiring, connector registration, query execution, output selection, and the REPL.
  - `cli.go` — `App` struct, `NewApp()`, `Run()`, `runQueryInto()`, `registerSources()`/`registerSource()`/`registerSourceExpand()` (wildcard `table:*`/`sheet:*` expansion), `expandExcelSheets()`, `expandSQLTables()`, `formatPlan()`.
  - `repl.go` — interactive loop, dot-commands (`.tables`, `.use`, `.schema`, `.output`, `.explain`, `.strict`, `.help`, `.quit`), `replCompleter`, history file (`~/.turntable_history`).

- **internal/config**
  - Loads and validates `turntable.yaml`; interpolates `${VAR}` / `${VAR:-default}` env vars in DSNs.
  - Key symbols: `File`, `Source`, `Defaults`, `Load()`, `Parse()`, `interpolate()`.

- **internal/sql**
  - Hand-written lexer + recursive-descent parser producing an AST for the supported SQL subset.
  - `lexer.go` — `Token`, `TokenKind`, `Lex()`, `keywords`.
  - `ast.go` — `Statement`, `SelectStmt`, `SelectList`/`SelectItem`, `TableRef`, `Join`/`JoinKind`, `OrderTerm`, and expression nodes (`LitInt/Float/String/Bool/Null`, `ColRef`, `BinaryOp`, `UnaryOp`, `InExpr`, `BetweenExpr`, `LikeExpr`, `IsNullExpr`, `FuncCall`, `CaseExpr`/`CaseWhen`, `CastExpr`, `ExtractExpr`, `PositionExpr`).
  - `parser.go` — `Parser`, `Parse()`, `ParseExpr()`, `parseSelect()`, `parseTableRef()`, `parseJoin()`, expression precedence chain (`parseOr`→`parseAnd`→`parseCompare`→`parseAdd`→`parseMul`→`parsePrimary`), `parseCase()`, `parseCast()`, `parseExtract()`, `parsePosition()`.

- **internal/plan**
  - Builds the logical plan from the AST: resolves table refs to connector sources, infers/merges schemas, validates columns/types, computes per-connector pushdown, then executes.
  - `plan.go` — `Plan`, `Node` interface, plan nodes (`Scan`, `NoFrom`, `Filter`, `Project`, `Join`, `Aggregate`, `Sort`, `Limit`), `buildCtx`, `Build()`, `WithStrict()`/`IfStrict()`, join-key splitting/classification, aggregate/projection building.
  - `exec.go` — `Exec()`, `execNode()`, `execScan()`, `execJoin()`, `execAggregate()`, schema resolvers.

- **internal/engine**
  - Core types and the in-memory, iterator-based execution operators plus the function registry.
  - `types.go` — `Type` (TypeInvalid…TypeAny), `Value`, `Column`, `Schema`, `Row`, `RowIterator`, `SliceIter`, `FormatValue()`.
  - `value.go` — Value constructors (`Null`, `IntVal`, …), `Compare`, `Arith`.
  - `eval.go` — `Evaluator` (`Eval`, `evalBinary`, `evalIn`, `evalBetween`, `evalLike`, `evalFunc`), `Cast`/`castWithMode`, `parseTime`/`asTime`, `extractField`.
  - `ops.go` — pull-based iterators: `FilterIter`, `ProjectIter`, `SortIter`, `LimitIter`, `HashJoinIter` (inner/left), `AggregateIter` (`AggSpec`/`aggGroup`), `DistinctIter`; `computeAgg()`.
  - `funcs.go` — `FuncRegistry`, `ScalarFunc`, `NewFuncRegistry()`, all built-in scalar functions, `IsAggregate()`.
  - `engine.go` — `Materialize()`, `SchemaResolver()`, `JoinResolver()`, `AliasRange`.

- **internal/connector**
  - The connector extension surface and source registry.
  - `connector.go` — `Connector` interface (`Name`, `Datasets`, `Resolve`, `Scan`), `Dataset`, `ScanRequest` (Columns/Predicate/Limit/OrderBy), `OrderTerm`, `Expr`, `ScanResponse` (`AppliedPredicate`/`AppliedLimit`/`AppliedOrderBy`).
  - `registry.go` — `Registry` (mutex-guarded), `Source`, `RegisterConnector()`, `RegisterSource()`, `Resolve()`/`ResolveQualified()`, `Sources()`.
  - `helpers.go` — shared helpers used across connectors.
  - **connectors/jsonc** — JSON array-of-objects connector.
  - **connectors/csvc** — CSV connector with type inference + delimiter option.
  - **connectors/yamlc** — YAML sequence-of-mappings connector.
  - **connectors/excelc** — `.xlsx` connector (excelize); per-sheet datasets, type inference, `sheet: "*"` wildcard.
  - **connectors/sqlc** — `database/sql` connector with schema discovery (`PRAGMA table_info` / `information_schema.columns` / `DESCRIBE`), `table: "*"` wildcard enumeration, predicate/limit/order pushdown via `translateExpr()`, `rowIter` scanning with type mapping.

- **internal/render**
  - Output formatters for `table`, `csv`, `json`, `ndjson`, `yaml`, `raw`. Buffered (`Renderer`) and streaming (`StreamRenderer`) variants for bounded-memory output.
  - Key symbols: `Format`, `Renderer`, `StreamRenderer`, `New()`, `NewStream()`, per-format renderer structs.

- **examples**
  - `turntable.yaml` (sample config), `run.sh` (15-query demo driver via `go run`), `init_sqlite.sql` (SQLite seed), `data/` sample datasets.

## Documentation

- `README.md` — overview, install, usage examples, expression/function reference, REPL, streaming/safety flags, SQL & Excel source config, layout, examples.
- `DESIGN.md` — architecture, guiding principles, high-level component diagram, the SQL dialect spec, connector interface, planned future connectors (CloudWatch, Prometheus, HTTP/REST, Parquet/Arrow, Git), config schema, roadmap (v0.1–v0.4+), testing strategy, open questions.

## Code Patterns

- **`internal/` boundary**: all implementation is under `internal/`, keeping the v1 API private. `pkg/` for stable out-of-tree connector APIs is planned but not yet present.
- **Pull-based iterator pipeline**: execution is a tree of `RowIterator` operators (`FilterIter`, `ProjectIter`, `SortIter`, `LimitIter`, `HashJoinIter`, `AggregateIter`, `DistinctIter`) each with `Next()`/`Close()` and a `closed bool` guard. The plan tree (`plan.Node`) is walked by `execNode()` into this iterator tree.
  ```go
  type RowIterator interface {
      Next() (*Row, error)
      Close() error
  }
  ```
- **Recursive-descent parser with precedence climbing**: `parseOr` → `parseAnd` → `parseCompare` → `parseAdd` → `parseMul` → `parsePrimary`, with dedicated `parseCase`/`parseCast`/`parseExtract`/`parsePosition` handled in `parsePrimary`.
- **Connector interface as the sole extension surface**: connectors implement `Name`/`Datasets`/`Resolve`/`Scan`; `ScanRequest` carries optional pushdown (`Predicate`, `Limit`, `OrderBy`) and `ScanResponse` reports what was applied so the engine applies the remainder in-memory.
- **Registry with mutex-guarded maps**: `connector.Registry` holds both connector instances and named sources; `Resolve`/`ResolveQualified` map logical table names (and `connector:path` qualified refs) to `Source`.
- **Wildcard expansion at registration**: `table: "*"` (SQL) and `sheet: "*"` (Excel) enumerate all user tables/sheets and register each under its own name (`expandSQLTables`, `expandExcelSheets`, `sqlc.listTables`).
- **Env-var interpolation in config**: `config.interpolate()` expands `${VAR}` and `${VAR:-default}` in DSNs/paths.
- **Schema resolution by alias ranges**: joins track `AliasRange` (`SchemaResolver`/`JoinResolver`) so qualified column refs resolve across joined schemas.
- **Streaming render**: `StreamRenderer` interface renders row-by-row (csv/json/ndjson/yaml/raw) for bounded memory; `table` buffers for column widths. `--max-rows` caps output.
- **Strict vs. lenient coercion**: `Evaluator.Strict` / `castWithMode` — coercion failures yield `NULL` by default or hard errors under `--strict` (`plan.WithStrict`/`IfStrict`).
- **Function registry**: `engine.FuncRegistry` maps names to `ScalarFunc`; `IsAggregate()` distinguishes aggregates; built-ins registered in `registerDefaults()`.
- **Testing**: table-driven tests (`eval_test.go`, `ops_test.go`), parser golden-style tests (`parser_test.go`), per-connector tests with real files / in-memory SQLite, and end-to-end CLI tests (`cli_test.go`) that spin up temp data + config and assert on rendered output.
  ```go
  func run(t *testing.T, cfgPath string, args ...string) (stdout, stderr string, code int) {
      app := NewApp()
      var out, errb bytes.Buffer
      app.Out = &out; app.Err = &errb
      all := append([]string{"-c", cfgPath}, args...)
      code = app.Run(context.Background(), all)
      return out.String(), errb.String(), code
  }
  ```
- **Demo driver**: `examples/run.sh` runs 15 numbered queries via `go run ./cmd/turntable -c examples/turntable.yaml`, filterable by index.

## Code State

- **Implemented (v0.3)**: JSON/CSV/YAML/Excel/SQL connectors, inner & left hash join, group-by + aggregates + HAVING, sort, limit/offset, distinct, `CASE`/`CAST`/`EXTRACT`/`POSITION`, rich string/time/numeric functions, REPL with dot-commands + completion + history, streaming render, `--strict`, `--explain`, `--max-rows`, env-var config interpolation, wildcard table/sheet registration.
- **Missing features (per DESIGN.md roadmap v0.4+)**:
  - Future connectors: CloudWatch Logs/Metrics, Prometheus, HTTP/REST, Parquet/Arrow, Git.
  - Right / full outer joins (only inner + left currently).
  - Window functions and CTEs (`WITH`).
  - Query/result caching and view definitions in config.
  - Public `pkg/` connector API for out-of-tree plugins (currently `internal/` only); runtime plugin mechanism (Yaegi/RPC) deferred.
- **Observed limitations / potential bugs**:
  - SQL pushdown is limited to simple predicates; scalar functions in `WHERE` (e.g. `LOWER(name)`) are not pushed down and run in-memory.
  - Execution is in-memory / bounded-dataset oriented; no spilling or streaming join strategy for very large inputs (explicit v1 non-goal, planned for later).
  - Subqueries are parsed as `TableRef.Subquery` but the planner/build path focuses on the primary SELECT/join flow; deeply nested correlated subqueries are not part of the documented dialect.
- **Areas for improvement**:
  - Promote the connector interface to `pkg/` once stable to enable third-party connectors without forking.
  - Add parser golden-file tests (SQL→AST fixtures) and more error-case coverage, as called out in the DESIGN testing strategy.
  - Expand pushdown support (more predicate shapes, projection pushdown into SQL) to reduce in-memory work.
  - Add right/full outer joins and window functions.
  - Fuzzing for expression evaluation (mentioned as future work in DESIGN.md).
  - Clarify/document dialect deviations from ANSI SQL explicitly.