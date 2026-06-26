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
const MAX_POINTS = 500;

type ChartType = "bar" | "line" | "area" | "scatter" | "pie";
const TYPES: ChartType[] = ["bar", "line", "area", "scatter", "pie"];

const toNum = (v: Cell) => (typeof v === "number" ? v : Number(v));
const labelOf = (v: Cell) => (v === null || v === undefined ? "NULL" : String(v));
const alpha = (hex: string, a: string) => hex + a;

export function Chart({ columns, rows }: { columns: Column[]; rows: Cell[][] }) {
  const numeric = useMemo(
    () =>
      columns
        .map((c, i) => ({ c, i }))
        .filter(({ i }) => rows.some((r) => typeof r[i] === "number")),
    [columns, rows],
  );

  const [type, setType] = useState<ChartType>("bar");
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

  const data = useMemo(() => rows.slice(0, MAX_POINTS), [rows]);
  const labels = useMemo(() => data.map((r) => labelOf(r[x])), [data, x]);

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
    const idx = series[0];
    const pieData = {
      labels,
      datasets: [
        {
          data: data.map((r) => toNum(r[idx])),
          backgroundColor: labels.map((_, i) => PALETTE[i % PALETTE.length]),
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
        data: data.map((r) => ({ x: toNum(r[x]), y: toNum(r[idx]) })),
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
      labels,
      datasets: series.map((idx, k) => {
        const color = PALETTE[k % PALETTE.length];
        return {
          label: columns[idx]?.name ?? "",
          data: data.map((r) => toNum(r[idx])),
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

      {(rows.length > MAX_POINTS || (type === "pie" && ys.length > 1)) && (
        <div className="hint" style={{ padding: "4px 2px" }}>
          {rows.length > MAX_POINTS &&
            `showing first ${MAX_POINTS} of ${rows.length} rows — add a tighter LIMIT or aggregate. `}
          {type === "pie" && ys.length > 1 && "pie shows a single series."}
        </div>
      )}
    </div>
  );
}
