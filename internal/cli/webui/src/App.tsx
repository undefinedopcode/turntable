import { useCallback, useEffect, useRef, useState } from "react";
import type { Completion } from "@codemirror/autocomplete";
import { runQuery, type QueryResult } from "./api";
import { Sidebar } from "./components/Sidebar";
import { Editor, type EditorHandle } from "./components/Editor";
import { Results } from "./components/Results";
import {
  loadLastQuery,
  loadPaneSize,
  pushHistory,
  savePaneSize,
  saveLastQuery,
} from "./storage";
import { buildCompletions } from "./completions";
import { Gutter } from "./components/Gutter";

const clamp = (v: number, lo: number, hi: number) =>
  Math.max(lo, Math.min(hi, v));

export function App() {
  const [query, setQuery] = useState(() => loadLastQuery());
  const [result, setResult] = useState<QueryResult | null>(null);
  const [status, setStatus] = useState("");
  const [running, setRunning] = useState(false);
  const [sourcesVersion, setSourcesVersion] = useState(0);
  const [historyVersion, setHistoryVersion] = useState(0);
  const [completions, setCompletions] = useState<Completion[]>([]);
  const editorRef = useRef<EditorHandle>(null);

  // Resizable layout: sidebar width + query-pane height (persisted).
  const [sidebarW, setSidebarW] = useState(() => loadPaneSize("sidebar", 240));
  const [queryH, setQueryH] = useState(() => loadPaneSize("query", 160));
  const workRef = useRef<HTMLElement>(null);

  // Persist the editor content and (re)load autocompletion when sources change.
  useEffect(() => saveLastQuery(query), [query]);
  useEffect(() => {
    buildCompletions().then(setCompletions);
  }, [sourcesVersion]);
  useEffect(() => savePaneSize("sidebar", sidebarW), [sidebarW]);
  useEffect(() => savePaneSize("query", queryH), [queryH]);

  // Drag a gutter: `axis` picks the pointer coordinate; `apply` maps the live
  // value to the clamped pane size. A body cursor/select lock keeps the drag
  // smooth over the editor and table.
  const startDrag = useCallback(
    (axis: "x" | "y", apply: (delta: number) => void, cursor: string) =>
      (e: React.PointerEvent) => {
        e.preventDefault();
        const origin = axis === "x" ? e.clientX : e.clientY;
        const onMove = (ev: PointerEvent) => {
          apply((axis === "x" ? ev.clientX : ev.clientY) - origin);
        };
        const onUp = () => {
          window.removeEventListener("pointermove", onMove);
          window.removeEventListener("pointerup", onUp);
          document.body.style.cursor = "";
          document.body.style.userSelect = "";
        };
        window.addEventListener("pointermove", onMove);
        window.addEventListener("pointerup", onUp);
        document.body.style.cursor = cursor;
        document.body.style.userSelect = "none";
      },
    [],
  );

  // delta is measured from the size at drag start, so each apply is absolute.
  const dragSidebar = startDrag(
    "x",
    (d) => setSidebarW(clamp(sidebarW + d, 160, 520)),
    "col-resize",
  );
  const dragQuery = startDrag(
    "y",
    (d) => {
      const maxH = (workRef.current?.clientHeight ?? 800) - 140;
      setQueryH(clamp(queryH + d, 90, Math.max(120, maxH)));
    },
    "row-resize",
  );

  const run = useCallback(
    async (explain: boolean, override?: string) => {
      const q = (override ?? query).trim();
      if (!q) return;
      setRunning(true);
      setStatus("running…");
      try {
        const data = await runQuery(q, explain);
        setResult(data);
        if (data.error) {
          setStatus(data.elapsed_ms != null ? `${data.elapsed_ms} ms` : "");
        } else if (data.notice != null) {
          // A session statement (e.g. CREATE/DROP MATERIALIZED VIEW) — refresh
          // the source list/autocompletion since the available views changed.
          setStatus(`${data.elapsed_ms} ms`);
          setSourcesVersion((v) => v + 1);
        } else if (data.explain != null) {
          setStatus(`${data.elapsed_ms} ms`);
        } else {
          setStatus(
            `${data.count} row${data.count === 1 ? "" : "s"} · ${data.elapsed_ms} ms`,
          );
          pushHistory(q);
          setHistoryVersion((v) => v + 1);
        }
      } catch (e) {
        setResult({
          columns: [],
          rows: [],
          count: 0,
          elapsed_ms: 0,
          error: String(e),
        });
        setStatus("");
      } finally {
        setRunning(false);
      }
    },
    [query],
  );

  const runNow = useCallback(
    (q: string) => {
      setQuery(q);
      run(false, q);
    },
    [run],
  );

  return (
    <div className="app">
      <header>
        <span className="brand">turntable</span>
        <span className="tag">query anything with SQL</span>
      </header>
      <main style={{ gridTemplateColumns: `${sidebarW}px 6px 1fr` }}>
        <Sidebar
          onInsert={(t) => editorRef.current?.insert(t)}
          onSourceAdded={() => setSourcesVersion((v) => v + 1)}
          onLoadQuery={setQuery}
          onRunQuery={runNow}
          currentQuery={query}
          historyVersion={historyVersion}
          sourcesVersion={sourcesVersion}
        />
        <Gutter dir="col" onPointerDown={dragSidebar} />
        <section
          className="work"
          ref={workRef}
          style={{ gridTemplateRows: `${queryH}px 6px 1fr` }}
        >
          <div className="query-pane">
            <Editor
              ref={editorRef}
              value={query}
              onChange={setQuery}
              onRun={() => run(false)}
              onExplain={() => run(true)}
              running={running}
              status={status}
              completions={completions}
            />
          </div>
          <Gutter dir="row" onPointerDown={dragQuery} />
          <Results result={result} />
        </section>
      </main>
    </div>
  );
}
