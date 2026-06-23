import { useMemo, useState } from "react";
import { addSource, uploadFile } from "../api";
import {
  CONNECTOR_SPECS,
  specFor,
  type ConnectorSpec,
  type FieldSpec,
} from "../connectorSpecs";
import { Modal } from "./Modal";

type MsgKind = "" | "err" | "ok";

// seedValues pre-fills select fields with their first option so they render a
// valid value (and satisfy required checks) before the user touches them.
function seedValues(spec: ConnectorSpec): Record<string, string> {
  const v: Record<string, string> = {};
  for (const f of spec.fields) {
    if (f.type === "select" && f.options?.length) v[f.key] = f.options[0];
  }
  return v;
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

  const spec = useMemo(() => specFor(connector)!, [connector]);

  const reset = () => {
    setValues(seedValues(spec));
    setFile(null);
    setMsg({ kind: "", text: "" });
  };

  const pickConnector = (c: string) => {
    setConnector(c);
    setValues(seedValues(specFor(c)!));
    setFile(null);
    setMsg({ kind: "", text: "" });
  };

  const setField = (key: string, val: string) =>
    setValues((v) => ({ ...v, [key]: val }));

  const onFile = (f: File | null) => {
    setFile(f);
    // Default the source name to the file's stem on first pick.
    if (f && !name) setName(f.name.replace(/\.[^.]+$/, ""));
  };

  const submit = async () => {
    if (!name.trim()) {
      setMsg({ kind: "err", text: "name is required" });
      return;
    }
    // Required-field check (file connectors require a file instead of fields).
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
        setMsg({ kind: "", text: `uploading ${file.name}…` });
        const up = await uploadFile(file);
        if (up.error || !up.path) {
          setMsg({ kind: "err", text: up.error || "upload failed" });
          setBusy(false);
          return;
        }
        fields.path = up.path;
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
      // Close shortly after showing success.
      setName("");
      reset();
      setBusy(false);
      onClose();
    } catch (e) {
      setMsg({ kind: "err", text: String(e) });
      setBusy(false);
    }
  };

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

        {spec.fields.map((f) => (
          <Field key={f.key} field={f} value={values[f.key] ?? ""} onChange={setField} />
        ))}

        {spec.note && <div className="hint">{spec.note}</div>}

        <div className="modal-actions">
          <button className="ghost" onClick={onClose} disabled={busy}>
            Cancel
          </button>
          <button onClick={submit} disabled={busy}>
            Add source
          </button>
        </div>
        <div className={`msg ${msg.kind}`}>{msg.text}</div>
      </div>
    </Modal>
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
