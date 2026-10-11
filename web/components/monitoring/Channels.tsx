"use client";

import { useState, type FormEvent } from "react";
import { api, errorMessage } from "@/lib/api";
import { formatTime, isZeroTime } from "@/lib/format";
import type { AlertChannel, ChannelConfig, ChannelKind } from "@/lib/types";
import { Badge, Banner, Checkbox, ConfirmDialog, Field, Modal } from "@/components/ui";

export const EVENTS: { type: string; label: string }[] = [
  { type: "migration.failed", label: "A migration fails" },
  { type: "migration.completed", label: "A migration completes" },
  { type: "migration.lag", label: "A migration falls behind its source" },
  { type: "backup.failed", label: "A backup fails" },
  { type: "connection.down", label: "A database stops answering" },
  { type: "connection.up", label: "A database answers again" },
];

const KINDS: { value: ChannelKind; label: string }[] = [
  { value: "slack", label: "Slack, Teams or Mattermost (incoming webhook)" },
  { value: "webhook", label: "Webhook (JSON)" },
  { value: "email", label: "Email (SMTP)" },
];

function ChannelForm({ channel, onClose, onDone }: { channel: AlertChannel | null; onClose: () => void; onDone: () => void }) {
  const [name, setName] = useState(channel?.name ?? "");
  const [kind, setKind] = useState<ChannelKind>(channel?.kind ?? "slack");
  const [config, setConfig] = useState<ChannelConfig>({ smtp_port: 587 });
  const [to, setTo] = useState("");
  // No selection means every event, which is the safe default for alerts.
  const [events, setEvents] = useState<string[]>(channel?.events ?? []);
  const [enabled, setEnabled] = useState(channel?.enabled ?? true);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const set = (change: ChannelConfig) => setConfig((c) => ({ ...c, ...change }));
  const keep = channel ? "Leave empty to keep the current value." : undefined;

  const submit = async (e: FormEvent) => {
    e.preventDefault();
    setBusy(true);
    setError("");
    try {
      const recipients = to.split(/[,\s]+/).filter(Boolean);
      await api.saveAlertChannel(channel?.id ?? "", {
        name, kind, events, enabled,
        config: kind === "email" ? { ...config, url: undefined, secret: undefined, to: recipients } : { url: config.url, secret: kind === "webhook" ? config.secret : undefined },
      });
      onDone();
      onClose();
    } catch (err) {
      setError(errorMessage(err));
      setBusy(false);
    }
  };

  const toggle = (type: string) => setEvents((ev) => (ev.includes(type) ? ev.filter((t) => t !== type) : [...ev, type]));

  return (
    <Modal title={channel ? `Edit ${channel.name}` : "New alert channel"} onClose={onClose} footer={
      <>
        <button className="btn" type="button" onClick={onClose} disabled={busy}>Cancel</button>
        <button className="btn primary" type="submit" form="channel-form" disabled={busy}>{busy ? "Saving…" : "Save channel"}</button>
      </>
    }>
      <form id="channel-form" onSubmit={submit}>
        {error && <Banner tone="error">{error}</Banner>}
        <Field label="Name">
          <input className="input" value={name} onChange={(e) => setName(e.target.value)} required maxLength={80} placeholder="#database-alerts" autoFocus />
        </Field>
        <Field label="Deliver to">
          <select className="select" value={kind} onChange={(e) => setKind(e.target.value as ChannelKind)} disabled={!!channel}>
            {KINDS.map((k) => <option key={k.value} value={k.value}>{k.label}</option>)}
          </select>
        </Field>
        {kind !== "email" ? (
          <>
            <Field label="Address" hint={channel ? `Currently posting to ${channel.summary}. ${keep}` : "The full URL to post to. It is stored encrypted and never shown again."}>
              <input className="input mono" type="url" value={config.url ?? ""} onChange={(e) => set({ url: e.target.value })}
                required={!channel} placeholder="https://hooks.slack.com/services/…" spellCheck={false} autoComplete="off" />
            </Field>
            {kind === "webhook" && (
              <Field label="Signing secret (optional)" hint={`Each request then carries X-Dedie-Signature: sha256=<HMAC of the body>, so the receiver can check it came from Dedie. ${keep ?? ""}`}>
                <input className="input" type="password" value={config.secret ?? ""} onChange={(e) => set({ secret: e.target.value })} autoComplete="new-password" />
              </Field>
            )}
          </>
        ) : (
          <>
            <div className="form-row">
              <Field label="SMTP server">
                <input className="input" value={config.smtp_host ?? ""} onChange={(e) => set({ smtp_host: e.target.value })} required placeholder="smtp.example.com" />
              </Field>
              <Field label="Port" hint="587 with STARTTLS, or 465.">
                <input className="input" type="number" value={config.smtp_port ?? 587} onChange={(e) => set({ smtp_port: Number(e.target.value) })} />
              </Field>
            </div>
            <div className="form-row">
              <Field label="Username">
                <input className="input" value={config.username ?? ""} onChange={(e) => set({ username: e.target.value })} autoComplete="off" />
              </Field>
              <Field label="Password" hint={keep}>
                <input className="input" type="password" value={config.password ?? ""} onChange={(e) => set({ password: e.target.value })} autoComplete="new-password" />
              </Field>
            </div>
            <Field label="From">
              <input className="input" type="email" value={config.from ?? ""} onChange={(e) => set({ from: e.target.value })} required placeholder="dedie@example.com" />
            </Field>
            <Field label="Recipients" hint={channel ? `Currently: ${channel.summary}. Enter the full list again to change it.` : "Separate addresses with commas."}>
              <input className="input" value={to} onChange={(e) => setTo(e.target.value)} required placeholder="dba@example.com, oncall@example.com" />
            </Field>
          </>
        )}
        <Field label="Send when" hint="Tick nothing to receive every alert.">
          <div className="picker">
            <div className="picker-list" style={{ maxHeight: "none" }}>
              {EVENTS.map((ev) => (
                <label key={ev.type} className="picker-item">
                  <input type="checkbox" checked={events.includes(ev.type)} onChange={() => toggle(ev.type)} />
                  <span>{ev.label}</span>
                </label>
              ))}
            </div>
          </div>
        </Field>
        <Checkbox label="Enabled" checked={enabled} onChange={setEnabled} />
      </form>
    </Modal>
  );
}

/** Where alerts are delivered. */
export default function Channels({ channels, mayManage, reload }: {
  channels: AlertChannel[]; mayManage: boolean; reload: () => Promise<void>;
}) {
  const [editing, setEditing] = useState<AlertChannel | "new" | null>(null);
  const [deleting, setDeleting] = useState<AlertChannel | null>(null);
  const [testing, setTesting] = useState("");
  const [notice, setNotice] = useState<{ tone: "ok" | "error"; text: string } | null>(null);

  const test = async (ch: AlertChannel) => {
    setTesting(ch.id);
    setNotice(null);
    try {
      await api.testAlertChannel(ch.id);
      setNotice({ tone: "ok", text: `Test alert accepted by ${ch.name}. Check that it arrived.` });
    } catch (err) {
      setNotice({ tone: "error", text: errorMessage(err) });
    } finally {
      setTesting("");
      await reload();
    }
  };

  return (
    <div className="card">
      <div className="card-head">
        <h2>Alert channels</h2>
        {mayManage && <button className="btn small" onClick={() => setEditing("new")}>New channel</button>}
      </div>
      <div className="card-body flush table-wrap">
        {notice && <Banner tone={notice.tone}>{notice.text}</Banner>}
        {channels.length === 0 ? (
          <div className="empty">
            No alert channels. Alerts are still recorded below{mayManage ? "; add a channel to be told when something needs attention." : "."}
          </div>
        ) : (
          <table className="data">
            <thead><tr><th>Name</th><th>Delivers to</th><th>Alerts</th><th>Last delivery</th><th /></tr></thead>
            <tbody>
              {channels.map((ch) => (
                <tr key={ch.id}>
                  <td><strong>{ch.name}</strong> {!ch.enabled && <Badge>disabled</Badge>}</td>
                  <td>{ch.kind} <span className="muted">· {ch.summary}</span></td>
                  <td title={ch.events.join(", ")}>{ch.events.length === 0 ? "All" : `${ch.events.length} selected`}</td>
                  <td>
                    {isZeroTime(ch.last_sent_at) ? <span className="faint">Never</span> : (
                      <>
                        <Badge tone={ch.last_error ? "danger" : "ok"}>{ch.last_error ? "failed" : "delivered"}</Badge>{" "}
                        <span className="muted nowrap">{formatTime(ch.last_sent_at)}</span>
                        {ch.last_error && <div className="pre-line" style={{ color: "var(--danger)", maxWidth: 360 }}>{ch.last_error}</div>}
                      </>
                    )}
                  </td>
                  <td>
                    {mayManage && (
                      <div className="row-actions">
                        <button className="btn small" onClick={() => test(ch)} disabled={testing === ch.id}>{testing === ch.id ? "Sending…" : "Send test"}</button>
                        <button className="btn small" onClick={() => setEditing(ch)}>Edit</button>
                        <button className="btn small" onClick={() => setDeleting(ch)}>Delete</button>
                      </div>
                    )}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </div>
      {editing && <ChannelForm channel={editing === "new" ? null : editing} onClose={() => setEditing(null)} onDone={reload} />}
      {deleting && (
        <ConfirmDialog title={`Delete ${deleting.name}?`} confirmLabel="Delete channel" danger onClose={() => setDeleting(null)}
          onConfirm={async () => { await api.deleteAlertChannel(deleting.id); await reload(); }}>
          <p>Alerts stop being delivered there. The alert history is kept.</p>
        </ConfirmDialog>
      )}
    </div>
  );
}
