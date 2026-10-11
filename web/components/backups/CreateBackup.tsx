"use client";

import { useState, type FormEvent } from "react";
import { api, errorMessage } from "@/lib/api";
import type { BackupType, Connection } from "@/lib/types";
import { Banner, Field, Modal } from "@/components/ui";
import TablePicker from "./TablePicker";
import { typeOptions } from "./options";

export default function CreateBackup({ connections, onClose, onDone }: {
  connections: Connection[]; onClose: () => void; onDone: () => void;
}) {
  const candidates = connections.filter((c) => c.kind === "postgres" || c.kind === "mysql");
  const [connId, setConnId] = useState(candidates[0]?.id ?? "");
  const [type, setType] = useState<BackupType>("full");
  const [tables, setTables] = useState<string[]>([]);
  const [note, setNote] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");

  const connection = candidates.find((c) => c.id === connId) ?? null;
  const options = typeOptions(connection?.kind ?? "");
  const current = options.find((o) => o.value === type) ?? options[0];

  const chooseConnection = (next: string) => {
    setConnId(next);
    setTables([]);
    // Physical backups exist for PostgreSQL only.
    const kind = candidates.find((c) => c.id === next)?.kind ?? "";
    if (!typeOptions(kind).some((o) => o.value === type)) setType("full");
  };

  const submit = async (e: FormEvent) => {
    e.preventDefault();
    setBusy(true);
    setError("");
    try {
      await api.createBackup({ connection_id: connId, note, type: current.value, tables: current.value === "tables" ? tables : [] });
      onDone();
      onClose();
    } catch (err) {
      setError(errorMessage(err));
      setBusy(false);
    }
  };

  const incomplete = !connId || (current.value === "tables" && tables.length === 0);

  return (
    <Modal title="Create backup" onClose={onClose} footer={
      <>
        <button className="btn" type="button" onClick={onClose} disabled={busy}>Cancel</button>
        <button className="btn primary" type="submit" form="backup-form" disabled={busy || incomplete}>
          {busy ? "Starting…" : "Start backup"}
        </button>
      </>
    }>
      <form id="backup-form" onSubmit={submit}>
        {error && <Banner tone="error">{error}</Banner>}
        <Field label="Connection">
          <select className="select" value={connId} onChange={(e) => chooseConnection(e.target.value)} required>
            {candidates.length === 0 && <option value="">No PostgreSQL or MySQL connections</option>}
            {candidates.map((c) => <option key={c.id} value={c.id}>{c.name} ({c.database})</option>)}
          </select>
        </Field>
        <Field label="What to back up" hint={current.description}>
          <select className="select" value={current.value} onChange={(e) => setType(e.target.value as BackupType)}>
            {options.map((o) => <option key={o.value} value={o.value}>{o.label}</option>)}
          </select>
        </Field>
        {current.value === "tables" && connection && (
          <Field label="Tables">
            <TablePicker connection={connection} selected={tables} onChange={setTables} />
          </Field>
        )}
        <Field label="Note (optional)">
          <input className="input" value={note} onChange={(e) => setNote(e.target.value)} placeholder="Before the 4.2 release" maxLength={200} />
        </Field>
        <p className="hint">
          The database stays online while the backup is taken from a consistent snapshot. The archive is compressed
          and encrypted before it is written to the server&apos;s data directory.
        </p>
      </form>
    </Modal>
  );
}
