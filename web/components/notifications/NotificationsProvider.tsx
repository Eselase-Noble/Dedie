"use client";

import { usePathname, useRouter } from "next/navigation";
import { createContext, useCallback, useContext, useEffect, useMemo, useRef, useState, type ReactNode } from "react";
import { api } from "@/lib/api";
import { showSystemNotification, titleWithCount } from "@/lib/notifications";
import type { AppNotification } from "@/lib/types";

// The newest notifications are fetched together with the unread count, so
// the bell's panel opens with nothing to wait for.
const RECENT = 20;
const POLL_MS = 20_000;
const TOAST_MS = 8_000;
const MAX_TOASTS = 4;

interface NotificationsValue {
  unread: number;
  /** The newest notifications, newest first. */
  recent: AppNotification[];
  /** False until the first answer from the server. */
  loaded: boolean;
  /** Notifications that arrived while the page was open and are still on screen. */
  toasts: AppNotification[];
  refresh: () => Promise<void>;
  markRead: (id: number) => Promise<void>;
  markAllRead: () => Promise<void>;
  /** Marks a notification read and goes to the page it links to. */
  open: (n: AppNotification) => void;
  dismissToast: (id: number) => void;
}

const NotificationsContext = createContext<NotificationsValue | null>(null);

export function useNotifications(): NotificationsValue {
  const value = useContext(NotificationsContext);
  if (!value) throw new Error("useNotifications must be used inside the app shell");
  return value;
}

/**
 * Keeps the unread count and the newest notifications current for the
 * whole application, and announces the ones that arrive while it is open.
 * With `paused` set nothing is fetched: a user who has yet to set up
 * two-factor sign-in may not call the API.
 */
export function NotificationsProvider({ paused, children }: { paused: boolean; children: ReactNode }) {
  const router = useRouter();
  const [unread, setUnread] = useState(0);
  const [recent, setRecent] = useState<AppNotification[]>([]);
  const [loaded, setLoaded] = useState(false);
  const [toasts, setToasts] = useState<AppNotification[]>([]);
  // The highest id seen so far. Null until the first load, whose contents
  // are the backlog and are not announced.
  const newest = useRef<number | null>(null);
  const timers = useRef(new Map<number, number>());

  const dismissToast = useCallback((id: number) => {
    window.clearTimeout(timers.current.get(id));
    timers.current.delete(id);
    setToasts((list) => list.filter((t) => t.id !== id));
  }, []);

  const markRead = useCallback(async (id: number) => {
    dismissToast(id);
    setRecent((list) => list.map((n) => (n.id === id ? { ...n, read: true } : n)));
    try {
      setUnread((await api.readNotification(id)).unread);
    } catch {
      // The next poll puts the count right.
    }
  }, [dismissToast]);

  const open = useCallback((n: AppNotification) => {
    if (!n.read) void markRead(n.id);
    else dismissToast(n.id);
    if (n.link) router.push(n.link);
  }, [markRead, dismissToast, router]);
  const openRef = useRef(open);
  openRef.current = open;

  const announce = useCallback((arrived: AppNotification[]) => {
    const fresh = arrived.filter((n) => !n.read);
    if (fresh.length === 0) return;
    // Both lists are newest first, so the newest ends up on top.
    setToasts((list) => [...fresh, ...list.filter((t) => !fresh.some((n) => n.id === t.id))].slice(0, MAX_TOASTS));
    // Outside the page they are shown in the order they happened.
    for (const n of [...fresh].reverse()) {
      // An error stays until it is dismissed; anything else leaves by itself.
      if (n.level !== "error") {
        timers.current.set(n.id, window.setTimeout(() => dismissToast(n.id), TOAST_MS));
      }
      showSystemNotification(n, (clicked) => openRef.current(clicked));
    }
  }, [dismissToast]);

  const refresh = useCallback(async () => {
    let list;
    try {
      list = await api.notifications({ limit: RECENT });
    } catch {
      // A failed poll is not worth interrupting anyone for; the last
      // known state stays on screen and the next poll tries again.
      return;
    }
    setUnread(list.unread);
    setRecent(list.items);
    setLoaded(true);
    const top = list.items.reduce((max, n) => Math.max(max, n.id), 0);
    if (newest.current !== null) {
      const seen = newest.current;
      announce(list.items.filter((n) => n.id > seen));
    }
    newest.current = Math.max(newest.current ?? 0, top);
  }, [announce]);

  const markAllRead = useCallback(async () => {
    setRecent((list) => list.map((n) => ({ ...n, read: true })));
    setToasts([]);
    try {
      setUnread((await api.readAllNotifications()).unread);
    } catch {
      void refresh();
    }
  }, [refresh]);

  useEffect(() => {
    if (paused) return;
    void refresh();
    // A hidden tab stops asking, and catches up the moment it is shown.
    const timer = window.setInterval(() => {
      if (!document.hidden) void refresh();
    }, POLL_MS);
    const onVisible = () => {
      if (!document.hidden) void refresh();
    };
    document.addEventListener("visibilitychange", onVisible);
    return () => {
      window.clearInterval(timer);
      document.removeEventListener("visibilitychange", onVisible);
    };
  }, [paused, refresh]);

  useEffect(() => {
    const pending = timers.current;
    return () => pending.forEach((timer) => window.clearTimeout(timer));
  }, []);

  // The count in the title is what shows on a background tab, and what
  // the desktop application puts in its window title.
  const pathname = usePathname();
  useEffect(() => {
    document.title = titleWithCount(unread);
  }, [unread, pathname]);

  const value = useMemo(
    () => ({ unread, recent, loaded, toasts, refresh, markRead, markAllRead, open, dismissToast }),
    [unread, recent, loaded, toasts, refresh, markRead, markAllRead, open, dismissToast],
  );
  return <NotificationsContext.Provider value={value}>{children}</NotificationsContext.Provider>;
}
