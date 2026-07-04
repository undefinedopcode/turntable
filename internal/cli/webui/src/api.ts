// Typed client for the turntable JSON API exposed by `--serve` (see serve.go).
// Paths are relative so the bundle works whether served from the Go binary or
// the Vite dev server (which proxies /api to the Go server).

import type { ViewConfig } from "./view";

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
  /** A materialized view (connector "mem") persisted to disk — survives restart. */
  persistent?: boolean;
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

export async function getSchema(source: string): Promise<{
  columns?: Column[];
  error?: string;
  // present for local-file sources: the file's last-modified time + size
  modified?: string;
  size?: number;
  path?: string;
}> {
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

// exportParquet sends the displayed columns + rows to the server, which encodes
// them as a Parquet file (the browser can't write Parquet itself) and returns the
// bytes as a Blob.
export async function exportParquet(
  columns: Column[],
  rows: Cell[][],
): Promise<Blob> {
  const res = await fetch("/api/export", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ format: "parquet", columns, rows }),
  });
  if (!res.ok) throw new Error((await res.text()) || `${res.status} ${res.statusText}`);
  return res.blob();
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

// ---- dashboards ----------------------------------------------------------
// A dashboard is a named, ordered list of panels stored server-side as YAML
// (.turntable/dashboards/<slug>.yaml). The server only stores definitions;
// panels run their queries through the normal /api/query path client-side.

export interface DashboardVariable {
  default?: string;
  options_query?: string;
}

export interface DashboardPanel {
  kind: "markdown" | "table" | "pivot" | "chart" | "stat";
  title?: string;
  text?: string; // markdown panels
  width?: "full" | "half";
  query?: string;
  view?: ViewConfig; // frozen results-pane config (chart/pivot settings)
}

export interface Dashboard {
  name: string;
  description?: string;
  refresh?: number; // seconds between automatic panel re-runs (0/absent = manual)
  variables?: Record<string, DashboardVariable>;
  panels: DashboardPanel[];
}

export interface DashboardSummary {
  slug: string;
  name: string;
  description?: string;
  panels: number;
  error?: string; // set when the YAML on disk failed to parse
}

export async function listDashboards(): Promise<DashboardSummary[]> {
  const res = await fetch("/api/dashboards");
  if (!res.ok) throw new Error(`${res.status} ${res.statusText}`);
  return res.json();
}

export async function getDashboard(slug: string): Promise<Dashboard & { slug: string }> {
  const res = await fetch("/api/dashboards/" + encodeURIComponent(slug));
  if (!res.ok) throw new Error((await res.text()) || `${res.status} ${res.statusText}`);
  return res.json();
}

// saveDashboard creates (no slug — derived from the name) or updates (slug set)
// a dashboard. Returns the slug it was stored under.
export function saveDashboard(
  d: Dashboard & { slug?: string },
): Promise<{ slug?: string; saved?: string; error?: string }> {
  return postJSON("/api/dashboards", d);
}

export async function deleteDashboard(slug: string): Promise<void> {
  const res = await fetch("/api/dashboards/" + encodeURIComponent(slug), { method: "DELETE" });
  if (!res.ok) throw new Error((await res.text()) || `${res.status} ${res.statusText}`);
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
