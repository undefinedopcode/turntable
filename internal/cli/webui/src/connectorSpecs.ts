// Per-connector form specs that drive the Add Source modal. The specs live
// server-side in internal/cli/connspec.go (the single source of truth, shared
// with the MCP list_connectors tool) and are fetched from GET /api/connectors;
// this module is just the types and a cached fetch.
//
// Field keys map directly onto the JSON sent to POST /api/sources `fields`
// (which the server routes via applySourceField, same as the REPL .use command).

export interface FieldSpec {
  key: string;
  label: string;
  type?: "text" | "password" | "select";
  options?: string[];
  required?: boolean;
  placeholder?: string;
  help?: string;
  // sensitive fields hold credentials: the UI requires an ${ENV_VAR} reference
  // (validated server-side too), so secrets never land in the config. For sql
  // `dsn` this is waived when the driver is sqlite (a local path) — see the
  // modal's fieldSensitive().
  sensitive?: boolean;
}

export interface ConnectorSpec {
  name: string; // connector prefix (csv, sql, ...)
  label: string; // human label for the picker
  file?: boolean; // file connector: offer upload, path is supplied by it
  fileExt?: string; // accept filter for the file input
  fields: FieldSpec[]; // extra option fields beyond the file/path
  note?: string;
}

let cache: ConnectorSpec[] | null = null;

export async function fetchConnectorSpecs(): Promise<ConnectorSpec[]> {
  if (cache) return cache;
  const res = await fetch("/api/connectors");
  if (!res.ok) throw new Error(`${res.status} ${res.statusText}`);
  cache = (await res.json()) as ConnectorSpec[];
  return cache;
}

export function specFor(
  specs: ConnectorSpec[],
  name: string,
): ConnectorSpec | undefined {
  return specs.find((c) => c.name === name);
}
