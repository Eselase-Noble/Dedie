"use client";

import { useState, type FormEvent } from "react";
import { api, errorMessage } from "@/lib/api";
import { formatDuration } from "@/lib/format";
import type { Connection, DbAccounts, SessionFilter } from "@/lib/types";
import { Badge, Banner, Checkbox, Field, Modal, Progress } from "@/components/ui";

/** A pool at or above this share of its limit is flagged. */
const NEAR_LIMIT = 0.8;

interface Target {
  /** Empty for every user on the server. */
  user: string;
  idle: number;
  idleInTransaction: number;
  /** How long the longest-idle of them has been idle. */
  oldestIdleSeconds: number;
}

/** What to tell the administrator when closed connections are back already. */
function reopened(openNow: number): string {
  if (openNow <= 0) return "";
  return ` ${openNow} ${openNow === 1 ? "is" : "are"} idle now: an application with a connection pool opens new ones as soon as it loses some. ` +
    "To hold the number down, set a connection limit for the user or lower the pool's minimum size in the application.";
}

/** Asks which idle connections to close, and closes them. */
function CloseIdle({ connection, target, onClose, onDone }: {
  connection: Connection; target: Target; onClose: () => void; onDone: (message: string) => void;
}) {
  const postgres = connection.kind === "postgres";
  // A pool that pings its connections never lets them sit for minutes,
  // so an age nothing has reached would only close nothing.
  const [minutes, setMinutes] = useState(target.oldestIdleSeconds >= 300 ? 5 : 0);
  const tooOld = minutes * 60 > target.oldestIdleSeconds;
  const [plainIdle, setPlainIdle] = useState(true);
  const [inTransaction, setInTransaction] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const who = target.user ? `${target.user}'s` : "all users'";

  const submit = async (e: FormEvent) => {
    e.preventDefault();
    setBusy(true);
    setError("");
    const base: SessionFilter = { user: target.user, idle_seconds: Math.max(0, Math.round(minutes * 60)) };
    try {
      let closed = 0;
      let openNow = 0;
      for (const state of ["idle", "idle in transaction"] as const) {
        if (state === "idle" ? !plainIdle : !(inTransaction && postgres)) continue;
        const res = await api.terminateSessions(connection.id, { ...base, state });
        closed += res.sessions;
        openNow += Math.max(0, res.open_now);
      }
      onDone(closed === 0
        ? `No connection${target.user ? ` of ${target.user}` : ""} has been idle for ${minutes} minute${minutes === 1 ? "" : "s"}, so none was closed. Choose 0 minutes to close every idle connection.`
        : `Closed ${closed} idle connection${closed === 1 ? "" : "s"}${target.user ? ` of ${target.user}` : ""}.${reopened(openNow)}`);
      onClose();
    } catch (err) {
      setError(errorMessage(err));
      setBusy(false);
    }
  };

  return (
    <Modal title={`Close ${who} idle connections`} onClose={onClose} footer={
      <>
        <button className="btn" type="button" onClick={onClose} disabled={busy}>Cancel</button>
        <button className="btn danger" type="submit" form="close-idle" disabled={busy || (!plainIdle && !(inTransaction && postgres))}>
          {busy ? "Closing…" : "Close idle connections"}
        </button>
      </>
    }>
      <form id="close-idle" onSubmit={submit}>
        {error && <Banner tone="error">{error}</Banner>}
        <p style={{ marginBottom: 12 }}>
          Connections that are running a statement are left alone. A well-behaved connection pool opens a new
          connection when it next needs one, so closing idle ones frees room on the server without interrupting work.
        </p>
        <Field label="Idle for at least (minutes)" hint="0 closes every idle connection, however recently it was used.">
          <input className="input" type="number" min={0} step={1} value={minutes} onChange={(e) => setMinutes(Number(e.target.value))} style={{ maxWidth: 160 }} />
        </Field>
        {tooOld && (
          <Banner tone="warn">
            None of these has been idle that long: the longest is {formatDuration(target.oldestIdleSeconds)}. Applications
            that keep a pool test their connections every so often, which starts the idle time again.{" "}
            <button className="link-btn" type="button" onClick={() => setMinutes(0)}>Close every idle connection instead</button>
          </Banner>
        )}
        <Checkbox label={`Idle connections (${target.idle} now)`} checked={plainIdle} onChange={setPlainIdle}
          hint="Open but waiting for the application. Nothing is lost by closing them." />
        {postgres && (
          <Checkbox label={`Idle inside a transaction (${target.idleInTransaction} now)`} checked={inTransaction} onChange={setInTransaction}
            hint="The application began a transaction and stopped. These hold locks and keep old row versions alive. Closing one rolls its transaction back, so whatever it had changed and not committed is undone." />
        )}
      </form>
    </Modal>
  );
}

/**
 * How much of its connection allowance each user is using, with the
 * users closest to their limit first, and a way to hand idle connections
 * back to the server.
 */
export default function Pools({ connection, data, readOnly, onDone }: {
  connection: Connection; data: DbAccounts; readOnly: boolean; onDone: (message: string) => void;
}) {
  const [closing, setClosing] = useState<Target | null>(null);
  const serverRoom = Math.max(1, data.max_connections - data.reserved_connections);

  const pools = data.accounts
    .filter((a) => a.connections > 0)
    .map((a) => {
      // A user without a limit of its own can take everything the server has.
      const limit = a.connection_limit >= 0 ? a.connection_limit : serverRoom;
      return { ...a, limit, own: a.connection_limit >= 0, share: limit > 0 ? a.connections / limit : 1 };
    })
    .sort((x, y) => y.share - x.share || y.connections - x.connections);

  const totals = pools.reduce((t, p) => ({ idle: t.idle + p.idle, tx: t.tx + p.idle_in_transaction, oldest: Math.max(t.oldest, p.oldest_idle_seconds) }), { idle: 0, tx: 0, oldest: 0 });
  const full = pools.filter((p) => p.share >= 1 && p.own);

  return (
    <div className="card">
      <div className="card-head">
        <h2>Connection pools</h2>
        {!readOnly && (
          <button className="btn small" disabled={totals.idle + totals.tx === 0}
            onClick={() => setClosing({ user: "", idle: totals.idle, idleInTransaction: totals.tx, oldestIdleSeconds: totals.oldest })}>
            Close idle connections…
          </button>
        )}
      </div>
      {full.length > 0 && (
        <div className="card-body" style={{ paddingBottom: 0 }}>
          <Banner tone="error">
            {full.map((p) => p.name).join(", ")} {full.length === 1 ? "has" : "have"} reached {full.length === 1 ? "its" : "their"} connection
            limit. New connections for {full.length === 1 ? "that user" : "those users"} are being refused. Close idle
            connections to make room, or raise the limit.
          </Banner>
        </div>
      )}
      <div className="card-body flush table-wrap">
        {pools.length === 0 ? <div className="empty">No user is connected.</div> : (
          <table className="data stack">
            <thead>
              <tr>
                <th>User</th><th style={{ minWidth: 220 }}>Used of limit</th><th className="num">Active</th><th className="num">Idle</th>
                <th className="num">Idle in transaction</th><th className="num">Longest idle</th><th />
              </tr>
            </thead>
            <tbody>
              {pools.map((p) => (
                <tr key={`${p.name}@${p.host ?? ""}`}>
                  <td>
                    <strong className="mono">{p.name}</strong>{" "}
                    {p.own && p.share >= 1 ? <Badge tone="danger">at limit</Badge> : p.own && p.share >= NEAR_LIMIT ? <Badge tone="warn">near limit</Badge> : null}
                  </td>
                  <td>
                    <div className="pool">
                      <Progress value={p.connections} total={p.limit} />
                      <span className="nowrap">{p.connections} of {p.limit}{!p.own && <span className="muted"> (server limit)</span>}</span>
                    </div>
                  </td>
                  <td className="num">{p.active}</td>
                  <td className="num">{p.idle}</td>
                  <td className="num">{p.idle_in_transaction > 0 ? <Badge tone="warn">{p.idle_in_transaction}</Badge> : 0}</td>
                  <td className="num nowrap">{p.idle + p.idle_in_transaction > 0 ? formatDuration(p.oldest_idle_seconds) : "—"}</td>
                  <td>
                    {!readOnly && (
                      <div className="row-actions">
                        <button className="btn small" disabled={p.idle + p.idle_in_transaction === 0}
                          onClick={() => setClosing({ user: p.name, idle: p.idle, idleInTransaction: p.idle_in_transaction, oldestIdleSeconds: p.oldest_idle_seconds })}>
                          Close idle
                        </button>
                      </div>
                    )}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </div>
      {closing && <CloseIdle connection={connection} target={closing} onClose={() => setClosing(null)} onDone={onDone} />}
    </div>
  );
}
