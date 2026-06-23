// Typed client for the turntable JSON API exposed by `--serve` (see serve.go).
// Paths are relative so the bundle works whether served from the Go binary or
// the Vite dev server (which proxies /api to the Go server).

export interface Column {
  name: string;
  type: string;
  nullable: boolean;
}

export type Cell = string | number | boolean | null | object;

export interface QueryResult {
  columns: Column[];
  rows: Cell[][];
  count: number;
  elapsed_ms: number;
  truncated?: boolean;
  explain?: string;
  error?: string;
}

export interface Source {
  name: string;
  connector: string;
}

async function postJSON<T>(path: string, body: unknown): Promise<T> {
  const res = await fetch(path, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(body),
  });
  if (!res.ok) {
    const text = await res.text();
    throw new Error(text || `${res.status} ${res.statusText}`);
  }
  return res.json() as Promise<T>;
}

export function runQuery(query: string, explain = false): Promise<QueryResult> {
  return postJSON<QueryResult>("/api/query", { query, explain });
}

export async function listSources(): Promise<Source[]> {
  const res = await fetch("/api/sources");
  if (!res.ok) throw new Error(`${res.status} ${res.statusText}`);
  return (await res.json()) as Source[];
}

export async function getSchema(
  source: string,
): Promise<{ columns?: Column[]; error?: string }> {
  const res = await fetch("/api/schema?source=" + encodeURIComponent(source));
  if (!res.ok) throw new Error(`${res.status} ${res.statusText}`);
  return res.json();
}

export function addSource(
  name: string,
  connector: string,
  fields: Record<string, string>,
): Promise<{ registered?: string[]; error?: string }> {
  return postJSON("/api/sources", { name, connector, fields });
}
