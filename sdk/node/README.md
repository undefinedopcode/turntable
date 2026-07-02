# ttplugin (Node.js)

The Node.js SDK for writing [turntable](https://github.com/undefinedopcode/turntable) plugin connectors — a
single dependency-free ES module implementing the stdio JSON-RPC protocol from
[PLUGINS.md](https://github.com/undefinedopcode/turntable/blob/main/PLUGINS.md): framing, dispatch, scan cursors, predicate
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

See [`examples/plugins/nodeos`](https://github.com/undefinedopcode/turntable/blob/main/examples/plugins/nodeos/nodeos.mjs) for
a complete reference plugin — it imports the module by relative path; with
this repository checked out (or the package installed) a plain
`import { serve } from "ttplugin"` works.

Published standalone at
[undefinedopcode/turntable-node-sdk](https://github.com/undefinedopcode/turntable-node-sdk)
(split from the turntable monorepo's `sdk/node` — develop and file issues
there). Sibling SDKs:
[Go](https://github.com/undefinedopcode/turntable-go-sdk),
[Python](https://github.com/undefinedopcode/turntable-python-sdk).
