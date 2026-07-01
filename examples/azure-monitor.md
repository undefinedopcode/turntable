# Azure Monitor Metrics connector (`azmetrics`)

Query a resource's Azure Monitor metrics — AKS, Functions, VMs, anything with
platform metrics — as a SQL relation. It mirrors the CloudWatch metrics
connector (`cloudwatch`): the query is driven by **options**, and turntable
applies any residual `WHERE` / `ORDER BY` / `LIMIT`.

## Authentication

Ambient Azure AD via `DefaultAzureCredential` — no key options. It picks up, in
order: environment variables (`AZURE_CLIENT_ID` / `AZURE_TENANT_ID` /
`AZURE_CLIENT_SECRET`), a managed identity, or your `az login` session. The
identity needs the **Monitoring Reader** role on the target resource (or its
subscription/resource group).

## Options

| option        | required | meaning                                                         |
| ------------- | -------- | --------------------------------------------------------------- |
| `resource`    | * | one ARM resource ID (`/subscriptions/<id>/resourceGroups/<rg>/providers/<provider>/<name>`) |
| `resources`   | * | **batch mode:** many ARM resource IDs, comma-separated (see below) |
| `region`      | batch | the metrics data-plane region (e.g. `eastus`); required with `resources` |
| `metric`      | yes      | metric name(s), comma-separated (e.g. `Percentage CPU`)         |
| `aggregation` | no       | `Average` (default) / `Total` / `Minimum` / `Maximum` / `Count` |
| `interval`    | no       | ISO-8601 bucket duration (`PT5M` default; `PT1H`, `P1D`, `FULL`)|
| `timespan`    | no       | ISO-8601 `start/end`; default is the last hour                  |
| `dimension`   | no       | dimension name(s) to split by, comma-separated — **adds a column per dimension** |
| `namespace`   | no       | metric namespace (custom or ambiguous metrics)                  |
| `filter`      | no       | raw Azure `$filter` override (advanced; supersedes `dimension`) |

## Schema

```
timestamp   time     bucket start
resource    string   the ARM resource ID
metric      string   metric name
aggregation string   which aggregation the value is
value       float    the aggregated value
<dimension> string    one column per requested `dimension` split
```

One row per (metric, dimension tuple, time bucket).

## Example

```yaml
# turntable.yaml
sources:
  aks-cpu:
    connector: azmetrics
    options:
      resource: /subscriptions/00000000-0000-0000-0000-000000000000/resourceGroups/prod/providers/Microsoft.ContainerService/managedClusters/prod-aks
      metric: node_cpu_usage_percentage
      aggregation: Average
      interval: PT5M
      dimension: node
```

```bash
turntable -c turntable.yaml \
  "SELECT node, MAX(value) AS peak_cpu
     FROM aks-cpu
     GROUP BY node
     ORDER BY peak_cpu DESC"
```

Here Azure returns per-node 5-minute averages; turntable does the `GROUP BY node`
and `MAX` over them. (The connector does **not** push down `GROUP BY` — the Azure
API is already pre-aggregated by `aggregation` + `interval`, and the engine
handles any further rollup.)

## Batch mode: many resources in one query

Give `resources` (a comma-separated list of ARM IDs) plus `region` to query
metrics for many resources at once via the Metrics Batch API. Every row carries
its own `resource`, so you can group across the fleet. All resources must share
the same **subscription and region**; the connector chunks the list into calls of
up to 50.

```yaml
sources:
  vm-cpu:
    connector: azmetrics
    options:
      region: eastus
      metric: Percentage CPU
      aggregation: Maximum
      resources: >
        /subscriptions/…/virtualMachines/vm1,
        /subscriptions/…/virtualMachines/vm2,
        /subscriptions/…/virtualMachines/vm3
```

```bash
turntable -c turntable.yaml \
  "SELECT resource, MAX(value) AS peak_cpu FROM vm-cpu GROUP BY resource ORDER BY peak_cpu DESC"
```

Pair with `azrgraph` to get the resource IDs for a fleet: run a Resource Graph
query to list (say) every VM in a region, then feed those IDs into `resources`.
(A single correlated query — JOIN Resource Graph to metrics per row — still isn't
possible; turntable has no lateral join, so the IDs are supplied to the source,
not joined in.)

## Notes & limitations

- The default window is the **last hour**; set `timespan` for anything else
  (e.g. `2026-06-30T00:00:00Z/2026-06-30T06:00:00Z`).
- `dimension` both adds columns and asks Azure to split the series; without it
  you get one aggregated series for the whole resource.
- Metric names are case- and space-sensitive and vary by resource type — check
  the resource's *Metrics* blade in the Azure portal, or its
  `metricDefinitions`.
