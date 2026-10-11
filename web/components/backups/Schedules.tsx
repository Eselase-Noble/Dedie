"use client";

import { useState, type FormEvent } from "react";
import { api, errorMessage } from "@/lib/api";
import { formatTime } from "@/lib/format";
import type { BackupLocations, Connection, Schedule } from "@/lib/types";
import { Banner, Checkbox, ConfirmDialog, Field, Modal, StatusBadge } from "@/components/ui";
import BackupOptions, { type BackupChoice } from "./BackupOptions";
import { METHODS, levelLabel, methodOf, typeLabel } from "./options";

const PRESETS = [
  { label: "Every hour", cron: "0 * * * *" },
  { label: "Every 6 hours", cron: "0 */6 * * *" },
  { label: "Daily at 02:00", cron: "0 2 * * *" },
  { label: "Weekdays at 22:00", cron: "0 22 * * 1-5" },
  { label: "Weekly, Sunday 03:00", cron: "0 3 * * 0" },
  { label: "Monthly, 1st at 01:00", cron: "0 1 1 * *" },
];

function describe(cron: string): string {
  return PRESETS.find((p) => p.cron === cron)?.label ?? cron;
}

function ScheduleForm({ schedule, connections, locations, onClose, onDone }: {
  schedule: Schedule | null; connections: Connection[]; locations: BackupLocations | null; onClose: () => void; onDone: () => void;
}) {
  const candidates = connections.filter((c) => c.kind === "postgres" || c.kind === "mysql");
  const [name, setName] = useState(schedule?.name ?? "");
  const [connId, setConnId] = useState(schedule?.connection_id ?? candidates[0]?.id ?? "");
  const [choice, setChoice] = useState<BackupChoice>({
    type: schedule?.type ?? "full", level: schedule?.level ?? "full", tables: schedule?.tables ?? [],
    locationId: schedule?.location_id ?? "", plain: schedule ? !schedule.encrypted : false,
  });
  const [cron, setCron] = useState(schedule?.cron ?? "0 2 * * *");
  const [keepLast, setKeepLast] = useState(schedule?.keep_last ?? 7);
  const [keepDays, setKeepDays] = useState(schedule?.keep_days ?? 0);
  const [verify, setVerify] = useState(schedule?.verify ?? true);
  const [enabled, setEnabled] = useState(schedule?.enabled ?? true);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");

  const connection = candidates.find((c) => c.id === connId) ?? null;
  const preset = PRESETS.some((p) => p.cron === cron) ? cron : "custom";

  const submit = async (e: FormEvent) => {
    e.preventDefault();
    setBusy(true);
    setError("");
    try {
      await api.saveSchedule(schedule?.id ?? "", {
        name, connection_id: connId, type: choice.type, tables: choice.type === "tables" ? choice.tables : [],
        level: choice.level, location_id: choice.locationId, plain: choice.plain, cron,
        keep_last: keepLast, keep_days: keepDays, verify, enabled,
      });
      onDone();
      onClose();
    } catch (err) {
      setError(errorMessage(err));
      setBusy(false);
    }
  };

  return (
    <Modal title={schedule ? "Edit schedule" : "New backup schedule"} onClose={onClose} footer={
      <>
        <button className="btn" type="button" onClick={onClose} disabled={busy}>Cancel</button>
        <button className="btn primary" type="submit" form="schedule-form" disabled={busy || !connId || (choice.type === "tables" && choice.tables.length === 0)}>
          {busy ? "Saving…" : "Save schedule"}
        </button>
      </>
    }>
      <form id="schedule-form" onSubmit={submit}>
        {error && <Banner tone="error">{error}</Banner>}
        <Field label="Name">
          <input className="input" value={name} onChange={(e) => setName(e.target.value)} placeholder="Nightly orders database" required maxLength={80} />
        </Field>
        <Field label="Connection">
          <select className="select" value={connId} required
            onChange={(e) => { setConnId(e.target.value); setChoice({ ...choice, type: "full", level: "full", tables: [] }); }}>
            {candidates.map((c) => <option key={c.id} value={c.id}>{c.name} ({c.database})</option>)}
          </select>
        </Field>
        <BackupOptions connection={connection} locations={locations} value={choice} onChange={setChoice} />
        {choice.level !== "full" && (
          <Banner tone="info">
            A {choice.level} schedule builds on the newest full backup of this connection. Pair it with a schedule that
            takes full backups, for example weekly; if there is none yet, the first run takes a full backup itself.
            A backup that later ones build on is kept until they have expired.
          </Banner>
        )}
        <div className="form-row">
          <Field label="When">
            <select className="select" value={preset} onChange={(e) => e.target.value !== "custom" && setCron(e.target.value)}>
              {PRESETS.map((p) => <option key={p.cron} value={p.cron}>{p.label}</option>)}
              <option value="custom">Custom…</option>
            </select>
          </Field>
          <Field label="Cron expression" hint="minute hour day month weekday, in the server's time zone.">
            <input className="input mono" value={cron} onChange={(e) => setCron(e.target.value)} spellCheck={false} required />
          </Field>
        </div>
        <div className="form-row">
          <Field label="Keep the last" hint="Number of backups. 0 keeps all.">
            <input className="input" type="number" min={0} value={keepLast} onChange={(e) => setKeepLast(Number(e.target.value))} />
          </Field>
          <Field label="Delete after (days)" hint="0 never deletes by age.">
            <input className="input" type="number" min={0} value={keepDays} onChange={(e) => setKeepDays(Number(e.target.value))} />
          </Field>
        </div>
        <p className="hint" style={{ marginBottom: 10 }}>
          A backup is deleted as soon as it falls outside either limit. The newest backup is always kept.
        </p>
        <Checkbox label="Verify every backup" checked={verify} onChange={setVerify}
          hint="After each run, check the file against its checksum and confirm that it decrypts." />
        <Checkbox label="Enabled" checked={enabled} onChange={setEnabled} />
      </form>
    </Modal>
  );
}

/** The card that lists backup schedules and lets them be managed. */
export default function Schedules({ schedules, connections, locations, mayManage, reload }: {
  schedules: Schedule[]; connections: Connection[]; locations: BackupLocations | null; mayManage: boolean; reload: () => Promise<void>;
}) {
  const [editing, setEditing] = useState<Schedule | "new" | null>(null);
  const [deleting, setDeleting] = useState<Schedule | null>(null);
  const [error, setError] = useState("");
  const connName = (id: string) => connections.find((c) => c.id === id)?.name ?? "(deleted connection)";

  const runNow = async (s: Schedule) => {
    setError("");
    try {
      await api.runSchedule(s.id);
      await reload();
    } catch (err) {
      setError(errorMessage(err));
    }
  };

  return (
    <div className="card">
      <div className="card-head">
        <h2>Schedules</h2>
        {mayManage && <button className="btn small" onClick={() => setEditing("new")}>New schedule</button>}
      </div>
      <div className="card-body flush table-wrap">
        {error && <Banner tone="error">{error}</Banner>}
        {schedules.length === 0 ? (
          <div className="empty">No schedules. Create one to take backups automatically and delete old ones.</div>
        ) : (
          <table className="data">
            <thead>
              <tr><th>Name</th><th>Connection</th><th>Type</th><th>When</th><th>Retention</th><th>Last run</th><th>Next run</th><th /></tr>
            </thead>
            <tbody>
              {schedules.map((s) => (
                <tr key={s.id}>
                  <td>{s.name}{!s.enabled && <div className="muted">disabled</div>}</td>
                  <td>{connName(s.connection_id)}</td>
                  <td>{METHODS[methodOf(s.type)].label} · {levelLabel(s.level)}<div className="muted">{typeLabel(s.type)}</div></td>
                  <td title={s.cron}>{describe(s.cron)}</td>
                  <td>
                    {s.keep_last === 0 && s.keep_days === 0 ? "Keep all" : [
                      s.keep_last > 0 ? `last ${s.keep_last}` : "", s.keep_days > 0 ? `${s.keep_days} days` : "",
                    ].filter(Boolean).join(", ")}
                  </td>
                  <td>
                    <StatusBadge status={s.last_status} />
                    <div className="muted nowrap">{formatTime(s.last_run_at)}</div>
                    {s.last_error && <div className="pre-line" style={{ color: "var(--danger)", maxWidth: 320 }}>{s.last_error}</div>}
                  </td>
                  <td className="nowrap muted">{s.enabled ? formatTime(s.next_run_at) : "—"}</td>
                  <td>
                    <div className="row-actions">
                      <button className="btn small" onClick={() => runNow(s)} disabled={s.last_status === "running"}>Run now</button>
                      {mayManage && <button className="btn small" onClick={() => setEditing(s)}>Edit</button>}
                      {mayManage && <button className="btn small" onClick={() => setDeleting(s)}>Delete</button>}
                    </div>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </div>
      {editing && (
        <ScheduleForm schedule={editing === "new" ? null : editing} connections={connections} locations={locations}
          onClose={() => setEditing(null)} onDone={reload} />
      )}
      {deleting && (
        <ConfirmDialog title="Delete this schedule?" confirmLabel="Delete schedule" danger onClose={() => setDeleting(null)}
          onConfirm={async () => { await api.deleteSchedule(deleting.id); await reload(); }}>
          <p><strong>{deleting.name}</strong> stops running. The backups it has taken are kept and no longer expire.</p>
        </ConfirmDialog>
      )}
    </div>
  );
}
