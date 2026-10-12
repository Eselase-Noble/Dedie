"use client";

import Link from "next/link";
import { useRouter, useSearchParams } from "next/navigation";
import { Suspense, useState, type ReactNode } from "react";
import { api, errorMessage } from "@/lib/api";
import { formatBytes, formatDuration, formatNumber, formatTime } from "@/lib/format";
import { useLoad } from "@/lib/hooks";
import type { MigrationAction, MigrationDetail, OnExisting, PreflightReport } from "@/lib/types";
import ApplyRate from "@/components/migrations/ApplyRate";
import CopyProgress from "@/components/migrations/CopyProgress";
import ExistingObjectsDialog from "@/components/migrations/ExistingObjects";
import MigrationForm from "@/components/MigrationForm";
import NotMigrated, { leavesBehind } from "@/components/migrations/NotMigrated";
import SchemaPreview from "@/components/migrations/SchemaPreview";
import { download } from "@/lib/csv";
import { copyText } from "@/lib/desktop";
import { useSession } from "@/components/session";
import { ConnectionName, useProductionGuard } from "@/components/Environment";
import { Banner, ConfirmDialog, Loading, PageHeader, Progress, StatusBadge } from "@/components/ui";

const PHASES = ["schema", "snapshot", "indexing", "streaming", "cutover", "done"];
const PHASE_LABELS: Record<string, string> = {
  schema: "Schema", snapshot: "Initial copy", indexing: "Indexes", streaming: "Streaming", cutover: "Cutover", done: "Done",
};

function Stepper({ job }: { job: MigrationDetail }) {
  // An offline migration has no streaming phase, and what the engine
  // calls cutover is its validation. A sync is finished, not cut over.
  const offline = job.config.mode === "offline";
  const cutover = offline ? "Validation" : job.config.mode === "sync" ? "Finish" : PHASE_LABELS.cutover;
  const phases = offline ? PHASES.filter((p) => p !== "streaming") : PHASES;
  const current = job.status === "completed" ? phases.length : phases.indexOf(job.phase);
  return (
    <div className="stepper">
      {phases.map((phase, i) => {
        let cls = "";
        if (i < current) cls = "done";
        else if (i === current) cls = job.status === "failed" ? "failed" : "current";
        return (
          <div key={phase} className={`step ${cls}`}>
            <div className="dot">{i < current ? "✓" : i + 1}</div>
            {phase === "cutover" ? cutover : PHASE_LABELS[phase]}
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

/** Renders a preflight report as plain text, for a ticket or a change request. */
function reportText(name: string, report: PreflightReport): string {
  const lines = [`Preflight for ${name}: ${report.ok ? "passed" : "FAILED"} (${report.tables} table(s))`, ""];
  for (const c of report.checks) {
    lines.push(`[${c.status.toUpperCase()}] ${c.name}`);
    for (const line of c.detail.split("\n")) lines.push(`    ${line}`);
    lines.push("");
  }
  return lines.join("\n");
}

function Preflight({ report, name }: { report: PreflightReport; name: string }) {
  const [only, setOnly] = useState<"all" | "attention">("all");
  const count = (status: string) => report.checks.filter((c) => c.status === status).length;
  const shown = only === "all" ? report.checks : report.checks.filter((c) => c.status !== "ok");
  return (
    <>
      <div className="preflight-bar">
        <span><strong>{count("fail")}</strong> blocking</span>
        <span><strong>{count("warn")}</strong> to read</span>
        <span><strong>{count("ok")}</strong> passed</span>
        <span className="spacer" />
        <div className="segmented small">
          <button type="button" className={only === "all" ? "active" : ""} onClick={() => setOnly("all")}>All</button>
          <button type="button" className={only === "attention" ? "active" : ""} onClick={() => setOnly("attention")}>Needs attention</button>
        </div>
        <button className="btn small" onClick={() => copyText(reportText(name, report))}>Copy</button>
        <button className="btn small" onClick={() => download(`preflight-${name.replace(/[^A-Za-z0-9]+/g, "-").toLowerCase()}.txt`, reportText(name, report), "text/plain;charset=utf-8")}>Download</button>
      </div>
      <ul className="checks">
        {shown.map((c, i) => (
          <li key={i}>
            <StatusBadge status={c.status} />
            <strong>{c.name}</strong>
            <span className="check-detail">{c.detail}</span>
          </li>
        ))}
      </ul>
      {shown.length === 0 && <div className="empty">Nothing needs attention.</div>}
    </>
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
  const { guarded, dialog: productionDialog } = useProductionGuard();
  const [actionError, setActionError] = useState("");
  const [busy, setBusy] = useState("");
  const [confirm, setConfirm] = useState<Confirm | null>(null);
  const [preflighting, setPreflighting] = useState(false);
  const [asking, setAsking] = useState(false);
  const [previewing, setPreviewing] = useState(false);
  const [leftovers, setLeftovers] = useState(false);
  // Duplicating opens the new-migration form filled in from this job;
  // "replace" is the same with existing objects on the target replaced.
  const [duplicating, setDuplicating] = useState<"" | "copy" | "replace">("");

  if (loading && !job) return <Loading page lines={6} />;
  if (!job) return <div className="content"><Banner tone="error">{error || "Migration not found."}</Banner></div>;

  const connectionOf = (connId: string) => connections.data?.find((c) => c.id === connId) ?? null;
  const nameOf = (connId: string) => connectionOf(connId)?.name ?? connId;
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
      // Starting a migration that replaces objects on a production target
      // asks for the target's name.
      const done = await guarded(nameOf(job.target_id), (typed) => api.migrationAction(job.id, action, typed), "Start and replace");
      if (!done) return;
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

  const runPreflight = async (ask = true) => {
    setPreflighting(true);
    setActionError("");
    try {
      const result = await api.preflight(job.id);
      await reload();
      // Objects already on the target are the administrator's call; ask
      // as soon as they are found, unless an answer was already given.
      if (ask && (result.existing?.length ?? 0) > 0 && onExisting === "fail") setAsking(true);
    } catch (err) {
      setActionError(errorMessage(err));
    } finally {
      setPreflighting(false);
    }
  };

  const fenced = !!job.config.fence_source;
  const onExisting: OnExisting = job.config.on_existing || "fail";
  // The choice is acted on when the job creates its objects, so it can
  // only be made before that.
  const canDecide = status === "draft" || (status === "failed" && job.phase === "schema");
  // A continuous sync is the same job with a different intent: it is not
  // expected to end, and ending it is called finishing rather than cutover.
  const sync = job.config.mode === "sync";
  // An offline migration copies once and finishes by itself; there is no
  // stream to cut over from.
  const offline = job.config.mode === "offline";
  // Every pipeline copies once per job: a retry after the copy carries on
  // from the indexes or the validation and never reads the source again.
  const copyStands = offline && status === "failed" && !!st.snapshot_done;
  const confirmations: Record<"cutover" | "abort" | "delete", Confirm> = {
    cutover: {
      action: "cutover",
      title: sync ? "Finish this sync?" : "Start cutover?",
      label: sync ? "Finish sync" : "Start cutover",
      body: (
        <>
          <p>{sync
            ? "Finishing stops the sync and leaves the target as an independent copy. Changes made on the source afterwards are no longer carried over. In order, it will:"
            : "Cutover finishes the migration. In order, it will:"}</p>
          <ol style={{ margin: "0 0 10px", paddingLeft: 20 }}>
            {fenced
              ? <li>Make the source database read-only and disconnect its client sessions.</li>
              : <li>Assume writes to the source have already been stopped. <strong>Stop your application first</strong>; this migration is not configured to fence the source.</li>}
            <li>Wait for the remaining changes to be applied to the target.</li>
            <li>Synchronize sequences and auto-increment counters, then validate every table ({job.config.validation || "checksum"}).</li>
            <li>Create triggers, refresh statistics and remove what the migration left on both servers.</li>
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
            Replication stops and anything the migration created on the source (for PostgreSQL, the replication
            slot and publication) is removed. The source database keeps running normally.
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
  const existing = report?.existing ?? [];

  return (
    <div className="content">
      <PageHeader
        back={{ href: "/migrations/", label: "Migrations" }}
        title={job.name}
        subtitle={<><ConnectionName connection={connectionOf(job.source_id)} name={job.source_id} /> → <ConnectionName connection={connectionOf(job.target_id)} name={job.target_id} /> · created by {job.created_by} on {formatTime(job.created_at)}</>}
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
            {!offline && status !== "completed" && status !== "aborted" && (
              <button className="btn primary" disabled={!canCutover || !!busy || job.desired === "cutover"}
                title={canCutover ? undefined : "Available while the migration is streaming changes"}
                onClick={() => setConfirm(confirmations.cutover)}>
                {sync ? "Finish sync" : "Cutover"}
              </button>
            )}
            {canAbort && <button className="btn danger" disabled={!!busy || job.desired === "abort"} onClick={() => setConfirm(confirmations.abort)}>Abort</button>}
            {canDelete && <button className="btn" disabled={!!busy} onClick={() => setConfirm(confirmations.delete)}>Delete</button>}
            <button className="btn" onClick={() => setDuplicating("copy")}
              title="Open the new migration form filled in from this one">Duplicate</button>
          </>
        )}
      </PageHeader>

      {error && <Banner tone="error">{error}</Banner>}
      {actionError && <Banner tone="error">{actionError}</Banner>}
      {job.error && <Banner tone="error"><strong>Failed:</strong> <span className="pre-line">{job.error}</span></Banner>}
      {copyStands && (
        <Banner tone="warn">
          The rows were copied before this failed, and Retry does not copy them again: it repeats only the steps after
          the copy, on the rows already there. If the copy itself is what is wrong, as when the source was written to
          while it ran, stop those writes, then empty the target (or choose to replace existing objects) and create the
          migration again.{" "}
          {manage && <button className="btn small" onClick={() => setDuplicating("replace")}>Create again</button>}
        </Banner>
      )}
      {status === "running" && job.desired && job.desired !== "run" && (
        <Banner tone="info">Requested: {job.desired}. The worker picks this up within a couple of seconds.</Banner>
      )}
      {status === "queued" && <Banner tone="info">Waiting for a worker to pick the job up.</Banner>}
      {status === "paused" && (
        <Banner tone="warn">{offline
          ? "Paused. The copy starts over from a new snapshot when the job is resumed."
          : "Paused. The source keeps its change log for this migration until it is resumed or aborted; do not leave it paused for long on a busy database."}</Banner>
      )}
      {sync && status === "running" && job.phase === "streaming" && (
        <Banner tone="ok">In sync: every change on the source is being applied to the target. This continues until you pause or finish it.</Banner>
      )}
      {st.fenced && status !== "completed" && <Banner tone="warn">The source database is currently read-only (fenced for cutover).</Banner>}
      {status === "completed" && (
        <Banner tone="ok">
          {sync ? "Sync finished. The target is an independent, validated copy."
            : offline ? "Offline copy complete. The target holds the source as it was when the copy started, validated."
              : "Cutover complete. The target is ready for application traffic."}
          {st.fenced && " The source database was left read-only."}
        </Banner>
      )}

      {st.objects && leavesBehind(job.kind) && (
        <div className="card">
          <div className="card-head">
            <h2>Views and stored code</h2>
            <div className="actions">
              {manage && (st.objects.review > 0 || st.objects.left > 0) && (
                <button className="btn small" onClick={() => setLeftovers(true)}
                  title="What was left behind or wants checking, with its definition, the converted draft and the notes">See what needs attention</button>
              )}
            </div>
          </div>
          <div className="figures" style={{ borderTop: "none" }}>
            <Figure label="Created">{formatNumber(st.objects.created)}</Figure>
            <Figure label="Created, needs review">{formatNumber(st.objects.review)}</Figure>
            <Figure label="Left for you">{formatNumber(st.objects.left)}</Figure>
          </div>
        </div>
      )}

      <div className="card">
        <Stepper job={job} />
        {job.phase === "snapshot" && job.tables.length > 0 && status !== "aborted" && <CopyProgress job={job} />}
        <div className="figures">
          {/* An offline copy follows no changes, so it has no stream to report on. */}
          {!offline && (
            <>
              <Figure label="Replication lag">
                {job.phase !== "streaming" && job.phase !== "cutover" ? "—"
                  : st.lag_seconds !== undefined ? formatDuration(st.lag_seconds) : formatBytes(st.lag_bytes ?? 0)}
              </Figure>
              <Figure label="Applied position"><span className="mono">{st.applied_lsn || "—"}</span></Figure>
              <Figure label="Source position"><span className="mono">{st.source_lsn || "—"}</span></Figure>
              <Figure label="Transactions applied">{formatNumber(st.tx_applied ?? 0)}</Figure>
              <Figure label="Row changes applied">{formatNumber(st.rows_applied ?? 0)}</Figure>
              <Figure label="Apply rate"><ApplyRate job={job} /></Figure>
              <Figure label="Apply anomalies">
                <span style={{ color: (st.anomalies ?? 0) > 0 ? "var(--danger)" : undefined }}>{formatNumber(st.anomalies ?? 0)}</span>
              </Figure>
              <Figure label="Last applied commit">{formatTime(st.last_commit_time)}</Figure>
            </>
          )}
          <Figure label="Source version"><span title={st.source_version}>{st.source_version || "—"}</span></Figure>
          <Figure label="Target version"><span title={st.target_version}>{st.target_version || "—"}</span></Figure>
          {st.slot && <Figure label="Replication slot"><span className="mono">{st.slot}</span></Figure>}
        </div>
      </div>

      <div className="card">
        <div className="card-head">
          <h2>Preflight check</h2>
          <div className="actions">
            {report && <StatusBadge status={report.ok ? "ok" : "fail"} />}
            {manage && (
              <button className="btn small" onClick={() => setPreviewing(true)}
                title="Read the statements this migration would run on the target, without running them">Preview schema</button>
            )}
            {manage && leavesBehind(job.kind) && (
              <button className="btn small" onClick={() => setLeftovers(true)}
                title="Stored code and views this migration does not carry over, with their definitions on the source">Not migrated</button>
            )}
            {manage && (
              <button className="btn small" onClick={() => runPreflight()} disabled={preflighting}>
                {preflighting ? "Checking…" : report ? "Run again" : "Run preflight"}
              </button>
            )}
          </div>
        </div>
        {existing.length > 0 && (
          <div className="card-body">
            {st.schema_done && onExisting !== "fail" ? (
              // The report was made before the job created anything; by now
              // the choice has been carried out.
              <Banner tone="info">
                The target had {existing.length} object{existing.length === 1 ? "" : "s"} this migration would create.{" "}
                {onExisting === "skip" ? "They were kept as they were and left out of the migration."
                  : "They were dropped and created again from the source when the migration started."}
              </Banner>
            ) : (
              <Banner tone={onExisting === "fail" ? "error" : "warn"}>
                The target already has {existing.length} object{existing.length === 1 ? "" : "s"} this migration would create.{" "}
                {onExisting === "fail" ? "The migration will not start until you decide what to do with them."
                  : onExisting === "skip" ? "They will be kept as they are and left out of the migration."
                    : "They will be dropped and created again from the source when the migration starts."}{" "}
                {manage && canDecide && <button className="btn small" onClick={() => setAsking(true)}>{onExisting === "fail" ? "Decide…" : "Change…"}</button>}
              </Banner>
            )}
          </div>
        )}
        <div className="card-body flush table-wrap">
          {report
            ? <Preflight report={report} name={job.name} />
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
        <div className="card-body flush table-wrap tall">
          {job.tables.length === 0 ? (
            <div className="empty">The table list appears once the schema phase has run.</div>
          ) : (
            <table className="data stack">
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
          <Figure label="Purpose">{sync ? "continuous sync" : offline ? "offline copy" : "online migration"}</Figure>
          <Figure label="Level">{job.config.tables?.length ? "table" : job.config.schemas?.length ? "schema" : "database"}</Figure>
          <Figure label="Schemas">{job.config.schemas?.length ? job.config.schemas.join(", ") : "all"}</Figure>
          <Figure label="Tables">
            <span title={job.config.tables?.join(", ")}>{job.config.tables?.length ? job.config.tables.join(", ") : "all"}</span>
          </Figure>
          <Figure label="Excluded tables">
            <span title={job.config.exclude_tables?.join(", ")}>{job.config.exclude_tables?.length ? job.config.exclude_tables.join(", ") : "none"}</span>
          </Figure>
          <Figure label="Snapshot workers">{job.config.snapshot_workers || 4}</Figure>
          <Figure label="Validation">{job.config.validation || "checksum"}</Figure>
          {!offline && !sync && <Figure label="Fence source at cutover">{fenced ? "yes" : "no"}</Figure>}
          <Figure label="Objects already on the target">{onExisting === "skip" ? "keep and skip" : onExisting === "replace" ? "replace" : "refuse"}</Figure>
          {leavesBehind(job.kind) && (
            <Figure label="Stored code">
              {/* Short enough for one cell; the sentence is there on hover. */}
              {job.config.code === "off" ? <span title="Nothing is converted; the source's definitions are handed over">not converted</span>
                : job.config.code === "review" ? <span title="Created on the target, including what may behave differently and needs review">all conversions</span>
                  : <span title="Created on the target where the conversion keeps the behaviour; the rest is handed over as drafts">clean conversions</span>}
            </Figure>
          )}
          {job.kind === "postgres-postgres" && !offline && <Figure label="Keep replication slot">{job.config.keep_slot ? "yes" : "no"}</Figure>}
          {!offline && <Figure label="Cutover timeout">{job.config.cutover_timeout_seconds || 300}s</Figure>}
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

      {duplicating && connections.data && (
        <MigrationForm
          connections={connections.data}
          template={{
            name: `${job.name} (copy)`,
            source_id: job.source_id,
            target_id: job.target_id,
            config: duplicating === "replace" ? { ...job.config, on_existing: "replace" } : job.config,
          }}
          onClose={() => setDuplicating("")}
          onCreated={(created) => router.push(`/migrations/detail/?id=${encodeURIComponent(created.id)}`)}
        />
      )}
      {previewing && <SchemaPreview jobId={job.id} name={job.name} onClose={() => setPreviewing(false)} />}
      {leftovers && <NotMigrated jobId={job.id} name={job.name} onClose={() => setLeftovers(false)} />}
      {asking && (
        <ExistingObjectsDialog jobId={job.id} objects={existing} current={onExisting}
          onClose={() => setAsking(false)} onChosen={() => runPreflight(false)} />
      )}
      {productionDialog}
      {confirm && (
        <ConfirmDialog
          title={confirm.title}
          confirmLabel={confirm.label}
          danger={confirm.action !== "cutover"}
          typeName={confirm.action === "cutover" && fenced && connectionOf(job.source_id)?.environment === "production" ? nameOf(job.source_id) : undefined}
          onClose={() => setConfirm(null)}
          onConfirm={async (typed) => {
            await api.migrationAction(job.id, confirm.action, typed);
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
    <Suspense fallback={<Loading page lines={6} />}>
      <DetailFromQuery />
    </Suspense>
  );
}
