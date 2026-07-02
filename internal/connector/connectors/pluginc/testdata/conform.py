#!/usr/bin/env python3
"""Conformance fixture for the Python plugin SDK, driven by sdkconform_test.go
through the real pluginc connector. Fixed rows exercising the cell types,
NULLs, and the SDK's automatic predicate/limit application."""

import datetime
import os
import sys

sys.path.insert(
    0,
    os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", "..", "..", "..", "..", "sdk", "python"),
)
import ttplugin  # noqa: E402

T1 = datetime.datetime(2026, 1, 1, 10, 0, 0, tzinfo=datetime.timezone.utc)
T2 = datetime.datetime(2026, 1, 2, 10, 0, 0, tzinfo=datetime.timezone.utc)

ROWS = [
    [1, 1.5, "alpha", True, T1, b"hi"],
    [2, 2.5, "beta", False, T2, None],
    [3, None, "aloe", True, None, b"yo"],
]

ttplugin.serve(ttplugin.Plugin(
    name="conform",
    datasets={
        "vals": ttplugin.Dataset(
            columns=[
                ttplugin.Column("i", "int"),
                ttplugin.Column("f", "float", nullable=True),
                ttplugin.Column("s", "string"),
                ttplugin.Column("b", "bool"),
                ttplugin.Column("t", "time", nullable=True),
                ttplugin.Column("by", "bytes", nullable=True),
            ],
            rows=lambda req: ROWS,
        ),
    },
))
