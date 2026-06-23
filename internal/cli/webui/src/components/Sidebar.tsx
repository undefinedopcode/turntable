import { useEffect, useState } from "react";
import { getSchema, listSources, type Column, type Source } from "../api";
import { AddSourceModal } from "./AddSourceModal";

interface SidebarProps {
  onInsert: (text: string) => void;
  onSourceAdded: () => void;
}

export function Sidebar({ onInsert, onSourceAdded }: SidebarProps) {
  const [sources, setSources] = useState<Source[] | null>(null);
  const [error, setError] = useState<string>("");
  const [modalOpen, setModalOpen] = useState(false);

  const reload = () => {
    listSources()
      .then(setSources)
      .catch((e) => setError(String(e)));
  };

  useEffect(reload, []);

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
      {error && <div className="hint">{error}</div>}
      {sources && sources.length === 0 && (
        <div className="hint" style={{ padding: 6 }}>
          no sources configured — query qualified refs like{" "}
          <code>csv:./x.csv</code>, or add one above.
        </div>
      )}
      {sources?.map((s) => (
        <SourceItem key={s.name} source={s} onInsert={onInsert} />
      ))}
    </aside>
  );
}

function SourceItem({
  source,
  onInsert,
}: {
  source: Source;
  onInsert: (text: string) => void;
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
