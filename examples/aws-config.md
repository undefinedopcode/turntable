# AWS Config connector (`awsconfig`)

Account/region resource inventory — every resource type AWS Config records
(EC2, Lambda, EKS, RDS, S3, …) — queried through Config's Advanced Query SQL
surface and exposed as turntable SQL. The AWS analogue of the Azure Resource
Graph connector.

Config's query language is already SQL-shaped, so this connector pushes
`WHERE` / `LIMIT` down as a Config `SELECT` over the well-known top-level
resource properties; nested `configuration` and `tags` come through as JSON you
index into, and a raw `query` option carries a full Config `SELECT` for
resource-type-specific paths.

## Prerequisite

**AWS Config must be enabled and recording** in the account/region you query
(a single-account recorder is enough; an aggregator covers many). If Config
isn't recording a resource type, it won't appear.

## Authentication

Ambient via the AWS SDK default chain — environment variables, a shared-config
`profile`, or an instance/role. Needs `config:SelectResourceConfig` (and
`config:SelectAggregateResourceConfig` for an aggregator). Set `region` (and
`profile`) as options.

## Options

| option       | meaning                                                            |
| ------------ | ----------------------------------------------------------------- |
| `region`     | AWS region (defaults to the environment/profile)                  |
| `profile`    | shared-config profile name                                        |
| `aggregator` | a Config **configuration aggregator** name → query across the accounts/regions it aggregates |
| `query`      | a raw Config `SELECT` expression (overrides table mode + pushdown)|
| `top`        | safety row cap per scan (default 5000)                             |

## Schema (table mode)

A fixed set of Config's top-level properties (Config-native names):

```
resourceId            string
resourceType          string   e.g. 'AWS::EC2::Instance'
resourceName          string
arn                   string
awsRegion             string
availabilityZone      string
accountId             string
resourceCreationTime  time
tags                  any      (JSON)
configuration         any      (JSON — the resource-type-specific config)
```

`resourceId`, `resourceType`, `resourceName`, `arn`, `awsRegion`,
`availabilityZone`, `accountId` push into the Config query with `=`, `IN`,
`LIKE`; everything else (including filters on `configuration`/`tags`) is applied
by the engine.

## Examples

Running EC2 instances by type (the `resourceType` filter pushes to Config; the
nested `configuration` filter runs in-engine):

```bash
turntable "SELECT resourceId, awsRegion
             FROM awsconfig:resources
             WHERE resourceType = 'AWS::EC2::Instance'"
```

A census across resource types:

```bash
turntable "SELECT resourceType, COUNT(*) AS n
             FROM awsconfig:resources
             GROUP BY resourceType ORDER BY n DESC"
```

Raw Config SQL for resource-type-specific fields (Config's dotted paths):

```yaml
# turntable.yaml
sources:
  ec2:
    connector: awsconfig
    options:
      region: us-east-1
      query: >
        SELECT resourceId, configuration.instanceType, configuration.state.name, tags
        WHERE resourceType = 'AWS::EC2::Instance'
```

```bash
turntable -c turntable.yaml \
  "SELECT * FROM ec2 WHERE state_name = 'running'"   # engine filters the flattened column
```

Multi-account via an aggregator:

```yaml
sources:
  org-inventory:
    connector: awsconfig
    options:
      aggregator: my-org-aggregator
```

## Notes

- **Pushdown is an optimization** — the engine re-applies the full
  `WHERE`/`LIMIT`, so a filter Config can't express (nested config, `<`/`>`,
  `<>`) still works, just fetched-then-filtered.
- Config paginates at 100 rows/page; the connector pages up to `top`.
- Nested `configuration` varies by `resourceType`; use a raw `query` with
  explicit paths when you need those fields as first-class columns, or index into
  the `configuration` JSON in table mode.
