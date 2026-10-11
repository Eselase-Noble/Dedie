"use client";

import { useEffect, useMemo, useRef, useState } from "react";
import { prettyJSON } from "@/lib/format";
import type { ResultSet } from "@/lib/types";
import { Modal } from "./ui";

/** Every row has this height, which is what makes windowing possible. */
const ROW_HEIGHT = 28;
/** Rows rendered beyond the visible area, so fast scrolling shows no gaps. */
const OVERSCAN = 12;
const MIN_COLUMN = 70;
const MAX_COLUMN = 460;

/** Column widths from the header and a sample of the data, in pixels. */
function columnWidths(result: ResultSet): number[] {
  const sample = result.rows.slice(0, 200);
  return result.columns.map((c, i) => {
    let chars = Math.max(c.name.length, c.type.length);
    for (const row of sample) {
      const v = row[i];
      if (v !== null && v.length > chars) chars = v.length;
    }
    return Math.min(Math.max(chars * 7.7 + 24, MIN_COLUMN), MAX_COLUMN);
  });
}

/**
 * A read-only grid for one result set, built for wide and long results.
 *
 * Only the rows in view are in the page, so ten thousand rows scroll as
 * smoothly as ten. Columns keep a fixed width and the grid scrolls in both
 * directions under a pinned header and row-number column. Clicking a cell
 * opens its full value, since long values are cut short in the grid.
 */
export default function ResultGrid({ result }: { result: ResultSet }) {
  const scroller = useRef<HTMLDivElement>(null);
  const [scrollTop, setScrollTop] = useState(0);
  const [viewport, setViewport] = useState(600);
  const [open, setOpen] = useState<{ row: number; col: number } | null>(null);
  const widths = useMemo(() => columnWidths(result), [result]);

  useEffect(() => {
    const el = scroller.current;
    if (!el) return;
    el.scrollTop = 0;
    setScrollTop(0);
    const measure = () => setViewport(el.clientHeight);
    measure();
    const observer = new ResizeObserver(measure);
    observer.observe(el);
    return () => observer.disconnect();
  }, [result]);

  if (result.columns.length === 0) {
    return <div className="empty">{result.command || "Statement executed."}</div>;
  }

  const total = result.rows.length;
  const first = Math.max(0, Math.floor(scrollTop / ROW_HEIGHT) - OVERSCAN);
  const last = Math.min(total, Math.ceil((scrollTop + viewport) / ROW_HEIGHT) + OVERSCAN);
  const span = result.columns.length + 1;
  const rowNumberWidth = Math.max(44, String(total).length * 8 + 20);
  const value = open ? result.rows[open.row]?.[open.col] ?? null : null;

  return (
    <>
      <div className="result-grid" ref={scroller} onScroll={(e) => setScrollTop(e.currentTarget.scrollTop)}>
        <table style={{ width: rowNumberWidth + widths.reduce((a, b) => a + b, 0) }}>
          <colgroup>
            <col style={{ width: rowNumberWidth }} />
            {widths.map((w, i) => <col key={i} style={{ width: w }} />)}
          </colgroup>
          <thead>
            <tr>
              <th className="rownum">#</th>
              {result.columns.map((c, i) => (
                <th key={i} title={`${c.name} (${c.type})`}>
                  <span className="cname">{c.name}</span>
                  <span className="ctype">{c.type}</span>
                </th>
              ))}
            </tr>
          </thead>
          <tbody>
            {first > 0 && <tr className="spacer-row" style={{ height: first * ROW_HEIGHT }}><td colSpan={span} /></tr>}
            {result.rows.slice(first, last).map((row, offset) => {
              const r = first + offset;
              return (
                <tr key={r} className={open?.row === r ? "selected" : undefined}>
                  <td className="rownum">{r + 1}</td>
                  {row.map((v, i) =>
                    v === null
                      ? <td key={i} className="null" onClick={() => setOpen({ row: r, col: i })}>NULL</td>
                      // A value is shown on one line; its line breaks are kept in the cell viewer.
                      : <td key={i} onClick={() => setOpen({ row: r, col: i })}>{v.length > 300 ? v.slice(0, 300) : v}</td>,
                  )}
                </tr>
              );
            })}
            {last < total && <tr className="spacer-row" style={{ height: (total - last) * ROW_HEIGHT }}><td colSpan={span} /></tr>}
            {total === 0 && <tr><td className="null" colSpan={span}>No rows</td></tr>}
          </tbody>
        </table>
      </div>

      {open && (
        <Modal
          wide
          title={`${result.columns[open.col].name} · row ${open.row + 1}`}
          onClose={() => setOpen(null)}
          footer={
            <>
              <span className="hint" style={{ marginRight: "auto" }}>
                {result.columns[open.col].type}{value !== null ? ` · ${value.length.toLocaleString()} characters` : ""}
              </span>
              {value !== null && (
                <button className="btn" onClick={() => { void navigator.clipboard?.writeText(value); }}>Copy</button>
              )}
              <button className="btn primary" onClick={() => setOpen(null)}>Close</button>
            </>
          }
        >
          {value === null
            ? <span className="faint" style={{ fontStyle: "italic" }}>NULL</span>
            // Only objects and arrays are reformatted; a number or plain
            // text must be shown exactly as the database returned it.
            : <pre className="cell-value">{/^\s*[[{]/.test(value) ? prettyJSON(value) : value}</pre>}
        </Modal>
      )}
    </>
  );
}
