interface TabBarProps {
  tabs: { id: string; name: string }[];
  activeId: string;
  renamingId: string | null;
  onSelect: (id: string) => void;
  onAdd: () => void;
  onClose: (id: string) => void;
  onStartRename: (id: string) => void;
  onRename: (id: string, name: string) => void;
}

// TabBar renders the open query tabs: click to switch, double-click to rename
// inline, × to close, + to open a new one.
export function TabBar({
  tabs,
  activeId,
  renamingId,
  onSelect,
  onAdd,
  onClose,
  onStartRename,
  onRename,
}: TabBarProps) {
  return (
    <div className="tabbar">
      {tabs.map((t) => (
        <div
          key={t.id}
          className={`tab ${t.id === activeId ? "active" : ""}`}
          onClick={() => onSelect(t.id)}
          onDoubleClick={() => onStartRename(t.id)}
          title="double-click to rename"
        >
          {renamingId === t.id ? (
            <input
              className="tab-rename"
              autoFocus
              defaultValue={t.name}
              onClick={(e) => e.stopPropagation()}
              onBlur={(e) => onRename(t.id, e.target.value)}
              onKeyDown={(e) => {
                if (e.key === "Enter") onRename(t.id, e.currentTarget.value);
                else if (e.key === "Escape") onRename(t.id, t.name);
              }}
            />
          ) : (
            <span className="tab-name">{t.name}</span>
          )}
          {tabs.length > 1 && (
            <button
              className="tab-close"
              title="close tab"
              onClick={(e) => {
                e.stopPropagation();
                onClose(t.id);
              }}
            >
              ×
            </button>
          )}
        </div>
      ))}
      <button className="tab-add" title="new query tab" onClick={onAdd}>
        +
      </button>
    </div>
  );
}
