// Serializable per-tab view configuration: how the results pane presents a
// result (table / chart / pivot, plus each view's settings). Column references
// are BY NAME, not index, so a saved config survives column reordering and
// schema drift — a name that doesn't resolve in the current result is simply
// ignored and the view falls back to its default choice.
//
// This is also the interchange format for future dashboards: a dashboard panel
// is a query plus one of these frozen view configs.
import type { Agg } from "./pivot";

export type ChartType =
  | "bar"
  | "line"
  | "area"
  | "scatter"
  | "bubble"
  | "heatmap"
  | "calendar"
  | "pie"
  | "graph"
  | "tree";

export interface ChartViewConfig {
  type?: ChartType;
  agg?: Agg;
  x?: string;
  y?: string[]; // empty/omitted = auto (first numeric column)
  y2?: string[]; // series plotted on a secondary (right) Y axis
  seriesBy?: string;
  size?: string;
  thresholds?: number[]; // horizontal reference lines (alarm levels etc.)
  bandLo?: string; // envelope band: lower/upper bound columns (e.g. p10/p90),
  bandHi?: string; // rendered as a translucent fill behind the series

  // graph/tree settings
  node?: string;
  link?: string;
  label?: string;
  color?: string;
  graphSource?: "edges" | "path";
  levels?: string[]; // hierarchy (path) mode: ordered level columns
}

export interface PivotViewConfig {
  rows?: string;
  cols?: string;
  value?: string;
  agg?: Agg;
  color?: boolean;
}

export interface ViewConfig {
  mode?: "table" | "chart" | "pivot";
  chart?: ChartViewConfig;
  pivot?: PivotViewConfig;
}
