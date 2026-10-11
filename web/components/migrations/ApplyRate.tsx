"use client";

import { useRate } from "@/lib/hooks";
import { formatRate } from "@/lib/progress";
import type { MigrationDetail } from "@/lib/types";

/**
 * How fast changes are being applied to the target while a job streams,
 * measured from one poll to the next.
 */
export default function ApplyRate({ job }: { job: MigrationDetail }) {
  const streaming = job.status === "running" && (job.phase === "streaming" || job.phase === "cutover");
  const transactions = useRate(streaming ? job.state.tx_applied ?? 0 : undefined, job);
  const rows = useRate(streaming ? job.state.rows_applied ?? 0 : undefined, job);
  if (!streaming) return <>—</>;
  if (transactions === null || rows === null) return <span className="muted">measuring…</span>;
  return <>{formatRate(transactions)} tx/s · {formatRate(rows)} rows/s</>;
}
