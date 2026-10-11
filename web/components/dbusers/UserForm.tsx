"use client";

import { useState, type FormEvent } from "react";
import { api, errorMessage } from "@/lib/api";
import type { AccountChange, Connection, DbAccount } from "@/lib/types";
import { Banner, Checkbox, Field, Modal } from "@/components/ui";
import { copyText } from "@/lib/desktop";

/** A random password of letters and digits that is safe to paste anywhere. */
function generatePassword(): string {
  const alphabet = "ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz23456789";
  const bytes = new Uint8Array(24);
  crypto.getRandomValues(bytes);
  return Array.from(bytes, (b) => alphabet[b % alphabet.length]).join("");
}

/** Creates a database user, or changes one. */
export default function UserForm({ connection, existing, roles, onClose, onSaved }: {
  connection: Connection; existing: DbAccount | null; roles: string[]; onClose: () => void; onSaved: (message: string) => void;
}) {
  const mysql = connection.kind === "mysql";
  const [name, setName] = useState(existing?.name ?? "");
  const [host, setHost] = useState(existing?.host ?? "%");
  const [password, setPassword] = useState("");
  const [canLogin, setCanLogin] = useState(existing?.can_login ?? true);
  const [limit, setLimit] = useState(existing && existing.connection_limit >= 0 ? String(existing.connection_limit) : "");
  const [member, setMember] = useState<string[]>(existing?.member_of ?? []);
  const [access, setAccess] = useState<AccountChange["access"]>("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  // A role cannot be granted to itself.
  const grantable = roles.filter((r) => r !== name);

  const submit = async (e: FormEvent) => {
    e.preventDefault();
    setBusy(true);
    setError("");
    const before = existing?.member_of ?? [];
    const wanted = limit.trim() === "" ? -1 : Number(limit);
    try {
      await api.saveDbAccount(connection.id, {
        create: !existing,
        name,
        host: mysql ? host : "",
        password: password || null,
        can_login: !existing || canLogin !== existing.can_login ? canLogin : null,
        connection_limit: !existing || wanted !== existing.connection_limit ? wanted : null,
        grant: member.filter((r) => !before.includes(r)),
        revoke: before.filter((r) => !member.includes(r)),
        access,
      });
      onSaved(existing ? `${name} was updated.` : `Database user ${name} was created${access ? ` with ${access === "readonly" ? "read-only" : "read and write"} access to ${connection.database}` : ""}.`);
      onClose();
    } catch (err) {
      setError(errorMessage(err));
      setBusy(false);
    }
  };

  const toggle = (role: string) => setMember((m) => (m.includes(role) ? m.filter((r) => r !== role) : [...m, role]));

  return (
    <Modal title={existing ? `Edit ${existing.name}` : "New database user"} onClose={onClose} footer={
      <>
        <button className="btn" type="button" onClick={onClose} disabled={busy}>Cancel</button>
        <button className="btn primary" type="submit" form="dbuser-form" disabled={busy}>{busy ? "Saving…" : existing ? "Save changes" : "Create user"}</button>
      </>
    }>
      <form id="dbuser-form" onSubmit={submit}>
        {error && <Banner tone="error"><pre>{error}</pre></Banner>}
        <p className="hint" style={{ marginBottom: 12 }}>
          An account on the database server behind {connection.name}, not a Dedie sign-in. The server decides whether{" "}
          {connection.username} may manage users.
        </p>
        <div className="form-row">
          <Field label="User name">
            <input className="input mono" value={name} onChange={(e) => setName(e.target.value)} required disabled={!!existing} maxLength={63} autoFocus={!existing} spellCheck={false} />
          </Field>
          {mysql ? (
            <Field label="Allowed from host" hint="% means any host.">
              <input className="input mono" value={host} onChange={(e) => setHost(e.target.value)} required disabled={!!existing} spellCheck={false} />
            </Field>
          ) : (
            <Field label="Connection limit" hint="Most connections at once. Empty for no limit.">
              <input className="input" type="number" min={0} value={limit} onChange={(e) => setLimit(e.target.value)} placeholder="No limit" />
            </Field>
          )}
        </div>
        {mysql && (
          <Field label="Connection limit" hint="Most connections at once. Empty for no limit.">
            <input className="input" type="number" min={0} value={limit} onChange={(e) => setLimit(e.target.value)} placeholder="No limit" />
          </Field>
        )}
        <Field label={existing ? "New password" : "Password"}
          hint={existing ? "Leave empty to keep the current password." : "At least 8 characters. It is sent to the database once and not stored by Dedie: copy it now."}>
          <div className="input-group">
            <input className="input mono" value={password} onChange={(e) => setPassword(e.target.value)} required={!existing && canLogin}
              minLength={password ? 8 : undefined} autoComplete="off" spellCheck={false} />
            <button className="btn" type="button" onClick={() => setPassword(generatePassword())}>Generate</button>
            {password && <button className="btn" type="button" onClick={() => copyText(password)}>Copy</button>}
          </div>
        </Field>
        <Checkbox label="May sign in" checked={canLogin} onChange={setCanLogin}
          hint={mysql ? "Off locks the account." : "Off makes this a role that only groups privileges for other users."} />
        <Field label={`Access to ${connection.database}`}
          hint={access === "" ? (existing ? "Leaves the user's privileges as they are." : "The user is created without privileges on any table; grant them yourself or pick a level.")
            : mysql ? "Applies to every table in this database, including ones created later."
              : "Applies to every table in every schema of this database now, and to tables this connection's account creates later. Tables created by other accounts need their own grant."}>
          <select className="select" value={access} onChange={(e) => setAccess(e.target.value as AccountChange["access"])}>
            <option value="">{existing ? "No change" : "None"}</option>
            <option value="readonly">Read only (SELECT)</option>
            <option value="readwrite">Read and write (SELECT, INSERT, UPDATE, DELETE)</option>
          </select>
        </Field>
        {grantable.length > 0 && (
          <Field label="Member of roles" hint="The user gains everything these roles are allowed to do.">
            <div className="picker">
              <div className="picker-list" style={{ maxHeight: 150 }}>
                {grantable.map((r) => (
                  <label key={r} className="picker-item">
                    <input type="checkbox" checked={member.includes(r)} onChange={() => toggle(r)} />
                    <span className="mono">{r}</span>
                  </label>
                ))}
              </div>
            </div>
          </Field>
        )}
      </form>
    </Modal>
  );
}
