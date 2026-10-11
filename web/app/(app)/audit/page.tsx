"use client";

import { Fragment, useCallback, useEffect, useState } from "react";
import { api, errorMessage } from "@/lib/api";
import { formatNumber, formatTime, prettyJSON } from "@/lib/format";
import type { AuditEntry, AuditVerification } from "@/lib/types";
import { Banner, Loading, PageHeader } from "@/components/ui";

const PAGE_SIZE = 100;

export default function AuditPage() {
  const [entries, setEntries] = useState<AuditEntry[]>([]);
  const [loading, setLoading] = useState(true);
  const [more, setMore] = useState(false);
  const [error, setError] = useState("");
  const [open, setOpen] = useState<number | null>(null);
  const [filter, setFilter] = useState("");
  const [verifying, setVerifying] = useState(false);
  const [verification, setVerification] = useState<AuditVerification | null>(null);

  // Pages are keyed by entry id: each request returns entries older than `before`.
  const load = useCallback(async (before: number) => {
    setLoading(true);
    setError("");
    try {
      const page = await api.audit(before, PAGE_SIZE);
      setEntries((prev) => (before === 0 ? page : [...prev, ...page]));
      setMore(page.length === PAGE_SIZE);
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => {
    void load(0);
  }, [load]);

  const verify = async () => {
    setVerifying(true);
    setError("");
    try {
      setVerification(await api.verifyAudit());
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setVerifying(false);
    }
  };

  const needle = filter.trim().toLowerCase();
  const shown = needle
    ? entries.filter((e) => `${e.actor} ${e.action} ${e.target} ${e.detail}`.toLowerCase().includes(needle))
    : entries;

  return (
    <div className="content">
      <PageHeader
        title="Audit log"
        subtitle="Every sign-in, query, connection change, migration action and restore. Entries are hash-chained, so an edited or deleted entry is detectable."
      >
        <button className="btn" onClick={() => load(0)} disabled={loading}>Refresh</button>
        <button className="btn primary" onClick={verify} disabled={verifying}>{verifying ? "Verifying…" : "Verify integrity"}</button>
      </PageHeader>
      {error && <Banner tone="error">{error}</Banner>}
      {verification && (
        verification.intact
          ? <Banner tone="ok">The log is intact: {formatNumber(verification.checked)} entries verified against the hash chain.</Banner>
          : <Banner tone="error">
              The log has been tampered with. The chain breaks at entry #{verification.broken_at}, after{" "}
              {formatNumber(verification.checked)} valid entries. That entry, or one next to it, was altered or removed.
            </Banner>
      )}

      <div className="toolbar">
        <input className="input" style={{ maxWidth: 320 }} placeholder="Filter loaded entries" value={filter} onChange={(e) => setFilter(e.target.value)} />
        <span className="muted">{formatNumber(shown.length)} shown</span>
      </div>

      <div className="card">
        <div className="card-body flush table-wrap">
          {entries.length === 0 && loading ? <Loading /> : shown.length === 0 ? (
            <div className="empty">No entries.</div>
          ) : (
            <table className="data">
              <thead>
                <tr><th className="num">#</th><th>Time</th><th>Actor</th><th>Action</th><th>Target</th><th>IP</th><th /></tr>
              </thead>
              <tbody>
                {shown.map((e) => (
                  <Fragment key={e.id}>
                    <tr>
                      <td className="num muted">{e.id}</td>
                      <td className="nowrap">{formatTime(e.time)}</td>
                      <td>{e.actor}</td>
                      <td className="mono">{e.action}</td>
                      <td className="mono">{e.target || <span className="faint">—</span>}</td>
                      <td className="mono muted">{e.ip || "—"}</td>
                      <td>
                        <div className="row-actions">
                          <button className="btn small" onClick={() => setOpen(open === e.id ? null : e.id)}>
                            {open === e.id ? "Hide" : "Details"}
                          </button>
                        </div>
                      </td>
                    </tr>
                    {open === e.id && (
                      <tr>
                        <td colSpan={7} style={{ background: "var(--bg-subtle)" }}>
                          <pre>{prettyJSON(e.detail) || "No additional detail."}</pre>
                          <div className="faint mono" style={{ marginTop: 6 }}>hash {e.hash}</div>
                        </td>
                      </tr>
                    )}
                  </Fragment>
                ))}
              </tbody>
            </table>
          )}
        </div>
      </div>

      {more && (
        <button className="btn" disabled={loading} onClick={() => load(entries[entries.length - 1].id)}>
          {loading ? "Loading…" : "Load older entries"}
        </button>
      )}
    </div>
  );
}
