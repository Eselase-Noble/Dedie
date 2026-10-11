"use client";

import { useState } from "react";
import { api, errorMessage } from "@/lib/api";
import type { ExistingObject, OnExisting } from "@/lib/types";
import { Banner, Modal } from "@/components/ui";

const CHOICES: { value: OnExisting; label: string; detail: string }[] = [
  {
    value: "fail",
    label: "Refuse",
    detail: "Do not start. Nothing on the target is touched; deal with the objects yourself, or migrate into an empty database or schema.",
  },
  {
    value: "skip",
    label: "Keep them and skip them",
    detail: "Leave every one exactly as it is and do not migrate it. An existing table receives no rows and changes to its source are not followed; an existing view, routine or sequence stays the target's own version.",
  },
  {
    value: "replace",
    label: "Replace them",
    detail: "Drop each one on the target and create it again from the source. A dropped table loses the data it holds there, together with whatever depends on it.",
  },
];

/** Groups objects by kind, keeping the order kinds were first seen in. */
function byKind(objects: ExistingObject[]): [string, string[]][] {
  const groups = new Map<string, string[]>();
  for (const o of objects) groups.set(o.kind, [...(groups.get(o.kind) ?? []), o.name]);
  return [...groups.entries()];
}

/**
 * Asks the administrator what a migration should do about objects the
 * target already has. Nothing is skipped or dropped without an answer
 * given here, and replacing has to be confirmed by typing the word.
 */
export default function ExistingObjectsDialog({ jobId, objects, current, onClose, onChosen }: {
  jobId: string; objects: ExistingObject[]; current: OnExisting; onClose: () => void; onChosen: () => Promise<void>;
}) {
  const [choice, setChoice] = useState<OnExisting>(current);
  const [typed, setTyped] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const confirmed = choice !== "replace" || typed.trim() === "REPLACE";

  const save = async () => {
    setBusy(true);
    setError("");
    try {
      await api.setOnExisting(jobId, choice);
      await onChosen();
      onClose();
    } catch (err) {
      setError(errorMessage(err));
      setBusy(false);
    }
  };

  return (
    <Modal
      wide
      title="Some objects already exist on the target"
      onClose={onClose}
      footer={
        <>
          <button className="btn" type="button" onClick={onClose} disabled={busy}>Cancel</button>
          <button className={`btn ${choice === "replace" ? "danger" : "primary"}`} type="button" onClick={save} disabled={busy || !confirmed}>
            {busy ? "Saving…" : choice === "replace" ? `Replace ${objects.length} object(s)` : "Save and check again"}
          </button>
        </>
      }
    >
      {error && <Banner tone="error">{error}</Banner>}
      <p>
        The target already holds {objects.length} object{objects.length === 1 ? "" : "s"} with the name of one this
        migration would create. Choose what the migration does about them.
      </p>
      <div className="existing-list">
        {byKind(objects).map(([kind, names]) => (
          <div key={kind}>
            <strong>{kind} ({names.length})</strong>
            <span className="mono">{names.slice(0, 40).join(", ")}{names.length > 40 ? ` and ${names.length - 40} more` : ""}</span>
          </div>
        ))}
      </div>
      <div className="choice-list" role="radiogroup" aria-label="What to do with existing objects">
        {CHOICES.map((c) => (
          <label key={c.value} className={`choice ${choice === c.value ? "selected" : ""} ${c.value === "replace" ? "danger" : ""}`}>
            <input type="radio" name="on-existing" checked={choice === c.value} onChange={() => setChoice(c.value)} />
            <span>
              <strong>{c.label}</strong>
              <span className="hint">{c.detail}</span>
            </span>
          </label>
        ))}
      </div>
      {choice === "replace" && (
        <div className="field">
          <label htmlFor="confirm-replace">Type REPLACE to confirm that these objects may be dropped on the target</label>
          <input id="confirm-replace" className="input" value={typed} onChange={(e) => setTyped(e.target.value)} autoComplete="off" />
        </div>
      )}
    </Modal>
  );
}
