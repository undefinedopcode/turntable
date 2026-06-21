# Turntable

Query heterogeneous data sources — JSON, CSV, YAML, Excel, SQL databases, and
(later) CloudWatch, Prometheus, REST APIs — using a single SQL-style query
language.

> **Status:** v0.3. JSON, CSV, YAML, Excel, and SQL database connectors are
> implemented, with predicate/limit/order pushdown into SQL databases via
> `database/sql`. Cross-source joins work (e.g. join a Postgres table against a
> CSV file). v0.3 adds a `CASE WHEN`/`CAST`/`EXTRACT` expression layer, a richer
> string/time function library, an interactive REPL, streaming result rendering
> (bounded memory), and a `--strict` mode. See [DESIGN.md](./DESIGN.md) for the
> architecture, supported dialect, and roadmap.

## Install

```bash
go build ./cmd/turntable
```

## Usage

```bash
# Query a registered source (see examples/turntable.yaml)
turntable 'SELECT region, COUNT(*) AS n FROM sales
            WHERE amount > 100 GROUP BY region ORDER BY n DESC LIMIT 10'

# Qualified inline source (no config needed)
turntable 'SELECT * FROM csv:./events.csv LIMIT 5'

# Explain the plan (pushdown per connector) instead of running
turntable --explain 'SELECT * FROM users'

# Choose an output format
turntable -o json 'SELECT * FROM users'

# Query a SQL database with pushdown (WHERE/ORDER BY/LIMIT sent to the DB)
turntable -c examples/turntable.yaml \
  'SELECT id, item, qty, price FROM inventory WHERE qty > 20 ORDER BY price DESC LIMIT 5'
```

### Expressions: CASE, CAST, EXTRACT

The v0.3 expression layer adds SQL-standard conditionals and date parts:

```sql
-- conditional logic
SELECT name, CASE WHEN active THEN 'active' ELSE 'inactive' END AS status
FROM customers

-- type conversion (int, float, string, bool, time)
SELECT CAST(amount AS int) AS dollars FROM orders

-- date parts: YEAR, MONTH, DAY, HOUR, MINUTE, SECOND, DOW, DOY, EPOCH
SELECT order_id, EXTRACT(MONTH FROM placed_at) AS month FROM orders

-- substring search (1-based; 0 if not found)
SELECT POSITION('parse' IN 'turntable') AS pos
```

### Built-in functions

Beyond the v0.1/v0.2 set (`COALESCE`, `LOWER/UPPER`, `LENGTH`, `SUBSTR`,
`TRIM/LTRIM/RTRIM`, `CONCAT`, `ABS`, `ROUND`, `FLOOR`, `CEIL`, `REPLACE`,
`NOW`), v0.3 adds:

- **String:** `LEFT`, `RIGHT`, `POSITION`/`STRPOS`, `SPLIT_PART`,
  `REGEXP_REPLACE`, `REGEXP_MATCHES`, `REPEAT`, `REVERSE`, `INITCAP`,
  `LPAD`, `RPAD`
- **Time:** `EXTRACT`, `DATE_TRUNC`, `DATE_ADD`, `AGE`, `TO_TIMESTAMP`,
  `DATE`, `STRFTIME` (strftime `%Y/%m/%d` specifiers or Go layouts),
  `CURRENT_DATE`

### REPL

Interactive mode with line editing, history (`~/.turntable_history`), tab
completion, and dot-commands:

```bash
turntable -c examples/turntable.yaml --repl
turntable> .tables
turntable> .schema customers
turntable> .use sales csv:./data/sales.csv      # register a source at runtime
turntable> .use inv sql driver=sqlite dsn=./inventory.db table=inventory
turntable> SELECT name, region FROM customers WHERE active = true LIMIT 3;
turntable> .output json
turntable> .explain
turntable> .quit
```

Commands: `.tables`, `.use <name> <spec>`, `.schema [name]`, `.output <fmt>`,
`.explain [off]`, `.strict [off]`, `.help`, `.quit`.

`.use` registers a source without restarting. It takes a `connector:path`
shorthand (e.g. `.use sales csv:./data/sales.csv`) or explicit `key=value`
options (e.g. `.use inv sql driver=sqlite dsn=./x.db table=inventory`); the
source is then queryable by name just like a config-declared source.

For a SQL database, set `table=*` to register **every** user table in the
database at once, each queryable by its own name:

```
turntable> .use db sql driver=sqlite dsn=./inventory.db table=*
registered 3 tables: events, metrics, users
turntable> SELECT name FROM users LIMIT 2;
turntable> SELECT count(*) FROM events;
```

This also works in the config file (`table: "*"`) — useful for pointing at a
whole database without listing each table.

### Streaming and safety flags

```bash
# Stream rows as produced (csv/json/ndjson/yaml/raw) — bounded memory
turntable -o ndjson 'SELECT * FROM big_table'

# Cap rows rendered as a safety guard
turntable --max-rows 100 'SELECT * FROM huge_table'

# Strict mode: type-coercion failures are hard errors instead of NULL
turntable --strict 'SELECT CAST(amount AS int) FROM orders'
```

### SQL database sources

Declare a SQL source in `turntable.yaml`. Credentials may be interpolated from
environment variables (`${VAR}`, `${VAR:-default}`):

```yaml
sources:
  warehouse:
    connector: sql
    driver: postgres          # or sqlite, mysql
    dsn: "postgres://${PGUSER}:${PGPASSWORD}@${PGHOST}:5432/analytics"
  inventory:
    connector: sql
    driver: sqlite
    dsn: "./examples/data/inventory.db"
    table: inventory           # optional; defaults to the source name
  analytics:
    connector: sql
    driver: postgres
    dsn: "postgres://${PGUSER}:${PGPASSWORD}@${PGHOST}:5432/analytics"
    table: "*"                  # wildcard: register every user table in the DB
```

The `sql` connector discovers schema via `PRAGMA table_info` (SQLite),
`information_schema.columns` (Postgres/MySQL), or `DESCRIBE` (MySQL), and
pushes down `WHERE`, `ORDER BY`, and `LIMIT` into the database. Unsupported
predicates (e.g. scalar functions like `LOWER(name)`) are not pushed and are
applied in memory by the engine instead.

A `table: "*"` source enumerates every user table in the database (via
`PRAGMA table_list` / `information_schema.tables`, filtering out system tables)
and registers each one under its own name — so a multi-table DB becomes a set
of independently queryable sources without listing them by hand.

### Excel workbooks

The `excel` connector (backed by [xuri/excelize](https://github.com/qax-os/excelize))
reads `.xlsx` files. Each worksheet is a dataset: the first row is the header
and column types are inferred (int, float, bool, time, string). Use the `sheet`
option to pick a sheet, or `sheet: "*"` to register every sheet:

```yaml
sources:
  report:
    connector: excel
    path: ./data/report.xlsx
    sheet: Q1                # a specific sheet
  workbook:
    connector: excel
    path: ./data/report.xlsx
    sheet: "*"               # register every sheet under its own name
```

```bash
# inline (uses the first sheet)
turntable 'SELECT * FROM excel:./data/report.xlsx LIMIT 5'

# in the REPL — register one sheet, or all of them
turntable> .use q1 excel:./data/report.xlsx sheet=Q1
turntable> .use wb excel:./data/report.xlsx sheet=*
registered 3 tables: summary, Q1, Q2
```

## Layout

```
DESIGN.md            Architecture and SQL dialect
cmd/turntable/      CLI entrypoint
internal/cli         flag handling, wiring, REPL
internal/sql         lexer, parser, AST
internal/plan        resolution, validation, pushdown
internal/engine      types, rows, operator pipeline
internal/connector   Connector interface + Registry
internal/connector/connectors/{jsonc,csvc,yamlc,excelc,sqlc}
internal/render       output formatters
internal/config       turntable.yaml loader
examples/             sample config, data, and run.sh demo script
```

## Examples

```bash
# Run all demo queries (JSON, CSV, YAML, SQL, joins, output formats, explain)
./examples/run.sh

# Run a single demo by index
./examples/run.sh 10   # SQL database with pushdown

# (Re)create the SQLite demo database
sqlite3 examples/data/inventory.db < examples/init_sqlite.sql
```

See [DESIGN.md](./DESIGN.md) for the full design, roadmap, and extension model.