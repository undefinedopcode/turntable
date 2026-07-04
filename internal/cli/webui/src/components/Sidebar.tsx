import { useEffect, useState } from "react";
import {
  deleteDashboard,
  getSchema,
  listDashboards,
  listSources,
  type Column,
  type DashboardSummary,
  type Source,
} from "../api";
import { AddSourceModal } from "./AddSourceModal";
import {
  clearHistory,
  deleteSaved,
  loadHistory,
  loadSaved,
  relTime,
  saveQuery,
  type HistoryEntry,
  type SavedQuery,
} from "../storage";

const fmtSize = (n?: number): string =>
  n == null
    ? ""
    : n < 1024
      ? `${n} B`
      : n < 1024 * 1024
        ? `${(n / 1024).toFixed(1)} KB`
        : `${(n / (1024 * 1024)).toFixed(1)} MB`;

interface SidebarProps {
  onInsert: (text: string) => void;
  onSourceAdded: () => void;
  onLoadQuery: (q: string) => void;
  onRunQuery: (q: string) => void;
  onRefresh: (name: string) => void;
  currentQuery: string;
  historyVersion: number;
  sourcesVersion: number;
  dashVersion: number;
  onOpenDashboard: (slug: string) => void;
  onDashboardDeleted: (slug: string) => void;
}

export function Sidebar({
  onInsert,
  onSourceAdded,
  onLoadQuery,
  onRunQuery,
  onRefresh,
  currentQuery,
  historyVersion,
  sourcesVersion,
  dashVersion,
  onOpenDashboard,
  onDashboardDeleted,
}: SidebarProps) {
  const [sources, setSources] = useState<Source[] | null>(null);
  const [error, setError] = useState<string>("");
  const [modalOpen, setModalOpen] = useState(false);
  const [search, setSearch] = useState("");

  const reload = () => {
    listSources()
      .then(setSources)
      .catch((e) => setError(String(e)));
  };
  // Reload on mount and whenever the source set changes (e.g. a materialized
  // view was created or dropped).
  useEffect(reload, [sourcesVersion]);

  const shown = (sources ?? []).filter((s) =>
    s.name.toLowerCase().includes(search.trim().toLowerCase()),
  );

  return (
    <aside>
      <div className="sources-head">
        <h2>Sources</h2>
        <button className="add-btn" onClick={() => setModalOpen(true)}>
          + Add
        </button>
      </div>
      <AddSourceModal
        open={modalOpen}
        onClose={() => setModalOpen(false)}
        onAdded={() => {
          reload();
          onSourceAdded();
        }}
      />
      {sources && sources.length > 3 && (
        <input
          className="side-search"
          placeholder="filter sources…"
          value={search}
          onChange={(e) => setSearch(e.target.value)}
        />
      )}
      {error && <div className="hint">{error}</div>}
      {sources && sources.length === 0 && (
        <div className="hint" style={{ padding: 6 }}>
          no sources configured — query qualified refs like{" "}
          <code>csv:./x.csv</code>, or add one above.
        </div>
      )}
      {shown.map((s) => (
        <SourceItem
          key={s.name}
          source={s}
          onInsert={onInsert}
          onPreview={() => onRunQuery(`SELECT * FROM ${s.name} LIMIT 100`)}
          onRefresh={() => onRefresh(s.name)}
        />
      ))}

      <DashboardsPanel
        version={dashVersion}
        onOpen={onOpenDashboard}
        onDeleted={onDashboardDeleted}
      />
      <HistoryPanel version={historyVersion} onLoad={onLoadQuery} />
      <SavedPanel currentQuery={currentQuery} onLoad={onLoadQuery} />
    </aside>
  );
}

function SourceItem({
  source,
  onInsert,
  onPreview,
  onRefresh,
}: {
  source: Source;
  onInsert: (text: string) => void;
  onPreview: () => void;
  onRefresh: () => void;
}) {
  // Materialized views are mem-backed; only they can be REFRESHed.
  const isMatView = source.connector === "mem";
  const [open, setOpen] = useState(false);
  const [cols, setCols] = useState<Column[] | null>(null);
  const [meta, setMeta] = useState<{ modified?: string; size?: number; path?: string }>({});
  const [colError, setColError] = useState<string>("");

  const toggle = () => {
    const next = !open;
    setOpen(next);
    // Re-fetch each time it opens so a file source's modified-time stays current
    // (file sources are read live on every query).
    if (next) {
      setColError("");
      getSchema(source.name)
        .then((sc) => {
          if (sc.error) setColError(sc.error);
          else {
            setCols(sc.columns ?? []);
            setMeta({ modified: sc.modified, size: sc.size, path: sc.path });
          }
        })
        .catch((e) => setColError(String(e)));
    }
  };

  return (
    <div className="src">
      <div className="row" onClick={toggle}>
        <span
          className="name"
          onClick={(e) => {
            e.stopPropagation();
            onInsert(source.name);
          }}
        >
          {source.name}
        </span>
        {isMatView && (
          <button
            className="preview-btn"
            title="REFRESH this materialized view"
            onClick={(e) => {
              e.stopPropagation();
              onRefresh();
            }}
          >
            ⟳
          </button>
        )}
        <button
          className="preview-btn"
          title="SELECT * … LIMIT 100"
          onClick={(e) => {
            e.stopPropagation();
            onPreview();
          }}
        >
          ▶
        </button>
        <span
          className="conn"
          title={
            source.persistent
              ? "materialized view — persisted to disk (survives restart)"
              : undefined
          }
        >
          {source.persistent ? `${source.connector} · persistent` : source.connector}
        </span>
      </div>
      {open && (
        <ul className="cols">
          {colError && <li style={{ color: "var(--err)" }}>{colError}</li>}
          {meta.modified && (
            <li className="file-meta" title={meta.path}>
              updated {relTime(Date.parse(meta.modified))} · {fmtSize(meta.size)}
            </li>
          )}
          {cols?.map((c) => (
            <li
              key={c.name}
              title="insert column"
              onClick={() => onInsert(c.name)}
            >
              {c.name}
              <span className="ty"> {c.type}</span>
            </li>
          ))}
        </ul>
      )}
    </div>
  );
}

// DashboardsPanel lists the server-side dashboards. Deleting is a two-click
// confirm (× → sure?) rather than a blocking dialog.
function DashboardsPanel({
  version,
  onOpen,
  onDeleted,
}: {
  version: number;
  onOpen: (slug: string) => void;
  onDeleted: (slug: string) => void;
}) {
  const [items, setItems] = useState<DashboardSummary[]>([]);
  const [confirmDel, setConfirmDel] = useState<string | null>(null);
  useEffect(() => {
    listDashboards()
      .then(setItems)
      .catch(() => {});
  }, [version]);
  if (items.length === 0) return null;

  return (
    <div className="panel">
      <div className="panel-head">
        <h2>Dashboards</h2>
      </div>
      {items.map((d) => (
        <div key={d.slug} className="hist saved" title={d.error ?? d.description ?? d.name}>
          <span className="hist-q" onClick={() => onOpen(d.slug)}>
            {d.name}
            {d.error ? " ⚠" : ""}
          </span>
          <button
            className="link del"
            title={confirmDel === d.slug ? "click again to delete" : "delete"}
            onClick={() => {
              if (confirmDel !== d.slug) {
                setConfirmDel(d.slug);
                return;
              }
              deleteDashboard(d.slug)
                .then(() => {
                  setItems((it) => it.filter((x) => x.slug !== d.slug));
                  onDeleted(d.slug);
                })
                .catch(() => {})
                .finally(() => setConfirmDel(null));
            }}
          >
            {confirmDel === d.slug ? "sure?" : "×"}
          </button>
        </div>
      ))}
    </div>
  );
}

function HistoryPanel({
  version,
  onLoad,
}: {
  version: number;
  onLoad: (q: string) => void;
}) {
  const [items, setItems] = useState<HistoryEntry[]>([]);
  useEffect(() => setItems(loadHistory()), [version]);
  if (items.length === 0) return null;

  return (
    <div className="panel">
      <div className="panel-head">
        <h2>History</h2>
        <button
          className="link"
          onClick={() => {
            clearHistory();
            setItems([]);
          }}
        >
          clear
        </button>
      </div>
      {items.slice(0, 15).map((e, i) => (
        <div
          key={i}
          className="hist"
          title={e.q}
          onClick={() => onLoad(e.q)}
        >
          <span className="hist-q">{e.q.replace(/\s+/g, " ")}</span>
          <span className="hist-t">{relTime(e.ts)}</span>
        </div>
      ))}
    </div>
  );
}

function SavedPanel({
  currentQuery,
  onLoad,
}: {
  currentQuery: string;
  onLoad: (q: string) => void;
}) {
  const [items, setItems] = useState<SavedQuery[]>([]);
  useEffect(() => setItems(loadSaved()), []);

  const save = () => {
    const q = currentQuery.trim();
    if (!q) return;
    const name = window.prompt("Save query as:");
    if (name) setItems(saveQuery(name, q));
  };

  return (
    <div className="panel">
      <div className="panel-head">
        <h2>Saved</h2>
        <button className="link" onClick={save} title="save the current query">
          + save
        </button>
      </div>
      {items.length === 0 && (
        <div className="hint" style={{ padding: "2px 6px" }}>
          save the current query for later.
        </div>
      )}
      {items.map((s) => (
        <div key={s.name} className="hist saved" title={s.q}>
          <span className="hist-q" onClick={() => onLoad(s.q)}>
            {s.name}
          </span>
          <button
            className="link del"
            onClick={() => setItems(deleteSaved(s.name))}
            title="delete"
          >
            ×
          </button>
        </div>
      ))}
    </div>
  );
}
