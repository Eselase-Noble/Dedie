"use client";

import { useEffect, useState } from "react";
import { api, errorMessage } from "@/lib/api";
import type { Connection, User } from "@/lib/types";
import { Banner, Checkbox, Loading, Modal } from "@/components/ui";

/** Chooses who may use a connection. */
export default function ConnectionAccessDialog({ connection, onClose, onSaved }: {
  connection: Connection; onClose: () => void; onSaved: () => void;
}) {
  const [users, setUsers] = useState<User[] | null>(null);
  const [restricted, setRestricted] = useState(false);
  const [granted, setGranted] = useState<string[]>([]);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");

  useEffect(() => {
    Promise.all([api.users(), api.connectionAccess(connection.id)])
      .then(([list, access]) => {
        setUsers(list);
        setRestricted(access.restricted);
        setGranted(access.user_ids);
      })
      .catch((err) => setError(errorMessage(err)));
  }, [connection.id]);

  const save = async () => {
    setBusy(true);
    setError("");
    try {
      await api.setConnectionAccess(connection.id, { restricted, user_ids: restricted ? granted : [] });
      onSaved();
      onClose();
    } catch (err) {
      setError(errorMessage(err));
      setBusy(false);
    }
  };

  const toggle = (id: string) => setGranted((g) => (g.includes(id) ? g.filter((x) => x !== id) : [...g, id]));
  // Owners and admins always have access, so there is nothing to grant them.
  const grantable = users?.filter((u) => u.role !== "owner" && u.role !== "admin") ?? [];

  return (
    <Modal title={`Access to ${connection.name}`} onClose={onClose} footer={
      <>
        <button className="btn" onClick={onClose} disabled={busy}>Cancel</button>
        <button className="btn primary" onClick={save} disabled={busy || !users}>{busy ? "Saving…" : "Save access"}</button>
      </>
    }>
      {error && <Banner tone="error">{error}</Banner>}
      {!users ? <Loading /> : (
        <>
          <Checkbox label="Restrict this connection to selected people" checked={restricted} onChange={setRestricted}
            hint="When off, everyone whose role allows working with connections can use it. When on, it is hidden from everyone except owners, admins and the people ticked below. What each person may do with it still follows their role." />
          {restricted && (
            <div className="picker">
              <div className="picker-list" style={{ maxHeight: 260 }}>
                {grantable.length === 0 && <div className="hint" style={{ padding: 8 }}>There are no other users to grant access to.</div>}
                {grantable.map((u) => (
                  <label key={u.id} className="picker-item">
                    <input type="checkbox" checked={granted.includes(u.id)} onChange={() => toggle(u.id)} />
                    <span>{u.name} <span className="muted">· {u.email} · {u.role}</span></span>
                  </label>
                ))}
              </div>
            </div>
          )}
        </>
      )}
    </Modal>
  );
}
