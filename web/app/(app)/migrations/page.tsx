"use client";

import Link from "next/link";
import { useRouter } from "next/navigation";
import { useState } from "react";
import { api } from "@/lib/api";
import { formatBytes, formatDuration, formatTime } from "@/lib/format";
import { useLoad } from "@/lib/hooks";
import MigrationForm from "@/components/MigrationForm";
import { useSession } from "@/components/session";
import { Badge, Banner, Loading, PageHeader, StatusBadge } from "@/components/ui";

export default function MigrationsPage() {
  const { can } = useSession();
  const router = useRouter();
  const [creating, setCreating] = useState(false);
  const { data, error, loading } = useLoad(
    async () => ({
      jobs: await api.migrations(),
      connections: can("connections.view") ? await api.connections() : [],
    }),
    [],
    3000,
  );
  const nameOf = (id: string) => data?.connections.find((c) => c.id === id)?.name ?? id;

  return (
    <div className="content">
      <PageHeader title="Migrations" subtitle="Move a database to a newer version while it stays online: initial copy, live change streaming, then a short cutover.">
        {can("migrations.manage") && <button className="btn primary" onClick={() => setCreating(true)}>New migration</button>}
      </PageHeader>
      {error && <Banner tone="error">{error}</Banner>}

      <div className="card">
        <div className="card-body flush table-wrap">
          {loading ? <Loading /> : !data || data.jobs.length === 0 ? (
            <div className="empty">No migrations yet.</div>
          ) : (
            <table className="data">
              <thead>
                <tr><th>Name</th><th>Source</th><th>Target</th><th>Status</th><th>Phase</th><th className="num">Lag</th><th>Updated</th></tr>
              </thead>
              <tbody>
                {data.jobs.map((m) => (
                  <tr key={m.id}>
                    <td>
                      <Link href={`/migrations/detail/?id=${encodeURIComponent(m.id)}`}><strong>{m.name}</strong></Link>{" "}
                      {m.config.mode === "sync" && <Badge tone="info">sync</Badge>}
                    </td>
                    <td>{nameOf(m.source_id)}</td>
                    <td>{nameOf(m.target_id)}</td>
                    <td><StatusBadge status={m.status} /></td>
                    <td>{m.phase || <span className="faint">—</span>}</td>
                    <td className="num">{m.phase !== "streaming" && m.phase !== "cutover" ? "—"
                      : m.state.lag_seconds !== undefined ? formatDuration(m.state.lag_seconds) : formatBytes(m.state.lag_bytes ?? 0)}</td>
                    <td className="nowrap muted">{formatTime(m.updated_at)}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          )}
        </div>
      </div>

      {creating && data && (
        <MigrationForm
          connections={data.connections}
          onClose={() => setCreating(false)}
          onCreated={(job) => router.push(`/migrations/detail/?id=${encodeURIComponent(job.id)}`)}
        />
      )}
    </div>
  );
}
