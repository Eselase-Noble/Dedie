// How far along a migration's initial copy is, worked out from the rows
// copied so far and the source's estimate of how many there are. The
// estimate comes from the source's statistics, so it can be missing or
// stale; everything here says so rather than show a figure it cannot stand
// behind.

import type { CopyProgress, JobTable, Migration } from "./types";

/**
 * The phase of a listed job in a few words: the engine's name for it,
 * with the progress of the initial copy while that runs. What the engine
 * calls cutover is, for an offline copy, its validation.
 */
export function phaseSummary(m: Migration): string {
  if (m.config.mode === "offline" && m.phase === "cutover") return "validation";
  const percent = m.copy ? copyPercent(m.copy) : null;
  return percent === null ? m.phase : `${m.phase} ${percent}%`;
}

/** Sums the tables of a job the way the server does for a listed job. */
export function summarizeCopy(tables: JobTable[]): CopyProgress {
  const sum: CopyProgress = { tables: tables.length, tables_copied: 0, rows_copied: 0, est_rows: 0, rows_expected: 0 };
  for (const t of tables) {
    const copied = t.status === "copied";
    if (copied) sum.tables_copied++;
    sum.rows_copied += t.rows_copied;
    sum.est_rows += t.est_rows;
    // A finished table counts for what it really held. One still running
    // counts for its estimate, or for what it has copied once it passes that.
    sum.rows_expected += copied ? t.rows_copied : Math.max(t.est_rows, t.rows_copied);
  }
  return sum;
}

/**
 * The copy's progress as a whole percentage, or null when the estimate
 * cannot support one: there is none, or everything it expected has been
 * copied while tables are still outstanding. It stops at 99 because only
 * the end of the phase says the copy is finished.
 */
export function copyPercent(p: CopyProgress): number | null {
  if (p.tables === 0 || p.est_rows <= 0 || p.rows_expected <= 0) return null;
  if (p.tables_copied < p.tables && p.rows_copied >= p.rows_expected) return null;
  return Math.min(99, Math.floor((100 * p.rows_copied) / p.rows_expected));
}

/** How far the finished tables may differ from their estimates, in total. */
const TOLERANCE = 0.25;

/**
 * Whether the estimate is good enough to predict the time remaining. The
 * tables already copied show how accurate it was for them, and a table that
 * has passed its estimate while still copying shows it was wrong there.
 */
export function estimateIsSound(tables: JobTable[]): boolean {
  let estimated = 0;
  let actual = 0;
  for (const t of tables) {
    if (t.status === "copied") {
      estimated += t.est_rows;
      actual += t.rows_copied;
    } else if (t.rows_copied > t.est_rows) {
      return false;
    }
  }
  return Math.abs(actual - estimated) <= TOLERANCE * Math.max(estimated, actual);
}

/** Seconds until the copy ends at the given rate, or null when that cannot be said. */
export function secondsRemaining(p: CopyProgress, rowsPerSecond: number | null): number | null {
  if (!rowsPerSecond || rowsPerSecond <= 0 || copyPercent(p) === null) return null;
  return Math.ceil((p.rows_expected - p.rows_copied) / rowsPerSecond);
}

/** A rate for display: whole numbers once it is large enough not to need a decimal. */
export function formatRate(perSecond: number): string {
  return perSecond >= 10 ? Math.round(perSecond).toLocaleString("en-US") : perSecond.toFixed(1);
}
