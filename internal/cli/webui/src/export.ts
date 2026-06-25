import type { Cell, Column } from "./api";

// Export helpers operate on a column list + the currently displayed rows (so
// filtering/sorting in the results view carries through to the export).

function cellString(c: Cell): string {
  return c === null ? "" : typeof c === "object" ? JSON.stringify(c) : String(c);
}

function csvField(c: Cell): string {
  const s = cellString(c);
  return /[",\n]/.test(s) ? `"${s.replace(/"/g, '""')}"` : s;
}

export function toCSV(columns: Column[], rows: Cell[][]): string {
  const lines = [columns.map((c) => csvField(c.name)).join(",")];
  for (const row of rows) lines.push(row.map(csvField).join(","));
  return lines.join("\n");
}

export function toTSV(columns: Column[], rows: Cell[][]): string {
  const lines = [columns.map((c) => c.name).join("\t")];
  for (const row of rows) lines.push(row.map(cellString).join("\t"));
  return lines.join("\n");
}

function rowObjects(columns: Column[], rows: Cell[][]): Record<string, Cell>[] {
  return rows.map((row) => {
    const o: Record<string, Cell> = {};
    columns.forEach((c, i) => (o[c.name] = row[i] ?? null));
    return o;
  });
}

export function toJSON(columns: Column[], rows: Cell[][]): string {
  return JSON.stringify(rowObjects(columns, rows), null, 2);
}

export function toNDJSON(columns: Column[], rows: Cell[][]): string {
  return rowObjects(columns, rows)
    .map((o) => JSON.stringify(o))
    .join("\n");
}

export function download(
  text: string,
  filename: string,
  mime = "text/plain",
): void {
  const blob = new Blob([text], { type: mime });
  const url = URL.createObjectURL(blob);
  const a = document.createElement("a");
  a.href = url;
  a.download = filename;
  a.click();
  URL.revokeObjectURL(url);
}

export async function copyText(text: string): Promise<void> {
  try {
    await navigator.clipboard.writeText(text);
  } catch {
    // Fallback for non-secure contexts where the Clipboard API is unavailable.
    const ta = document.createElement("textarea");
    ta.value = text;
    ta.style.position = "fixed";
    ta.style.opacity = "0";
    document.body.appendChild(ta);
    ta.select();
    try {
      document.execCommand("copy");
    } finally {
      document.body.removeChild(ta);
    }
  }
}
