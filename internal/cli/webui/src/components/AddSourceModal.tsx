import { useEffect, useMemo, useState } from "react";
import {
  addSource,
  loginfer,
  uploadFile,
  type Cell,
  type LoginferResult,
} from "../api";
import {
  fetchConnectorSpecs,
  specFor,
  type ConnectorSpec,
  type FieldSpec,
} from "../connectorSpecs";
import { Modal } from "./Modal";

type MsgKind = "" | "err" | "ok";

// A sensitive field must be exactly an ${ENV_VAR} / ${ENV_VAR:-default} reference.
const ENV_REF = /^\$\{[A-Za-z_][A-Za-z0-9_]*(:-[^}]*)?\}$/;

// fieldSensitive reports whether f holds a credential that must be an env-var
// reference. The sql `dsn` is exempt for sqlite (a local file path).
function fieldSensitive(
  connector: string,
  f: FieldSpec,
  values: Record<string, string>,
): boolean {
  if (!f.sensitive) return false;
  if (connector === "sql" && f.key === "dsn" && values.driver === "sqlite") return false;
  return true;
}

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
  // Specs come from GET /api/connectors (fetched once, cached module-wide);
  // until they arrive the modal renders a loading hint.
  const [specs, setSpecs] = useState<ConnectorSpec[] | null>(null);
  const [connector, setConnector] = useState("");
  const [name, setName] = useState("");
  const [values, setValues] = useState<Record<string, string>>({});
  const [file, setFile] = useState<File | null>(null);
  // File connectors can either upload a copy or point at a path on the server's
  // own filesystem (read live on each query — good for logs/CSVs being updated).
  const [fileMode, setFileMode] = useState<"upload" | "path">("upload");
  const [serverPath, setServerPath] = useState("");
  const [save, setSave] = useState(false);
  const [busy, setBusy] = useState(false);
  const [msg, setMsg] = useState<{ kind: MsgKind; text: string }>({ kind: "", text: "" });

  // Inference state (log connector only).
  const [uploadedPath, setUploadedPath] = useState<string | null>(null);
  const [analyzing, setAnalyzing] = useState(false);
  const [analysis, setAnalysis] = useState<LoginferResult | null>(null);
  const [selected, setSelected] = useState<number | null>(null);
  const [tplNames, setTplNames] = useState<string[]>([]);

  useEffect(() => {
    if (!open || specs) return;
    fetchConnectorSpecs()
      .then((s) => {
        setSpecs(s);
        if (s.length && !connector) {
          setConnector(s[0].name);
          setValues(seedValues(s[0]));
        }
      })
      .catch((e) => setMsg({ kind: "err", text: `load connectors: ${e}` }));
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [open, specs]);

  const spec = useMemo(
    () => (specs ? specFor(specs, connector) : undefined),
    [specs, connector],
  );

  const resetInference = () => {
    setUploadedPath(null);
    setAnalysis(null);
    setSelected(null);
    setTplNames([]);
  };

  const reset = () => {
    setValues(spec ? seedValues(spec) : {});
    setFile(null);
    setServerPath("");
    setMsg({ kind: "", text: "" });
    resetInference();
  };

  const pickConnector = (c: string) => {
    setConnector(c);
    const s = specs ? specFor(specs, c) : undefined;
    setValues(s ? seedValues(s) : {});
    setFile(null);
    setServerPath("");
    setMsg({ kind: "", text: "" });
    resetInference();
  };

  // setPath updates the server-path field and seeds the source name from the
  // file's basename when the name is still empty.
  const setPath = (p: string) => {
    setServerPath(p);
    if (!name) {
      const base = p.split(/[\\/]/).pop()?.replace(/\.[^.]+$/, "") ?? "";
      if (base) setName(base);
    }
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
    if (!spec) return;
    if (!name.trim()) {
      setMsg({ kind: "err", text: "name is required" });
      return;
    }
    for (const f of spec.fields) {
      if (f.required && !values[f.key]?.trim()) {
        setMsg({ kind: "err", text: `${f.label} is required` });
        return;
      }
      const val = values[f.key]?.trim();
      if (val && fieldSensitive(connector, f, values) && !ENV_REF.test(val)) {
        setMsg({
          kind: "err",
          text: `${f.label} must reference an environment variable, e.g. \${MY_SECRET} — set it in your shell or a .env file.`,
        });
        return;
      }
    }
    if (spec.file) {
      if (fileMode === "upload" && !file) {
        setMsg({ kind: "err", text: "choose a file to upload" });
        return;
      }
      if (fileMode === "path" && !serverPath.trim()) {
        setMsg({ kind: "err", text: "enter a path the server can read" });
        return;
      }
    }

    setBusy(true);
    try {
      const fields: Record<string, string> = {};
      for (const f of spec.fields) {
        const val = values[f.key]?.trim();
        if (val) fields[f.key] = val;
      }

      if (spec.file) {
        if (fileMode === "path") {
          fields.path = serverPath.trim();
        } else if (file) {
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
      }

      setMsg({ kind: "", text: "registering…" });
      const data = await addSource(name.trim(), connector, fields, save);
      if (data.error) {
        setMsg({ kind: "err", text: data.error });
        setBusy(false);
        return;
      }
      // A save error is non-fatal: the source registered, it just wasn't written.
      if (data.saveError) {
        setMsg({
          kind: "err",
          text: `registered, but not saved to config: ${data.saveError}`,
        });
        setBusy(false);
        return;
      }
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

  if (!specs || !spec) {
    return (
      <Modal open={open} title="Add source" onClose={onClose}>
        <div className={msg.kind === "err" ? "msg err" : "hint"}>
          {msg.kind === "err" ? msg.text : "loading connectors…"}
        </div>
      </Modal>
    );
  }

  return (
    <Modal open={open} title="Add source" onClose={onClose}>
      <div className="form">
        <div>
          <label>Connector</label>
          <select value={connector} onChange={(e) => pickConnector(e.target.value)}>
            {specs.map((c) => (
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

        {spec.file && (
          <div>
            <div className="seg file-mode">
              <button
                type="button"
                className={fileMode === "upload" ? "on" : ""}
                onClick={() => setFileMode("upload")}
              >
                Upload a copy
              </button>
              <button
                type="button"
                className={fileMode === "path" ? "on" : ""}
                onClick={() => setFileMode("path")}
              >
                Server path
              </button>
            </div>
            {fileMode === "upload" ? (
              <FileField spec={spec} file={file} onFile={onFile} />
            ) : (
              <div>
                <label>Server file path</label>
                <input
                  placeholder={spec.fileExt ? `e.g. /var/log/app.log (${spec.fileExt})` : "/path/on/the/server"}
                  spellCheck={false}
                  value={serverPath}
                  onChange={(e) => setPath(e.target.value)}
                />
                <div className="hint">
                  a path the server can read — read live on every query (no copy), so
                  an updated file refreshes on the next run.
                </div>
              </div>
            )}
          </div>
        )}

        {isLog && fileMode === "upload" && (
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
                  sensitive={fieldSensitive(connector, f, values)}
                />
              ))}
            </div>
          </details>
        ) : (
          spec.fields.map((f) => (
            <Field
              key={f.key}
              field={f}
              value={values[f.key] ?? ""}
              onChange={setField}
              sensitive={fieldSensitive(connector, f, values)}
            />
          ))
        )}

        {spec.note && <div className="hint">{spec.note}</div>}

        <label className="save-toggle" title="append this source to turntable.yaml (secrets stay as ${ENV_VAR} references)">
          <input type="checkbox" checked={save} onChange={(e) => setSave(e.target.checked)} />
          Save to config file
        </label>

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
  sensitive,
}: {
  field: FieldSpec;
  value: string;
  onChange: (key: string, val: string) => void;
  sensitive?: boolean;
}) {
  const bad = sensitive && !!value.trim() && !ENV_REF.test(value.trim());
  return (
    <div>
      <label>
        {field.label}
        {field.required ? " *" : ""}
        {sensitive && <span className="env-badge" title="must be an ${ENV_VAR} reference">env ref</span>}
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
          className={bad ? "field-bad" : ""}
          placeholder={field.placeholder}
          spellCheck={false}
          value={value}
          onChange={(e) => onChange(field.key, e.target.value)}
        />
      )}
      {sensitive ? (
        <div className={`hint ${bad ? "hint-bad" : ""}`}>
          reference an env var like <code>${"{MY_SECRET}"}</code> — keeps the secret
          out of the config{field.help ? ` (${field.help})` : ""}
        </div>
      ) : (
        field.help && <div className="hint">{field.help}</div>
      )}
    </div>
  );
}
