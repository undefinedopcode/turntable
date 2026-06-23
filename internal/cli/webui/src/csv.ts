import type { Cell, QueryResult } from "./api";

function escapeCell(c: Cell): string {
  const s =
    c === null ? "" : typeof c === "object" ? JSON.stringify(c) : String(c);
  return /[",\n]/.test(s) ? `"${s.replace(/"/g, '""')}"` : s;
}

export function resultToCSV(result: QueryResult): string {
  const lines = [result.columns.map((c) => escapeCell(c.name)).join(",")];
  for (const row of result.rows) {
    lines.push(row.map(escapeCell).join(","));
  }
  return lines.join("\n");
}

export function downloadCSV(csv: string, filename = "turntable.csv"): void {
  const blob = new Blob([csv], { type: "text/csv" });
  const url = URL.createObjectURL(blob);
  const a = document.createElement("a");
  a.href = url;
  a.download = filename;
  a.click();
  URL.revokeObjectURL(url);
}
