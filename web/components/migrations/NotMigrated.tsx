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

/** One definition as it goes into a script: closed the way its dialect closes it. */
function statement(l: Leftover): string {
  const source = l.source.trim();
  if (!source) return "-- (the source did not show its definition)";
  return l.end ? `${source}\n${l.end}` : `${source.replace(/;$/, "")};`;
}

/** Renders the definitions as one script, each under the reason it is not migrated. */
function script(name: string, list: Leftover[]): string {
  const out = [
    `-- What the migration "${name}" does not carry over, as the source defines it.`,
    "-- This is the source's dialect: convert it before running it on the target.",
    "",
  ];
  for (const l of list) {
    out.push(`-- ${l.kind} ${l.name}`, `-- Not migrated: ${l.reason.replace(/\n/g, " ")}`, statement(l), "");
  }
  return out.join("\n");
}

/**
 * Lists what a migration leaves behind on the source (stored code, and the
 * views it could not translate or had to drop again) with the definition of
 * each, so that converting them by hand can start from one script.
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
            Stored procedures, functions and triggers are written in the source database&apos;s own language, and no tool
            converts them reliably, so this migration does not try. A view is carried over only when it can be translated
            and then returns the same on the target. What stays behind is listed here as the source defines it: the place
            to start converting it by hand. Reading this changes neither database.
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
          {list.length === 0 && <div className="empty">Nothing is left behind: the source has no stored code in scope, and every view was carried over.</div>}
          <section className="preview-phase">
            {shown.map((l) => (
              <details key={`${l.kind} ${l.name}`} open={shown.length <= 12}>
                <summary><span className="badge">{l.kind}</span> {l.name} <span className="hint">{l.reason}</span></summary>
                <pre className="mono">{l.source.trim() || "The source did not show this definition."}</pre>
                <div style={{ padding: "6px 10px", borderTop: "1px solid var(--border)" }}>
                  <button className="btn small" type="button" onClick={() => copyText(statement(l))} disabled={!l.source.trim()}>Copy</button>
                </div>
              </details>
            ))}
          </section>
        </>
      )}
    </Modal>
  );
}
