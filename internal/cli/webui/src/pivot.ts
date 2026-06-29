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
