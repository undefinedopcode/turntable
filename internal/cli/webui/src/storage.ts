// Local persistence for the editor: recent query history, named saved queries,
// and the last-edited query. All best-effort (localStorage may be unavailable).

const HISTORY_KEY = "tt.history";
const SAVED_KEY = "tt.saved";
const LAST_KEY = "tt.lastQuery";
const MAX_HISTORY = 50;

export interface HistoryEntry {
  q: string;
  ts: number;
}

export interface SavedQuery {
  name: string;
  q: string;
}

function read<T>(key: string, fallback: T): T {
  try {
    const raw = localStorage.getItem(key);
    return raw ? (JSON.parse(raw) as T) : fallback;
  } catch {
    return fallback;
  }
}

function write(key: string, value: unknown): void {
  try {
    localStorage.setItem(key, JSON.stringify(value));
  } catch {
    /* ignore quota / disabled storage */
  }
}

export function loadHistory(): HistoryEntry[] {
  return read<HistoryEntry[]>(HISTORY_KEY, []);
}

// pushHistory records a query as the most recent, de-duplicating an identical
// previous entry and capping the list.
export function pushHistory(q: string): HistoryEntry[] {
  const trimmed = q.trim();
  if (!trimmed) return loadHistory();
  const rest = loadHistory().filter((e) => e.q !== trimmed);
  const next = [{ q: trimmed, ts: Date.now() }, ...rest].slice(0, MAX_HISTORY);
  write(HISTORY_KEY, next);
  return next;
}

export function clearHistory(): void {
  write(HISTORY_KEY, []);
}

export function loadSaved(): SavedQuery[] {
  return read<SavedQuery[]>(SAVED_KEY, []);
}

export function saveQuery(name: string, q: string): SavedQuery[] {
  const trimmed = name.trim();
  if (!trimmed) return loadSaved();
  const rest = loadSaved().filter((s) => s.name !== trimmed);
  const next = [...rest, { name: trimmed, q }].sort((a, b) =>
    a.name.localeCompare(b.name),
  );
  write(SAVED_KEY, next);
  return next;
}

export function deleteSaved(name: string): SavedQuery[] {
  const next = loadSaved().filter((s) => s.name !== name);
  write(SAVED_KEY, next);
  return next;
}

export function loadLastQuery(): string {
  return read<string>(LAST_KEY, "");
}

export function saveLastQuery(q: string): void {
  write(LAST_KEY, q);
}

// loadPaneSize / savePaneSize persist a resizable pane's pixel size (sidebar
// width, query-pane height) so the layout survives a reload.
export function loadPaneSize(key: string, fallback: number): number {
  const n = read<number>("tt.pane." + key, fallback);
  return typeof n === "number" && isFinite(n) ? n : fallback;
}

export function savePaneSize(key: string, px: number): void {
  write("tt.pane." + key, px);
}

// relTime renders a short "2m ago" style label for a timestamp.
export function relTime(ts: number): string {
  const s = Math.max(0, Math.floor((Date.now() - ts) / 1000));
  if (s < 60) return `${s}s ago`;
  const m = Math.floor(s / 60);
  if (m < 60) return `${m}m ago`;
  const h = Math.floor(m / 60);
  if (h < 24) return `${h}h ago`;
  return `${Math.floor(h / 24)}d ago`;
}
