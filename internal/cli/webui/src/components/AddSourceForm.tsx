import { useState } from "react";
import { addSource } from "../api";

// Connector prefixes registered by the server (see NewApp in cli.go). Kept in
// sync here because the API has no "list connectors" endpoint yet.
const CONNECTORS = [
  "csv",
  "json",
  "yaml",
  "excel",
  "parquet",
  "sql",
  "http",
  "linear",
  "trello",
  "azuredevops",
  "dynamodb",
  "azuretables",
  "cloudwatchlogs",
  "cloudwatch",
];

type MsgKind = "" | "err" | "ok";

export function AddSourceForm({ onAdded }: { onAdded: () => void }) {
  const [name, setName] = useState("");
  const [connector, setConnector] = useState(CONNECTORS[0]);
  const [fieldsText, setFieldsText] = useState("");
  const [msg, setMsg] = useState<{ kind: MsgKind; text: string }>({
    kind: "",
    text: "",
  });

  const submit = async () => {
    const fields: Record<string, string> = {};
    for (const line of fieldsText.split("\n")) {
      const t = line.trim();
      if (!t) continue;
      const i = t.indexOf("=");
      if (i <= 0) {
        setMsg({ kind: "err", text: `bad field "${t}" (want key=value)` });
        return;
      }
      fields[t.slice(0, i).trim()] = t.slice(i + 1).trim();
    }
    if (!name.trim()) {
      setMsg({ kind: "err", text: "name is required" });
      return;
    }
    setMsg({ kind: "", text: "adding…" });
    try {
      const data = await addSource(name.trim(), connector, fields);
      if (data.error) {
        setMsg({ kind: "err", text: data.error });
        return;
      }
      setMsg({ kind: "ok", text: "registered: " + (data.registered ?? []).join(", ") });
      setName("");
      setFieldsText("");
      onAdded();
    } catch (e) {
      setMsg({ kind: "err", text: String(e) });
    }
  };

  return (
    <details className="add">
      <summary>+ Add source</summary>
      <div className="form">
        <div>
          <label>Name</label>
          <input
            placeholder="sales"
            spellCheck={false}
            value={name}
            onChange={(e) => setName(e.target.value)}
          />
        </div>
        <div>
          <label>Connector</label>
          <select value={connector} onChange={(e) => setConnector(e.target.value)}>
            {CONNECTORS.map((c) => (
              <option key={c} value={c}>
                {c}
              </option>
            ))}
          </select>
        </div>
        <div>
          <label>
            Fields (one <code>key=value</code> per line)
          </label>
          <textarea
            spellCheck={false}
            placeholder="path=/data/sales.csv"
            value={fieldsText}
            onChange={(e) => setFieldsText(e.target.value)}
          />
        </div>
        <button onClick={submit}>Add source</button>
        <div className={`msg ${msg.kind}`}>{msg.text}</div>
      </div>
    </details>
  );
}
