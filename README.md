# OctoParser

Query heterogeneous data sources — JSON, CSV, YAML, SQL databases, and (later)
CloudWatch, Prometheus, REST APIs — using a single SQL-style query language.

> **Status:** v0.2. JSON, CSV, YAML, and SQL database connectors are
> implemented, with predicate/limit/order pushdown into SQL databases via
> `database/sql`. Cross-source joins work (e.g. join a Postgres table against a
> CSV file). See [DESIGN.md](./DESIGN.md) for the architecture, supported
> dialect, and roadmap.

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