import { useMemo, useState } from "react";
import {
  addSource,
  loginfer,
  uploadFile,
  type Cell,
  type LoginferResult,
} from "../api";
import {
  CONNECTOR_SPECS,
  specFor,
  type ConnectorSpec,
  type FieldSpec,
} from "../connectorSpecs";
import { Modal } from "./Modal";

type MsgKind = "" | "err" | "ok";

function seedValues(spec: ConnectorSpec): Record<string, string> {
  const v: Record<string, string> = {};
  for (const f of spec.fields) {
    if (f.type === "select" && f.options?.length) v[f.key] = f.options[0];
  }
  return v;
}

// patternWith rebuilds a regex from its original group names + the user's edited
// names — robust against repeated renames (always derived from the base).
function patternWith(base: string, orig: string[], names: string[]): string {
  let p = base;
  orig.forEach((o, i) => {
    p = p.replace(`(?P<${o}>`, `(?P<${names[i]}>`);
  });
  return p;
}

function sanitizeName(s: string): string {
  return s.replace(/[^a-zA-Z0-9_]/g, "").toLowerCase() || "field";
}

export function AddSourceModal({
  open,
  onClose,
  onAdded,
}: {
  open: boolean;
  onClose: () => void;
  onAdded: () => void;
}) {
  const [connector, setConnector] = useState(CONNECTOR_SPECS[0].name);
  const [name, setName] = useState("");
  const [values, setValues] = useState<Record<string, string>>(() =>
    seedValues(CONNECTOR_SPECS[0]),
  );
  const [file, setFile] = useState<File | null>(null);
  const [busy, setBusy] = useState(false);
  const [msg, setMsg] = useState<{ kind: MsgKind; text: string }>({ kind: "", text: "" });

  // Inference state (log connector only).
  const [uploadedPath, setUploadedPath] = useState<string | null>(null);
  const [analyzing, setAnalyzing] = useState(false);
  const [analysis, setAnalysis] = useState<LoginferResult | null>(null);
  const [selected, setSelected] = useState<number | null>(null);
  const [tplNames, setTplNames] = useState<string[]>([]);

  const spec = useMemo(() => specFor(connector)!, [connector]);

  const resetInference = () => {
    setUploadedPath(null);
    setAnalysis(null);
    setSelected(null);
    setTplNames([]);
  };

  const reset = () => {
    setValues(seedValues(spec));
    setFile(null);
    setMsg({ kind: "", text: "" });
    resetInference();
  };

  const pickConnector = (c: string) => {
    setConnector(c);
    setValues(seedValues(specFor(c)!));
    setFile(null);
    setMsg({ kind: "", text: "" });
    resetInference();
  };

  const setField = (key: string, val: string) =>
    setValues((v) => ({ ...v, [key]: val }));

  const selectTemplate = (i: number, res: LoginferResult) => {
    const tpl = res.templates![i];
    setSelected(i);
    const names = tpl.columns.map((c) => c.name);
    setTplNames(names);
    setField("pattern", tpl.pattern);
  };

  const renameColumn = (i: number, raw: string) => {
    if (selected === null || !analysis?.templates) return;
    const next = [...tplNames];
    next[i] = sanitizeName(raw);
    setTplNames(next);
    const tpl = analysis.templates[selected];
    const orig = tpl.columns.map((c) => c.name);
    setField("pattern", patternWith(tpl.pattern, orig, next));
  };

  const onFile = async (f: File | null) => {
    setFile(f);
    if (f && !name) setName(f.name.replace(/\.[^.]+$/, ""));
    resetInference();
    if (!f || connector !== "log") return;
    // Upload immediately and analyze so we can offer detection/inference.
    setAnalyzing(true);
    setMsg({ kind: "", text: "" });
    try {
      const up = await uploadFile(f);
      if (up.error || !up.path) {
        setMsg({ kind: "err", text: up.error || "upload failed" });
        setAnalyzing(false);
        return;
      }
      setUploadedPath(up.path);
      const res = await loginfer(up.path);
      setAnalysis(res);
      if (res.templates?.length) selectTemplate(0, res);
    } catch (e) {
      setMsg({ kind: "err", text: String(e) });
    }
    setAnalyzing(false);
  };

  const submit = async () => {
    if (!name.trim()) {
      setMsg({ kind: "err", text: "name is required" });
      return;
    }
    for (const f of spec.fields) {
      if (f.required && !values[f.key]?.trim()) {
        setMsg({ kind: "err", text: `${f.label} is required` });
        return;
      }
    }
    if (spec.file && !file) {
      setMsg({ kind: "err", text: "choose a file to upload" });
      return;
    }

    setBusy(true);
    try {
      const fields: Record<string, string> = {};
      for (const f of spec.fields) {
        const val = values[f.key]?.trim();
        if (val) fields[f.key] = val;
      }

      if (spec.file && file) {
        let path = uploadedPath;
        if (!path) {
          setMsg({ kind: "", text: `uploading ${file.name}…` });
          const up = await uploadFile(file);
          if (up.error || !up.path) {
            setMsg({ kind: "err", text: up.error || "upload failed" });
            setBusy(false);
            return;
          }
          path = up.path;
        }
        fields.path = path;
      }

      setMsg({ kind: "", text: "registering…" });
      const data = await addSource(name.trim(), connector, fields);
      if (data.error) {
        setMsg({ kind: "err", text: data.error });
        setBusy(false);
        return;
      }
      setMsg({ kind: "ok", text: "registered: " + (data.registered ?? []).join(", ") });
      onAdded();
      setName("");
      reset();
      setBusy(false);
      onClose();
    } catch (e) {
      setMsg({ kind: "err", text: String(e) });
      setBusy(false);
    }
  };

  const isLog = connector === "log";

  return (
    <Modal open={open} title="Add source" onClose={onClose}>
      <div className="form">
        <div>
          <label>Connector</label>
          <select value={connector} onChange={(e) => pickConnector(e.target.value)}>
            {CONNECTOR_SPECS.map((c) => (
              <option key={c.name} value={c.name}>
                {c.label}
              </option>
            ))}
          </select>
        </div>

        <div>
          <label>Source name</label>
          <input
            placeholder="sales"
            spellCheck={false}
            value={name}
            onChange={(e) => setName(e.target.value)}
          />
        </div>

        {spec.file && <FileField spec={spec} file={file} onFile={onFile} />}

        {isLog && (
          <InferPanel
            analyzing={analyzing}
            analysis={analysis}
            selected={selected}
            tplNames={tplNames}
            onSelect={(i) => analysis && selectTemplate(i, analysis)}
            onRename={renameColumn}
          />
        )}

        {/* For the log connector the format/pattern fields are auto-filled by the
            panel; expose them under Advanced for manual override. */}
        {isLog ? (
          <details className="add">
            <summary>Advanced (format / pattern)</summary>
            <div style={{ padding: "8px 0" }}>
              {spec.fields.map((f) => (
                <Field
                  key={f.key}
                  field={f}
                  value={values[f.key] ?? ""}
                  onChange={setField}
                />
              ))}
            </div>
          </details>
        ) : (
          spec.fields.map((f) => (
            <Field key={f.key} field={f} value={values[f.key] ?? ""} onChange={setField} />
          ))
        )}

        {spec.note && <div className="hint">{spec.note}</div>}

        <div className="modal-actions">
          <button className="ghost" onClick={onClose} disabled={busy}>
            Cancel
          </button>
          <button onClick={submit} disabled={busy || analyzing}>
            Add source
          </button>
        </div>
        <div className={`msg ${msg.kind}`}>{msg.text}</div>
      </div>
    </Modal>
  );
}

function InferPanel({
  analyzing,
  analysis,
  selected,
  tplNames,
  onSelect,
  onRename,
}: {
  analyzing: boolean;
  analysis: LoginferResult | null;
  selected: number | null;
  tplNames: string[];
  onSelect: (i: number) => void;
  onRename: (i: number, name: string) => void;
}) {
  if (analyzing) return <div className="hint">⟳ analyzing log…</div>;
  if (!analysis) return null;
  if (analysis.error) return <div className="msg err">{analysis.error}</div>;

  if (analysis.detected) {
    const d = analysis.detected;
    return (
      <div className="infer">
        <div className="infer-banner ok">
          ✓ Detected: <b>{d.format}</b>
        </div>
        <PreviewTable columns={d.columns.map((c) => c.name)} rows={d.rows} />
      </div>
    );
  }

  if (analysis.templates && analysis.templates.length > 0) {
    return (
      <div className="infer">
        <div className="infer-banner warn">
          ⚠ no standard format — inferred {analysis.templates.length} pattern
          {analysis.templates.length === 1 ? "" : "s"} (pick one; rename columns)
        </div>
        {analysis.templates.map((t, i) => (
          <div
            key={i}
            className={`tpl ${selected === i ? "sel" : ""} ${t.common ? "common" : ""}`}
            onClick={() => onSelect(i)}
          >
            <div className="tpl-head">
              <input type="radio" checked={selected === i} readOnly />
              {t.common && <span className="tpl-badge">all rows</span>}
              <code className="tpl-label">{t.label}</code>
              <span className="tpl-count">×{t.count}</span>
            </div>
            {selected === i && (
              <div className="tpl-cols">
                {t.columns.map((c, j) => (
                  <span className="col-edit" key={j}>
                    <input
                      value={tplNames[j] ?? c.name}
                      onClick={(e) => e.stopPropagation()}
                      onChange={(e) => onRename(j, e.target.value)}
                    />
                    <span className="col-sample" title={c.type}>
                      {t.sample[j]}
                    </span>
                  </span>
                ))}
              </div>
            )}
          </div>
        ))}
      </div>
    );
  }

  return <div className="hint">no structure inferred — use raw or a custom pattern.</div>;
}

function PreviewTable({ columns, rows }: { columns: string[]; rows: Cell[][] }) {
  if (!rows.length) return null;
  return (
    <div className="infer-preview">
      <table>
        <thead>
          <tr>
            {columns.map((c) => (
              <th key={c}>{c}</th>
            ))}
          </tr>
        </thead>
        <tbody>
          {rows.map((row, i) => (
            <tr key={i}>
              {row.map((cell, j) => (
                <td key={j}>{cell === null ? "" : String(cell)}</td>
              ))}
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}

function FileField({
  spec,
  file,
  onFile,
}: {
  spec: ConnectorSpec;
  file: File | null;
  onFile: (f: File | null) => void;
}) {
  return (
    <div>
      <label>File (uploaded to the server's scratch dir)</label>
      <input
        type="file"
        accept={spec.fileExt}
        onChange={(e) => onFile(e.target.files?.[0] ?? null)}
      />
      {file && (
        <div className="hint">
          {file.name} · {(file.size / 1024).toFixed(1)} KB
        </div>
      )}
    </div>
  );
}

function Field({
  field,
  value,
  onChange,
}: {
  field: FieldSpec;
  value: string;
  onChange: (key: string, val: string) => void;
}) {
  return (
    <div>
      <label>
        {field.label}
        {field.required ? " *" : ""}
      </label>
      {field.type === "select" ? (
        <select value={value} onChange={(e) => onChange(field.key, e.target.value)}>
          {(field.options ?? []).map((o) => (
            <option key={o} value={o}>
              {o}
            </option>
          ))}
        </select>
      ) : (
        <input
          type={field.type === "password" ? "password" : "text"}
          placeholder={field.placeholder}
          spellCheck={false}
          value={value}
          onChange={(e) => onChange(field.key, e.target.value)}
        />
      )}
      {field.help && <div className="hint">{field.help}</div>}
    </div>
  );
}
