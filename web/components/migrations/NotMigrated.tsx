"use client";

import { useEffect, useState } from "react";
import { api, errorMessage } from "@/lib/api";
import { download } from "@/lib/csv";
import { copyText } from "@/lib/desktop";
import type { Leftover } from "@/lib/types";
import { Banner, Loading, Modal } from "@/components/ui";

/** The pipelines that translate between two kinds of database, and so leave some of the source behind. */
const TRANSLATING = ["mysql-postgres", "postgres-mysql", "oracle-postgres", "oracle-mysql"];

/** Whether a migration of this kind has a list of what it does not migrate. */
export function leavesBehind(kind: string): boolean {
  return TRANSLATING.includes(kind);
}

/** How each status of a converted routine is shown. */
const STATUS: Record<string, { label: string; tone: string }> = {
  "review": { label: "needs review", tone: "warn" },
  "unconverted": { label: "no conversion", tone: "danger" },
  "not created": { label: "not created on the target", tone: "danger" },
  "created, needs review": { label: "created, needs review", tone: "warn" },
};

/** The definition on the source as it goes into a script: closed the way its dialect closes it. */
function original(l: Leftover): string {
  const source = l.source.trim();
  if (!source) return "-- (the source did not show its definition)";
  return l.end ? `${source}\n${l.end}` : `${source.replace(/;$/, "")};`;
}

/**
 * Why the object is listed. For what has no conversion the reason quotes
 * the first note, and the notes follow it anyway, so it is cut there
 * rather than said twice.
 */
function reason(l: Leftover): string {
  const first = l.notes?.[0];
  const at = first ? l.reason.indexOf(first) : -1;
  return at > 0 ? l.reason.slice(0, at).trimEnd() : l.reason;
}

/** Whether the converted statement was run on the target, so it is no longer only a draft. */
function created(l: Leftover): boolean {
  return l.status === "created, needs review";
}

/**
 * One object as it goes into the script. Where there is a converted draft
 * it comes first, under the notes on what to check, and the original
 * follows as a comment to compare against; otherwise the original stands
 * as it is.
 */
function section(l: Leftover): string {
  const out = [`-- ${l.kind} ${l.name}${l.status ? `  [${l.status}]` : ""}`, `-- ${reason(l).replace(/\n/g, " ")}`];
  for (const note of l.notes ?? []) out.push(`--   ${note}`);
  const draft = (l.draft ?? "").trim();
  if (!draft) return [...out, original(l), ""].join("\n");
  out.push(created(l) ? "-- As created on the target:" : "-- Converted draft:", draft, "-- The original on the source:");
  for (const line of original(l).split("\n")) out.push(`--   ${line}`);
  return [...out, ""].join("\n");
}

/** Renders everything as one script. */
function script(name: string, list: Leftover[]): string {
  return [
    `-- What the migration "${name}" does not carry over, or carried over with a caveat.`,
    "-- A converted draft is in the target's dialect; everything else is as the",
    "-- source defines it, and has to be converted before it runs on the target.",
    "",
    ...list.map(section),
  ].join("\n");
}

/** One object: its reason and notes, and its definition, with the converted draft beside it where there is one. */
function Item({ l, open }: { l: Leftover; open: boolean }) {
  const draft = (l.draft ?? "").trim();
  const [showing, setShowing] = useState<"draft" | "original">(draft ? "draft" : "original");
  const status = l.status ? STATUS[l.status] : undefined;
  const text = showing === "draft" ? draft : l.source.trim();
  return (
    <details open={open}>
      <summary>
        <span className="badge">{l.kind}</span> {l.name}{" "}
        {status && <span className={`badge ${status.tone}`}>{status.label}</span>}
      </summary>
      <div style={{ padding: "8px 12px", borderTop: "1px solid var(--border)" }}>
        <p className="hint" style={{ margin: 0 }}>{reason(l)}</p>
        {l.notes && l.notes.length > 0 && (
          <ul className="hint" style={{ margin: "6px 0 0", paddingLeft: 18 }}>
            {l.notes.map((note) => <li key={note}>{note}</li>)}
          </ul>
        )}
        {/* Above the definition, so the toggle stays put when the two differ in length. */}
        <div className="actions" style={{ marginTop: 8 }}>
          {draft && (
            <div className="segmented small" role="group" aria-label="Which definition to show">
              <button type="button" className={showing === "draft" ? "active" : ""} aria-pressed={showing === "draft"} onClick={() => setShowing("draft")}>{created(l) ? "As created" : "Converted draft"}</button>
              <button type="button" className={showing === "original" ? "active" : ""} aria-pressed={showing === "original"} onClick={() => setShowing("original")}>Original</button>
            </div>
          )}
          <button className="btn small" type="button" onClick={() => copyText(showing === "draft" ? draft : original(l))} disabled={!text}>Copy</button>
        </div>
      </div>
      <pre className="mono">{text || "The source did not show this definition."}</pre>
    </details>
  );
}

/**
 * Lists what a migration leaves behind on the source, or carried over
 * with a caveat: the views it could not translate or had to drop again,
 * and the stored code it could not convert, did not create because a
 * person has to check it, or created with something still to check. Each
 * comes with its definition on the source and, where the converter got
 * that far, the converted draft and the notes on it.
 */
export default function NotMigrated({ jobId, name, onClose }: { jobId: string; name: string; onClose: () => void }) {
  const [list, setList] = useState<Leftover[] | null>(null);
  const [error, setError] = useState("");
  const [kind, setKind] = useState("");

  useEffect(() => {
    api.migrationLeftovers(jobId).then(setList).catch((err) => setError(errorMessage(err)));
  }, [jobId]);

  const kinds = [...new Set((list ?? []).map((l) => l.kind))];
  const shown = (list ?? []).filter((l) => !kind || l.kind === kind);
  const file = `${name.replace(/[^A-Za-z0-9]+/g, "-").toLowerCase()}-not-migrated.sql`;

  return (
    <Modal
      wide
      title="What is not migrated"
      onClose={onClose}
      footer={
        <>
          {list && list.length > 0 && (
            <>
              <button className="btn" type="button" onClick={() => copyText(script(name, list))}>Copy script</button>
              <button className="btn" type="button" onClick={() => download(file, script(name, list), "application/sql;charset=utf-8")}>Download .sql</button>
            </>
          )}
          <button className="btn primary" type="button" onClick={onClose}>Close</button>
        </>
      }
    >
      {error && <Banner tone="error">{error}</Banner>}
      {!list && !error && <Loading />}
      {list && (
        <>
          <p className="hint">
            A view is carried over only when it can be translated and then returns the same on the target. A stored
            procedure, function or trigger is created only when its conversion keeps its behaviour; one that may behave
            differently is converted but left for you to check, with notes on where to look, unless the migration was
            told to create those as well. What has no conversion is listed as the source defines it. Reading this
            changes neither database.
          </p>
          <div className="preflight-bar" style={{ padding: "8px 0" }}>
            <span>
              {kind && <><strong>{shown.length}</strong> of{" "}</>}
              <strong>{list.length}</strong> object{list.length === 1 ? "" : "s"}
            </span>
            <span className="spacer" />
            <select className="select" style={{ width: "auto" }} value={kind} onChange={(e) => setKind(e.target.value)} aria-label="Show one kind of object">
              <option value="">Every kind</option>
              {kinds.map((k) => <option key={k} value={k}>{k} ({list.filter((l) => l.kind === k).length})</option>)}
            </select>
          </div>
          {list.length === 0 && <div className="empty">Nothing is left behind: every view and all the stored code in scope was carried over.</div>}
          <section className="preview-phase">
            {shown.map((l) => <Item key={`${l.kind} ${l.name}`} l={l} open={shown.length <= 12} />)}
          </section>
        </>
      )}
    </Modal>
  );
}
