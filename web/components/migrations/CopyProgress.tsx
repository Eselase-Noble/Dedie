"use client";

import { formatDuration, formatNumber } from "@/lib/format";
import { useRate } from "@/lib/hooks";
import { copyPercent, estimateIsSound, formatRate, secondsRemaining, summarizeCopy } from "@/lib/progress";
import type { MigrationDetail } from "@/lib/types";

/**
 * The initial copy as a whole: how much of it is done, how fast it is
 * going and, when the source's row estimates have proved accurate so far,
 * how long is left. Shown under the stepper while a job is in that phase.
 */
export default function CopyProgress({ job }: { job: MigrationDetail }) {
  const sum = summarizeCopy(job.tables);
  const running = job.status === "running";
  // The rate is measured from one poll to the next, so it exists only
  // while the job runs and this page is open.
  const rate = useRate(running ? sum.rows_copied : undefined, job);
  const percent = copyPercent(sum);
  const left = estimateIsSound(job.tables) ? secondsRemaining(sum, rate) : null;

  return (
    <div className="copy-progress">
      <div className="copy-progress-head">
        <strong>Initial copy</strong>
        {percent !== null && <span className="copy-progress-pct">{percent}%</span>}
      </div>
      {percent !== null && (
        <div className="progress" role="progressbar" aria-label="Initial copy" aria-valuemin={0} aria-valuemax={100} aria-valuenow={percent}>
          <span style={{ width: `${percent}%` }} />
        </div>
      )}
      <div className="copy-progress-facts">
        <span>
          {percent !== null
            ? <>{formatNumber(sum.rows_copied)} of about {formatNumber(sum.rows_expected)} rows</>
            : <>{formatNumber(sum.rows_copied)} rows copied</>}
        </span>
        <span>{sum.tables_copied} of {sum.tables} tables</span>
        {running && <span>{rate === null ? "measuring speed…" : `${formatRate(rate)} rows/s`}</span>}
        {left !== null && <span>{left <= 5 ? "a few seconds left" : `about ${formatDuration(left)} left`}</span>}
      </div>
      {percent === null && (
        <div className="hint">
          The source has no usable estimate of how many rows these tables hold, so no percentage is shown.
        </div>
      )}
    </div>
  );
}
