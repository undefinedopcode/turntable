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

- **Read-only**: no `INSERT`/`UPDATE`/`DELETE`/DDL.
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
optional), on a single equality condition (`a.x = b.y`) — including across
connectors. The unmatched side of an outer join is filled with `NULL`s. Compound
or non-equality join conditions are not yet supported (put extra predicates in
`WHERE`).

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

A CTE is expanded wherever it is referenced (referencing it twice plans it
twice). Recursive CTEs (`WITH RECURSIVE`) are not supported.

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
  **decorrelated** into a semi-/anti-join (one hash pass) — fast, and it may then
  be combined with `GROUP BY`. Other correlated subqueries (scalar, `IN`,
  non-equality or multi-key `EXISTS`) are evaluated per outer row (`O(rows)`):
  correct, but slower on large inputs, and not combinable with
  `GROUP BY`/aggregates/window in the same query. Correlation more than one level
  deep, and subqueries in `HAVING`/`GROUP BY`, are not supported.

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
| `REGEXP_REPLACE(s, pattern, repl[, flags])` | regex replace (`repl` uses `$1`/`${name}` group refs; `flags` `'g'` = all) |
| `REGEXP_EXTRACT(s, pattern[, group])` | pull a substring out by regex: with `group` the n-th capture (0 = whole match), else the first capturing group (or the whole match if the pattern has none); NULL on no match. Returns a string — wrap in `CAST(… AS int/float)` to compute on it |
| `REGEXP_MATCHES(s, pattern)` | alias of the 2-arg `REGEXP_EXTRACT` |

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
       ROW_NUMBER() OVER (PARTITION BY dept ORDER BY salary DESC) AS rank_in_dept,
       SUM(salary)  OVER (PARTITION BY dept)                      AS dept_total,
       salary - LAG(salary) OVER (ORDER BY salary)                AS gap
FROM emp
```

`OVER ( [PARTITION BY ...] [ORDER BY ...] )` — both clauses optional. Supported
functions:

| Function | Result |
|----------|--------|
| `ROW_NUMBER()` | 1-based position within the partition |
| `RANK()` / `DENSE_RANK()` | rank by the `ORDER BY` (with / without gaps for ties) |
| `LAG(expr[, n[, default]])` / `LEAD(...)` | value `n` rows back / forward (default `1`; `default` or `NULL` past the edge) |
| `SUM`/`AVG`/`COUNT`/`MIN`/`MAX` `(expr)` | aggregate over the window |

A window aggregate covers the whole partition when there is no `ORDER BY`, or a
running frame (cumulative through the current row, ties sharing one value) when
there is. Window calls may be wrapped in scalar expressions and used in
`ORDER BY`. Explicit frame clauses (`ROWS`/`RANGE BETWEEN …`) and combining
window functions with `GROUP BY` in one query are not yet supported.

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

Explicit window frames (`ROWS`/`RANGE BETWEEN …`), subqueries together with
`GROUP BY`/aggregates/window functions, recursive CTEs (`WITH RECURSIVE`),
parenthesized set-op grouping, non-equality / compound join conditions, and
DML/DDL. See `DESIGN.md` §11 for the roadmap.
