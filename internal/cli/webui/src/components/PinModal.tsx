import { useEffect, useState } from "react";
import {
  getDashboard,
  listDashboards,
  saveDashboard,
  type DashboardPanel,
  type DashboardSummary,
} from "../api";
import type { ViewConfig } from "../view";
import { Modal } from "./Modal";

const NEW = "__new__";

// jsSlugify mirrors the server's slugify (dashboard.go) so the modal can warn
// before a "new" dashboard would silently overwrite a same-named one.
function jsSlugify(name: string): string {
  const s = name
    .trim()
    .toLowerCase()
    .replace(/[^a-z0-9]+/g, "-")
    .replace(/^-+|-+$/g, "")
    .slice(0, 100)
    .replace(/-+$/, "");
  return s || "dashboard";
}

// PinModal appends the current result — its query plus the frozen view config —
// as a panel of an existing dashboard, or creates a new dashboard around it.
export function PinModal({
  open,
  onClose,
  kind,
  view,
  query,
  defaultTitle,
  onPinned,
}: {
  open: boolean;
  onClose: () => void;
  kind: "table" | "chart" | "pivot";
  view?: ViewConfig;
  query: string;
  defaultTitle: string;
  onPinned: () => void;
}) {
  const [dashes, setDashes] = useState<DashboardSummary[]>([]);
  const [target, setTarget] = useState<string>(NEW);
  const [newName, setNewName] = useState("");
  const [title, setTitle] = useState(defaultTitle);
  const [half, setHalf] = useState(false);
  const [status, setStatus] = useState("");
  const [busy, setBusy] = useState(false);

  useEffect(() => {
    if (!open) return;
    setStatus("");
    setTitle(defaultTitle);
    listDashboards()
      .then((d) => {
        setDashes(d);
        setTarget(d.length ? d[0].slug : NEW);
      })
      .catch((e) => setStatus(String(e)));
  }, [open, defaultTitle]);

  const pin = async () => {
    setBusy(true);
    setStatus("");
    try {
      const panel: DashboardPanel = {
        kind,
        title: title.trim() || undefined,
        query,
        width: half ? "half" : undefined,
      };
      if (kind === "chart" && view?.chart) panel.view = { chart: view.chart };
      if (kind === "pivot" && view?.pivot) panel.view = { pivot: view.pivot };

      let resp;
      if (target === NEW) {
        const name = newName.trim();
        if (!name) {
          setStatus("dashboard name required");
          setBusy(false);
          return;
        }
        if (dashes.some((d) => d.slug === jsSlugify(name))) {
          setStatus(`"${name}" already exists — pick it in the list to add to it`);
          setBusy(false);
          return;
        }
        resp = await saveDashboard({ name, panels: [panel] });
      } else {
        const d = await getDashboard(target);
        resp = await saveDashboard({
          slug: d.slug,
          name: d.name,
          description: d.description,
          variables: d.variables,
          panels: [...(d.panels ?? []), panel],
        });
      }
      if (resp.error) {
        setStatus(resp.error);
      } else {
        onPinned();
        onClose();
      }
    } catch (e) {
      setStatus(String(e));
    }
    setBusy(false);
  };

  return (
    <Modal open={open} title="Pin to dashboard" onClose={onClose}>
      <div className="pin-form">
        <label className="chart-field">
          dashboard
          <select value={target} onChange={(e) => setTarget(e.target.value)}>
            {dashes.map((d) => (
              <option key={d.slug} value={d.slug}>
                {d.name}
              </option>
            ))}
            <option value={NEW}>new dashboard…</option>
          </select>
        </label>
        {target === NEW && (
          <label className="chart-field">
            name
            <input
              className="dash-var"
              placeholder="Station Overview"
              value={newName}
              onChange={(e) => setNewName(e.target.value)}
              autoFocus
            />
          </label>
        )}
        <label className="chart-field">
          panel title
          <input
            className="dash-var"
            value={title}
            onChange={(e) => setTitle(e.target.value)}
          />
        </label>
        <label className="chart-field pivot-color" title="two half panels share a row">
          <input type="checkbox" checked={half} onChange={(e) => setHalf(e.target.checked)} />
          half width
        </label>
        <div className="hint" style={{ padding: 0 }}>
          pins the current query as a <b>{kind}</b> panel
          {kind !== "table" ? " with its current settings" : ""}.
        </div>
        {status && <div className="banner err">{status}</div>}
        <div className="modal-actions">
          <button onClick={pin} disabled={busy}>
            {busy ? "pinning…" : "Pin"}
          </button>
        </div>
      </div>
    </Modal>
  );
}
