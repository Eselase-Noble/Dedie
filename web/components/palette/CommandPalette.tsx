"use client";

import { usePathname, useRouter } from "next/navigation";
import { useEffect, useMemo, useRef, useState, type KeyboardEvent } from "react";
import { api } from "@/lib/api";
import { matchEntries, type PaletteEntry } from "@/lib/palette";
import type { Connection, Migration } from "@/lib/types";
import { useSession } from "@/components/session";
import { StatusBadge, useScrollLock } from "@/components/ui";

// More than this many rows is a list nobody reads; typing narrows it.
const MAX_SHOWN = 60;

export function SearchIcon() {
  return (
    <svg width="14" height="14" viewBox="0 0 16 16" fill="none" stroke="currentColor" strokeWidth="1.6" strokeLinecap="round" aria-hidden="true">
      <circle cx="7" cy="7" r="4.5" />
      <path d="M10.5 10.5 14 14" />
    </svg>
  );
}

/**
 * A box to type in and a list to pick from: every page, saved connection,
 * migration and common action the user may use, reachable from anywhere
 * with the keyboard. `pages` are the shell's own navigation entries, so the
 * two never disagree about what the user may open.
 */
export default function CommandPalette({ pages, onClose }: { pages: PaletteEntry[]; onClose: () => void }) {
  const { can } = useSession();
  const router = useRouter();
  const pathname = usePathname() ?? "/";
  const [typed, setTyped] = useState("");
  const [active, setActive] = useState(0);
  const [connections, setConnections] = useState<Connection[]>([]);
  const [migrations, setMigrations] = useState<Migration[]>([]);
  const [loading, setLoading] = useState(true);
  const input = useRef<HTMLInputElement>(null);
  const list = useRef<HTMLUListElement>(null);
  useScrollLock();

  // Whatever had the focus gets it back when the palette closes without
  // going anywhere.
  useEffect(() => {
    const before = document.activeElement as HTMLElement | null;
    input.current?.focus();
    return () => {
      if (before?.isConnected) before.focus();
    };
  }, []);

  // Connections and migrations are asked for when the palette opens, not
  // on every page. A list that fails to load is simply left out.
  useEffect(() => {
    let current = true;
    const none = <T,>(): T[] => [];
    Promise.all([
      can("connections.view") && can("query.read") ? api.connections().catch(none<Connection>) : none<Connection>(),
      can("migrations.view") ? api.migrations().catch(none<Migration>) : none<Migration>(),
    ]).then(([c, m]) => {
      if (!current) return;
      setConnections(c);
      setMigrations(m);
      setLoading(false);
    });
    return () => {
      current = false;
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  const entries = useMemo(() => {
    const actions: PaletteEntry[] = [];
    if (can("connections.manage")) actions.push({ id: "new-connection", group: "Action", label: "New connection", href: "/connections/?new=1" });
    if (can("migrations.manage")) {
      actions.push(
        { id: "new-migration", group: "Action", label: "New migration", detail: "online, with a cutover", href: "/migrations/?new=migrate" },
        { id: "new-offline", group: "Action", label: "New offline migration", detail: "copy once", href: "/migrations/?new=offline" },
        { id: "new-sync", group: "Action", label: "New continuous sync", href: "/migrations/?new=sync" },
      );
    }
    if (can("backups.create")) actions.push({ id: "backup", group: "Action", label: "Run a backup", href: "/backups/?new=1" });
    return [
      ...pages,
      ...actions,
      ...connections.map((c): PaletteEntry => ({
        id: `connection-${c.id}`, group: "Connection", label: `Open in query browser: ${c.name}`,
        detail: `${c.environment ? `${c.environment} · ` : ""}${c.kind}, ${c.database}`, href: `/query/?connection=${encodeURIComponent(c.id)}`,
      })),
      ...migrations.map((m): PaletteEntry => ({
        id: `migration-${m.id}`, group: "Migration", label: m.name, detail: m.status,
        href: `/migrations/detail/?id=${encodeURIComponent(m.id)}`,
      })),
    ];
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [pages, connections, migrations]);

  const shown = useMemo(() => matchEntries(entries, typed).slice(0, MAX_SHOWN), [entries, typed]);
  const current = Math.min(active, shown.length - 1);

  useEffect(() => {
    list.current?.querySelector('[aria-selected="true"]')?.scrollIntoView({ block: "nearest" });
  }, [current, shown]);

  const go = (entry: PaletteEntry) => {
    onClose();
    const [path] = entry.href.split("?");
    const here = pathname.endsWith("/") ? pathname : pathname + "/";
    // The list pages read "?new=" and the query browser its connection
    // when they load, so asking the page that is already open means
    // loading it again.
    if (path === here && entry.href !== path) window.location.assign(entry.href);
    else router.push(entry.href);
  };

  const onKeyDown = (e: KeyboardEvent<HTMLDivElement>) => {
    const move = (to: number) => {
      e.preventDefault();
      if (shown.length > 0) setActive((to + shown.length) % shown.length);
    };
    if (e.key === "ArrowDown") move(current + 1);
    else if (e.key === "ArrowUp") move(current - 1);
    else if (e.key === "Home" && typed === "") move(0);
    else if (e.key === "End" && typed === "") move(shown.length - 1);
    else if (e.key === "Enter") {
      e.preventDefault();
      if (shown[current]) go(shown[current]);
    } else if (e.key === "Escape") {
      // Kept from the page underneath, whose dialogs also close on Escape.
      e.preventDefault();
      e.stopPropagation();
      onClose();
    } else if (e.key === "Tab") {
      // The text box is the only stop, so the focus stays in the palette.
      e.preventDefault();
    }
  };

  return (
    <div className="palette-backdrop" onMouseDown={(e) => e.target === e.currentTarget && onClose()}>
      <div className="palette" role="dialog" aria-modal="true" aria-label="Go to anything" onKeyDown={onKeyDown}>
        <div className="palette-input">
          <SearchIcon />
          <input ref={input} value={typed} spellCheck={false} autoComplete="off"
            onChange={(e) => { setTyped(e.target.value); setActive(0); }}
            onBlur={() => window.setTimeout(() => input.current?.focus(), 0)}
            placeholder="Go to a page, connection or migration, or start something new"
            role="combobox" aria-expanded="true" aria-controls="palette-list" aria-autocomplete="list"
            aria-activedescendant={shown[current] ? `palette-${shown[current].id}` : undefined}
            aria-label="Go to anything" />
          <kbd>Esc</kbd>
        </div>
        <ul ref={list} id="palette-list" className="palette-list" role="listbox" aria-label="Results">
          {shown.map((entry, i) => (
            <li key={entry.id} id={`palette-${entry.id}`} role="option" aria-selected={i === current}
              className={i === current ? "active" : ""}
              onMouseMove={() => setActive(i)}
              onMouseDown={(e) => e.preventDefault()}
              onClick={() => go(entry)}>
              <span className="palette-label">{entry.label}</span>
              {entry.group === "Migration"
                ? <StatusBadge status={entry.detail ?? ""} />
                : entry.detail && <span className="palette-detail">{entry.detail}</span>}
              <span className="palette-group">{entry.group}</span>
            </li>
          ))}
          {shown.length === 0 && !loading && <li className="palette-empty" role="presentation">Nothing matches “{typed.trim()}”.</li>}
          {loading && <li className="palette-empty" role="presentation">Loading connections and migrations…</li>}
        </ul>
        <div className="palette-foot" aria-hidden="true">
          <span><kbd>↑</kbd><kbd>↓</kbd> to move</span>
          <span><kbd>Enter</kbd> to open</span>
        </div>
      </div>
    </div>
  );
}
