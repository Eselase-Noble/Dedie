"use client";

import { useState, type FormEvent } from "react";
import { api, errorMessage } from "@/lib/api";
import { formatTime, isZeroTime } from "@/lib/format";
import { useLoad } from "@/lib/hooks";
import { ROLES, type ApiToken, type Role } from "@/lib/types";
import { useSession } from "@/components/session";
import { Banner, ConfirmDialog, Field, Loading, Modal } from "@/components/ui";
import { copyText } from "@/lib/desktop";

const RANK: Record<Role, number> = { viewer: 1, analyst: 2, operator: 3, admin: 4, owner: 5 };

function CreateToken({ onClose, onCreated }: { onClose: () => void; onCreated: (t: ApiToken) => void }) {
  const { session } = useSession();
  // A token can never do more than the person who creates it.
  const roles = ROLES.filter((r) => RANK[r] <= RANK[session.user.role]);
  const [name, setName] = useState("");
  const [role, setRole] = useState<Role>(session.user.role);
  const [days, setDays] = useState(90);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");

  const submit = async (e: FormEvent) => {
    e.preventDefault();
    setBusy(true);
    setError("");
    try {
      onCreated(await api.createToken({ name, role, days }));
      onClose();
    } catch (err) {
      setError(errorMessage(err));
      setBusy(false);
    }
  };

  return (
    <Modal title="New API token" onClose={onClose} footer={
      <>
        <button className="btn" type="button" onClick={onClose} disabled={busy}>Cancel</button>
        <button className="btn primary" type="submit" form="token-form" disabled={busy}>{busy ? "Creating…" : "Create token"}</button>
      </>
    }>
      <form id="token-form" onSubmit={submit}>
        {error && <Banner tone="error">{error}</Banner>}
        <Field label="Name" hint="What will use it, for example “CI pipeline” or “Ama's laptop”.">
          <input className="input" value={name} onChange={(e) => setName(e.target.value)} required maxLength={80} autoFocus />
        </Field>
        <div className="form-row">
          <Field label="Role" hint="Give a token the least it needs.">
            <select className="select" value={role} onChange={(e) => setRole(e.target.value as Role)}>
              {roles.map((r) => <option key={r} value={r}>{r}</option>)}
            </select>
          </Field>
          <Field label="Expires">
            <select className="select" value={days} onChange={(e) => setDays(Number(e.target.value))}>
              <option value={30}>In 30 days</option>
              <option value={90}>In 90 days</option>
              <option value={365}>In 1 year</option>
              <option value={0}>Never</option>
            </select>
          </Field>
        </div>
      </form>
    </Modal>
  );
}

export default function Tokens() {
  const { session, can } = useSession();
  const { data, error, loading, reload } = useLoad(() => api.tokens(), []);
  const [creating, setCreating] = useState(false);
  const [created, setCreated] = useState<ApiToken | null>(null);
  const [revoking, setRevoking] = useState<ApiToken | null>(null);
  const showOwner = can("users.manage");

  return (
    <div className="card">
      <div className="card-head">
        <h2>API tokens</h2>
        <button className="btn small" onClick={() => setCreating(true)}>New token</button>
      </div>
      <div className="card-body flush table-wrap">
        {error && <Banner tone="error">{error}</Banner>}
        <p className="hint" style={{ padding: "10px 14px 0" }}>
          For scripts, CI and the command line (<code>dedie login</code>). Send a token as{" "}
          <code>Authorization: Bearer &lt;token&gt;</code>. It acts as you, limited to its role.
        </p>
        {loading ? <Loading /> : !data || data.length === 0 ? <div className="empty">No API tokens.</div> : (
          <table className="data stack">
            <thead><tr><th>Name</th>{showOwner && <th>Owner</th>}<th>Role</th><th>Created</th><th>Last used</th><th>Expires</th><th /></tr></thead>
            <tbody>
              {data.map((t) => (
                <tr key={t.id}>
                  <td><strong>{t.name}</strong></td>
                  {showOwner && <td>{t.user_email}{t.user_id === session.user.id && <span className="muted"> (you)</span>}</td>}
                  <td>{t.role}</td>
                  <td className="nowrap muted">{formatTime(t.created_at)}</td>
                  <td className="nowrap muted">{isZeroTime(t.last_used_at) ? "Never" : formatTime(t.last_used_at)}</td>
                  <td className="nowrap muted">{isZeroTime(t.expires_at) ? "Never" : formatTime(t.expires_at)}</td>
                  <td><div className="row-actions"><button className="btn small" onClick={() => setRevoking(t)}>Revoke</button></div></td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </div>
      {creating && <CreateToken onClose={() => setCreating(false)} onCreated={(t) => { setCreated(t); void reload(); }} />}
      {created && (
        <Modal title="Copy your token now" onClose={() => setCreated(null)} footer={
          <>
            <button className="btn" onClick={() => copyText(created.token ?? "")}>Copy</button>
            <button className="btn primary" onClick={() => setCreated(null)}>Done</button>
          </>
        }>
          <p style={{ marginBottom: 12 }}>
            This is the only time the token is shown. Dedie stores a hash of it and cannot display it again.
          </p>
          <pre className="codes" style={{ wordBreak: "break-all", whiteSpace: "pre-wrap" }}>{created.token}</pre>
        </Modal>
      )}
      {revoking && (
        <ConfirmDialog title={`Revoke ${revoking.name}?`} confirmLabel="Revoke token" danger onClose={() => setRevoking(null)}
          onConfirm={async () => { await api.revokeToken(revoking.id); await reload(); }}>
          <p>Anything using this token stops working immediately. This cannot be undone.</p>
        </ConfirmDialog>
      )}
    </div>
  );
}
