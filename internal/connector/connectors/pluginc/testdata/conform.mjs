#!/usr/bin/env node
// Conformance fixture for the Node.js plugin SDK, driven by sdkconform_test.go
// through the real pluginc connector. Same fixed rows as conform.py.
import { serve } from "../../../../../sdk/node/ttplugin.js";

const T1 = new Date(Date.UTC(2026, 0, 1, 10, 0, 0));
const T2 = new Date(Date.UTC(2026, 0, 2, 10, 0, 0));

const ROWS = [
  [1, 1.5, "alpha", true, T1, Buffer.from("hi")],
  [2, 2.5, "beta", false, T2, null],
  [3, null, "aloe", true, null, Buffer.from("yo")],
];

serve({
  name: "conform",
  datasets: {
    vals: {
      columns: [
        { name: "i", type: "int" },
        { name: "f", type: "float", nullable: true },
        { name: "s", type: "string" },
        { name: "b", type: "bool" },
        { name: "t", type: "time", nullable: true },
        { name: "by", type: "bytes", nullable: true },
      ],
      rows: () => ROWS,
    },
  },
});
