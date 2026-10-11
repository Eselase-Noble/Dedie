"use client";

import { LEVEL_LABEL } from "@/lib/notifications";
import { LevelIcon } from "./icons";
import { useNotifications } from "./NotificationsProvider";

/**
 * Notifications that arrived while the application was open, stacked in
 * the corner of the window. Clicking one goes to its page.
 */
export default function Toasts() {
  const { toasts, open, dismissToast } = useNotifications();
  if (toasts.length === 0) return null;
  return (
    <div className="toasts" aria-label="New notifications">
      {toasts.map((n) => (
        <div key={n.id} className={`toast ${n.level}`} role={n.level === "error" ? "alert" : "status"}>
          <button className="toast-open" onClick={() => open(n)}>
            <LevelIcon level={n.level} />
            <span className="note-text">
              <span className="note-title"><span className="sr-only">{LEVEL_LABEL[n.level]}: </span>{n.title}</span>
              {n.body && <span className="note-body">{n.body}</span>}
            </span>
          </button>
          <button className="toast-close" onClick={() => dismissToast(n.id)} aria-label={`Dismiss: ${n.title}`}>×</button>
        </div>
      ))}
    </div>
  );
}
