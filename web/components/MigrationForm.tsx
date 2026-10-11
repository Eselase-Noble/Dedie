"use client";

import { useState, type FormEvent } from "react";
import { api, errorMessage } from "@/lib/api";
import type { Connection, Migration, Validation } from "@/lib/types";
import { Banner, Checkbox, Field, Modal } from "./ui";

/** Splits a comma or newline separated list, dropping blanks. */
function parseList(text: string): string[] {
  return text.split(/[,\n]/).map((s) => s.trim()).filter(Boolean);
}

export default function MigrationForm({ connections, onClose, onCreated }: {
  connections: Connection[]; onClose: () => void; onCreated: (job: Migration) => void;
}) {
  // Only PostgreSQL to PostgreSQL migrations exist in this version.
  const candidates = connections.filter((c) => c.kind === "postgres");
  const [name, setName] = useState("");
  const [sourceId, setSourceId] = useState(candidates[0]?.id ?? "");
  const [targetId, setTargetId] = useState(candidates[1]?.id ?? "");
  const [schemas, setSchemas] = useState("");
  const [exclude, setExclude] = useState("");
  const [workers, setWorkers] = useState(4);
  const [validation, setValidation] = useState<Validation>("checksum");
  const [fence, setFence] = useState(false);
  const [keepSlot, setKeepSlot] = useState(false);
  const [timeout, setTimeoutSeconds] = useState(300);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");

  const submit = async (e: FormEvent) => {
    e.preventDefault();
    setBusy(true);
    setError("");
    try {
      const job = await api.createMigration({
        name,
        source_id: sourceId,
        target_id: targetId,
        config: {
          schemas: parseList(schemas),
          exclude_tables: parseList(exclude),
          snapshot_workers: workers,
          fence_source: fence,
          validation,
          keep_slot: keepSlot,
          cutover_timeout_seconds: timeout,
        },
      });
      onCreated(job);
    } catch (err) {
      setError(errorMessage(err));
      setBusy(false);
    }
  };

  return (
    <Modal
      wide
      title="New migration"
      onClose={onClose}
      footer={
        <>
          <button className="btn" type="button" onClick={onClose} disabled={busy}>Cancel</button>
          <button className="btn primary" type="submit" form="migration-form" disabled={busy || candidates.length < 2}>
            {busy ? "Creating…" : "Create migration"}
          </button>
        </>
      }
    >
      <form id="migration-form" onSubmit={submit}>
        {error && <Banner tone="error">{error}</Banner>}
        {candidates.length < 2 && (
          <Banner tone="warn">A migration needs two PostgreSQL connections: a source and an empty target.</Banner>
        )}
        <p className="hint" style={{ marginBottom: 12 }}>
          Creating a migration changes nothing on either database. Run the preflight check, then start it.
        </p>
        <div className="form-grid">
          <Field label="Name" className="span2">
            <input className="input" value={name} onChange={(e) => setName(e.target.value)} required autoFocus
              placeholder="orders database: 13 to 17" />
          </Field>
          <Field label="Source" hint="Stays online. Needs wal_level = logical and a role with REPLICATION.">
            <select className="select" value={sourceId} onChange={(e) => setSourceId(e.target.value)} required>
              {candidates.map((c) => <option key={c.id} value={c.id}>{c.name} ({c.database})</option>)}
            </select>
          </Field>
          <Field label="Target" hint="Must not already contain the tables being migrated.">
            <select className="select" value={targetId} onChange={(e) => setTargetId(e.target.value)} required>
              {candidates.map((c) => <option key={c.id} value={c.id}>{c.name} ({c.database})</option>)}
            </select>
          </Field>
          <Field label="Schemas" hint="Comma separated. Leave empty to migrate every schema.">
            <input className="input" value={schemas} onChange={(e) => setSchemas(e.target.value)} placeholder="public, billing" />
          </Field>
          <Field label="Excluded tables" hint="Comma separated schema.table names.">
            <input className="input" value={exclude} onChange={(e) => setExclude(e.target.value)} placeholder="public.sessions" />
          </Field>
          <Field label="Snapshot workers" hint="Tables copied in parallel during the initial copy (1 to 16).">
            <input className="input" type="number" min={1} max={16} value={workers} onChange={(e) => setWorkers(Number(e.target.value))} />
          </Field>
          <Field label="Validation at cutover" hint="Checksum compares every row; count compares row totals only.">
            <select className="select" value={validation} onChange={(e) => setValidation(e.target.value as Validation)}>
              <option value="checksum">checksum</option>
              <option value="count">count</option>
              <option value="none">none</option>
            </select>
          </Field>
          <Field label="Cutover timeout (seconds)" hint="How long cutover waits for the stream to drain before giving up.">
            <input className="input" type="number" min={10} value={timeout} onChange={(e) => setTimeoutSeconds(Number(e.target.value))} />
          </Field>
          <div className="span2">
            <Checkbox
              label="Fence the source at cutover"
              checked={fence}
              onChange={setFence}
              hint="At cutover the source database is made read-only and its client sessions are disconnected, so no write can land after the final sync. The source stays read-only once the migration completes. Leave this off if you will stop application writes yourself."
            />
            <Checkbox
              label="Keep the replication slot after completion"
              checked={keepSlot}
              onChange={setKeepSlot}
              hint="The source retains WAL for a kept slot until you drop it. Leave off unless you have a reason to keep it."
            />
          </div>
        </div>
      </form>
    </Modal>
  );
}
