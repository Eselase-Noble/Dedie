"use client";

import Link from "next/link";
import { useRouter } from "next/navigation";
import { useEffect, useState } from "react";
import { api } from "@/lib/api";
import { formatBytes, formatDuration, formatNumber, formatTime } from "@/lib/format";
import { useLoad } from "@/lib/hooks";
import { phaseSummary } from "@/lib/progress";
import MigrationForm from "@/components/MigrationForm";
import MigrationPaths from "@/components/migrations/MigrationPaths";
import { useSession } from "@/components/session";
import { TransferIcon } from "@/components/icons";
import { Badge, Banner, EmptyState, Loading, PageHeader, StatusBadge } from "@/components/ui";

export default function MigrationsPage() {
  const { can } = useSession();
  const router = useRouter();
  const [creating, setCreating] = useState(false);
  const [mode, setMode] = useState<"migrate" | "sync" | "offline">("migrate");
  const [showPaths, setShowPaths] = useState(false);
  // "?new=offline" opens the form straight away; the desktop application's
  // File menu uses it, and it makes a useful bookmark.
  useEffect(() => {
    const wanted = new URLSearchParams(window.location.search).get("new");
    if (wanted === null) return;
    setMode(wanted === "offline" || wanted === "sync" ? wanted : "migrate");
    setCreating(true);
    window.history.replaceState(null, "", window.location.pathname);
  }, []);
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
      <PageHeader title="Migrations" subtitle="Move a database to a newer version or another engine: online with a short cutover, as a one-time offline copy, or as a continuous sync.">
        <button className="btn" onClick={() => setShowPaths((v) => !v)} aria-expanded={showPaths}>{showPaths ? "Hide supported paths" : "Supported paths"}</button>
        {can("migrations.manage") && <button className="btn primary" onClick={() => { setMode("migrate"); setCreating(true); }}>New migration</button>}
      </PageHeader>
      {showPaths && <MigrationPaths />}
      {error && <Banner tone="error">{error}</Banner>}

      <div className="card">
        <div className="card-body flush table-wrap">
          {loading ? <Loading /> : !data || data.jobs.length === 0 ? (
            <EmptyState icon={<TransferIcon size={20} />} title="No migrations yet"
              action={can("migrations.manage") && (
                <>
                  <button className="btn primary" onClick={() => { setMode("migrate"); setCreating(true); }}>New migration</button>
                  <button className="btn" onClick={() => { setMode("offline"); setCreating(true); }}>Offline copy</button>
                </>
              )}>
              A migration copies a database to another server or another engine and checks the result. Creating one
              changes nothing; the preflight check tells you what would happen first.
            </EmptyState>
          ) : (
            <table className="data stack">
              <thead>
                <tr><th>Name</th><th>Source</th><th>Target</th><th>Status</th><th>Phase</th><th className="num">Lag</th><th>Updated</th></tr>
              </thead>
              <tbody>
                {data.jobs.map((m) => (
                  <tr key={m.id}>
                    <td>
                      <Link href={`/migrations/detail/?id=${encodeURIComponent(m.id)}`}><strong>{m.name}</strong></Link>{" "}
                      {m.config.mode === "sync" && <Badge tone="info">sync</Badge>}
                      {m.config.mode === "offline" && <Badge>offline</Badge>}
                    </td>
                    <td>{nameOf(m.source_id)}</td>
                    <td>{nameOf(m.target_id)}</td>
                    <td><StatusBadge status={m.status} /></td>
                    <td title={m.copy ? `${formatNumber(m.copy.rows_copied)} of about ${formatNumber(m.copy.rows_expected)} rows copied` : undefined}>
                      {phaseSummary(m) || <span className="faint">—</span>}
                    </td>
                    <td className="num">{m.config.mode === "offline" || (m.phase !== "streaming" && m.phase !== "cutover") ? "—"
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
          initialMode={mode}
          onClose={() => setCreating(false)}
          onCreated={(job) => router.push(`/migrations/detail/?id=${encodeURIComponent(job.id)}`)}
        />
      )}
    </div>
  );
}
