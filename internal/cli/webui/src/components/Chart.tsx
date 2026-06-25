import { useMemo, useState } from "react";
import type { Cell, Column } from "../api";

const MAX_BARS = 60;

// Chart renders a simple horizontal bar chart of one numeric column grouped by a
// category column. Hand-rolled (no chart dependency).
export function Chart({ columns, rows }: { columns: Column[]; rows: Cell[][] }) {
  const numericCols = columns
    .map((c, i) => ({ c, i }))
    .filter(({ i }) => rows.some((r) => typeof r[i] === "number"));

  const [labelIdx, setLabelIdx] = useState(0);
  const [valueIdx, setValueIdx] = useState(
    numericCols.length ? numericCols[0].i : 0,
  );

  const bars = useMemo(() => {
    const out = rows.slice(0, MAX_BARS).map((r) => {
      const v = r[valueIdx];
      return {
        label: r[labelIdx] === null ? "NULL" : String(r[labelIdx]),
        value: typeof v === "number" ? v : Number(v) || 0,
      };
    });
    return out;
  }, [rows, labelIdx, valueIdx]);

  if (numericCols.length === 0) {
    return (
      <div className="hint" style={{ padding: 12 }}>
        no numeric column to chart — select a numeric value, e.g.{" "}
        <code>COUNT(*)</code> or <code>SUM(...)</code>.
      </div>
    );
  }

  const max = Math.max(0, ...bars.map((b) => Math.abs(b.value)));

  return (
    <div className="chart">
      <div className="chart-axes">
        <label>
          category
          <select
            value={labelIdx}
            onChange={(e) => setLabelIdx(Number(e.target.value))}
          >
            {columns.map((c, i) => (
              <option key={c.name} value={i}>
                {c.name}
              </option>
            ))}
          </select>
        </label>
        <label>
          value
          <select
            value={valueIdx}
            onChange={(e) => setValueIdx(Number(e.target.value))}
          >
            {numericCols.map(({ c, i }) => (
              <option key={c.name} value={i}>
                {c.name}
              </option>
            ))}
          </select>
        </label>
      </div>
      <div className="bars">
        {bars.map((b, i) => (
          <div className="bar-row" key={i} title={`${b.label}: ${b.value}`}>
            <span className="bar-label">{b.label}</span>
            <span className="bar-track">
              <span
                className="bar-fill"
                style={{ width: max ? `${(Math.abs(b.value) / max) * 100}%` : "0" }}
              />
            </span>
            <span className="bar-value">{b.value}</span>
          </div>
        ))}
      </div>
      {rows.length > MAX_BARS && (
        <div className="hint" style={{ padding: "6px 2px" }}>
          showing first {MAX_BARS} of {rows.length} rows — add a tighter{" "}
          <code>LIMIT</code> or aggregate
        </div>
      )}
    </div>
  );
}
