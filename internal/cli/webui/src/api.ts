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
  notice?: string;
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

export interface FunctionList {
  scalar: string[];
  aggregate: string[];
  keywords: string[];
}

export async function getFunctions(): Promise<FunctionList> {
  const res = await fetch("/api/functions");
  if (!res.ok) throw new Error(`${res.status} ${res.statusText}`);
  return res.json();
}

export function addSource(
  name: string,
  connector: string,
  fields: Record<string, string>,
  save = false,
): Promise<{ registered?: string[]; error?: string; saved?: string; saveError?: string }> {
  return postJSON("/api/sources", { name, connector, fields, save });
}

export interface UploadResult {
  path?: string;
  filename?: string;
  size?: number;
  error?: string;
}

export interface InferColumn {
  name: string;
  type: string;
}

export interface InferTemplate {
  label: string;
  count: number;
  pattern: string;
  columns: InferColumn[];
  sample: string[];
  sample_line: string;
  common?: boolean; // the synthesized "matches all rows" catch-all
}

export interface LoginferResult {
  detected?: { format: string; columns: Column[]; rows: Cell[][] };
  templates?: InferTemplate[];
  error?: string;
}

// loginfer analyzes a log file path: either the recognized format with a parsed
// preview, or inferred templates each carrying a ready-to-use `pattern`.
export function loginfer(path: string): Promise<LoginferResult> {
  return postJSON<LoginferResult>("/api/loginfer", { path });
}

// uploadFile streams a file to the server's per-session scratch directory and
// returns its stored path, for use as a file connector's `path` field.
export async function uploadFile(file: File): Promise<UploadResult> {
  const form = new FormData();
  form.append("file", file);
  const res = await fetch("/api/upload", { method: "POST", body: form });
  if (!res.ok && res.status !== 200) {
    throw new Error((await res.text()) || `${res.status} ${res.statusText}`);
  }
  return res.json();
}
