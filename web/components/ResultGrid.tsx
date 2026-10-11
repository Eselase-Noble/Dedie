"use client";

import { useEffect, useMemo, useRef, useState } from "react";
import { errorMessage } from "@/lib/api";
import { prettyJSON } from "@/lib/format";
import type { ResultSet } from "@/lib/types";
import { Banner, Modal } from "./ui";
import { copyText } from "@/lib/desktop";

/** Every row has this height, which is what makes windowing possible. */
const ROW_HEIGHT = 28;
/** Rows rendered beyond the visible area, so fast scrolling shows no gaps. */
const OVERSCAN = 12;
const MIN_COLUMN = 70;
const MAX_COLUMN = 460;
// A result of one column is usually text meant to be read whole, such as
// a query plan, and has no neighbours to make room for.
const MAX_ONLY_COLUMN = 1400;
const SHOWN_CHARS = 300;

type Row = (string | null)[];

/** What the grid needs to let rows be changed. */
export interface GridEditor {
  /** Columns whose values may be edited. */
  canEdit: (column: string) => boolean;
  update: (row: Row, column: string, value: string | null) => Promise<void>;
  remove: (row: Row) => Promise<void>;
}

export interface Sort {
  column: number;
  descending: boolean;
}

/** Column widths from the header and a sample of the data, in pixels. */
function columnWidths(result: ResultSet): number[] {
  const sample = result.rows.slice(0, 200);
  return result.columns.map((c, i) => {
    let chars = Math.max(c.name.length + 2, c.type.length);
    for (const row of sample) {
      const v = row[i];
      if (v !== null && v.length > chars) chars = v.length;
    }
    const widest = result.columns.length === 1 ? MAX_ONLY_COLUMN : MAX_COLUMN;
    return Math.min(Math.max(Math.min(chars, SHOWN_CHARS) * 7.7 + 24, MIN_COLUMN), widest);
  });
}

/**
 * The rows to show, as indexes into result.rows: those matching the filter
 * text, in the chosen order. A column whose values are all numbers sorts
 * numerically; NULLs go last either way.
 */
export function visibleRows(result: ResultSet, filter: string, sort: Sort | null): number[] {
  const needle = filter.trim().toLowerCase();
  let order = result.rows.map((_, i) => i);
  if (needle) {
    order = order.filter((i) => result.rows[i].some((v) => v !== null && v.toLowerCase().includes(needle)));
  }
  if (sort) {
    const col = sort.column;
    const numeric = order.every((i) => {
      const v = result.rows[i][col];
      return v === null || (v.trim() !== "" && Number.isFinite(Number(v)));
    });
    const dir = sort.descending ? -1 : 1;
    order.sort((a, b) => {
      const x = result.rows[a][col];
      const y = result.rows[b][col];
      if (x === null || y === null) return x === y ? a - b : x === null ? 1 : -1;
      const cmp = numeric ? Number(x) - Number(y) : x < y ? -1 : x > y ? 1 : 0;
      return cmp !== 0 ? cmp * dir : a - b;
    });
  }
  return order;
}

/**
 * A grid for one result set, built for wide and long results.
 *
 * Only the rows in view are in the page, so ten thousand rows scroll as
 * smoothly as ten. Columns keep a fixed width and the grid scrolls in both
 * directions under a pinned header and row-number column. Clicking a header
 * sorts; clicking a cell opens its full value, where it can be copied and,
 * when an editor is supplied, changed.
 */
export default function ResultGrid({ result, order, sort, onSort, editor }: {
  result: ResultSet;
  /** Rows to show, from visibleRows. */
  order: number[];
  sort: Sort | null;
  onSort: (sort: Sort | null) => void;
  editor?: GridEditor;
}) {
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
    const measure = () => setViewport(el.clientHeight || 600);
    measure();
    const observer = new ResizeObserver(measure);
    observer.observe(el);
    return () => observer.disconnect();
  }, [result]);

  if (result.columns.length === 0) {
    return <div className="empty">{result.command || "Statement executed."}</div>;
  }

  const total = order.length;
  const first = Math.max(0, Math.floor(scrollTop / ROW_HEIGHT) - OVERSCAN);
  const last = Math.min(total, Math.ceil((scrollTop + viewport) / ROW_HEIGHT) + OVERSCAN);
  const span = result.columns.length + 1;
  const rowNumberWidth = Math.max(44, String(result.rows.length).length * 8 + 20);

  const cycleSort = (column: number) => {
    if (!sort || sort.column !== column) onSort({ column, descending: false });
    else if (!sort.descending) onSort({ column, descending: true });
    else onSort(null);
  };

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
                <th key={i} className="sortable" title={`${c.name} (${c.type}) · click to sort`} onClick={() => cycleSort(i)}
                  aria-sort={sort?.column === i ? (sort.descending ? "descending" : "ascending") : "none"}>
                  <span className="cname">{c.name}{sort?.column === i && <span className="sort-mark">{sort.descending ? " ↓" : " ↑"}</span>}</span>
                  <span className="ctype">{c.type}</span>
                </th>
              ))}
            </tr>
          </thead>
          <tbody>
            {first > 0 && <tr className="spacer-row" style={{ height: first * ROW_HEIGHT }}><td colSpan={span} /></tr>}
            {order.slice(first, last).map((r) => (
              <tr key={r} className={open?.row === r ? "selected" : undefined}>
                <td className="rownum">{r + 1}</td>
                {result.rows[r].map((v, i) =>
                  v === null
                    ? <td key={i} className="null" onClick={() => setOpen({ row: r, col: i })}>NULL</td>
                    // A value is shown on one line, with its spacing as it is:
                    // the indentation of a query plan is its structure. The
                    // line breaks are kept in the cell viewer.
                    : <td key={i} onClick={() => setOpen({ row: r, col: i })}>{v.slice(0, SHOWN_CHARS).replace(/\s*[\r\n]+\s*/g, " ")}</td>,
                )}
              </tr>
            ))}
            {last < total && <tr className="spacer-row" style={{ height: (total - last) * ROW_HEIGHT }}><td colSpan={span} /></tr>}
            {total === 0 && <tr><td className="null" colSpan={span}>{result.rows.length === 0 ? "No rows" : "No row matches the filter"}</td></tr>}
          </tbody>
        </table>
      </div>
      {open && result.rows[open.row] && (
        <CellViewer result={result} row={open.row} col={open.col} editor={editor} onClose={() => setOpen(null)} />
      )}
    </>
  );
}

/** Shows one value in full and, with an editor, lets it be changed. */
function CellViewer({ result, row, col, editor, onClose }: {
  result: ResultSet; row: number; col: number; editor?: GridEditor; onClose: () => void;
}) {
  const column = result.columns[col];
  const value = result.rows[row][col];
  const editable = !!editor && editor.canEdit(column.name);
  const [editing, setEditing] = useState(false);
  const [draft, setDraft] = useState(value ?? "");
  const [confirmDelete, setConfirmDelete] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");

  const run = async (action: () => Promise<void>) => {
    setBusy(true);
    setError("");
    try {
      await action();
      onClose();
    } catch (err) {
      setError(errorMessage(err));
      setBusy(false);
    }
  };

  return (
    <Modal wide title={`${column.name} · row ${row + 1}`} onClose={onClose} footer={
      editing ? (
        <>
          <button className="btn" style={{ marginRight: "auto" }} disabled={busy}
            onClick={() => run(() => editor!.update(result.rows[row], column.name, null))}>Set to NULL</button>
          <button className="btn" onClick={() => setEditing(false)} disabled={busy}>Cancel</button>
          <button className="btn primary" disabled={busy} onClick={() => run(() => editor!.update(result.rows[row], column.name, draft))}>
            {busy ? "Saving…" : "Save"}
          </button>
        </>
      ) : confirmDelete ? (
        <>
          <span style={{ marginRight: "auto" }}>Delete this whole row? This cannot be undone.</span>
          <button className="btn" onClick={() => setConfirmDelete(false)} disabled={busy}>Cancel</button>
          <button className="btn danger" disabled={busy} onClick={() => run(() => editor!.remove(result.rows[row]))}>Delete row</button>
        </>
      ) : (
        <>
          <span className="hint" style={{ marginRight: "auto" }}>
            {column.type}{value !== null ? ` · ${value.length.toLocaleString()} characters` : ""}
          </span>
          {editor && <button className="btn" onClick={() => setConfirmDelete(true)}>Delete row</button>}
          {editable && <button className="btn" onClick={() => setEditing(true)}>Edit</button>}
          {value !== null && <button className="btn" onClick={() => { copyText(value); }}>Copy</button>}
          <button className="btn primary" onClick={onClose}>Close</button>
        </>
      )
    }>
      {error && <Banner tone="error">{error}</Banner>}
      {editing ? (
        <textarea className="input mono cell-edit" value={draft} onChange={(e) => setDraft(e.target.value)} autoFocus spellCheck={false} />
      ) : value === null
        ? <span className="faint" style={{ fontStyle: "italic" }}>NULL</span>
        // Only objects and arrays are reformatted; a number or plain
        // text must be shown exactly as the database returned it.
        : <pre className="cell-value">{/^\s*[[{]/.test(value) ? prettyJSON(value) : value}</pre>}
    </Modal>
  );
}
