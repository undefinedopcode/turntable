import { useMemo, useState } from "react";
import type { Cell, QueryResult } from "../api";
import type { ViewConfig } from "../view";
import { Modal } from "./Modal";
import { Chart } from "./Chart";
import { PivotTable } from "./PivotTable";
import {
  copyText,
  download,
  toCSV,
  toJSON,
  toNDJSON,
  toTSV,
} from "../export";

type SortDir = "asc" | "desc";

function cmp(a: Cell, b: Cell): number {
  if (a === null && b === null) return 0;
  if (a === null) return -1;
  if (b === null) return 1;
  if (typeof a === "number" && typeof b === "number") return a - b;
  return String(a).localeCompare(String(b), undefined, { numeric: true });
}

function cellText(c: Cell): string {
  return c === null ? "" : typeof c === "object" ? JSON.stringify(c) : String(c);
}

// Results renders one tab's query result. `view` is the tab's persisted view
// config (read once at mount — tab switches remount via key={activeId}); every
// change is reported back through `onView` so it survives a reload. The row
// filter/sort stay transient — they belong to one specific result.
export function Results({
  result,
  view,
  onView,
}: {
  result: QueryResult | null;
  view?: ViewConfig;
  onView?: (patch: Partial<ViewConfig>) => void;
}) {
  const [filter, setFilter] = useState("");
  const [sortCol, setSortCol] = useState<number | null>(null);
  const [sortDir, setSortDir] = useState<SortDir>("asc");
  const [mode, setMode] = useState<"table" | "chart" | "pivot">(view?.mode ?? "table");
  const [expand, setExpand] = useState<Cell | null>(null);
  const [copied, setCopied] = useState("");

  const switchMode = (m: "table" | "chart" | "pivot") => {
    setMode(m);
    onView?.({ mode: m });
  };

  // A row-less response (error / notice / explain) has no columns field, so
  // guard the length access with optional chaining.
  const isTable =
    !!result &&
    !result.error &&
    result.explain == null &&
    result.notice == null &&
    (result.columns?.length ?? 0) > 0;

  // Derive the displayed rows: filter (substring over all cells), then sort.
  const rows = useMemo(() => {
    if (!isTable || !result) return [];
    let rs = result.rows;
    const f = filter.trim().toLowerCase();
    if (f) {
      rs = rs.filter((row) => row.some((c) => cellText(c).toLowerCase().includes(f)));
    }
    if (sortCol !== null) {
      rs = [...rs].sort((x, y) => {
        const d = cmp(x[sortCol], y[sortCol]);
        return sortDir === "asc" ? d : -d;
      });
    }
    return rs;
  }, [isTable, result, filter, sortCol, sortDir]);

  if (!result) return <div className="results" />;

  if (result.error) {
    return (
      <div className="results">
        <div className="banner err">{result.error}</div>
      </div>
    );
  }

  if (result.notice != null) {
    return (
      <div className="results">
        <div className="banner ok">{result.notice}</div>
      </div>
    );
  }

  if (result.explain != null) {
    return (
      <div className="results">
        <pre className="plan">{result.explain}</pre>
      </div>
    );
  }

  const cols = result.columns;

  const onSort = (i: number) => {
    if (sortCol !== i) {
      setSortCol(i);
      setSortDir("asc");
    } else if (sortDir === "asc") {
      setSortDir("desc");
    } else {
      setSortCol(null);
    }
  };

  const flash = (msg: string) => {
    setCopied(msg);
    setTimeout(() => setCopied(""), 1100);
  };

  const onCell = (c: Cell) => {
    if (c !== null && typeof c === "object") {
      setExpand(c);
      return;
    }
    copyText(cellText(c)).then(() => flash("cell copied"));
  };

  const exportAs = (fmt: "csv" | "json" | "ndjson" | "tsv") => {
    const fns = { csv: toCSV, json: toJSON, ndjson: toNDJSON, tsv: toTSV };
    const text = fns[fmt](cols, rows);
    if (fmt === "tsv") {
      copyText(text).then(() => flash("copied to clipboard"));
      return;
    }
    const ext = fmt;
    const mime = fmt === "csv" ? "text/csv" : "application/json";
    download(text, `turntable.${ext}`, mime);
  };

  return (
    <div className="results">
      {result.truncated && (
        <div className="banner note">
          results truncated to {result.count} rows (raise with --max-rows)
        </div>
      )}

      <div className="result-bar">
        <input
          className="filter"
          placeholder="filter rows…"
          value={filter}
          onChange={(e) => setFilter(e.target.value)}
        />
        <span className="rowcount">
          {rows.length}
          {rows.length !== result.count ? ` / ${result.count}` : ""} rows
        </span>
        <div className="seg">
          <button
            className={mode === "table" ? "on" : ""}
            onClick={() => switchMode("table")}
          >
            Table
          </button>
          <button
            className={mode === "chart" ? "on" : ""}
            onClick={() => switchMode("chart")}
          >
            Chart
          </button>
          <button
            className={mode === "pivot" ? "on" : ""}
            onClick={() => switchMode("pivot")}
          >
            Pivot
          </button>
        </div>
        <div className="exports">
          <button className="ghost sm" onClick={() => exportAs("csv")}>
            CSV
          </button>
          <button className="ghost sm" onClick={() => exportAs("json")}>
            JSON
          </button>
          <button className="ghost sm" onClick={() => exportAs("ndjson")}>
            NDJSON
          </button>
          <button
            className="ghost sm"
            title="copy all (TSV) to clipboard"
            onClick={() => exportAs("tsv")}
          >
            Copy
          </button>
        </div>
        {copied && <span className="copied">{copied}</span>}
      </div>

      {mode === "chart" ? (
        <Chart
          columns={cols}
          rows={rows}
          config={view?.chart}
          onConfig={(c) => onView?.({ chart: c })}
        />
      ) : mode === "pivot" ? (
        <PivotTable
          columns={cols}
          rows={rows}
          config={view?.pivot}
          onConfig={(c) => onView?.({ pivot: c })}
        />
      ) : (
        <div className="table-wrap">
          <table>
            <thead>
              <tr>
                {cols.map((c, i) => (
                  <th
                    key={c.name}
                    className={c.type === "int" || c.type === "float" ? "num" : ""}
                    onClick={() => onSort(i)}
                    title="click to sort"
                  >
                    {c.name}
                    <span className="ty">{c.type}</span>
                    {sortCol === i && (
                      <span className="arrow">{sortDir === "asc" ? " ▲" : " ▼"}</span>
                    )}
                  </th>
                ))}
              </tr>
            </thead>
            <tbody>
              {rows.map((row, i) => (
                <tr key={i}>
                  {row.map((cell, j) => (
                    <Td key={j} cell={cell} onClick={() => onCell(cell)} />
                  ))}
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}

      <Modal open={expand !== null} title="cell" onClose={() => setExpand(null)}>
        <pre className="plan" style={{ margin: 8 }}>
          {expand !== null ? JSON.stringify(expand, null, 2) : ""}
        </pre>
        <div className="modal-actions" style={{ padding: "0 8px 8px" }}>
          <button
            onClick={() =>
              expand !== null &&
              copyText(JSON.stringify(expand)).then(() => flash("copied"))
            }
          >
            Copy JSON
          </button>
        </div>
      </Modal>
    </div>
  );
}

function Td({ cell, onClick }: { cell: Cell; onClick: () => void }) {
  if (cell === null)
    return (
      <td className="null" onClick={onClick}>
        NULL
      </td>
    );
  if (typeof cell === "number")
    return (
      <td className="num clk" onClick={onClick} title="click to copy">
        {String(cell)}
      </td>
    );
  if (typeof cell === "object")
    return (
      <td className="json clk" onClick={onClick} title="click to expand">
        {JSON.stringify(cell)}
      </td>
    );
  return (
    <td className="clk" onClick={onClick} title="click to copy">
      {String(cell)}
    </td>
  );
}
