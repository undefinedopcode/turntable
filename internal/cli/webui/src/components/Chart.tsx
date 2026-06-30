import { useMemo, useRef, useState } from "react";
import {
  Chart as ChartJS,
  CategoryScale,
  LinearScale,
  BarElement,
  PointElement,
  LineElement,
  ArcElement,
  Tooltip,
  Legend,
  Filler,
  type ChartOptions,
} from "chart.js";
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
import { Bar, Line, Scatter, Pie, Bubble, Chart as ReactChart } from "react-chartjs-2";
import type { Cell, Column } from "../api";
import { downloadCanvasPNG } from "../export";
import {
  type Agg,
  AGGS,
  applyAgg,
  heatColor,
  labelOf,
  nodesEdges,
  numOrNull,
  numericColumns,
  pivot,
} from "../pivot";

ChartJS.register(
  CategoryScale,
  LinearScale,
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

type ChartType =
  | "bar"
  | "line"
  | "area"
  | "scatter"
  | "bubble"
  | "heatmap"
  | "pie"
  | "graph"
  | "tree";
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

export function Chart({ columns, rows }: { columns: Column[]; rows: Cell[][] }) {
  const numeric = useMemo(() => numericColumns(columns, rows), [columns, rows]);

  const [type, setType] = useState<ChartType>("bar");
  const [agg, setAgg] = useState<Agg>("none");
  const [xIdx, setXIdx] = useState(0);
  const [ySel, setYSel] = useState<number[]>([]);
  const [seriesBy, setSeriesBy] = useState(-1); // -1 = none (no breakdown)
  const [sizeIdx, setSizeIdx] = useState(-1); // bubble/node size measure (-1 = constant)
  const [nodeIdx, setNodeIdx] = useState(0); // graph: the node column
  const [linkIdx, setLinkIdx] = useState(1); // graph: the parent / links-to column
  const [labelIdx, setLabelIdx] = useState(-1); // graph: node label (-1 = node value)
  const canvasRef = useRef<HTMLDivElement>(null);

  const isGraph = type === "graph" || type === "tree";

  // Resolve selections defensively against the current columns, so switching
  // queries never references a stale index.
  const numericIdx = numeric.map((n) => n.i);
  const isScatter = type === "scatter";
  const isBubble = type === "bubble";
  const isPoint = isScatter || isBubble; // x must be numeric for point charts
  const xChoices = isPoint ? numericIdx : columns.map((_, i) => i);
  const x = xChoices.includes(xIdx) ? xIdx : (xChoices[0] ?? 0);
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

  // Build the graph/tree data+options once per input change and memoize them.
  // react-chartjs-2's update effects depend on the options / data.labels /
  // data.datasets *references*, so stable references mean an unrelated re-render
  // (e.g. typing in the query editor) does not trigger an in-place chart update —
  // which for the graph controller would blank the canvas. Real input changes
  // also bump the chart key below, remounting cleanly.
  const graphView = useMemo(() => {
    if (!isGraph) return null;
    const numIdx = numeric.map((n) => n.i);
    const nodeCol = nodeIdx < columns.length ? nodeIdx : 0;
    const linkCol = linkIdx < columns.length ? linkIdx : Math.min(1, columns.length - 1);
    const labelCol = labelIdx < columns.length ? labelIdx : -1;
    const sizeC = numIdx.includes(sizeIdx) ? sizeIdx : -1;
    const g = nodesEdges(rows, nodeCol, linkCol, labelCol, sizeC, type, NODE_CAP);
    const radii = bubbleRadii(g.nodes.map((n) => n.size));
    const showLabels = g.nodes.length <= LABEL_CAP;

    const graphData = {
      labels: g.nodes.map((n) => n.label),
      datasets: [
        {
          data: g.nodes.map(() => ({})),
          edges: g.edges,
          pointBackgroundColor: g.nodes.map((_, i) =>
            i === g.rootIndex ? "rgba(138,147,167,0.35)" : alpha(PALETTE[0], "cc"),
          ),
          pointRadius: g.nodes.map((_, i) => (i === g.rootIndex ? 0 : radii[i])),
          pointHoverRadius: g.nodes.map((_, i) => (i === g.rootIndex ? 0 : radii[i] + 2)),
          borderColor: "rgba(138,147,167,0.35)",
          borderWidth: 1,
        },
      ],
    };
    // eslint-disable-next-line @typescript-eslint/no-explicit-any
    const opts: any = {
      responsive: true,
      maintainAspectRatio: false,
      animation: { duration: 0 },
      layout: { padding: 24 },
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
              return n.size !== null ? `${n.label} (${n.size})` : n.label;
            },
          },
        },
      },
      ...(type === "tree" ? { tree: { orientation: "horizontal" } } : {}),
      scales: { x: { display: false }, y: { display: false } },
    };
    return { g, graphData, opts, nodeCol, linkCol, labelCol, sizeCol: sizeC, showLabels };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [isGraph, type, nodeIdx, linkIdx, labelIdx, sizeIdx, columns, rows, numeric]);

  // Remount the chart exactly when the graph's inputs change. graphView is a new
  // object only when an input (type/columns/rows/selections) changed, so bumping
  // a counter on that identity change gives a key that: remounts on any real
  // change (avoiding the graph controller's broken in-place update — including a
  // re-run that returns the same row count), and stays stable on unrelated
  // re-renders (typing in the editor), where the memoized data/options refs mean
  // react-chartjs-2 does nothing.
  const graphKeyRef = useRef(0);
  const prevGraphViewRef = useRef(graphView);
  if (prevGraphViewRef.current !== graphView) {
    prevGraphViewRef.current = graphView;
    graphKeyRef.current += 1;
  }

  // Graph/tree need a node + a link column, not a numeric measure, so they are
  // built and returned before the numeric-only guard below.
  if (isGraph && graphView) {
    const { g, graphData, opts, nodeCol, linkCol, labelCol, sizeCol: gSizeCol, showLabels } =
      graphView;

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
          <button className="ghost sm chart-png" title="download chart as PNG" onClick={exportPNG}>
            PNG
          </button>
        </div>
        <div className="chart-canvas" ref={canvasRef}>
          {/* see graphKeyRef above: remount on input change, stable otherwise. */}
          <ReactChart
            key={graphKeyRef.current}
            type={type === "tree" ? "tree" : "forceDirectedGraph"}
            data={graphData}
            options={opts}
          />
        </div>
        {g.total > NODE_CAP && (
          <div className="hint" style={{ padding: "4px 2px" }}>
            showing first {NODE_CAP} of {g.total} nodes — add a tighter{" "}
            <code>WHERE</code>/<code>LIMIT</code>.
            {!showLabels && " labels hidden (too many nodes); hover for detail."}
          </div>
        )}
      </div>
    );
  }

  if (numeric.length === 0) {
    return (
      <div className="hint" style={{ padding: 12 }}>
        no numeric column to chart — select a numeric value, e.g.{" "}
        <code>COUNT(*)</code> or <code>SUM(...)</code>.
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
    const scatterData = {
      datasets: series.map((idx, k) => ({
        label: `${columns[idx]?.name} × ${columns[x]?.name}`,
        data: rawRows
          .map((r) => ({ x: numOrNull(r[x]), y: numOrNull(r[idx]) }))
          .filter((p): p is { x: number; y: number } => p.x !== null && p.y !== null),
        backgroundColor: PALETTE[k % PALETTE.length],
      })),
    };
    const opts: ChartOptions<"scatter"> = {
      ...(baseOptions as ChartOptions<"scatter">),
      scales: {
        x: {
          type: "linear",
          grid: { color: GRID },
          title: { display: true, text: columns[x]?.name },
        },
        y: { grid: { color: GRID } },
      },
    };
    chart = <Scatter data={scatterData} options={opts} />;
  } else if (isBubble) {
    const yIdx = series[0];
    const sizes = bubbleRadii(rawRows.map((r) => (sizeCol >= 0 ? numOrNull(r[sizeCol]) : null)));
    const points = rawRows
      .map((r, i) => ({ x: numOrNull(r[x]), y: numOrNull(r[yIdx]), r: sizes[i] }))
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
    const opts: ChartOptions<"bubble"> = {
      ...(baseOptions as ChartOptions<"bubble">),
      scales: {
        x: {
          type: "linear",
          grid: { color: GRID },
          title: { display: true, text: columns[x]?.name },
        },
        y: {
          grid: { color: GRID },
          title: { display: true, text: columns[yIdx]?.name },
        },
      },
    };
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
          {isPoint ? "x (numeric)" : "x axis"}
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
        (!grouping && !splitting && rows.length > RAW_CAP) ||
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
            rows.length > RAW_CAP &&
            `showing first ${RAW_CAP} of ${rows.length} rows — aggregate or add a tighter LIMIT. `}
          {type === "pie" && ys.length > 1 && "pie shows a single series."}
        </div>
      )}
    </div>
  );
}
