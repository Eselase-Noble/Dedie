"use client";

import { useEffect, useMemo, useState, type ReactNode } from "react";
import { download, toCSV } from "@/lib/csv";
import { toInserts, toJSON } from "@/lib/export";
import { formatNumber } from "@/lib/format";
import type { ResultSet } from "@/lib/types";
import ResultGrid, { visibleRows, type GridEditor, type Sort } from "@/components/ResultGrid";
import { Badge } from "@/components/ui";

/**
 * One result set with its tools: a filter, sorting, and export of exactly
 * the rows shown, in the order shown.
 */
export default function ResultPanel({ result, name, kind, tableName, editor, children }: {
  result: ResultSet;
  /** Base name for exported files. */
  name: string;
  kind?: string;
  /** Table name used in exported INSERT statements. */
  tableName?: string;
  editor?: GridEditor;
  /** Extra items for the status line, such as timing. */
  children?: ReactNode;
}) {
  const [filter, setFilter] = useState("");
  const [sort, setSort] = useState<Sort | null>(null);
  const [menu, setMenu] = useState(false);

  // A new result has different columns; an old sort would point at the wrong one.
  useEffect(() => { setSort(null); setFilter(""); }, [result.columns.length, result.columns.map((c) => c.name).join("\u0000")]);

  const order = useMemo(() => visibleRows(result, filter, sort), [result, filter, sort]);
  const hasColumns = result.columns.length > 0;

  const exportAs = (format: "csv" | "json" | "sql") => {
    setMenu(false);
    const shown: ResultSet = { ...result, rows: order.map((i) => result.rows[i]) };
    const stamp = new Date().toISOString().replace(/[:.]/g, "-").slice(0, 19);
    const file = `${name.replace(/[^\w.-]+/g, "_")}-${stamp}`;
    if (format === "csv") download(`${file}.csv`, toCSV(shown));
    if (format === "json") download(`${file}.json`, toJSON(shown), "application/json;charset=utf-8");
    if (format === "sql") download(`${file}.sql`, toInserts(shown, tableName || "table_name", kind), "application/sql;charset=utf-8");
  };

  const copy = () => {
    setMenu(false);
    const lines = [result.columns.map((c) => c.name).join("\t"), ...order.map((i) => result.rows[i].map((v) => v ?? "").join("\t"))];
    void navigator.clipboard?.writeText(lines.join("\n"));
  };

  return (
    <>
      <ResultGrid result={result} order={order} sort={sort} onSort={setSort} editor={editor} />
      <div className="result-meta">
        {hasColumns && (
          <span>
            {order.length !== result.rows.length && `${formatNumber(order.length)} of `}
            {formatNumber(result.rows.length)} row{result.rows.length === 1 ? "" : "s"} · {formatNumber(result.columns.length)} column{result.columns.length === 1 ? "" : "s"}
          </span>
        )}
        {result.command && <span className="mono">{result.command}</span>}
        {children}
        {result.truncated && <Badge tone="warn">truncated at {formatNumber(result.rows.length)} rows; raise the limit or add LIMIT</Badge>}
        <span className="spacer" />
        {hasColumns && (
          <>
            <input className="input small" placeholder="Filter rows" value={filter} onChange={(e) => setFilter(e.target.value)} aria-label="Filter rows" />
            <div className="popover-anchor">
              <button className="btn small" onClick={() => setMenu((v) => !v)} aria-expanded={menu}>Export ▾</button>
              {menu && (
                <div className="popover menu up">
                  <button onClick={() => exportAs("csv")}>CSV file</button>
                  <button onClick={() => exportAs("json")}>JSON file</button>
                  <button onClick={() => exportAs("sql")}>SQL INSERT statements</button>
                  <button onClick={copy}>Copy to clipboard (for spreadsheets)</button>
                </div>
              )}
            </div>
          </>
        )}
      </div>
    </>
  );
}
