# Azure Resource Graph connector (`azrgraph`)

Query your entire Azure fleet — AKS clusters, Functions, VMs, NICs, tags,
across every subscription — through Resource Graph's KQL endpoint, as SQL.

Resource Graph is a KQL engine, so this connector pushes `WHERE` / `ORDER BY` /
`LIMIT` down as KQL (like `athena` does with SQL); a raw `query` option carries a
full KQL string for anything the translator can't express. Rows are
semi-structured, so the schema is **inferred from a sample** — scalar columns
(`id`, `name`, `type`, `location`, `resourceGroup`, `subscriptionId`, …) get
real types; nested `tags` / `properties` / `sku` come through as JSON you index
into with the dialect's `->`/`.` paths.

## Authentication

Ambient Azure AD via `DefaultAzureCredential` — no key options (env vars, a
managed identity, or your `az login` session). The identity needs the **Reader**
role on the subscriptions/management groups you query.

## Options

| option          | meaning                                                          |
| --------------- | ---------------------------------------------------------------- |
| `table`         | Resource Graph table (default `Resources`; also `ResourceContainers`, `ResourceChanges`, …). Also settable via the ref: `azrgraph:Resources`. |
| `subscriptions` | comma-separated subscription IDs to scope to (default: all accessible) |
| `query`         | a raw Resource Graph KQL query (overrides `table` and pushdown)   |
| `top`           | safety row cap per scan (default 5000)                            |

## Examples

Inventory by type and region (no config file needed — Azure AD is ambient):

```bash
turntable "SELECT type, location, COUNT(*) AS n
             FROM azrgraph:Resources
             GROUP BY type, location
             ORDER BY n DESC"
```

Find AKS clusters, filtered at the source (the `WHERE` and `LIMIT` push into
KQL — check `--explain`):

```bash
turntable "SELECT name, resourceGroup, location
             FROM azrgraph:Resources
             WHERE type = 'microsoft.containerservice/managedclusters'
             ORDER BY name"
```

A configured source scoped to two subscriptions:

```yaml
# turntable.yaml
sources:
  fleet:
    connector: azrgraph
    options:
      table: Resources
      subscriptions: 00000000-0000-0000-0000-000000000001, 00000000-0000-0000-0000-000000000002
```

```bash
# untagged VMs
turntable -c turntable.yaml \
  "SELECT name, resourceGroup FROM fleet
     WHERE type = 'microsoft.compute/virtualmachines' AND tags IS NULL"
```

Raw KQL for anything the translator doesn't cover (joins across RG tables,
`mv-expand`, dynamic access, etc.):

```yaml
sources:
  aks-versions:
    connector: azrgraph
    options:
      query: >
        Resources
        | where type == 'microsoft.containerservice/managedclusters'
        | project name, kubernetesVersion = properties.kubernetesVersion, location
```

## Notes

- **Pushdown is an optimization.** The engine always re-applies the full
  `WHERE`/`ORDER BY`/`LIMIT`, so predicates the translator can't express (e.g. on
  nested `tags.env`) still work — they just run in-engine over the fetched rows.
  Push-eligible: `=`, `<>`, `<`,`<=`,`>`,`>=`, `IN`, `LIKE '%x%'` (→ `contains`),
  `IS [NOT] NULL`, on top-level columns.
- **Sampling.** The schema comes from the first ~32 rows; a column that only
  appears on rare resource types may be missed. Use a raw `query` with an
  explicit `project` when you need a guaranteed column set.
- **Fleet + metrics.** Resource Graph gives you the resource `id`s; pair it with
  the `azmetrics` connector (one metric per resource) to go from inventory to
  telemetry. Single-query fleet metrics awaits the Metrics Batch API (see
  `docs/azure-monitor-design.md`).
