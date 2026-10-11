"use client";

import { useState, type FormEvent } from "react";
import { api, errorMessage } from "@/lib/api";
import type { ColumnSpec, Connection, Migration, Validation, OnExisting } from "@/lib/types";
import { Banner, Checkbox, Field, Modal } from "./ui";

/** Splits a comma or newline separated list, dropping blanks. */
function parseList(text: string): string[] {
  return text.split(/[,\n]/).map((s) => s.trim()).filter(Boolean);
}

const COLUMN_TYPES = ["text", "bigint", "double precision", "numeric", "boolean", "timestamptz", "jsonb"];

/**
 * Reads the columns box: one "collection field type" per line. The type
 * comes last and may be two words (double precision).
 */
function parseColumns(text: string): Record<string, ColumnSpec[]> {
  const out: Record<string, ColumnSpec[]> = {};
  for (const line of text.split("\n")) {
    const words = line.trim().split(/\s+/);
    if (words.length < 3 || !words[0]) continue;
    const [collection, field, ...type] = words;
    (out[collection] ??= []).push({ field, type: type.join(" ").toLowerCase() });
  }
  return out;
}

function formatColumns(columns: Record<string, ColumnSpec[]>): string {
  return Object.entries(columns).flatMap(([collection, list]) => list.map((c) => `${collection} ${c.field} ${c.type}`)).join("\n");
}

/** Which kinds of target each kind of source can be migrated to. */
const TARGET_KINDS: Record<string, string[]> = {
  postgres: ["postgres", "mysql"],
  mysql: ["mysql", "postgres"],
  mongodb: ["postgres"],
  oracle: ["oracle", "postgres", "mysql"],
};

const SOURCE_HINTS: Record<string, string> = {
  postgres: "Stays online. Needs wal_level = logical and a role with REPLICATION.",
  mysql: "Stays online. Needs binary logging in ROW format and an account with RELOAD, REPLICATION SLAVE and REPLICATION CLIENT.",
  mongodb: "Stays online. Must be a replica set member, and the account must be able to read the database and open a change stream on it.",
  oracle: "Stays online. Needs supplemental logging and an account that may use LogMiner (LOGMINING, SELECT ANY TRANSACTION, EXECUTE_CATALOG_ROLE, SELECT ANY DICTIONARY).",
};

/** What an offline copy needs of its source: far less. */
const OFFLINE_HINTS: Record<string, string> = {
  postgres: "Read from one snapshot. Needs only a role that can read the tables; no replication settings.",
  mysql: "Read from one snapshot. Needs only SELECT (and RELOAD to copy with several workers); no binary log.",
  mongodb: "Each collection is read once. Works on a standalone server; keep the source still while it runs.",
  oracle: "Read as of one moment with flashback queries. Needs only an account that can read the tables; no LogMiner.",
};

export default function MigrationForm({ connections, onClose, onCreated, initialMode = "migrate" }: {
  connections: Connection[]; onClose: () => void; onCreated: (job: Migration) => void;
  initialMode?: "migrate" | "sync" | "offline";
}) {
  // Migrations run between two databases of the same kind, or across the
  // two engines in either direction.
  const candidates = connections.filter((c) => c.kind in TARGET_KINDS);
  const [name, setName] = useState("");
  const [mode, setMode] = useState<"migrate" | "sync" | "offline">(initialMode);
  const [onExisting, setOnExisting] = useState<OnExisting>("fail");
  const [sourceId, setSourceId] = useState((candidates.find((c) => c.kind !== "mongodb") ?? candidates[0])?.id ?? "");
  const source = candidates.find((c) => c.id === sourceId);
  const targets = candidates.filter((c) => c.id !== sourceId && (TARGET_KINDS[source?.kind ?? ""] ?? []).includes(c.kind));
  const [chosenTarget, setTargetId] = useState("");
  // Until one is chosen, or when the source changes kind, offer a target
  // of the source's own engine first: a version upgrade is the common case,
  // and moving between engines should be a deliberate choice.
  const targetId = targets.some((c) => c.id === chosenTarget) ? chosenTarget
    : (targets.find((c) => c.kind === source?.kind) ?? targets[0])?.id ?? "";
  const mysql = source?.kind === "mysql";
  const mongo = source?.kind === "mongodb";
  const sync = mode === "sync";
  const offline = mode === "offline";
  const oracle = source?.kind === "oracle";
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
  const [columns, setColumns] = useState("");
  const [suggesting, setSuggesting] = useState(false);

  const suggest = async () => {
    setSuggesting(true);
    setError("");
    try {
      setColumns(formatColumns(await api.suggestColumns(sourceId, scope === "table" ? parseList(tables) : [], parseList(exclude))));
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setSuggesting(false);
    }
  };

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
          mode,
          columns: mongo ? parseColumns(columns) : undefined,
          schemas: scope === "schema" && !mysql && !mongo ? parseList(schemas) : [],
          tables: scope === "table" ? parseList(tables) : [],
          exclude_tables: parseList(exclude),
          snapshot_workers: workers,
          fence_source: fence && !sync && !offline && !mongo && !oracle,
          validation,
          keep_slot: keepSlot && !offline && !mysql && !mongo,
          on_existing: onExisting,
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
      title={sync ? "New continuous sync" : offline ? "New offline migration" : "New migration"}
      onClose={onClose}
      footer={
        <>
          <button className="btn" type="button" onClick={onClose} disabled={busy}>Cancel</button>
          <button className="btn primary" type="submit" form="migration-form" disabled={busy || !targetId}>
            {busy ? "Creating…" : sync ? "Create sync" : "Create migration"}
          </button>
        </>
      }
    >
      <form id="migration-form" onSubmit={submit}>
        {error && <Banner tone="error">{error}</Banner>}
        {!targetId && (
          <Banner tone="warn">A migration needs a source and an empty target. PostgreSQL and MySQL can each be migrated to either engine, Oracle to a newer Oracle, PostgreSQL or MySQL, and MongoDB to PostgreSQL.</Banner>
        )}
        {mongo && (
          <Banner tone="info">
            Each collection becomes a PostgreSQL table with two columns: <code>_id</code> (text, primary key) and{" "}
            <code>doc</code> (jsonb) holding the whole document, so nothing is lost or reinterpreted. Fields are read in
            SQL with <code>doc-&gt;&gt;&apos;field&apos;</code>. MongoDB indexes, validators and views are not translated.
          </Banner>
        )}
        {crossEngine && !mongo && (
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
          <Field label="Purpose" className="span2" hint={sync
            ? "The target is filled, then kept in step with every change on the source for as long as the sync runs. One direction only: do not write to the migrated tables on the target. Finish it whenever the target should stand on its own."
            : offline
              ? "The source is copied once, as it stands at one moment, then validated; the job finishes by itself. Changes made to the source after the copy starts are not carried over, so stop application writes first. The source needs no replication set up."
              : "The target is filled and kept in step until you cut over, at which point it is validated and becomes the database your applications use."}>
            <div className="segmented">
              <button type="button" className={mode === "migrate" ? "active" : ""} onClick={() => setMode("migrate")}>Online: migrate and cut over</button>
              <button type="button" className={offline ? "active" : ""} onClick={() => setMode("offline")}>Offline: copy once</button>
              <button type="button" className={sync ? "active" : ""} onClick={() => setMode("sync")}>Keep in sync continuously</button>
            </div>
          </Field>
          <Field label="Name" className="span2">
            <input className="input" value={name} onChange={(e) => setName(e.target.value)} required autoFocus
              placeholder="orders database: 13 to 17" />
          </Field>
          <Field label="Source" hint={(offline ? OFFLINE_HINTS : SOURCE_HINTS)[source?.kind ?? "postgres"]}>
            <select className="select" value={sourceId} onChange={(e) => setSourceId(e.target.value)} required>
              {candidates.map((c) => <option key={c.id} value={c.id}>{c.name} ({c.kind}, {c.database})</option>)}
            </select>
          </Field>
          <Field label="Target" hint="Best empty. What happens to objects it already has is chosen below.">
            <select className="select" value={targetId} onChange={(e) => setTargetId(e.target.value)} required>
              {targets.map((c) => <option key={c.id} value={c.id}>{c.name} ({c.kind}, {c.database})</option>)}
            </select>
          </Field>
          <Field label="Level" className="span2"
            hint={scope === "database"
              ? oracle ? "Every table of the connecting user's own schema is migrated. Choose the schema level to name other schemas."
                : "Everything in the source connection's database is migrated."
              : scope === "schema"
                ? "Everything in the schemas you list is migrated."
                : mongo ? "Only the collections you list are migrated."
                  : "Only the tables you list are migrated, with their indexes, constraints and triggers. Views and other standalone objects are left out."}>
            <select className="select" value={(mysql || mongo) && scope === "schema" ? "database" : scope}
              onChange={(e) => setScope(e.target.value as "database" | "schema" | "table")}>
              <option value="database">Database: everything</option>
              {!mysql && !mongo && <option value="schema">Schema: selected schemas</option>}
              <option value="table">{mongo ? "Collection: selected collections" : "Table: selected tables"}</option>
            </select>
          </Field>
          {scope === "schema" && !mysql && !mongo && (
            <Field label="Schemas" hint="Comma separated.">
              <input className="input" value={schemas} onChange={(e) => setSchemas(e.target.value)} placeholder={oracle ? "HR, SALES" : "public, billing"} required />
            </Field>
          )}
          {scope === "table" && (
            <Field label={mongo ? "Collections" : "Tables"} hint={mongo ? "Comma separated collection names." : mysql ? "Comma separated table names." : "Comma separated schema.table names."}>
              <input className="input" value={tables} onChange={(e) => setTables(e.target.value)}
                placeholder={mysql || mongo ? "orders, order_items" : oracle ? "HR.EMPLOYEES, HR.DEPARTMENTS" : "public.orders, public.order_items"} required />
            </Field>
          )}
          <Field label={mongo ? "Excluded collections" : "Excluded tables"} hint={mongo ? "Comma separated collection names." : mysql ? "Comma separated table names. The source connection's database is migrated." : "Comma separated schema.table names."}>
            <input className="input" value={exclude} onChange={(e) => setExclude(e.target.value)} placeholder={mysql || mongo ? "sessions, audit_trail" : oracle ? "HR.JOB_HISTORY" : "public.sessions"} />
          </Field>
          {mongo && (
            <Field label="Fields to lift into columns (optional)" className="span2"
              hint={<>One per line: <code>collection field type</code>. Use dots for nested fields (<code>address.city</code>). Types: {COLUMN_TYPES.join(", ")}. The column is filled from the field and kept current; a value of another type is stored as NULL there, and the whole document is always kept in <code>doc</code>.</>}>
              <textarea className="textarea mono" rows={5} value={columns} onChange={(e) => setColumns(e.target.value)} spellCheck={false}
                placeholder={"users email text\nusers address.city text\norders total numeric\norders created timestamptz"} />
              <div><button className="btn small" type="button" onClick={suggest} disabled={suggesting || !sourceId}>
                {suggesting ? "Sampling…" : "Suggest from a sample"}
              </button></div>
            </Field>
          )}
          <Field label="Snapshot workers" hint="Tables copied in parallel during the initial copy (1 to 16).">
            <input className="input" type="number" min={1} max={16} value={workers} onChange={(e) => setWorkers(Number(e.target.value))} />
          </Field>
          <Field label={sync ? "Validation when finished" : offline ? "Validation after the copy" : "Validation at cutover"} hint={mongo ? "Checksum compares the set of document keys; count compares totals only." : oracle ? "Checksum compares row counts and the set of primary keys; count compares totals only." : "Checksum compares every row; count compares row totals only."}>
            <select className="select" value={validation} onChange={(e) => setValidation(e.target.value as Validation)}>
              <option value="checksum">checksum</option>
              <option value="count">count</option>
              <option value="none">none</option>
            </select>
          </Field>
          <Field label="If the target already has some of the objects" className="span2"
            hint={onExisting === "fail"
              ? "Preflight lists them and the migration does not start until you decide. Nothing on the target is touched."
              : onExisting === "skip"
                ? "Each is left exactly as it is and is not migrated. An existing table receives no rows and changes to its source are not followed."
                : "Each is dropped on the target and created again from the source. A dropped table loses the data it holds there. Preflight shows exactly what would be dropped before anything is."}>
            <select className="select" value={onExisting} onChange={(e) => setOnExisting(e.target.value as OnExisting)}>
              <option value="fail">Refuse, and ask me</option>
              <option value="skip">Keep them and skip them</option>
              <option value="replace">Replace them</option>
            </select>
          </Field>
          {!offline && (
            <Field label="Cutover timeout (seconds)" hint="How long cutover waits for the stream to drain before giving up.">
              <input className="input" type="number" min={10} value={timeout} onChange={(e) => setTimeoutSeconds(Number(e.target.value))} />
            </Field>
          )}
          <div className="span2">
            {!sync && !offline && !mongo && !oracle && <Checkbox
              label="Fence the source at cutover"
              checked={fence}
              onChange={setFence}
              hint={mysql ? "At cutover the source database is made read-only (needs MySQL 8.0.22 or newer), so no write can land after the final sync. It stays read-only once the migration completes. Leave this off if you will stop application writes yourself." : "At cutover the source database is made read-only and its client sessions are disconnected, so no write can land after the final sync. The source stays read-only once the migration completes. Leave this off if you will stop application writes yourself."}
            />}
            {!offline && !mysql && !mongo && <Checkbox
              label={oracle ? "Keep primary key logging after completion" : "Keep the replication slot after completion"}
              checked={keepSlot}
              onChange={setKeepSlot}
              hint={oracle
                ? "The job switches on primary key supplemental logging for the tables it follows and switches it off again when done. Tick this to leave it on, for example for another tool that reads the redo log."
                : "The source retains WAL for a kept slot until you drop it. Leave off unless you have a reason to keep it."}
            />}
          </div>
        </div>
      </form>
    </Modal>
  );
}
