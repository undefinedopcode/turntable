# Turntable SQL dialect

A pragmatic, **read-only** subset of SQL for querying across connectors. This is
the authoritative reference for the supported grammar, types, and functions.
(`DESIGN.md` covers the architecture; this file covers the language.)

In the REPL, run `.functions` to list every available function from the live
registry, and `.schema <source>` to see a source's columns.

---

## Statement

```sql
SELECT [DISTINCT] <select_list>
[ FROM <table_ref> ]
[ JOIN <table_ref> ON <expr> ]...
[ WHERE <expr> ]
[ GROUP BY <expr>, ... [ HAVING <expr> ] ]
[ ORDER BY <expr> [ASC|DESC], ... ]
[ LIMIT <n> ] [ OFFSET <n> ]
[ UNION [ALL] <select> ]...
```

- **Read-only**: no `INSERT`/`UPDATE`/`DELETE`/DDL.
- A trailing `;` is accepted. Only one statement per query (except `UNION`).
- `FROM` is optional: `SELECT 1 + 1` evaluates a single row (handy in the REPL).
- `SELECT *` and `alias.*` expand all columns (not allowed with aggregation).
- `GROUP BY` / `ORDER BY` accept **positional references**: a bare integer `N`
  means the `N`-th select item (`ORDER BY 2 DESC`, `GROUP BY 1`). An
  out-of-range position is an error.

### Joins

`INNER JOIN` (default) and `LEFT JOIN`, on an equality condition
(`a.x = b.y`) — including across connectors. Right/full outer joins are not yet
supported.

### Subqueries

- **Derived tables** (FROM-clause subqueries) — must be aliased, and may
  themselves be a `UNION`:
  ```sql
  SELECT region, n
  FROM (SELECT region, COUNT(*) AS n FROM sales GROUP BY region) AS g
  WHERE n > 100

  SELECT COUNT(*) FROM (SELECT id FROM a UNION SELECT id FROM b) AS u
  ```
- **`IN` subqueries** — non-correlated, single column; executed once and folded
  into a value set:
  ```sql
  SELECT name FROM users WHERE id IN (SELECT user_id FROM orders)
  ```
  Scalar/`EXISTS` and correlated subqueries are not yet supported.

### UNION

`UNION` (deduplicated) and `UNION ALL` (kept) combine branches with matching
column counts; a trailing `ORDER BY`/`LIMIT` applies to the whole result. In a
chain mixing the two, any plain `UNION` deduplicates the final result.
`INTERSECT`/`EXCEPT` are not supported.

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
`parquet`, `sql`, `http`, `linear`, `trello`, `azuredevops`, `dynamodb`,
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
--                     string|text|varchar, bool|boolean, time|timestamp|datetime
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
| `REGEXP_REPLACE(s, pattern, repl[, flags])` | regex replace |
| `REGEXP_MATCHES(s, pattern)` | true if `s` matches the regex |

### Numeric

| Function | Result |
|----------|--------|
| `ABS(n)` | absolute value |
| `ROUND(n[, digits])` | round (to `digits` decimals) |
| `FLOOR(n)` / `CEIL(n)` / `CEILING(n)` | round down / up |

### Date & time

| Function | Result |
|----------|--------|
| `NOW()` / `CURRENT_TIMESTAMP` | current timestamp |
| `CURRENT_DATE` | today at 00:00 |
| `DATE(x)` | truncate a timestamp to the date |
| `DATE_TRUNC(unit, ts)` | truncate to `second/minute/hour/day/week/month/quarter/year` |
| `DATE_ADD(ts, interval)` | add an interval string (e.g. `'1 day'`, `'2h30m'`) |
| `AGE(ts1, ts2)` | duration `ts1 - ts2` (one-arg form returns `ts1`) |
| `TO_TIMESTAMP(epoch)` | time from unix epoch seconds |
| `STRFTIME(format, ts)` | format a timestamp — **format is the first arg**; accepts strftime `%Y/%m/%d…` specifiers or a Go layout |

### Conditional / null

| Function | Result |
|----------|--------|
| `COALESCE(a, b, …)` | first non-NULL argument |

### Aggregates

Used with (or without) `GROUP BY`:

| Aggregate | Result |
|-----------|--------|
| `COUNT(*)` / `COUNT(expr)` | row count / non-null count |
| `SUM` / `AVG` / `MIN` / `MAX` | the usual |
| `MEDIAN(expr)` | middle value (mean of the two middle for an even count) |
| `STDDEV` / `STDDEV_SAMP` / `STDDEV_POP` | standard deviation (`STDDEV` = sample) |
| `VARIANCE` / `VAR_SAMP` / `VAR_POP` | variance (`VARIANCE` = sample) |
| `STRING_AGG(expr, sep)` | concatenate values, separated by `sep` (input order) |

Each accepts a leading `DISTINCT` (`COUNT(DISTINCT region)`,
`SUM(DISTINCT amount)`, `STRING_AGG(DISTINCT tag, ',')`), which deduplicates the
argument values first. Sample `STDDEV`/`VARIANCE` of a single value is `NULL`.
Filter groups with `HAVING`.

> Aggregates must currently be a top-level select item — `STDDEV(x)` works,
> but wrapping one in a scalar call (`ROUND(STDDEV(x), 2)`) does not yet.

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

Window functions, CTEs (`WITH`), `INTERSECT`/`EXCEPT`, right/full outer joins,
scalar/`EXISTS`/correlated subqueries, and DML/DDL. See `DESIGN.md` §11 for the
roadmap.
