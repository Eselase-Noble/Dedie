"use client";

import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { api, errorMessage } from "@/lib/api";
import { download, toCSV } from "@/lib/csv";
import { formatNumber, formatTime, quoteIdent } from "@/lib/format";
import { clearHistory, loadHistory, pushHistory, type HistoryItem } from "@/lib/history";
import { useLoad } from "@/lib/hooks";
import { beginDrag, useStoredSize } from "@/lib/resize";
import type { Catalog, QueryResponse } from "@/lib/types";
import ResultGrid from "@/components/ResultGrid";
import CreateObject from "@/components/CreateObjects";
import SchemaTree from "@/components/SchemaTree";
import { useSession } from "@/components/session";
import SqlEditor from "@/components/SqlEditor";
import { Badge, Banner, Loading } from "@/components/ui";

const ROW_LIMITS = [100, 500, 1000, 5000, 10000];
const LAST_CONNECTION = "dedie.query.connection";

export default function QueryPage() {
  const { can } = useSession();
  const { data: connections, error: loadError, reload: reloadConnections } = useLoad(() => api.connections(), []);
  const [connId, setConnId] = useState("");
  const [catalog, setCatalog] = useState<Catalog | null>(null);
  const [catalogError, setCatalogError] = useState("");
  const [catalogLoading, setCatalogLoading] = useState(false);
  const [sql, setSql] = useState("");
  const [write, setWrite] = useState(false);
  const [maxRows, setMaxRows] = useState(1000);
  const [running, setRunning] = useState(false);
  const [response, setResponse] = useState<QueryResponse | null>(null);
  const [error, setError] = useState("");
  const [tab, setTab] = useState(0);
  const [history, setHistory] = useState<HistoryItem[]>([]);
  const [showHistory, setShowHistory] = useState(false);
  const [showTree, setShowTree] = useState(true);
  // Hides the editor so a large result can use the whole height.
  const [maximized, setMaximized] = useState(false);
  const selection = useRef<() => string>(() => "");
  // The object being created from the Create menu, if any.
  const [creating, setCreating] = useState<"database" | "schema" | "table" | null>(null);
  const [showCreate, setShowCreate] = useState(false);
  const [notice, setNotice] = useState("");

  // Pane sizes are draggable and remembered. The limits leave room for the
  // other pane whatever the window size.
  const [treeWidth, setTreeWidth] = useStoredSize("dedie.query.tree", 270, 180, () => Math.max(220, window.innerWidth * 0.45));
  const [editorHeight, setEditorHeight] = useStoredSize("dedie.query.editor", 280, 110, () => Math.max(160, window.innerHeight - 260));

  const connection = connections?.find((c) => c.id === connId) ?? null;
  const mayWrite = can("query.write") && !!connection && !connection.read_only;

  // Reopen the connection used last time, if it still exists.
  useEffect(() => {
    if (!connections || connId) return;
    let remembered = "";
    try {
      remembered = window.localStorage.getItem(LAST_CONNECTION) ?? "";
    } catch {
      // Storage unavailable: fall back to the first connection.
    }
    const pick = connections.find((c) => c.id === remembered) ?? connections[0];
    if (pick) setConnId(pick.id);
  }, [connections, connId]);

  const loadCatalog = useCallback(async (id: string) => {
    setCatalogLoading(true);
    setCatalogError("");
    try {
      setCatalog(await api.catalog(id));
    } catch (err) {
      setCatalog(null);
      setCatalogError(errorMessage(err));
    } finally {
      setCatalogLoading(false);
    }
  }, []);

  useEffect(() => {
    if (!connId) return;
    setWrite(false);
    setResponse(null);
    setError("");
    setHistory(loadHistory(connId));
    try {
      window.localStorage.setItem(LAST_CONNECTION, connId);
    } catch {
      // Remembering the connection is optional.
    }
    void loadCatalog(connId);
  }, [connId, loadCatalog]);

  // Table and column names for the editor's completions. A table is offered
  // both by its bare name and qualified with its schema.
  const completions = useMemo(() => {
    const out: Record<string, string[]> = {};
    for (const schema of catalog?.schemas ?? []) {
      for (const t of [...schema.tables, ...schema.views]) {
        const columns = t.columns.map((c) => c.name);
        out[t.name] = columns;
        out[`${schema.name}.${t.name}`] = columns;
      }
    }
    return out;
  }, [catalog]);

  // Runs the selected text when there is a selection, otherwise everything.
  const run = async (selected?: string) => {
    const statement = (selected ?? selection.current()).trim() || sql.trim();
    if (!connId || !statement || running) return;
    setRunning(true);
    setError("");
    try {
      const res = await api.query(connId, { sql: statement, write: write && mayWrite, max_rows: maxRows });
      setResponse(res);
      setTab(0);
    } catch (err) {
      setResponse(null);
      setError(errorMessage(err));
    } finally {
      setHistory(pushHistory(connId, statement));
      setRunning(false);
    }
  };

  const current = response?.results[Math.min(tab, (response?.results.length ?? 1) - 1)] ?? null;

  const exportCSV = () => {
    if (!current) return;
    const stamp = new Date().toISOString().replace(/[:.]/g, "-").slice(0, 19);
    download(`${connection?.name ?? "result"}-${stamp}.csv`, toCSV(current));
  };

  return (
    <div className="workspace">
      <div className="workspace-bar">
        <h1>Query browser</h1>
        <select className="select" value={connId} onChange={(e) => setConnId(e.target.value)} aria-label="Connection">
          {(!connections || connections.length === 0) && <option value="">No connections</option>}
          {connections?.map((c) => <option key={c.id} value={c.id}>{c.name} ({c.kind}, {c.database})</option>)}
        </select>
        {connection?.read_only && <Badge>read-only connection</Badge>}
        <span className="spacer" />
        {mayWrite && (
          <div className="popover-anchor">
            <button className="btn small" onClick={() => setShowCreate((v) => !v)} aria-expanded={showCreate}>Create…</button>
            {showCreate && (
              <div className="popover menu">
                <button onClick={() => { setCreating("database"); setShowCreate(false); }}>New database</button>
                {connection?.kind !== "mysql" && (
                  <button onClick={() => { setCreating("schema"); setShowCreate(false); }}>New schema</button>
                )}
                <button onClick={() => { setCreating("table"); setShowCreate(false); }}>New table</button>
              </div>
            )}
          </div>
        )}
        <button className="btn small" onClick={() => setShowTree((v) => !v)} aria-pressed={showTree}>
          {showTree ? "Hide schema" : "Show schema"}
        </button>
        <button className="btn small" onClick={() => connId && loadCatalog(connId)} disabled={!connId || catalogLoading}>
          Refresh schema
        </button>
      </div>
      {loadError && <Banner tone="error">{loadError}</Banner>}
      {notice && <Banner tone="ok">{notice}</Banner>}
      {creating && connection && (
        <CreateObject
          what={creating}
          connection={connection}
          catalog={catalog}
          canAddConnection={can("connections.manage")}
          onClose={() => setCreating(null)}
          onCreated={(message) => {
            setCreating(null);
            setNotice(message);
            void loadCatalog(connId);
            void reloadConnections();
          }}
        />
      )}

      <div className="workspace-body">
        {showTree && (
          <>
            <div className="pane tree-pane" style={{ width: treeWidth }}>
              {catalogLoading && <Loading />}
              {catalogError && <Banner tone="error">{catalogError}</Banner>}
              {catalog && !catalogLoading && (
                <SchemaTree
                  key={connId}
                  catalog={catalog}
                  onPick={(schema, table) => setSql(`SELECT * FROM ${quoteIdent(schema, connection?.kind)}.${quoteIdent(table, connection?.kind)} LIMIT 100`)}
                />
              )}
            </div>
            <div className="splitter x" role="separator" aria-orientation="vertical" title="Drag to resize"
              onPointerDown={(e) => beginDrag(e, "x", treeWidth, setTreeWidth)} />
          </>
        )}

        <div className="pane main-pane">
          {!maximized && (
            <>
              <div className="editor-pane" style={{ height: editorHeight }}>
                <SqlEditor value={sql} onChange={setSql} onRun={run} kind={connection?.kind} schema={completions} selection={selection} />
              </div>
              <div className="splitter y" role="separator" aria-orientation="horizontal" title="Drag to resize"
                onPointerDown={(e) => beginDrag(e, "y", editorHeight, setEditorHeight)} />
            </>
          )}

          <div className="run-bar">
            <button className="btn primary" onClick={() => run()} disabled={running || !connId || !sql.trim()}>
              {running ? "Running…" : "Run"}
            </button>
            <span className="hint">Ctrl/Cmd + Enter · runs the selection when text is selected</span>
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
                      onClick={() => { clearHistory(connId); setHistory([]); }}>
                      Clear
                    </button>
                  </div>
                  <div className="history">
                    {history.length === 0 && <div className="empty">Nothing run yet.</div>}
                    {history.map((h) => (
                      <button key={h.at + h.sql} onClick={() => { setSql(h.sql); setShowHistory(false); }} title={h.sql}>
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
            {error && (
              <Banner tone="error">
                <pre>{error}</pre>
              </Banner>
            )}
            {!response && !error && (
              <div className="empty">
                {connId ? "Run a statement to see its result here. Click a table on the left to start from a SELECT." : "Add a connection to start querying."}
              </div>
            )}
            {response && (
              <>
                {response.results.length > 1 && (
                  <div className="tabs" role="tablist">
                    {response.results.map((r, i) => (
                      <button key={i} role="tab" aria-selected={i === tab} className={`tab ${i === tab ? "active" : ""}`} onClick={() => setTab(i)}>
                        Result {i + 1}{r.command ? ` · ${r.command}` : ""}
                      </button>
                    ))}
                  </div>
                )}
                {current ? <ResultGrid result={current} /> : <div className="empty">Statement executed; no result returned.</div>}
                <div className="result-meta">
                  {current && current.columns.length > 0 && (
                    <span>
                      {formatNumber(current.rows.length)} row{current.rows.length === 1 ? "" : "s"} · {formatNumber(current.columns.length)} column{current.columns.length === 1 ? "" : "s"}
                    </span>
                  )}
                  {current?.command && <span className="mono">{current.command}</span>}
                  <span>{formatNumber(response.duration_ms)} ms</span>
                  <span>{response.read_only ? "read-only transaction" : "read-write session"}</span>
                  {current?.truncated && (
                    <Badge tone="warn">truncated at {formatNumber(current.rows.length)} rows; raise the limit or add LIMIT</Badge>
                  )}
                  <span className="spacer" />
                  {current && current.columns.length > 0 && <span className="hint">Click a cell to see its full value</span>}
                  {current && current.columns.length > 0 && (
                    <button className="btn small" onClick={exportCSV}>Export CSV</button>
                  )}
                </div>
              </>
            )}
          </div>
        </div>
      </div>
    </div>
  );
}
