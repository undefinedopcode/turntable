import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import type { Completion } from "@codemirror/autocomplete";
import { runQuery, type QueryResult } from "./api";
import { Sidebar } from "./components/Sidebar";
import { Editor, type EditorHandle } from "./components/Editor";
import { Results } from "./components/Results";
import { TabBar } from "./components/TabBar";
import { DashboardView } from "./components/DashboardView";
import {
  loadLastQuery,
  loadPaneSize,
  loadTabs,
  pushHistory,
  savePaneSize,
  saveTabs,
  type TabState,
} from "./storage";
import { buildCompletions } from "./completions";
import { Gutter } from "./components/Gutter";
import type { ViewConfig } from "./view";

const clamp = (v: number, lo: number, hi: number) =>
  Math.max(lo, Math.min(hi, v));

// Tab is one query workspace: its editable text plus the transient result/status
// of its last run. Only id/name/query persist (see storage.saveTabs).
interface Tab extends TabState {
  result: QueryResult | null;
  status: string;
  running: boolean;
}

const newId = () => Math.random().toString(36).slice(2, 9);

// nextName picks the lowest unused "Query N" so closing/reopening stays tidy.
function nextName(tabs: { name: string }[]): string {
  const used = new Set(tabs.map((t) => t.name));
  for (let n = 1; ; n++) {
    const name = `Query ${n}`;
    if (!used.has(name)) return name;
  }
}

function freshTab(name: string, query = ""): Tab {
  return { id: newId(), name, query, result: null, status: "", running: false };
}

// initTabs restores persisted tabs, migrating the old single last-query if there
// are none. Always returns at least one tab.
function initTabs(): Tab[] {
  const saved = loadTabs();
  if (saved.length > 0) {
    return saved.map((t) => ({ ...t, result: null, status: "", running: false }));
  }
  return [freshTab("Query 1", loadLastQuery())];
}

export function App() {
  const [tabs, setTabs] = useState<Tab[]>(initTabs);
  const [activeId, setActiveId] = useState<string>(() => tabs[0].id);
  const [renamingId, setRenamingId] = useState<string | null>(null);
  const [sourcesVersion, setSourcesVersion] = useState(0);
  const [historyVersion, setHistoryVersion] = useState(0);
  // activeDash: an open dashboard replaces the query workspace until closed.
  const [activeDash, setActiveDash] = useState<string | null>(null);
  const [dashVersion, setDashVersion] = useState(0);
  const [completions, setCompletions] = useState<Completion[]>([]);
  const editorRef = useRef<EditorHandle>(null);

  const active = useMemo(
    () => tabs.find((t) => t.id === activeId) ?? tabs[0],
    [tabs, activeId],
  );

  // Resizable layout: sidebar width + query-pane height (persisted).
  const [sidebarW, setSidebarW] = useState(() => loadPaneSize("sidebar", 240));
  const [queryH, setQueryH] = useState(() => loadPaneSize("query", 160));
  const workRef = useRef<HTMLElement>(null);

  // Persist tab text + view config (not results) and (re)load autocompletion
  // when sources change.
  useEffect(() => {
    saveTabs(tabs.map(({ id, name, query, view }) => ({ id, name, query, view })));
  }, [tabs]);
  useEffect(() => {
    buildCompletions().then(setCompletions);
  }, [sourcesVersion]);
  useEffect(() => savePaneSize("sidebar", sidebarW), [sidebarW]);
  useEffect(() => savePaneSize("query", queryH), [queryH]);

  const patchTab = useCallback(
    (id: string, patch: Partial<Tab>) =>
      setTabs((ts) => ts.map((t) => (t.id === id ? { ...t, ...patch } : t))),
    [],
  );
  const setActiveQuery = useCallback(
    (q: string) => patchTab(activeId, { query: q }),
    [activeId, patchTab],
  );
  // patchView merges a partial view config (mode / chart / pivot settings) into
  // a tab, so the chart and the pivot can each update their slice independently.
  const patchView = useCallback(
    (id: string, patch: Partial<ViewConfig>) =>
      setTabs((ts) =>
        ts.map((t) => (t.id === id ? { ...t, view: { ...t.view, ...patch } } : t)),
      ),
    [],
  );

  const addTab = useCallback(() => {
    setTabs((ts) => {
      const t = freshTab(nextName(ts));
      setActiveId(t.id);
      return [...ts, t];
    });
  }, []);

  const closeTab = useCallback(
    (id: string) =>
      setTabs((ts) => {
        const i = ts.findIndex((t) => t.id === id);
        const next = ts.filter((t) => t.id !== id);
        if (next.length === 0) {
          const t = freshTab("Query 1");
          setActiveId(t.id);
          return [t];
        }
        setActiveId((cur) => (cur === id ? (next[i] ?? next[i - 1]).id : cur));
        return next;
      }),
    [],
  );

  const renameTab = useCallback(
    (id: string, name: string) => {
      const trimmed = name.trim();
      if (trimmed) patchTab(id, { name: trimmed });
      setRenamingId(null);
    },
    [patchTab],
  );

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
      const id = activeId;
      const q = (override ?? active.query).trim();
      if (!q) return;
      patchTab(id, { running: true, status: "running…" });
      try {
        const data = await runQuery(q, explain);
        let status: string;
        if (data.error) {
          status = data.elapsed_ms != null ? `${data.elapsed_ms} ms` : "";
        } else if (data.notice != null) {
          // A session statement (e.g. CREATE/DROP VIEW) — refresh the source
          // list/autocompletion since the available sources changed.
          status = `${data.elapsed_ms} ms`;
          setSourcesVersion((v) => v + 1);
        } else if (data.explain != null) {
          status = `${data.elapsed_ms} ms`;
        } else {
          status = `${data.count} row${data.count === 1 ? "" : "s"} · ${data.elapsed_ms} ms`;
          pushHistory(q);
          setHistoryVersion((v) => v + 1);
        }
        patchTab(id, { result: data, status });
      } catch (e) {
        patchTab(id, {
          result: { columns: [], rows: [], count: 0, elapsed_ms: 0, error: String(e) },
          status: "",
        });
      } finally {
        patchTab(id, { running: false });
      }
    },
    [activeId, active.query, patchTab],
  );

  const runNow = useCallback(
    (q: string) => {
      patchTab(activeId, { query: q });
      run(false, q);
    },
    [activeId, patchTab, run],
  );

  // refreshMatView re-runs a materialized view's query. Unlike a preview it does
  // not touch the editor — the notice just lands in the results pane — and it
  // reloads the source list (row count/schema may have changed).
  const refreshMatView = useCallback(
    async (name: string) => {
      const id = activeId;
      patchTab(id, { running: true, status: "refreshing…" });
      try {
        const data = await runQuery(`REFRESH MATERIALIZED VIEW ${name}`, false);
        patchTab(id, { result: data, status: data.error ? "" : `${data.elapsed_ms} ms` });
        if (!data.error) setSourcesVersion((v) => v + 1);
      } catch (e) {
        patchTab(id, {
          result: { columns: [], rows: [], count: 0, elapsed_ms: 0, error: String(e) },
          status: "",
        });
      } finally {
        patchTab(id, { running: false });
      }
    },
    [activeId, patchTab],
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
          onLoadQuery={(q) => {
            setActiveDash(null);
            setActiveQuery(q);
          }}
          onRunQuery={(q) => {
            setActiveDash(null);
            runNow(q);
          }}
          onRefresh={refreshMatView}
          currentQuery={active.query}
          historyVersion={historyVersion}
          sourcesVersion={sourcesVersion}
          dashVersion={dashVersion}
          onOpenDashboard={setActiveDash}
          onDashboardDeleted={(slug) => {
            setDashVersion((v) => v + 1);
            setActiveDash((cur) => (cur === slug ? null : cur));
          }}
        />
        <Gutter dir="col" onPointerDown={dragSidebar} />
        {activeDash != null ? (
          <section className="work dash-work">
            <DashboardView slug={activeDash} onClose={() => setActiveDash(null)} />
          </section>
        ) : (
          <section
            className="work"
            ref={workRef}
            style={{ gridTemplateRows: `auto ${queryH}px 6px 1fr` }}
          >
            <TabBar
              tabs={tabs}
              activeId={activeId}
              renamingId={renamingId}
              onSelect={setActiveId}
              onAdd={addTab}
              onClose={closeTab}
              onStartRename={setRenamingId}
              onRename={renameTab}
            />
            <div className="query-pane">
              <Editor
                key={activeId}
                ref={editorRef}
                value={active.query}
                onChange={setActiveQuery}
                onRun={() => run(false)}
                onExplain={() => run(true)}
                running={active.running}
                status={active.status}
                completions={completions}
              />
            </div>
            <Gutter dir="row" onPointerDown={dragQuery} />
            <Results
              key={activeId}
              result={active.result}
              view={active.view}
              onView={(p) => patchView(active.id, p)}
              query={active.query}
              tabName={active.name}
              onPinned={() => setDashVersion((v) => v + 1)}
            />
          </section>
        )}
      </main>
    </div>
  );
}
