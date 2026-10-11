"use client";

import { useState, type FormEvent } from "react";
import { api, errorMessage } from "@/lib/api";
import type { Connection, Migration, Validation } from "@/lib/types";
import { Banner, Checkbox, Field, Modal } from "./ui";

/** Splits a comma or newline separated list, dropping blanks. */
function parseList(text: string): string[] {
  return text.split(/[,\n]/).map((s) => s.trim()).filter(Boolean);
}

/** Which kinds of target each kind of source can be migrated to. */
const TARGET_KINDS: Record<string, string[]> = {
  postgres: ["postgres", "mysql"],
  mysql: ["mysql", "postgres"],
};

export default function MigrationForm({ connections, onClose, onCreated }: {
  connections: Connection[]; onClose: () => void; onCreated: (job: Migration) => void;
}) {
  // Migrations run between two databases of the same kind, or across the
  // two engines in either direction.
  const candidates = connections.filter((c) => c.kind === "postgres" || c.kind === "mysql");
  const [name, setName] = useState("");
  const [sourceId, setSourceId] = useState(candidates[0]?.id ?? "");
  const source = candidates.find((c) => c.id === sourceId);
  const targets = candidates.filter((c) => c.id !== sourceId && (TARGET_KINDS[source?.kind ?? ""] ?? []).includes(c.kind));
  const [chosenTarget, setTargetId] = useState("");
  // Fall back to the first valid target when the source changes kind.
  const targetId = targets.some((c) => c.id === chosenTarget) ? chosenTarget : targets[0]?.id ?? "";
  const mysql = source?.kind === "mysql";
  const crossEngine = !!source && !!targetId && candidates.find((c) => c.id === targetId)?.kind !== source.kind;
  // What to migrate: the whole database, selected schemas, or selected tables.
  const [scope, setScope] = useState<"database" | "schema" | "table">("database");
  const [schemas, setSchemas] = useState("");
  const [tables, setTables] = useState("");
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
          schemas: scope === "schema" && !mysql ? parseList(schemas) : [],
          tables: scope === "table" ? parseList(tables) : [],
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
          <button className="btn primary" type="submit" form="migration-form" disabled={busy || !targetId}>
            {busy ? "Creating…" : "Create migration"}
          </button>
        </>
      }
    >
      <form id="migration-form" onSubmit={submit}>
        {error && <Banner tone="error">{error}</Banner>}
        {!targetId && (
          <Banner tone="warn">A migration needs a source and an empty target. PostgreSQL and MySQL can each be migrated to either engine.</Banner>
        )}
        {crossEngine && (
          <Banner tone="info">
            Moving between engines translates the schema: tables, keys, indexes and foreign keys are recreated
            with equivalent types on the target. Views, routines and triggers are not translated. The preflight
            check lists every difference before anything is changed.
          </Banner>
        )}
        <p className="hint" style={{ marginBottom: 12 }}>
          Creating a migration changes nothing on either database. Run the preflight check, then start it.
        </p>
        <div className="form-grid">
          <Field label="Name" className="span2">
            <input className="input" value={name} onChange={(e) => setName(e.target.value)} required autoFocus
              placeholder="orders database: 13 to 17" />
          </Field>
          <Field label="Source" hint={mysql ? "Stays online. Needs binary logging in ROW format and an account with RELOAD, REPLICATION SLAVE and REPLICATION CLIENT." : "Stays online. Needs wal_level = logical and a role with REPLICATION."}>
            <select className="select" value={sourceId} onChange={(e) => setSourceId(e.target.value)} required>
              {candidates.map((c) => <option key={c.id} value={c.id}>{c.name} ({c.kind}, {c.database})</option>)}
            </select>
          </Field>
          <Field label="Target" hint="Must not already contain the tables being migrated.">
            <select className="select" value={targetId} onChange={(e) => setTargetId(e.target.value)} required>
              {targets.map((c) => <option key={c.id} value={c.id}>{c.name} ({c.kind}, {c.database})</option>)}
            </select>
          </Field>
          <Field label="Level" className="span2"
            hint={scope === "database"
              ? "Everything in the source connection's database is migrated."
              : scope === "schema"
                ? "Everything in the schemas you list is migrated."
                : "Only the tables you list are migrated, with their indexes, constraints and triggers. Views and other standalone objects are left out."}>
            <select className="select" value={mysql && scope === "schema" ? "database" : scope}
              onChange={(e) => setScope(e.target.value as "database" | "schema" | "table")}>
              <option value="database">Database: everything</option>
              {!mysql && <option value="schema">Schema: selected schemas</option>}
              <option value="table">Table: selected tables</option>
            </select>
          </Field>
          {scope === "schema" && !mysql && (
            <Field label="Schemas" hint="Comma separated.">
              <input className="input" value={schemas} onChange={(e) => setSchemas(e.target.value)} placeholder="public, billing" required />
            </Field>
          )}
          {scope === "table" && (
            <Field label="Tables" hint={mysql ? "Comma separated table names." : "Comma separated schema.table names."}>
              <input className="input" value={tables} onChange={(e) => setTables(e.target.value)}
                placeholder={mysql ? "orders, order_items" : "public.orders, public.order_items"} required />
            </Field>
          )}
          <Field label="Excluded tables" hint={mysql ? "Comma separated table names. The source connection's database is migrated." : "Comma separated schema.table names."}>
            <input className="input" value={exclude} onChange={(e) => setExclude(e.target.value)} placeholder={mysql ? "sessions, audit_trail" : "public.sessions"} />
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
              hint={mysql ? "At cutover the source database is made read-only (needs MySQL 8.0.22 or newer), so no write can land after the final sync. It stays read-only once the migration completes. Leave this off if you will stop application writes yourself." : "At cutover the source database is made read-only and its client sessions are disconnected, so no write can land after the final sync. The source stays read-only once the migration completes. Leave this off if you will stop application writes yourself."}
            />
            {!mysql && <Checkbox
              label="Keep the replication slot after completion"
              checked={keepSlot}
              onChange={setKeepSlot}
              hint="The source retains WAL for a kept slot until you drop it. Leave off unless you have a reason to keep it."
            />}
          </div>
        </div>
      </form>
    </Modal>
  );
}
