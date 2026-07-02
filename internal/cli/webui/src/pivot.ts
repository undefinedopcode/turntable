// Shared aggregation / pivot helpers used by the chart and pivot-table views.
import type { Cell, Column } from "./api";

export type Agg = "none" | "count" | "sum" | "avg" | "min" | "max";
export const AGGS: Agg[] = ["none", "count", "sum", "avg", "min", "max"];

export const labelOf = (v: Cell) =>
  v === null || v === undefined ? "NULL" : String(v);

// numOrNull extracts a finite number, or null for null/blank/non-numeric cells
// (NB: Number(null) is 0, so a naive cast would wrongly count/sum nulls).
export function numOrNull(v: Cell): number | null {
  if (typeof v === "number") return Number.isFinite(v) ? v : null;
  if (v === null || v === undefined || v === "") return null;
  const n = Number(v);
  return Number.isFinite(n) ? n : null;
}

export function applyAgg(nums: number[], fn: Agg): number | null {
  if (fn === "count") return nums.length;
  if (nums.length === 0) return null; // empty group → gap (SQL-like NULL)
  switch (fn) {
    case "sum":
      return nums.reduce((a, b) => a + b, 0);
    case "avg":
      return nums.reduce((a, b) => a + b, 0) / nums.length;
    case "min":
      return Math.min(...nums);
    case "max":
      return Math.max(...nums);
    default:
      return null;
  }
}

// reduceCell collapses the rows that fall in one (x[,series]) cell. "none" keeps
// the last value (a pass-through for 1:1 data); otherwise it aggregates.
export function reduceCell(nums: number[], fn: Agg): number | null {
  if (nums.length === 0) return null;
  if (fn === "none") return nums[nums.length - 1];
  return applyAgg(nums, fn);
}

// numericColumns returns the columns that contain at least one numeric value.
export function numericColumns(columns: Column[], rows: Cell[][]) {
  return columns
    .map((c, i) => ({ c, i }))
    .filter(({ i }) => rows.some((r) => typeof r[i] === "number"));
}

export interface Pivot {
  labels: string[]; // x (column) keys, first-seen order, capped
  seriesKeys: string[]; // series (row) keys, first-seen order, capped
  data: (number | null)[][]; // data[seriesPos][xPos]
  xTotal: number; // pre-cap counts, for "showing first N of M" hints
  sTotal: number;
}

// pivot buckets rows by (x, series) and reduces the y measure in each cell by fn.
// X labels and series both follow first-seen order, each capped (caps default to
// the chart's; the pivot table passes larger ones).
export function pivot(
  rows: Cell[][],
  xIdx: number,
  yIdx: number,
  sIdx: number,
  fn: Agg,
  xCap = 100,
  sCap = 12,
): Pivot {
  const xOrder: string[] = [];
  const xIndex = new Map<string, number>();
  const sOrder: string[] = [];
  const sIndex = new Map<string, number>();
  const cells = new Map<string, number[]>(); // `${xKey} ${sKey}` -> numbers
  for (const r of rows) {
    const xk = labelOf(r[xIdx]);
    if (!xIndex.has(xk)) {
      xIndex.set(xk, xOrder.length);
      xOrder.push(xk);
    }
    const sk = labelOf(r[sIdx]);
    if (!sIndex.has(sk)) {
      sIndex.set(sk, sOrder.length);
      sOrder.push(sk);
    }
    const v = numOrNull(r[yIdx]);
    if (v === null) continue;
    const key = xk + " " + sk;
    let arr = cells.get(key);
    if (!arr) {
      arr = [];
      cells.set(key, arr);
    }
    arr.push(v);
  }
  const labels = xOrder.slice(0, xCap);
  const seriesKeys = sOrder.slice(0, sCap);
  const data = seriesKeys.map((sk) =>
    labels.map((xk) => reduceCell(cells.get(xk + " " + sk) ?? [], fn)),
  );
  return { labels, seriesKeys, data, xTotal: xOrder.length, sTotal: sOrder.length };
}

export interface GraphNode {
  key: string; // canonical node identity (the node column value), for focus
  label: string;
  size: number | null; // optional measure for node sizing
  color: Cell; // optional raw value for node colouring (null when unset/absent)
}

export interface Graph {
  nodes: GraphNode[];
  edges: { source: number; target: number }[];
  total: number; // node count for the "showing first N of M" hint
  rootIndex: number; // index of the synthetic root (tree mode), else -1
  focusMissing: boolean; // true when a focus key was given but not found
}

// nodesEdges turns an edge-list / parent-pointer result into a node+edge graph.
// Each row contributes a node (its nodeIdx value) and, when the linkIdx cell is
// non-null, an edge between the two. A value appearing only as a link target
// (e.g. a parent pid outside the filtered set) still becomes a node. Edges are
// oriented link→node (parent→child) so the tree layout roots correctly.
//
// In tree mode a synthetic root is added and every node that has no parent in
// the set is attached to it, so a forest (e.g. the process table's many roots)
// renders as one tree. Nodes are capped to keep force layouts legible.
//
// focus (a node key) drills into a subtree: only that node and its descendants
// (forward-reachable along edges) are kept, the focus node becomes the root, and
// the cap is lifted (a subtree is small) so descendants outside the cap still
// appear. An unknown focus key falls back to the full graph (focusMissing=true).
export function nodesEdges(
  rows: Cell[][],
  nodeIdx: number,
  linkIdx: number,
  labelIdx: number, // -1 = use the node value as the label
  sizeIdx: number, // -1 = no sizing
  colorIdx: number, // -1 = no colouring
  mode: "graph" | "tree",
  focus: string | null = null,
  cap = 300,
): Graph {
  const focused = focus != null;
  const index = new Map<string, number>();
  const nodes: GraphNode[] = [];
  const ensure = (key: string): number => {
    let i = index.get(key);
    if (i === undefined) {
      i = nodes.length;
      index.set(key, i);
      nodes.push({ key, label: key, size: null, color: null });
    }
    return i;
  };

  const edgeSet = new Set<string>();
  const edges: { source: number; target: number }[] = [];
  const hasParent = new Set<number>();

  for (const r of rows) {
    // The cap bounds an unfocused graph; when focusing we build everything and
    // then extract the (small) subtree below.
    if (!focused && index.size >= cap && index.get(labelOf(r[nodeIdx])) === undefined) continue;
    const nodeKey = labelOf(r[nodeIdx]);
    const ni = ensure(nodeKey);
    if (labelIdx >= 0) nodes[ni].label = labelOf(r[labelIdx]);
    if (sizeIdx >= 0) nodes[ni].size = numOrNull(r[sizeIdx]);
    if (colorIdx >= 0) nodes[ni].color = r[colorIdx] ?? null;

    const linkCell = r[linkIdx];
    if (linkCell !== null && linkCell !== undefined && linkCell !== "") {
      const pi = ensure(labelOf(linkCell));
      const key = pi + ">" + ni;
      if (pi !== ni && !edgeSet.has(key)) {
        edgeSet.add(key);
        edges.push({ source: pi, target: ni });
        hasParent.add(ni);
      }
    }
  }

  if (focused) {
    const start = index.get(focus!);
    if (start !== undefined) {
      const adj = new Map<number, number[]>();
      for (const e of edges) {
        const a = adj.get(e.source);
        if (a) a.push(e.target);
        else adj.set(e.source, [e.target]);
      }
      const keep = new Set<number>([start]); // Set keeps insertion (BFS) order
      const queue = [start];
      while (queue.length) {
        const u = queue.shift()!;
        for (const v of adj.get(u) ?? []) {
          if (!keep.has(v)) {
            keep.add(v);
            queue.push(v);
          }
        }
      }
      const remap = new Map<number, number>();
      const fnodes: GraphNode[] = [];
      for (const oi of keep) {
        remap.set(oi, fnodes.length);
        fnodes.push(nodes[oi]);
      }
      const fedges = edges
        .filter((e) => keep.has(e.source) && keep.has(e.target))
        .map((e) => ({ source: remap.get(e.source)!, target: remap.get(e.target)! }));
      return { nodes: fnodes, edges: fedges, total: fnodes.length, rootIndex: -1, focusMissing: false };
    }
    // Focus key not in this data — show the full graph and flag it.
  }

  let rootIndex = -1;
  if (mode === "tree") {
    rootIndex = nodes.length;
    nodes.push({ key: "", label: "", size: null, color: null });
    for (let i = 0; i < rootIndex; i++) {
      if (!hasParent.has(i)) edges.push({ source: rootIndex, target: i });
    }
  }
  return { nodes, edges, total: index.size, rootIndex, focusMissing: focused };
}

// finalizeGraph applies focus extraction (drill into a node's subtree) and, in
// tree mode, attaches every parentless node to a synthetic root. It is the shared
// tail used by the hierarchy builder below (nodesEdges has the equivalent inline).
function finalizeGraph(
  nodes: GraphNode[],
  edges: { source: number; target: number }[],
  index: Map<string, number>,
  hasParent: Set<number>,
  mode: "graph" | "tree",
  focus: string | null,
): Graph {
  const focused = focus != null;
  if (focused) {
    const start = index.get(focus!);
    if (start !== undefined) {
      const adj = new Map<number, number[]>();
      for (const e of edges) {
        const a = adj.get(e.source);
        if (a) a.push(e.target);
        else adj.set(e.source, [e.target]);
      }
      const keep = new Set<number>([start]);
      const queue = [start];
      while (queue.length) {
        const u = queue.shift()!;
        for (const v of adj.get(u) ?? []) {
          if (!keep.has(v)) {
            keep.add(v);
            queue.push(v);
          }
        }
      }
      const remap = new Map<number, number>();
      const fnodes: GraphNode[] = [];
      for (const oi of keep) {
        remap.set(oi, fnodes.length);
        fnodes.push(nodes[oi]);
      }
      const fedges = edges
        .filter((e) => keep.has(e.source) && keep.has(e.target))
        .map((e) => ({ source: remap.get(e.source)!, target: remap.get(e.target)! }));
      return { nodes: fnodes, edges: fedges, total: fnodes.length, rootIndex: -1, focusMissing: false };
    }
  }
  let rootIndex = -1;
  if (mode === "tree") {
    rootIndex = nodes.length;
    nodes.push({ key: "", label: "", size: null, color: null });
    for (let i = 0; i < rootIndex; i++) {
      if (!hasParent.has(i)) edges.push({ source: rootIndex, target: i });
    }
  }
  return { nodes, edges, total: index.size, rootIndex, focusMissing: focused };
}

// nodesEdgesFromPath builds a hierarchy from an ordered list of columns
// (outer→inner, e.g. subscriptionId › resourceGroup › name). Unlike nodesEdges
// (a self-referential parent-pointer in one column), here each row is a leaf and
// its ancestry is the path across those columns: every cumulative prefix becomes
// a node (id = the full prefix so same-named branches under different parents stay
// distinct, label = the last segment), edges link parent prefix → child prefix,
// and a measure is summed up each prefix for sizing. Null/empty levels are
// skipped. It returns the same Graph shape, so the renderer and focus/root
// handling are shared.
export function nodesEdgesFromPath(
  rows: Cell[][],
  levelIdxs: number[],
  sizeIdx: number, // -1 = no sizing
  colorIdx: number, // -1 = no colouring (applied to leaf nodes)
  mode: "graph" | "tree",
  focus: string | null = null,
  cap = 300,
): Graph {
  const SEP = "›"; // › — path separator for node ids
  const index = new Map<string, number>();
  const nodes: GraphNode[] = [];
  const edgeSet = new Set<string>();
  const edges: { source: number; target: number }[] = [];
  const hasParent = new Set<number>();
  const focused = focus != null;

  const ensure = (key: string, label: string): number => {
    let i = index.get(key);
    if (i === undefined) {
      i = nodes.length;
      index.set(key, i);
      nodes.push({ key, label, size: null, color: null });
    }
    return i;
  };

  for (const r of rows) {
    const segs: string[] = [];
    for (const c of levelIdxs) {
      const v = r[c];
      if (v === null || v === undefined || v === "") continue; // skip a missing level
      segs.push(labelOf(v));
    }
    if (segs.length === 0) continue;
    const leafKey = segs.join(SEP);
    if (!focused && index.size >= cap && index.get(leafKey) === undefined) continue;

    const rowSize = sizeIdx >= 0 ? numOrNull(r[sizeIdx]) : null;
    let prevIdx = -1;
    let prefix = "";
    for (let d = 0; d < segs.length; d++) {
      prefix = d === 0 ? segs[0] : prefix + SEP + segs[d];
      const ni = ensure(prefix, segs[d]);
      if (rowSize !== null) nodes[ni].size = (nodes[ni].size ?? 0) + rowSize;
      if (colorIdx >= 0 && d === segs.length - 1) nodes[ni].color = r[colorIdx] ?? null;
      if (prevIdx >= 0 && prevIdx !== ni) {
        const ek = prevIdx + ">" + ni;
        if (!edgeSet.has(ek)) {
          edgeSet.add(ek);
          edges.push({ source: prevIdx, target: ni });
          hasParent.add(ni);
        }
      }
      prevIdx = ni;
    }
  }
  return finalizeGraph(nodes, edges, index, hasParent, mode, focus);
}

// ---- calendar (contribution-graph) binning -----------------------------------

export interface CalendarCell {
  week: number; // column index
  day: number; // 0 = Sunday … 6 = Saturday
  v: number | null; // null = a day in range with no data
  date: string; // YYYY-MM-DD, for the tooltip
}

export interface Calendar {
  cells: CalendarCell[];
  weeks: number;
  monthLabels: (string | null)[]; // per week column; label where a month starts
  lo: number;
  hi: number;
  capped: boolean; // true when older weeks were dropped to fit maxWeeks
}

const dayKey = (d: Date) =>
  `${d.getFullYear()}-${String(d.getMonth() + 1).padStart(2, "0")}-${String(d.getDate()).padStart(2, "0")}`;

// calendarize bins rows by LOCAL calendar day from a time column and lays the
// days out GitHub-contribution-graph style: weekday rows (Sunday top) × week
// columns. yIdx < 0 counts rows per day; otherwise the measure is reduced by
// fn ("none" acts as sum — a calendar cell is inherently an aggregate). Days
// inside the range with no rows render as null cells, so quiet days stay
// visible. Returns null when no cell parses as a time.
export function calendarize(
  rows: Cell[][],
  xIdx: number,
  yIdx: number,
  fn: Agg,
  maxWeeks = 106,
): Calendar | null {
  const days = new Map<string, { date: Date; nums: number[]; count: number }>();
  let min: Date | null = null;
  let max: Date | null = null;
  for (const r of rows) {
    const t = Date.parse(String(r[xIdx] ?? ""));
    if (Number.isNaN(t)) continue;
    const d = new Date(t);
    const day = new Date(d.getFullYear(), d.getMonth(), d.getDate());
    const key = dayKey(day);
    let e = days.get(key);
    if (!e) {
      e = { date: day, nums: [], count: 0 };
      days.set(key, e);
    }
    e.count++;
    if (yIdx >= 0) {
      const v = numOrNull(r[yIdx]);
      if (v !== null) e.nums.push(v);
    }
    if (!min || day < min) min = day;
    if (!max || day > max) max = day;
  }
  if (!min || !max) return null;

  // Snap the start back to a Sunday; cap to the most recent maxWeeks.
  const start = new Date(min);
  start.setDate(start.getDate() - start.getDay());
  const end = new Date(max);
  let totalDays = Math.round((end.getTime() - start.getTime()) / 86400000) + 1;
  let capped = false;
  if (totalDays > maxWeeks * 7) {
    capped = true;
    start.setTime(end.getTime());
    start.setDate(start.getDate() - start.getDay() - (maxWeeks - 1) * 7);
    totalDays = Math.round((end.getTime() - start.getTime()) / 86400000) + 1;
  }

  const reduce = (e: { nums: number[]; count: number }): number | null => {
    if (yIdx < 0) return e.count;
    const f: Agg = fn === "none" ? "sum" : fn;
    return applyAgg(e.nums, f);
  };

  const cells: CalendarCell[] = [];
  const monthLabels: (string | null)[] = [];
  let lo = Infinity;
  let hi = -Infinity;
  const cursor = new Date(start);
  for (let i = 0; i < totalDays; i++) {
    const week = Math.floor(i / 7);
    if (cursor.getDay() === 0) {
      // Label the column where a month begins (its 1st falls in this week).
      const weekEnd = new Date(cursor);
      weekEnd.setDate(weekEnd.getDate() + 6);
      monthLabels[week] =
        cursor.getDate() <= 7
          ? cursor.toLocaleString("default", { month: "short" })
          : weekEnd.getDate() <= 7
            ? weekEnd.toLocaleString("default", { month: "short" })
            : null;
    }
    const e = days.get(dayKey(cursor));
    const v = e ? reduce(e) : null;
    if (v !== null) {
      lo = Math.min(lo, v);
      hi = Math.max(hi, v);
    }
    cells.push({ week, day: cursor.getDay(), v, date: dayKey(cursor) });
    cursor.setDate(cursor.getDate() + 1); // date-component step: DST-safe
  }
  if (!isFinite(lo)) {
    lo = 0;
    hi = 0;
  }
  return { cells, weeks: Math.ceil(totalDays / 7), monthLabels, lo, hi, capped };
}

// heatColor maps a value in [lo,hi] to a blue→accent→amber gradient (null →
// faint). Three stops keep low/mid/high visually distinct on the dark UI. alpha
// < 1 lets a colored cell sit over the dark table while keeping its text legible.
export function heatColor(
  v: number | null,
  lo: number,
  hi: number,
  alpha = 1,
): string {
  if (v === null) return "rgba(138,147,167,0.06)";
  const t = hi === lo ? 0.5 : (v - lo) / (hi - lo);
  const stops = [
    [28, 42, 74], // low  (deep blue)
    [110, 168, 254], // mid (accent blue)
    [224, 179, 65], // high (amber)
  ];
  const seg = t <= 0.5 ? 0 : 1;
  const f = t <= 0.5 ? t / 0.5 : (t - 0.5) / 0.5;
  const a = stops[seg];
  const b = stops[seg + 1];
  const c = a.map((x, i) => Math.round(x + (b[i] - x) * f));
  return `rgba(${c[0]}, ${c[1]}, ${c[2]}, ${alpha})`;
}
