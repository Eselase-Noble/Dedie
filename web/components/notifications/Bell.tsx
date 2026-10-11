"use client";

import Link from "next/link";
import { usePathname } from "next/navigation";
import { useEffect, useLayoutEffect, useRef, useState } from "react";
import type { AppNotification } from "@/lib/types";
import { BellIcon } from "./icons";
import NotificationRow from "./NotificationRow";
import { useNotifications } from "./NotificationsProvider";

const PANEL_WIDTH = 380;
const MARGIN = 8;

/** The bell in the shell's header, with the panel of recent notifications it opens. */
export default function Bell() {
  const { unread, recent, loaded, markAllRead, open, refresh } = useNotifications();
  const pathname = usePathname();
  const [shown, setShown] = useState(false);
  const [position, setPosition] = useState({ top: 0, left: 0 });
  const button = useRef<HTMLButtonElement>(null);
  const panel = useRef<HTMLDivElement>(null);

  const close = (returnFocus: boolean) => {
    setShown(false);
    if (returnFocus) button.current?.focus();
  };

  // The panel hangs below the bell wherever the bell is: beside the
  // brand, in the folded sidebar, or in the bar across a narrow screen.
  const place = () => {
    const rect = button.current?.getBoundingClientRect();
    if (!rect) return;
    const width = Math.min(PANEL_WIDTH, window.innerWidth - 2 * MARGIN);
    setPosition({
      top: rect.bottom + 6,
      left: Math.max(MARGIN, Math.min(rect.left, window.innerWidth - width - MARGIN)),
    });
  };

  useLayoutEffect(() => {
    if (!shown) return;
    place();
    // Keyboard and screen-reader users land in the panel they just opened.
    panel.current?.focus();
  }, [shown]);

  useEffect(() => {
    if (!shown) return;
    void refresh();
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape") close(true);
    };
    const onPointer = (e: MouseEvent) => {
      const target = e.target as Node;
      if (!panel.current?.contains(target) && !button.current?.contains(target)) close(false);
    };
    window.addEventListener("keydown", onKey);
    window.addEventListener("mousedown", onPointer);
    window.addEventListener("resize", place);
    return () => {
      window.removeEventListener("keydown", onKey);
      window.removeEventListener("mousedown", onPointer);
      window.removeEventListener("resize", place);
    };
  }, [shown, refresh]);

  useEffect(() => setShown(false), [pathname]);

  const follow = (n: AppNotification) => {
    setShown(false);
    open(n);
  };

  const label = unread === 0 ? "Notifications" : `Notifications, ${unread} unread`;
  return (
    <>
      <button ref={button} className={`btn small ghost bell ${shown ? "open" : ""}`} onClick={() => setShown((v) => !v)}
        aria-label={label} title={label} aria-haspopup="dialog" aria-expanded={shown}>
        <BellIcon />
        {unread > 0 && <span className="bell-count" aria-hidden="true">{unread > 99 ? "99+" : unread}</span>}
      </button>
      {shown && (
        <div ref={panel} className="note-panel" role="dialog" aria-label="Notifications" tabIndex={-1} style={position}
          onBlur={(e) => {
            // Tabbing past the last link leaves the panel, which then closes.
            const next = e.relatedTarget as Node | null;
            if (next && !e.currentTarget.contains(next) && !button.current?.contains(next)) close(false);
          }}>
          <div className="note-panel-head">
            <h2>Notifications</h2>
            <button className="link-btn" onClick={() => void markAllRead()} disabled={unread === 0}>Mark all read</button>
          </div>
          <div className="note-list">
            {recent.length === 0 ? (
              <div className="empty">
                {loaded ? "Nothing yet. Migrations, backups and databases that stop answering are reported here." : "Loading…"}
              </div>
            ) : recent.map((n) => <NotificationRow key={n.id} notification={n} onOpen={follow} />)}
          </div>
          <div className="note-panel-foot">
            <Link href="/notifications/" onClick={() => setShown(false)}>See all notifications</Link>
          </div>
        </div>
      )}
    </>
  );
}
