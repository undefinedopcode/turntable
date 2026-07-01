import { memo, useEffect, useMemo, useRef, useState } from "react";
import {
  Chart as ChartJS,
  CategoryScale,
  LinearScale,
  TimeScale,
  BarElement,
  PointElement,
  LineElement,
  ArcElement,
  Tooltip,
  Legend,
  Filler,
  Decimation,
  type ChartOptions,
} from "chart.js";
// Date adapter for the time scale (time-typed X columns arrive as RFC3339
// strings; the scale needs an adapter to compute/format time ticks).
import "chartjs-adapter-date-fns";
import { MatrixController, MatrixElement } from "chartjs-chart-matrix";
// NOTE: chart.js is pinned to ~4.4 (see package.json). chartjs-chart-graph 4.3.5
// (latest) is incompatible with chart.js 4.5's option-sharing internals — under
// 4.5 the graph controller never assigns options to edge elements and
// EdgeLine.draw throws "Cannot read properties of undefined (reading
// 'borderCapStyle')". Do not bump chart.js to 4.5 until the graph plugin fixes it.
import {
  ForceDirectedGraphController,
  TreeController,
  EdgeLine,
} from "chartjs-chart-graph";
import ChartDataLabels from "chartjs-plugin-datalabels";
import zoomPlugin from "chartjs-plugin-zoom";
import { Bar, Line, Scatter, Pie, Bubble, Chart as ReactChart } from "react-chartjs-2";
import type { Cell, Column } from "../api";
import type { ChartType, ChartViewConfig } from "../view";
import { downloadCanvasPNG } from "../export";
import {
  type Agg,
  AGGS,
  applyAgg,
  heatColor,
  labelOf,
  nodesEdges,
  nodesEdgesFromPath,
  numOrNull,
  numericColumns,
  pivot,
} from "../pivot";

ChartJS.register(
  CategoryScale,
  LinearScale,
  TimeScale,
  BarElement,
  PointElement,
  LineElement,
  ArcElement,
  Tooltip,
  Legend,
  Filler,
  MatrixController,
  MatrixElement,
  ForceDirectedGraphController,
  TreeController,
  EdgeLine,
  ChartDataLabels,
  zoomPlugin,
  Decimation,
);

// datalabels is registered globally for the graph/tree node labels, but it must
// stay off for every other chart type — enable it per-chart in the graph branch.
ChartJS.defaults.plugins.datalabels = { display: false };

// Match the dark UI (values mirror the CSS custom properties in styles.css).
ChartJS.defaults.color = "#8b93a7";
ChartJS.defaults.borderColor = "rgba(138,147,167,0.15)";
ChartJS.defaults.font.family = "ui-monospace, SFMono-Regular, Menlo, monospace";

const PALETTE = [
  "#6ea8fe",
  "#5fd38d",
  "#e0b341",
  "#c08bd6",
  "#ff6b6b",
  "#4dd4c8",
  "#f4a259",
  "#9aa7ff",
];
const GRID = "rgba(138,147,167,0.12)";
const RAW_CAP = 500; // points plotted when not aggregating
const GROUP_CAP = 100; // groups (bars/slices / x labels) kept
const SERIES_CAP = 12; // distinct "series by" values kept

const TYPES: ChartType[] = [
  "bar",
  "line",
  "area",
  "scatter",
  "bubble",
  "heatmap",
  "pie",
  "graph",
  "tree",
];
const NODE_CAP = 300; // nodes plotted in a graph/tree before capping
const LABEL_CAP = 60; // above this many nodes, hide on-node labels (tooltip only)

// GraphLegend describes the node colouring for the swatch legend below a graph.
type GraphLegend =
  | null
  | { kind: "cat"; name: string; items: { label: string; color: string }[]; total: number }
  | { kind: "numeric"; name: string; lo: number; hi: number };

// Map a measure's values to a pixel bubble radius. Equal/blank ranges collapse to
// a mid radius so a bubble chart with a constant size column still renders.
function bubbleRadii(values: (number | null)[]): number[] {
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

const alpha = (hex: string, a: string) => hex + a;

// timeMs parses a time cell (the API serializes TypeTime as an RFC3339 string)
// to epoch milliseconds, or null when it isn't a parseable time.
function timeMs(v: Cell): number | null {
  if (typeof v !== "string") return null;
  const t = Date.parse(v);
  return Number.isNaN(t) ? null : t;
}

// aggregate groups rows by the X column (first-seen order) and reduces each Y
// series within a group. Counts non-null numeric values; sum/avg/min/max ignore
// non-numeric cells. Reports the total group count so the UI can flag capping.
function aggregate(rows: Cell[][], xIdx: number, series: number[], fn: Agg) {
  const order: string[] = [];
  const index = new Map<string, number>();
  const acc: number[][][] = []; // acc[group][seriesPos] = numbers
  for (const r of rows) {
    const key = labelOf(r[xIdx]);
    let gi = index.get(key);
    if (gi === undefined) {
      gi = order.length;
      index.set(key, gi);
      order.push(key);
      acc.push(series.map(() => []));
    }
    series.forEach((si, k) => {
      const v = numOrNull(r[si]);
      if (v !== null) acc[gi][k].push(v);
    });
  }
  const labels = order.slice(0, GROUP_CAP);
  const values = series.map((_, k) => labels.map((_, gi) => applyAgg(acc[gi][k], fn)));
  return { labels, values, total: order.length };
}

// Chart renders one result as a configurable chart. `config` seeds the controls
// at mount (column refs by name — see view.ts); every change is reported back
// through `onConfig` so the tab can persist it.
export function Chart({
  columns,
  rows,
  config,
  onConfig,
}: {
  columns: Column[];
  rows: Cell[][];
  config?: ChartViewConfig;
  onConfig?: (c: ChartViewConfig) => void;
}) {
  const numeric = useMemo(() => numericColumns(columns, rows), [columns, rows]);

  // Resolve a persisted column name to its current index (fallback when absent).
  const byName = (name: string | undefined, fallback: number) => {
    const i = name == null ? -1 : columns.findIndex((c) => c.name === name);
    return i >= 0 ? i : fallback;
  };
  const [type, setType] = useState<ChartType>(() =>
    config?.type && TYPES.includes(config.type) ? config.type : "bar",
  );
  const [agg, setAgg] = useState<Agg>(() =>
    config?.agg && AGGS.includes(config.agg) ? config.agg : "none",
  );
  const [xIdx, setXIdx] = useState(() => byName(config?.x, 0));
  const [ySel, setYSel] = useState<number[]>(() =>
    (config?.y ?? []).map((n) => byName(n, -1)).filter((i) => i >= 0),
  );
  const [seriesBy, setSeriesBy] = useState(() => byName(config?.seriesBy, -1)); // -1 = none (no breakdown)
  const [sizeIdx, setSizeIdx] = useState(() => byName(config?.size, -1)); // bubble/node size measure (-1 = constant)
  const [nodeIdx, setNodeIdx] = useState(() => byName(config?.node, 0)); // graph: the node column
  const [linkIdx, setLinkIdx] = useState(() => byName(config?.link, 1)); // graph: the parent / links-to column
  const [labelIdx, setLabelIdx] = useState(() => byName(config?.label, -1)); // graph: node label (-1 = node value)
  const [colorIdx, setColorIdx] = useState(() => byName(config?.color, -1)); // graph: node colour column (-1 = constant)
  const [focusNode, setFocusNode] = useState<string | null>(null); // graph: drilled-in subtree
  // graph input model: "edges" = node + links-to (self-referential parent
  // pointer); "path" = a hierarchy synthesized from an ordered list of columns.
  const [graphSource, setGraphSource] = useState<"edges" | "path">(() =>
    config?.graphSource === "path" ? "path" : "edges",
  );
  const [levelIdxs, setLevelIdxs] = useState<number[]>(() => {
    const l = (config?.levels ?? []).map((n) => byName(n, -1)).filter((i) => i >= 0);
    return l.length ? l : [0, 1]; // path mode: the ordered levels
  });
  // The ordered levels, clamped to the current columns (at least one). Memoized so
  // its array reference is stable across unrelated re-renders — GraphChart is
  // memoized and a fresh array each render would defeat that (and blank the graph).
  const levels = useMemo(() => {
    const l = levelIdxs.filter((i) => i >= 0 && i < columns.length);
    return l.length ? l : [0];
  }, [levelIdxs, columns.length]);
  const canvasRef = useRef<HTMLDivElement>(null);

  // Report the current settings upward, by column name, whenever they change.
  // onConfig goes through a ref so an unstable callback prop doesn't re-fire
  // the effect (and can't loop: the effect depends only on local state).
  const onConfigRef = useRef(onConfig);
  onConfigRef.current = onConfig;
  useEffect(() => {
    const colName = (i: number) =>
      i >= 0 && i < columns.length ? columns[i].name : undefined;
    const names = (idxs: number[]) =>
      idxs.map(colName).filter((n): n is string => n != null);
    onConfigRef.current?.({
      type,
      agg,
      x: colName(xIdx),
      y: names(ySel),
      seriesBy: colName(seriesBy),
      size: colName(sizeIdx),
      node: colName(nodeIdx),
      link: colName(linkIdx),
      label: colName(labelIdx),
      color: colName(colorIdx),
      graphSource,
      levels: names(levelIdxs),
    });
  }, [
    type,
    agg,
    xIdx,
    ySel,
    seriesBy,
    sizeIdx,
    nodeIdx,
    linkIdx,
    labelIdx,
    colorIdx,
    graphSource,
    levelIdxs,
    columns,
  ]);

  // A focus key belongs to a specific dataset/structure; drop it when the data
  // or the node/parent columns (or the hierarchy levels) change so it can't point
  // at a stale node.
  const focusResetKey = `${rows.length}:${nodeIdx}:${linkIdx}:${graphSource}:${levelIdxs.join(",")}`;
  const prevFocusResetKey = useRef(focusResetKey);
  if (prevFocusResetKey.current !== focusResetKey) {
    prevFocusResetKey.current = focusResetKey;
    if (focusNode !== null) setFocusNode(null);
  }

  const isGraph = type === "graph" || type === "tree";

  // Resolve selections defensively against the current columns, so switching
  // queries never references a stale index.
  const numericIdx = numeric.map((n) => n.i);
  const timeIdx = columns.map((c, i) => (c.type === "time" ? i : -1)).filter((i) => i >= 0);
  const isScatter = type === "scatter";
  const isBubble = type === "bubble";
  const isPoint = isScatter || isBubble; // x must be numeric (or a time) for point charts
  const xChoices = isPoint
    ? columns.map((_, i) => i).filter((i) => numericIdx.includes(i) || timeIdx.includes(i))
    : columns.map((_, i) => i);
  const x = xChoices.includes(xIdx) ? xIdx : (xChoices[0] ?? 0);
  // A time-typed X gets a real time axis (see the line/point branches below).
  const xIsTime = columns[x]?.type === "time";
  let ys = ySel.filter((i) => numericIdx.includes(i) && i !== (isPoint ? x : -1));
  if (ys.length === 0) ys = numericIdx.filter((i) => i !== x).slice(0, 1);
  // Pie and bubble show a single Y series.
  const series = type === "pie" || isBubble ? ys.slice(0, 1) : ys;
  const sizeCol = numericIdx.includes(sizeIdx) ? sizeIdx : -1;

  const exportPNG = () => {
    const canvas = canvasRef.current?.querySelector("canvas");
    if (canvas) downloadCanvasPNG(canvas, `turntable-${type}.png`);
  };

  // "Series by" splits one measure into a line/bar per category value. Only for
  // the cartesian category charts; the breakdown column must differ from X.
  const isHeatmap = type === "heatmap";
  const canSplit = type === "bar" || type === "line" || type === "area";
  const splitting = canSplit && seriesBy >= 0 && seriesBy < columns.length && seriesBy !== x;
  // A heatmap is a pivot too: X × (series-by → Y) coloured by one measure.
  const heatActive = isHeatmap && seriesBy >= 0 && seriesBy < columns.length && seriesBy !== x;
  const yMeasure = series[0];
  // Aggregation applies to the grouped/pivoted cartesian views (heatmap does its
  // own pivot; point charts plot raw rows).
  const grouping = agg !== "none" && !isPoint && !isHeatmap;

  const rawRows = useMemo(() => rows.slice(0, RAW_CAP), [rows]);
  const grouped = useMemo(
    () => (grouping && !splitting ? aggregate(rows, x, series, agg) : null),
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [rows, x, agg, grouping, splitting, series.join(",")],
  );
  const pivoted = useMemo(
    () => (splitting ? pivot(rows, x, yMeasure, seriesBy, agg) : null),
    [rows, x, yMeasure, seriesBy, agg, splitting],
  );
  const heat = useMemo(
    () => (heatActive ? pivot(rows, x, yMeasure, seriesBy, agg) : null),
    [rows, x, yMeasure, seriesBy, agg, heatActive],
  );

  // Graph/tree need a node + a link column, not a numeric measure, so they are
  // built and returned before the numeric-only guard below. The chart itself is
  // rendered by GraphChart, a memoized child: it builds the chart data/options
  // *fresh* (the force layout needs the per-render updates that react-chartjs-2
  // runs) but only re-renders when the inputs below actually change — so an
  // unrelated re-render (typing in the query editor) never touches the chart and
  // cannot blank it.
  if (isGraph) {
    const numIdx = numeric.map((n) => n.i);
    const nodeCol = nodeIdx < columns.length ? nodeIdx : 0;
    const linkCol = linkIdx < columns.length ? linkIdx : Math.min(1, columns.length - 1);
    const labelCol = labelIdx < columns.length ? labelIdx : -1;
    const gSizeCol = numIdx.includes(sizeIdx) ? sizeIdx : -1;
    const colorCol = colorIdx >= 0 && colorIdx < columns.length ? colorIdx : -1;
    // Path mode: `levels` (memoized above) is the ordered, clamped level list.
    const setLevel = (pos: number, col: number) =>
      setLevelIdxs(levels.map((v, i) => (i === pos ? col : v)));
    const addLevel = () => setLevelIdxs([...levels, Math.min(levels.length, columns.length - 1)]);
    const removeLevel = (pos: number) => setLevelIdxs(levels.filter((_, i) => i !== pos));

    return (
      <div className="chart">
        <div className="chart-controls">
          <div className="seg">
            {TYPES.map((t) => (
              <button key={t} className={type === t ? "on" : ""} onClick={() => setType(t)}>
                {t}
              </button>
            ))}
          </div>
          <div className="seg" title="edges: a self-referential parent column · hierarchy: an ordered list of columns">
            <button className={graphSource === "edges" ? "on" : ""} onClick={() => setGraphSource("edges")}>
              edges
            </button>
            <button className={graphSource === "path" ? "on" : ""} onClick={() => setGraphSource("path")}>
              hierarchy
            </button>
          </div>
          {graphSource === "path" ? (
            <>
              {levels.map((col, pos) => (
                <label className="chart-field" key={pos}>
                  {pos === 0 ? "levels" : "›"}
                  <select value={col} onChange={(e) => setLevel(pos, Number(e.target.value))}>
                    {columns.map((c, i) => (
                      <option key={i} value={i}>
                        {c.name}
                      </option>
                    ))}
                  </select>
                  {levels.length > 1 && (
                    <button className="ghost sm" title="remove level" onClick={() => removeLevel(pos)}>
                      ✕
                    </button>
                  )}
                </label>
              ))}
              {levels.length < columns.length && (
                <button className="ghost sm" title="add a level" onClick={addLevel}>
                  + level
                </button>
              )}
            </>
          ) : (
            <>
              <label className="chart-field">
                node
                <select value={nodeCol} onChange={(e) => setNodeIdx(Number(e.target.value))}>
                  {columns.map((c, i) => (
                    <option key={i} value={i}>
                      {c.name}
                    </option>
                  ))}
                </select>
              </label>
              <label className="chart-field">
                {type === "tree" ? "parent" : "links to"}
                <select value={linkCol} onChange={(e) => setLinkIdx(Number(e.target.value))}>
                  {columns.map((c, i) => (
                    <option key={i} value={i}>
                      {c.name}
                    </option>
                  ))}
                </select>
              </label>
              <label className="chart-field">
                label
                <select value={labelCol} onChange={(e) => setLabelIdx(Number(e.target.value))}>
                  <option value={-1}>(node)</option>
                  {columns.map((c, i) => (
                    <option key={i} value={i}>
                      {c.name}
                    </option>
                  ))}
                </select>
              </label>
            </>
          )}
          <label className="chart-field">
            size
            <select value={gSizeCol} onChange={(e) => setSizeIdx(Number(e.target.value))}>
              <option value={-1}>(constant)</option>
              {numeric.map(({ c, i }) => (
                <option key={i} value={i}>
                  {c.name}
                </option>
              ))}
            </select>
          </label>
          <label className="chart-field">
            color
            <select value={colorCol} onChange={(e) => setColorIdx(Number(e.target.value))}>
              <option value={-1}>(constant)</option>
              {columns.map((c, i) => (
                <option key={i} value={i}>
                  {c.name}
                </option>
              ))}
            </select>
          </label>
          <button className="ghost sm chart-png" title="download chart as PNG" onClick={exportPNG}>
            PNG
          </button>
        </div>
        <GraphChart
          canvasRef={canvasRef}
          rows={rows}
          columns={columns}
          numeric={numeric}
          type={type}
          graphSource={graphSource}
          levels={levels}
          nodeCol={nodeCol}
          linkCol={linkCol}
          labelCol={labelCol}
          sizeCol={gSizeCol}
          colorCol={colorCol}
          focusNode={focusNode}
          onFocus={setFocusNode}
        />
      </div>
    );
  }

  if (numeric.length === 0) {
    // The axis-based charts need a numeric column, but graph/tree do not (they map
    // a node + link column). Keep the type selector visible so those stay
    // reachable without a dummy numeric column.
    return (
      <div className="chart">
        <div className="chart-controls">
          <div className="seg">
            {TYPES.map((t) => (
              <button key={t} className={type === t ? "on" : ""} onClick={() => setType(t)}>
                {t}
              </button>
            ))}
          </div>
        </div>
        <div className="hint" style={{ padding: 12 }}>
          no numeric column to chart — select a numeric value, e.g.{" "}
          <code>COUNT(*)</code> or <code>SUM(...)</code>, or switch to{" "}
          <b>graph</b>/<b>tree</b> (which map a node + link column, no numeric
          needed).
        </div>
      </div>
    );
  }

  const toggleY = (i: number) =>
    setYSel((s) => (s.includes(i) ? s.filter((v) => v !== i) : [...s, i]));

  const seriesName = (idx: number) =>
    grouping ? `${agg}(${columns[idx]?.name})` : (columns[idx]?.name ?? "");

  // Unified series list for the cartesian/pie charts.
  let chartLabels: string[];
  let sers: { label: string; data: (number | null)[] }[];
  if (pivoted) {
    chartLabels = pivoted.labels;
    sers = pivoted.seriesKeys.map((sk, k) => ({ label: sk, data: pivoted.data[k] }));
  } else if (grouped) {
    chartLabels = grouped.labels;
    sers = series.map((idx, k) => ({ label: seriesName(idx), data: grouped.values[k] }));
  } else {
    chartLabels = rawRows.map((r) => labelOf(r[x]));
    sers = series.map((idx) => ({
      label: seriesName(idx),
      data: rawRows.map((r) => numOrNull(r[idx])),
    }));
  }

  // A time-typed X on a line/area chart gets a real time axis: points become
  // (epoch ms, y), so uneven sampling and time gaps render truthfully, and the
  // decimation plugin (LTTB) thins dense series for display — so ALL rows are
  // plotted, not just the first RAW_CAP. Bails to categorical labels when a
  // value doesn't parse as a time.
  let timeSers: { label: string; data: { x: number; y: number }[] }[] | null = null;
  if (xIsTime && (type === "line" || type === "area")) {
    if (grouped || pivoted) {
      // Grouped/pivoted labels are the time cells' label strings — parse back.
      const ms = chartLabels.map(timeMs);
      if (ms.every((m): m is number => m !== null)) {
        timeSers = sers.map((s) => ({
          label: s.label,
          data: ms
            .map((m, i) => ({ x: m, y: s.data[i] }))
            .filter((p): p is { x: number; y: number } => p.y !== null)
            .sort((a, b) => a.x - b.x),
        }));
      }
    } else {
      timeSers = series.map((idx) => {
        const pts: { x: number; y: number }[] = [];
        for (const r of rows) {
          const m = timeMs(r[x]);
          const yv = numOrNull(r[idx]);
          if (m !== null && yv !== null) pts.push({ x: m, y: yv });
        }
        pts.sort((a, b) => a.x - b.x);
        return { label: seriesName(idx), data: pts };
      });
      if (!timeSers.some((s) => s.data.length > 0)) timeSers = null;
    }
  }

  const showLegend = sers.length > 1 || type === "pie";
  const baseOptions: ChartOptions<"bar"> = {
    responsive: true,
    maintainAspectRatio: false,
    animation: { duration: 250 },
    interaction: { mode: "index", intersect: false },
    plugins: {
      legend: {
        display: showLegend,
        position: "bottom",
        labels: { boxWidth: 12, boxHeight: 12, padding: 14 },
      },
    },
    scales: {
      x: { grid: { color: GRID }, ticks: { maxRotation: 0, autoSkip: true } },
      y: { grid: { color: GRID }, beginAtZero: true },
    },
  };

  let chart;
  if (type === "pie") {
    const pieData = {
      labels: chartLabels,
      datasets: [
        {
          label: sers[0]?.label ?? "",
          data: sers[0]?.data ?? [],
          backgroundColor: chartLabels.map((_, i) => PALETTE[i % PALETTE.length]),
          borderColor: "#171a21",
          borderWidth: 1,
        },
      ],
    };
    const pieOptions: ChartOptions<"pie"> = {
      responsive: true,
      maintainAspectRatio: false,
      plugins: { legend: { position: "right", labels: { boxWidth: 12 } } },
    };
    chart = <Pie data={pieData} options={pieOptions} />;
  } else if (isScatter) {
    // A time X plots at epoch ms on a time scale; a numeric X stays linear.
    const px = (r: Cell[]) => (xIsTime ? timeMs(r[x]) : numOrNull(r[x]));
    const scatterData = {
      datasets: series.map((idx, k) => ({
        label: `${columns[idx]?.name} × ${columns[x]?.name}`,
        data: rawRows
          .map((r) => ({ x: px(r), y: numOrNull(r[idx]) }))
          .filter((p): p is { x: number; y: number } => p.x !== null && p.y !== null),
        backgroundColor: PALETTE[k % PALETTE.length],
      })),
    };
    const opts = {
      ...(baseOptions as ChartOptions<"scatter">),
      scales: {
        x: {
          type: xIsTime ? "time" : "linear",
          grid: { color: GRID },
          title: { display: true, text: columns[x]?.name },
        },
        y: { grid: { color: GRID } },
      },
    } as ChartOptions<"scatter">;
    chart = <Scatter data={scatterData} options={opts} />;
  } else if (isBubble) {
    const yIdx = series[0];
    const sizes = bubbleRadii(rawRows.map((r) => (sizeCol >= 0 ? numOrNull(r[sizeCol]) : null)));
    const points = rawRows
      .map((r, i) => ({
        x: xIsTime ? timeMs(r[x]) : numOrNull(r[x]),
        y: numOrNull(r[yIdx]),
        r: sizes[i],
      }))
      .filter((p): p is { x: number; y: number; r: number } => p.x !== null && p.y !== null);
    const bubbleData = {
      datasets: [
        {
          label:
            sizeCol >= 0
              ? `${columns[yIdx]?.name} × ${columns[x]?.name} (size: ${columns[sizeCol]?.name})`
              : `${columns[yIdx]?.name} × ${columns[x]?.name}`,
          data: points,
          backgroundColor: alpha(PALETTE[0], "99"),
          borderColor: PALETTE[0],
        },
      ],
    };
    const opts = {
      ...(baseOptions as ChartOptions<"bubble">),
      scales: {
        x: {
          type: xIsTime ? "time" : "linear",
          grid: { color: GRID },
          title: { display: true, text: columns[x]?.name },
        },
        y: {
          grid: { color: GRID },
          title: { display: true, text: columns[yIdx]?.name },
        },
      },
    } as ChartOptions<"bubble">;
    chart = <Bubble data={bubbleData} options={opts} />;
  } else if (isHeatmap) {
    if (!heat) {
      chart = (
        <div className="hint" style={{ padding: 12 }}>
          pick a <b>y axis</b> column and a numeric <b>value</b> to draw a heatmap.
        </div>
      );
    } else {
      const flat: { x: string; y: string; v: number | null }[] = [];
      let lo = Infinity;
      let hi = -Infinity;
      heat.labels.forEach((xk, xi) => {
        heat.seriesKeys.forEach((yk, yi) => {
          const v = heat.data[yi][xi];
          if (v !== null) {
            lo = Math.min(lo, v);
            hi = Math.max(hi, v);
          }
          flat.push({ x: xk, y: yk, v });
        });
      });
      if (!isFinite(lo)) {
        lo = 0;
        hi = 0;
      }
      const nx = heat.labels.length || 1;
      const ny = heat.seriesKeys.length || 1;
      // eslint-disable-next-line @typescript-eslint/no-explicit-any
      const matrixData: any = {
        datasets: [
          {
            label: columns[yMeasure]?.name ?? "",
            data: flat,
            backgroundColor: (c: { raw?: { v: number | null } }) =>
              heatColor(c.raw?.v ?? null, lo, hi),
            borderColor: "rgba(23,26,33,0.7)",
            borderWidth: 1,
            width: (c: { chart: { chartArea?: { width: number } } }) =>
              (c.chart.chartArea?.width ?? 0) / nx - 1,
            height: (c: { chart: { chartArea?: { height: number } } }) =>
              (c.chart.chartArea?.height ?? 0) / ny - 1,
          },
        ],
      };
      // eslint-disable-next-line @typescript-eslint/no-explicit-any
      const opts: any = {
        responsive: true,
        maintainAspectRatio: false,
        plugins: {
          legend: { display: false },
          tooltip: {
            callbacks: {
              title: () => "",
              label: (c: { raw: { x: string; y: string; v: number | null } }) =>
                `${c.raw.x} · ${c.raw.y}: ${c.raw.v ?? "—"}`,
            },
          },
        },
        scales: {
          x: {
            type: "category",
            labels: heat.labels,
            offset: true,
            grid: { display: false },
            ticks: { maxRotation: 0, autoSkip: true },
          },
          y: {
            type: "category",
            labels: heat.seriesKeys,
            offset: true,
            reverse: true,
            grid: { display: false },
          },
        },
      };
      chart = <ReactChart type="matrix" data={matrixData} options={opts} />;
    }
  } else if (timeSers) {
    const filled = type === "area";
    const timeData = {
      datasets: timeSers.map((s, k) => {
        const color = PALETTE[k % PALETTE.length];
        return {
          label: s.label,
          data: s.data,
          parsing: false as const, // pre-parsed {x: ms, y} points (decimation needs this)
          borderColor: color,
          backgroundColor: filled ? alpha(color, "33") : color,
          borderWidth: 2,
          fill: filled,
          tension: 0.3,
          spanGaps: true,
          pointRadius: 0, // dense series; hover still snaps to points
          pointHoverRadius: 4,
        };
      }),
    };
    const opts = {
      ...(baseOptions as ChartOptions<"line">),
      // Chart-LEVEL parsing:false is what arms the decimation plugin (a
      // dataset-level flag is not enough — the plugin checks chart.options).
      parsing: false as const,
      scales: {
        x: { type: "time", grid: { color: GRID }, ticks: { maxRotation: 0, autoSkip: true } },
        y: { grid: { color: GRID }, beginAtZero: true },
      },
      plugins: {
        ...baseOptions.plugins,
        // Above `threshold` points per series, LTTB thins to ~1 point/pixel
        // (default sample count) while preserving the visual shape — so
        // plotting every row stays cheap even for very large results.
        decimation: { enabled: true, algorithm: "lttb", threshold: 1000 },
      },
    } as ChartOptions<"line">;
    chart = <Line data={timeData} options={opts} />;
  } else {
    const filled = type === "area";
    const isLine = type === "line" || filled;
    const chartData = {
      labels: chartLabels,
      datasets: sers.map((s, k) => {
        const color = PALETTE[k % PALETTE.length];
        return {
          label: s.label,
          data: s.data,
          borderColor: color,
          backgroundColor: isLine ? (filled ? alpha(color, "33") : color) : alpha(color, "cc"),
          borderWidth: 2,
          fill: filled,
          tension: 0.3,
          spanGaps: true,
          pointRadius: isLine ? 2 : 0,
          pointHoverRadius: 4,
        };
      }),
    };
    chart = isLine ? (
      <Line data={chartData} options={baseOptions as ChartOptions<"line">} />
    ) : (
      <Bar data={chartData} options={baseOptions} />
    );
  }

  return (
    <div className="chart">
      <div className="chart-controls">
        <div className="seg">
          {TYPES.map((t) => (
            <button key={t} className={type === t ? "on" : ""} onClick={() => setType(t)}>
              {t}
            </button>
          ))}
        </div>
        <label className="chart-field">
          {isPoint ? "x (numeric/time)" : "x axis"}
          <select value={x} onChange={(e) => setXIdx(Number(e.target.value))}>
            {xChoices.map((i) => (
              <option key={i} value={i}>
                {columns[i]?.name}
              </option>
            ))}
          </select>
        </label>
        {isBubble && (
          <label className="chart-field">
            size
            <select value={sizeCol} onChange={(e) => setSizeIdx(Number(e.target.value))}>
              <option value={-1}>(constant)</option>
              {numeric
                .filter(({ i }) => i !== x)
                .map(({ c, i }) => (
                  <option key={i} value={i}>
                    {c.name}
                  </option>
                ))}
            </select>
          </label>
        )}
        {(canSplit || isHeatmap) && (
          <label className="chart-field">
            {isHeatmap ? "y axis" : "series by"}
            <select value={seriesBy} onChange={(e) => setSeriesBy(Number(e.target.value))}>
              <option value={-1}>{isHeatmap ? "(pick one)" : "(none)"}</option>
              {columns.map((c, i) =>
                i === x ? null : (
                  <option key={i} value={i}>
                    {c.name}
                  </option>
                ),
              )}
            </select>
          </label>
        )}
        {!isPoint && (
          <label className="chart-field">
            aggregate
            <select value={agg} onChange={(e) => setAgg(e.target.value as Agg)}>
              {AGGS.map((a) => (
                <option key={a} value={a}>
                  {a === "none" ? "none (raw rows)" : a}
                </option>
              ))}
            </select>
          </label>
        )}
        {splitting || isHeatmap ? (
          <label className="chart-field">
            value
            <select value={yMeasure} onChange={(e) => setYSel([Number(e.target.value)])}>
              {numeric.map(({ c, i }) => (
                <option key={i} value={i}>
                  {c.name}
                </option>
              ))}
            </select>
          </label>
        ) : (
          <div className="chart-field">
            {type === "pie" ? "value" : isBubble ? "y (numeric)" : "y series"}
            <div className="y-chips">
              {numeric
                .filter(({ i }) => !(isPoint && i === x))
                .map(({ c, i }) => (
                  <button
                    key={i}
                    className={`chip ${series.includes(i) ? "on" : ""}`}
                    onClick={() => toggleY(i)}
                    title={c.name}
                  >
                    {c.name}
                  </button>
                ))}
            </div>
          </div>
        )}
        <button
          className="ghost sm chart-png"
          title="download chart as PNG"
          onClick={exportPNG}
        >
          PNG
        </button>
      </div>

      <div className="chart-canvas" ref={canvasRef}>
        {chart}
      </div>

      {(pivoted ||
        (grouped && grouped.total > GROUP_CAP) ||
        (!grouping && !splitting && !timeSers && rows.length > RAW_CAP) ||
        (type === "pie" && ys.length > 1)) && (
        <div className="hint" style={{ padding: "4px 2px" }}>
          {pivoted &&
            pivoted.xTotal > GROUP_CAP &&
            `showing first ${GROUP_CAP} of ${pivoted.xTotal} x-values. `}
          {pivoted &&
            pivoted.sTotal > SERIES_CAP &&
            `showing first ${SERIES_CAP} of ${pivoted.sTotal} series. `}
          {grouped &&
            grouped.total > GROUP_CAP &&
            `showing first ${GROUP_CAP} of ${grouped.total} groups. `}
          {!grouping &&
            !splitting &&
            !timeSers &&
            rows.length > RAW_CAP &&
            `showing first ${RAW_CAP} of ${rows.length} rows — aggregate or add a tighter LIMIT. `}
          {type === "pie" && ys.length > 1 && "pie shows a single series."}
        </div>
      )}
    </div>
  );
}

// GraphChart renders a node-link graph/tree. It is memoized so it only re-renders
// when its inputs actually change — an unrelated parent re-render (e.g. typing in
// the query editor) is a no-op and cannot disturb the chart. On each real render
// it builds fresh data/options (the force layout relies on react-chartjs-2's
// per-render updates to run) and bumps a remount counter, so the chart is created
// anew rather than updated in place — the graph controller's in-place update is
// the path that blanks the canvas.
const GraphChart = memo(function GraphChart({
  canvasRef,
  rows,
  columns,
  numeric,
  type,
  graphSource,
  levels,
  nodeCol,
  linkCol,
  labelCol,
  sizeCol,
  colorCol,
  focusNode,
  onFocus,
}: {
  canvasRef: React.RefObject<HTMLDivElement>;
  rows: Cell[][];
  columns: Column[];
  numeric: { c: Column; i: number }[];
  type: "graph" | "tree";
  graphSource: "edges" | "path";
  levels: number[];
  nodeCol: number;
  linkCol: number;
  labelCol: number;
  sizeCol: number;
  colorCol: number;
  focusNode: string | null;
  onFocus: (key: string | null) => void;
}) {
  const renderCount = useRef(0);
  renderCount.current += 1;
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  const chartRef = useRef<any>(null);

  const numIdx = numeric.map((n) => n.i);
  const g =
    graphSource === "path"
      ? nodesEdgesFromPath(rows, levels, sizeCol, colorCol, type, focusNode, NODE_CAP)
      : nodesEdges(rows, nodeCol, linkCol, labelCol, sizeCol, colorCol, type, focusNode, NODE_CAP);
  const radii = bubbleRadii(g.nodes.map((n) => n.size));
  const showLabels = g.nodes.length <= LABEL_CAP;

  // Per-node colours + a legend describing them. A numeric colour column is a
  // value gradient (heatColor); any other column is categorical (a palette colour
  // per distinct value). Nodes with no value, and the synthetic root, get grey.
  const ROOT_COLOR = "rgba(138,147,167,0.35)";
  const NULL_COLOR = "rgba(138,147,167,0.45)";
  const colorNumeric = numIdx.includes(colorCol);
  let nodeColors: string[];
  let legend: GraphLegend = null;
  if (colorCol < 0) {
    nodeColors = g.nodes.map((_, i) => (i === g.rootIndex ? ROOT_COLOR : alpha(PALETTE[0], "cc")));
  } else if (colorNumeric) {
    const vals = g.nodes.map((n) => (n.color == null ? null : numOrNull(n.color)));
    const present = vals.filter((v): v is number => v !== null);
    const lo = present.length ? Math.min(...present) : 0;
    const hi = present.length ? Math.max(...present) : 0;
    nodeColors = g.nodes.map((_, i) =>
      i === g.rootIndex ? ROOT_COLOR : vals[i] === null ? NULL_COLOR : heatColor(vals[i], lo, hi),
    );
    legend = present.length ? { kind: "numeric", lo, hi, name: columns[colorCol]?.name ?? "" } : null;
  } else {
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
    nodeColors = g.nodes.map((n, i) =>
      i === g.rootIndex ? ROOT_COLOR : n.color == null ? NULL_COLOR : palette.get(labelOf(n.color))!,
    );
    legend = {
      kind: "cat",
      name: columns[colorCol]?.name ?? "",
      items: order.slice(0, 16).map((k) => ({ label: k, color: palette.get(k)! })),
      total: order.length,
    };
  }

  const graphData = {
    labels: g.nodes.map((n) => n.label),
    datasets: [
      {
        data: g.nodes.map(() => ({})),
        edges: g.edges,
        pointBackgroundColor: nodeColors,
        pointRadius: g.nodes.map((_, i) => (i === g.rootIndex ? 0 : radii[i])),
        pointHoverRadius: g.nodes.map((_, i) => (i === g.rootIndex ? 0 : radii[i] + 2)),
        borderColor: "rgba(138,147,167,0.35)",
        borderWidth: 1,
      },
    ],
  };
  const colorName = colorCol >= 0 ? columns[colorCol]?.name : "";
  // Click a node to drill into its subtree; the synthetic root is not focusable.
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  const onClick = (_evt: any, els: { index: number }[]) => {
    if (!els.length) return;
    const idx = els[0].index;
    if (idx === g.rootIndex) return;
    const key = g.nodes[idx]?.key;
    if (key) onFocus(key);
  };
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  const onHover = (evt: any, els: unknown[]) => {
    const t = evt?.native?.target;
    if (t) t.style.cursor = els.length ? "pointer" : "default";
  };

  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  const opts: any = {
    responsive: true,
    maintainAspectRatio: false,
    animation: { duration: 0 },
    layout: { padding: 24 },
    onClick,
    onHover,
    plugins: {
      legend: { display: false },
      datalabels: {
        display: (c: { dataIndex: number }) => showLabels && c.dataIndex !== g.rootIndex,
        formatter: (_v: unknown, c: { dataIndex: number }) => g.nodes[c.dataIndex]?.label,
        color: "#c8cdd8",
        font: { size: 10 },
        anchor: "end",
        align: "top",
        offset: 2,
        clip: true,
      },
      tooltip: {
        callbacks: {
          title: () => "",
          label: (c: { dataIndex: number }) => {
            const n = g.nodes[c.dataIndex];
            let s = n.label;
            if (colorCol >= 0 && n.color != null) s += ` · ${colorName}=${n.color}`;
            if (n.size !== null) s += ` (${n.size})`;
            return s;
          },
        },
      },
      // Scroll/pinch to zoom, drag to pan, into a region of the layout. The
      // zoomed scale range persists across the force layout's own updates.
      zoom: {
        pan: { enabled: true, mode: "xy" },
        zoom: { wheel: { enabled: true }, pinch: { enabled: true }, mode: "xy" },
      },
    },
    ...(type === "tree" ? { tree: { orientation: "horizontal" } } : {}),
    scales: { x: { display: false }, y: { display: false } },
  };

  return (
    <>
      <div className="graph-bar">
        <span className="hint" style={{ padding: 0 }}>
          {focusNode != null && !g.focusMissing
            ? "click a node to drill deeper · scroll to zoom · drag to pan"
            : "click a node to focus its subtree · scroll to zoom · drag to pan"}
        </span>
        <span style={{ flex: 1 }} />
        {focusNode != null && (
          <button className="ghost sm" onClick={() => onFocus(null)} title="show the whole graph">
            {g.focusMissing ? `focus ${focusNode} (not here) ✕` : `▸ ${focusNode} ✕`}
          </button>
        )}
        <button
          className="ghost sm"
          title="reset zoom / pan"
          onClick={() => chartRef.current?.resetZoom?.()}
        >
          reset view
        </button>
      </div>
      <div className="chart-canvas" ref={canvasRef}>
        {/* keyed by a per-render counter so each real change remounts a fresh
            chart (never an in-place update, which the graph controller mishandles). */}
        <ReactChart
          ref={chartRef}
          key={renderCount.current}
          type={type === "tree" ? "tree" : "forceDirectedGraph"}
          data={graphData}
          options={opts}
        />
      </div>
      {legend && <GraphColorLegend legend={legend} />}
      {g.total > NODE_CAP && (
        <div className="hint" style={{ padding: "4px 2px" }}>
          showing first {NODE_CAP} of {g.total} nodes — add a tighter <code>WHERE</code>/
          <code>LIMIT</code>.
          {!showLabels && " labels hidden (too many nodes); hover for detail."}
        </div>
      )}
    </>
  );
});

// GraphColorLegend renders the node-colour key below a graph/tree: palette
// swatches for a categorical colour column, or a gradient bar for a numeric one.
function GraphColorLegend({ legend }: { legend: Exclude<GraphLegend, null> }) {
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
