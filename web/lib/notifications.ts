// Helpers for the notification centre: how a notification is worded and
// coloured, and how one is shown outside the page, by the desktop
// application or by the browser, when nobody is looking at the window.

import { desktop } from "./desktop";
import type { AppNotification, NotificationLevel, NotificationTopic } from "./types";

export const TOPICS: { value: NotificationTopic; label: string }[] = [
  { value: "migration", label: "Migrations" },
  { value: "backup", label: "Backups" },
  { value: "monitoring", label: "Monitoring" },
  { value: "security", label: "Security" },
];

export const LEVEL_LABEL: Record<NotificationLevel, string> = {
  info: "Information", success: "Success", warning: "Warning", error: "Error",
};

/** "5 min ago" for recent times, the date for older ones. */
export function relativeTime(value: string, now = Date.now()): string {
  const then = new Date(value).getTime();
  if (Number.isNaN(then)) return "";
  const seconds = Math.max(0, Math.round((now - then) / 1000));
  if (seconds < 45) return "just now";
  if (seconds < 3600) return `${Math.max(1, Math.round(seconds / 60))} min ago`;
  if (seconds < 86400) return `${Math.round(seconds / 3600)} h ago`;
  if (seconds < 7 * 86400) return `${Math.round(seconds / 86400)} d ago`;
  return new Date(then).toLocaleDateString(undefined, { year: "numeric", month: "short", day: "numeric" });
}

/** The page title, which carries the unread count so a background tab shows it. */
export function titleWithCount(unread: number): string {
  return unread > 0 ? `(${unread > 99 ? "99+" : unread}) Dedie` : "Dedie";
}

// ---------- notifications outside the page ----------

const PREFERENCE_KEY = "dedie.notifications.system";

/** Whether this environment can show a notification outside the page at all. */
export function systemNotificationsSupported(): boolean {
  if (typeof window === "undefined") return false;
  return !!desktop()?.notify || "Notification" in window;
}

/**
 * Whether the user wants them. The desktop application shows them unless
 * told not to; a browser shows them only once the user has asked for them
 * and given it permission.
 */
export function systemNotificationsEnabled(): boolean {
  if (!systemNotificationsSupported()) return false;
  let stored: string | null = null;
  try {
    stored = window.localStorage.getItem(PREFERENCE_KEY);
  } catch {
    // Without storage the default applies.
  }
  if (desktop()?.notify) return stored !== "0";
  return stored === "1" && Notification.permission === "granted";
}

/**
 * Records the user's choice, asking the browser for permission when they
 * turn notifications on. Resolves to whether they are on afterwards.
 */
export async function setSystemNotifications(on: boolean): Promise<boolean> {
  if (!systemNotificationsSupported()) return false;
  if (on && !desktop()?.notify && Notification.permission !== "granted") {
    try {
      if ((await Notification.requestPermission()) !== "granted") return false;
    } catch {
      return false;
    }
  }
  try {
    window.localStorage.setItem(PREFERENCE_KEY, on ? "1" : "0");
  } catch {
    // The choice then lasts until the page is closed.
  }
  return on && systemNotificationsEnabled();
}

/** True when the browser has refused permission and only its own settings can change that. */
export function systemNotificationsBlocked(): boolean {
  return typeof window !== "undefined" && !desktop()?.notify && "Notification" in window && Notification.permission === "denied";
}

/**
 * Shows a notification outside the page, if the user asked for that and
 * is not looking at the window, where the toast already tells them.
 * onOpen runs when the user clicks it in a browser; the desktop
 * application navigates by itself.
 */
export function showSystemNotification(n: AppNotification, onOpen: (n: AppNotification) => void): void {
  if (!systemNotificationsEnabled()) return;
  try {
    const bridge = desktop();
    if (bridge?.notify) {
      // The application knows better than its embedded page whether its
      // window is in front, so the decision is left to it.
      bridge.notify(n.title, n.body, n.link);
      return;
    }
    if (!document.hidden && document.hasFocus()) return;
    // The tag makes a second tab of the same browser replace the
    // notification instead of repeating it.
    const shown = new Notification(n.title, { body: n.body, tag: `dedie-${n.id}` });
    shown.onclick = () => {
      window.focus();
      shown.close();
      onOpen(n);
    };
  } catch {
    // Some browsers only allow notifications from a service worker. The
    // toast inside the page still tells the user.
  }
}
