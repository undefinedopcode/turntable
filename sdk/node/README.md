# ttplugin (Node.js)

The Node.js SDK for writing [turntable](../../README.md) plugin connectors — a
single dependency-free ES module implementing the stdio JSON-RPC protocol from
[PLUGINS.md](../../PLUGINS.md): framing, dispatch, scan cursors, predicate
evaluation, and cell encoding. You declare datasets and a rows function (sync
or async):

```js
import os from "node:os";
import { serve } from "ttplugin"; // or a relative path to ttplugin.js

serve({
  name: "osinfo",
  datasets: {
    cpus: {
      columns: [
        { name: "model", type: "string" },
        { name: "speed_mhz", type: "int" },
      ],
      rows: () => os.cpus().map((c) => [c.model, c.speed]),
    },
  },
});
```

Register the script as a plugin source (no build step):

```yaml
sources:
  osinfo:
    connector: plugin
    command: ["node", "./osinfo.mjs"]
    options: { dataset: "*" }
```

- Cells are plain JS values (number, string, boolean, `Date` for time columns,
  `Buffer`/`Uint8Array` for bytes, `null` for NULL).
- The SDK applies the pushed-down `WHERE`/`LIMIT` to the rows you return, so
  you get pushdown for free; pass `manualPushdown: true` to handle them
  yourself (the request carries the decoded predicate and `evalPredicate()`
  is exported).
- stdout carries protocol messages only — log with `console.error`.

See [`examples/plugins/nodeos`](../../examples/plugins/nodeos/nodeos.mjs) for
a complete reference plugin. This directory is intended to graduate into its
own repository/package eventually; until then, import it by relative path (the
reference plugin shows how).
