"""ttplugin — the Python SDK for writing turntable plugin connectors.

A plugin is a standalone program that turntable launches as a subprocess and
drives over stdio with JSON-RPC 2.0 (see PLUGINS.md in the turntable repo for
the wire protocol). This module implements all of the protocol plumbing —
message framing, dispatch, scan cursors, predicate evaluation, and cell
encoding — so an author only declares datasets and a function that produces
rows:

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

Cells are plain Python values matching the column type — int, float, str,
bool, datetime.datetime (time), datetime.timedelta (duration), bytes, None
(NULL), or any JSON value for an "any" column; the SDK encodes them to the
wire form (RFC3339 times, base64 bytes, nanosecond durations; NaN/inf floats
become NULL).

By default the SDK applies the pushed-down WHERE and LIMIT to the rows you
return, so a plugin gets predicate/limit pushdown for free. Set
Plugin(manual_pushdown=True) to take over (e.g. to push filters into a remote
backend); the Request still carries the decoded predicate dict, and
eval_predicate() is exported so you can reuse the evaluator.

Standard library only. Write diagnostics to stderr — stdout carries protocol
messages only.
"""

from __future__ import annotations

import base64
import datetime as _dt
import json
import math
import re
import sys
from dataclasses import dataclass, field
from typing import Any, Callable, Dict, List, Optional

PROTOCOL_VERSION = 1

__all__ = [
    "Column",
    "Dataset",
    "Plugin",
    "Request",
    "serve",
    "eval_predicate",
]


@dataclass
class Column:
    """One column: type is int, float, string, bool, time, duration, bytes or any."""

    name: str
    type: str = "any"
    nullable: bool = False


@dataclass
class Request:
    """The pushed-down scan hints handed to a dataset's rows function. In
    automatic mode you may ignore predicate/limit (the SDK applies them)."""

    dataset: str
    columns: List[str] = field(default_factory=list)
    limit: Optional[int] = None
    predicate: Optional[dict] = None
    options: Dict[str, Any] = field(default_factory=dict)


@dataclass
class Dataset:
    """One queryable relation: its columns and a function producing rows
    (each row a positional list aligned to the columns)."""

    columns: List[Column]
    rows: Callable[[Request], List[list]]


@dataclass
class Plugin:
    """A whole connector: a name (the advertised qualified-ref prefix) and its
    datasets. manual_pushdown=True disables the SDK's automatic WHERE/LIMIT
    filtering — set it only when your rows function applies them itself."""

    name: str
    datasets: Dict[str, Dataset]
    manual_pushdown: bool = False


# ---- predicate evaluation ----------------------------------------------------


def eval_predicate(pred: Optional[dict], get: Callable[[str], Any]) -> bool:
    """Report whether a row satisfies a pushdown predicate tree (the JSON
    subset in PLUGINS.md). `get` returns a column's value by name (None for
    NULL/unknown). Exported for manual-pushdown plugins."""
    if not pred:
        return True
    kind = pred.get("kind")
    if kind == "and":
        return all(eval_predicate(a, get) for a in pred.get("args", []))
    if kind == "or":
        return any(eval_predicate(a, get) for a in pred.get("args", []))
    if kind == "not":
        arg = pred.get("arg")
        return arg is None or not eval_predicate(arg, get)
    if kind == "isnull":
        return (get(pred.get("column", "")) is None) != bool(pred.get("negate"))
    if kind == "in":
        v = get(pred.get("column", ""))
        hit = any(_compare(v, "=", lit) for lit in pred.get("values", []))
        return hit != bool(pred.get("negate"))
    if kind == "between":
        v = get(pred.get("column", ""))
        low, high = pred.get("low"), pred.get("high")
        if low is None or high is None:
            return False
        inside = _compare(v, ">=", low) and _compare(v, "<=", high)
        return inside != bool(pred.get("negate"))
    if kind == "like":
        v = get(pred.get("column", ""))
        if v is None:
            return False
        s = v if isinstance(v, str) else str(v)
        hit = _like(s, pred.get("pattern", ""), bool(pred.get("insensitive")))
        return hit != bool(pred.get("negate"))
    if kind == "compare":
        lit = pred.get("value")
        if lit is None:
            return False
        return _compare(get(pred.get("column", "")), pred.get("op", "="), lit)
    return False


def _compare(v: Any, op: str, lit: dict) -> bool:
    """`cellValue OP literal`. NULL compares false to everything. Numbers
    compare numerically, datetimes against a parseable string literal,
    everything else as strings — mirroring the Go SDK."""
    if v is None or lit.get("type") == "null":
        return False
    lv = lit.get("value")
    if isinstance(v, _dt.datetime) and isinstance(lv, str):
        lt = _parse_time(lv)
        if lt is not None:
            return _cmp(_ts(v), _ts(lt), op)
    t = lit.get("type")
    if t in ("int", "float"):
        a, b = _to_float(v), _to_float(lv)
        if a is None or b is None:
            return False
        return _cmp(a, b, op)
    if t == "bool":
        if not isinstance(v, bool) or not isinstance(lv, bool):
            return False
        if op == "=":
            return v is lv
        if op == "<>":
            return v is not lv
        return False
    return _cmp(str(v), str(lv), op)


def _to_float(v: Any) -> Optional[float]:
    if isinstance(v, bool):  # bool is an int subclass; Go's toFloat rejects it
        return None
    if isinstance(v, (int, float)):
        return float(v)
    if isinstance(v, str):
        try:
            return float(v)
        except ValueError:
            return None
    return None


def _cmp(a, b, op: str) -> bool:
    if op == "=":
        return a == b
    if op == "<>":
        return a != b
    if op == "<":
        return a < b
    if op == "<=":
        return a <= b
    if op == ">":
        return a > b
    if op == ">=":
        return a >= b
    return False


def _like(s: str, pattern: str, insensitive: bool) -> bool:
    expr = "^" + "".join(
        ".*" if ch == "%" else "." if ch == "_" else re.escape(ch) for ch in pattern
    ) + "$"
    return re.search(expr, s, re.IGNORECASE if insensitive else 0) is not None


def _ts(t: _dt.datetime) -> float:
    if t.tzinfo is None:
        t = t.replace(tzinfo=_dt.timezone.utc)
    return t.timestamp()


def _parse_time(s: str) -> Optional[_dt.datetime]:
    for parse in (
        lambda x: _dt.datetime.fromisoformat(x.replace("Z", "+00:00")),
        lambda x: _dt.datetime.strptime(x, "%Y-%m-%d %H:%M:%S"),
        lambda x: _dt.datetime.strptime(x, "%Y-%m-%d"),
    ):
        try:
            return parse(s)
        except ValueError:
            continue
    return None


# ---- cell encoding -------------------------------------------------------------


def _encode_cell(v: Any, typ: str) -> Any:
    if v is None:
        return None
    if typ == "time":
        if isinstance(v, _dt.datetime):
            if v.tzinfo is None:
                v = v.replace(tzinfo=_dt.timezone.utc)
            return v.astimezone(_dt.timezone.utc).isoformat().replace("+00:00", "Z")
        if isinstance(v, _dt.date):
            return v.isoformat() + "T00:00:00Z"
        return v
    if typ == "duration":
        if isinstance(v, _dt.timedelta):
            return int(v.total_seconds() * 1e9)  # integer nanoseconds
        return v
    if typ == "bytes":
        if isinstance(v, (bytes, bytearray)):
            return base64.b64encode(bytes(v)).decode("ascii")
        return v
    if isinstance(v, float) and (math.isnan(v) or math.isinf(v)):
        return None  # JSON has no NaN/inf; NULL is the honest encoding
    return v


def _encode_row(row: list, columns: List[Column]) -> list:
    out = []
    for i, col in enumerate(columns):
        out.append(_encode_cell(row[i], col.type) if i < len(row) else None)
    return out


def _wire_schema(columns: List[Column]) -> dict:
    return {
        "columns": [
            {"name": c.name, "type": c.type or "any", "nullable": c.nullable}
            for c in columns
        ]
    }


# ---- server --------------------------------------------------------------------


class _Server:
    def __init__(self, plugin: Plugin):
        self.plugin = plugin
        self.scans: Dict[str, dict] = {}
        self.next_id = 0

    def dispatch(self, method: str, params: dict) -> dict:
        if method == "initialize":
            return {
                "protocolVersion": PROTOCOL_VERSION,
                "name": self.plugin.name,
                # The SDK implements predicate + limit pushdown itself (or the
                # author does, in manual mode), so advertise them either way.
                "capabilities": {"predicatePushdown": True, "limitPushdown": True},
                "datasets": [{"name": n} for n in self.plugin.datasets],
            }
        if method == "datasets":
            return {"datasets": [{"name": n} for n in self.plugin.datasets]}
        if method == "resolve":
            ds = self._dataset(params)
            return {"schema": _wire_schema(ds.columns)}
        if method == "scan":
            return self._scan(params)
        if method == "next":
            return self._next(params)
        if method == "close":
            self.scans.pop(params.get("scanId", ""), None)
            return {}
        raise ValueError(f"unknown method {method!r}")

    def _dataset(self, params: dict) -> Dataset:
        name = (params.get("dataset") or {}).get("name", "")
        ds = self.plugin.datasets.get(name)
        if ds is None:
            raise ValueError(f"unknown dataset {name!r}")
        return ds

    def _scan(self, params: dict) -> dict:
        ds = self._dataset(params)
        wire_ds = params.get("dataset") or {}
        # Turntable sends the source's options on the dataset itself; merge a
        # top-level options object (if a host ever sends one) underneath.
        options = dict(params.get("options") or {})
        options.update(wire_ds.get("options") or {})
        req = Request(
            dataset=wire_ds.get("name", ""),
            columns=params.get("columns") or [],
            limit=params.get("limit"),
            predicate=params.get("predicate"),
            options=options,
        )
        rows = list(ds.rows(req))

        applied = {}
        if not self.plugin.manual_pushdown:
            if req.predicate is not None:
                idx = {c.name: i for i, c in enumerate(ds.columns)}

                def getter(row):
                    return lambda col: row[idx[col]] if col in idx and idx[col] < len(row) else None

                rows = [r for r in rows if eval_predicate(req.predicate, getter(r))]
                applied["predicate"] = True
            # Limit is safe only because the predicate was fully evaluated.
            if req.limit is not None and req.limit < len(rows):
                rows = rows[: req.limit]
                applied["limit"] = True

        self.next_id += 1
        scan_id = str(self.next_id)
        self.scans[scan_id] = {"rows": rows, "columns": ds.columns, "pos": 0}
        return {"scanId": scan_id, "schema": _wire_schema(ds.columns), "applied": applied}

    def _next(self, params: dict) -> dict:
        cur = self.scans.get(params.get("scanId", ""))
        if cur is None:
            raise ValueError(f"unknown scanId {params.get('scanId')!r}")
        max_rows = params.get("maxRows") or 1000
        if max_rows <= 0:
            max_rows = 1000
        end = min(cur["pos"] + max_rows, len(cur["rows"]))
        batch = [_encode_row(r, cur["columns"]) for r in cur["rows"][cur["pos"] : end]]
        cur["pos"] = end
        return {"rows": batch, "done": cur["pos"] >= len(cur["rows"])}


def _read_message(stream) -> Optional[bytes]:
    """Read one Content-Length framed message; None on EOF."""
    length = -1
    while True:
        line = stream.readline()
        if not line:
            return None
        line = line.rstrip(b"\r\n")
        if line == b"":
            break
        name, _, val = line.partition(b":")
        if name.strip().lower() == b"content-length":
            try:
                length = int(val.strip())
            except ValueError:
                pass
    if length < 0:
        return None
    buf = stream.read(length)
    if buf is None or len(buf) < length:
        return None
    return buf


def _write_message(stream, payload: bytes) -> None:
    stream.write(f"Content-Length: {len(payload)}\r\n\r\n".encode("ascii"))
    stream.write(payload)
    stream.flush()


def serve(plugin: Plugin, stdin=None, stdout=None) -> None:
    """Run the plugin over stdin/stdout until shutdown or EOF. The normal entry
    point from a plugin's main. Streams are overridable for testing."""
    inp = stdin if stdin is not None else sys.stdin.buffer
    out = stdout if stdout is not None else sys.stdout.buffer
    server = _Server(plugin)
    while True:
        payload = _read_message(inp)
        if payload is None:
            return
        try:
            msg = json.loads(payload)
        except ValueError:
            continue
        if msg.get("method") == "shutdown":
            return
        if msg.get("id") is None:
            continue  # unknown notification
        resp: Dict[str, Any] = {"jsonrpc": "2.0", "id": msg["id"]}
        try:
            resp["result"] = server.dispatch(msg.get("method", ""), msg.get("params") or {})
        except Exception as e:  # surface as a JSON-RPC error, keep serving
            resp["error"] = {"code": -32000, "message": str(e)}
        _write_message(out, json.dumps(resp, allow_nan=False).encode("utf-8"))
