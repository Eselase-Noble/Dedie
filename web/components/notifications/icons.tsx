import type { NotificationLevel } from "@/lib/types";

export function BellIcon() {
  return (
    <svg width="16" height="16" viewBox="0 0 16 16" fill="none" stroke="currentColor" strokeWidth="1.4" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
      <path d="M4 6.5a4 4 0 0 1 8 0c0 3 1.25 4.25 1.75 5H2.25C2.75 10.75 4 9.5 4 6.5Z" />
      <path d="M6.5 13.5a1.6 1.6 0 0 0 3 0" />
    </svg>
  );
}

const PATHS: Record<NotificationLevel, string> = {
  info: "M8 7.25v4M8 4.75v.01",
  success: "M5 8.25l2 2 4-4.5",
  warning: "M8 4.5v4M8 11.25v.01",
  error: "M5.75 5.75l4.5 4.5M10.25 5.75l-4.5 4.5",
};

/** A round mark in the colour of a notification's level. */
export function LevelIcon({ level }: { level: NotificationLevel }) {
  return (
    <span className={`note-icon ${level}`} aria-hidden="true">
      <svg width="18" height="18" viewBox="2 2 12 12" fill="none" stroke="currentColor" strokeWidth="1.3" strokeLinecap="round" strokeLinejoin="round">
        <path d={PATHS[level] ?? PATHS.info} />
      </svg>
    </span>
  );
}
