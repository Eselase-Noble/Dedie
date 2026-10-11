"use client";

import { LEVEL_LABEL, relativeTime } from "@/lib/notifications";
import { formatTime } from "@/lib/format";
import type { AppNotification } from "@/lib/types";
import { LevelIcon } from "./icons";

/**
 * One notification, as a link to the page where the user can act. It is
 * rendered as an anchor so it can be opened in a new tab, but a plain
 * click is handled by onOpen, which also marks it read.
 */
export default function NotificationRow({ notification: n, onOpen }: { notification: AppNotification; onOpen: (n: AppNotification) => void }) {
  return (
    <a
      className={`note ${n.read ? "" : "unread"}`}
      href={n.link || "#"}
      onClick={(e) => {
        if (e.metaKey || e.ctrlKey || e.shiftKey || e.button !== 0) return;
        e.preventDefault();
        onOpen(n);
      }}
    >
      <LevelIcon level={n.level} />
      <span className="note-text">
        <span className="note-title">
          <span className="sr-only">{LEVEL_LABEL[n.level]}: </span>
          {n.title}
        </span>
        {n.body && <span className="note-body">{n.body}</span>}
        <span className="note-time" title={formatTime(n.created_at)}>{relativeTime(n.created_at)}</span>
      </span>
      {!n.read && <span className="note-dot" role="img" aria-label="Unread" />}
    </a>
  );
}
