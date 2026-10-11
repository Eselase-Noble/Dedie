"use client";

import { Fragment, useState, type FormEvent } from "react";
import { api, errorMessage } from "@/lib/api";
import { formatBytes, formatTime } from "@/lib/format";
import { useLoad } from "@/lib/hooks";
import type { Backup, Connection } from "@/lib/types";
import { useSession } from "@/components/session";
import { Banner, Checkbox, ConfirmDialog, Field, Loading, Modal, PageHeader, StatusBadge } from "@/components/ui";

function CreateBackup({ connections, onClose, onDone }: { connections: Connection[]; onClose: () => void; onDone: () => void }) {
  const candidates = connections.filter((c) => c.kind === "postgres");
  const [connId, setConnId] = useState(candidates[0]?.id ?? "");
  const [note, setNote] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");

  const submit = async (e: FormEvent) => {
    e.preventDefault();
    setBusy(true);
    setError("");
    try {
      await api.createBackup(connId, note);
      onDone();
      onClose();
    } catch (err) {
      setError(errorMessage(err));
      setBusy(false);
    }
  };

  return (
    <Modal title="Create backup" onClose={onClose} footer={
      <>
        <button className="btn" type="button" onClick={onClose} disabled={busy}>Cancel</button>
        <button className="btn primary" type="submit" form="backup-form" disabled={busy || !connId}>
          {busy ? "Starting…" : "Start backup"}
        </button>
      </>
    }>
      <form id="backup-form" onSubmit={submit}>
        {error && <Banner tone="error">{error}</Banner>}
        <p className="hint" style={{ marginBottom: 12 }}>
          A logical backup of the whole database, taken with pg_dump from a consistent snapshot while the database
          stays online. The archive is encrypted before it is written to the server&apos;s data directory.
        </p>
        <Field label="Connection">
          <select className="select" value={connId} onChange={(e) => setConnId(e.target.value)} required>
            {candidates.length === 0 && <option value="">No PostgreSQL connections</option>}
            {candidates.map((c) => <option key={c.id} value={c.id}>{c.name} ({c.database})</option>)}
          </select>
        </Field>
        <Field label="Note (optional)">
          <input className="input" value={note} onChange={(e) => setNote(e.target.value)} placeholder="Before the 4.2 release" maxLength={200} />
        </Field>
      </form>
    </Modal>
  );
}

function RestoreDialog({ backup, connections, onClose, onDone }: {
  backup: Backup; connections: Connection[]; onClose: () => void; onDone: () => void;
}) {
  const candidates = connections.filter((c) => c.kind === backup.kind && !c.read_only);
  const [targetId, setTargetId] = useState("");
  const [clean, setClean] = useState(false);
  const [keepOwners, setKeepOwners] = useState(false);
  const [confirm, setConfirm] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const target = candidates.find((c) => c.id === targetId) ?? null;

  const submit = async (e: FormEvent) => {
    e.preventDefault();
    setBusy(true);
    setError("");
    try {
      await api.restoreBackup(backup.id, { target_id: targetId, clean, keep_owners: keepOwners, confirm });
      onDone();
      onClose();
    } catch (err) {
      setError(errorMessage(err));
      setBusy(false);
    }
  };

  return (
    <Modal title="Restore backup" onClose={onClose} footer={
      <>
        <button className="btn" type="button" onClick={onClose} disabled={busy}>Cancel</button>
        <button className="btn danger" type="submit" form="restore-form" disabled={busy || !target || confirm !== target.database}>
          {busy ? "Starting…" : "Restore"}
        </button>
      </>
    }>
      <form id="restore-form" onSubmit={submit}>
        {error && <Banner tone="error">{error}</Banner>}
        <p style={{ marginBottom: 12 }}>
          Restoring <strong>{backup.database}</strong> from {backup.connection_name}, taken {formatTime(backup.created_at)}.
          The file is verified first, and the restore runs in a single transaction: if it fails, the target is left unchanged.
        </p>
        <Field label="Target connection" hint="Read-only connections cannot be restore targets.">
          <select className="select" value={targetId} onChange={(e) => { setTargetId(e.target.value); setConfirm(""); }} required>
            <option value="">Select a connection</option>
            {candidates.map((c) => <option key={c.id} value={c.id}>{c.name} ({c.database} on {c.host})</option>)}
          </select>
        </Field>
        <Checkbox label="Replace existing objects (clean)" checked={clean} onChange={setClean}
          hint="Drops each object in the target before recreating it from the backup. Data in those objects is lost. Without this, the restore fails if an object already exists." />
        <Checkbox label="Restore ownership and privileges" checked={keepOwners} onChange={setKeepOwners}
          hint="Requires the same roles to exist on the target server. Leave off to have everything owned by the connecting user." />
        {target && (
          <Field label={`Type the target database name (${target.database}) to confirm`}>
            <input className="input" value={confirm} onChange={(e) => setConfirm(e.target.value)} autoComplete="off" spellCheck={false} />
          </Field>
        )}
      </form>
    </Modal>
  );
}

export default function BackupsPage() {
  const { can } = useSession();
  const mayRestore = can("backups.restore");
  const { data, error, loading, reload } = useLoad(
    async () => {
      const [backups, restores, connections] = await Promise.all([
        api.backups(), api.restores(), can("connections.view") ? api.connections() : [],
      ]);
      return { backups, restores, connections };
    },
    [],
    2000,
    // Keep refreshing only while a backup or restore is in flight.
    (d) => !d || d.backups.some((b) => b.status === "running") || d.restores.some((r) => r.status === "running"),
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
      <PageHeader title="Backups" subtitle="Encrypted logical backups stored in the server's data directory, with verified restore.">
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
                  <th>Connection</th><th>Database</th><th>Server version</th><th>Status</th><th className="num">Size</th>
                  <th>SHA-256</th><th>Created</th><th />
                </tr>
              </thead>
              <tbody>
                {data.backups.map((b) => (
                  <tr key={b.id}>
                    <td>{b.connection_name}{b.note && <div className="muted">{b.note}</div>}</td>
                    <td>{b.database}</td>
                    <td className="truncate" style={{ maxWidth: 160 }} title={b.server_version}>{b.server_version}</td>
                    <td>
                      <StatusBadge status={b.status} />
                      {b.error && <div className="pre-line" style={{ color: "var(--danger)", maxWidth: 320 }}>{b.error}</div>}
                    </td>
                    <td className="num">{b.status === "completed" ? formatBytes(b.size_bytes) : "—"}</td>
                    <td className="mono" title={b.sha256}>{b.sha256 ? b.sha256.slice(0, 12) + "…" : "—"}</td>
                    <td className="nowrap muted">{formatTime(b.created_at)}<div>{b.created_by}</div></td>
                    <td>
                      <div className="row-actions">
                        {b.status === "completed" && (
                          <button className="btn small" onClick={() => verify(b)} disabled={verifying === b.id}>
                            {verifying === b.id ? "Verifying…" : "Verify"}
                          </button>
                        )}
                        {mayRestore && b.status === "completed" && <button className="btn small" onClick={() => setRestoring(b)}>Restore</button>}
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
                      <td>{r.target_name}</td>
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
