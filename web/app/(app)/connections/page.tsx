"use client";

import { useState } from "react";
import { api, errorMessage } from "@/lib/api";
import { formatTime } from "@/lib/format";
import { useLoad } from "@/lib/hooks";
import type { Connection, ConnectionStatus, ServerInfo } from "@/lib/types";
import ConnectionForm, { ServerInfoSummary } from "@/components/ConnectionForm";
import { useSession } from "@/components/session";
import { Badge, Banner, ConfirmDialog, Loading, PageHeader } from "@/components/ui";

const WEAK_SSL = ["disable", "allow", "prefer"];

/** Shows whether a connection's server answered the last check. */
function StatusCell({ status }: { status?: ConnectionStatus }) {
  if (!status) return <span className="status"><span className="dot pending" />Checking…</span>;
  if (status.online) {
    return (
      <span className="status" title={`${status.version ?? ""} · checked ${formatTime(status.checked_at)}`}>
        <span className="dot online" />Online <span className="faint">· {status.latency_ms} ms</span>
      </span>
    );
  }
  return (
    <span className="status" title={status.error}>
      <span className="dot offline" />Offline
      <span className="status-error">{status.error}</span>
    </span>
  );
}

export default function ConnectionsPage() {
  const { session, can } = useSession();
  const manage = can("connections.manage");
  const { data, error, loading, reload } = useLoad(() => api.connections(), []);
  // Each server is contacted to see whether it answers; refreshed every 30 seconds.
  const { data: statuses, loading: checking, reload: recheck } = useLoad(() => api.connectionStatuses(), [data?.length ?? 0], 30000);
  // undefined: form closed; null: creating; a connection: editing it.
  const [editing, setEditing] = useState<Connection | null | undefined>(undefined);
  const [deleting, setDeleting] = useState<Connection | null>(null);
  const [testing, setTesting] = useState("");
  const [result, setResult] = useState<{ name: string; info?: ServerInfo; error?: string } | null>(null);

  const test = async (c: Connection) => {
    setTesting(c.id);
    setResult(null);
    try {
      setResult({ name: c.name, info: await api.testConnection(c.id) });
    } catch (err) {
      setResult({ name: c.name, error: errorMessage(err) });
    } finally {
      setTesting("");
    }
  };

  return (
    <div className="content">
      <PageHeader title="Connections" subtitle="Saved databases. Passwords are encrypted at rest and never returned by the API.">
        <button className="btn" onClick={() => void recheck()} disabled={checking}>{checking ? "Checking…" : "Check status"}</button>
        {manage && <button className="btn primary" onClick={() => setEditing(null)}>New connection</button>}
      </PageHeader>
      {error && <Banner tone="error">{error}</Banner>}
      {result?.error && <Banner tone="error">{result.name}: {result.error}</Banner>}
      {result?.info && <ServerInfoSummary info={result.info} />}

      <div className="card">
        <div className="card-body flush table-wrap">
          {loading ? <Loading /> : !data || data.length === 0 ? (
            <div className="empty">No connections yet.{manage && " Add one to start browsing or migrating."}</div>
          ) : (
            <table className="data">
              <thead>
                <tr><th>Name</th><th>Status</th><th>Type</th><th>Server</th><th>Database</th><th>User</th><th>SSL</th><th>Created</th><th /></tr>
              </thead>
              <tbody>
                {data.map((c) => (
                  <tr key={c.id}>
                    <td>
                      <strong>{c.name}</strong>{" "}
                      {c.read_only && <Badge>read-only</Badge>}
                    </td>
                    <td className="nowrap"><StatusCell status={statuses?.[c.id]} /></td>
                    <td>{c.kind}</td>
                    <td className="mono">{c.host}:{c.port}</td>
                    <td>{c.database}</td>
                    <td>{c.username}</td>
                    <td><Badge tone={WEAK_SSL.includes(c.sslmode) ? "warn" : "ok"}>{c.sslmode}</Badge></td>
                    <td className="nowrap muted" title={c.created_by}>{formatTime(c.created_at)}</td>
                    <td>
                      <div className="row-actions">
                        <button className="btn small" onClick={() => test(c)} disabled={testing === c.id}>
                          {testing === c.id ? "Testing…" : "Test"}
                        </button>
                        {manage && <button className="btn small" onClick={() => setEditing(c)}>Edit</button>}
                        {manage && <button className="btn small" onClick={() => setDeleting(c)}>Delete</button>}
                      </div>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          )}
        </div>
      </div>

      {editing !== undefined && (
        <ConnectionForm existing={editing} kinds={session.kinds} onClose={() => setEditing(undefined)} onSaved={reload} />
      )}
      {deleting && (
        <ConfirmDialog
          title={`Delete ${deleting.name}?`}
          confirmLabel="Delete connection"
          danger
          onClose={() => setDeleting(null)}
          onConfirm={async () => {
            await api.deleteConnection(deleting.id);
            await reload();
          }}
        >
          <p>
            The saved settings and encrypted password are removed. The database itself is not touched. A connection
            used by an active migration cannot be deleted.
          </p>
        </ConfirmDialog>
      )}
    </div>
  );
}
