# OctoParser

Query heterogeneous data sources — JSON, CSV, YAML, SQL databases, and (later)
CloudWatch, Prometheus, REST APIs — using a single SQL-style query language.

> **Status:** v0.3. JSON, CSV, YAML, and SQL database connectors are
> implemented, with predicate/limit/order pushdown into SQL databases via
> `database/sql`. Cross-source joins work (e.g. join a Postgres table against a
> CSV file). v0.3 adds a `CASE WHEN`/`CAST`/`EXTRACT` expression layer, a richer
> string/time function library, an interactive REPL, streaming result rendering
> (bounded memory), and a `--strict` mode. See [DESIGN.md](./DESIGN.md) for the
> architecture, supported dialect, and roadmap.

## Install

```bash
go build ./cmd/octoparser
```

## Usage

```bash
# Query a registered source (see examples/octoparser.yaml)
octoparser 'SELECT region, COUNT(*) AS n FROM sales
            WHERE amount > 100 GROUP BY region ORDER BY n DESC LIMIT 10'

# Qualified inline source (no config needed)
octoparser 'SELECT * FROM csv:./events.csv LIMIT 5'

# Explain the plan (pushdown per connector) instead of running
octoparser --explain 'SELECT * FROM users'

# Choose an output format
octoparser -o json 'SELECT * FROM users'

# Query a SQL database with pushdown (WHERE/ORDER BY/LIMIT sent to the DB)
octoparser -c examples/octoparser.yaml \
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
SELECT POSITION('parse' IN 'octoparser') AS pos
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

Interactive mode with line editing, history (`~/.octoparser_history`), tab
completion, and dot-commands:

```bash
octoparser -c examples/octoparser.yaml --repl
octo> .tables
octo> .schema customers
octo> .use sales csv:./data/sales.csv      # register a source at runtime
octo> .use inv sql driver=sqlite dsn=./inventory.db table=inventory
octo> SELECT name, region FROM customers WHERE active = true LIMIT 3;
octo> .output json
octo> .explain
octo> .quit
```

Commands: `.tables`, `.use <name> <spec>`, `.schema [name]`, `.output <fmt>`,
`.explain [off]`, `.strict [off]`, `.help`, `.quit`.

`.use` registers a source without restarting. It takes a `connector:path`
shorthand (e.g. `.use sales csv:./data/sales.csv`) or explicit `key=value`
options (e.g. `.use inv sql driver=sqlite dsn=./x.db table=inventory`); the
source is then queryable by name just like a config-declared source.

### Streaming and safety flags

```bash
# Stream rows as produced (csv/json/ndjson/yaml/raw) — bounded memory
octoparser -o ndjson 'SELECT * FROM big_table'

# Cap rows rendered as a safety guard
octoparser --max-rows 100 'SELECT * FROM huge_table'

# Strict mode: type-coercion failures are hard errors instead of NULL
octoparser --strict 'SELECT CAST(amount AS int) FROM orders'
```

### SQL database sources

Declare a SQL source in `octoparser.yaml`. Credentials may be interpolated from
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
```

The `sql` connector discovers schema via `PRAGMA table_info` (SQLite),
`information_schema.columns` (Postgres/MySQL), or `DESCRIBE` (MySQL), and
pushes down `WHERE`, `ORDER BY`, and `LIMIT` into the database. Unsupported
predicates (e.g. scalar functions like `LOWER(name)`) are not pushed and are
applied in memory by the engine instead.

## Layout

```
DESIGN.md            Architecture and SQL dialect
cmd/octoparser/      CLI entrypoint
internal/cli         flag handling, wiring, REPL
internal/sql         lexer, parser, AST
internal/plan        resolution, validation, pushdown
internal/engine      types, rows, operator pipeline
internal/connector   Connector interface + Registry
internal/connector/connectors/{jsonc,csvc,yamlc,sqlc}
internal/render       output formatters
internal/config       octoparser.yaml loader
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