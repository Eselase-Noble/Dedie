"use client";

import { useState, type FormEvent } from "react";
import { api, errorMessage } from "@/lib/api";
import type { BackupLocations, Connection } from "@/lib/types";
import { Banner, Field, Modal } from "@/components/ui";
import BackupOptions, { DEFAULT_CHOICE, type BackupChoice } from "./BackupOptions";

export default function CreateBackup({ connections, locations, onClose, onDone }: {
  connections: Connection[]; locations: BackupLocations | null; onClose: () => void; onDone: () => void;
}) {
  const candidates = connections.filter((c) => c.kind === "postgres" || c.kind === "mysql");
  const [connId, setConnId] = useState(candidates[0]?.id ?? "");
  const [choice, setChoice] = useState<BackupChoice>(DEFAULT_CHOICE);
  const [note, setNote] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const connection = candidates.find((c) => c.id === connId) ?? null;

  const chooseConnection = (next: string) => {
    setConnId(next);
    // What is possible depends on the kind of database; start again.
    setChoice({ ...DEFAULT_CHOICE, locationId: choice.locationId, plain: choice.plain });
  };

  const submit = async (e: FormEvent) => {
    e.preventDefault();
    setBusy(true);
    setError("");
    try {
      await api.createBackup({
        connection_id: connId, note, type: choice.type, level: choice.level,
        tables: choice.type === "tables" ? choice.tables : [], location_id: choice.locationId, plain: choice.plain,
      });
      onDone();
      onClose();
    } catch (err) {
      setError(errorMessage(err));
      setBusy(false);
    }
  };

  const incomplete = !connId || (choice.type === "tables" && choice.tables.length === 0);

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
        <BackupOptions connection={connection} locations={locations} value={choice} onChange={setChoice} />
        <Field label="Note (optional)">
          <input className="input" value={note} onChange={(e) => setNote(e.target.value)} placeholder="Before the 4.2 release" maxLength={200} />
        </Field>
        <p className="hint">The database stays online while the backup is taken from a consistent point.</p>
      </form>
    </Modal>
  );
}
