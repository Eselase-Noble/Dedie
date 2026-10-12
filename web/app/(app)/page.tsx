"use client";

import Link from "next/link";
import { api } from "@/lib/api";
import { formatAgo, formatBytes, formatTime, isZeroTime } from "@/lib/format";
import { useLoad } from "@/lib/hooks";
import { copyPercent, phaseSummary } from "@/lib/progress";
import type { Backup, ConnectionSummary, Migration, Summary } from "@/lib/types";
import { ConnectionName } from "@/components/Environment";
import { ArchiveIcon, PlugIcon, TransferIcon } from "@/components/icons";
import FirstSteps from "@/components/overview/FirstSteps";
import NeedsAttention from "@/components/overview/NeedsAttention";
import { useSession } from "@/components/session";
import { Banner, EmptyState, Loading, PageHeader, Progress, StatusBadge } from "@/components/ui";

interface Overview {
  summary: Summary;
  migrations: Migration[] | null;
  backups: Backup[] | null;
}

// A backup older than this is pointed out on a connection that has no
// schedule to take the next one.
const STALE_DAYS = 7;

/** Whether a connection answers, in the words of the connections page. */
function Health({ c }: { c: ConnectionSummary }) {
  if (!c.checked) return <span className="status"><span className="dot pending" />Not checked yet</span>;
  if (c.online) return <span className="status"><span className="dot online" />Online <span className="faint">· {c.latency_ms} ms</span></span>;
  return <span className="status"><span className="dot offline" />Not answering</span>;
}

/** When a connection was last backed up, and whether a schedule will do it again. */
function Freshness({ c }: { c: ConnectionSummary }) {
  if (!c.backups_apply) return <span className="fresh faint">not backed up by Dedie</span>;
  if (isZeroTime(c.last_backup_at)) {
    return <span className="fresh never">never backed up{c.scheduled && <span className="faint"> · scheduled</span>}</span>;
  }
  const days = (Date.now() - new Date(c.last_backup_at).getTime()) / 86_400_000;
  return (
    <span className={`fresh ${days > STALE_DAYS && !c.scheduled ? "stale" : ""}`} title={formatTime(c.last_backup_at)}>
      backed up {formatAgo(c.last_backup_at)}
      <span className="faint"> · {c.scheduled ? "scheduled" : "no schedule"}</span>
    </span>
  );
}

export default function OverviewPage() {
  const { session, can } = useSession();
  const seesBackups = can("backups.create");

  // Each list is fetched only when the role may see it.
  const { data, error, loading } = useLoad<Overview>(async () => {
    const [summary, migrations, backups] = await Promise.all([
      api.summary(),
      can("migrations.view") ? api.migrations() : null,
      seesBackups ? api.backups() : null,
    ]);
    return { summary, migrations, backups };
  }, [], 5000);

  const connections = data?.summary.connections ?? [];
  const migrations = data?.migrations ?? [];
  const active = migrations.filter((m) => m.status === "running" || m.status === "queued");
  const waiting = migrations.filter((m) => m.status === "failed" || m.status === "paused");
  const checked = connections.filter((c) => c.checked);
  const down = checked.filter((c) => !c.online);
  const completed = (data?.backups ?? []).filter((b) => b.status === "completed");
  const latest = completed[0];
  // Running work first, then whatever changed most recently.
  const shown = [...active, ...migrations.filter((m) => !active.includes(m))].slice(0, 6);

  return (
    <div className="content">
      <PageHeader title="Overview" subtitle={`${session.organization} · signed in as ${session.user.name}, ${session.user.role}`} />
      {error && <Banner tone="error">{error}</Banner>}
      {loading && !data && <Loading lines={6} />}

      {data && (
        <>
          {data.summary.setup && <FirstSteps setup={data.summary.setup} />}

          <div className="grid cols-3">
            {can("connections.view") && (
              <Link className="card stat" href="/connections/">
                <span className="label"><PlugIcon />Connections</span>
                <span className="value">{connections.length}</span>
                <span className={`sub ${down.length > 0 ? "danger" : ""}`}>
                  {connections.length === 0 ? "None saved yet"
                    : down.length > 0 ? `${down.length} not answering`
                      : checked.length === 0 ? "Waiting for the first check"
                        : checked.length === connections.length ? "All answering" : `${checked.length} answering, ${connections.length - checked.length} not checked yet`}
                </span>
              </Link>
            )}
            {data.migrations && (
              <Link className="card stat" href="/migrations/">
                <span className="label"><TransferIcon />Migrations</span>
                <span className="value">{active.length}<span className="unit"> running</span></span>
                <span className={`sub ${waiting.length > 0 ? "warn" : ""}`}>
                  {waiting.length > 0 ? `${waiting.length} failed or paused · ` : ""}{migrations.length} in all
                </span>
              </Link>
            )}
            {data.backups && (
              <Link className="card stat" href="/backups/">
                <span className="label"><ArchiveIcon />Backups</span>
                <span className="value">{completed.length}</span>
                <span className="sub">
                  {latest ? `${formatBytes(completed.reduce((sum, b) => sum + b.size_bytes, 0))} kept · latest ${formatAgo(latest.created_at)}` : "None taken yet"}
                </span>
              </Link>
            )}
          </div>

          <NeedsAttention items={data.summary.attention} />

          <div className="grid cols-2 overview-split">
            {can("connections.view") && (
              <section className="card" aria-labelledby="overview-connections">
                <div className="card-head">
                  <h2 id="overview-connections">Connections</h2>
                  <Link href="/connections/">View all</Link>
                </div>
                {connections.length === 0 ? (
                  <EmptyState icon={<PlugIcon size={20} />} title="No connections yet"
                    action={can("connections.manage") && <Link className="btn primary" href="/connections/?new=1">New connection</Link>}>
                    Save a database to browse it, back it up or migrate it.
                  </EmptyState>
                ) : (
                  <ul className={`health ${seesBackups ? "" : "pair"}`}>
                    {connections.map((c) => (
                      <li key={c.id}>
                        <span>
                          <ConnectionName connection={c} strong />
                          <span className="sub">{c.kind}</span>
                        </span>
                        <Health c={c} />
                        {seesBackups && <Freshness c={c} />}
                      </li>
                    ))}
                  </ul>
                )}
              </section>
            )}

            {data.migrations && (
              <section className="card" aria-labelledby="overview-migrations">
                <div className="card-head">
                  <h2 id="overview-migrations">Migrations</h2>
                  <Link href="/migrations/">View all</Link>
                </div>
                {migrations.length === 0 ? (
                  <EmptyState icon={<TransferIcon size={20} />} title="No migrations yet"
                    action={can("migrations.manage") && connections.length > 1 && <Link className="btn primary" href="/migrations/?new=1">New migration</Link>}>
                    Move a database to a newer version or another engine, online or as a one-time copy.
                  </EmptyState>
                ) : (
                  <ul className="health pair">
                    {shown.map((m) => {
                      const percent = m.copy ? copyPercent(m.copy) : null;
                      return (
                        <li key={m.id}>
                          <span>
                            <Link href={`/migrations/detail/?id=${encodeURIComponent(m.id)}`}><strong>{m.name}</strong></Link>
                            <span className="sub">
                              {m.status === "running" || m.status === "queued" ? phaseSummary(m) || "starting" : `updated ${formatAgo(m.updated_at)}`}
                            </span>
                            {percent !== null && <Progress value={percent} total={100} />}
                          </span>
                          <StatusBadge status={m.status} />
                        </li>
                      );
                    })}
                  </ul>
                )}
              </section>
            )}
          </div>

          {data.backups && data.backups.length > 0 && (
            <section className="card" aria-labelledby="overview-backups">
              <div className="card-head">
                <h2 id="overview-backups">Recent backups</h2>
                <Link href="/backups/">View all</Link>
              </div>
              <div className="card-body flush table-wrap">
                <table className="data stack">
                  <thead>
                    <tr><th>Database</th><th>Connection</th><th>Status</th><th className="num">Size</th><th>Taken</th></tr>
                  </thead>
                  <tbody>
                    {data.backups.slice(0, 5).map((b) => (
                      <tr key={b.id}>
                        <td><strong>{b.database}</strong></td>
                        <td><ConnectionName connection={connections.find((c) => c.id === b.connection_id)} name={b.connection_name} /></td>
                        <td><StatusBadge status={b.status} /></td>
                        <td className="num">{b.status === "completed" ? formatBytes(b.size_bytes) : "—"}</td>
                        <td className="nowrap muted" title={formatTime(b.created_at)}>{formatAgo(b.created_at)}</td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              </div>
            </section>
          )}
        </>
      )}
    </div>
  );
}
