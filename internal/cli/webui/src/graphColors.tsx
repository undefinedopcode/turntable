// Shared node-colouring + sizing for the node-link graph views (the 2D
// chartjs-chart-graph renderer in Chart.tsx and the 3D canvas renderer in
// Graph3D.tsx). Kept here so both map a colour column to node colours the same
// way and render an identical legend. (Chart.tsx's GraphChart still carries its
// own inline copy for now — this module is the shared home the 3D view uses.)
import type { Column } from "./api";
import { heatColor, labelOf, numOrNull, type Graph } from "./pivot";

// The categorical palette, matching Chart.tsx.
export const PALETTE = [
  "#6ea8fe",
  "#5fd38d",
  "#e0b341",
  "#c08bd6",
  "#ff6b6b",
  "#4dd4c8",
  "#f4a259",
  "#9aa7ff",
];

export const alpha = (hex: string, a: string) => hex + a;

const ROOT_COLOR = "rgba(138,147,167,0.35)";
const NULL_COLOR = "rgba(138,147,167,0.45)";

// GraphLegend describes the node colouring for the swatch legend below a graph.
export type GraphLegend =
  | null
  | { kind: "cat"; name: string; items: { label: string; color: string }[]; total: number }
  | { kind: "numeric"; name: string; lo: number; hi: number };

// Map a measure's values to a pixel bubble radius. Equal/blank ranges collapse to
// a mid radius so a graph with a constant size column still renders.
export function bubbleRadii(values: (number | null)[]): number[] {
  const present = values.filter((v): v is number => v !== null);
  const lo = present.length ? Math.min(...present) : 0;
  const hi = present.length ? Math.max(...present) : 0;
  const MIN_R = 4;
  const MAX_R = 26;
  return values.map((v) => {
    if (v === null) return MIN_R;
    if (hi === lo) return (MIN_R + MAX_R) / 2;
    return MIN_R + ((v - lo) / (hi - lo)) * (MAX_R - MIN_R);
  });
}

// computeNodeColors resolves a per-node colour array (aligned to g.nodes) plus a
// legend. A numeric colour column is a value gradient (heatColor); any other
// column is categorical (a palette colour per distinct value). Nodes with no
// value, and the synthetic tree root, get grey.
export function computeNodeColors(
  g: Graph,
  columns: Column[],
  colorCol: number,
  numIdx: number[],
): { colors: string[]; legend: GraphLegend } {
  if (colorCol < 0) {
    return {
      colors: g.nodes.map((_, i) => (i === g.rootIndex ? ROOT_COLOR : alpha(PALETTE[0], "cc"))),
      legend: null,
    };
  }
  if (numIdx.includes(colorCol)) {
    const vals = g.nodes.map((n) => (n.color == null ? null : numOrNull(n.color)));
    const present = vals.filter((v): v is number => v !== null);
    const lo = present.length ? Math.min(...present) : 0;
    const hi = present.length ? Math.max(...present) : 0;
    const colors = g.nodes.map((_, i) =>
      i === g.rootIndex ? ROOT_COLOR : vals[i] === null ? NULL_COLOR : heatColor(vals[i]!, lo, hi),
    );
    const legend: GraphLegend = present.length
      ? { kind: "numeric", lo, hi, name: columns[colorCol]?.name ?? "" }
      : null;
    return { colors, legend };
  }
  const palette = new Map<string, string>();
  const order: string[] = [];
  for (const n of g.nodes) {
    if (n.color == null) continue;
    const k = labelOf(n.color);
    if (!palette.has(k)) {
      palette.set(k, PALETTE[palette.size % PALETTE.length]);
      order.push(k);
    }
  }
  const colors = g.nodes.map((n, i) =>
    i === g.rootIndex ? ROOT_COLOR : n.color == null ? NULL_COLOR : palette.get(labelOf(n.color))!,
  );
  const legend: GraphLegend = {
    kind: "cat",
    name: columns[colorCol]?.name ?? "",
    items: order.slice(0, 16).map((k) => ({ label: k, color: palette.get(k)! })),
    total: order.length,
  };
  return { colors, legend };
}

// GraphColorLegend renders the node-colour key below a graph: palette swatches
// for a categorical colour column, or a gradient bar for a numeric one.
export function GraphColorLegend({ legend }: { legend: Exclude<GraphLegend, null> }) {
  const swatch = (bg: string, w = 10) => ({
    width: w,
    height: 10,
    borderRadius: 2,
    background: bg,
    display: "inline-block",
  });
  return (
    <div
      style={{
        display: "flex",
        flexWrap: "wrap",
        alignItems: "center",
        gap: "4px 14px",
        padding: "4px 2px",
        fontSize: 12,
        color: "var(--muted, #8b93a7)",
      }}
    >
      <span style={{ opacity: 0.8 }}>{legend.name}:</span>
      {legend.kind === "cat" ? (
        <>
          {legend.items.map((it) => (
            <span key={it.label} style={{ display: "inline-flex", alignItems: "center", gap: 6 }}>
              <span style={swatch(it.color)} />
              {it.label}
            </span>
          ))}
          {legend.total > legend.items.length && (
            <span>+{legend.total - legend.items.length} more</span>
          )}
        </>
      ) : (
        <span style={{ display: "inline-flex", alignItems: "center", gap: 6 }}>
          {legend.lo}
          <span
            style={swatch(
              `linear-gradient(to right, ${heatColor(legend.lo, legend.lo, legend.hi)}, ${heatColor(
                (legend.lo + legend.hi) / 2,
                legend.lo,
                legend.hi,
              )}, ${heatColor(legend.hi, legend.lo, legend.hi)})`,
              64,
            )}
          />
          {legend.hi}
        </span>
      )}
    </div>
  );
}
