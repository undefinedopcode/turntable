# Kubernetes plugin (`k8s`)

Query a Kubernetes cluster's resources — pods, deployments, nodes, services,
events, and any other kind/CRD — as SQL. This is a **plugin**, not a built-in
connector: it uses `client-go`, a large dependency, which is kept out of
turntable's own dependency graph by living in its own module (the same way the
`procinfo` plugin isolates gopsutil).

Because it uses `client-go`, it authenticates with **whatever your kubeconfig
provides — including AKS/EKS exec credential plugins** (`kubelogin` / `az` for
AKS, `aws eks get-token` for EKS). If `kubectl` works, this works.

## Build & register

It's a separate Go module, so it isn't built by `go build ./...`:

```bash
./examples/plugins/build.sh            # builds bin/k8s (+ the other example plugins)
```

```yaml
# turntable.yaml
sources:
  k8s:
    connector: plugin
    command: ["/abs/path/to/bin/k8s"]
    # options: {context: my-aks, namespace: prod}   # optional
```

```bash
turntable -c turntable.yaml "SELECT name, namespace, phase, restarts FROM k8s:pods"
```

(`connector: plugin` with a `command:` runs an arbitrary binary, so plugins are
configured via the file / `.use`, not the web add-source UI.)

## Options

| option       | meaning                                                          |
| ------------ | ---------------------------------------------------------------- |
| `context`    | kubeconfig context (default: current-context) — pick the cluster |
| `kubeconfig` | path to a kubeconfig file (default: `$KUBECONFIG` or `~/.kube/config`) |
| `namespace`  | scope namespaced kinds to one namespace (default: all)           |
| `resource`   | (generic dataset) the resource name, plural, e.g. `configmaps`   |
| `group`      | (generic dataset) API group (default: core `""`)                 |
| `version`    | (generic dataset) API version (default: `v1`)                    |

## Datasets

Flattened views of the common kinds (bare qualified refs like `k8s:pods` work
with your default kubeconfig):

| dataset        | columns |
| -------------- | ------- |
| `pods`         | name, namespace, node, phase, ready, restarts, ip, image, created |
| `deployments`  | name, namespace, desired, ready, updated, available, created |
| `statefulsets` | name, namespace, desired, ready, updated, available, created |
| `daemonsets`   | name, namespace, desired, ready, available, created |
| `nodes`        | name, status, roles, version, os_image, cpu, memory, unschedulable, created |
| `services`     | name, namespace, type, cluster_ip, external_ip, ports, created |
| `namespaces`   | name, status, created |
| `events`       | namespace, type, reason, object, message, count, last_seen |

Plus a generic **`resource`** dataset for any other kind or CRD — returns
`name, namespace, apiVersion, kind, created` plus `metadata`, `spec`, `status`
as JSON to index into. Select the kind with options:

```yaml
sources:
  configmaps:
    connector: plugin
    command: ["/abs/path/to/bin/k8s"]
    options: {dataset: resource, resource: configmaps}
  # a CRD:
  rollouts:
    connector: plugin
    command: ["/abs/path/to/bin/k8s"]
    options: {dataset: resource, resource: rollouts, group: argoproj.io, version: v1alpha1}
```

## Examples

Pods not running, cluster-wide:

```sql
SELECT namespace, name, node, phase, restarts
  FROM k8s:pods WHERE phase <> 'Running' ORDER BY restarts DESC
```

Nodes and capacity:

```sql
SELECT name, status, version, cpu, memory FROM k8s:nodes
```

Recent warning events:

```sql
SELECT namespace, object, reason, message, count
  FROM k8s:events WHERE type = 'Warning' ORDER BY last_seen DESC
```

Deployments that aren't fully available (join-friendly with the infra inventory
from the `awsconfig` / `azrgraph` connectors):

```sql
SELECT namespace, name, desired, ready FROM k8s:deployments WHERE ready < desired
```

## Notes

- **Pushdown**: the SDK applies the `WHERE`/`LIMIT` to the rows the plugin
  returns, so filtering works, but the plugin lists the full kind first (no
  server-side field/label-selector pushdown yet). For very large clusters, scope
  with `namespace`.
- **Auth troubleshooting**: if a query errors with a credential/exec message,
  confirm `kubectl --context <ctx> get pods` works in the same shell — the plugin
  uses the same kubeconfig and exec plugins.
- **Multiple clusters**: register one source per context (each with its own
  `context` option); clients are built and cached per kubeconfig+context.
