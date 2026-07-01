import { useEffect, useMemo, useRef, useState } from "react";
import type { Cell, Column } from "../api";
import { type Agg, AGGS, heatColor, numericColumns, pivot } from "../pivot";
import type { PivotViewConfig } from "../view";

const ROW_CAP = 300;
const COL_CAP = 60;

// fmtNum keeps integers exact and trims long decimals (e.g. avg) to 2 places.
function fmtNum(v: number | null): string {
  if (v === null) return "";
  if (Number.isInteger(v)) return String(v);
  return v.toFixed(2);
}

// PivotTable cross-tabulates the result: a row dimension × a column dimension,
// each cell an aggregate of one measure. It reuses the chart's pivot() so the
// "series by" chart and this table agree. Cells can be colour-scaled (a
// heatmap-in-a-table). `config` seeds the selections at mount (by column name);
// every change is reported through `onConfig` so it can be persisted.
export function PivotTable({
  columns,
  rows,
  config,
  onConfig,
}: {
  columns: Column[];
  rows: Cell[][];
  config?: PivotViewConfig;
  onConfig?: (c: PivotViewConfig) => void;
}) {
  const numeric = useMemo(() => numericColumns(columns, rows), [columns, rows]);
  const numericIdx = numeric.map((n) => n.i);

  // Resolve a persisted column name to its current index (fallback when absent).
  const byName = (name: string | undefined, fallback: number) => {
    const i = name == null ? -1 : columns.findIndex((c) => c.name === name);
    return i >= 0 ? i : fallback;
  };
  const [rowDim, setRowDim] = useState(() => byName(config?.rows, 0));
  const [colDim, setColDim] = useState(() => byName(config?.cols, 1));
  const [valIdx, setValIdx] = useState(() => byName(config?.value, -1));
  const [agg, setAgg] = useState<Agg>(() =>
    config?.agg && AGGS.includes(config.agg) && config.agg !== "none" ? config.agg : "sum",
  );
  const [color, setColor] = useState(config?.color ?? true);

  // Resolve selections defensively against the current columns.
  const rd = rowDim >= 0 && rowDim < columns.length ? rowDim : 0;
  const cd =
    colDim >= 0 && colDim < columns.length && colDim !== rd
      ? colDim
      : columns.findIndex((_, i) => i !== rd);
  const value = numericIdx.includes(valIdx) ? valIdx : (numericIdx[0] ?? -1);

  // Report the (resolved) selections upward, by column name, whenever they
  // change. onConfig goes through a ref so an unstable callback prop doesn't
  // re-fire the effect.
  const onConfigRef = useRef(onConfig);
  onConfigRef.current = onConfig;
  useEffect(() => {
    onConfigRef.current?.({
      rows: columns[rd]?.name,
      cols: columns[cd]?.name,
      value: columns[value]?.name,
      agg,
      color,
    });
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [rd, cd, value, agg, color, columns]);

  const p = useMemo(
    () => (value >= 0 && cd >= 0 ? pivot(rows, cd, value, rd, agg, COL_CAP, ROW_CAP) : null),
    [rows, cd, value, rd, agg],
  );

  const [lo, hi] = useMemo(() => {
    if (!p) return [0, 0];
    let mn = Infinity;
    let mx = -Infinity;
    for (const r of p.data)
      for (const v of r)
        if (v !== null) {
          mn = Math.min(mn, v);
          mx = Math.max(mx, v);
        }
    return isFinite(mn) ? [mn, mx] : [0, 0];
  }, [p]);

  if (numeric.length === 0 || columns.length < 2) {
    return (
      <div className="hint" style={{ padding: 12 }}>
        a pivot needs at least two columns and one numeric measure.
      </div>
    );
  }

  return (
    <div className="pivot">
      <div className="chart-controls">
        <label className="chart-field">
          rows
          <select value={rd} onChange={(e) => setRowDim(Number(e.target.value))}>
            {columns.map((c, i) => (
              <option key={i} value={i}>
                {c.name}
              </option>
            ))}
          </select>
        </label>
        <label className="chart-field">
          columns
          <select value={cd} onChange={(e) => setColDim(Number(e.target.value))}>
            {columns.map((c, i) =>
              i === rd ? null : (
                <option key={i} value={i}>
                  {c.name}
                </option>
              ),
            )}
          </select>
        </label>
        <label className="chart-field">
          value
          <select value={value} onChange={(e) => setValIdx(Number(e.target.value))}>
            {numeric.map(({ c, i }) => (
              <option key={i} value={i}>
                {c.name}
              </option>
            ))}
          </select>
        </label>
        <label className="chart-field">
          aggregate
          <select value={agg} onChange={(e) => setAgg(e.target.value as Agg)}>
            {AGGS.filter((a) => a !== "none").map((a) => (
              <option key={a} value={a}>
                {a}
              </option>
            ))}
          </select>
        </label>
        <label className="chart-field pivot-color">
          <input type="checkbox" checked={color} onChange={(e) => setColor(e.target.checked)} />
          color
        </label>
      </div>

      <div className="table-wrap">
        {p && (
          <table className="pivot-table">
            <thead>
              <tr>
                <th className="pivot-corner">
                  {columns[rd]?.name} \ {columns[cd]?.name}
                </th>
                {p.labels.map((l) => (
                  <th key={l} title={l}>
                    {l}
                  </th>
                ))}
              </tr>
            </thead>
            <tbody>
              {p.seriesKeys.map((rk, ri) => (
                <tr key={rk}>
                  <th className="pivot-rowhead" title={rk}>
                    {rk}
                  </th>
                  {p.labels.map((_, ci) => {
                    const v = p.data[ri][ci];
                    return (
                      <td
                        key={ci}
                        className="num"
                        style={color ? { background: heatColor(v, lo, hi, 0.5) } : undefined}
                      >
                        {fmtNum(v)}
                      </td>
                    );
                  })}
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </div>

      {p && (p.xTotal > COL_CAP || p.sTotal > ROW_CAP) && (
        <div className="hint" style={{ padding: "4px 2px" }}>
          {p.xTotal > COL_CAP && `showing first ${COL_CAP} of ${p.xTotal} columns. `}
          {p.sTotal > ROW_CAP && `showing first ${ROW_CAP} of ${p.sTotal} rows. `}
        </div>
      )}
    </div>
  );
}
