// Formatting helpers shared across pages.

const UNITS = ["B", "KB", "MB", "GB", "TB", "PB"];

export function formatBytes(n: number | undefined | null): string {
  if (n === undefined || n === null || n < 0) return "—";
  let value = n;
  let unit = 0;
  while (value >= 1024 && unit < UNITS.length - 1) {
    value /= 1024;
    unit++;
  }
  return `${unit === 0 ? value : value.toFixed(value >= 100 ? 0 : 1)} ${UNITS[unit]}`;
}

export function formatNumber(n: number | undefined | null): string {
  if (n === undefined || n === null) return "—";
  return n.toLocaleString("en-US");
}

/** Go encodes an unset time as year 1; treat that as absent. */
export function isZeroTime(value: string | undefined | null): boolean {
  return !value || value.startsWith("0001-");
}

export function formatTime(value: string | undefined | null): string {
  if (isZeroTime(value)) return "—";
  const d = new Date(value as string);
  if (Number.isNaN(d.getTime())) return "—";
  return d.toLocaleString(undefined, {
    year: "numeric", month: "short", day: "2-digit", hour: "2-digit", minute: "2-digit", second: "2-digit",
  });
}

export function formatDuration(seconds: number): string {
  if (seconds < 60) return `${seconds}s`;
  if (seconds < 3600) return `${Math.floor(seconds / 60)}m ${seconds % 60}s`;
  const h = Math.floor(seconds / 3600);
  if (h < 48) return `${h}h ${Math.floor((seconds % 3600) / 60)}m`;
  return `${Math.floor(h / 24)}d ${h % 24}h`;
}

/** Quotes an SQL identifier the way PostgreSQL expects. */
export function quoteIdent(name: string, kind = "postgres"): string {
  // MySQL quotes identifiers with backticks; double quotes there are strings.
  if (kind === "mysql") return "`" + name.replace(/`/g, "``") + "`";
  return `"${name.replace(/"/g, '""')}"`;
}

/** Pretty-prints a JSON string, returning it unchanged when it is not JSON. */
export function prettyJSON(text: string): string {
  if (!text) return "";
  try {
    return JSON.stringify(JSON.parse(text), null, 2);
  } catch {
    return text;
  }
}
