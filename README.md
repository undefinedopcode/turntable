# Turntable

Query heterogeneous data sources — JSON, CSV, YAML, Excel, Parquet, and log
files; SQL databases; and a broad set of URL/API connectors (HTTP/REST, Linear,
Trello, Azure DevOps Boards, Prometheus, Grafana, Honeycomb, AWS CloudWatch /
Athena / Config / Cost Explorer, DynamoDB, Azure Table Storage, Azure Monitor
metrics & logs, Azure Resource Graph, Azure Cost Management) — plus your own via an
external-process plugin protocol, all through a single SQL-style query language.

> **Status:** active development. Implemented: file connectors (JSON, CSV, YAML,
> Excel, Parquet, logs), SQL databases (SQLite / Postgres / MySQL / SQL Server,
> with predicate/limit/order **and aggregate** pushdown), and a wide range of
> URL/API connectors (see the sections below). Cross-source joins work (e.g.
> join a Postgres table against a CSV file, or a REST endpoint against a Parquet
> file). The SQL layer covers `CASE`/`CAST`/`EXTRACT`, a rich string / numeric /
> date-time function library, aggregates, **window functions** (with
> `ROWS`/`RANGE` frames), CTEs (`WITH`), **views** and in-memory **materialized
> views**, set operations (`UNION`/`INTERSECT`/`EXCEPT`), `ASOF` joins, table
> functions (`generate_series`), and subqueries (`IN`/`EXISTS`/scalar/
> correlated). Beyond the one-shot CLI there is an interactive **REPL**, a **web
> UI** (query editor, charts, pivots, dashboards), and an **MCP server** for AI
> agents. Streaming result rendering keeps memory bounded, and `--strict` turns
> coercion failures into errors. See [DESIGN.md](./DESIGN.md) for the
> architecture and [DIALECT.md](./DIALECT.md) for the full language reference.

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

The expression layer provides SQL-standard conditionals and date parts:

```sql
-- conditional logic
SELECT name, CASE WHEN active THEN 'active' ELSE 'inactive' END AS status
FROM customers

-- type conversion (int, float, string, bool, time)
SELECT CAST(amount AS int) AS dollars FROM orders

-- date parts: YEAR, MONTH, DAY, HOUR, MINUTE, SECOND, DOW, DOY
SELECT order_id, EXTRACT(MONTH FROM placed_at) AS month FROM orders

-- substring search (1-based; 0 if not found)
SELECT POSITION('parse' IN 'turntable') AS pos
```

### Subqueries and set operations

A `FROM`-clause subquery (derived table) must be aliased; the outer query reads
its output columns like any table — including across connectors:

```sql
SELECT region, n
FROM (SELECT region, COUNT(*) AS n FROM sales GROUP BY region) AS g
WHERE n > 100

-- a subquery can be one side of a join
SELECT u.name, t.total
FROM users u
JOIN (SELECT user_id, SUM(amount) AS total FROM csv:./orders.csv GROUP BY user_id) AS t
  ON t.user_id = u.id
```

`IN (SELECT ...)`, `EXISTS`/`NOT EXISTS`, and scalar `(SELECT ...)` subqueries
are supported in `WHERE` and the select list, in both non-correlated and
correlated forms. A non-correlated `IN` is executed once and folded into a value
set (so it works across connectors and can even be pushed into a SQL source);
a correlated `EXISTS` with an equality correlation is decorrelated into a
semi/anti-join:

```sql
SELECT name FROM users
WHERE id IN (SELECT user_id FROM csv:./orders.csv WHERE amount > 100)

-- correlated scalar subquery in the select list
SELECT name, (SELECT COUNT(*) FROM ord o WHERE o.emp_id = e.id) AS orders
FROM emp e
```

`UNION`, `INTERSECT`, and `EXCEPT` combine branches with matching column counts,
each with an `ALL` form (`UNION` dedupes, `UNION ALL` keeps all). `INTERSECT`
binds tighter than `UNION`/`EXCEPT`. A trailing `ORDER BY`/`LIMIT` applies to
the whole result:

```sql
SELECT id, name FROM csv:./current.csv
UNION ALL
SELECT id, name FROM csv:./archive.csv
ORDER BY name LIMIT 50
```

See [DIALECT.md](./DIALECT.md) for the precise semantics and the remaining
limits (e.g. a subquery in the `SELECT` list / `ORDER BY` of a grouped query,
recursive CTEs, and parenthesized set-op grouping are not yet supported).

### SQL dialect & functions

[**DIALECT.md**](./DIALECT.md) is the full language reference — grammar, table
references, types, operators, expressions, and every built-in function (string,
numeric, date/time, conditional) and aggregate with signatures.

The library spans `COALESCE`, case/length/pad/trim/regex string functions,
`ABS`/`ROUND`/`FLOOR`/`CEIL`, and a rich date/time set (`EXTRACT`, `DATE_TRUNC`,
`DATE_ADD`, `AGE`, `STRFTIME`, …). For a quick live list, run `.functions` in
the REPL.

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

Commands: `.tables`, `.functions`, `.use <name> <spec>`, `.schema [name]`,
`.output <fmt>`, `.explain [off]`, `.strict [off]`, `.help`, `.quit`.

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

### Web UI

`--serve` starts a browser-based query UI — a complement to the REPL using the
same parse/plan/exec path. The UI is a React + Vite app (source under
`internal/cli/webui/`) built to a static bundle that is embedded into the binary,
so `go build` needs no Node toolchain. See `internal/cli/webui/README.md` for the
dev (`npm run dev` with HMR) and rebuild workflow.

```bash
turntable -c examples/turntable.yaml --serve            # http://localhost:8080
turntable -c examples/turntable.yaml --serve --addr localhost:9000
```

The page is **tabbed** — each tab an independent query workspace (its editor
text, result, and view settings persist in the browser). Each has a CodeMirror
SQL editor (`Ctrl`/`⌘`+`Enter` to run, with source/column/function
autocomplete), a sidebar listing sources (click to expand columns or insert a
name), query history and saved queries, and an **Explain** button. The results
pane has three views: a **table** (client-side sort/filter, cell copy/expand,
CSV/JSON/NDJSON export plus **Parquet** — encoded server-side), a **chart**
(Chart.js — bar/line/area/scatter/bubble/
heatmap/pie plus node-link graph & tree, and a dependency-free **3D graph**
you can rotate/zoom, with time axes and dual-axis support),
and a **pivot** table. Any view can be **maximised** to fullscreen (⤢). Results and views can be **pinned** into **dashboards** —
named panel lists (markdown/table/pivot/chart/stat) stored as YAML, with
toolbar variables and optional auto-refresh; a dashboard can also be rendered to
a self-contained HTML report headlessly (`turntable dashboard render <slug>`).
Results are capped (per `--max-rows`, default 5000) and the response notes
truncation.

The **Add source** button opens a modal that registers a source at runtime —
the browser equivalent of the REPL's `.use`, going through the same registration
path (so wildcards like `table=*`, option routing, and validation behave
identically). The form adapts to the chosen connector: SQL shows driver/DSN/
table, an API connector shows its URL/keys, and so on. For **file** connectors
(CSV, JSON, YAML, Excel, Parquet, log) it offers a **file upload** — the file is
streamed to a persistent, project-relative directory (`.turntable/data`) and the
new source points at it. Uploads are kept across restarts; runtime source
registrations are in-memory by default, but the add-source form has a **Save to
config file** option that appends the (secret-free) source to `turntable.yaml`,
so a saved file source keeps resolving on the next run.

The API: `POST /api/query` (`{"query": "...", "explain": false}` →
`{columns, rows, count, elapsed_ms, ...}`), `GET /api/sources`,
`POST /api/sources` (`{"name", "connector", "fields": {...}}` → `{registered}`),
`POST /api/upload` (multipart `file` → `{path, filename, size}`),
`GET /api/schema?source=<name>`, and `GET /api/connectors` (the per-connector
field specs that drive the add-source form).

It binds to `localhost` by default. Queries are read-only SQL, but they — and
runtime-added sources — run with this process's file and network access (a
qualified ref or a new source can read local files and reach internal URLs), so
binding to a non-local address prints a warning. Only serve on a trusted
network.

### MCP server

`turntable mcp` serves the [Model Context Protocol](https://modelcontextprotocol.io)
over stdio, so an AI agent (Claude Code, for example) can explore and query
your sources conversationally. Flags go before the subcommand, and the config's
declared sources are all available:

```bash
turntable -c turntable.yaml mcp
```

Register it in Claude Code with a project `.mcp.json`:

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

Tools: `query` (run SQL — including `CREATE VIEW` / `CREATE MATERIALIZED VIEW`
session statements; results default to 200 rows with a `truncated` flag, capped
by `--max-rows`), `list_sources`, `describe_source` (columns/types, plus file
freshness for file sources), `list_functions` (the live dialect surface),
`list_connectors` (every connector's fields, with required/sensitive flags),
`add_source` (register a source at runtime — the `.use` equivalent, with the
same secret rules: credentials must be `${ENV_VAR}` references; `save: true`
persists the declared form to the config; the `plugin` connector is refused —
declare those in `turntable.yaml`), `remove_source`, and the dashboard suite:
`list_dashboards` / `get_dashboard` / `save_dashboard` / `delete_dashboard`
(the same YAML store as the web UI) plus `render_dashboard`, which executes a
dashboard's panels headlessly and writes a self-contained HTML report — so an
agent can build a dashboard *and hand you the artifact* without `--serve`.
Query errors come back in-band so the agent can read and self-correct. The
same access caveat as the web UI applies: queries run with this process's file
and network access. See `docs/mcp-server-design.md` for the roadmap.

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
    driver: postgres          # or sqlite, mysql, sqlserver
    dsn: "postgres://${PGUSER}:${PGPASSWORD}@${PGHOST}:5432/analytics"
  reporting:
    connector: sql
    driver: sqlserver
    dsn: "sqlserver://${MSSQLUSER}:${MSSQLPASSWORD}@${MSSQLHOST}:1433?database=reporting"
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

Four drivers are compiled in: **`sqlite`** (`modernc.org/sqlite`, pure Go),
**`postgres`** (`github.com/lib/pq`), **`mysql`**
(`github.com/go-sql-driver/mysql`), and **`sqlserver`**
(`github.com/microsoft/go-mssqldb`). The `driver` field selects which one.

The `sql` connector discovers schema via `PRAGMA table_info` (SQLite),
`information_schema.columns` (Postgres/MySQL/SQL Server), or `DESCRIBE` (MySQL).
For a single-table query the planner pushes the `WHERE`, `ORDER BY`, and `LIMIT`
into the database; pushdown is a pure optimization, so the engine re-applies the
filter, sort, and limit and the result is correct even when only part of the
predicate is pushed. Pushdown is dialect-aware: identifiers are quoted with
double quotes for SQLite/Postgres, backticks for MySQL, and `[brackets]` for SQL
Server; bind parameters are `$1`-style for Postgres, `@pN` for SQL Server, and
`?` elsewhere; the row limit is `LIMIT n` except on SQL Server, which uses
`SELECT TOP (n)`. Unsupported predicates (e.g. scalar functions like
`LOWER(name)`) are not pushed and are applied in memory by the engine instead —
and the `LIMIT` is then held back too, so the engine still sees every matching
row. A single-table `GROUP BY`/aggregate query can also be **pushed whole** to
the database (group-bys, `DATE_BIN` time buckets, and `COUNT`/`SUM`/`AVG`/`MIN`/
`MAX`). Run `turntable --explain` to see what was pushed (e.g. `Scan inv
[pushdown: predicate, limit=3, order]`).

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

### HTTP / REST APIs

The `http` connector fetches a JSON document over HTTP(S) and exposes it as
rows. The response may be a top-level array, a single object (one row), or an
array nested under a dotted `path` (e.g. `data.items`). Schema is inferred from
the records, like the JSON file connector.

```yaml
sources:
  issues:
    connector: http
    url: https://api.example.com/v1/issues
    options:
      path: data.items          # dotted path to the array in the response
      bearer: ${API_TOKEN}      # -> "Authorization: Bearer ..."
      header_x_api_key: ${KEY}  # any header_<name> sets a request header
```

```bash
# inline qualified ref — no config needed (http:// and https:// are recognized)
turntable 'SELECT id, name FROM https://api.example.com/users.json
            WHERE active = true ORDER BY id DESC'

# in the REPL — a full URL selects the http connector
turntable> .use feed https://api.example.com/users.json path=data.users
turntable> SELECT name FROM feed WHERE active = true LIMIT 5;
```

Options: `url` (or the dataset source), `path`, `method` (default GET), `body`,
`bearer`, and `header_<name>` (underscores become hyphens). Any string option
may interpolate `${ENV_VAR}` so tokens stay out of the config file.

An inline URL ref runs to the next whitespace, so query strings work
(`https://h/items?since=2024&limit=50`); a URL containing spaces, commas, or
parentheses must instead be registered as a source (config or `.use`), where
options like `path` can't be confused with the URL. Inline refs carry no
options, so auth headers need the config/`.use` form.

### Parquet files

The `parquet` connector reads a `.parquet` file, taking its schema from the file
footer and streaming rows with bounded memory.

```bash
turntable 'SELECT id, amount FROM parquet:./data/orders.parquet WHERE amount > 100'
turntable> .use orders parquet:./data/orders.parquet
```

### Log files (auto-detected)

The `log` connector reads a plain-text log file and **detects the format** by
sampling the first lines — no schema to declare. Supported: **JSON lines**,
Apache/nginx **combined** and **common** (CLF) access logs, **syslog**
(RFC3164), **logfmt** (`k=v`), **bracketed** `[time] [component] message`
(pacman/ALPM and similar), and a generic **leveled** line (leading timestamp +
level + message); anything else falls back to a `raw` view (`line`, plus a
best-effort `time`/`level`). Fields are typed — `status`/`bytes`/`pid` are ints,
`time` is a timestamp, and numeric `logfmt`/JSON values are coerced.

```bash
# count slow requests by status from a combined access log
turntable "SELECT status, COUNT(*) AS n FROM log:/var/log/nginx/access.log
            WHERE bytes > 1000000 GROUP BY status ORDER BY n DESC"

# errors per hour from a JSON-lines app log
turntable "SELECT EXTRACT(HOUR FROM ts) AS hr, COUNT(*) FROM log:./app.jsonl
            WHERE level = 'error' GROUP BY hr"
```

```yaml
sources:
  access: { connector: log, path: /var/log/nginx/access.log }
  app:     { connector: log, path: ./app.log, options: { format: logfmt } }   # force a format
  custom:                                                                       # custom layout
    connector: log
    path: ./weird.log
    options:
      pattern: '^\[(?P<time>[^\]]+)\] \[(?P<worker>[^\]]+)\] (?P<message>.*)$'
```

In the **web UI**, picking a log file under *Add source* auto-analyzes it: a
recognized format previews its parsed columns, and an unrecognized one is mined
into candidate patterns you can pick and rename — no regex by hand.

`format` (`auto` default, or `json`/`logfmt`/`clf`/`combined`/`syslog`/
`bracketed`/`leveled`/`raw`) forces a parser; `pattern` (a regular expression
with `(?P<name>…)` named
groups → columns) handles anything bespoke. Parsing is line-oriented — multi-line
entries (stack traces) are one row per line for now.

### Linear

The `linear` connector queries the [Linear](https://linear.app) GraphQL API and
exposes a fixed set of datasets — `issues`, `teams`, `projects`, `users` —
flattened into typed columns (issue state, assignee, team are flattened). It
paginates automatically.

```yaml
sources:
  issues:
    connector: linear
    options:
      dataset: issues          # issues | teams | projects | users
      api_key: ${LINEAR_API_KEY}   # personal API key (raw Authorization header)
      # or: bearer: ${LINEAR_OAUTH_TOKEN}
```

```bash
turntable -c turntable.yaml \
  "SELECT identifier, title, state, assignee FROM issues
   WHERE team = 'ENG' ORDER BY priority DESC LIMIT 20"
```

### Trello

The `trello` connector queries the [Trello](https://trello.com) REST API and
exposes a fixed set of datasets — `boards`, `lists`, `cards`, `members` —
flattened into typed columns. Authentication uses a Trello API key and token
(sent in the `Authorization` header, not the query string). `boards` lists your
boards; `lists`, `cards`, and `members` are board-scoped and need a `board` id.

```yaml
sources:
  cards:
    connector: trello
    options:
      dataset: cards
      board: ${TRELLO_BOARD_ID}
      key: ${TRELLO_KEY}
      token: ${TRELLO_TOKEN}
```

```bash
turntable -c turntable.yaml \
  "SELECT name, due, id_list FROM cards WHERE closed = false ORDER BY due"

# in the REPL
turntable> .use boards trello dataset=boards key=$KEY token=$TOKEN
turntable> SELECT name, url FROM boards WHERE closed = false;
```

`cards` exposes `id, id_short, name, desc, closed, id_board, id_list, id_members,
id_labels, labels, start, due, due_complete, due_reminder, url, short_url,
short_link, subscribed, badges, date_last_activity, pos` (`id_members`,
`id_labels`, `labels`, and `badges` — the comment/attachment/checklist-progress
counts — are structured `any` columns; reach into them with `JSON_EXTRACT`). Get
an API key/token at <https://trello.com/app-key>.

### Azure DevOps Boards

The `azuredevops` connector exposes a project's Boards **work items** as a
single dataset, `work_items`, flattened into typed columns. Authentication uses
a Personal Access Token (PAT, with *Work Items: Read* scope) over HTTP Basic
auth. Internally it runs the two-step work-item API — a WIQL query for IDs, then
a batch field fetch — but that is hidden behind the dataset.

```yaml
sources:
  work_items:
    connector: azuredevops
    options:
      organization: my-org
      project: My Project
      pat: ${AZDO_PAT}
      type: Bug              # optional: filter the default query by work item type
```

```bash
turntable -c turntable.yaml \
  "SELECT id, title, state, assigned_to FROM work_items
   WHERE state = 'Active' ORDER BY changed_date DESC LIMIT 20"
```

Columns: `id, title, work_item_type, state, reason, assigned_to,
assigned_to_email, created_by, created_by_email, changed_by, area_path,
iteration_path, board_column, tags, priority, severity, parent_id, comment_count,
story_points, effort, remaining_work, completed_work, created_date, changed_date,
state_change_date, activated_date, resolved_date, closed_date` (`assigned_to`/
`created_by` are display names; the `*_email` columns are the identities' unique
names; the lifecycle timestamps drive cycle-time/lead-time metrics; the estimate
fields are floats). For full control, pass a `wiql` option with a complete WIQL query
(it must `SELECT ... FROM workitems`), e.g. `wiql=SELECT [System.Id] FROM
workitems WHERE [System.Tags] CONTAINS 'release'`.

The `organization` may be the bare slug (`my-org`) or a full
`https://dev.azure.com/my-org` URL. WIQL fails any query that *matches* more than
20,000 items, so the connector **pushes your SQL `WHERE` into the WIQL** to filter
server-side — `WHERE assigned_to_email = 'me@co' AND state = 'Active'` becomes an
Azure-side filter, returning only your items even on a huge project. (Translatable
comparisons and `IN` on the columns above are pushed; the rest still run in the
engine.) A plain-column `ORDER BY` is pushed too, so `ORDER BY changed_date DESC
LIMIT 50` returns the 50 most-recently-changed *from Azure*. An *unfiltered*
query over a >20,000-item project can still exceed the cap — add a `WHERE`, or a
custom `wiql` `WHERE` clause.

### AWS CloudWatch

`cloudwatchlogs` queries log groups (via `FilterLogEvents`); `cloudwatch`
queries metrics (via `GetMetricData`). Both build the AWS client lazily from the
`region`/`profile` options using the standard credential chain.

```yaml
sources:
  applogs:
    connector: cloudwatchlogs
    options:
      region: us-east-1
      log_group: /aws/lambda/my-fn
      filter: "ERROR"          # optional CloudWatch filter pattern
  cpu:
    connector: cloudwatch
    options:
      region: us-east-1
      namespace: AWS/EC2
      metric: CPUUtilization
      stat: Average            # default Average
      period: 300             # seconds
      dim_InstanceId: i-0abc123
```

Logs expose `timestamp, message, log_stream, event_id, ingestion_time`; metrics
expose `timestamp, namespace, metric, stat, value`. Use `start`/`end` (RFC3339
or unix millis) to bound the time range.

### DynamoDB

The `dynamodb` connector exposes a DynamoDB table as rows by scanning its items.
DynamoDB is schemaless, so the schema is inferred from a sample of items (the
union of their attribute names, typed as `any`) — like the JSON connector.

```yaml
sources:
  events:
    connector: dynamodb
    table: events            # table name; "*" registers every table in the account
    options:
      region: us-east-1
      # endpoint: http://localhost:8000   # e.g. DynamoDB Local
```

```bash
turntable -c turntable.yaml 'SELECT id, status FROM events WHERE status = '"'"'open'"'"' LIMIT 20'

# in the REPL — one table, or every table in the account
turntable> .use ev dynamodb table=events region=us-east-1
turntable> .use db dynamodb table=* region=us-east-1
```

Scans are read-only and paginated with bounded memory. Filtering and ordering
run in the engine (no predicate pushdown into DynamoDB yet), so a `LIMIT`
without a `WHERE` is fetched lazily, but a `WHERE` reads the table to filter in
memory — scope queries accordingly on large tables.

### AWS Athena

The `athena` connector queries Athena tables (Presto/Trino over S3, catalogued
in Glue). Athena is itself a SQL engine, so turntable pushes the projection,
`WHERE`, `ORDER BY`, and `LIMIT` down as SQL — important because Athena bills by
data scanned. Schema discovery is free (it reads the Glue catalog); only a
`SELECT` runs a billed query.

```yaml
sources:
  hits:
    connector: athena
    table: page_hits          # or "db.table"; "*" registers every table in the database
    options:
      database: web_analytics
      output_location: s3://my-athena-results/staging/   # required unless the workgroup sets one
      region: us-east-1
      # catalog: AwsDataCatalog
      # workgroup: primary
```

```bash
turntable -c turntable.yaml 'SELECT path, COUNT(*) c FROM hits WHERE day = '"'"'2026-06-01'"'"' GROUP BY path ORDER BY c DESC LIMIT 10'

# in the REPL — one table, or every table in the database
turntable> .use hits athena table=page_hits database=web_analytics output_location=s3://my-athena-results/staging/ region=us-east-1
turntable> .use db athena table=* database=web_analytics output_location=s3://my-athena-results/staging/ region=us-east-1
```

Credentials come from the standard AWS chain (env, shared config, `profile`,
instance role). Push down a partition predicate (e.g. on `day`) to keep scans —
and cost — small.

### Azure Table Storage

The `azuretables` connector exposes a table as rows by listing its entities.
Like DynamoDB it is schemaless, so the schema is inferred from a sample of
entities. Two auth methods are supported:

```yaml
sources:
  # 1. connection string (account key / SAS / Azurite)
  events:
    connector: azuretables
    table: events            # "*" registers every table in the account
    options:
      connection_string: ${AZURE_TABLES_CONNECTION_STRING}
  # 2. Azure AD via DefaultAzureCredential (env, managed identity, az CLI, ...)
  audit:
    connector: azuretables
    table: audit
    options:
      account: mystorageacct           # -> https://mystorageacct.table.core.windows.net/
      # endpoint: http://127.0.0.1:10002/devstoreaccount1   # e.g. Azurite
```

```bash
# in the REPL — one table, or every table in the account
turntable> .use ev azuretables table=events connection_string=UseDevelopmentStorage=true
turntable> .use db azuretables table=* account=mystorageacct
```

A `WHERE` predicate is translated to an OData `$filter` where Azure can express
it (comparisons, `AND`/`OR`/`NOT`, `IN`, `BETWEEN`); anything else (`LIKE`,
`IS NULL`, functions) falls back to in-engine filtering. Local development can
point `endpoint`/`connection_string` at the [Azurite](https://github.com/Azure/Azurite)
emulator.

### Prometheus

The `prom` connector evaluates a PromQL `query` (or a bare `metric` selector)
over a time window via `/api/v1/query_range`, returning one row per
(`ts`, series) with a `value` and one column per label. Reduce at the source
with PromQL (`rate`, `sum by (…)`); the engine does the rest. There is no
predicate pushdown.

```yaml
sources:
  http_rate:
    connector: prom
    url: http://localhost:9090
    options:
      query: 'rate(http_requests_total[5m])'   # or metric: http_requests_total
      time_range: 3600        # lookback seconds (default 1h); or start/end + step
      # bearer: ${PROM_TOKEN}
```

### Honeycomb

The `honeycomb` connector exposes dataset/column/environment **metadata** plus a
per-dataset **`events`** table selected by the `kind` option. Honeycomb has no
raw-event read API, so `events` is aggregate-only — it computes
`GROUP BY`/aggregates at the source via the Query Data API (a paid-plan
feature); the metadata datasets work on any plan. Auth: `api_key`
(`${HONEYCOMB_API_KEY}`), or `management_key` for `environments`.

```yaml
sources:
  events:
    connector: honeycomb
    options:
      kind: events            # events | datasets | columns | environments
      dataset: my-service     # * for every dataset
      api_key: ${HONEYCOMB_API_KEY}
      time_range: 7200        # events query window seconds (default 2h)
```

### Grafana

The `grafana` connector is a **datasource proxy**: instead of connecting to
Prometheus/Loki/a SQL database directly, it runs queries through a Grafana
instance's HTTP API (`POST /api/ds/query`), reusing Grafana's already-configured
datasources and a single Grafana token. Two modes:

- **`kind: datasources`** — the table of contents: every datasource Grafana
  knows about (`id`/`uid`/`name`/`type`/`url`/`is_default`/…). Run this first to
  see what you can query.
- **query mode** (the default) — `datasource=<name-or-uid>` plus a native query
  (`query` / `expr` / `raw_sql`). The connector resolves the datasource's type
  and renders the right request body (Prometheus/Loki take `expr`, SQL
  datasources take `rawSql`, InfluxDB `query`, Graphite `target`).

Grafana answers with typed **dataframes**, so — like Azure Logs — the schema is
exact (no inference): field types map straight to engine types, and multiple
series-frames are flattened into one relation (the union of their fields plus one
string column per distinct label). No SQL pushdown — reduce at the source via the
native query and let the engine apply the residual.

```yaml
sources:
  grafana_ds:                   # list what's available
    connector: grafana
    url: https://grafana.example.com
    options:
      kind: datasources
      token: ${GRAFANA_TOKEN}   # service-account / API key
  cpu:                          # query one datasource
    connector: grafana
    url: https://grafana.example.com
    options:
      datasource: Prometheus    # name or uid (from the datasources table)
      query: 'rate(node_cpu_seconds_total[5m])'
      from: now-6h              # Grafana relative or epoch-ms; default now-1h
      token: ${GRAFANA_TOKEN}
```

```sql
SELECT name, type, uid FROM grafana_ds ORDER BY name;   -- the TOC
SELECT * FROM cpu;                                       -- proxied PromQL
```

### Azure Monitor, Resource Graph & Cost

A family of Azure connectors, all authenticating with `DefaultAzureCredential`
(environment / managed identity / `az login`) and retrying on ARM throttling:

- **`azmetrics`** — Azure Monitor metrics for one `resource` (or a `resources`
  batch across a `region`), by `metric` + `aggregation` + `interval`, optionally
  split by `dimension`.
- **`azlogs`** — Azure Monitor Logs / Log Analytics: a `workspace` queried by
  `table` (or a raw KQL `query`), with `WHERE`/`ORDER BY`/`LIMIT` pushed down as
  KQL.
- **`azrgraph`** — Azure Resource Graph, fleet inventory across `subscriptions`;
  schema inferred from a sample, KQL pushdown, paginated.
- **`azcost`** — Azure Cost Management (pre-aggregated by `metric` /
  `granularity` / `group_by`), paginated across the full result set.

```yaml
sources:
  vm_cpu:
    connector: azmetrics
    options:
      resource: /subscriptions/…/providers/Microsoft.Compute/virtualMachines/vm1
      metric: Percentage CPU
      aggregation: Average
      interval: PT5M
  inventory:
    connector: azrgraph
    options:
      table: Resources        # or a raw KQL `query`
      # subscriptions: sub-1, sub-2   (default: all)
  spend:
    connector: azcost
    options:
      subscription: ${AZURE_SUBSCRIPTION_ID}
      group_by: ServiceName
      granularity: Daily
      timeframe: MonthToDate
```

### AWS Config & Cost Explorer

Two AWS account connectors using the standard credential chain
(`region`/`profile` options):

- **`awsconfig`** — resource inventory via AWS Config Advanced Query. A fixed
  top-level schema with `WHERE`/`LIMIT` pushed down as a Config `SELECT`, or a
  raw `query`; `aggregator` targets a multi-account aggregator.
- **`awscost`** — AWS Cost Explorer (`GetCostAndUsage`), by `granularity` /
  `metric` / `group_by` (`SERVICE`, `REGION`, `TAG:key`; ≤2) over a `start`/`end`
  window.

```yaml
sources:
  resources:
    connector: awsconfig
    options: { region: us-east-1 }        # or query: "SELECT resourceId, resourceType WHERE …"
  aws_spend:
    connector: awscost
    options:
      granularity: DAILY
      group_by: SERVICE
      # start: 2026-06-01   end: 2026-07-01
```

### External-process plugins

Beyond the built-in connectors, a **plugin** connector runs an external program
as a data source, speaking a small JSON-RPC protocol over stdio. Author SDKs
exist for **Go**, **Python**, and **Node.js** (`sdk/`), so a plugin is just a
schema plus a rows function; the reference plugins under `examples/plugins/`
(system info, process table, Docker, Kubernetes, MQTT, GitHub, …) show the shape.
Because a plugin is arbitrary code, it is declared in `turntable.yaml` only (not
via the web add-source form):

```yaml
sources:
  procs:
    connector: plugin
    command: ["./examples/plugins/procinfo/procinfo"]   # ${ENV} interpolated
```

See **PLUGINS.md** for the protocol and `sdk/` for the language SDKs.

### Claude Code transcripts

The `claudelogs` connector reads Claude Code's JSONL session logs (under
`~/.claude/projects/<slug>/`) and exposes the conversation messages as rows —
handy for querying your own usage. Bookkeeping events (titles, mode changes,
snapshots) are skipped; `message.content` is flattened into a `text` column and
`tool_use` blocks are counted.

```bash
# default: the transcripts of the project you run turntable in
turntable "SELECT model, COUNT(*) AS n FROM claudelogs WHERE type='assistant'
            AND model IS NOT NULL GROUP BY model ORDER BY n DESC"

# a specific session file, or a whole project directory (qualified ref)
turntable "SELECT LEFT(text, 80) AS prompt FROM claudelogs:./session.jsonl
            WHERE type='user' ORDER BY timestamp DESC LIMIT 10"
```

```yaml
sources:
  logs:
    connector: claudelogs
    options:
      path: /home/me/.claude/projects/-home-me-projects-foo   # file or directory
      # project: -home-me-projects-foo                        # or a slug / working-dir path
```

Default columns (`kind: messages`): `session_id, session_file, uuid,
parent_uuid, type, role, model, timestamp, text, tool_uses, cwd, git_branch`.

Set `kind: tools` for a **tool-call view** — one row per `tool_use` block, with
`tool_name`, `tool_id`, and `tool_input` (the input re-encoded as JSON, so it's
queryable as a string):

```yaml
sources:
  tools: { connector: claudelogs, options: { kind: tools, path: ./session.jsonl } }
```
```sql
-- which tools get used most
SELECT tool_name, COUNT(*) AS n FROM tools GROUP BY tool_name ORDER BY n DESC

-- grep tool inputs
SELECT tool_input FROM tools WHERE tool_name = 'Bash' AND tool_input LIKE '%git%'
```

`kind: tool_results` is the matching output view — one row per `tool_result`
block, with `tool_use_id`, `is_error`, and `text`. Join it to the tools view to
pair each call with its result:

```sql
-- error rate per tool
SELECT c.tool_name, COUNT(*) AS calls,
       SUM(CASE WHEN r.is_error THEN 1 ELSE 0 END) AS errors
FROM calls c JOIN results r ON c.tool_id = r.tool_use_id
GROUP BY c.tool_name ORDER BY errors DESC
```

With no `path`/`project` it defaults to the current working directory's project.
Note: project slugs contain `-`; the config `path`/`project` options or a
single-session qualified ref are the easiest ways to point at one.

## Layout

```
DESIGN.md            Architecture and roadmap
DIALECT.md           SQL language reference
cmd/turntable/      CLI entrypoint
internal/cli         flag handling, wiring, REPL, web UI, MCP server
internal/sql         lexer, parser, AST
internal/plan        resolution, validation, pushdown
internal/engine      types, rows, operator pipeline
internal/connector   Connector interface + Registry
internal/connector/connectors/
  files   jsonc csvc yamlc excelc parquetc logc claudelogsc
  sql     sqlc                       (sqlite/postgres/mysql/sqlserver)
  api     httpc linearc trelloc azdevopsc cwlogsc cwmetricsc dynamodbc
          aztablesc athenac awsconfigc awscostc promc honeycombc grafanac
          azrgraphc azmetricsc azlogsc azcostc
  plugin  pluginc                    (external-process connectors; sdk/ + examples/plugins/)
  mem     memc                       (backs materialized views)
internal/render       output formatters
internal/config       turntable.yaml loader
examples/             sample config, data, run.sh demo script, plugins/
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