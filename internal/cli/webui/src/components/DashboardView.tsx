import { useEffect, useMemo, useState } from "react";
import {
  getDashboard,
  runQuery,
  type Cell,
  type Dashboard,
  type DashboardPanel,
  type QueryResult,
} from "../api";
import { Chart } from "./Chart";
import { PivotTable } from "./PivotTable";
import { Markdown } from "./Markdown";

// substituteVars replaces {{name}} with a quoted SQL string literal (' doubled)
// and {{name:raw}} with the raw value (for INTERVAL/identifier positions). An
// unknown variable is left as-is so the mistake stays visible in the error.
export function substituteVars(q: string, vars: Record<string, string>): string {
  return q.replace(/\{\{\s*(\w+)(:raw)?\s*\}\}/g, (m, name: string, raw?: string) => {
    const v = vars[name];
    if (v === undefined) return m;
    return raw ? v : `'${v.replace(/'/g, "''")}'`;
  });
}

// DashboardView renders one dashboard: header (name + variables toolbar +
// run-all), then the panels top-to-bottom (width:half pairs share a row). The
// definition comes from the server; each panel runs its query through the
// normal /api/query path client-side.
export function DashboardView({ slug, onClose }: { slug: string; onClose: () => void }) {
  const [dash, setDash] = useState<(Dashboard & { slug: string }) | null>(null);
  const [error, setError] = useState("");
  const [vars, setVars] = useState<Record<string, string>>({});
  const [options, setOptions] = useState<Record<string, string[]>>({});
  const [runId, setRunId] = useState(0); // bump to re-run every panel

  useEffect(() => {
    setDash(null);
    setError("");
    setVars({});
    setOptions({});
    getDashboard(slug)
      .then((d) => {
        setDash(d);
        const init: Record<string, string> = {};
        for (const [k, v] of Object.entries(d.variables ?? {})) init[k] = v.default ?? "";
        setVars(init);
        // Populate each variable's dropdown from its options_query (first column).
        for (const [k, v] of Object.entries(d.variables ?? {})) {
          if (!v.options_query) continue;
          runQuery(v.options_query)
            .then((r) => {
              if (r.error || !r.rows?.length) return;
              const opts = r.rows.map((row: Cell[]) => String(row[0] ?? ""));
              setOptions((o) => ({ ...o, [k]: opts }));
            })
            .catch(() => {});
        }
      })
      .catch((e) => setError(String(e)));
  }, [slug]);

  const varNames = useMemo(() => Object.keys(dash?.variables ?? {}).sort(), [dash]);
  const setVar = (k: string, v: string) => setVars((old) => ({ ...old, [k]: v }));

  // Auto-refresh: a dashboard with `refresh: N` re-runs every panel every N
  // seconds while open.
  const refresh = dash?.refresh ?? 0;
  useEffect(() => {
    if (!(refresh > 0)) return;
    const id = setInterval(() => setRunId((n) => n + 1), refresh * 1000);
    return () => clearInterval(id);
  }, [refresh]);

  if (error) {
    return (
      <div className="dash-view">
        <div className="dash-head">
          <span style={{ flex: 1 }} />
          <button className="ghost sm" onClick={onClose}>✕ close</button>
        </div>
        <div className="banner err">{error}</div>
      </div>
    );
  }
  if (!dash) {
    return (
      <div className="dash-view">
        <div className="hint" style={{ padding: 12 }}>loading…</div>
      </div>
    );
  }

  return (
    <div className="dash-view">
      <div className="dash-head">
        <div>
          <h1>{dash.name}</h1>
          {dash.description && <div className="dash-desc">{dash.description}</div>}
        </div>
        <span style={{ flex: 1 }} />
        {varNames.map((k) => (
          <label className="chart-field" key={k}>
            {k}
            {options[k] ? (
              <select value={vars[k] ?? ""} onChange={(e) => setVar(k, e.target.value)}>
                {!options[k].includes(vars[k] ?? "") && (
                  <option value={vars[k] ?? ""}>{vars[k]}</option>
                )}
                {options[k].map((o) => (
                  <option key={o} value={o}>
                    {o}
                  </option>
                ))}
              </select>
            ) : (
              <VarInput value={vars[k] ?? ""} onCommit={(v) => setVar(k, v)} />
            )}
          </label>
        ))}
        <button
          className="ghost sm"
          onClick={() => setRunId((n) => n + 1)}
          title="re-run every panel"
        >
          ↻ run all
        </button>
        <button className="ghost sm" onClick={onClose} title="back to queries">
          ✕ close
        </button>
      </div>
      <div className="dash-panels">
        {dash.panels.map((p, i) => (
          <DashPanel
            key={`${slug}:${i}`}
            panel={p}
            query={p.query ? substituteVars(p.query, vars) : ""}
            runId={runId}
          />
        ))}
        {dash.panels.length === 0 && (
          <div className="hint" style={{ padding: 12 }}>
            no panels yet — run a query in a tab and use <b>Pin</b> on its result.
          </div>
        )}
      </div>
    </div>
  );
}

// VarInput edits a text variable, committing on Enter/blur (not per keystroke,
// which would re-run every panel while typing).
function VarInput({ value, onCommit }: { value: string; onCommit: (v: string) => void }) {
  const [v, setV] = useState(value);
  useEffect(() => setV(value), [value]);
  return (
    <input
      className="dash-var"
      value={v}
      onChange={(e) => setV(e.target.value)}
      onBlur={() => v !== value && onCommit(v)}
      onKeyDown={(e) => {
        if (e.key === "Enter") onCommit(v);
      }}
    />
  );
}

// DashPanel runs one panel's (already-substituted) query and renders it by
// kind, reusing the results-pane components with the panel's frozen view
// config. A changed query (variable edit) or a runId bump re-fetches.
function DashPanel({
  panel,
  query,
  runId,
}: {
  panel: DashboardPanel;
  query: string;
  runId: number;
}) {
  const [result, setResult] = useState<QueryResult | null>(null);
  const [running, setRunning] = useState(false);

  useEffect(() => {
    if (!query) return;
    let cancelled = false;
    setRunning(true);
    runQuery(query)
      .then((r) => !cancelled && setResult(r))
      .catch(
        (e) =>
          !cancelled &&
          setResult({ columns: [], rows: [], count: 0, elapsed_ms: 0, error: String(e) }),
      )
      .finally(() => !cancelled && setRunning(false));
    return () => {
      cancelled = true;
    };
  }, [query, runId]);

  const width = panel.width === "half" ? "half" : "full";

  if (panel.kind === "markdown") {
    return (
      <div className={`dash-panel md ${width}`}>
        <Markdown text={panel.text ?? ""} />
      </div>
    );
  }

  let body;
  if (result?.error) {
    body = <div className="banner err">{result.error}</div>;
  } else if (!result || !(result.columns?.length > 0)) {
    body = (
      <div className="hint" style={{ padding: 12 }}>
        {running ? "running…" : "no result"}
      </div>
    );
  } else if (panel.kind === "stat") {
    body = <Stat result={result} />;
  } else if (panel.kind === "chart") {
    body = <Chart columns={result.columns} rows={result.rows ?? []} config={panel.view?.chart} />;
  } else if (panel.kind === "pivot") {
    body = <PivotTable columns={result.columns} rows={result.rows ?? []} config={panel.view?.pivot} />;
  } else {
    body = <PanelTable result={result} />;
  }

  return (
    <div className={`dash-panel ${width}`}>
      <div className="dash-panel-head">
        <span className="dash-panel-title">{panel.title ?? ""}</span>
        <span className="dash-panel-status">
          {running
            ? "running…"
            : result && !result.error
              ? `${result.count} rows · ${result.elapsed_ms} ms`
              : ""}
        </span>
      </div>
      {body}
    </div>
  );
}

const fmtStat = (n: number) =>
  Number.isInteger(n)
    ? n.toLocaleString()
    : n.toLocaleString(undefined, { maximumFractionDigits: 2 });

// Stat renders the first cell of the first row, big, captioned by its column
// name — the classic dashboard "big number".
function Stat({ result }: { result: QueryResult }) {
  const v = result.rows?.[0]?.[0];
  const label = result.columns[0]?.name ?? "";
  const text =
    v === null || v === undefined ? "—" : typeof v === "number" ? fmtStat(v) : String(v);
  return (
    <div className="stat">
      <div className="stat-value">{text}</div>
      <div className="stat-label">{label}</div>
    </div>
  );
}

// PanelTable is a plain, read-only row table (the interactive sort/filter/export
// table lives in the Results pane; a dashboard panel just shows the data).
const PANEL_ROW_CAP = 200;
function PanelTable({ result }: { result: QueryResult }) {
  const all = result.rows ?? [];
  const rows = all.slice(0, PANEL_ROW_CAP);
  return (
    <>
      <div className="table-wrap">
        <table>
          <thead>
            <tr>
              {result.columns.map((c) => (
                <th key={c.name} className={c.type === "int" || c.type === "float" ? "num" : ""}>
                  {c.name}
                  <span className="ty">{c.type}</span>
                </th>
              ))}
            </tr>
          </thead>
          <tbody>
            {rows.map((row, i) => (
              <tr key={i}>
                {row.map((cell, j) => (
                  <td
                    key={j}
                    className={typeof cell === "number" ? "num" : cell === null ? "null" : ""}
                  >
                    {cell === null
                      ? "NULL"
                      : typeof cell === "object"
                        ? JSON.stringify(cell)
                        : String(cell)}
                  </td>
                ))}
              </tr>
            ))}
          </tbody>
        </table>
      </div>
      {all.length > PANEL_ROW_CAP && (
        <div className="hint" style={{ padding: "4px 2px" }}>
          showing first {PANEL_ROW_CAP} of {result.count} rows.
        </div>
      )}
    </>
  );
}
