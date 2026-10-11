"use client";

import { useCallback, useEffect, useRef, useState } from "react";
import { api, errorMessage } from "@/lib/api";
import { TOPICS } from "@/lib/notifications";
import type { AppNotification, NotificationTopic } from "@/lib/types";
import NotificationRow from "@/components/notifications/NotificationRow";
import { useNotifications } from "@/components/notifications/NotificationsProvider";
import { Banner, Loading, PageHeader } from "@/components/ui";

const PAGE = 30;

/** Every notification the signed-in user may see, with filters and paging. */
export default function NotificationsPage() {
  const { unread, recent, markAllRead, open } = useNotifications();
  const [onlyUnread, setOnlyUnread] = useState(false);
  const [topic, setTopic] = useState<NotificationTopic | "">("");
  const [items, setItems] = useState<AppNotification[]>([]);
  const [more, setMore] = useState(false);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState("");
  // Answers can arrive out of order when a filter is changed quickly;
  // only the latest request may fill the list.
  const request = useRef(0);

  const load = useCallback(async (before: number) => {
    const mine = ++request.current;
    setLoading(true);
    try {
      const page = await api.notifications({ unread: onlyUnread, topic, before, limit: PAGE });
      if (mine !== request.current) return;
      setItems((had) => (before ? [...had, ...page.items] : page.items));
      setMore(page.items.length === PAGE);
      setError("");
    } catch (err) {
      if (mine === request.current) setError(errorMessage(err));
    } finally {
      if (mine === request.current) setLoading(false);
    }
  }, [onlyUnread, topic]);

  useEffect(() => {
    void load(0);
  }, [load]);

  // When the shell hears of a notification this list has not got, the
  // first page is fetched again so it appears here too.
  const newestKnown = recent[0]?.id ?? 0;
  const newestShown = useRef(0);
  newestShown.current = items[0]?.id ?? 0;
  useEffect(() => {
    if (newestKnown > newestShown.current && !loading) void load(0);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [newestKnown]);

  const follow = (n: AppNotification) => {
    setItems((list) => list.map((item) => (item.id === n.id ? { ...item, read: true } : item)));
    open(n);
  };

  const readAll = async () => {
    await markAllRead();
    await load(0);
  };

  return (
    <div className="content">
      <PageHeader title="Notifications"
        subtitle="What happened to your migrations, backups and databases, and to your own account. They are kept for 30 days.">
        <button className="btn" onClick={() => void readAll()} disabled={unread === 0}>Mark all read</button>
      </PageHeader>
      {error && <Banner tone="error">{error}</Banner>}

      <div className="toolbar">
        <div className="segmented" role="group" aria-label="Show">
          <button className={onlyUnread ? "" : "active"} aria-pressed={!onlyUnread} onClick={() => setOnlyUnread(false)}>All</button>
          <button className={onlyUnread ? "active" : ""} aria-pressed={onlyUnread} onClick={() => setOnlyUnread(true)}>
            Unread{unread > 0 ? ` (${unread})` : ""}
          </button>
        </div>
        <select className="select" aria-label="Kind" value={topic} onChange={(e) => setTopic(e.target.value as NotificationTopic | "")}>
          <option value="">All kinds</option>
          {TOPICS.map((t) => <option key={t.value} value={t.value}>{t.label}</option>)}
        </select>
      </div>

      <div className="card">
        <div className="card-body flush note-list page">
          {items.length === 0 && loading ? <Loading /> : items.length === 0 ? (
            <div className="empty">
              {onlyUnread ? "You have read everything." : topic ? "Nothing of this kind in the last 30 days." : "Nothing has happened yet."}
            </div>
          ) : items.map((n) => <NotificationRow key={n.id} notification={n} onOpen={follow} />)}
        </div>
      </div>

      {more && (
        <button className="btn" disabled={loading} onClick={() => void load(items[items.length - 1].id)}>
          {loading ? "Loading…" : "Load older notifications"}
        </button>
      )}
    </div>
  );
}
