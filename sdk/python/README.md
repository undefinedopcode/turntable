# ttplugin (Python)

The Python SDK for writing [turntable](https://github.com/undefinedopcode/turntable) plugin connectors — a
single stdlib-only module implementing the stdio JSON-RPC protocol from
[PLUGINS.md](https://github.com/undefinedopcode/turntable/blob/main/PLUGINS.md): framing, dispatch, scan cursors, predicate
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

See [`examples/plugins/pyfiles`](https://github.com/undefinedopcode/turntable/blob/main/examples/plugins/pyfiles/pyfiles.py)
for a complete reference plugin — it imports the module by path (a two-line
`sys.path` bootstrap); with this repository checked out or the package
installed, a plain `import ttplugin` works.

Published standalone at
[undefinedopcode/turntable-python-sdk](https://github.com/undefinedopcode/turntable-python-sdk)
(split from the turntable monorepo's `sdk/python` — develop and file issues
there). Sibling SDKs:
[Go](https://github.com/undefinedopcode/turntable-go-sdk),
[Node.js](https://github.com/undefinedopcode/turntable-node-sdk).
