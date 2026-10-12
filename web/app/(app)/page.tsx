"use client";

import Link from "next/link";
import { api } from "@/lib/api";
import { formatBytes, formatTime } from "@/lib/format";
import { useLoad } from "@/lib/hooks";
import { phaseSummary } from "@/lib/progress";
import type { Backup, Connection, Migration } from "@/lib/types";
import { useSession } from "@/components/session";
import { Banner, PageHeader, StatusBadge } from "@/components/ui";

interface Summary {
  connections: Connection[] | null;
  migrations: Migration[] | null;
  backups: Backup[] | null;
}

export default function DashboardPage() {
  const { session, can } = useSession();

  // Each list is fetched only when the role may see it.
  const { data, error } = useLoad<Summary>(async () => {
    const [connections, migrations, backups] = await Promise.all([
      can("connections.view") ? api.connections() : null,
      can("migrations.view") ? api.migrations() : null,
      can("backups.create") ? api.backups() : null,
    ]);
    return { connections, migrations, backups };
  }, [], 5000);

  const migrations = data?.migrations ?? [];
  const active = migrations.filter((m) => m.status === "running" || m.status === "queued");
  const attention = migrations.filter((m) => m.status === "failed" || m.status === "paused");
  const insecure = (data?.connections ?? []).filter((c) => ["disable", "allow", "prefer"].includes(c.sslmode));

  return (
    <div className="content">
      <PageHeader title="Overview" subtitle={`${session.organization} · signed in as ${session.user.name} (${session.user.role})`} />
      {error && <Banner tone="error">{error}</Banner>}

      <div className="grid cols-3">
        {data?.connections && (
          <div className="card stat">
            <div className="label">Connections</div>
            <div className="value">{data.connections.length}</div>
            <div className="sub">
              {insecure.length > 0
                ? `${insecure.length} without enforced TLS`
                : data.connections.length > 0 ? "All enforce TLS" : "None configured yet"}
            </div>
          </div>
        )}
        {data?.migrations && (
          <div className="card stat">
            <div className="label">Migrations</div>
            <div className="value">{migrations.length}</div>
            <div className="sub">{active.length} active · {attention.length} need attention</div>
          </div>
        )}
        {data?.backups && (
          <div className="card stat">
            <div className="label">Backups</div>
            <div className="value">{data.backups.length}</div>
            <div className="sub">
              {formatBytes(data.backups.reduce((sum, b) => sum + b.size_bytes, 0))} stored, encrypted
            </div>
          </div>
        )}
      </div>

      {data?.migrations && (
        <div className="card">
          <div className="card-head">
            <h2>Migrations</h2>
            <Link href="/migrations/">View all</Link>
          </div>
          <div className="card-body flush table-wrap">
            {migrations.length === 0 ? (
              <div className="empty">No migrations yet.</div>
            ) : (
              <table className="data stack">
                <thead>
                  <tr><th>Name</th><th>Status</th><th>Phase</th><th>Updated</th></tr>
                </thead>
                <tbody>
                  {migrations.slice(0, 8).map((m) => (
                    <tr key={m.id}>
                      <td><Link href={`/migrations/detail/?id=${encodeURIComponent(m.id)}`}>{m.name}</Link></td>
                      <td><StatusBadge status={m.status} /></td>
                      <td>{phaseSummary(m) || <span className="faint">—</span>}</td>
                      <td className="nowrap muted">{formatTime(m.updated_at)}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            )}
          </div>
        </div>
      )}

      {data?.backups && (
        <div className="card">
          <div className="card-head">
            <h2>Recent backups</h2>
            <Link href="/backups/">View all</Link>
          </div>
          <div className="card-body flush table-wrap">
            {data.backups.length === 0 ? (
              <div className="empty">No backups yet.</div>
            ) : (
              <table className="data stack">
                <thead>
                  <tr><th>Connection</th><th>Database</th><th>Status</th><th className="num">Size</th><th>Created</th></tr>
                </thead>
                <tbody>
                  {data.backups.slice(0, 5).map((b) => (
                    <tr key={b.id}>
                      <td>{b.connection_name}</td>
                      <td>{b.database}</td>
                      <td><StatusBadge status={b.status} /></td>
                      <td className="num">{b.status === "completed" ? formatBytes(b.size_bytes) : "—"}</td>
                      <td className="nowrap muted">{formatTime(b.created_at)}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            )}
          </div>
        </div>
      )}
    </div>
  );
}
