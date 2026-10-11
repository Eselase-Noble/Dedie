"use client";

import { useCallback, useEffect, useState } from "react";
import { api, errorMessage } from "@/lib/api";
import { download, toCSV } from "@/lib/csv";
import { formatNumber, formatTime, quoteIdent } from "@/lib/format";
import { clearHistory, loadHistory, pushHistory, type HistoryItem } from "@/lib/history";
import { useLoad } from "@/lib/hooks";
import type { Catalog, QueryResponse } from "@/lib/types";
import ResultGrid from "@/components/ResultGrid";
import SchemaTree from "@/components/SchemaTree";
import { useSession } from "@/components/session";
import SqlEditor from "@/components/SqlEditor";
import { Badge, Banner, Loading, PageHeader } from "@/components/ui";

const ROW_LIMITS = [100, 500, 1000, 5000, 10000];
const LAST_CONNECTION = "dedie.query.connection";

export default function QueryPage() {
  const { can } = useSession();
  const { data: connections, error: loadError } = useLoad(() => api.connections(), []);
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

  const run = async () => {
    if (!connId || !sql.trim() || running) return;
    setRunning(true);
    setError("");
    try {
      const res = await api.query(connId, { sql, write: write && mayWrite, max_rows: maxRows });
      setResponse(res);
      setTab(0);
    } catch (err) {
      setResponse(null);
      setError(errorMessage(err));
    } finally {
      setHistory(pushHistory(connId, sql));
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
    <div className="content wide">
      <PageHeader title="Query browser" subtitle="Statements run read-only unless writes are explicitly allowed. Every execution is recorded in the audit log." />
      {loadError && <Banner tone="error">{loadError}</Banner>}

      <div className="toolbar">
        <select className="select" value={connId} onChange={(e) => setConnId(e.target.value)} aria-label="Connection">
          {(!connections || connections.length === 0) && <option value="">No connections</option>}
          {connections?.map((c) => <option key={c.id} value={c.id}>{c.name} ({c.database})</option>)}
        </select>
        {connection?.read_only && <Badge>read-only connection</Badge>}
        <span className="spacer" />
        <button className="btn small" onClick={() => connId && loadCatalog(connId)} disabled={!connId || catalogLoading}>
          Refresh schema
        </button>
      </div>

      <div className="query-layout">
        <div>
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

        <div style={{ minWidth: 0 }}>
          <SqlEditor value={sql} onChange={setSql} onRun={run} />
          <div className="toolbar" style={{ marginTop: 10 }}>
            <button className="btn primary" onClick={run} disabled={running || !connId || !sql.trim()}>
              {running ? "Running…" : "Run"}
            </button>
            <span className="hint">Ctrl/Cmd + Enter</span>
            <label className="hint" style={{ display: "flex", alignItems: "center", gap: 6 }}>
              Max rows
              <select className="select" style={{ minWidth: 90 }} value={maxRows} onChange={(e) => setMaxRows(Number(e.target.value))}>
                {ROW_LIMITS.map((n) => <option key={n} value={n}>{formatNumber(n)}</option>)}
              </select>
            </label>
            {mayWrite && (
              <label style={{ display: "flex", alignItems: "center", gap: 6 }}>
                <input type="checkbox" checked={write} onChange={(e) => setWrite(e.target.checked)} />
                Allow writes
              </label>
            )}
            <span className="spacer" />
            <button className="btn small" onClick={() => setShowHistory((v) => !v)}>
              {showHistory ? "Hide history" : `History (${history.length})`}
            </button>
          </div>

          {write && mayWrite && (
            <Banner tone="warn">
              Writes are allowed: statements run in a read-write session and several statements may be sent at once.
              Changes are committed immediately.
            </Banner>
          )}

          {showHistory && (
            <div className="card">
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

          {error && (
            <Banner tone="error">
              <pre>{error}</pre>
            </Banner>
          )}

          {response && (
            <div className="card">
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
                {current && current.columns.length > 0 && <span>{formatNumber(current.rows.length)} row{current.rows.length === 1 ? "" : "s"}</span>}
                {current?.command && <span className="mono">{current.command}</span>}
                <span>{formatNumber(response.duration_ms)} ms</span>
                <span>{response.read_only ? "read-only transaction" : "read-write session"}</span>
                {current?.truncated && (
                  <Badge tone="warn">truncated at {formatNumber(current.rows.length)} rows; raise the limit or add LIMIT</Badge>
                )}
                <span className="spacer" />
                {current && current.columns.length > 0 && (
                  <button className="btn small" onClick={exportCSV}>Export CSV</button>
                )}
              </div>
            </div>
          )}
        </div>
      </div>
    </div>
  );
}
