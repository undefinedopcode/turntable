// ttplugin — the Node.js SDK for writing turntable plugin connectors.
//
// A plugin is a standalone program that turntable launches as a subprocess and
// drives over stdio with JSON-RPC 2.0 (see PLUGINS.md in the turntable repo for
// the wire protocol). This module implements all of the protocol plumbing —
// message framing, dispatch, scan cursors, predicate evaluation, and cell
// encoding — so an author only declares datasets and a function that produces
// rows:
//
//   import { serve } from "ttplugin";   // or a relative path to this file
//
//   serve({
//     name: "osinfo",
//     datasets: {
//       cpus: {
//         columns: [
//           { name: "model", type: "string" },
//           { name: "speed_mhz", type: "int" },
//         ],
//         rows: (req) => os.cpus().map((c) => [c.model, c.speed]),
//       },
//     },
//   });
//
// Cells are plain JS values matching the column type — number, string, boolean,
// Date (time columns), Buffer/Uint8Array (bytes), null (NULL), or any JSON
// value for an "any" column; the SDK encodes them to the wire form (RFC3339
// times, base64 bytes; NaN/Infinity become NULL). A rows function may be async.
//
// By default the SDK applies the pushed-down WHERE and LIMIT to the rows you
// return, so a plugin gets predicate/limit pushdown for free. Set
// { manualPushdown: true } to take over; the request still carries the decoded
// predicate, and evalPredicate() is exported so you can reuse the evaluator.
//
// No dependencies. Write diagnostics to stderr (console.error) — stdout carries
// protocol messages only.

export const PROTOCOL_VERSION = 1;

// ---- predicate evaluation ----------------------------------------------------

// evalPredicate reports whether a row satisfies a pushdown predicate tree (the
// JSON subset in PLUGINS.md). `get` returns a column's value by name (null for
// NULL/unknown). Exported for manual-pushdown plugins.
export function evalPredicate(pred, get) {
  if (!pred) return true;
  switch (pred.kind) {
    case "and":
      return (pred.args ?? []).every((a) => evalPredicate(a, get));
    case "or":
      return (pred.args ?? []).some((a) => evalPredicate(a, get));
    case "not":
      return !pred.arg || !evalPredicate(pred.arg, get);
    case "isnull":
      return (get(pred.column) == null) !== !!pred.negate;
    case "in": {
      const v = get(pred.column);
      const hit = (pred.values ?? []).some((lit) => compare(v, "=", lit));
      return hit !== !!pred.negate;
    }
    case "between": {
      const v = get(pred.column);
      if (!pred.low || !pred.high) return false;
      const inside = compare(v, ">=", pred.low) && compare(v, "<=", pred.high);
      return inside !== !!pred.negate;
    }
    case "like": {
      const v = get(pred.column);
      if (v == null) return false;
      const s = typeof v === "string" ? v : String(v);
      return likeMatch(s, pred.pattern ?? "", !!pred.insensitive) !== !!pred.negate;
    }
    case "compare":
      if (!pred.value) return false;
      return compare(get(pred.column), pred.op ?? "=", pred.value);
  }
  return false;
}

// compare evaluates `cellValue OP literal`. NULL compares false to everything.
// Numbers compare numerically, Dates against a parseable string literal,
// everything else as strings — mirroring the Go SDK.
function compare(v, op, lit) {
  if (v == null || lit.type === "null") return false;
  if (v instanceof Date && typeof lit.value === "string") {
    const lt = Date.parse(lit.value);
    if (!Number.isNaN(lt)) return numCmp(v.getTime(), lt, op);
  }
  if (lit.type === "int" || lit.type === "float") {
    const a = toNumber(v);
    const b = toNumber(lit.value);
    if (a == null || b == null) return false;
    return numCmp(a, b, op);
  }
  if (lit.type === "bool") {
    if (typeof v !== "boolean" || typeof lit.value !== "boolean") return false;
    if (op === "=") return v === lit.value;
    if (op === "<>") return v !== lit.value;
    return false;
  }
  return strCmp(String(v), String(lit.value), op);
}

function toNumber(v) {
  if (typeof v === "boolean") return null; // mirror Go's toFloat
  if (typeof v === "number") return v;
  if (typeof v === "string") {
    const f = Number(v);
    return v.trim() !== "" && Number.isFinite(f) ? f : null;
  }
  return null;
}

function numCmp(a, b, op) {
  switch (op) {
    case "=": return a === b;
    case "<>": return a !== b;
    case "<": return a < b;
    case "<=": return a <= b;
    case ">": return a > b;
    case ">=": return a >= b;
  }
  return false;
}

function strCmp(a, b, op) {
  switch (op) {
    case "=": return a === b;
    case "<>": return a !== b;
    case "<": return a < b;
    case "<=": return a <= b;
    case ">": return a > b;
    case ">=": return a >= b;
  }
  return false;
}

// likeMatch implements SQL LIKE: % matches any run, _ one character.
function likeMatch(s, pattern, insensitive) {
  let expr = "^";
  for (const ch of pattern) {
    if (ch === "%") expr += ".*";
    else if (ch === "_") expr += ".";
    else expr += ch.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
  }
  expr += "$";
  try {
    return new RegExp(expr, insensitive ? "i" : "").test(s);
  } catch {
    return false;
  }
}

// ---- cell encoding -------------------------------------------------------------

function encodeCell(v, type) {
  if (v == null) return null;
  if (type === "time") {
    if (v instanceof Date) return v.toISOString();
    return v;
  }
  if (type === "bytes") {
    if (v instanceof Uint8Array) return Buffer.from(v).toString("base64");
    return v;
  }
  if (typeof v === "number" && !Number.isFinite(v)) {
    return null; // JSON has no NaN/Infinity; NULL is the honest encoding
  }
  if (typeof v === "bigint") {
    return Number(v);
  }
  return v;
}

function encodeRow(row, columns) {
  return columns.map((c, i) => (i < row.length ? encodeCell(row[i], c.type) : null));
}

function wireSchema(columns) {
  return {
    columns: columns.map((c) => ({
      name: c.name,
      type: c.type || "any",
      nullable: !!c.nullable,
    })),
  };
}

// ---- server --------------------------------------------------------------------

class Server {
  constructor(plugin) {
    this.plugin = plugin;
    this.scans = new Map();
    this.nextId = 0;
  }

  async dispatch(method, params) {
    switch (method) {
      case "initialize":
        return {
          protocolVersion: PROTOCOL_VERSION,
          name: this.plugin.name,
          // The SDK implements predicate + limit pushdown itself (or the
          // author does, in manual mode), so advertise them either way.
          capabilities: { predicatePushdown: true, limitPushdown: true },
          datasets: Object.keys(this.plugin.datasets).map((name) => ({ name })),
        };
      case "datasets":
        return { datasets: Object.keys(this.plugin.datasets).map((name) => ({ name })) };
      case "resolve":
        return { schema: wireSchema(this.dataset(params).columns) };
      case "scan":
        return this.scan(params);
      case "next":
        return this.next(params);
      case "close":
        this.scans.delete(params?.scanId ?? "");
        return {};
    }
    throw new Error(`unknown method ${JSON.stringify(method)}`);
  }

  dataset(params) {
    const name = params?.dataset?.name ?? "";
    const ds = this.plugin.datasets[name];
    if (!ds) throw new Error(`unknown dataset ${JSON.stringify(name)}`);
    return ds;
  }

  async scan(params) {
    const ds = this.dataset(params);
    // Turntable sends the source's options on the dataset itself; a top-level
    // options object (if a host ever sends one) is merged underneath.
    const options = { ...(params?.options ?? {}), ...(params?.dataset?.options ?? {}) };
    const req = {
      dataset: params?.dataset?.name ?? "",
      columns: params?.columns ?? [],
      limit: params?.limit ?? null,
      predicate: params?.predicate ?? null,
      options,
    };
    let rows = await ds.rows(req);
    rows = Array.isArray(rows) ? rows : [];

    const applied = {};
    if (!this.plugin.manualPushdown) {
      if (req.predicate) {
        const idx = new Map(ds.columns.map((c, i) => [c.name, i]));
        rows = rows.filter((row) =>
          evalPredicate(req.predicate, (col) => {
            const i = idx.get(col);
            return i != null && i < row.length ? row[i] : null;
          }),
        );
        applied.predicate = true;
      }
      // Limit is safe only because the predicate was fully evaluated.
      if (req.limit != null && req.limit < rows.length) {
        rows = rows.slice(0, req.limit);
        applied.limit = true;
      }
    }

    const scanId = String(++this.nextId);
    this.scans.set(scanId, { rows, columns: ds.columns, pos: 0 });
    return { scanId, schema: wireSchema(ds.columns), applied };
  }

  next(params) {
    const cur = this.scans.get(params?.scanId ?? "");
    if (!cur) throw new Error(`unknown scanId ${JSON.stringify(params?.scanId)}`);
    let max = params?.maxRows ?? 1000;
    if (!(max > 0)) max = 1000;
    const end = Math.min(cur.pos + max, cur.rows.length);
    const batch = cur.rows.slice(cur.pos, end).map((r) => encodeRow(r, cur.columns));
    cur.pos = end;
    return { rows: batch, done: cur.pos >= cur.rows.length };
  }
}

// serve runs the plugin over the process's stdin/stdout until shutdown or EOF —
// the normal entry point from a plugin's main module. Returns a promise that
// resolves when the host disconnects.
export function serve(plugin, stdin = process.stdin, stdout = process.stdout) {
  const server = new Server(plugin);
  let buf = Buffer.alloc(0);
  let processing = Promise.resolve();
  let finish;
  const done = new Promise((r) => (finish = r));

  const pump = () => {
    for (;;) {
      const sep = buf.indexOf("\r\n\r\n");
      if (sep < 0) return;
      const header = buf.subarray(0, sep).toString("ascii");
      const m = /content-length:\s*(\d+)/i.exec(header);
      if (!m) {
        buf = buf.subarray(sep + 4);
        continue;
      }
      const length = Number(m[1]);
      if (buf.length < sep + 4 + length) return; // wait for the full payload
      const payload = buf.subarray(sep + 4, sep + 4 + length);
      buf = buf.subarray(sep + 4 + length);
      handle(payload);
    }
  };

  const handle = (payload) => {
    let msg;
    try {
      msg = JSON.parse(payload.toString("utf-8"));
    } catch {
      return;
    }
    if (msg.method === "shutdown") {
      finish();
      process.exit(0);
    }
    if (msg.id == null) return; // unknown notification
    // Chain responses so they are written in request order even if a rows
    // function is async.
    processing = processing.then(async () => {
      const resp = { jsonrpc: "2.0", id: msg.id };
      try {
        resp.result = await server.dispatch(msg.method ?? "", msg.params ?? {});
      } catch (e) {
        resp.error = { code: -32000, message: e instanceof Error ? e.message : String(e) };
      }
      const body = Buffer.from(JSON.stringify(resp), "utf-8");
      stdout.write(`Content-Length: ${body.length}\r\n\r\n`);
      stdout.write(body);
    });
  };

  stdin.on("data", (chunk) => {
    buf = Buffer.concat([buf, chunk]);
    pump();
  });
  stdin.on("end", () => finish());
  return done;
}
