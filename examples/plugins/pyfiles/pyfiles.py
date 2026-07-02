#!/usr/bin/env python3
"""pyfiles — a reference turntable plugin connector in Python.

Exposes a directory tree as a queryable relation, one row per entry — query
your filesystem with SQL:

    SELECT ext, COUNT(*) AS n, SUM(size) AS bytes
    FROM files WHERE NOT is_dir GROUP BY ext ORDER BY bytes DESC

Datasets:
    files   path, name, ext, dir, size (bytes), modified (time), is_dir

Options:
    root         directory to walk (default ".")
    max_entries  cap on rows (default 10000) — the walk stops there
    hidden       "true" to include dot-files/dirs (default: skipped)

Register it (no build step — see PLUGINS.md):

    sources:
      files:
        connector: plugin
        command: ["python3", "./examples/plugins/pyfiles/pyfiles.py"]
        options:
          dataset: files
          root: "."

The SDK (sdk/python/ttplugin.py, stdlib-only) does all protocol work; while in
this repo it is imported by path — a published package would be a plain
`import ttplugin`.
"""

import datetime
import os
import sys

sys.path.insert(
    0, os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", "..", "..", "sdk", "python")
)
import ttplugin  # noqa: E402


def walk(req: ttplugin.Request):
    root = req.options.get("root") or "."
    max_entries = int(req.options.get("max_entries") or 10000)
    hidden = str(req.options.get("hidden") or "").lower() == "true"

    rows = []
    for dirpath, dirnames, filenames in os.walk(root):
        if not hidden:
            dirnames[:] = [d for d in dirnames if not d.startswith(".")]
            filenames = [f for f in filenames if not f.startswith(".")]
        for name in dirnames + filenames:
            full = os.path.join(dirpath, name)
            try:
                st = os.stat(full)
            except OSError:
                continue
            is_dir = name in dirnames
            _, ext = os.path.splitext(name)
            rows.append([
                full,
                name,
                ext.lstrip(".").lower() if not is_dir else None,
                dirpath,
                int(st.st_size),
                datetime.datetime.fromtimestamp(st.st_mtime, datetime.timezone.utc),
                is_dir,
            ])
            if len(rows) >= max_entries:
                print(f"pyfiles: capped at {max_entries} entries", file=sys.stderr)
                return rows
    return rows


ttplugin.serve(ttplugin.Plugin(
    name="pyfiles",
    datasets={
        "files": ttplugin.Dataset(
            columns=[
                ttplugin.Column("path", "string"),
                ttplugin.Column("name", "string"),
                ttplugin.Column("ext", "string", nullable=True),
                ttplugin.Column("dir", "string"),
                ttplugin.Column("size", "int"),
                ttplugin.Column("modified", "time"),
                ttplugin.Column("is_dir", "bool"),
            ],
            rows=walk,
        ),
    },
))
