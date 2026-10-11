"use client";

import { Fragment, useState } from "react";
import { api, errorMessage } from "@/lib/api";
import { formatBytes, formatTime } from "@/lib/format";
import { useLoad } from "@/lib/hooks";
import type { Backup } from "@/lib/types";
import { useSession } from "@/components/session";
import { Banner, ConfirmDialog, Loading, PageHeader, StatusBadge } from "@/components/ui";
import CreateBackup from "@/components/backups/CreateBackup";
import RestoreDialog from "@/components/backups/RestoreDialog";
import Schedules from "@/components/backups/Schedules";
import { typeLabel } from "@/components/backups/options";

export default function BackupsPage() {
  const { can } = useSession();
  const mayRestore = can("backups.restore");
  const { data, error, loading, reload } = useLoad(
    async () => {
      const [backups, restores, schedules, connections] = await Promise.all([
        api.backups(), api.restores(), api.schedules(), can("connections.view") ? api.connections() : [],
      ]);
      return { backups, restores, schedules, connections };
    },
    [],
    2000,
    // Keep refreshing only while a backup or restore is in flight.
    (d) => !d || d.backups.some((b) => b.status === "running") || d.restores.some((r) => r.status === "running")
      || d.schedules.some((s) => s.last_status === "running"),
  );
  const [creating, setCreating] = useState(false);
  const [restoring, setRestoring] = useState<Backup | null>(null);
  const [deleting, setDeleting] = useState<Backup | null>(null);
  const [verifying, setVerifying] = useState("");
  const [notice, setNotice] = useState<{ tone: "ok" | "error"; text: string } | null>(null);
  const [openLog, setOpenLog] = useState("");

  const verify = async (b: Backup) => {
    setVerifying(b.id);
    setNotice(null);
    try {
      await api.verifyBackup(b.id);
      await reload();
      setNotice({ tone: "ok", text: `Backup of ${b.database} from ${formatTime(b.created_at)} is intact: the checksum matches and the archive decrypts.` });
    } catch (err) {
      setNotice({ tone: "error", text: `Verification failed: ${errorMessage(err)}` });
    } finally {
      setVerifying("");
    }
  };

  const backupLabel = (id: string) => {
    const b = data?.backups.find((x) => x.id === id);
    return b ? `${b.database} (${formatTime(b.created_at)})` : id;
  };

  return (
    <div className="content">
      <PageHeader title="Backups" subtitle="Encrypted backups on demand or on a schedule, with retention, verification and restore.">
        <button className="btn primary" onClick={() => setCreating(true)}>Create backup</button>
      </PageHeader>
      {error && <Banner tone="error">{error}</Banner>}
      {notice && <Banner tone={notice.tone}>{notice.text}</Banner>}

      <div className="card">
        <div className="card-head"><h2>Backups</h2></div>
        <div className="card-body flush table-wrap">
          {loading ? <Loading /> : !data || data.backups.length === 0 ? (
            <div className="empty">No backups yet.</div>
          ) : (
            <table className="data">
              <thead>
                <tr>
                  <th>Connection</th><th>Database</th><th>Type</th><th>Status</th><th className="num">Size</th>
                  <th>Verified</th><th>Created</th><th />
                </tr>
              </thead>
              <tbody>
                {data.backups.map((b) => (
                  <tr key={b.id}>
                    <td>{b.connection_name}{b.note && <div className="muted">{b.note}</div>}</td>
                    <td>{b.database}</td>
                    <td title={b.tables.join(", ")}>
                      {typeLabel(b.type)}
                      {b.type === "tables" && <div className="muted">{b.tables.length} table{b.tables.length === 1 ? "" : "s"}</div>}
                    </td>
                    <td>
                      <StatusBadge status={b.status} />
                      {b.error && <div className="pre-line" style={{ color: "var(--danger)", maxWidth: 320 }}>{b.error}</div>}
                    </td>
                    <td className="num">{b.status === "completed" ? formatBytes(b.size_bytes) : "—"}</td>
                    <td className="nowrap muted" title={b.sha256 ? `SHA-256 ${b.sha256}` : ""}>{formatTime(b.verified_at)}</td>
                    <td className="nowrap muted">{formatTime(b.created_at)}<div>{b.created_by}</div></td>
                    <td>
                      <div className="row-actions">
                        {b.status === "completed" && (
                          <button className="btn small" onClick={() => verify(b)} disabled={verifying === b.id}>
                            {verifying === b.id ? "Verifying…" : "Verify"}
                          </button>
                        )}
                        {mayRestore && b.status === "completed" && b.type !== "physical" && (
                          <button className="btn small" onClick={() => setRestoring(b)}>Restore</button>
                        )}
                        {mayRestore && b.status === "completed" && (
                          <a className="btn small" href={api.backupDownloadUrl(b.id)} download
                            title="Download the decrypted archive. The copy is no longer protected by Dedie.">Download</a>
                        )}
                        {mayRestore && b.status !== "running" && <button className="btn small" onClick={() => setDeleting(b)}>Delete</button>}
                      </div>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          )}
        </div>
      </div>

      {data && <Schedules schedules={data.schedules} connections={data.connections} mayManage={mayRestore} reload={reload} />}

      <div className="card">
        <div className="card-head"><h2>Restore history</h2></div>
        <div className="card-body flush table-wrap">
          {!data || data.restores.length === 0 ? (
            <div className="empty">No restores have been run.</div>
          ) : (
            <table className="data">
              <thead>
                <tr><th>Backup</th><th>Target</th><th>Status</th><th>Started</th><th>Finished</th><th /></tr>
              </thead>
              <tbody>
                {data.restores.map((r) => (
                  <Fragment key={r.id}>
                    <tr>
                      <td>{backupLabel(r.backup_id)}</td>
                      <td>{r.target_name}{r.database && <div className="muted">{r.database}</div>}</td>
                      <td><StatusBadge status={r.status} /></td>
                      <td className="nowrap muted">{formatTime(r.created_at)}<div>{r.created_by}</div></td>
                      <td className="nowrap muted">{formatTime(r.finished_at)}</td>
                      <td>
                        <div className="row-actions">
                          {(r.log || r.error) && (
                            <button className="btn small" onClick={() => setOpenLog(openLog === r.id ? "" : r.id)}>
                              {openLog === r.id ? "Hide log" : "Show log"}
                            </button>
                          )}
                        </div>
                      </td>
                    </tr>
                    {openLog === r.id && (
                      <tr>
                        <td colSpan={6} style={{ background: "var(--bg-subtle)" }}>
                          <pre>{r.error || r.log}</pre>
                        </td>
                      </tr>
                    )}
                  </Fragment>
                ))}
              </tbody>
            </table>
          )}
        </div>
      </div>

      {creating && data && <CreateBackup connections={data.connections} onClose={() => setCreating(false)} onDone={reload} />}
      {restoring && data && (
        <RestoreDialog backup={restoring} connections={data.connections} onClose={() => setRestoring(null)} onDone={reload} />
      )}
      {deleting && (
        <ConfirmDialog
          title="Delete this backup?"
          confirmLabel="Delete backup"
          danger
          onClose={() => setDeleting(null)}
          onConfirm={async () => {
            await api.deleteBackup(deleting.id);
            await reload();
          }}
        >
          <p>
            The backup of <strong>{deleting.database}</strong> from {formatTime(deleting.created_at)} is removed from
            disk permanently. This cannot be undone.
          </p>
        </ConfirmDialog>
      )}
    </div>
  );
}
