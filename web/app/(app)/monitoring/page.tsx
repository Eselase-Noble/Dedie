"use client";

import Link from "next/link";
import { api } from "@/lib/api";
import { formatBytes, formatNumber, formatTime } from "@/lib/format";
import { useLoad } from "@/lib/hooks";
import Channels, { EVENTS } from "@/components/monitoring/Channels";
import Uptime from "@/components/monitoring/Uptime";
import { useSession } from "@/components/session";
import { Badge, Banner, Loading, PageHeader, StatusBadge } from "@/components/ui";

const EVENT_TONES: Record<string, "ok" | "warn" | "danger" | "info"> = {
  "migration.failed": "danger", "backup.failed": "danger", "connection.down": "danger",
  "migration.lag": "warn", "migration.completed": "ok", "connection.up": "ok",
};

function eventLabel(type: string): string {
  return EVENTS.find((e) => e.type === type)?.label ?? type;
}

/** Health of every database, progress of running migrations, and alerts. */
export default function MonitoringPage() {
  const { can } = useSession();
  const { data, error, loading, reload } = useLoad(async () => {
    const [monitoring, channels] = await Promise.all([api.monitoring(), api.alertChannels()]);
    return { ...monitoring, channels };
  }, [], 30000);

  const down = data?.connections.filter((c) => c.last && !c.last.online) ?? [];

  return (
    <div className="content">
      <PageHeader title="Monitoring" subtitle="Every database is checked once a minute. The last 24 hours are shown.">
        <button className="btn" onClick={() => void reload()}>Refresh</button>
      </PageHeader>
      {error && <Banner tone="error">{error}</Banner>}
      {down.length > 0 && (
        <Banner tone="error">
          {down.length === 1 ? `${down[0].name} is not answering.` : `${down.length} databases are not answering: ${down.map((c) => c.name).join(", ")}.`}
        </Banner>
      )}

      <div className="card">
        <div className="card-head"><h2>Databases</h2></div>
        <div className="card-body flush table-wrap">
          {loading ? <Loading /> : !data || data.connections.length === 0 ? <div className="empty">No connections to watch.</div> : (
            <table className="data stack">
              <thead>
                <tr><th>Connection</th><th>Now</th><th className="num">Uptime (24 h)</th><th className="num">Avg response</th><th>Checks, oldest to newest</th></tr>
              </thead>
              <tbody>
                {data.connections.map((c) => (
                  <tr key={c.connection_id}>
                    <td><strong>{c.name}</strong> <span className="muted">· {c.kind}</span></td>
                    <td className="nowrap">
                      {!c.last ? <span className="status"><span className="dot pending" />Waiting for first check</span>
                        : c.last.online ? <span className="status"><span className="dot online" />Online</span>
                        : <span className="status" title={c.last.error}><span className="dot offline" />Offline</span>}
                    </td>
                    <td className="num">{c.checks > 0 ? `${c.uptime_percent.toFixed(c.uptime_percent === 100 ? 0 : 2)}%` : "—"}</td>
                    <td className="num">{c.checks > 0 && c.avg_latency_ms > 0 ? `${c.avg_latency_ms} ms` : "—"}</td>
                    <td style={{ minWidth: 260 }}><Uptime samples={c.samples} /></td>
                  </tr>
                ))}
              </tbody>
            </table>
          )}
        </div>
      </div>

      <div className="card">
        <div className="card-head"><h2>Migrations in progress</h2></div>
        <div className="card-body flush table-wrap">
          {!data || data.migrations.length === 0 ? <div className="empty">No migration is running, paused or failed.</div> : (
            <table className="data stack">
              <thead><tr><th>Migration</th><th>Status</th><th>Phase</th><th className="num">Waiting to apply</th><th className="num">Changes applied</th><th>Last change applied</th></tr></thead>
              <tbody>
                {data.migrations.map((m) => (
                  <tr key={m.id}>
                    <td><Link href={`/migrations/detail/?id=${encodeURIComponent(m.id)}`}><strong>{m.name}</strong></Link></td>
                    <td><StatusBadge status={m.status} /></td>
                    <td>{m.phase || "—"}</td>
                    <td className="num">{m.status === "running" ? formatBytes(m.lag_bytes) : "—"}</td>
                    <td className="num">{formatNumber(m.rows_applied)}</td>
                    <td className="nowrap muted">{formatTime(m.last_commit_time)}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          )}
        </div>
      </div>

      {data && <Channels channels={data.channels} mayManage={can("alerts.manage")} reload={reload} />}

      <div className="card">
        <div className="card-head"><h2>Recent alerts</h2></div>
        <div className="card-body flush table-wrap">
          {!data || data.events.length === 0 ? <div className="empty">Nothing has needed attention.</div> : (
            <table className="data stack">
              <thead><tr><th>When</th><th>Alert</th><th>Detail</th></tr></thead>
              <tbody>
                {data.events.map((e) => (
                  <tr key={e.id}>
                    <td className="nowrap muted">{formatTime(e.time)}</td>
                    <td className="nowrap"><Badge tone={EVENT_TONES[e.type] ?? ""}>{eventLabel(e.type)}</Badge></td>
                    <td><strong>{e.subject}</strong><div className="muted pre-line">{e.message}</div></td>
                  </tr>
                ))}
              </tbody>
            </table>
          )}
        </div>
      </div>

      <div className="card">
        <div className="card-head"><h2>Prometheus and health checks</h2></div>
        <div className="card-body">
          <p style={{ marginBottom: 8 }}>
            Scrape <code>/metrics</code> with an API token from your <Link href="/account/">Account</Link> page (role
            operator or higher) to chart these figures in Grafana and alert from Prometheus:
          </p>
          <pre className="mono" style={{ margin: 0 }}>{`scrape_configs:
  - job_name: dedie
    metrics_path: /metrics
    authorization:
      credentials: dedie_pat_…
    static_configs:
      - targets: ["${typeof window === "undefined" ? "dedie.example.com" : window.location.host}"]`}</pre>
          <p className="hint" style={{ marginTop: 8 }}>
            <code>/healthz</code> needs no sign-in and answers 200 while the service is working; point a load balancer
            or uptime check at it.
          </p>
        </div>
      </div>
    </div>
  );
}
