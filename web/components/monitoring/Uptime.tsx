import { formatTime } from "@/lib/format";
import type { HealthSample } from "@/lib/types";

/**
 * A strip of bars, one per check, oldest on the left. Height follows the
 * response time; a failed check is a full-height red bar.
 */
export default function Uptime({ samples }: { samples: HealthSample[] }) {
  if (samples.length === 0) return <span className="faint">No checks yet</span>;
  const slowest = Math.max(1, ...samples.filter((s) => s.online).map((s) => s.latency_ms));
  return (
    <div className="uptime" role="img" aria-label={`${samples.filter((s) => !s.online).length} failed checks of ${samples.length}`}>
      {samples.map((s, i) => (
        <span key={i} className={s.online ? "up" : "down"}
          style={{ height: s.online ? `${Math.max(15, Math.round((s.latency_ms / slowest) * 100))}%` : "100%" }}
          title={`${formatTime(s.time)} · ${s.online ? `${s.latency_ms} ms` : s.error || "no answer"}`} />
      ))}
    </div>
  );
}
