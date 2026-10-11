"use client";

import { useState, type FormEvent } from "react";
import { api, errorMessage } from "@/lib/api";
import { formatTime } from "@/lib/format";
import type { Backup, Connection } from "@/lib/types";
import { Banner, Checkbox, Field, Modal } from "@/components/ui";
import { typeLabel } from "./options";

export default function RestoreDialog({ backup, connections, onClose, onDone }: {
  backup: Backup; connections: Connection[]; onClose: () => void; onDone: () => void;
}) {
  const candidates = connections.filter((c) => c.kind === backup.kind && !c.read_only);
  const [targetId, setTargetId] = useState("");
  const [where, setWhere] = useState<"existing" | "new">("existing");
  const [newDatabase, setNewDatabase] = useState("");
  const [onlyTables, setOnlyTables] = useState("");
  const [clean, setClean] = useState(false);
  const [keepOwners, setKeepOwners] = useState(false);
  const [confirm, setConfirm] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");

  const target = candidates.find((c) => c.id === targetId) ?? null;
  const postgres = backup.kind === "postgres";
  const database = where === "new" ? newDatabase.trim() : target?.database ?? "";
  // A restore into a database that was just created cannot overwrite
  // anything, so it needs no typed confirmation.
  const confirmed = where === "new" ? database !== "" : confirm === database;

  const submit = async (e: FormEvent) => {
    e.preventDefault();
    setBusy(true);
    setError("");
    try {
      await api.restoreBackup(backup.id, {
        target_id: targetId,
        new_database: where === "new" ? database : "",
        tables: onlyTables.split(",").map((t) => t.trim()).filter(Boolean),
        clean: where === "existing" && clean,
        keep_owners: keepOwners,
        confirm: database,
      });
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
        <button className={`btn ${where === "new" ? "primary" : "danger"}`} type="submit" form="restore-form" disabled={busy || !target || !confirmed}>
          {busy ? "Starting…" : "Restore"}
        </button>
      </>
    }>
      <form id="restore-form" onSubmit={submit}>
        {error && <Banner tone="error">{error}</Banner>}
        <p style={{ marginBottom: 12 }}>
          Restoring the <strong>{typeLabel(backup.type).toLowerCase()}</strong> backup of <strong>{backup.database}</strong> from{" "}
          {backup.connection_name}, taken {formatTime(backup.created_at)}. The file is verified first.
          {postgres
            ? " The restore runs in a single transaction: if it fails, the target is left unchanged."
            : " A MySQL restore cannot be rolled back: if it fails part-way, the tables already replaced stay replaced."}
        </p>
        {backup.level !== "full" && (
          <Banner tone="info">
            This is {backup.level === "incremental" ? "an incremental" : "a differential"} backup. The full backup it
            builds on{backup.level === "incremental" ? ", and every backup between," : ""} is restored first, then this
            one, in order. The target database must be named <strong>{backup.database}</strong>, as the source was.
          </Banner>
        )}
        {backup.type === "data" && (
          <Banner tone="warn">This backup holds rows only. The target must already have the tables, and they should be empty.</Banner>
        )}
        <Field label="Target server" hint="Read-only connections cannot be restore targets.">
          <select className="select" value={targetId} onChange={(e) => { setTargetId(e.target.value); setConfirm(""); }} required>
            <option value="">Select a connection</option>
            {candidates.map((c) => <option key={c.id} value={c.id}>{c.name} ({c.database} on {c.host})</option>)}
          </select>
        </Field>
        {backup.level === "full" && <Field label="Restore into">
          <div className="segmented">
            <button type="button" className={where === "existing" ? "active" : ""} onClick={() => setWhere("existing")}>
              The connection&apos;s database
            </button>
            <button type="button" className={where === "new" ? "active" : ""} onClick={() => setWhere("new")}>
              A new database
            </button>
          </div>
        </Field>}
        {where === "new" && (
          <Field label="New database name" hint="Created on the target server, then restored into. Nothing existing is touched, which makes this the safe way to inspect a backup or recover a single table.">
            <input className="input" value={newDatabase} onChange={(e) => setNewDatabase(e.target.value)} placeholder={`${backup.database}_restored`} autoComplete="off" spellCheck={false} required />
          </Field>
        )}
        {postgres && backup.type !== "tables" && backup.level === "full" && (
          <Field label="Only these tables (optional)" hint="Comma-separated, as schema.table. Leave empty to restore everything in the backup.">
            <input className="input" value={onlyTables} onChange={(e) => setOnlyTables(e.target.value)} placeholder="public.orders, public.customers" spellCheck={false} />
          </Field>
        )}
        {postgres && where === "existing" && (
          <Checkbox label="Replace existing objects (clean)" checked={clean} onChange={setClean}
            hint="Drops each object in the target before recreating it from the backup. Data in those objects is lost. Without this, the restore fails if an object already exists." />
        )}
        {postgres && (
          <Checkbox label="Restore ownership and privileges" checked={keepOwners} onChange={setKeepOwners}
            hint="Requires the same roles to exist on the target server. Leave off to have everything owned by the connecting user." />
        )}
        {target && where === "existing" && (
          <Field label={`Type the target database name (${target.database}) to confirm`}>
            <input className="input" value={confirm} onChange={(e) => setConfirm(e.target.value)} autoComplete="off" spellCheck={false} />
          </Field>
        )}
      </form>
    </Modal>
  );
}
