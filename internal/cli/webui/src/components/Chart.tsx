import { useMemo, useState } from "react";
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
import { Bar, Line, Scatter, Pie } from "react-chartjs-2";
import type { Cell, Column } from "../api";

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
);

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
const GROUP_CAP = 100; // groups (bars/slices) kept when aggregating

type ChartType = "bar" | "line" | "area" | "scatter" | "pie";
const TYPES: ChartType[] = ["bar", "line", "area", "scatter", "pie"];

type Agg = "none" | "count" | "sum" | "avg" | "min" | "max";
const AGGS: Agg[] = ["none", "count", "sum", "avg", "min", "max"];

const labelOf = (v: Cell) => (v === null || v === undefined ? "NULL" : String(v));
const alpha = (hex: string, a: string) => hex + a;

// numOrNull extracts a finite number, or null for null/blank/non-numeric cells
// (NB: Number(null) is 0, so a naive cast would wrongly count/sum nulls).
function numOrNull(v: Cell): number | null {
  if (typeof v === "number") return Number.isFinite(v) ? v : null;
  if (v === null || v === undefined || v === "") return null;
  const n = Number(v);
  return Number.isFinite(n) ? n : null;
}

function applyAgg(nums: number[], fn: Agg): number | null {
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
  const numeric = useMemo(
    () =>
      columns
        .map((c, i) => ({ c, i }))
        .filter(({ i }) => rows.some((r) => typeof r[i] === "number")),
    [columns, rows],
  );

  const [type, setType] = useState<ChartType>("bar");
  const [agg, setAgg] = useState<Agg>("none");
  const [xIdx, setXIdx] = useState(0);
  const [ySel, setYSel] = useState<number[]>([]);

  // Resolve selections defensively against the current columns, so switching
  // queries never references a stale index.
  const numericIdx = numeric.map((n) => n.i);
  const isScatter = type === "scatter";
  const xChoices = isScatter ? numericIdx : columns.map((_, i) => i);
  const x = xChoices.includes(xIdx) ? xIdx : (xChoices[0] ?? 0);
  let ys = ySel.filter((i) => numericIdx.includes(i) && i !== (isScatter ? x : -1));
  if (ys.length === 0) ys = numericIdx.filter((i) => i !== x).slice(0, 1);
  // Pie shows a single series.
  const series = type === "pie" ? ys.slice(0, 1) : ys;
  // Aggregation doesn't apply to a scatter (point) plot.
  const grouping = agg !== "none" && !isScatter;

  const rawRows = useMemo(() => rows.slice(0, RAW_CAP), [rows]);
  const grouped = useMemo(
    () => (grouping ? aggregate(rows, x, series, agg) : null),
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [rows, x, agg, grouping, series.join(",")],
  );

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

  const chartLabels = grouped ? grouped.labels : rawRows.map((r) => labelOf(r[x]));
  const valuesOf = (k: number): (number | null)[] =>
    grouped ? grouped.values[k] : rawRows.map((r) => numOrNull(r[series[k]]));
  const seriesName = (idx: number) =>
    grouping ? `${agg}(${columns[idx]?.name})` : (columns[idx]?.name ?? "");

  const baseOptions: ChartOptions<"bar"> = {
    responsive: true,
    maintainAspectRatio: false,
    animation: { duration: 250 },
    interaction: { mode: "index", intersect: false },
    plugins: {
      legend: {
        display: series.length > 1 || type === "pie",
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
          label: seriesName(series[0]),
          data: valuesOf(0),
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
  } else {
    const filled = type === "area";
    const isLine = type === "line" || filled;
    const chartData = {
      labels: chartLabels,
      datasets: series.map((idx, k) => {
        const color = PALETTE[k % PALETTE.length];
        return {
          label: seriesName(idx),
          data: valuesOf(k),
          borderColor: color,
          backgroundColor: isLine ? (filled ? alpha(color, "33") : color) : alpha(color, "cc"),
          borderWidth: 2,
          fill: filled,
          tension: 0.3,
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
          {isScatter ? "x (numeric)" : "x axis"}
          <select value={x} onChange={(e) => setXIdx(Number(e.target.value))}>
            {xChoices.map((i) => (
              <option key={i} value={i}>
                {columns[i]?.name}
              </option>
            ))}
          </select>
        </label>
        {!isScatter && (
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
        <div className="chart-field">
          {type === "pie" ? "value" : "y series"}
          <div className="y-chips">
            {numeric
              .filter(({ i }) => !(isScatter && i === x))
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
      </div>

      <div className="chart-canvas">{chart}</div>

      {((grouped && grouped.total > GROUP_CAP) ||
        (!grouping && rows.length > RAW_CAP) ||
        (type === "pie" && ys.length > 1)) && (
        <div className="hint" style={{ padding: "4px 2px" }}>
          {grouped && grouped.total > GROUP_CAP &&
            `showing first ${GROUP_CAP} of ${grouped.total} groups. `}
          {!grouping && rows.length > RAW_CAP &&
            `showing first ${RAW_CAP} of ${rows.length} rows — aggregate or add a tighter LIMIT. `}
          {type === "pie" && ys.length > 1 && "pie shows a single series."}
        </div>
      )}
    </div>
  );
}
