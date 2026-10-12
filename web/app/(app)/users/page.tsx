"use client";

import { useState, type FormEvent } from "react";
import { api, errorMessage } from "@/lib/api";
import { formatTime } from "@/lib/format";
import { useLoad } from "@/lib/hooks";
import { ROLES, type Role, type User } from "@/lib/types";
import { useSession } from "@/components/session";
import SecurityPolicyCard from "@/components/SecurityPolicyCard";
import { Badge, Banner, Field, Loading, Modal, PageHeader } from "@/components/ui";

// The organization's policy may ask for more; the server has the final say.
const MIN_PASSWORD = 8;

const ROLE_HELP: Record<Role, string> = {
  owner: "Everything, including managing other owners.",
  admin: "Everything except changing owners.",
  operator: "Connections, queries with writes, migrations, backups and server activity.",
  analyst: "Read-only queries; can view connections and migrations.",
  viewer: "Can view connections and migrations only.",
};

function CreateUser({ roles, onClose, onDone }: { roles: Role[]; onClose: () => void; onDone: () => void }) {
  const [name, setName] = useState("");
  const [email, setEmail] = useState("");
  const [password, setPassword] = useState("");
  const [role, setRole] = useState<Role>("analyst");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");

  const submit = async (e: FormEvent) => {
    e.preventDefault();
    setBusy(true);
    setError("");
    try {
      await api.createUser({ name, email, password, role });
      onDone();
      onClose();
    } catch (err) {
      setError(errorMessage(err));
      setBusy(false);
    }
  };

  return (
    <Modal title="New user" onClose={onClose} footer={
      <>
        <button className="btn" type="button" onClick={onClose} disabled={busy}>Cancel</button>
        <button className="btn primary" type="submit" form="user-form" disabled={busy}>{busy ? "Creating…" : "Create user"}</button>
      </>
    }>
      <form id="user-form" onSubmit={submit}>
        {error && <Banner tone="error">{error}</Banner>}
        <Field label="Name"><input className="input" value={name} onChange={(e) => setName(e.target.value)} required autoFocus /></Field>
        <Field label="Email"><input className="input" type="email" value={email} onChange={(e) => setEmail(e.target.value)} required autoComplete="off" /></Field>
        <Field label="Initial password" hint="Must meet the security policy below. Share it over a secure channel; the user can change it on their Account page.">
          <input className="input" type="password" value={password} onChange={(e) => setPassword(e.target.value)} required minLength={MIN_PASSWORD} autoComplete="new-password" />
        </Field>
        <Field label="Role" hint={ROLE_HELP[role]}>
          <select className="select" value={role} onChange={(e) => setRole(e.target.value as Role)}>
            {roles.map((r) => <option key={r} value={r}>{r}</option>)}
          </select>
        </Field>
      </form>
    </Modal>
  );
}

function ResetPassword({ user, onClose }: { user: User; onClose: () => void }) {
  const [password, setPassword] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");

  const submit = async (e: FormEvent) => {
    e.preventDefault();
    setBusy(true);
    setError("");
    try {
      await api.updateUser(user.id, { password });
      onClose();
    } catch (err) {
      setError(errorMessage(err));
      setBusy(false);
    }
  };

  return (
    <Modal title={`Reset password for ${user.name}`} onClose={onClose} footer={
      <>
        <button className="btn" type="button" onClick={onClose} disabled={busy}>Cancel</button>
        <button className="btn primary" type="submit" form="reset-form" disabled={busy}>{busy ? "Saving…" : "Set password"}</button>
      </>
    }>
      <form id="reset-form" onSubmit={submit}>
        {error && <Banner tone="error">{error}</Banner>}
        <p className="hint" style={{ marginBottom: 12 }}>Setting a new password signs the user out of all their sessions and revokes their API tokens.</p>
        <Field label="New password" hint="Must meet the organization's security policy.">
          <input className="input" type="password" value={password} onChange={(e) => setPassword(e.target.value)} required minLength={MIN_PASSWORD} autoComplete="new-password" autoFocus />
        </Field>
      </form>
    </Modal>
  );
}

export default function UsersPage() {
  const { session } = useSession();
  const { data, error, loading, reload } = useLoad(() => api.users(), []);
  const [creating, setCreating] = useState(false);
  const [resetting, setResetting] = useState<User | null>(null);
  const [actionError, setActionError] = useState("");
  const isOwner = session.user.role === "owner";
  // Only owners may grant the owner role or change an owner.
  const assignable = ROLES.filter((r) => isOwner || r !== "owner");

  const update = async (user: User, change: { role?: Role; disabled?: boolean; reset_totp?: boolean }) => {
    setActionError("");
    try {
      await api.updateUser(user.id, change);
    } catch (err) {
      setActionError(errorMessage(err));
    }
    await reload();
  };

  return (
    <div className="content">
      <PageHeader title="Dedie users" subtitle="People who sign in to Dedie and what their role allows. Accounts on the databases themselves are under Database users.">
        <button className="btn primary" onClick={() => setCreating(true)}>New user</button>
      </PageHeader>
      {error && <Banner tone="error">{error}</Banner>}
      {actionError && <Banner tone="error">{actionError}</Banner>}

      <div className="card">
        <div className="card-body flush table-wrap">
          {loading ? <Loading /> : (
            <table className="data stack">
              <thead>
                <tr><th>Name</th><th>Email</th><th>Role</th><th>Status</th><th>Two-factor</th><th>Created</th><th /></tr>
              </thead>
              <tbody>
                {data?.map((u) => {
                  const self = u.id === session.user.id;
                  const locked = u.role === "owner" && !isOwner;
                  return (
                    <tr key={u.id}>
                      <td><strong>{u.name}</strong>{self && <span className="muted"> (you)</span>}</td>
                      <td>{u.email}</td>
                      <td>
                        <select className="select" style={{ width: 130 }} value={u.role} disabled={locked}
                          title={ROLE_HELP[u.role]} onChange={(e) => update(u, { role: e.target.value as Role })}>
                          {(locked ? ROLES : assignable).map((r) => <option key={r} value={r}>{r}</option>)}
                        </select>
                      </td>
                      <td><Badge tone={u.disabled ? "danger" : "ok"}>{u.disabled ? "disabled" : "active"}</Badge></td>
                      <td><Badge tone={u.totp_enabled ? "ok" : ""}>{u.totp_enabled ? "on" : "off"}</Badge></td>
                      <td className="nowrap muted">{formatTime(u.created_at)}</td>
                      <td>
                        <div className="row-actions">
                          {u.totp_enabled && !self && (
                            <button className="btn small" disabled={locked} onClick={() => update(u, { reset_totp: true })}
                              title="For a user who has lost their authenticator and recovery codes. They sign in with their password and set it up again.">
                              Reset two-factor
                            </button>
                          )}
                          <button className="btn small" disabled={locked} onClick={() => setResetting(u)}>Reset password</button>
                          <button className="btn small" disabled={locked || self} onClick={() => update(u, { disabled: !u.disabled })}>
                            {u.disabled ? "Enable" : "Disable"}
                          </button>
                        </div>
                      </td>
                    </tr>
                  );
                })}
              </tbody>
            </table>
          )}
        </div>
      </div>

      <SecurityPolicyCard />

      <div className="card">
        <div className="card-head"><h2>Roles</h2></div>
        <div className="card-body flush">
          <table className="data stack">
            <tbody>
              {ROLES.map((r) => <tr key={r}><td style={{ width: 120 }}><strong>{r}</strong></td><td>{ROLE_HELP[r]}</td></tr>)}
            </tbody>
          </table>
        </div>
      </div>

      {creating && <CreateUser roles={assignable} onClose={() => setCreating(false)} onDone={reload} />}
      {resetting && <ResetPassword user={resetting} onClose={() => setResetting(null)} />}
    </div>
  );
}
