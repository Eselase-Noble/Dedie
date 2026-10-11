"use client";

import Link from "next/link";
import { useRouter, useSearchParams } from "next/navigation";
import { Suspense, useState, type ReactNode } from "react";
import { api, errorMessage } from "@/lib/api";
import { formatBytes, formatNumber, formatTime } from "@/lib/format";
import { useLoad } from "@/lib/hooks";
import type { MigrationAction, MigrationDetail, PreflightReport } from "@/lib/types";
import { useSession } from "@/components/session";
import { Banner, ConfirmDialog, Loading, PageHeader, Progress, StatusBadge } from "@/components/ui";

const PHASES = ["schema", "snapshot", "indexing", "streaming", "cutover", "done"];
const PHASE_LABELS: Record<string, string> = {
  schema: "Schema", snapshot: "Initial copy", indexing: "Indexes", streaming: "Streaming", cutover: "Cutover", done: "Done",
};

function Stepper({ job }: { job: MigrationDetail }) {
  const current = job.status === "completed" ? PHASES.length : PHASES.indexOf(job.phase);
  return (
    <div className="stepper">
      {PHASES.map((phase, i) => {
        let cls = "";
        if (i < current) cls = "done";
        else if (i === current) cls = job.status === "failed" ? "failed" : "current";
        return (
          <div key={phase} className={`step ${cls}`}>
            <div className="dot">{i < current ? "✓" : i + 1}</div>
            {PHASE_LABELS[phase]}
          </div>
        );
      })}
    </div>
  );
}

function Figure({ label, children }: { label: string; children: ReactNode }) {
  return (
    <div className="figure">
      <div className="k">{label}</div>
      <div className="v">{children}</div>
    </div>
  );
}

function Preflight({ report }: { report: PreflightReport }) {
  return (
    <table className="data">
      <tbody>
        {report.checks.map((c, i) => (
          <tr key={i}>
            <td style={{ width: 70 }}><StatusBadge status={c.status} /></td>
            <td style={{ width: 220 }}><strong>{c.name}</strong></td>
            <td className="pre-line">{c.detail}</td>
          </tr>
        ))}
      </tbody>
    </table>
  );
}

interface Confirm {
  action: MigrationAction;
  title: string;
  label: string;
  body: ReactNode;
}

function Detail({ id }: { id: string }) {
  const { can } = useSession();
  const router = useRouter();
  const manage = can("migrations.manage");
  // Poll only while something can change on its own.
  const { data: job, error, loading, reload } = useLoad(
    () => api.migration(id), [id], 2000,
    (d) => !d || d.status === "queued" || d.status === "running",
  );
  const connections = useLoad(async () => (can("connections.view") ? api.connections() : []), []);
  const [actionError, setActionError] = useState("");
  const [busy, setBusy] = useState("");
  const [confirm, setConfirm] = useState<Confirm | null>(null);
  const [preflighting, setPreflighting] = useState(false);

  if (loading && !job) return <div className="content"><Loading /></div>;
  if (!job) return <div className="content"><Banner tone="error">{error || "Migration not found."}</Banner></div>;

  const nameOf = (connId: string) => connections.data?.find((c) => c.id === connId)?.name ?? connId;
  const st = job.state;
  const status = job.status;

  // These mirror the rules the server enforces in MigrationAction.
  const canStart = status === "draft" || status === "paused" || status === "failed";
  const canPause = status === "running";
  const canCutover = status === "running" && job.phase === "streaming";
  const canAbort = status !== "completed" && status !== "aborted";
  const canDelete = status === "draft" || status === "completed" || status === "aborted";

  const act = async (action: MigrationAction) => {
    setBusy(action);
    setActionError("");
    try {
      await api.migrationAction(job.id, action);
      if (action === "delete") {
        router.push("/migrations/");
        return;
      }
      await reload();
    } catch (err) {
      setActionError(errorMessage(err));
    } finally {
      setBusy("");
    }
  };

  const runPreflight = async () => {
    setPreflighting(true);
    setActionError("");
    try {
      await api.preflight(job.id);
      await reload();
    } catch (err) {
      setActionError(errorMessage(err));
    } finally {
      setPreflighting(false);
    }
  };

  const fenced = !!job.config.fence_source;
  const confirmations: Record<"cutover" | "abort" | "delete", Confirm> = {
    cutover: {
      action: "cutover",
      title: "Start cutover?",
      label: "Start cutover",
      body: (
        <>
          <p>Cutover finishes the migration. In order, it will:</p>
          <ol style={{ margin: "0 0 10px", paddingLeft: 20 }}>
            {fenced
              ? <li>Make the source database read-only and disconnect its client sessions.</li>
              : <li>Assume writes to the source have already been stopped. <strong>Stop your application first</strong>; this migration is not configured to fence the source.</li>}
            <li>Wait for the remaining changes to be applied to the target.</li>
            <li>Synchronize sequences and validate every table ({job.config.validation || "checksum"}).</li>
            <li>Create triggers, refresh statistics and remove the replication slot.</li>
          </ol>
          <p>
            If validation finds a difference, or the stream does not drain in time, cutover is cancelled
            {fenced ? ", the source is made writable again" : ""} and streaming continues.
            {fenced && " After a successful cutover the source is left read-only."}
          </p>
          <p>Point your application at the target once the migration shows as completed.</p>
        </>
      ),
    },
    abort: {
      action: "abort",
      title: "Abort this migration?",
      label: "Abort migration",
      body: (
        <>
          <p>
            Replication stops and the replication slot and publication are removed from the source. The source
            database keeps running normally.
          </p>
          <p>
            Tables already copied to the target are left in place for you to inspect or drop. An aborted migration
            cannot be resumed; start a new one against an empty target instead.
          </p>
        </>
      ),
    },
    delete: {
      action: "delete",
      title: "Delete this migration?",
      label: "Delete",
      body: <p>The migration record, its progress and its event log are removed. Neither database is touched.</p>,
    },
  };

  const totalEst = job.tables.reduce((n, t) => n + t.est_rows, 0);
  const totalCopied = job.tables.reduce((n, t) => n + t.rows_copied, 0);
  const report = job.preflight ?? null;

  return (
    <div className="content">
      <p style={{ marginBottom: 10 }}><Link href="/migrations/">← Migrations</Link></p>
      <PageHeader
        title={job.name}
        subtitle={<>{nameOf(job.source_id)} → {nameOf(job.target_id)} · created by {job.created_by} on {formatTime(job.created_at)}</>}
      >
        <StatusBadge status={status} />
        {manage && (
          <>
            {canStart && (
              <button className="btn primary" disabled={!!busy} onClick={() => act(status === "paused" ? "resume" : "start")}>
                {status === "paused" ? "Resume" : status === "failed" ? "Retry" : "Start"}
              </button>
            )}
            {canPause && <button className="btn" disabled={!!busy || job.desired === "pause"} onClick={() => act("pause")}>Pause</button>}
            <button className="btn primary" disabled={!canCutover || !!busy || job.desired === "cutover"}
              title={canCutover ? undefined : "Available while the migration is streaming changes"}
              onClick={() => setConfirm(confirmations.cutover)}>
              Cutover
            </button>
            {canAbort && <button className="btn danger" disabled={!!busy || job.desired === "abort"} onClick={() => setConfirm(confirmations.abort)}>Abort</button>}
            {canDelete && <button className="btn" disabled={!!busy} onClick={() => setConfirm(confirmations.delete)}>Delete</button>}
          </>
        )}
      </PageHeader>

      {error && <Banner tone="error">{error}</Banner>}
      {actionError && <Banner tone="error">{actionError}</Banner>}
      {job.error && <Banner tone="error"><strong>Failed:</strong> <span className="pre-line">{job.error}</span></Banner>}
      {status === "running" && job.desired && job.desired !== "run" && (
        <Banner tone="info">Requested: {job.desired}. The worker picks this up within a couple of seconds.</Banner>
      )}
      {status === "queued" && <Banner tone="info">Waiting for a worker to pick the job up.</Banner>}
      {status === "paused" && (
        <Banner tone="warn">Paused. The source keeps WAL for this migration until it is resumed or aborted; do not leave it paused for long on a busy database.</Banner>
      )}
      {st.fenced && status !== "completed" && <Banner tone="warn">The source database is currently read-only (fenced for cutover).</Banner>}
      {status === "completed" && (
        <Banner tone="ok">
          Cutover complete. The target is ready for application traffic.
          {st.fenced && " The source database was left read-only."}
        </Banner>
      )}

      <div className="card">
        <Stepper job={job} />
        <div className="figures">
          <Figure label="Replication lag">{job.phase === "streaming" || job.phase === "cutover" ? formatBytes(st.lag_bytes ?? 0) : "—"}</Figure>
          <Figure label="Applied LSN"><span className="mono">{st.applied_lsn || "—"}</span></Figure>
          <Figure label="Source LSN"><span className="mono">{st.source_lsn || "—"}</span></Figure>
          <Figure label="Transactions applied">{formatNumber(st.tx_applied ?? 0)}</Figure>
          <Figure label="Row changes applied">{formatNumber(st.rows_applied ?? 0)}</Figure>
          <Figure label="Apply anomalies">
            <span style={{ color: (st.anomalies ?? 0) > 0 ? "var(--danger)" : undefined }}>{formatNumber(st.anomalies ?? 0)}</span>
          </Figure>
          <Figure label="Last applied commit">{formatTime(st.last_commit_time)}</Figure>
          <Figure label="Source version"><span title={st.source_version}>{st.source_version || "—"}</span></Figure>
          <Figure label="Target version"><span title={st.target_version}>{st.target_version || "—"}</span></Figure>
          <Figure label="Replication slot"><span className="mono">{st.slot || "—"}</span></Figure>
        </div>
      </div>

      <div className="card">
        <div className="card-head">
          <h2>Preflight check</h2>
          <div className="actions">
            {report && <StatusBadge status={report.ok ? "ok" : "fail"} />}
            {manage && (
              <button className="btn small" onClick={runPreflight} disabled={preflighting}>
                {preflighting ? "Checking…" : report ? "Run again" : "Run preflight"}
              </button>
            )}
          </div>
        </div>
        <div className="card-body flush table-wrap">
          {report
            ? <Preflight report={report} />
            : <div className="empty">Not run yet. Preflight inspects both databases without changing either.</div>}
        </div>
      </div>

      <div className="card">
        <div className="card-head">
          <h2>Tables</h2>
          {job.tables.length > 0 && (
            <span className="muted">{job.tables.length} tables · {formatNumber(totalCopied)} of ~{formatNumber(totalEst)} rows copied</span>
          )}
        </div>
        <div className="card-body flush table-wrap">
          {job.tables.length === 0 ? (
            <div className="empty">The table list appears once the schema phase has run.</div>
          ) : (
            <table className="data">
              <thead>
                <tr>
                  <th>Table</th><th>Status</th><th style={{ minWidth: 140 }}>Initial copy</th><th className="num">Rows copied</th>
                  <th className="num">Estimated</th><th className="num">Source count</th><th className="num">Target count</th><th>Validation</th>
                </tr>
              </thead>
              <tbody>
                {job.tables.map((t) => (
                  <tr key={`${t.schema}.${t.table}`}>
                    <td className="mono">{t.schema}.{t.table}{t.detail && <div className="muted" style={{ fontFamily: "var(--font)" }}>{t.detail}</div>}</td>
                    <td><StatusBadge status={t.status} /></td>
                    <td style={{ verticalAlign: "middle" }}>
                      <Progress value={t.status === "copied" ? 1 : t.rows_copied} total={t.status === "copied" ? 1 : Math.max(t.est_rows, t.rows_copied)} />
                    </td>
                    <td className="num">{formatNumber(t.rows_copied)}</td>
                    <td className="num muted">~{formatNumber(t.est_rows)}</td>
                    <td className="num">{t.src_count >= 0 ? formatNumber(t.src_count) : "—"}</td>
                    <td className="num">{t.dst_count >= 0 ? formatNumber(t.dst_count) : "—"}</td>
                    <td><StatusBadge status={t.validation} /></td>
                  </tr>
                ))}
              </tbody>
            </table>
          )}
        </div>
      </div>

      <div className="card">
        <div className="card-head"><h2>Configuration</h2></div>
        <div className="figures" style={{ borderTop: "none" }}>
          <Figure label="Schemas">{job.config.schemas?.length ? job.config.schemas.join(", ") : "all"}</Figure>
          <Figure label="Excluded tables">
            <span title={job.config.exclude_tables?.join(", ")}>{job.config.exclude_tables?.length ? job.config.exclude_tables.join(", ") : "none"}</span>
          </Figure>
          <Figure label="Snapshot workers">{job.config.snapshot_workers || 4}</Figure>
          <Figure label="Validation">{job.config.validation || "checksum"}</Figure>
          <Figure label="Fence source at cutover">{fenced ? "yes" : "no"}</Figure>
          <Figure label="Keep replication slot">{job.config.keep_slot ? "yes" : "no"}</Figure>
          <Figure label="Cutover timeout">{job.config.cutover_timeout_seconds || 300}s</Figure>
        </div>
      </div>

      <div className="card">
        <div className="card-head"><h2>Events</h2><span className="muted">newest first</span></div>
        <div className="events">
          {job.events.length === 0 && <div className="empty">No events yet.</div>}
          {job.events.map((e) => (
            <div className="event" key={e.id}>
              <span className="muted">{formatTime(e.time)}</span>
              <span className={`lvl-${e.level}`}>{e.level}</span>
              <span className="pre-line">{e.message}</span>
            </div>
          ))}
        </div>
      </div>

      {confirm && (
        <ConfirmDialog
          title={confirm.title}
          confirmLabel={confirm.label}
          danger={confirm.action !== "cutover"}
          onClose={() => setConfirm(null)}
          onConfirm={async () => {
            await api.migrationAction(job.id, confirm.action);
            if (confirm.action === "delete") router.push("/migrations/");
            else await reload();
          }}
        >
          {confirm.body}
        </ConfirmDialog>
      )}
    </div>
  );
}

function DetailFromQuery() {
  const id = useSearchParams().get("id") ?? "";
  if (!id) return <div className="content"><Banner tone="error">No migration selected.</Banner></div>;
  return <Detail key={id} id={id} />;
}

// The export is static, so the job id travels in the query string, which is
// only known in the browser; hence the Suspense boundary.
export default function MigrationDetailPage() {
  return (
    <Suspense fallback={<div className="content"><Loading /></div>}>
      <DetailFromQuery />
    </Suspense>
  );
}
