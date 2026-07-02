# ttplugin (Python)

The Python SDK for writing [turntable](../../README.md) plugin connectors — a
single stdlib-only module implementing the stdio JSON-RPC protocol from
[PLUGINS.md](../../PLUGINS.md): framing, dispatch, scan cursors, predicate
evaluation, and cell encoding. You declare datasets and a rows function:

```python
import os
import ttplugin

ttplugin.serve(ttplugin.Plugin(
    name="envinfo",
    datasets={
        "env": ttplugin.Dataset(
            columns=[
                ttplugin.Column("name", "string"),
                ttplugin.Column("value", "string", nullable=True),
            ],
            rows=lambda req: [[k, v] for k, v in os.environ.items()],
        ),
    },
))
```

Register the script as a plugin source (no build step):

```yaml
sources:
  envinfo:
    connector: plugin
    command: ["python3", "./envinfo.py"]
    options: { dataset: "*" }
```

- Cells are plain Python values (int, float, str, bool, `datetime` for time
  columns, `timedelta` for durations, `bytes`, `None` for NULL).
- The SDK applies the pushed-down `WHERE`/`LIMIT` to the rows you return, so
  you get pushdown for free; pass `manual_pushdown=True` to handle them
  yourself (the `Request` carries the decoded predicate and
  `eval_predicate()` is exported).
- stdout carries protocol messages only — log to stderr.

See [`examples/plugins/pyfiles`](../../examples/plugins/pyfiles/pyfiles.py)
for a complete reference plugin. This directory is intended to graduate into
its own repository/package eventually; until then, import it by path (the
reference plugin shows the two-line `sys.path` bootstrap).
