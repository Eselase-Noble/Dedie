"use client";

import { useCallback, useEffect, useMemo, useState, type FormEvent } from "react";
import { api, errorMessage } from "@/lib/api";
import { formatNumber, quoteIdent } from "@/lib/format";
import type { CatalogTable, Connection, ResultSet } from "@/lib/types";
import type { GridEditor } from "@/components/ResultGrid";
import { Badge, Banner, Field, Loading, Modal } from "@/components/ui";
import ResultPanel from "./ResultPanel";

const PAGE_SIZES = [100, 500, 1000, 5000];

/** A form with one field per column. An empty field takes the column's default. */
function InsertRow({ table, onClose, onInsert }: {
  table: CatalogTable; onClose: () => void; onInsert: (values: Record<string, string | null>) => Promise<void>;
}) {
  const [values, setValues] = useState<Record<string, string>>({});
  const [nulls, setNulls] = useState<Record<string, boolean>>({});
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");

  const submit = async (e: FormEvent) => {
    e.preventDefault();
    setBusy(true);
    setError("");
    const out: Record<string, string | null> = {};
    for (const c of table.columns) {
      if (nulls[c.name]) out[c.name] = null;
      else if ((values[c.name] ?? "") !== "") out[c.name] = values[c.name];
    }
    try {
      await onInsert(out);
      onClose();
    } catch (err) {
      setError(errorMessage(err));
      setBusy(false);
    }
  };

  return (
    <Modal wide title={`New row in ${table.name}`} onClose={onClose} footer={
      <>
        <button className="btn" type="button" onClick={onClose} disabled={busy}>Cancel</button>
        <button className="btn primary" type="submit" form="insert-form" disabled={busy}>{busy ? "Inserting…" : "Insert row"}</button>
      </>
    }>
      <form id="insert-form" onSubmit={submit}>
        {error && <Banner tone="error">{error}</Banner>}
        <p className="hint" style={{ marginBottom: 12 }}>Leave a field empty to use the column&apos;s default.</p>
        <div className="form-grid">
          {table.columns.map((c) => (
            <Field key={c.name} label={`${c.name}${c.primary_key ? " (key)" : ""}`}
              hint={<>{c.type}{c.nullable ? "" : " · required"}{c.default ? ` · default ${c.default}` : ""}</>}>
              <div className="input-group">
                <input className="input mono" value={values[c.name] ?? ""} disabled={!!nulls[c.name]} spellCheck={false}
                  onChange={(e) => setValues((v) => ({ ...v, [c.name]: e.target.value }))} />
                {c.nullable && (
                  <label className="inline hint">
                    <input type="checkbox" checked={!!nulls[c.name]} onChange={(e) => setNulls((n) => ({ ...n, [c.name]: e.target.checked }))} />
                    NULL
                  </label>
                )}
              </div>
            </Field>
          ))}
        </div>
      </form>
    </Modal>
  );
}

/**
 * Browses one table a page at a time, ordered by its primary key, with an
 * optional WHERE condition. With editing switched on, a cell can be changed
 * and rows added or deleted; each change is one audited statement that the
 * server applies only if it touches exactly one row.
 */
export default function TableTab({ connection, schema, table, info, mayWrite, active }: {
  connection: Connection;
  schema: string;
  table: string;
  /** The table's columns from the catalog; missing while it loads. */
  info: CatalogTable | null;
  mayWrite: boolean;
  active: boolean;
}) {
  const kind = connection.kind;
  const qualified = `${quoteIdent(schema, kind)}.${quoteIdent(table, kind)}`;
  const keys = useMemo(() => info?.columns.filter((c) => c.primary_key).map((c) => c.name) ?? [], [info]);

  const [where, setWhere] = useState("");
  const [applied, setApplied] = useState("");
  const [pageSize, setPageSize] = useState(500);
  const [page, setPage] = useState(0);
  const [result, setResult] = useState<ResultSet | null>(null);
  const [ms, setMs] = useState(0);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState("");
  const [editing, setEditing] = useState(false);
  const [inserting, setInserting] = useState(false);

  const load = useCallback(async () => {
    setLoading(true);
    setError("");
    const order = keys.length > 0 ? ` ORDER BY ${keys.map((k) => quoteIdent(k, kind)).join(", ")}` : "";
    const sql = `SELECT * FROM ${qualified}${applied.trim() ? ` WHERE ${applied.trim()}` : ""}${order}${kind === "oracle" ? ` OFFSET ${page * pageSize} ROWS FETCH NEXT ${pageSize} ROWS ONLY` : ` LIMIT ${pageSize} OFFSET ${page * pageSize}`}`;
    try {
      const res = await api.query(connection.id, { sql, write: false, max_rows: pageSize });
      setResult(res.results[0] ?? null);
      setMs(res.duration_ms);
    } catch (err) {
      setResult(null);
      setError(errorMessage(err));
    } finally {
      setLoading(false);
    }
  }, [connection.id, qualified, applied, pageSize, page, keys, kind]);

  useEffect(() => { void load(); }, [load]);

  const editor: GridEditor | undefined = useMemo(() => {
    if (!editing || !result || keys.length === 0) return undefined;
    const keyOf = (row: (string | null)[]) => {
      const key: Record<string, string | null> = {};
      for (const k of keys) key[k] = row[result.columns.findIndex((c) => c.name === k)] ?? null;
      return key;
    };
    return {
      canEdit: () => true,
      update: async (row, column, value) => {
        await api.changeRow(connection.id, { schema, table, action: "update", key: keyOf(row), values: { [column]: value } });
        await load();
      },
      remove: async (row) => {
        await api.changeRow(connection.id, { schema, table, action: "delete", key: keyOf(row) });
        await load();
      },
    };
  }, [editing, result, keys, connection.id, schema, table, load]);

  const applyFilter = (e: FormEvent) => {
    e.preventDefault();
    setPage(0);
    setApplied(where);
  };

  return (
    <div className="pane main-pane" style={active ? undefined : { display: "none" }}>
      <form className="run-bar" onSubmit={applyFilter}>
        <strong className="mono">{schema}.{table}</strong>
        <span className="hint">WHERE</span>
        <input className="input mono grow" value={where} onChange={(e) => setWhere(e.target.value)} spellCheck={false}
          placeholder={`status = 'open' AND created_at > '2026-01-01'`} aria-label="Filter condition" />
        <button className="btn small primary" type="submit" disabled={loading}>Apply</button>
        {applied && <button className="btn small" type="button" onClick={() => { setWhere(""); setApplied(""); setPage(0); }}>Clear</button>}
        <button className="btn small" type="button" onClick={() => void load()} disabled={loading}>Refresh</button>
        <span className="spacer" />
        {mayWrite && (
          <label className="inline" title={keys.length === 0 ? "This table has no primary key, so a row cannot be identified for editing" : ""}>
            <input type="checkbox" checked={editing} disabled={keys.length === 0} onChange={(e) => setEditing(e.target.checked)} />
            Edit data
          </label>
        )}
        {editing && info && <button className="btn small" type="button" onClick={() => setInserting(true)}>Add row</button>}
      </form>
      {editing && <Banner tone="warn">Editing is on. Click a cell to change it or delete its row; each change is saved to the database immediately and recorded in the audit log.</Banner>}
      {mayWrite && info && keys.length === 0 && <div className="hint" style={{ padding: "0 4px 6px" }}>This table has no primary key, so its rows cannot be edited here.</div>}

      <div className="results-pane">
        {error && <Banner tone="error"><pre>{error}</pre></Banner>}
        {!result && loading && <Loading />}
        {result && (
          <ResultPanel result={result} name={`${connection.name}-${table}`} kind={kind} tableName={qualified} editor={editor}>
            <span>{formatNumber(ms)} ms</span>
            <span className="inline">
              <button className="btn small" disabled={page === 0 || loading} onClick={() => setPage((p) => p - 1)}>‹ Previous</button>
              <span>rows {formatNumber(page * pageSize + 1)}–{formatNumber(page * pageSize + result.rows.length)}</span>
              <button className="btn small" disabled={result.rows.length < pageSize || loading} onClick={() => setPage((p) => p + 1)}>Next ›</button>
            </span>
            <label className="hint inline">
              Page size
              <select className="select" value={pageSize} onChange={(e) => { setPageSize(Number(e.target.value)); setPage(0); }}>
                {PAGE_SIZES.map((n) => <option key={n} value={n}>{formatNumber(n)}</option>)}
              </select>
            </label>
            {keys.length > 0 && <Badge>ordered by {keys.join(", ")}</Badge>}
          </ResultPanel>
        )}
      </div>
      {inserting && info && (
        <InsertRow table={info} onClose={() => setInserting(false)}
          onInsert={async (values) => {
            await api.changeRow(connection.id, { schema, table, action: "insert", values });
            await load();
          }} />
      )}
    </div>
  );
}

