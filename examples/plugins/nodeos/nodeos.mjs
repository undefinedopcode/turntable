#!/usr/bin/env node
// nodeos — a reference turntable plugin connector in Node.js.
//
// Exposes live OS state via node:os — no dependencies, no build step:
//
//   cpus   one row per logical CPU: model, speed_mhz, and cumulative
//          user/sys/idle milliseconds (rates via the engine's DELTA/RATE
//          across scans, or just ORDER BY busy time)
//   net    one row per network address: iface, address, family, mac,
//          internal, cidr
//   host   a single row: hostname, platform, arch, release, uptime_s,
//          total_mem, free_mem, load1/5/15
//
//   SELECT iface, address FROM nodeos:net WHERE NOT internal AND family = 'IPv4'
//
// Register it (see PLUGINS.md):
//
//   sources:
//     nodeos:
//       connector: plugin
//       command: ["node", "./examples/plugins/nodeos/nodeos.mjs"]
//       options: { dataset: "*" }
//
// The SDK (sdk/node/ttplugin.js, dependency-free) does all protocol work;
// while in this repo it is imported by relative path — a published package
// would be `import { serve } from "ttplugin"`.
import os from "node:os";
import { serve } from "../../../sdk/node/ttplugin.js";

serve({
  name: "nodeos",
  datasets: {
    cpus: {
      columns: [
        { name: "cpu", type: "int" },
        { name: "model", type: "string" },
        { name: "speed_mhz", type: "int" },
        { name: "user_ms", type: "int" },
        { name: "sys_ms", type: "int" },
        { name: "idle_ms", type: "int" },
      ],
      rows: () =>
        os.cpus().map((c, i) => [i, c.model.trim(), c.speed, c.times.user, c.times.sys, c.times.idle]),
    },
    net: {
      columns: [
        { name: "iface", type: "string" },
        { name: "address", type: "string" },
        { name: "family", type: "string" },
        { name: "mac", type: "string" },
        { name: "internal", type: "bool" },
        { name: "cidr", type: "string", nullable: true },
      ],
      rows: () => {
        const rows = [];
        for (const [iface, addrs] of Object.entries(os.networkInterfaces())) {
          for (const a of addrs ?? []) {
            rows.push([iface, a.address, a.family, a.mac, a.internal, a.cidr]);
          }
        }
        return rows;
      },
    },
    host: {
      columns: [
        { name: "hostname", type: "string" },
        { name: "platform", type: "string" },
        { name: "arch", type: "string" },
        { name: "release", type: "string" },
        { name: "uptime_s", type: "int" },
        { name: "total_mem", type: "int" },
        { name: "free_mem", type: "int" },
        { name: "load1", type: "float" },
        { name: "load5", type: "float" },
        { name: "load15", type: "float" },
      ],
      rows: () => {
        const [l1, l5, l15] = os.loadavg();
        return [[
          os.hostname(), os.platform(), os.arch(), os.release(),
          Math.floor(os.uptime()), os.totalmem(), os.freemem(), l1, l5, l15,
        ]];
      },
    },
  },
});
