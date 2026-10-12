"use client";

import { useEffect, useState } from "react";
import { api, errorMessage } from "@/lib/api";
import { download } from "@/lib/csv";
import { copyText } from "@/lib/desktop";
import type { PreviewStatement } from "@/lib/types";
import { Banner, Loading, Modal } from "@/components/ui";

const PHASES: { phase: string; title: string; note: string }[] = [
  { phase: "schema", title: "Before the copy", note: "What has to exist for the rows to land in." },
  { phase: "indexing", title: "After the copy", note: "Cheaper, or only possible, to build once the data is in." },
  { phase: "cutover", title: "At the end, after validation", note: "Created last so it cannot interfere with rows still arriving." },
];

/** Renders the statements as one script, in the order they would run. */
function script(name: string, list: PreviewStatement[]): string {
  const out = [`-- What the migration "${name}" would run on the target. Nothing here has been run.`, ""];
  for (const { phase, title } of PHASES) {
    const part = list.filter((s) => s.phase === phase);
    if (part.length === 0) continue;
    out.push(`-- ===== ${title} =====`, "");
    for (const s of part) {
      out.push(`-- ${s.kind} ${s.name}`, s.end ? `${s.sql.trim()}\n${s.end}` : `${s.sql.trim().replace(/;$/, "")};`, "");
    }
  }
  return out.join("\n");
}

/**
 * Shows the statements a migration would run on the target, without
 * running them, so a translated schema can be read and reviewed before
 * the migration is started.
 */
export default function SchemaPreview({ jobId, name, onClose }: { jobId: string; name: string; onClose: () => void }) {
  const [list, setList] = useState<PreviewStatement[] | null>(null);
  const [error, setError] = useState("");
  const [kind, setKind] = useState("");

  useEffect(() => {
    api.previewMigration(jobId).then(setList).catch((err) => setError(errorMessage(err)));
  }, [jobId]);

  const kinds = [...new Set((list ?? []).map((s) => s.kind))];
  const shown = (list ?? []).filter((s) => !kind || s.kind === kind);
  const file = `${name.replace(/[^A-Za-z0-9]+/g, "-").toLowerCase()}-target.sql`;

  return (
    <Modal
      wide
      title="What will be created on the target"
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
            These are the statements the migration would run, in order, with the source&apos;s tables translated for the
            target. Nothing has been run: reading this changes neither database.
          </p>
          <div className="preflight-bar" style={{ padding: "8px 0" }}>
            <span>
              {kind && <><strong>{shown.length}</strong> of{" "}</>}
              <strong>{list.length}</strong> statement{list.length === 1 ? "" : "s"}
            </span>
            <span className="spacer" />
            <select className="select" style={{ width: "auto" }} value={kind} onChange={(e) => setKind(e.target.value)} aria-label="Show one kind of object">
              <option value="">Every kind</option>
              {kinds.map((k) => <option key={k} value={k}>{k} ({list.filter((s) => s.kind === k).length})</option>)}
            </select>
          </div>
          {list.length === 0 && <div className="empty">The migration would create nothing on the target.</div>}
          {PHASES.map(({ phase, title, note }) => {
            const part = shown.filter((s) => s.phase === phase);
            if (part.length === 0) return null;
            return (
              <section key={phase} className="preview-phase">
                <h3>{title} <span className="hint">{note}</span></h3>
                {part.map((s, i) => (
                  <details key={i} open={part.length <= 12}>
                    <summary><span className="badge">{s.kind}</span> {s.name}</summary>
                    <pre className="mono">{s.sql.trim()}</pre>
                  </details>
                ))}
              </section>
            );
          })}
        </>
      )}
    </Modal>
  );
}
