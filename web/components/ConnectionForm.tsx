"use client";

import { useState, type FormEvent } from "react";
import { api, errorMessage } from "@/lib/api";
import type { Connection, ConnectionInput, ServerInfo } from "@/lib/types";
import { Banner, Checkbox, Field, Modal } from "./ui";

const SSL_MODES = ["disable", "allow", "prefer", "require", "verify-ca", "verify-full"];
const WEAK_SSL = ["disable", "allow", "prefer"];
const DEFAULT_PORTS: Record<string, number> = { postgres: 5432, mysql: 3306, mongodb: 27017, oracle: 1521 };
export const KIND_LABELS: Record<string, string> = { postgres: "PostgreSQL", mysql: "MySQL", mongodb: "MongoDB", oracle: "Oracle" };

export function ServerInfoSummary({ info }: { info: ServerInfo }) {
  return (
    <Banner tone={info.encrypted ? "ok" : "warn"}>
      Connected to <strong>{info.database}</strong> as <strong>{info.user}</strong> · {info.kind} {info.version} ·{" "}
      {info.encrypted ? "encrypted (TLS)" : "not encrypted"} · {info.superuser ? "superuser" : "regular role"}
    </Banner>
  );
}

/** Create or edit a saved connection. On edit, a blank password keeps the stored one. */
export default function ConnectionForm({ existing, kinds, onClose, onSaved }: {
  existing: Connection | null; kinds: string[]; onClose: () => void; onSaved: () => void;
}) {
  const [form, setForm] = useState<ConnectionInput>({
    name: existing?.name ?? "",
    kind: existing?.kind ?? (kinds.includes("postgres") ? "postgres" : kinds[0] ?? "postgres"),
    host: existing?.host ?? "",
    port: existing?.port ?? DEFAULT_PORTS[kinds.includes("postgres") ? "postgres" : kinds[0] ?? "postgres"] ?? 5432,
    database: existing?.database ?? "",
    username: existing?.username ?? "",
    password: "",
    sslmode: existing?.sslmode ?? "require",
    ssl_root_cert: existing?.ssl_root_cert ?? "",
    read_only: existing?.read_only ?? false,
  });
  const [busy, setBusy] = useState(false);
  const [testing, setTesting] = useState(false);
  const [error, setError] = useState("");
  const [info, setInfo] = useState<ServerInfo | null>(null);

  const set = <K extends keyof ConnectionInput>(key: K, value: ConnectionInput[K]) =>
    setForm((f) => ({ ...f, [key]: value }));

  const save = async (e: FormEvent) => {
    e.preventDefault();
    setBusy(true);
    setError("");
    try {
      if (existing) await api.updateConnection(existing.id, form);
      else await api.createConnection(form);
      onSaved();
      onClose();
    } catch (err) {
      setError(errorMessage(err));
      setBusy(false);
    }
  };

  // The unsaved settings can only be tested when a password is present;
  // otherwise an existing connection is tested with its stored secret.
  const test = async () => {
    setTesting(true);
    setError("");
    setInfo(null);
    try {
      setInfo(existing && !form.password ? await api.testConnection(existing.id) : await api.testConnectionInput(form));
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setTesting(false);
    }
  };

  return (
    <Modal
      wide
      title={existing ? `Edit ${existing.name}` : "New connection"}
      onClose={onClose}
      footer={
        <>
          <button className="btn" type="button" onClick={test} disabled={testing || busy}>
            {testing ? "Testing…" : "Test connection"}
          </button>
          <span className="spacer" />
          <button className="btn" type="button" onClick={onClose} disabled={busy}>Cancel</button>
          <button className="btn primary" type="submit" form="connection-form" disabled={busy}>
            {busy ? "Saving…" : "Save"}
          </button>
        </>
      }
    >
      <form id="connection-form" onSubmit={save}>
        {error && <Banner tone="error">{error}</Banner>}
        {info && <ServerInfoSummary info={info} />}
        {existing && !form.password && info && (
          <p className="hint">Tested with the saved settings. Enter the password to test unsaved changes.</p>
        )}
        <div className="form-grid">
          <Field label="Name">
            <input className="input" value={form.name} onChange={(e) => set("name", e.target.value)} required autoFocus />
          </Field>
          <Field label="Database type">
            <select className="select" value={form.kind} disabled={!!existing}
              onChange={(e) => setForm((f) => ({
                ...f, kind: e.target.value, port: DEFAULT_PORTS[e.target.value] ?? f.port,
                // An Oracle listener speaks TLS on a separate port, so the
                // usual default of requiring it would not connect.
                sslmode: e.target.value === "oracle" ? "disable" : f.sslmode === "disable" && f.kind === "oracle" ? "require" : f.sslmode,
              }))}>
              {kinds.map((k) => <option key={k} value={k}>{KIND_LABELS[k] ?? k}</option>)}
            </select>
          </Field>
          <Field label="Host">
            <input className="input" value={form.host} onChange={(e) => set("host", e.target.value)} required />
          </Field>
          <Field label="Port">
            <input className="input" type="number" min={1} max={65535} value={form.port}
              onChange={(e) => set("port", Number(e.target.value))} required />
          </Field>
          <Field label={form.kind === "oracle" ? "Service name" : "Database"}
            hint={form.kind === "oracle" ? "The service of the pluggable database, for example FREEPDB1 or ORCLPDB1; not the instance SID." : undefined}>
            <input className="input" value={form.database} onChange={(e) => set("database", e.target.value)} required />
          </Field>
          <Field label="Username" hint={form.kind === "mongodb" ? "The account is looked up in the admin database. Leave empty for a server without access control." : undefined}>
            <input className="input" value={form.username} onChange={(e) => set("username", e.target.value)} required={form.kind !== "mongodb"} autoComplete="off" />
          </Field>
          <Field label="Password" hint={existing ? "Leave blank to keep the stored password." : "Stored encrypted; never shown again."}>
            <input className="input" type="password" value={form.password} onChange={(e) => set("password", e.target.value)}
              autoComplete="new-password" />
          </Field>
          <Field label="SSL mode" hint={form.kind === "oracle" ? "Choose require or stricter only for a TCPS listener, and give its port." : undefined}>
            <select className="select" value={form.sslmode} onChange={(e) => set("sslmode", e.target.value)}>
              {SSL_MODES.map((m) => <option key={m} value={m}>{m}</option>)}
            </select>
          </Field>
          {WEAK_SSL.includes(form.sslmode) && (
            <div className="span2">
              <Banner tone="warn">
                SSL mode <strong>{form.sslmode}</strong> does not guarantee encryption. Credentials and data may cross
                the network in clear text.{" "}
                {form.kind === "oracle"
                  ? <>Outside a trusted local network, connect to a TCPS listener with <strong>require</strong> or stricter.</>
                  : <>Use <strong>require</strong> or stricter outside a trusted local network.</>}
              </Banner>
            </div>
          )}
          <Field className="span2" label="CA certificate (optional)" hint="PEM. Needed for verify-ca and verify-full when the server uses a private certificate authority.">
            <textarea className="textarea mono" rows={4} value={form.ssl_root_cert} spellCheck={false}
              onChange={(e) => set("ssl_root_cert", e.target.value)} placeholder="-----BEGIN CERTIFICATE-----" />
          </Field>
          <div className="span2">
            <Checkbox label="Read-only connection" checked={form.read_only} onChange={(v) => set("read_only", v)}
              hint="Queries always run in read-only transactions and the connection cannot be a restore target." />
          </div>
        </div>
      </form>
    </Modal>
  );
}
