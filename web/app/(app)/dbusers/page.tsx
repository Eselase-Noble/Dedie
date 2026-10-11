"use client";

import { useEffect, useState, type FormEvent } from "react";
import { api, errorMessage } from "@/lib/api";
import { formatDuration } from "@/lib/format";
import { useLoad } from "@/lib/hooks";
import type { ConnectionUsage, DbAccount, SessionFilter } from "@/lib/types";
import UserForm from "@/components/dbusers/UserForm";
import { Badge, Banner, Checkbox, ConfirmDialog, Field, Loading, Modal, PageHeader, Progress } from "@/components/ui";

const limitText = (n: number) => (n < 0 ? "no limit" : String(n));

/** Sets the connection cap of a user or a database. */
function LimitDialog({ title, current, onClose, onSave }: {
  title: string; current: number; onClose: () => void; onSave: (limit: number) => Promise<void>;
}) {
  const [value, setValue] = useState(current >= 0 ? String(current) : "");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const submit = async (e: FormEvent) => {
    e.preventDefault();
    setBusy(true);
    setError("");
    try {
      await onSave(value.trim() === "" ? -1 : Number(value));
      onClose();
    } catch (err) {
      setError(errorMessage(err));
      setBusy(false);
    }
  };
  return (
    <Modal title={title} onClose={onClose} footer={
      <>
        <button className="btn" type="button" onClick={onClose} disabled={busy}>Cancel</button>
        <button className="btn primary" type="submit" form="limit-form" disabled={busy}>{busy ? "Saving…" : "Set limit"}</button>
      </>
    }>
      <form id="limit-form" onSubmit={submit}>
        {error && <Banner tone="error">{error}</Banner>}
        <Field label="Most connections at once" hint="Empty for no limit. Connections already open are not closed; the limit applies to new ones.">
          <input className="input" type="number" min={0} value={value} onChange={(e) => setValue(e.target.value)} placeholder="No limit" autoFocus />
        </Field>
      </form>
    </Modal>
  );
}

/** Users, roles and connection usage of one database server. */
export default function DatabaseUsersPage() {
  const connections = useLoad(async () => (await api.connections()).filter((c) => c.kind === "postgres" || c.kind === "mysql"), []);
  const [connId, setConnId] = useState("");
  useEffect(() => {
    if (!connId && connections.data?.[0]) setConnId(connections.data[0].id);
  }, [connections.data, connId]);
  const connection = connections.data?.find((c) => c.id === connId) ?? null;

  const { data, error, loading, reload } = useLoad(async () => (connId ? api.dbAccounts(connId) : null), [connId], 10000);
  const [editing, setEditing] = useState<DbAccount | "new" | null>(null);
  const [dropping, setDropping] = useState<DbAccount | null>(null);
  const [handOver, setHandOver] = useState(false);
  const [limiting, setLimiting] = useState<{ scope: "account" | "database"; name: string; host: string; current: number } | null>(null);
  const [disconnecting, setDisconnecting] = useState<{ label: string; filter: SessionFilter } | null>(null);
  const [notice, setNotice] = useState("");

  const done = async (message: string) => {
    setNotice(message);
    await reload();
  };
  const readOnly = !!connection?.read_only;
  const usable = data ? Math.max(1, data.max_connections - data.reserved_connections) : 1;
  const nearFull = !!data && data.used_connections / usable >= 0.8;

  const usageFilter = (u: ConnectionUsage): SessionFilter =>
    ({ user: u.user, application: u.application, client: u.client === "local" && connection?.kind === "mysql" ? "" : u.client, database: u.database });

  return (
    <div className="content">
      <PageHeader title="Database users" subtitle="Accounts on a database server, what they may do, and who is using its connections.">
        {connection && !readOnly && <button className="btn primary" onClick={() => setEditing("new")} disabled={!data}>New database user</button>}
      </PageHeader>
      {connections.error && <Banner tone="error">{connections.error}</Banner>}

      <div className="toolbar">
        <select className="select" value={connId} onChange={(e) => { setConnId(e.target.value); setNotice(""); }} aria-label="Connection">
          {(!connections.data || connections.data.length === 0) && <option value="">No PostgreSQL or MySQL connections</option>}
          {connections.data?.map((c) => <option key={c.id} value={c.id}>{c.name} ({c.host})</option>)}
        </select>
        <button className="btn" onClick={() => void reload()} disabled={!connId}>Refresh</button>
        {connection && <span className="hint">Managed as {connection.username}. The server decides what that account may change.</span>}
      </div>

      {error && <Banner tone="error">{error}</Banner>}
      {notice && <Banner tone="ok">{notice}</Banner>}
      {readOnly && <Banner tone="info">This connection is marked read-only, so users and limits can be viewed but not changed through it.</Banner>}
      {loading && !data && connId && <Loading />}

      {data && connection && (
        <>
          <div className="card">
            <div className="card-head">
              <h2>Connections in use</h2>
              <span className="muted">
                {data.used_connections} of {data.max_connections}
                {data.reserved_connections > 0 && ` (${data.reserved_connections} reserved for administrators)`}
              </span>
            </div>
            <div className="card-body">
              <Progress value={data.used_connections} total={usable} />
              {nearFull && (
                <Banner tone="warn">
                  The server is close to its connection limit. New connections will be refused when it is reached. Look
                  below for an application holding more than it needs, or cap it with a connection limit.
                </Banner>
              )}
            </div>
            <div className="card-body flush table-wrap">
              <table className="data">
                <thead>
                  <tr>
                    <th>User</th><th>Application</th><th>From</th><th>Database</th><th className="num">Connections</th>
                    <th className="num">Active</th><th className="num">Idle</th><th className="num">Idle in transaction</th><th className="num">Longest</th><th />
                  </tr>
                </thead>
                <tbody>
                  {data.usage.map((u, i) => (
                    <tr key={i}>
                      <td><strong>{u.user || <span className="faint">(system)</span>}</strong></td>
                      <td>{u.application || <span className="faint">—</span>}</td>
                      <td className="mono">{u.client}</td>
                      <td>{u.database || <span className="faint">—</span>}</td>
                      <td className="num"><strong>{u.total}</strong></td>
                      <td className="num">{u.active}</td>
                      <td className="num">{u.idle}</td>
                      <td className="num">{u.idle_in_transaction > 0 ? <Badge tone="warn">{u.idle_in_transaction}</Badge> : 0}</td>
                      <td className="num nowrap">{formatDuration(u.longest_seconds)}</td>
                      <td>
                        <div className="row-actions">
                          {!readOnly && u.user && (
                            <button className="btn small" onClick={() => setDisconnecting({
                              label: `${u.total} connection${u.total === 1 ? "" : "s"} of ${u.user}${u.application ? ` (${u.application})` : ""} from ${u.client}`,
                              filter: usageFilter(u),
                            })}>Disconnect</button>
                          )}
                        </div>
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          </div>

          <div className="card">
            <div className="card-head"><h2>Users and roles</h2><span className="muted">{data.accounts.length}</span></div>
            <div className="card-body flush table-wrap">
              <table className="data">
                <thead>
                  <tr><th>Name</th><th>Sign-in</th><th>Privileges</th><th>Member of</th><th className="num">Connections</th><th className="num">Limit</th><th /></tr>
                </thead>
                <tbody>
                  {data.accounts.map((a) => {
                    const self = a.name === connection.username;
                    return (
                      <tr key={`${a.name}@${a.host ?? ""}`}>
                        <td><strong className="mono">{a.name}</strong>{a.host && <span className="muted mono">@{a.host}</span>}{self && <span className="muted"> (this connection)</span>}</td>
                        <td><Badge tone={a.can_login ? "ok" : ""}>{a.can_login ? "user" : connection.kind === "mysql" ? "locked" : "role"}</Badge></td>
                        <td>
                          {a.superuser && <Badge tone="danger">superuser</Badge>}{" "}
                          {a.replication && <Badge tone="info">replication</Badge>}{" "}
                          {a.create_db && <Badge>create database</Badge>}{" "}
                          {a.create_role && <Badge>create users</Badge>}
                        </td>
                        <td className="mono">{a.member_of.join(", ") || <span className="faint">—</span>}</td>
                        <td className="num">{a.connections}</td>
                        <td className="num">{limitText(a.connection_limit)}</td>
                        <td>
                          {!readOnly && (
                            <div className="row-actions">
                              <button className="btn small" onClick={() => setEditing(a)}>Edit</button>
                              <button className="btn small" onClick={() => setLimiting({ scope: "account", name: a.name, host: a.host ?? "", current: a.connection_limit })}>Limit</button>
                              <button className="btn small" disabled={a.connections === 0 || self}
                                onClick={() => setDisconnecting({ label: `all ${a.connections} connection${a.connections === 1 ? "" : "s"} of ${a.name}`, filter: { user: a.name } })}>
                                Disconnect
                              </button>
                              <button className="btn small" disabled={self} onClick={() => { setHandOver(false); setDropping(a); }}>Drop</button>
                            </div>
                          )}
                        </td>
                      </tr>
                    );
                  })}
                </tbody>
              </table>
            </div>
          </div>

          {data.database_limits && (
            <div className="card">
              <div className="card-head"><h2>Limits per database</h2></div>
              <div className="card-body flush table-wrap">
                <table className="data">
                  <thead><tr><th>Database</th><th className="num">Connections</th><th className="num">Limit</th><th /></tr></thead>
                  <tbody>
                    {data.databases.map((d) => (
                      <tr key={d.name}>
                        <td><strong>{d.name}</strong></td>
                        <td className="num">{d.connections}</td>
                        <td className="num">{limitText(d.connection_limit)}</td>
                        <td>
                          {!readOnly && (
                            <div className="row-actions">
                              <button className="btn small" onClick={() => setLimiting({ scope: "database", name: d.name, host: "", current: d.connection_limit })}>Limit</button>
                              <button className="btn small" disabled={d.connections === 0}
                                onClick={() => setDisconnecting({ label: `everyone connected to ${d.name}`, filter: { database: d.name } })}>
                                Disconnect all
                              </button>
                            </div>
                          )}
                        </td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              </div>
            </div>
          )}

          {editing && (
            <UserForm connection={connection} existing={editing === "new" ? null : editing} roles={data.roles}
              onClose={() => setEditing(null)} onSaved={(m) => void done(m)} />
          )}
          {limiting && (
            <LimitDialog title={`Connection limit for ${limiting.name}`} current={limiting.current} onClose={() => setLimiting(null)}
              onSave={async (limit) => {
                await api.setConnectionLimit(connection.id, { scope: limiting.scope, name: limiting.name, host: limiting.host, limit });
                await done(`${limiting.name} is now limited to ${limitText(limit)}${limit >= 0 ? " connections" : ""}.`);
              }} />
          )}
          {disconnecting && (
            <ConfirmDialog title="Disconnect these sessions?" confirmLabel="Disconnect" danger onClose={() => setDisconnecting(null)}
              onConfirm={async () => {
                const res = await api.terminateSessions(connection.id, disconnecting.filter);
                await done(`Disconnected ${res.sessions} session${res.sessions === 1 ? "" : "s"}.`);
              }}>
              <p>
                This closes {disconnecting.label}. Statements they are running are stopped and their open transactions
                rolled back. An application that reconnects by itself will be back shortly; to keep it out, lower its
                connection limit or disable its user first.
              </p>
            </ConfirmDialog>
          )}
          {dropping && (
            <ConfirmDialog title={`Drop ${dropping.name}?`} confirmLabel="Drop user" danger onClose={() => setDropping(null)}
              onConfirm={async () => {
                await api.dropDbAccount(connection.id, dropping.name, dropping.host ?? "", handOver);
                await done(`${dropping.name} was dropped.`);
              }}>
              <p style={{ marginBottom: 10 }}>
                The account is removed from the server and anything signing in with it stops working. This cannot be undone.
              </p>
              {connection.kind === "postgres" && (
                <Checkbox label={`First hand over what it has in ${connection.database}`} checked={handOver} onChange={setHandOver}
                  hint={`PostgreSQL refuses to drop a role that owns objects or holds privileges. With this on, objects it owns in ${connection.database} are given to ${connection.username} and its privileges there are revoked; no table or data is dropped. Other databases are not touched.`} />
              )}
            </ConfirmDialog>
          )}
        </>
      )}
    </div>
  );
}
