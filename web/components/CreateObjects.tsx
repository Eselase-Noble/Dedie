"use client";

import { useEffect, useState, type FormEvent } from "react";
import { api, errorMessage } from "@/lib/api";
import type { Catalog, ColumnDef, Connection } from "@/lib/types";
import { Banner, Checkbox, Field, Modal } from "./ui";

/** Common column types offered in the table designer, per database kind. */
const TYPES: Record<string, string[]> = {
  postgres: ["integer", "bigint", "numeric(12,2)", "text", "varchar(255)", "boolean", "date", "timestamp", "timestamptz", "uuid", "jsonb", "bytea", "double precision"],
  mysql: ["int", "bigint", "decimal(12,2)", "varchar(255)", "text", "tinyint(1)", "date", "datetime", "timestamp", "json", "blob", "double"],
  oracle: ["NUMBER(10)", "NUMBER(19)", "NUMBER(12,2)", "VARCHAR2(255)", "CLOB", "DATE", "TIMESTAMP", "TIMESTAMP WITH TIME ZONE", "RAW(16)", "BLOB", "BINARY_DOUBLE"],
};

type Kind = "database" | "schema" | "table";

const blankColumn = (kind: string): ColumnDef => ({
  name: "", type: (TYPES[kind] ?? TYPES.postgres)[0], nullable: true, primary_key: false, default: "",
});

/**
 * Dialog for creating a database, a schema or a table on a connection's
 * server. Whether the database account is allowed to is decided by the
 * server; its answer is shown as it comes back.
 */
export default function CreateObject({ what, connection, catalog, canAddConnection, onClose, onCreated }: {
  what: Kind;
  connection: Connection;
  catalog: Catalog | null;
  /** Whether the user may save connections, which the database dialog offers. */
  canAddConnection: boolean;
  onClose: () => void;
  onCreated: (message: string) => void;
}) {
  const types = TYPES[connection.kind] ?? TYPES.postgres;
  const schemas = catalog?.schemas.map((s) => s.name) ?? [];
  const [name, setName] = useState("");
  const [addConnection, setAddConnection] = useState(canAddConnection);
  const [schema, setSchema] = useState(
    connection.kind === "mysql" ? connection.database
      // An Oracle account creates tables in the schema that bears its name.
      : connection.kind === "oracle" ? schemas.find((s) => s === connection.username.toUpperCase()) ?? schemas[0] ?? ""
        : schemas.includes("public") ? "public" : schemas[0] ?? "public",
  );
  const [columns, setColumns] = useState<ColumnDef[]>([
    { name: "id", type: connection.kind === "oracle" ? "NUMBER(19)" : "bigint", nullable: false, primary_key: true, default: "" },
    blankColumn(connection.kind),
  ]);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  // A PostgreSQL schema or table is created inside one database. It
  // defaults to the connection's own, and any other database on the same
  // server can be chosen.
  const pickDatabase = connection.kind === "postgres" && what !== "database";
  const [database, setDatabase] = useState(connection.database);
  const [databases, setDatabases] = useState<string[]>([connection.database]);
  const elsewhere = pickDatabase && database !== connection.database;

  useEffect(() => {
    if (!pickDatabase) return;
    api.serverObjects(connection.id)
      .then((objects) => setDatabases(objects.databases.map((d) => d.name)))
      .catch(() => setDatabases([connection.database]));
  }, [pickDatabase, connection.id, connection.database]);

  const setColumn = (i: number, patch: Partial<ColumnDef>) =>
    setColumns((cols) => cols.map((c, j) => (j === i ? { ...c, ...patch } : c)));

  const submit = async (e: FormEvent) => {
    e.preventDefault();
    setBusy(true);
    setError("");
    try {
      if (what === "database") {
        const res = await api.createDatabase(connection.id, name, addConnection);
        onCreated(res.connection
          ? `Database ${name} created. A connection named "${res.connection.name}" was added for it.`
          : `Database ${name} created.`);
      } else if (what === "schema") {
        await api.createSchema(connection.id, name, elsewhere ? database : "");
        onCreated(`Schema ${name} created in database ${database}.`);
      } else {
        const cols = columns.filter((c) => c.name.trim() !== "");
        await api.createTable(connection.id, { schema, name, columns: cols }, elsewhere ? database : "");
        onCreated(pickDatabase ? `Table ${schema}.${name} created in database ${database}.` : `Table ${schema}.${name} created.`);
      }
    } catch (err) {
      setError(errorMessage(err));
      setBusy(false);
    }
  };

  return (
    <Modal
      wide={what === "table"}
      title={`New ${what}`}
      onClose={onClose}
      footer={
        <>
          <button className="btn" type="button" onClick={onClose} disabled={busy}>Cancel</button>
          <button className="btn primary" type="submit" form="create-object" disabled={busy || !name.trim()}>
            {busy ? "Creating…" : `Create ${what}`}
          </button>
        </>
      }
    >
      <form id="create-object" onSubmit={submit}>
        {error && <Banner tone="error"><pre>{error}</pre></Banner>}
        <p className="hint" style={{ marginBottom: 12 }}>
          On {connection.name} ({connection.host}), as {connection.username}. The server decides whether this account may
          create a {what}.
        </p>

        {pickDatabase && (
          <Field label="Database" hint={elsewhere
            ? `Another database on the same server, reached with the same account. This connection stays on ${connection.database}.`
            : `The ${what} is created in this database. Choose another to create it elsewhere on the same server.`}>
            <select className="select" value={database} onChange={(e) => setDatabase(e.target.value)}>
              {databases.map((d) => <option key={d} value={d}>{d}{d === connection.database ? " (this connection)" : ""}</option>)}
            </select>
          </Field>
        )}
        {what === "table" && (
          <Field label={connection.kind === "mysql" ? "Database" : "Schema"}>
            {elsewhere ? (
              // The schema list on screen belongs to the connection's own
              // database, so for another one the name is typed.
              <input className="input" value={schema} onChange={(e) => setSchema(e.target.value)} required placeholder="public" />
            ) : (
              <select className="select" value={schema} onChange={(e) => setSchema(e.target.value)}>
                {(schemas.length ? schemas : [schema]).map((s) => <option key={s} value={s}>{s}</option>)}
              </select>
            )}
          </Field>
        )}
        <Field label={`${what[0].toUpperCase()}${what.slice(1)} name`}>
          <input className="input" value={name} onChange={(e) => setName(e.target.value)} required autoFocus maxLength={63} />
        </Field>

        {what === "database" && canAddConnection && (
          <Checkbox label="Also add a connection for the new database" checked={addConnection} onChange={setAddConnection}
            hint="Uses the same server and credentials, so you can query the new database straight away." />
        )}

        {what === "table" && (
          <>
            <div className="label" style={{ margin: "6px 0" }}>Columns</div>
            <div className="table-wrap">
              <table className="data column-editor">
                <thead>
                  <tr><th>Name</th><th>Type</th><th>Default</th><th>Nullable</th><th>Primary key</th><th /></tr>
                </thead>
                <tbody>
                  {columns.map((c, i) => (
                    <tr key={i}>
                      <td><input className="input" value={c.name} onChange={(e) => setColumn(i, { name: e.target.value })} placeholder="column_name" maxLength={63} /></td>
                      <td>
                        <input className="input" list="column-types" value={c.type} onChange={(e) => setColumn(i, { type: e.target.value })} />
                      </td>
                      <td><input className="input" value={c.default} onChange={(e) => setColumn(i, { default: e.target.value })} placeholder="none" /></td>
                      <td className="center">
                        <input type="checkbox" checked={c.nullable && !c.primary_key} disabled={c.primary_key}
                          onChange={(e) => setColumn(i, { nullable: e.target.checked })} aria-label="Nullable" />
                      </td>
                      <td className="center">
                        <input type="checkbox" checked={c.primary_key} onChange={(e) => setColumn(i, { primary_key: e.target.checked })} aria-label="Primary key" />
                      </td>
                      <td>
                        <button type="button" className="btn small ghost" disabled={columns.length === 1}
                          onClick={() => setColumns((cols) => cols.filter((_, j) => j !== i))}>Remove</button>
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
            <datalist id="column-types">{types.map((t) => <option key={t} value={t} />)}</datalist>
            <button type="button" className="btn small" style={{ marginTop: 8 }}
              onClick={() => setColumns((cols) => [...cols, blankColumn(connection.kind)])}>Add column</button>
            <p className="hint" style={{ marginTop: 10 }}>
              Pick a type from the list or type your own. A default is taken as a number or keyword
              (such as CURRENT_TIMESTAMP) when it is one, otherwise as text. Rows without a name are ignored.
            </p>
          </>
        )}
      </form>
    </Modal>
  );
}
