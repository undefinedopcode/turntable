# Honeycomb connector

Turntable's `honeycomb` connector exposes four datasets, selected by a `kind`
option:

| kind           | what it returns                                   | API             | plan     |
| -------------- | ------------------------------------------------- | --------------- | -------- |
| `datasets`     | every dataset in the environment                  | v1 `/1/datasets`| any      |
| `columns`      | the columns (+ types) of one dataset              | v1 `/1/columns` | any      |
| `environments` | the team's environments                           | v2 Management   | any      |
| `events`       | **aggregate** queries over a dataset's event data | Query Data API  | **paid** |

`datasets`, `columns` and `environments` are plain metadata and work on any
plan. `events` runs Honeycomb's **Query Data API**, which is gated to paid plans
— on a free plan an events query returns a `403` with a hint. See
[Events queries](#events-queries-paid-plans) below.

## Authentication

| option           | header                    | used by                       |
| ---------------- | ------------------------- | ----------------------------- |
| `api_key`        | `X-Honeycomb-Team` (v1)   | datasets / columns / events   |
| `management_key` | `Authorization: Bearer`   | environments                  |

Both are treated as secrets: in `turntable.yaml` they must be an `${ENV_VAR}`
reference, never a literal. If unset, the connector falls back to the
`HONEYCOMB_API_KEY` / `HONEYCOMB_MANAGEMENT_KEY` environment variables (so bare
qualified refs like `honeycomb:datasets` work with no config file).

`api_key` must be a **Configuration key** (not an Ingest key): the v1 API that
lists datasets/columns and runs queries authenticates with a Configuration key.
Give it, at minimum:

- **Manage queries and columns** — read datasets/columns
- **Send events** + **Create datasets** — only if you also seed data with it
- **Run queries** — only for `events` queries (**paid plans only**)

`region: eu` selects `https://api.eu1.honeycomb.io`; the default is US
(`https://api.honeycomb.io`). `url` overrides the base entirely.

## Metadata queries (any plan)

### List datasets — no config file needed

```bash
export HONEYCOMB_API_KEY=<configuration key>
turntable "SELECT name, slug, columns_count, last_written_at
             FROM honeycomb:datasets
             ORDER BY last_written_at DESC"
```

`honeycomb:datasets` is a bare qualified ref: the connector prefix is
`honeycomb` and `kind` defaults from the ref to `datasets`. Auth comes from
`HONEYCOMB_API_KEY`.

### List a dataset's columns

`columns` needs a `dataset` slug, so configure it as a source
(`turntable.yaml`):

```yaml
sources:
  demo-columns:
    connector: honeycomb
    options:
      kind: columns
      dataset: turntable-demo        # the Honeycomb dataset slug
      api_key: ${HONEYCOMB_API_KEY}
```

```bash
turntable -c turntable.yaml \
  "SELECT key_name, type, last_written FROM demo-columns ORDER BY key_name"
```

Honeycomb column types map to turntable types as
`string`→string, `integer`→int, `float`→float, `boolean`→bool.

### List environments (needs a Management key)

```yaml
sources:
  envs:
    connector: honeycomb
    options:
      kind: environments
      team: my-team-slug
      management_key: ${HONEYCOMB_MGMT_KEY}   # "keyID:secret"
```

```bash
turntable -c turntable.yaml "SELECT name, slug, color FROM envs"
```

## Seeding a dataset with test data

Sending events auto-creates the dataset and its columns, so you can populate a
throwaway dataset with the helper script in this directory:

```bash
export HONEYCOMB_API_KEY=<configuration key with Send events + Create datasets>
python3 examples/honeycomb_seed.py --dataset turntable-demo --count 500
```

It sends synthetic spans (`service.name`, `duration_ms`, `http.status_code`,
`error`, …) spread over the last ~2 hours, so a default 2-hour query window
covers them. Pass `--region eu` for an EU account.

## Events queries (paid plans)

`events` has **no raw-row mode** — Honeycomb only answers aggregate queries — so
a plain scan is an error. Write a `GROUP BY` / aggregate query and the planner
pushes the whole aggregation (breakdowns, calculations, filters, time window)
down to Honeycomb's Query Data API; the aggregated rows come back and turntable
applies `HAVING` / `ORDER BY` / `LIMIT` on top.

```yaml
sources:
  turntable-demo:
    connector: honeycomb
    options:
      dataset: turntable-demo        # kind defaults to "events"
      api_key: ${HONEYCOMB_API_KEY}  # needs "Run queries" (paid)
      time_range: 7200               # query window, seconds (default 7200 = 2h)
```

```bash
turntable -c turntable.yaml \
  "SELECT service.name, COUNT(*) AS n, AVG(duration_ms) AS avg_dur
     FROM turntable-demo
     WHERE http.status_code >= 500
     GROUP BY service.name
     ORDER BY n DESC"
```

Notes:

- **Dotted attribute names** (`service.name`, `http.status_code`) work directly
  even though SQL lexes them as `qualifier.name`.
- Supported aggregates map to Honeycomb calculations: `COUNT`,
  `COUNT(DISTINCT c)`, `SUM`, `AVG`, `MIN`, `MAX`, `MEDIAN` (→ P50).
- The time window comes from `time_range` (seconds) or explicit
  `start_time` / `end_time` (Unix epoch seconds).
- `--explain` shows the aggregation pushed into the `Scan` (no engine
  `Aggregate` node above it).
- `dataset: "*"` registers one events source per Honeycomb dataset, each named
  by its slug.
- On a **free plan** this returns a `403` with a hint that the Query Data API
  requires a paid plan; the metadata datasets above still work.
