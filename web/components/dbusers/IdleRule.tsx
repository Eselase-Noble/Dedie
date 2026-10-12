"use client";

import { useEffect, useState, type FormEvent } from "react";
import { api, errorMessage } from "@/lib/api";
import { formatNumber, formatTime, isZeroTime } from "@/lib/format";
import type { Connection, IdlePolicy } from "@/lib/types";
import { Badge, Banner, Checkbox, Field } from "@/components/ui";

/**
 * The rule by which the server closes idle connections on its own. Dedie
 * applies it once a minute for as long as it is switched on.
 */
export default function IdleRule({ connection, readOnly }: { connection: Connection; readOnly: boolean }) {
  const postgres = connection.kind === "postgres";
  const [policy, setPolicy] = useState<IdlePolicy | null>(null);
  const [exclude, setExclude] = useState("");
  const [busy, setBusy] = useState(false);
  const [notice, setNotice] = useState<{ tone: "ok" | "error"; text: string } | null>(null);

  useEffect(() => {
    setPolicy(null);
    setNotice(null);
    api.idlePolicy(connection.id)
      .then((p) => { setPolicy(p); setExclude(p.exclude_users.join(", ")); })
      .catch((err) => setNotice({ tone: "error", text: errorMessage(err) }));
  }, [connection.id]);

  if (!policy) return notice ? <Banner tone={notice.tone}>{notice.text}</Banner> : null;
  const set = (change: Partial<IdlePolicy>) => setPolicy({ ...policy, ...change });

  const submit = async (e: FormEvent) => {
    e.preventDefault();
    setBusy(true);
    setNotice(null);
    try {
      const saved = await api.setIdlePolicy(connection.id, {
        enabled: policy.enabled, idle_minutes: policy.idle_minutes, tx_minutes: postgres ? policy.tx_minutes : 0,
        min_usage_percent: policy.min_usage_percent, exclude_users: exclude.split(/[,\s]+/).filter(Boolean),
      });
      setPolicy(saved);
      setNotice({ tone: "ok", text: saved.enabled ? "Saved. The rule is applied every minute from now on." : "Saved. Automatic closing is off." });
    } catch (err) {
      setNotice({ tone: "error", text: errorMessage(err) });
    } finally {
      setBusy(false);
    }
  };

  return (
    <div className="card">
      <div className="card-head">
        <h2>Close idle connections automatically</h2>
        <Badge tone={policy.enabled ? "ok" : ""}>{policy.enabled ? "on" : "off"}</Badge>
      </div>
      <form className="card-body" onSubmit={submit} style={{ maxWidth: 720 }}>
        {notice && <Banner tone={notice.tone}>{notice.text}</Banner>}
        {policy.last_error && <Banner tone="error">The last pass failed: {policy.last_error}</Banner>}
        <Checkbox label="Close connections that stay idle too long" checked={policy.enabled} onChange={(v) => set({ enabled: v })} disabled={readOnly}
          hint={`Dedie checks every minute. Connections that are running a statement are never closed, nor are Dedie's own or those of ${connection.username}, the account this connection uses.`} />
        <div className="form-row">
          <Field label="Idle for more than (minutes)" hint="Open, but waiting for the application. A pool that tests its connections every minute or two starts their idle time again each time, so a longer limit than that never applies to them.">
            <input className="input" type="number" min={1} value={policy.idle_minutes} disabled={readOnly}
              onChange={(e) => set({ idle_minutes: Number(e.target.value) })} />
          </Field>
          <Field label="Only while the server is at least this full (%)" hint="0 applies the rule always. 80 leaves everyone connected until the server nears its limit.">
            <input className="input" type="number" min={0} max={100} value={policy.min_usage_percent} disabled={readOnly}
              onChange={(e) => set({ min_usage_percent: Number(e.target.value) })} />
          </Field>
        </div>
        <div className="form-row">
          {postgres && (
            <Field label="Idle inside a transaction for more than (minutes)" hint="0 leaves these alone. Closing one rolls its uncommitted work back.">
              <input className="input" type="number" min={0} value={policy.tx_minutes} disabled={readOnly}
                onChange={(e) => set({ tx_minutes: Number(e.target.value) })} />
            </Field>
          )}
          <Field label="Never close connections of these users" hint="Comma separated, for example a replication or monitoring account.">
            <input className="input mono" value={exclude} onChange={(e) => setExclude(e.target.value)} disabled={readOnly} spellCheck={false} placeholder="replicator, monitoring" />
          </Field>
        </div>
        <div className="row-actions" style={{ justifyContent: "flex-start", alignItems: "center" }}>
          {!readOnly && <button className="btn primary" type="submit" disabled={busy}>{busy ? "Saving…" : "Save rule"}</button>}
          <span className="hint">
            {isZeroTime(policy.last_run_at) ? "Not applied yet." : `Last checked ${formatTime(policy.last_run_at)}. ${formatNumber(policy.total_closed)} connection${policy.total_closed === 1 ? "" : "s"} closed so far.`}
          </span>
        </div>
      </form>
    </div>
  );
}
