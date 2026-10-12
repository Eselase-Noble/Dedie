"use client";

import { useEffect, useState } from "react";
import { api, errorMessage } from "@/lib/api";
import { formatTime } from "@/lib/format";
import { useLoad } from "@/lib/hooks";
import type { Connection, ConnectionStatus, ServerInfo } from "@/lib/types";
import ConnectionAccessDialog from "@/components/ConnectionAccessDialog";
import ConnectionForm, { KIND_LABELS, ServerInfoSummary } from "@/components/ConnectionForm";
import { ConnectionName } from "@/components/Environment";
import { PlugIcon } from "@/components/icons";
import { useSession } from "@/components/session";
import { Badge, Banner, ConfirmDialog, EmptyState, Loading, PageHeader } from "@/components/ui";

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
    <div title={status.error}>
      <span className="status"><span className="dot offline" />Offline</span>
      <span className="sub status-error">{status.error}</span>
    </div>
  );
}

export default function ConnectionsPage() {
  const { session, can } = useSession();
  const manage = can("connections.manage");
  const manageAccess = can("users.manage");
  const [sharing, setSharing] = useState<Connection | null>(null);
  const { data, error, loading, reload } = useLoad(() => api.connections(), []);
  // Each server is contacted to see whether it answers; refreshed every 30 seconds.
  const { data: statuses, loading: checking, reload: recheck } = useLoad(() => api.connectionStatuses(), [data?.length ?? 0], 30000);
  // undefined: form closed; null: creating; a connection: editing it.
  const [editing, setEditing] = useState<Connection | null | undefined>(undefined);
  const [deleting, setDeleting] = useState<Connection | null>(null);
  // "?new=1" opens the form for a new connection, for the desktop
  // application's File menu.
  useEffect(() => {
    if (new URLSearchParams(window.location.search).get("new") === null) return;
    setEditing(null);
    window.history.replaceState(null, "", window.location.pathname);
  }, []);
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
            <EmptyState icon={<PlugIcon size={20} />} title="No connections yet"
              action={manage && <button className="btn primary" onClick={() => setEditing(null)}>New connection</button>}>
              A connection is a saved database: where it is and the account Dedie uses. Everything else starts from
              one{manage ? "." : "; ask an operator or an administrator to add it."}
            </EmptyState>
          ) : (
            <table className="data stack">
              <thead>
                <tr><th>Name</th><th>Status</th><th>Server</th><th>Database</th><th>SSL</th><th><span className="sr-only">Actions</span></th></tr>
              </thead>
              <tbody>
                {data.map((c) => (
                  <tr key={c.id}>
                    <td title={`Added ${formatTime(c.created_at)} by ${c.created_by}`}>
                      <ConnectionName connection={c} strong />{" "}
                      {c.read_only && <Badge>read-only</Badge>}{" "}
                      {c.restricted && <Badge tone="warn">restricted</Badge>}
                    </td>
                    <td><StatusCell status={statuses?.[c.id]} /></td>
                    <td>
                      <div>
                        {KIND_LABELS[c.kind] ?? c.kind}
                        <span className="sub mono">{c.host}:{c.port}</span>
                      </div>
                    </td>
                    <td>
                      <div>
                        {c.database}
                        <span className="sub">{c.username ? `as ${c.username}` : "no account"}</span>
                      </div>
                    </td>
                    <td><Badge tone={WEAK_SSL.includes(c.sslmode) ? "warn" : "ok"}>{c.sslmode}</Badge></td>
                    <td>
                      <div className="row-actions">
                        <button className="btn small" onClick={() => test(c)} disabled={testing === c.id}>
                          {testing === c.id ? "Testing…" : "Test"}
                        </button>
                        {manageAccess && <button className="btn small" onClick={() => setSharing(c)}>Access</button>}
                        {manage && <button className="btn small" onClick={() => setEditing(c)}>Edit</button>}
                        {manage && <button className="btn small danger-quiet" onClick={() => setDeleting(c)}>Delete</button>}
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
      {sharing && <ConnectionAccessDialog connection={sharing} onClose={() => setSharing(null)} onSaved={reload} />}
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
