# Azure Monitor Logs connector (`azlogs`)

Query a Log Analytics workspace — AKS container logs, Function logs, app traces,
requests — with KQL, exposed as SQL. The Azure twin of the CloudWatch Logs
connector.

Log Analytics is a KQL engine, so this connector pushes `WHERE` / `ORDER BY` /
`LIMIT` down as KQL (via the shared `azkql` renderer, same as Resource Graph); a
raw `query` option carries a full KQL string for anything the translator can't
express (`summarize`, `join`, `mv-expand`, …). The API returns **typed columns**
with the rows, so the schema is exact — no inference.

## Authentication

Ambient Azure AD via `DefaultAzureCredential` — no key options (env vars, a
managed identity, or your `az login` session). The identity needs the **Log
Analytics Reader** role on the workspace.

## Options

| option      | required | meaning                                                       |
| ----------- | -------- | ------------------------------------------------------------- |
| `workspace` | yes      | Log Analytics workspace ID (GUID)                             |
| `table`     | —        | the table to query (`ContainerLogV2`, `AppRequests`, …); also the ref Source (`azlogs:AppRequests`) |
| `query`     | —        | a raw KQL query (overrides `table` and pushdown)             |
| `timespan`  | —        | ISO-8601 duration (`PT1H`, `P1D`) or `start/end`; default `P1D` (last day) |
| `top`       | —        | safety row cap per scan (default 30000)                       |

One of `table` or `query` is required.

## Examples

Recent errors from AKS container logs:

```yaml
# turntable.yaml
sources:
  container-logs:
    connector: azlogs
    options:
      workspace: 00000000-0000-0000-0000-000000000000
      table: ContainerLogV2
      timespan: PT6H
```

```bash
turntable -c turntable.yaml \
  "SELECT TimeGenerated, PodName, LogMessage
     FROM container-logs
     WHERE LogLevel = 'error'
     ORDER BY TimeGenerated DESC"
```

The `WHERE`, `ORDER BY`, and `LIMIT` push into KQL (check `--explain`); the
`timespan` bounds the scan server-side.

Raw KQL for aggregations and anything the translator doesn't cover:

```yaml
sources:
  req-by-status:
    connector: azlogs
    options:
      workspace: 00000000-0000-0000-0000-000000000000
      timespan: P7D
      query: >
        AppRequests
        | summarize count() by ResultCode, bin(TimeGenerated, 1h)
        | order by TimeGenerated desc
```

```bash
turntable -c turntable.yaml "SELECT * FROM req-by-status"
```

## Notes

- **Pushdown is an optimization** — the engine re-applies the full
  `WHERE`/`ORDER BY`/`LIMIT`, so predicates the translator can't express still
  work (in-engine). Push-eligible: `=`, `<>`, `<`,`<=`,`>`,`>=`, `IN`,
  `LIKE '%x%'` (→ `contains`), `IS [NOT] NULL` on top-level columns.
- **Timespan matters.** Logs are large; the default is the last day. Widen with
  `timespan` (e.g. `P7D`) or pin an absolute window (`start/end` ISO interval).
- **Column types are exact.** Log Analytics reports each column's type
  (`datetime`→time, `int`/`long`→int, `real`/`decimal`→float, `bool`→bool,
  `dynamic`→JSON); string/guid/timespan come through as strings.
- **`top`** caps rows client-side (raw KQL may not carry its own `take`); raise
  it for big pulls, mindful of memory.
