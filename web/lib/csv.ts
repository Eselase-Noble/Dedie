import type { ResultSet } from "./types";

function cell(value: string | null): string {
  if (value === null) return "";
  return /[",\r\n]/.test(value) ? `"${value.replace(/"/g, '""')}"` : value;
}

/** Serialises a result set as RFC 4180 CSV. NULL becomes an empty field. */
export function toCSV(result: ResultSet): string {
  const lines = [result.columns.map((c) => cell(c.name)).join(",")];
  for (const row of result.rows) lines.push(row.map(cell).join(","));
  return lines.join("\r\n") + "\r\n";
}

/** Offers text to the browser as a file download. */
export function download(filename: string, text: string, mime = "text/csv;charset=utf-8"): void {
  const url = URL.createObjectURL(new Blob([text], { type: mime }));
  const a = document.createElement("a");
  a.href = url;
  a.download = filename;
  document.body.appendChild(a);
  a.click();
  a.remove();
  URL.revokeObjectURL(url);
}
