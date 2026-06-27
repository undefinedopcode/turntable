# Turntable

Query heterogeneous data sources — JSON, CSV, YAML, Excel, Parquet, SQL
databases, HTTP/REST APIs, Linear, Trello, Azure DevOps Boards, AWS CloudWatch,
DynamoDB, and Azure Table Storage — using a single SQL-style query language.

> **Status:** v0.4. File connectors (JSON, CSV, YAML, Excel, Parquet), SQL
> databases (with predicate/limit/order pushdown via `database/sql`), and
> URL/API connectors (HTTP/REST, Linear's GraphQL API, CloudWatch logs &
> metrics) are implemented. Cross-source joins work (e.g. join a Postgres table
> against a CSV file, or a REST endpoint against a Parquet file). The expression
> layer covers `CASE WHEN`/`CAST`/`EXTRACT`, a rich string/time function
> library, an interactive REPL, streaming result rendering (bounded memory),
> and a `--strict` mode. See [DESIGN.md](./DESIGN.md) for the architecture,
> supported dialect, and roadmap.

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

-- date parts: YEAR, MONTH, DAY, HOUR, MINUTE, SECOND, DOW, DOY
SELECT order_id, EXTRACT(MONTH FROM placed_at) AS month FROM orders

-- substring search (1-based; 0 if not found)
SELECT POSITION('parse' IN 'turntable') AS pos
```

### Subqueries and UNION

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

A `WHERE x IN (SELECT ...)` / `NOT IN (SELECT ...)` subquery is also supported.
The subquery must be non-correlated and return a single column; it is executed
once and folded into a value set, so it works across connectors and can even be
pushed into a SQL source:

```sql
SELECT name FROM users
WHERE id IN (SELECT user_id FROM csv:./orders.csv WHERE amount > 100)
```

`UNION` (deduplicated) and `UNION ALL` (kept) combine branches with matching
column counts. A trailing `ORDER BY`/`LIMIT` applies to the whole result:

```sql
SELECT id, name FROM csv:./current.csv
UNION ALL
SELECT id, name FROM csv:./archive.csv
ORDER BY name LIMIT 50
```

Not yet supported: scalar and `EXISTS` subqueries, correlated subqueries, and
`INTERSECT`/`EXCEPT`. An `IN` subquery's column must be a scalar type (int,
float, string, bool). In a chain mixing `UNION` and `UNION ALL`, the presence of
any plain `UNION` deduplicates the whole result.

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

The page has a SQL editor (`Ctrl`/`⌘`+`Enter` to run), a sidebar listing
sources (click to expand columns or insert a name), a results table, an
**Explain** button, and CSV export. Results are capped (per `--max-rows`,
default 5000) and the response notes truncation.

The **Add source** button opens a modal that registers a source at runtime —
the browser equivalent of the REPL's `.use`, going through the same registration
path (so wildcards like `table=*`, option routing, and validation behave
identically). The form adapts to the chosen connector: SQL shows driver/DSN/
table, an API connector shows its URL/keys, and so on. For **file** connectors
(CSV, JSON, YAML, Excel, Parquet) it offers a **file upload** — the file is
streamed to a per-session scratch directory on the server and the new source
points at it. Registrations and uploads live for the life of the process (the
scratch directory is removed on shutdown); nothing is written back to
`turntable.yaml`.

The API: `POST /api/query` (`{"query": "...", "explain": false}` →
`{columns, rows, count, elapsed_ms, ...}`), `GET /api/sources`,
`POST /api/sources` (`{"name", "connector", "fields": {...}}` → `{registered}`),
`POST /api/upload` (multipart `file` → `{path, filename, size}`), and
`GET /api/schema?source=<name>`.

It binds to `localhost` by default. Queries are read-only SQL, but they — and
runtime-added sources — run with this process's file and network access (a
qualified ref or a new source can read local files and reach internal URLs), so
binding to a non-local address prints a warning. Only serve on a trusted
network.

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

Three drivers are compiled in: **`sqlite`** (`modernc.org/sqlite`, pure Go),
**`postgres`** (`github.com/lib/pq`), and **`mysql`**
(`github.com/go-sql-driver/mysql`). The `driver` field selects which one.

The `sql` connector discovers schema via `PRAGMA table_info` (SQLite),
`information_schema.columns` (Postgres/MySQL), or `DESCRIBE` (MySQL). For a
single-table query the planner pushes the `WHERE` and `LIMIT` into the database
(`ORDER BY` is applied by the engine); pushdown is a pure optimization, so the
engine re-applies the filter and limit and the result is correct even when only
part of the predicate is pushed. Pushdown is dialect-aware: identifiers are
quoted with double quotes for SQLite/Postgres and backticks for MySQL, and
discovery uses `$1`-style bind parameters for Postgres and `?` elsewhere.
Unsupported predicates (e.g. scalar functions like `LOWER(name)`) are not pushed
and are applied in memory by the engine instead — and the `LIMIT` is then held
back too, so the engine still sees every matching row. Run `turntable --explain`
to see what was pushed (e.g. `Scan inv [pushdown: predicate, limit=3]`).

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

`cards` exposes `id, name, desc, closed, id_board, id_list, due, due_complete,
url, date_last_activity, pos`. Get an API key/token at
<https://trello.com/app-key>.

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

Columns: `id, title, work_item_type, state, assigned_to, assigned_to_email,
area_path, iteration_path, tags, priority, created_date, changed_date`
(`assigned_to` is the display name; `assigned_to_email` is the identity's unique
name/email). For full control, pass a `wiql` option with a complete WIQL query
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
DESIGN.md            Architecture and SQL dialect
cmd/turntable/      CLI entrypoint
internal/cli         flag handling, wiring, REPL
internal/sql         lexer, parser, AST
internal/plan        resolution, validation, pushdown
internal/engine      types, rows, operator pipeline
internal/connector   Connector interface + Registry
internal/connector/connectors/{jsonc,csvc,yamlc,excelc,parquetc,logc,sqlc,httpc,linearc,trelloc,azdevopsc,cwlogsc,cwmetricsc,dynamodbc,aztablesc,claudelogsc}
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