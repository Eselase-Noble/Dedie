import { quoteIdent } from "./format";
import type { ResultSet } from "./types";

// Ways of writing a result set to a file. CSV lives in csv.ts.

const NUMERIC = /^(smallint|integer|bigint|int[248]?|numeric|decimal|real|double precision|float[48]?|tinyint|mediumint|double|float|oid)$/i;

/** An array of objects keyed by column name. Numbers and booleans keep their type. */
export function toJSON(result: ResultSet): string {
  const rows = result.rows.map((row) => {
    const out: Record<string, unknown> = {};
    result.columns.forEach((c, i) => {
      const v = row[i];
      if (v === null) out[c.name] = null;
      else if (NUMERIC.test(c.type) && /^-?\d+(\.\d+)?$/.test(v) && Number.isSafeInteger(Math.trunc(Number(v)))) out[c.name] = Number(v);
      else if (/^bool/i.test(c.type)) out[c.name] = v === "t" || v === "true" || v === "1";
      else if (/^jsonb?$/i.test(c.type)) {
        try { out[c.name] = JSON.parse(v); } catch { out[c.name] = v; }
      } else out[c.name] = v;
    });
    return out;
  });
  return JSON.stringify(rows, null, 2) + "\n";
}

/**
 * A quoted SQL literal. MySQL treats a backslash inside a string as an
 * escape character, so it is doubled there.
 */
export function sqlLiteral(value: string | null, kind = "postgres"): string {
  if (value === null) return "NULL";
  let v = value.replace(/'/g, "''");
  if (kind === "mysql") v = v.replace(/\\/g, "\\\\");
  return `'${v}'`;
}

/** One INSERT statement per row, for loading the rows somewhere else. */
export function toInserts(result: ResultSet, table: string, kind = "postgres"): string {
  const cols = result.columns.map((c) => quoteIdent(c.name, kind)).join(", ");
  return result.rows
    .map((row) => `INSERT INTO ${table} (${cols}) VALUES (${row.map((v, i) =>
      v !== null && NUMERIC.test(result.columns[i].type) && /^-?\d+(\.\d+)?$/.test(v) ? v : sqlLiteral(v, kind)).join(", ")});`)
    .join("\n") + "\n";
}
