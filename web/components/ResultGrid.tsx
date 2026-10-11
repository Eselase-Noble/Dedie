"use client";

import type { ResultSet } from "@/lib/types";

/** A read-only grid for one result set. NULL is rendered distinctly from an empty string. */
export default function ResultGrid({ result }: { result: ResultSet }) {
  if (result.columns.length === 0) {
    return <div className="empty">{result.command || "Statement executed."}</div>;
  }
  return (
    <div className="result-grid">
      <table>
        <thead>
          <tr>
            <th className="rownum">#</th>
            {result.columns.map((c, i) => (
              <th key={i}>
                {c.name}
                <span className="ctype">{c.type}</span>
              </th>
            ))}
          </tr>
        </thead>
        <tbody>
          {result.rows.map((row, r) => (
            <tr key={r}>
              <td className="rownum">{r + 1}</td>
              {row.map((v, i) =>
                v === null
                  ? <td key={i} className="null">NULL</td>
                  : <td key={i} title={v.length > 60 ? v : undefined}>{v}</td>,
              )}
            </tr>
          ))}
          {result.rows.length === 0 && (
            <tr><td className="null" colSpan={result.columns.length + 1}>No rows</td></tr>
          )}
        </tbody>
      </table>
    </div>
  );
}
