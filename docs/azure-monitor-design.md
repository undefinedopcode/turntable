# Design: Azure Monitor connectors (metrics + logs)

Status: **proposal, for review** — no code yet.

Goal: give turntable parity with its AWS telemetry coverage on Azure, so an
operator running AKS / Azure Functions / VMs can query metrics and logs in the
same SQL dialect they already use for CloudWatch, and — the real payoff — `JOIN`
them against Honeycomb traces and (later) fleet inventory.

Three connectors, sequenced:

1. **`azmetricsc`** — Azure Monitor **Metrics**. ✅ *implemented*
2. **`azrgraphc`** — Azure **Resource Graph** (fleet inventory; the discovery
   layer that makes per-resource metrics usable at scale, via JOIN). ✅
   *implemented, incl. the shared `azkql` renderer.*
3. **`azlogsc`** — Azure Monitor **Logs** / Log Analytics (KQL), a fast follow.
   ✅ *implemented — reuses `azkql`.*

They share auth (Azure AD via `DefaultAzureCredential`, already used by
`aztablesc`) but hit different data planes and have different query models, so
they are separate packages. `azrgraphc` and `azlogsc` are both KQL engines and
share a translation core (see [shared KQL core](#strategic-note-a-shared-azure-kql-core)).

### Decisions (2026-06-30 review)

1. **Metrics model:** option-driven, mirroring `cwmetricsc`. ✅
2. **Resource identification:** accept raw ARM resource IDs in `azmetricsc`; the
   fleet story is **Resource Graph → JOIN**, so `azrgraphc` is in scope (below).
3. **Per-resource metrics API** for v1; Batch API deferred. ✅
4. **Naming:** `azmetrics:` / `azlogs:` / `azrgraph:` prefixes. ✅
5. **Source of AKS metrics:** platform metrics (Azure Monitor), **not** Managed
   Prometheus — so `azmetricsc` is the right first investment (a Prometheus
   connector is not needed for this platform right now).

---

## 1. `azmetricsc` — Azure Monitor Metrics

### Model: option-driven time series (mirror `cwmetricsc`)

Azure Monitor Metrics is **pre-aggregated by the API**: you name a resource, a
metric, an aggregation (Average/Total/Min/Max/Count), and a bucket interval
(`PT1M`, `PT5M`, …) over a timespan, and it returns bucketed points — optionally
split by a dimension. This is the same shape as CloudWatch `GetMetricData`, so
`azmetricsc` should mirror `cwmetricsc`: a fixed-schema relation driven by
dataset **options**, not a SQL→API translation. The engine applies any residual
WHERE/ORDER BY/LIMIT.

Deliberately **not** aggregate-pushdown (unlike Honeycomb): the API already does
the bucketing and aggregation, so there is no `GROUP BY` to translate. See
[Dimensions & the pushdown question](#dimensions--the-pushdown-question) for the
one place that changes.

### Options

| option           | required | meaning                                                        |
| ---------------- | -------- | -------------------------------------------------------------- |
| `resource`       | yes      | ARM resource ID (`/subscriptions/…/resourceGroups/…/providers/Microsoft.ContainerService/managedClusters/aks1`) |
| `metric`         | yes      | metric name(s), comma-separated (`Percentage CPU`, `NodeCpuUsagePercentage`) |
| `metricnamespace`| no       | metric namespace (needed for custom / some resource metrics)   |
| `aggregation`    | no       | `Average` (default) / `Total` / `Minimum` / `Maximum` / `Count` |
| `interval`       | no       | ISO-8601 duration bucket (`PT5M` default)                      |
| `timespan`       | no       | ISO-8601 interval or a relative window; default last 1h        |
| `dimension`      | no       | split by a dimension, e.g. `node` or `node=*` (see below)      |

`resource` ARM IDs are long and unfriendly; that's inherent to Azure, and we
accept the full ID rather than inventing a friendlier triple (decision 2).

**Honest limitation of v1:** a per-resource `azmetricsc` source queries **one**
resource (the `resource` option, fixed at registration). turntable has no
lateral/correlated join, so you *cannot* today write "JOIN Resource Graph to a
metrics source and fetch metrics for each returned `id`" — the metrics source
can't take a per-row resource. Fleet-wide metrics therefore needs one of:

- the **Batch metrics API** (query up to 50 resources in one call) — deferred by
  decision 3, and the natural v2 once this lands; or
- **lateral joins / parameterized scans** in the engine — a bigger core feature,
  not planned; or
- **drive it externally**: run an `azrgraphc` inventory query, take the `id`s,
  and register/emit one metrics query per resource (scriptable today).

So for v1, `azrgraphc` and `azmetricsc` are independently useful (inventory; and
metrics for a named resource), and the elegant single-query fleet metrics is an
explicit v2 goal riding on the Batch API — not something v1 quietly implies.

### Schema

Fixed core columns, plus one column per split dimension:

```
timestamp   time      bucket start
resource    string    the ARM resource ID (or its short name)
metric      string    metric name
aggregation string    which aggregation the value is
value       float     the aggregated value
<dimension> string     one column per requested dimension split (e.g. "node")
```

One row per (metric, dimension-tuple, time bucket). With no dimension split it's
one row per (metric, bucket) — exactly `cwmetricsc`'s shape plus a `resource`
column (Azure metric responses are per-resource, and a `resource` column makes
multi-resource JOINs and future batching clean).

### Dimensions & the pushdown question

Azure splits a metric by dimension via `$filter=NodeName eq '*'`, returning a
series per dimension value. That is the *one* spot that looks like aggregate
pushdown: `SELECT node, AVG(value) … GROUP BY node` ≈ "split by `node`,
aggregation Average". But the **time bucketing** (`interval`) is orthogonal to
SQL `GROUP BY`, and the aggregation is a *metric* property, not a SQL aggregate.
So a faithful SQL→API mapping is awkward and leaky.

**Recommendation:** v1 exposes the dimension split as an **option**
(`dimension=node`), yielding a `node` column the engine can `GROUP BY`/filter
normally. Revisit real aggregate-pushdown only if users ask — and if we do, it
belongs behind the same `AggregatePusher` interface we built for Honeycomb, with
the time bucket surfaced as a pseudo-column. Flagging the seam, not building it.

### API & SDK

- SDK: `github.com/Azure/azure-sdk-for-go/sdk/monitor/query/azquery` —
  `MetricsClient.QueryResource(ctx, resourceURI, opts)`. New dependency;
  `azidentity`/`azcore` are already in `go.mod`.
- Per-resource ARM API (`.../providers/Microsoft.Insights/metrics`) for v1:
  simplest, one resource per source. The **Metrics Batch** API
  (`azmetrics`, `metrics:getBatch`, up to 50 resources via a regional data-plane
  endpoint) is the fleet-scale upgrade — defer until Resource Graph exists to
  supply the resource list.
- Auth: `DefaultAzureCredential` (env / managed identity / CLI), audience
  `https://management.azure.com/.default` (the SDK sets this). No secret options,
  so nothing new for `config.IsSensitive`.

### Response mapping

`QueryResource` → `Value[]` (per metric) → `TimeSeries[]` (per dimension tuple) →
`Data[]` (per bucket, with `.Average/.Total/.Minimum/.Maximum/.Count`). Emit one
row per `Data` point, pulling the value for the requested `aggregation` and the
dimension values from the timeseries' `Metadatavalues`. Cap buffered points
(`maxPoints`, like `cwmetricsc`).

### Wildcards / enumeration

- `metric=*` → list the resource's available metrics via
  `armmonitor.MetricDefinitionsClient` and query them (bounded). Nice-to-have.
- Cross-resource fan-out is **out of scope** for `azmetricsc` — that's the
  Resource-Graph-feeds-metrics story, done with a JOIN, not baked in.

### Testing

Narrow interface (`metricsAPI`) with the one `QueryResource` method; a fake
returns canned `azquery.Response` values — no Azure creds — exactly as
`cwmetricsc`/`aztablesc` do. Table-driven mapping tests + option validation.

---

## 2. `azlogsc` — Azure Monitor Logs / Log Analytics (KQL)

The Azure twin of `cwlogsc`: AKS container logs, Function logs, app traces all
land in a Log Analytics workspace, queried with **KQL**.

### Model: table + KQL pushdown (like `athenac`)

Log Analytics is a query engine, so unlike metrics this is a real pushdown
target. Two candidate shapes:

- **(A) Table-per-dataset:** a source names a workspace **table** (`ContainerLog`,
  `AppTraces`, `AppRequests`); schema comes from the table's columns; the
  connector renders WHERE/projection/ORDER BY/LIMIT to a KQL query
  (`Table | where … | project … | take …`). Mirrors `athenac`'s SQL translation.
- **(B) Raw KQL passthrough:** a `query` option carries a full KQL string; the
  connector runs it and infers the schema from the result columns. Escape hatch
  for anything the translator can't express.

**Recommendation:** ship **both** — (A) as the primary ergonomic path,
(B) as the always-available escape hatch (KQL is expressive; we won't translate
all of it). This is the same "translate the common case, allow raw for the rest"
split we use elsewhere.

### Schema

- (A) from the table schema — Log Analytics exposes column types
  (`string/int/real/datetime/bool/dynamic`) → engine types, like the columns
  step in `honeycombc`.
- (B) inferred from the returned `Tables[0].Columns` (name + type), like a
  schemaless source.

### API, auth, testing

- SDK: same `azquery` package — `LogsClient.QueryWorkspace(ctx, workspaceID,
  Body{Query, Timespan}, opts)` → `Results.Tables[].{Columns, Rows}`.
- Options: `workspace` (workspace ID, required), `table` (for mode A), `query`
  (mode B), `timespan`.
- Auth: `DefaultAzureCredential`, audience `https://api.loganalytics.io/.default`.
- Narrow `logsAPI` interface + fake, as above.

---

## 3. `azrgraphc` — Azure Resource Graph

The fleet-inventory connector: one KQL endpoint that returns every Azure
resource (AKS clusters, Functions, VMs, NICs, tags, …) across subscriptions.
Highest management leverage of the three; also the connector that builds the
shared KQL core.

### Model: `Resources` table + KQL, with raw passthrough

- **Table-per-dataset:** the source names a Resource Graph table (`Resources`
  default; also `ResourceContainers`, `ResourceChanges`, …); WHERE / project /
  ORDER BY / LIMIT render to KQL via the shared `azkql` core (§below).
- **Raw KQL passthrough:** a `query` option carries a full Resource Graph KQL
  string; schema inferred from the returned columns.

### Schema

Resource Graph rows are semi-structured — common top-level columns
(`id`, `name`, `type`, `location`, `resourceGroup`, `subscriptionId`, `kind`,
`sku`, `tags`, `properties`) plus a per-type dynamic `properties` object. Model
like a schemaless store (`jsonc`/`dynamodbc`): infer the schema by running the
query with a small `$top` sample and unioning keys; `tags`/`properties`/`sku`
stay as nested (`TypeAny`) columns the dialect can index into.

### API, auth, options

- SDK: `github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/resourcegraph/armresourcegraph`
  — `Client.Resources(ctx, QueryRequest{Query, Subscriptions, Options})`;
  `resultFormat: objectArray`. Pagination via the response `$skipToken`.
- Auth: `DefaultAzureCredential`, audience `https://management.azure.com/.default`.
- Options: `subscriptions` (comma list; default all accessible), `table`
  (default `Resources`), `query` (raw KQL passthrough), `top`.
- Narrow `graphAPI` interface + fake, as elsewhere.

---

## Strategic note: a shared Azure-KQL core

`azlogsc` (Log Analytics), `azrgraphc` (Resource Graph), and a future
**Application Insights** connector are *all* KQL engines. The WHERE / projection
/ ORDER BY / LIMIT → KQL renderer is the same for all three. Build it as a small
shared helper — an internal `internal/connector/connectors/azkql` package (pure,
DB-free, unit-tested like `sqlc`'s `buildScanQuery`) — on whichever KQL connector
lands first (Resource Graph or Log Analytics), so the second is mostly wiring.
Resource Graph and Log Analytics KQL dialects differ slightly (table names,
`take` vs `limit` are both fine, some operators); the renderer takes a small
dialect struct, mirroring `sqlc`'s per-driver dialect.

---

## Cross-cutting conventions (both connectors)

- Register in `cli.go` `NewApp()`; connector `Name()` is the ref prefix
  (`azmetrics:`, `azlogs:`) and the `connector:` config value.
- Options flow through `applySourceField` → `Options` (already generic for
  non-file connectors). No new `config.Source` fields.
- No secret options (auth is ambient via `DefaultAzureCredential`), so nothing
  for `ValidateSourceSecrets`.
- Add a web spec in `connectorSpecs.ts`; a user guide under `examples/`.
- New dependency: `sdk/monitor/query/azquery` (+ maybe `azquery/azmetrics` and
  `armmonitor` later). Remember `go mod tidy`.

---

## Remaining build details (decide during implementation)

Resolved at review are recorded under [Decisions](#decisions-2026-06-30-review).
Left to settle when we build:

1. **`azkql` dialect scope** — how much KQL to translate in v1 (equality/compare
   /`in`/`contains`/`startswith` + project + order + take is plenty) vs. leaning
   on the raw-`query` passthrough for the rest.
2. **Resource Graph schema inference** — sample size for the `$top` schema probe,
   and how deep to flatten `properties`/`tags` vs. leaving them nested.
3. **Log Analytics vs. Resource Graph ordering** — either KQL connector can go
   second (both build `azkql`). Resource Graph has higher management value; Log
   Analytics is the closer twin of `cwlogsc`. Leaning Resource Graph second.

---

## Suggested sequence

1. **`azmetricsc`** (per-resource, option-driven, dimension-as-option) + tests +
   docs. Standalone-useful immediately for named resources; no KQL.
2. **`azrgraphc`** (Resource Graph) — builds the shared `azkql` core; delivers
   fleet inventory and the resource discovery behind future batch metrics.
3. **`azlogsc`** (Log Analytics) — reuses `azkql`; table mode + raw passthrough.
4. *(v2)* **Metrics Batch API** — fleet-wide metrics in one query, riding on the
   resource lists `azrgraphc` produces.
