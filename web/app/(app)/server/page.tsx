"use client";

import { useEffect, useState } from "react";
import { api } from "@/lib/api";
import { formatBytes, formatDuration } from "@/lib/format";
import { useLoad } from "@/lib/hooks";
import type { Backend } from "@/lib/types";
import { Badge, Banner, ConfirmDialog, Loading, PageHeader, StatusBadge } from "@/components/ui";

// Inactive slots holding more WAL than this are flagged: nothing is
// consuming them, and the server keeps the WAL until they are dropped.
const SLOT_WARN_BYTES = 256 * 1024 * 1024;

export default function ServerPage() {
  const connections = useLoad(() => api.connections(), []);
  const [connId, setConnId] = useState("");
  const [auto, setAuto] = useState(true);
  const [target, setTarget] = useState<{ backend: Backend; terminate: boolean } | null>(null);

  useEffect(() => {
    if (!connId && connections.data?.[0]) setConnId(connections.data[0].id);
  }, [connections.data, connId]);

  const { data, error, loading, reload } = useLoad(
    async () => (connId ? api.overview(connId) : null),
    [connId],
    5000,
    () => auto && !!connId,
  );

  return (
    <div className="content">
      <PageHeader title="Server activity" subtitle="Live sessions, replication slots, database sizes and key settings of a server." />
      {connections.error && <Banner tone="error">{connections.error}</Banner>}

      <div className="toolbar">
        <select className="select" value={connId} onChange={(e) => setConnId(e.target.value)} aria-label="Connection">
          {(!connections.data || connections.data.length === 0) && <option value="">No connections</option>}
          {connections.data?.map((c) => <option key={c.id} value={c.id}>{c.name} ({c.host})</option>)}
        </select>
        <button className="btn" onClick={() => reload()} disabled={!connId}>Refresh</button>
        <label style={{ display: "flex", alignItems: "center", gap: 6 }}>
          <input type="checkbox" checked={auto} onChange={(e) => setAuto(e.target.checked)} />
          Refresh every 5 seconds
        </label>
      </div>

      {error && <Banner tone="error">{error}</Banner>}
      {loading && !data && connId && <Loading />}

      {data && (
        <>
          <div className="card">
            <div className="card-head"><h2>Activity</h2><span className="muted">{data.activity.length} sessions</span></div>
            <div className="card-body flush table-wrap">
              {data.activity.length === 0 ? <div className="empty">No other sessions.</div> : (
                <table className="data">
                  <thead>
                    <tr><th className="num">PID</th><th>User</th><th>Database</th><th>Application</th><th>Client</th><th>State</th><th>Waiting on</th><th className="num">For</th><th>Statement</th><th /></tr>
                  </thead>
                  <tbody>
                    {data.activity.map((b) => (
                      <tr key={b.pid}>
                        <td className="num">{b.pid}</td>
                        <td>{b.user}</td>
                        <td>{b.database}</td>
                        <td>{b.application}</td>
                        <td className="mono">{b.client}</td>
                        <td><StatusBadge status={b.state} /></td>
                        <td>{b.wait_event || <span className="faint">—</span>}</td>
                        <td className="num nowrap">{formatDuration(b.seconds)}</td>
                        <td><div className="mono truncate" title={b.query}>{b.query}</div></td>
                        <td>
                          <div className="row-actions">
                            <button className="btn small" disabled={b.state !== "active"} onClick={() => setTarget({ backend: b, terminate: false })}>Cancel</button>
                            <button className="btn small" onClick={() => setTarget({ backend: b, terminate: true })}>Terminate</button>
                          </div>
                        </td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              )}
            </div>
          </div>

          <div className="card">
            <div className="card-head"><h2>Replication slots</h2></div>
            <div className="card-body flush table-wrap">
              {data.replication_slots.length === 0 ? <div className="empty">No replication slots.</div> : (
                <table className="data">
                  <thead>
                    <tr><th>Slot</th><th>Plugin</th><th>Database</th><th>State</th><th className="num">Retained WAL</th></tr>
                  </thead>
                  <tbody>
                    {data.replication_slots.map((s) => {
                      const stale = !s.active && s.retained_bytes >= SLOT_WARN_BYTES;
                      return (
                        <tr key={s.name} style={stale ? { background: "var(--warn-soft)" } : undefined}>
                          <td className="mono">{s.name}</td>
                          <td>{s.plugin || "physical"}</td>
                          <td>{s.database || <span className="faint">—</span>}</td>
                          <td>
                            <Badge tone={s.active ? "ok" : "warn"}>{s.active ? "active" : "inactive"}</Badge>
                            {stale && <span className="hint"> nothing is reading this slot; the server keeps its WAL until the slot is dropped</span>}
                          </td>
                          <td className="num">{formatBytes(s.retained_bytes)}</td>
                        </tr>
                      );
                    })}
                  </tbody>
                </table>
              )}
            </div>
          </div>

          <div className="grid cols-2">
            <div className="card">
              <div className="card-head"><h2>Databases</h2></div>
              <div className="card-body flush table-wrap">
                <table className="data">
                  <thead><tr><th>Name</th><th className="num">Size</th></tr></thead>
                  <tbody>
                    {data.databases.map((d) => (
                      <tr key={d.name}><td>{d.name}</td><td className="num">{d.size_bytes < 0 ? "no access" : formatBytes(d.size_bytes)}</td></tr>
                    ))}
                  </tbody>
                </table>
              </div>
            </div>
            <div className="card">
              <div className="card-head"><h2>Settings</h2></div>
              <div className="card-body flush table-wrap">
                <table className="data">
                  <thead><tr><th>Setting</th><th>Value</th></tr></thead>
                  <tbody>
                    {Object.entries(data.settings).sort(([a], [b]) => a.localeCompare(b)).map(([k, v]) => (
                      <tr key={k}><td className="mono">{k}</td><td>{v}</td></tr>
                    ))}
                  </tbody>
                </table>
              </div>
            </div>
          </div>
        </>
      )}

      {target && (
        <ConfirmDialog
          title={target.terminate ? `Terminate session ${target.backend.pid}?` : `Cancel the statement of session ${target.backend.pid}?`}
          confirmLabel={target.terminate ? "Terminate session" : "Cancel statement"}
          danger
          onClose={() => setTarget(null)}
          onConfirm={async () => {
            await api.cancelBackend(connId, target.backend.pid, target.terminate);
            await reload();
          }}
        >
          <p>
            {target.terminate
              ? "The session is disconnected and its open transaction is rolled back. The client will see a lost connection."
              : "The running statement is interrupted and its transaction is aborted. The session itself stays connected."}
          </p>
          <p className="muted">
            {target.backend.user}@{target.backend.database} from {target.backend.client}
            {target.backend.application && ` (${target.backend.application})`}
          </p>
          {target.backend.query && <pre className="banner">{target.backend.query}</pre>}
        </ConfirmDialog>
      )}
    </div>
  );
}
