"use client";

import { useState, type FormEvent } from "react";
import { api, errorMessage } from "@/lib/api";
import { copyText } from "@/lib/desktop";
import type { BackupLocations } from "@/lib/types";
import { Banner, ConfirmDialog, Field, Modal } from "@/components/ui";

function AddLocation({ onClose, onDone }: { onClose: () => void; onDone: () => void }) {
  const [name, setName] = useState("");
  const [path, setPath] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const submit = async (e: FormEvent) => {
    e.preventDefault();
    setBusy(true);
    setError("");
    try {
      await api.createBackupLocation(name, path);
      onDone();
      onClose();
    } catch (err) {
      setError(errorMessage(err));
      setBusy(false);
    }
  };
  return (
    <Modal title="Add a backup location" onClose={onClose} footer={
      <>
        <button className="btn" type="button" onClick={onClose} disabled={busy}>Cancel</button>
        <button className="btn primary" type="submit" form="location-form" disabled={busy}>{busy ? "Checking…" : "Add location"}</button>
      </>
    }>
      <form id="location-form" onSubmit={submit}>
        {error && <Banner tone="error">{error}</Banner>}
        <Field label="Name">
          <input className="input" value={name} onChange={(e) => setName(e.target.value)} required maxLength={80} autoFocus placeholder="Backup volume" />
        </Field>
        <Field label="Directory" hint="The full path of a directory on the machine that runs Dedie: a second disk, or a mounted network share. It is created if it does not exist, and Dedie must be able to write to it.">
          <input className="input mono" value={path} onChange={(e) => setPath(e.target.value)} required spellCheck={false} placeholder="/mnt/backups/dedie" />
        </Field>
      </form>
    </Modal>
  );
}

/** The directories backups may be written to. */
export default function Locations({ locations, mayManage, reload }: {
  locations: BackupLocations; mayManage: boolean; reload: () => Promise<void>;
}) {
  const [adding, setAdding] = useState(false);
  const [removing, setRemoving] = useState<BackupLocations["locations"][number] | null>(null);
  return (
    <div className="card">
      <div className="card-head">
        <h2>Storage locations</h2>
        {mayManage && <button className="btn small" onClick={() => setAdding(true)}>Add location</button>}
      </div>
      <div className="card-body flush table-wrap">
        <table className="data">
          <thead><tr><th>Name</th><th>Directory on the Dedie server</th><th className="num">Backups</th><th /></tr></thead>
          <tbody>
            <tr>
              <td><strong>Data directory</strong> <span className="muted">(default)</span></td>
              <td className="mono">{locations.default_path}</td>
              <td className="num">—</td>
              <td><div className="row-actions"><button className="btn small" onClick={() => copyText(locations.default_path)}>Copy path</button></div></td>
            </tr>
            {locations.locations.map((l) => (
              <tr key={l.id}>
                <td><strong>{l.name}</strong></td>
                <td className="mono">{l.path}</td>
                <td className="num">{l.backups}</td>
                <td>
                  <div className="row-actions">
                    <button className="btn small" onClick={() => copyText(l.path)}>Copy path</button>
                    {mayManage && <button className="btn small" onClick={() => setRemoving(l)}>Remove</button>}
                  </div>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
      {adding && <AddLocation onClose={() => setAdding(false)} onDone={() => void reload()} />}
      {removing && (
        <ConfirmDialog title={`Remove ${removing.name}?`} confirmLabel="Remove location" onClose={() => setRemoving(null)}
          onConfirm={async () => { await api.deleteBackupLocation(removing.id); await reload(); }}>
          <p>Dedie stops offering this directory for new backups. The directory and any files in it are left as they are. A location that still holds backups cannot be removed.</p>
        </ConfirmDialog>
      )}
    </div>
  );
}
