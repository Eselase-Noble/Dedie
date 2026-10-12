"use client";

import { useState, type FormEvent } from "react";
import { api, errorMessage } from "@/lib/api";
import { copyText } from "@/lib/desktop";
import { formatBytes, formatTime, isZeroTime } from "@/lib/format";
import type { Connection, WalArchive } from "@/lib/types";
import { ConnectionName } from "@/components/Environment";
import { Badge, Banner, ConfirmDialog, Field, Modal } from "@/components/ui";

/** Switches log archiving on for a connection. */
function Enable({ archive, onClose, onDone }: { archive: WalArchive; onClose: () => void; onDone: () => void }) {
  const [days, setDays] = useState(archive.retention_days || 14);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const submit = async (e: FormEvent) => {
    e.preventDefault();
    setBusy(true);
    setError("");
    try {
      await api.setWalArchive(archive.connection_id, true, days);
      onDone();
      onClose();
    } catch (err) {
      setError(errorMessage(err));
      setBusy(false);
    }
  };
  return (
    <Modal title={`Point-in-time recovery for ${archive.connection_name}`} onClose={onClose} footer={
      <>
        <button className="btn" type="button" onClick={onClose} disabled={busy}>Cancel</button>
        <button className="btn primary" type="submit" form="wal-form" disabled={busy}>{busy ? "Saving…" : archive.enabled ? "Save" : "Start archiving"}</button>
      </>
    }>
      <form id="wal-form" onSubmit={submit}>
        {error && <Banner tone="error">{error}</Banner>}
        <p style={{ marginBottom: 10 }}>
          Dedie receives the server&apos;s write-ahead log continuously, as it is written. Together with a physical
          backup, that log can rebuild the server as it was at any moment after the backup: just before a bad
          deployment, or a second before a table was dropped.
        </p>
        <ul className="plain">
          <li>The connection&apos;s account needs the <code>REPLICATION</code> attribute.</li>
          <li>The log is kept in <code>{archive.dir}</code> on the machine that runs Dedie. It is not encrypted by Dedie; keep that disk protected.</li>
          <li>A replication slot stops the server discarding log Dedie has not received. If Dedie is stopped for long, the server keeps that log on its own disk until Dedie returns, so do not leave it stopped while archiving is on.</li>
          <li>After switching this on, take a <strong>physical</strong> backup. Recovery reaches moments after that backup, not before.</li>
        </ul>
        <Field label="Keep the log for (days)" hint="Older log is deleted. 0 keeps everything. Keep it at least as long as the physical backups you would recover from.">
          <input className="input" type="number" min={0} value={days} onChange={(e) => setDays(Number(e.target.value))} style={{ maxWidth: 160 }} />
        </Field>
      </form>
    </Modal>
  );
}

/** Shows the command that rebuilds a server as it was at a moment. */
function HowTo({ archive, onClose }: { archive: WalArchive; onClose: () => void }) {
  const local = (d: Date) => new Date(d.getTime() - d.getTimezoneOffset() * 60000).toISOString().slice(0, 19);
  const [moment, setMoment] = useState(local(new Date(Date.now() - 5 * 60000)));
  const command = `dedie --local backup recover ${archive.connection_name} --to "${moment.replace("T", " ")}" --dir /var/lib/postgresql/recovered`;
  return (
    <Modal wide title={`Recover ${archive.connection_name} to a moment`} onClose={onClose} footer={
      <>
        <button className="btn" onClick={() => copyText(command)}>Copy command</button>
        <button className="btn primary" onClick={onClose}>Close</button>
      </>
    }>
      <p style={{ marginBottom: 10 }}>
        Recovery builds a <em>new</em> server beside the original; the original is not touched. It is done on the
        machine that runs Dedie, where the log archive is.
      </p>
      <Field label="Moment to recover to" hint={`Any moment after ${formatTime(archive.earliest_recovery)}, in the time zone of the machine that runs Dedie.`}>
        <input className="input" type="datetime-local" step={1} value={moment} onChange={(e) => setMoment(e.target.value)} style={{ maxWidth: 260 }} />
      </Field>
      <div className="label" style={{ marginBottom: 6 }}>1. Build the data directory</div>
      <pre className="cell-value">{command}</pre>
      <div className="label" style={{ margin: "12px 0 6px" }}>2. Start PostgreSQL of the same major version on it, on a free port</div>
      <pre className="cell-value">pg_ctl -D /var/lib/postgresql/recovered -o &quot;-p 5433&quot; start</pre>
      <p className="hint" style={{ marginTop: 10 }}>
        The server replays the log up to that moment and then opens for use. Check the data, then copy out what you
        need or point your application at it. Leave out <code>--to</code> to recover to the latest change received.
      </p>
    </Modal>
  );
}

/** Continuous log archiving and recovery to a moment, per PostgreSQL connection. */
export default function Recovery({ archives, connections, mayManage, reload }: {
  archives: WalArchive[]; connections: Connection[]; mayManage: boolean; reload: () => Promise<void>;
}) {
  const [enabling, setEnabling] = useState<WalArchive | null>(null);
  const [stopping, setStopping] = useState<WalArchive | null>(null);
  const [howTo, setHowTo] = useState<WalArchive | null>(null);
  if (archives.length === 0) return null;
  return (
    <div className="card">
      <div className="card-head"><h2>Point-in-time recovery</h2><span className="muted">PostgreSQL</span></div>
      <div className="card-body flush table-wrap">
        <table className="data stack">
          <thead><tr><th>Connection</th><th>Log archiving</th><th className="num">Archive size</th><th>Last received</th><th>Can recover to any moment since</th><th /></tr></thead>
          <tbody>
            {archives.map((w) => (
              <tr key={w.connection_id}>
                <td><ConnectionName connection={connections.find((c) => c.id === w.connection_id)} name={w.connection_name} strong /></td>
                <td>
                  {!w.enabled ? <Badge>off</Badge> : w.receiving ? <Badge tone="ok">receiving</Badge> : <Badge tone="warn">not receiving</Badge>}
                  {w.enabled && w.retention_days > 0 && <span className="muted"> · kept {w.retention_days} days</span>}
                  {w.last_error && <div className="pre-line" style={{ color: "var(--danger)", maxWidth: 420 }}>{w.last_error}</div>}
                </td>
                <td className="num">{w.segments > 0 ? formatBytes(w.size_bytes) : "—"}</td>
                <td className="nowrap muted">{formatTime(w.newest_at)}</td>
                <td>
                  {!w.enabled ? <span className="faint">—</span>
                    : isZeroTime(w.earliest_recovery) ? <span className="muted">Take a physical backup to open the recovery window</span>
                    : <span className="nowrap">{formatTime(w.earliest_recovery)}</span>}
                </td>
                <td>
                  <div className="row-actions">
                    {w.enabled && !isZeroTime(w.earliest_recovery) && <button className="btn small" onClick={() => setHowTo(w)}>Recover…</button>}
                    {mayManage && <button className="btn small" onClick={() => setEnabling(w)}>{w.enabled ? "Settings" : "Turn on"}</button>}
                    {mayManage && w.enabled && <button className="btn small" onClick={() => setStopping(w)}>Turn off</button>}
                  </div>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
      {enabling && <Enable archive={enabling} onClose={() => setEnabling(null)} onDone={() => void reload()} />}
      {howTo && <HowTo archive={howTo} onClose={() => setHowTo(null)} />}
      {stopping && (
        <ConfirmDialog title={`Stop archiving ${stopping.connection_name}?`} confirmLabel="Turn off" danger onClose={() => setStopping(null)}
          onConfirm={async () => { await api.setWalArchive(stopping.connection_id, false, stopping.retention_days); await reload(); }}>
          <p>
            Dedie stops receiving the log and drops its replication slot on the server. The log already received is
            kept, so moments up to now stay recoverable, but nothing after this. Turning it on again starts a new
            recovery window that needs a new physical backup.
          </p>
        </ConfirmDialog>
      )}
    </div>
  );
}
