# Cost connectors — AWS Cost Explorer (`awscost`) & Azure Cost Management (`azcost`)

Query cloud spend as SQL: cost grouped by service / resource group / tag over a
time window. Both APIs are pre-aggregated (you pick the metric, granularity, and
group-by), so — like the metrics connectors — these are **option-driven**: the
connector returns grouped time-series rows and the engine does any residual
filter / sort / join / further rollup.

Both authenticate ambiently (AWS SDK chain / Azure AD) — no key fields.

---

## AWS Cost Explorer (`awscost`)

Needs `ce:GetCostAndUsage`. Cost Explorer is a global endpoint (defaults to
`us-east-1`).

| option        | meaning                                                          |
| ------------- | ---------------------------------------------------------------- |
| `granularity` | `DAILY` (default) / `MONTHLY` / `HOURLY`                          |
| `metric(s)`   | cost metric(s), comma-separated (default `UnblendedCost`; also `BlendedCost`, `AmortizedCost`, `NetUnblendedCost`, `UsageQuantity`, …) |
| `group_by`    | up to 2, comma-separated; each `TYPE:KEY` or `KEY` (default type `DIMENSION`), e.g. `SERVICE`, `REGION`, `TAG:env`, `COST_CATEGORY:team` |
| `start`,`end` | window `YYYY-MM-DD` (`end` exclusive); default: last 30 days     |
| `region`,`profile` | AWS SDK region / shared-config profile                     |

Schema: `period_start`, `period_end`, one column per group-by (`service`, …), one
column per metric (`unblended_cost`, …), `currency`.

```yaml
# turntable.yaml
sources:
  aws-cost:
    connector: awscost
    options:
      granularity: MONTHLY
      group_by: SERVICE
      # start: 2026-06-01   # else last 30 days
```

```bash
# top services this window
turntable -c turntable.yaml \
  "SELECT service, SUM(unblended_cost) AS cost
     FROM aws-cost GROUP BY service ORDER BY cost DESC LIMIT 10"

# daily spend by tag, zero config beyond the ref:
turntable "SELECT period_start, unblended_cost FROM awscost:costs ORDER BY period_start"
```

---

## Azure Cost Management (`azcost`)

Needs Cost Management **Cost Management Reader** (or Reader) on the scope. The
Query API is free (not billed).

| option         | meaning                                                         |
| -------------- | -------------------------------------------------------------- |
| `subscription` | subscription id → scope `/subscriptions/<id>`                  |
| `scope`        | full ARM scope (overrides `subscription`) — management group or billing account |
| `metric`       | aggregated column (default `Cost`; `PreTaxCost` for EA)        |
| `group_by`     | dimensions, comma-separated; `TAG:key` for a tag, e.g. `ServiceName`, `ResourceGroup`, `ResourceLocation`, `TAG:env` |
| `granularity`  | `None` (default) / `Daily`                                     |
| `timeframe`    | `MonthToDate` (default) / `TheLastMonth` / `WeekToDate` / … / `Custom` |
| `start`,`end`  | `YYYY-MM-DD` (sets `timeframe` to `Custom`)                    |
| `type`         | `ActualCost` (default) / `AmortizedCost` / `Usage`             |

Schema comes straight from the typed result columns (the aggregation alias — the
lower-cased metric, e.g. `cost` — plus your group-by dimensions, `Currency`, and
`UsageDate` for `Daily`).

```yaml
sources:
  azure-cost:
    connector: azcost
    options:
      subscription: 00000000-0000-0000-0000-000000000000
      group_by: ServiceName
      timeframe: TheLastMonth
```

```bash
turntable -c turntable.yaml \
  "SELECT ServiceName, cost FROM azure-cost ORDER BY cost DESC LIMIT 10"

# by resource group, daily:
#   options: {subscription: …, group_by: ResourceGroup, granularity: Daily}
```

## Notes

- **Option-driven, not SQL-aggregate pushdown.** The `GROUP BY` in your turntable
  query runs in-engine over the already-grouped rows; the *source-side* grouping
  is the `group_by` option. (So to group by service at the source, set
  `group_by: SERVICE`, then optionally `SUM()` across time buckets in SQL.)
- **Cross-cloud:** register both and `UNION`/compare, e.g. normalize each to
  `(cloud, service, cost)` and union for one spend view.
- **Cost Explorer group-by is capped at 2** (an AWS limit); Azure allows more.
- **Windows**: AWS `end` is exclusive; Azure defaults to `MonthToDate`. Use
  `start`/`end` for an explicit range on either.
