"use client";

import { useState, type FormEvent } from "react";
import { api, errorMessage } from "@/lib/api";
import { formatTime } from "@/lib/format";
import type { SavedQuery } from "@/lib/types";
import { useSession } from "@/components/session";
import { Badge, Banner, Checkbox, Field, Modal } from "@/components/ui";

/** Saves a statement, or updates the saved query it was opened from. */
export function SaveQueryDialog({ sql, kind, existing, onClose, onSaved }: {
  sql: string; kind: string; existing: SavedQuery | null; onClose: () => void; onSaved: (q: SavedQuery) => void;
}) {
  const [name, setName] = useState(existing?.name ?? "");
  const [shared, setShared] = useState(existing?.shared ?? false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");

  const save = async (asNew: boolean) => {
    setBusy(true);
    setError("");
    try {
      onSaved(await api.saveQuery(asNew ? "" : existing?.id ?? "", { name, sql, kind, shared }));
      onClose();
    } catch (err) {
      setError(errorMessage(err));
      setBusy(false);
    }
  };
  const submit = (e: FormEvent) => {
    e.preventDefault();
    void save(false);
  };

  return (
    <Modal title={existing ? "Update saved query" : "Save query"} onClose={onClose} footer={
      <>
        <button className="btn" type="button" onClick={onClose} disabled={busy}>Cancel</button>
        {existing && <button className="btn" type="button" onClick={() => void save(true)} disabled={busy || !name.trim()}>Save as new</button>}
        <button className="btn primary" type="submit" form="save-query" disabled={busy}>{busy ? "Saving…" : existing ? "Update" : "Save"}</button>
      </>
    }>
      <form id="save-query" onSubmit={submit}>
        {error && <Banner tone="error">{error}</Banner>}
        <Field label="Name">
          <input className="input" value={name} onChange={(e) => setName(e.target.value)} required maxLength={120} autoFocus placeholder="Orders without an invoice" />
        </Field>
        <Checkbox label="Share with the organization" checked={shared} onChange={setShared}
          hint="Everyone who can run queries sees it. Only you and administrators can change or delete it." />
        <pre className="cell-value" style={{ maxHeight: 180 }}>{sql}</pre>
      </form>
    </Modal>
  );
}

/** The list of saved queries, opened from the toolbar. */
export default function SavedQueries({ queries, onOpen, onDeleted, onClose }: {
  queries: SavedQuery[]; onOpen: (q: SavedQuery) => void; onDeleted: () => void; onClose: () => void;
}) {
  const { session, can } = useSession();
  const [filter, setFilter] = useState("");
  const [error, setError] = useState("");
  const shown = queries.filter((q) => (q.name + " " + q.sql).toLowerCase().includes(filter.toLowerCase()));

  const remove = async (q: SavedQuery) => {
    setError("");
    try {
      await api.deleteSavedQuery(q.id);
      onDeleted();
    } catch (err) {
      setError(errorMessage(err));
    }
  };

  return (
    <Modal wide title="Saved queries" onClose={onClose} footer={<button className="btn primary" onClick={onClose}>Close</button>}>
      {error && <Banner tone="error">{error}</Banner>}
      <input className="input" placeholder="Search by name or text" value={filter} onChange={(e) => setFilter(e.target.value)} autoFocus style={{ marginBottom: 10 }} />
      {shown.length === 0 ? (
        <div className="empty">{queries.length === 0 ? "No saved queries yet. Write a statement and choose Save." : "Nothing matches."}</div>
      ) : (
        <div className="saved-list">
          {shown.map((q) => {
            const mine = q.user_id === session.user.id;
            return (
              <div key={q.id} className="saved-item">
                <button className="saved-open" onClick={() => { onOpen(q); onClose(); }} title="Open in a new tab">
                  <div>
                    <strong>{q.name}</strong>{" "}
                    {q.shared && <Badge tone="info">shared</Badge>} {q.kind && <Badge>{q.kind}</Badge>}
                  </div>
                  <div className="sql">{q.sql}</div>
                  <div className="faint" style={{ fontSize: 11.5 }}>{mine ? "You" : q.owner} · updated {formatTime(q.updated_at)}</div>
                </button>
                {(mine || (q.shared && can("users.manage"))) && <button className="btn small" onClick={() => void remove(q)}>Delete</button>}
              </div>
            );
          })}
        </div>
      )}
    </Modal>
  );
}
