"use client";

import { useEffect, useMemo, useState, type FormEvent } from "react";
import { api, errorMessage } from "@/lib/api";
import type { Catalog, Connection, OwnerChange, ServerObjects } from "@/lib/types";
import { Banner, Field, Loading, Modal } from "@/components/ui";

type Kind = OwnerChange["kind"];

/**
 * Hands a database, a schema or a table to another role. The server
 * decides whether the connecting account may do it: a superuser always
 * can, and an owner can give away what it owns to a role it belongs to.
 */
export default function OwnershipDialog({ connection, catalog, onClose, onChanged }: {
  connection: Connection; catalog: Catalog | null; onClose: () => void; onChanged: (message: string) => void;
}) {
  const [objects, setObjects] = useState<ServerObjects | null>(null);
  const [kind, setKind] = useState<Kind>("table");
  const [database, setDatabase] = useState(connection.database);
  const [schema, setSchema] = useState("");
  const [table, setTable] = useState("");
  const [owner, setOwner] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");

  useEffect(() => {
    api.serverObjects(connection.id).then(setObjects).catch((err) => setError(errorMessage(err)));
  }, [connection.id]);

  const schemas = catalog?.schemas ?? [];
  const tables = useMemo(() => {
    const s = schemas.find((x) => x.name === schema);
    return s ? [...s.tables, ...s.views] : [];
  }, [schemas, schema]);

  useEffect(() => {
    if (!schema && schemas.length > 0) setSchema((schemas.find((s) => s.name === "public") ?? schemas[0]).name);
  }, [schemas, schema]);
  useEffect(() => {
    if (!tables.some((t) => t.name === table)) setTable(tables[0]?.name ?? "");
  }, [tables, table]);

  const name = kind === "database" ? database : kind === "schema" ? schema : table;
  const current = kind === "database" ? objects?.databases.find((d) => d.name === database)?.owner
    : kind === "schema" ? schemas.find((s) => s.name === schema)?.owner
    : tables.find((t) => t.name === table)?.owner;
  const label = kind === "table" ? `${schema}.${table}` : name;

  const submit = async (e: FormEvent) => {
    e.preventDefault();
    setBusy(true);
    setError("");
    try {
      await api.setOwner(connection.id, { kind, schema: kind === "table" ? schema : "", name, owner });
      onChanged(`${kind[0].toUpperCase()}${kind.slice(1)} ${label} is now owned by ${owner}.`);
      onClose();
    } catch (err) {
      setError(errorMessage(err));
      setBusy(false);
    }
  };

  return (
    <Modal title="Change owner" onClose={onClose} footer={
      <>
        <button className="btn" type="button" onClick={onClose} disabled={busy}>Cancel</button>
        <button className="btn primary" type="submit" form="owner-form" disabled={busy || !name || !owner || owner === current}>
          {busy ? "Changing…" : "Change owner"}
        </button>
      </>
    }>
      {error && <Banner tone="error"><pre>{error}</pre></Banner>}
      {!objects ? (!error && <Loading />) : (
        <form id="owner-form" onSubmit={submit}>
          <p className="hint" style={{ marginBottom: 12 }}>
            On {connection.name}, as {connection.username}. The owner of an object can change and drop it and decides who
            else may use it. Schemas and tables listed here are those of {connection.database}.
          </p>
          <Field label="Object">
            <div className="segmented">
              {(["database", "schema", "table"] as Kind[]).map((k) => (
                <button key={k} type="button" className={kind === k ? "active" : ""} onClick={() => setKind(k)}>
                  {k[0].toUpperCase() + k.slice(1)}
                </button>
              ))}
            </div>
          </Field>
          {kind === "database" && (
            <Field label="Database">
              <select className="select" value={database} onChange={(e) => setDatabase(e.target.value)}>
                {objects.databases.map((d) => <option key={d.name} value={d.name}>{d.name}</option>)}
              </select>
            </Field>
          )}
          {kind !== "database" && (
            <Field label="Schema">
              <select className="select" value={schema} onChange={(e) => setSchema(e.target.value)}>
                {schemas.map((s) => <option key={s.name} value={s.name}>{s.name}</option>)}
              </select>
            </Field>
          )}
          {kind === "table" && (
            <Field label="Table or view" hint="A table's indexes and the sequences its columns own move with it.">
              <select className="select" value={table} onChange={(e) => setTable(e.target.value)}>
                {tables.length === 0 && <option value="">No tables in this schema</option>}
                {tables.map((t) => <option key={t.name} value={t.name}>{t.name}</option>)}
              </select>
            </Field>
          )}
          <div className="form-row">
            <Field label="Current owner">
              <input className="input" value={current ?? "—"} readOnly disabled />
            </Field>
            <Field label="New owner">
              <select className="select" value={owner} onChange={(e) => setOwner(e.target.value)} required>
                <option value="">Select a role</option>
                {objects.roles.map((r) => <option key={r} value={r}>{r}</option>)}
              </select>
            </Field>
          </div>
          {kind === "schema" && (
            <p className="hint">Changing a schema&apos;s owner does not change who owns the tables inside it; change those separately.</p>
          )}
        </form>
      )}
    </Modal>
  );
}
