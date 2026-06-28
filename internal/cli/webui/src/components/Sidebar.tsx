import { useEffect, useState } from "react";
import { getSchema, listSources, type Column, type Source } from "../api";
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

interface SidebarProps {
  onInsert: (text: string) => void;
  onSourceAdded: () => void;
  onLoadQuery: (q: string) => void;
  onRunQuery: (q: string) => void;
  currentQuery: string;
  historyVersion: number;
  sourcesVersion: number;
}

export function Sidebar({
  onInsert,
  onSourceAdded,
  onLoadQuery,
  onRunQuery,
  currentQuery,
  historyVersion,
  sourcesVersion,
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
        />
      ))}

      <HistoryPanel version={historyVersion} onLoad={onLoadQuery} />
      <SavedPanel currentQuery={currentQuery} onLoad={onLoadQuery} />
    </aside>
  );
}

function SourceItem({
  source,
  onInsert,
  onPreview,
}: {
  source: Source;
  onInsert: (text: string) => void;
  onPreview: () => void;
}) {
  const [open, setOpen] = useState(false);
  const [cols, setCols] = useState<Column[] | null>(null);
  const [colError, setColError] = useState<string>("");

  const toggle = () => {
    const next = !open;
    setOpen(next);
    if (next && cols === null && !colError) {
      getSchema(source.name)
        .then((sc) => {
          if (sc.error) setColError(sc.error);
          else setCols(sc.columns ?? []);
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
        <span className="conn">{source.connector}</span>
      </div>
      {open && (
        <ul className="cols">
          {colError && <li style={{ color: "var(--err)" }}>{colError}</li>}
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
