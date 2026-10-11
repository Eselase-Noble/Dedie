"use client";

import { useState, type FormEvent } from "react";
import { api, errorMessage } from "@/lib/api";
import { copyText } from "@/lib/desktop";
import type { BackupLocations } from "@/lib/types";
import { Banner, ConfirmDialog, Field, Modal } from "@/components/ui";

const EMPTY_S3 = { endpoint: "s3.amazonaws.com", region: "", bucket: "", prefix: "", access_key: "", secret_key: "" };

function AddLocation({ onClose, onDone }: { onClose: () => void; onDone: () => void }) {
  const [kind, setKind] = useState<"dir" | "s3">("dir");
  const [name, setName] = useState("");
  const [path, setPath] = useState("");
  const [s3, setS3] = useState(EMPTY_S3);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const set = (change: Partial<typeof EMPTY_S3>) => setS3((v) => ({ ...v, ...change }));
  const submit = async (e: FormEvent) => {
    e.preventDefault();
    setBusy(true);
    setError("");
    try {
      await api.createBackupLocation({ name, kind, path: kind === "dir" ? path : "", s3: kind === "s3" ? s3 : EMPTY_S3 });
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
        <Field label="Where">
          <div className="segmented">
            <button type="button" className={kind === "dir" ? "active" : ""} onClick={() => setKind("dir")}>A directory</button>
            <button type="button" className={kind === "s3" ? "active" : ""} onClick={() => setKind("s3")}>Object storage (S3)</button>
          </div>
        </Field>
        <Field label="Name">
          <input className="input" value={name} onChange={(e) => setName(e.target.value)} required maxLength={80} autoFocus placeholder={kind === "dir" ? "Backup volume" : "Offsite bucket"} />
        </Field>
        {kind === "dir" ? (
          <Field label="Directory" hint="The full path of a directory on the machine that runs Dedie: a second disk, or a mounted network share. It is created if it does not exist, and Dedie must be able to write to it.">
            <input className="input mono" value={path} onChange={(e) => setPath(e.target.value)} required spellCheck={false} placeholder="/mnt/backups/dedie" />
          </Field>
        ) : (
          <>
            <p className="hint" style={{ marginBottom: 10 }}>
              Amazon S3 or anything that speaks its protocol: Ceph, Cloudflare R2, Backblaze B2, Wasabi, a MinIO server.
              Backups are sent there as they are taken and kept away from this server, which is what protects them if it is lost.
            </p>
            <div className="form-row">
              <Field label="Endpoint" hint="HTTPS unless you write http://.">
                <input className="input mono" value={s3.endpoint} onChange={(e) => set({ endpoint: e.target.value })} required spellCheck={false} />
              </Field>
              <Field label="Region">
                <input className="input mono" value={s3.region} onChange={(e) => set({ region: e.target.value })} spellCheck={false} placeholder="eu-west-1" />
              </Field>
            </div>
            <div className="form-row">
              <Field label="Bucket" hint="It must already exist.">
                <input className="input mono" value={s3.bucket} onChange={(e) => set({ bucket: e.target.value })} required spellCheck={false} />
              </Field>
              <Field label="Prefix (optional)" hint="A folder inside the bucket.">
                <input className="input mono" value={s3.prefix} onChange={(e) => set({ prefix: e.target.value })} spellCheck={false} placeholder="dedie/production" />
              </Field>
            </div>
            <div className="form-row">
              <Field label="Access key">
                <input className="input mono" value={s3.access_key} onChange={(e) => set({ access_key: e.target.value })} required spellCheck={false} autoComplete="off" />
              </Field>
              <Field label="Secret key" hint="Stored encrypted; not shown again.">
                <input className="input" type="password" value={s3.secret_key} onChange={(e) => set({ secret_key: e.target.value })} required autoComplete="new-password" />
              </Field>
            </div>
            <p className="hint">Give the key only what it needs: to put, get and delete objects under this prefix. Adding the location writes and removes a small test object.</p>
          </>
        )}
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
          <thead><tr><th>Name</th><th>Where</th><th className="num">Backups</th><th /></tr></thead>
          <tbody>
            <tr>
              <td><strong>Data directory</strong> <span className="muted">(default)</span></td>
              <td className="mono">{locations.default_path}</td>
              <td className="num">—</td>
              <td><div className="row-actions"><button className="btn small" onClick={() => copyText(locations.default_path)}>Copy path</button></div></td>
            </tr>
            {locations.locations.map((l) => (
              <tr key={l.id}>
                <td><strong>{l.name}</strong> {l.kind === "s3" && <span className="muted">· object storage</span>}</td>
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
          <p>Dedie stops offering this location for new backups. What is stored there is left as it is. A location that still holds backups cannot be removed.</p>
        </ConfirmDialog>
      )}
    </div>
  );
}
