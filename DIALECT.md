# Turntable SQL dialect

A pragmatic, **read-only** subset of SQL for querying across connectors. This is
the authoritative reference for the supported grammar, types, and functions.
(`DESIGN.md` covers the architecture; this file covers the language.)

In the REPL, run `.functions` to list every available function from the live
registry, and `.schema <source>` to see a source's columns.

---

## Statement

```sql
[ WITH <name> AS ( <select> ), ... ]
SELECT [DISTINCT] <select_list>
[ FROM <table_ref> ]
[ JOIN <table_ref> ON <expr> ]...
[ WHERE <expr> ]
[ GROUP BY <expr>, ... [ HAVING <expr> ] ]
[ ORDER BY <expr> [ASC|DESC], ... ]
[ LIMIT <n> ] [ OFFSET <n> ]
[ UNION [ALL] <select> ]...
```

- **Read-only** with respect to data sources: no `INSERT`/`UPDATE`/`DELETE`, and
  no DDL that writes to a connector. The only stateful constructs are
  session-scoped [views](#views) (`CREATE/DROP VIEW`) and in-memory
  [materialized views](#materialized-views) (`CREATE/REFRESH/DROP MATERIALIZED
  VIEW`) — they register named queries/results in the session but never modify a
  source.
- A trailing `;` is accepted. Only one statement per query (except `UNION`).
- `FROM` is optional: `SELECT 1 + 1` evaluates a single row (handy in the REPL).
- `SELECT *` and `alias.*` expand all columns (not allowed with aggregation).
- Column aliases work with or without `AS`: `COUNT(*) AS n` or `COUNT(*) n`
  (an alias that is a reserved word still needs `AS`). Table aliases too.
- `GROUP BY` / `ORDER BY` accept **positional references**: a bare integer `N`
  means the `N`-th select item (`ORDER BY 2 DESC`, `GROUP BY 1`). An
  out-of-range position is an error.

### Joins

`INNER JOIN` (default), `LEFT`, `RIGHT`, and `FULL` (the `OUTER` keyword is
optional) — including across connectors. The unmatched side of an outer join is
filled with `NULL`s.

The `ON` condition may be compound. Each `a.x = b.y` equality conjunct (columns
on opposite sides, joined by `AND`) becomes a hash key, so a multi-column join
like `ON a.x = b.x AND a.y = b.y` still runs as a single hash pass. Any remaining
conjunct — a non-equality (`a.ts < b.ts`), an equality over expressions or a
constant, or one touching a single side — is applied as a residual filter on each
matched pair:

```sql
SELECT e.name, d.name
FROM employees e
JOIN departments d
  ON e.dept_id = d.id AND e.salary >= d.min_salary
```

A join whose `ON` has **no** equality conjunct at all (e.g. `ON a.lo <= b.v AND
b.v <= a.hi`) runs as a nested loop — correct, but `O(left × right)`; prefer at
least one equi-key where possible. `--explain` tags the join with `[N keys]`,
`[residual]`, and/or `[nested loop]`.

### Table functions (`generate_series`)

`FROM generate_series(start, stop[, step])` produces a one-column relation
(column `value`, renameable with a column-alias list — see below) — an
**integer** series, or a **timestamp** series when `start`/`stop` are timestamps
and `step` is an `INTERVAL`. `stop` is inclusive; `step` may be negative. The main
use is **gap-filling** a time series — LEFT JOIN the dense series against your
data so missing periods still produce a row:

```sql
SELECT d.value AS day, COALESCE(SUM(m.v), 0) AS total
FROM generate_series(CAST('2024-03-01' AS timestamp),
                     CAST('2024-03-31' AS timestamp), INTERVAL '1 day') AS d
LEFT JOIN (SELECT DATE_TRUNC('day', ts) AS day, v FROM metrics) AS m
       ON m.day = d.value
GROUP BY d.value
ORDER BY day
```

### Column aliases

Any source — base table, derived table, or table function — may rename its
columns with a parenthesized list after the table alias: `AS alias(c1, c2, …)`.
The names are assigned left to right and may be fewer than the source's columns
(trailing columns keep their names); supplying more is an error. Handy for
renaming `generate_series`'s `value`:

```sql
SELECT day FROM generate_series(1, 7) AS g(day)
```

### Common table expressions (`WITH`)

Name one or more queries up front, then reference them by name in `FROM`/`JOIN`
like any source. Each CTE is in scope for the later CTEs and the body; a CTE
shadows a registered source of the same name. The body (and a CTE) may be a
`UNION`.

```sql
WITH eng AS (SELECT name, salary FROM emp WHERE dept = 'eng'),
     rich AS (SELECT name FROM eng WHERE salary > 150000)
SELECT * FROM rich ORDER BY name
```

A CTE is **materialized once per query**: its plan runs a single time and the
resulting rows are buffered in memory and replayed at every reference. So a CTE
used several times — or self-joined — touches its underlying sources only once
(important when a source has latency), and all references see a consistent
snapshot. (A side effect: even a singly-referenced CTE buffers its rows rather
than streaming.) Recursive CTEs (`WITH RECURSIVE`) are not supported.

### Subqueries

- **Derived tables** (FROM-clause subqueries) — must be aliased, and may
  themselves be a `UNION`:
  ```sql
  SELECT region, n
  FROM (SELECT region, COUNT(*) AS n FROM sales GROUP BY region) AS g
  WHERE n > 100

  SELECT COUNT(*) FROM (SELECT id FROM a UNION SELECT id FROM b) AS u
  ```
- **`IN` subqueries** — a non-correlated, single-column `IN (SELECT ...)` is
  executed once and folded into a value set (and is pushdown-eligible):
  ```sql
  SELECT name FROM users WHERE id IN (SELECT user_id FROM orders)
  ```
- **`EXISTS` / `NOT EXISTS`**, **scalar subqueries** `(SELECT ...)` used as a
  value (in `WHERE` or the select list), and **correlated** forms of all three:
  ```sql
  -- correlated EXISTS
  SELECT name FROM emp e WHERE EXISTS (SELECT 1 FROM ord o WHERE o.emp_id = e.id)

  -- correlated scalar subquery in the select list
  SELECT name, (SELECT COUNT(*) FROM ord o WHERE o.emp_id = e.id) AS orders
  FROM emp e

  -- non-correlated scalar in a predicate
  SELECT * FROM ord WHERE amount > (SELECT AVG(amount) FROM ord)
  ```
  A correlated column must be **qualified** with the outer table's alias
  (`e.id`). A scalar subquery must return one column and at most one row (zero
  rows → `NULL`; more than one → an error).

  A correlated `[NOT] EXISTS` over a single table with an equality correlation is
  **decorrelated** into a semi-/anti-join (one hash pass) — fast. Other correlated
  subqueries (scalar, `IN`, non-equality or multi-key `EXISTS`) are evaluated per
  outer row (`O(rows)`): correct, but slower on large inputs.

  A subquery in **`WHERE`** may be combined with `GROUP BY`/aggregates/window
  functions — it filters rows before grouping:
  ```sql
  SELECT region, SUM(amount) AS revenue
  FROM orders
  WHERE amount > (SELECT AVG(amount) FROM orders)
  GROUP BY region
  ```
  A subquery in the **`SELECT` list or `ORDER BY`** of a grouped/windowed query
  is post-aggregation and not yet supported (use a derived table / CTE). Subqueries
  in `HAVING`/`GROUP BY`, and correlation more than one level deep, are also not
  supported.

### Set operations

`UNION`, `INTERSECT`, and `EXCEPT` combine branches with matching column counts;
each has an `ALL` form:

| Operator | Result | `ALL` form |
|----------|--------|------------|
| `UNION` | rows in either branch, deduplicated | keep all (no dedupe) |
| `INTERSECT` | distinct rows in both | per-row min of the two counts |
| `EXCEPT` | distinct rows in the left not in the right | left count minus right count (≥0) |

`INTERSECT` binds tighter than `UNION`/`EXCEPT`, which are left-associative, so
`a UNION b INTERSECT c` means `a UNION (b INTERSECT c)`. A trailing
`ORDER BY`/`LIMIT` applies to the whole result. `NULL`s are treated as equal for
duplicate elimination. (Parenthesized set-op grouping to override precedence is
not yet supported — use a CTE or derived table.)

---

## Views

A **view** is a named query, registered for the rest of the session and usable
anywhere a table is:

```sql
CREATE [OR REPLACE] VIEW name AS <query>
DROP VIEW [IF EXISTS] name
```

A view stores only its definition — no rows. Each query that references it
re-runs the definition against **current** source data, so a view always
reflects the latest data (unlike a materialized view, which is a snapshot until
refreshed). Within a single query, however, a view referenced several times — or
self-joined — is materialized **once** and replayed, like an externally-visible
CTE, so it stays cheap and presents a consistent snapshot for that query.

```sql
CREATE VIEW active_orders AS
  SELECT o.id, o.amount, c.region
  FROM orders o JOIN customers c ON c.id = o.customer_id
  WHERE o.status = 'open';

SELECT region, SUM(amount) AS open_value FROM active_orders GROUP BY region;
```

- `<query>` is any `SELECT` / set-operation / `WITH` query, across any
  connectors. It is bound (planned) at `CREATE` time so errors surface early.
- A view shares the source namespace: its name can't collide with a source or a
  materialized view. `CREATE OR REPLACE VIEW` redefines an existing view.
- A view may reference other views; a self- or cyclic reference is rejected. A
  view binds in the global scope (sources + views), not the CTEs of the query
  that references it.
- Views are **session-scoped** (definitions held for the life of the REPL or
  process), not persisted. They appear in `.tables` and the web sidebar tagged
  `(view)`, and `--explain` shows each reference as `View <name> [materialized]`.

Use a **view** when you want a reusable, always-current query; use a
[materialized view](#materialized-views) when you want to pay the cost once and
query a fixed snapshot repeatedly.

---

## Materialized views

A **materialized view** runs a query once, buffers the result in memory, and
exposes it as a named source for the rest of the session. Use it to assemble
data from several connectors (especially slow ones) once, then query and
re-query the snapshot cheaply. The syntax follows PostgreSQL:

```sql
CREATE MATERIALIZED VIEW [IF NOT EXISTS] name AS <query> [WITH [NO] DATA]
REFRESH MATERIALIZED VIEW name [WITH [NO] DATA]
DROP MATERIALIZED VIEW [IF EXISTS] name
```

- `<query>` is any `SELECT` / set-operation / `WITH` query, across any
  connectors. It is executed at `CREATE` time and the rows are held in memory.
- The view's columns take their **unqualified** output names (a projected
  `e.name` becomes `name`). Two columns that reduce to the same name are
  rejected — give one an `AS` alias, as PostgreSQL requires.
- `WITH NO DATA` defines the view without running the query; it is unscannable
  until a `REFRESH` populates it.
- `REFRESH MATERIALIZED VIEW` re-runs the stored query and replaces the rows
  (the view is **not** auto-updated when its sources change). `WITH NO DATA`
  resets it to the unpopulated state.
- `DROP MATERIALIZED VIEW` removes the rows and unregisters the name.

```sql
CREATE MATERIALIZED VIEW sales AS
  SELECT o.id, o.amount, c.region
  FROM orders o JOIN customers c ON c.id = o.customer_id;

SELECT region, SUM(amount) AS revenue FROM sales GROUP BY region;
SELECT * FROM sales WHERE amount > 1000 ORDER BY amount DESC;
```

Views are **session-scoped** (held in memory for the life of the REPL or process)
and not persisted to disk. They are available in the REPL and one-shot CLI;
`.tables` lists them with a `(mem)` tag. A view referenced multiple times in one
query also materializes just once — see [Common table expressions](#common-table-expressions-with)
for the query-scoped equivalent.

---

## Table references

| Form | Meaning |
|------|---------|
| `users` | a source registered in config / `.use` / the web UI |
| `users AS u`, `users u` | with an alias |
| `csv:./data/sales.csv` | qualified connector ref (prefix = connector name) |
| `excel:./report.xlsx` | file connector ref |
| `http://host/data.json` | inline URL ref (`http`/`https` → the http connector) |
| `sql:postgres://…/db` | the whole DSN is captured as the source |
| `(SELECT …) AS g` | derived table |

The connector prefix is the connector's name: `csv`, `json`, `yaml`, `excel`,
`parquet`, `log`, `sql`, `http`, `linear`, `trello`, `azuredevops`, `dynamodb`,
`azuretables`, `cloudwatchlogs`, `cloudwatch`.

---

## Data types

`int` (int64), `float` (float64), `string`, `bool`, `time`, `duration`, `bytes`,
and `any` (untyped/structured — nested JSON objects/arrays). Columns are
nullable; SQL `NULL` propagates through most expressions.

---

## Operators

| Kind | Operators |
|------|-----------|
| Arithmetic | `+` `-` `*` `/` (and unary `-`) |
| Comparison | `=` `<>` `<` `<=` `>` `>=` |
| Boolean | `AND` `OR` `NOT` |
| Predicates | `IN` `BETWEEN` `LIKE` `ILIKE` `IS [NOT] NULL` |

- `LIKE` uses SQL wildcards `%` (any run) and `_` (one char). `LIKE` is
  case-sensitive; `ILIKE` is the case-insensitive form. Both negate with `NOT`.
- `BETWEEN x AND y` is inclusive.
- `x IN (a, b, c)` or `x IN (SELECT …)`; negate with `NOT IN`.

---

## Conditional & conversion expressions

```sql
-- CASE (searched and simple forms)
CASE WHEN qty > 100 THEN 'bulk' WHEN qty > 0 THEN 'retail' ELSE 'none' END

-- CAST(expr AS type): int|integer|bigint, float|real|double,
--   string|text|varchar, bool|boolean, time|timestamp|datetime
CAST(amount AS int)

-- EXTRACT(field FROM ts): YEAR, MONTH, DAY, HOUR, MINUTE, SECOND, DOW, DOY
EXTRACT(MONTH FROM placed_at)

-- POSITION(sub IN str): 1-based index, 0 if not found
POSITION('parse' IN 'turntable')
```

---

## Functions

Function names are case-insensitive. Most return `NULL` when given `NULL`.
Run `.functions` in the REPL for the live list.

### String

| Function | Result |
|----------|--------|
| `LOWER(s)` / `UPPER(s)` | case conversion |
| `INITCAP(s)` | title-case each word |
| `LENGTH(s)` / `LEN(s)` | character length |
| `SUBSTR(s, start[, len])` / `SUBSTRING` | substring, 1-based start |
| `LEFT(s, n)` / `RIGHT(s, n)` | first / last `n` chars |
| `TRIM(s)` / `LTRIM(s)` / `RTRIM(s)` | strip whitespace |
| `LPAD(s, len[, pad])` / `RPAD(s, len[, pad])` | pad to width (`pad` default space) |
| `CONCAT(a, b, …)` | concatenate |
| `REPLACE(s, from, to)` | replace all occurrences |
| `REPEAT(s, n)` | repeat `n` times |
| `REVERSE(s)` | reverse |
| `STRPOS(s, sub)` / `INSTR(s, sub)` | 1-based index of `sub`, 0 if absent |
| `SPLIT_PART(s, delim, n)` | the `n`-th field (1-based) |
| `REGEXP_REPLACE(s, pattern, repl[, flags])` | regex replace (`repl` uses `$1`/`${name}` group refs; `flags` `'g'` = all) |
| `REGEXP_EXTRACT(s, pattern[, group])` | pull a substring out by regex: with `group` the n-th capture (0 = whole match), else the first capturing group (or the whole match if the pattern has none); NULL on no match. Returns a string — wrap in `CAST(… AS int/float)` to compute on it |
| `REGEXP_MATCHES(s, pattern)` | alias of the 2-arg `REGEXP_EXTRACT` |
| `EXTRACT_VALUE(key FROM s)` / `EXTRACT_VALUE(s, key)` | pull the value of `key` out of a `key: value` / `key=value` string (e.g. a log message or logfmt line); the value is a quoted `"…"`/`'…'` or a bare run, and `key` must sit on a separator boundary. NULL if absent. Returns a string — `CAST` to compute on it |

### Numeric

| Function | Result |
|----------|--------|
| `ABS(n)` | absolute value |
| `ROUND(n[, digits])` | round (to `digits` decimals) |
| `FLOOR(n)` / `CEIL(n)` / `CEILING(n)` | round down / up |
| `TRUNC(n[, places])` | truncate toward zero |
| `SQRT(n)` / `EXP(n)` / `POWER(b, e)` (`POW`) | √ / eˣ / bᵉ |
| `LN(n)` / `LOG10(n)` / `LOG(n)` / `LOG(b, n)` | natural log / base-10 / base-10 / base-`b` |
| `MOD(a, b)` | remainder (integer when both are ints; `NULL` if `b = 0`) |
| `SIGN(n)` | -1 / 0 / 1 |
| `GREATEST(a, b, …)` / `LEAST(a, b, …)` | largest / smallest argument (NULLs ignored) |
| `WIDTH_BUCKET(v, min, max, count)` | histogram bucket `1..count` for an equal-width split of `[min, max)` (`0` below, `count+1` at/above) |

Domain errors (`SQRT(-1)`, `LN(0)`, …) return `NULL` rather than NaN/∞.

### Date & time

| Function | Result |
|----------|--------|
| `NOW()` / `CURRENT_TIMESTAMP` | current timestamp |
| `CURRENT_DATE` | today at 00:00 |
| `INTERVAL '<spec>'` | a duration literal (e.g. `'7 days'`, `'2h30m'`). Add/subtract with a timestamp: `ts + INTERVAL '1 day'`, `NOW() - INTERVAL '1 hour'`; `ts1 - ts2` yields a duration |
| `DATE(x)` | truncate a timestamp to the date |
| `DATE_TRUNC(unit, ts)` | truncate to `second/minute/hour/day/week/month/quarter/year` |
| `DATE_BIN(stride, ts, origin)` | bucket `ts` into fixed `stride` intervals (any width, e.g. `INTERVAL '15 minutes'`) aligned to `origin` |
| `DATE_ADD(ts, interval)` | add an interval string (e.g. `'1 day'`, `'2h30m'`) |
| `AGE(ts1, ts2)` | duration `ts1 - ts2` (one-arg form returns `ts1`) |
| `TO_TIMESTAMP(epoch)` | time from unix epoch seconds |
| `STRFTIME(format, ts)` | format a timestamp — **format is the first arg**; accepts strftime `%Y/%m/%d…` specifiers or a Go layout |
| `CONVERT_TZ(ts, zone)` | render an instant in `zone` (same instant, display/`EXTRACT` become zone-local) |
| `FROM_TZ(ts, zone)` | treat a timestamp's wall-clock as local time in `zone`, yielding the correct instant — use to fix a zone-less timestamp the parser read as UTC |

`zone` is an IANA name (`'America/Los_Angeles'`, DST-aware via embedded tzdata)
or a fixed offset (`'-07:00'`, `'-0700'`, `'+05:30'`, `'Z'`). Timestamps are
compared by **instant**, so values that carry an offset (including non-colon
`-0700` and space-separated forms) sort/compare correctly across zones with no
conversion; a zone-less literal is assumed UTC (apply `FROM_TZ` if it is really
local).

### Conditional / null

| Function | Result |
|----------|--------|
| `COALESCE(a, b, …)` | first non-NULL argument (the NVL / IFNULL equivalent) |
| `NULLIF(a, b)` | NULL when `a = b`, else `a` (e.g. `COALESCE(NULLIF(qty, 0), 1)` to treat 0 as absent) |

### Aggregates

Used with (or without) `GROUP BY`:

| Aggregate | Result |
|-----------|--------|
| `COUNT(*)` / `COUNT(expr)` | row count / non-null count |
| `SUM` / `AVG` / `MIN` / `MAX` | the usual |
| `MEDIAN(expr)` | middle value (mean of the two middle for an even count) |
| `PERCENTILE_CONT(expr, p)` / `QUANTILE(expr, p)` | the `p`-th percentile (`p` in `[0,1]`), linearly interpolated — e.g. `PERCENTILE_CONT(latency, 0.95)` |
| `PERCENTILE_DISC(expr, p)` | the `p`-th percentile as an actual data value (no interpolation) |
| `STDDEV` / `STDDEV_SAMP` / `STDDEV_POP` | standard deviation (`STDDEV` = sample) |
| `VARIANCE` / `VAR_SAMP` / `VAR_POP` | variance (`VARIANCE` = sample) |
| `STRING_AGG(expr, sep)` | concatenate values, separated by `sep` (input order) |
| `CORR(y, x)` | Pearson correlation coefficient |
| `COVAR_POP(y, x)` / `COVAR_SAMP(y, x)` | population / sample covariance |
| `REGR_SLOPE(y, x)` / `REGR_INTERCEPT(y, x)` / `REGR_R2(y, x)` | least-squares line of `y` on `x`, and its R² |
| `REGR_COUNT(y, x)` / `REGR_AVGX(y, x)` / `REGR_AVGY(y, x)` | paired count / mean of `x` / mean of `y` |

The two-argument stats take the dependent variable first (`CORR(y, x)`) and skip
rows where either side is NULL.

Each accepts a leading `DISTINCT` (`COUNT(DISTINCT region)`,
`SUM(DISTINCT amount)`, `STRING_AGG(DISTINCT tag, ',')`), which deduplicates the
argument values first. Sample `STDDEV`/`VARIANCE` of a single value is `NULL`.

Aggregates may be nested inside scalar expressions and combined, and may appear
in `HAVING` and `ORDER BY`:

```sql
SELECT dept,
       ROUND(STDDEV(salary), 2)        AS sd,
       SUM(salary) * 100.0 / COUNT(*)  AS weighted
FROM emp
GROUP BY dept
HAVING SUM(salary) > 100000
ORDER BY COUNT(*) DESC
```

### Window functions

A function with an `OVER (...)` clause computes a value per row over a window of
related rows, without collapsing them:

```sql
SELECT name, dept, salary,
       ROW_NUMBER() OVER (PARTITION BY dept ORDER BY salary DESC)
         AS rank_in_dept,
       SUM(salary) OVER (PARTITION BY dept) AS dept_total,
       salary - LAG(salary) OVER (ORDER BY salary) AS gap
FROM emp
```

`OVER ( [PARTITION BY ...] [ORDER BY ...] [frame] )` — all clauses optional.
Supported functions:

| Function | Result |
|----------|--------|
| `ROW_NUMBER()` | 1-based position within the partition |
| `RANK()` / `DENSE_RANK()` | rank by the `ORDER BY` (with / without gaps for ties) |
| `LAG(expr[, n[, default]])` / `LEAD(...)` | value `n` rows back / forward (default `1`; `default` or `NULL` past the edge) |
| `FIRST_VALUE(expr)` / `LAST_VALUE(expr)` | value at the first / last row of the frame |
| `NTH_VALUE(expr, n)` | value at the `n`-th row of the frame (`NULL` if absent) |
| `NTILE(n)` | bucket the partition into `n` ~equal groups (`1..n`) |
| `PERCENT_RANK()` | `(rank - 1) / (rows - 1)` |
| `CUME_DIST()` | fraction of rows at or before the current peer group |
| `SUM`/`AVG`/`COUNT`/`MIN`/`MAX` `(expr)` | aggregate over the window |

**Frames** narrow which rows an aggregate covers, by physical row offset:
`ROWS BETWEEN <start> AND <end>`, where each bound is `UNBOUNDED PRECEDING`,
`<n> PRECEDING`, `CURRENT ROW`, `<n> FOLLOWING`, or `UNBOUNDED FOLLOWING`
(a single `ROWS <start>` means `… AND CURRENT ROW`). This is how you get moving
averages and rolling sums:

```sql
SELECT t, v,
       AVG(v) OVER (ORDER BY t ROWS BETWEEN 6 PRECEDING AND CURRENT ROW)
         AS avg7,
       SUM(v) OVER (ORDER BY t ROWS UNBOUNDED PRECEDING) AS running
FROM series
```

`RANGE` frames work on the `ORDER BY` **value** instead of row position: peers
(equal values) share a frame, and `n PRECEDING`/`n FOLLOWING` select a value
window (`v` within `cur ± n`, so gaps are skipped). For a **timestamp** order
column the offset is an `INTERVAL` — a rolling time window:

```sql
SELECT ts, v,
       AVG(v) OVER (
         ORDER BY ts
         RANGE BETWEEN INTERVAL '7 days' PRECEDING AND CURRENT ROW
       ) AS avg_7d
FROM metrics
```

RANGE needs exactly one `ORDER BY` column (numeric, or timestamp with an
`INTERVAL` offset). `GROUPS` is not supported.

Without a frame, a window aggregate (and `FIRST_VALUE`/`LAST_VALUE`/`NTH_VALUE`)
covers the whole partition when there is no `ORDER BY`, or a running frame
(cumulative through the current row, ties sharing one value) when there is — so
`LAST_VALUE` over the default frame is the *current* row; use an explicit frame
(`ROWS BETWEEN UNBOUNDED PRECEDING AND UNBOUNDED FOLLOWING`) for the partition's
last value. Window calls may be wrapped in scalar expressions and
used in `ORDER BY`. Combining window functions with `GROUP BY` in one query is
not yet supported.

---

## Pushdown

For a **single-table** scan the planner offers the `WHERE` predicate and a
`LIMIT` to the connector; the engine re-applies both, so results are correct
regardless of what the connector honors. SQL databases translate supported
predicates into the query (`x > 1`, `IN (…)`, `LIKE`, `BETWEEN`, `IS NULL`);
unsupported pieces (e.g. `LOWER(name) = 'x'`) and all `ORDER BY` run in the
engine. Azure Tables translates predicates to an OData `$filter`. Run
`turntable --explain '<query>'` to see what was pushed
(`Scan inv [pushdown: predicate, limit=3]`).

---

## Not yet supported

Subqueries in the `SELECT` list / `ORDER BY` / `HAVING` together with `GROUP BY`
/ aggregates / window functions in one query (a subquery in `WHERE` *is*
supported there — it filters before grouping), recursive CTEs (`WITH RECURSIVE`),
parenthesized set-op grouping, the `GROUPS` window-frame unit (`ROWS`/`RANGE` are
supported), and DML/DDL. See `DESIGN.md` §11 for the roadmap.
