"use client";

import { useEffect, useRef, useState } from "react";
import { format as formatSQL } from "sql-formatter";
import { ApiError, api, errorMessage } from "@/lib/api";
import { formatNumber, formatTime } from "@/lib/format";
import { clearHistory, loadHistory, pushHistory, type HistoryItem } from "@/lib/history";
import { beginDrag, useStoredSize } from "@/lib/resize";
import type { Connection, QueryResponse } from "@/lib/types";
import SqlEditor from "@/components/SqlEditor";
import { Badge, Banner } from "@/components/ui";
import ResultPanel from "./ResultPanel";

const ROW_LIMITS = [100, 500, 1000, 5000, 10000];

/** Prefixes a statement so the server describes how it would run it. */
function explain(statement: string, kind: string, analyze: boolean): string {
  const sql = statement.replace(/;\s*$/, "");
  if (kind === "mysql") return `${analyze ? "EXPLAIN ANALYZE" : "EXPLAIN FORMAT=TREE"} ${sql}`;
  return `EXPLAIN (${analyze ? "ANALYZE, BUFFERS, " : ""}FORMAT TEXT) ${sql}`;
}

/** One editor with its results. Every open tab keeps its own. */
export default function QueryTab({ connection, mayWrite, completions, sql, onSql, onSave, active }: {
  connection: Connection;
  mayWrite: boolean;
  completions: Record<string, string[]>;
  sql: string;
  onSql: (sql: string) => void;
  /** Opens the dialog that saves this tab's statement. */
  onSave: () => void;
  active: boolean;
}) {
  const [write, setWrite] = useState(false);
  const [maxRows, setMaxRows] = useState(1000);
  const [running, setRunning] = useState(false);
  const [response, setResponse] = useState<QueryResponse | null>(null);
  const [error, setError] = useState("");
  const [resultTab, setResultTab] = useState(0);
  const [history, setHistory] = useState<HistoryItem[]>([]);
  const [showHistory, setShowHistory] = useState(false);
  // Hides the editor so a large result can use the whole height.
  const [maximized, setMaximized] = useState(false);
  const selection = useRef<() => string>(() => "");
  const abort = useRef<AbortController | null>(null);
  const [editorHeight, setEditorHeight] = useStoredSize("dedie.query.editor", 280, 110, () => Math.max(160, window.innerHeight - 300));

  useEffect(() => setHistory(loadHistory(connection.id)), [connection.id, active]);
  // Leaving the page abandons a statement that is still running.
  useEffect(() => () => abort.current?.abort(), []);

  // Runs the selected text when there is a selection, otherwise everything.
  const run = async (selected?: string, wrap?: (statement: string) => string) => {
    let statement = (selected ?? selection.current()).trim() || sql.trim();
    if (!statement || running) return;
    if (wrap) statement = wrap(statement);
    const controller = new AbortController();
    abort.current = controller;
    setRunning(true);
    setError("");
    try {
      // A plan is only read, whatever the statement would do.
      const res = await api.query(connection.id, { sql: statement, write: !wrap && write && mayWrite, max_rows: maxRows }, controller.signal);
      setResponse(res);
      setResultTab(0);
    } catch (err) {
      setResponse(null);
      setError(err instanceof ApiError && err.code === "cancelled" ? "Cancelled. The server was told to stop the statement." : errorMessage(err));
    } finally {
      if (!wrap) setHistory(pushHistory(connection.id, statement));
      setRunning(false);
    }
  };

  const format = () => {
    try {
      onSql(formatSQL(sql, { language: connection.kind === "mysql" ? "mysql" : "postgresql", keywordCase: "upper", tabWidth: 2 }));
    } catch (err) {
      setError(`The statement could not be formatted: ${errorMessage(err)}`);
    }
  };

  const current = response?.results[Math.min(resultTab, (response?.results.length ?? 1) - 1)] ?? null;

  return (
    <div className="pane main-pane" style={active ? undefined : { display: "none" }}>
      {!maximized && (
        <>
          <div className="editor-pane" style={{ height: editorHeight }}>
            <SqlEditor value={sql} onChange={onSql} onRun={(s) => void run(s)} kind={connection.kind} schema={completions} selection={selection} />
          </div>
          <div className="splitter y" role="separator" aria-orientation="horizontal" title="Drag to resize"
            onPointerDown={(e) => beginDrag(e, "y", editorHeight, setEditorHeight)} />
        </>
      )}

      <div className="run-bar">
        {running ? (
          <button className="btn danger" onClick={() => abort.current?.abort()}>Cancel</button>
        ) : (
          <button className="btn primary" onClick={() => void run()} disabled={!sql.trim()} title="Ctrl/Cmd + Enter. Runs the selection when text is selected.">
            Run
          </button>
        )}
        <button className="btn small" disabled={running || !sql.trim()} onClick={() => void run(undefined, (s) => explain(s, connection.kind, false))}
          title="Show how the database would run the statement, without running it">Explain</button>
        <button className="btn small" disabled={running || !sql.trim()} onClick={() => void run(undefined, (s) => explain(s, connection.kind, true))}
          title="Run the statement in a read-only transaction and show where the time went">Analyze</button>
        <button className="btn small" disabled={!sql.trim()} onClick={format}>Format</button>
        <button className="btn small" disabled={!sql.trim()} onClick={onSave}>Save</button>
        {running && <span className="hint">Running…</span>}
        <label className="hint inline">
          Max rows
          <select className="select" value={maxRows} onChange={(e) => setMaxRows(Number(e.target.value))}>
            {ROW_LIMITS.map((n) => <option key={n} value={n}>{formatNumber(n)}</option>)}
          </select>
        </label>
        {mayWrite && (
          <label className="inline">
            <input type="checkbox" checked={write} onChange={(e) => setWrite(e.target.checked)} />
            Allow writes
          </label>
        )}
        {write && mayWrite && <Badge tone="warn">read-write: changes are committed immediately</Badge>}
        <span className="spacer" />
        <button className="btn small" onClick={() => setMaximized((v) => !v)} aria-pressed={maximized}>
          {maximized ? "Show editor" : "Maximize results"}
        </button>
        <div className="popover-anchor">
          <button className="btn small" onClick={() => setShowHistory((v) => !v)} aria-expanded={showHistory}>
            History ({history.length})
          </button>
          {showHistory && (
            <div className="popover">
              <div className="card-head">
                <h2>History for this connection</h2>
                <button className="btn small" disabled={history.length === 0}
                  onClick={() => { clearHistory(connection.id); setHistory([]); }}>
                  Clear
                </button>
              </div>
              <div className="history">
                {history.length === 0 && <div className="empty">Nothing run yet.</div>}
                {history.map((h) => (
                  <button key={h.at + h.sql} onClick={() => { onSql(h.sql); setShowHistory(false); }} title={h.sql}>
                    <div className="sql">{h.sql}</div>
                    <div className="faint" style={{ fontSize: 11.5 }}>{formatTime(h.at)}</div>
                  </button>
                ))}
              </div>
            </div>
          )}
        </div>
      </div>

      <div className="results-pane">
        {error && <Banner tone="error"><pre>{error}</pre></Banner>}
        {!response && !error && (
          <div className="empty">
            Run a statement to see its result here. Click a table on the left to browse its rows.
          </div>
        )}
        {response && (
          <>
            {response.results.length > 1 && (
              <div className="tabs" role="tablist">
                {response.results.map((r, i) => (
                  <button key={i} role="tab" aria-selected={i === resultTab} className={`tab ${i === resultTab ? "active" : ""}`} onClick={() => setResultTab(i)}>
                    Result {i + 1}{r.command ? ` · ${r.command}` : ""}
                  </button>
                ))}
              </div>
            )}
            {current ? (
              <ResultPanel result={current} name={connection.name} kind={connection.kind}>
                <span>{formatNumber(response.duration_ms)} ms</span>
                <span>{response.read_only ? "read-only transaction" : "read-write session"}</span>
              </ResultPanel>
            ) : <div className="empty">Statement executed; no result returned.</div>}
          </>
        )}
      </div>
    </div>
  );
}
