"use client";

import Link from "next/link";
import { usePathname } from "next/navigation";
import { useEffect, useMemo, useState, type ReactNode } from "react";
import { api, errorMessage } from "@/lib/api";
import { isMac, isPaletteShortcut, OPEN_PALETTE, type PaletteEntry } from "@/lib/palette";
import type { Permission, Session } from "@/lib/types";
import Bell from "./notifications/Bell";
import { NotificationsProvider } from "./notifications/NotificationsProvider";
import Toasts from "./notifications/Toasts";
import CommandPalette, { SearchIcon } from "./palette/CommandPalette";
import { SessionContext } from "./session";

interface NavItem {
  href: string;
  label: string;
  needs?: Permission;
  section: string;
}

// Items the user's role cannot use are hidden; the API enforces the same
// rules, so hiding is a courtesy rather than a control.
const NAV: NavItem[] = [
  { href: "/", label: "Overview", section: "Workspace" },
  { href: "/connections/", label: "Connections", needs: "connections.view", section: "Workspace" },
  { href: "/query/", label: "Query browser", needs: "query.read", section: "Workspace" },
  { href: "/migrations/", label: "Migrations", needs: "migrations.view", section: "Workspace" },
  { href: "/backups/", label: "Backups", needs: "backups.create", section: "Administration" },
  { href: "/monitoring/", label: "Monitoring", needs: "server.admin", section: "Administration" },
  { href: "/server/", label: "Server activity", needs: "server.admin", section: "Administration" },
  { href: "/dbusers/", label: "Database users", needs: "server.admin", section: "Administration" },
  { href: "/users/", label: "Dedie users", needs: "users.manage", section: "Administration" },
  { href: "/audit/", label: "Audit log", needs: "audit.view", section: "Administration" },
];

const SIDEBAR_KEY = "dedie.sidebar.collapsed";

function isActive(pathname: string, href: string): boolean {
  const path = pathname.endsWith("/") ? pathname : pathname + "/";
  return href === "/" ? path === "/" : path.startsWith(href);
}

export default function Shell({ children }: { children: ReactNode }) {
  const pathname = usePathname() ?? "/";
  const [session, setSession] = useState<Session | null>(null);
  const [error, setError] = useState("");
  const [menuOpen, setMenuOpen] = useState(false);
  // The sidebar can be folded away to give wide tables and the query
  // browser the whole window.
  const [collapsed, setCollapsed] = useState(false);

  useEffect(() => {
    try {
      setCollapsed(window.localStorage.getItem(SIDEBAR_KEY) === "1");
    } catch {
      // Storage unavailable: start expanded.
    }
  }, []);

  const toggleSidebar = () => {
    setCollapsed((was) => {
      try {
        window.localStorage.setItem(SIDEBAR_KEY, was ? "0" : "1");
      } catch {
        // Remembering the choice is optional.
      }
      return !was;
    });
  };

  useEffect(() => {
    // An expired or missing session redirects to the sign-in page from
    // within the API client.
    api.me().then(setSession).catch((err) => setError(errorMessage(err)));
  }, []);

  useEffect(() => setMenuOpen(false), [pathname]);

  // The command palette opens from anywhere with Ctrl+K, or Cmd+K on macOS.
  const [searching, setSearching] = useState(false);
  const [mac, setMac] = useState(false);
  useEffect(() => {
    const onMac = isMac();
    setMac(onMac);
    const open = () => setSearching(true);
    // Listening after everything else lets a control that uses the same
    // keys itself, such as the SQL editor, keep them.
    const onKey = (e: KeyboardEvent) => {
      if (e.defaultPrevented || !isPaletteShortcut(e, onMac)) return;
      // Otherwise the browser moves to its own search box.
      e.preventDefault();
      open();
    };
    window.addEventListener("keydown", onKey);
    // The desktop application's menu opens it through this.
    const exposed = window as unknown as Record<string, unknown>;
    exposed[OPEN_PALETTE] = open;
    return () => {
      window.removeEventListener("keydown", onKey);
      delete exposed[OPEN_PALETTE];
    };
  }, []);
  useEffect(() => setSearching(false), [pathname]);

  // Until two-factor sign-in is set up, an organization that requires it
  // offers the account page and nothing else.
  const confined = !!session?.must_enroll_totp;
  useEffect(() => {
    if (confined && !pathname.startsWith("/account")) window.location.href = "/account/";
  }, [confined, pathname]);

  const value = useMemo(
    () => (session ? { session, can: (p: Permission) => session.permissions.includes(p) } : null),
    [session],
  );

  if (!value || !session) {
    return <div className="loading">{error || "Loading…"}</div>;
  }

  const signOut = async () => {
    try {
      await api.logout();
    } finally {
      window.location.href = "/login/";
    }
  };

  const items = confined ? [] : NAV.filter((item) => !item.needs || value.can(item.needs));
  const sections = Array.from(new Set(items.map((i) => i.section)));
  const shortcut = mac ? "⌘K" : "Ctrl K";
  // The palette offers the pages of the sidebar and the two that are
  // reached from elsewhere in it.
  const pages: PaletteEntry[] = [
    ...items,
    { href: "/notifications/", label: "Notifications" },
    { href: "/account/", label: "Account" },
  ].map((p) => ({ id: `page-${p.href}`, group: "Page", label: p.label, href: p.href }));

  return (
    <SessionContext.Provider value={value}>
      <NotificationsProvider paused={confined}>
        <div className="shell">
          <aside className={`sidebar ${menuOpen ? "" : "closed"} ${collapsed ? "collapsed" : ""}`}>
            <div className="brand">
              <button className="btn small ghost collapse-toggle" onClick={toggleSidebar}
                aria-label={collapsed ? "Show navigation" : "Hide navigation"} title={collapsed ? "Show navigation" : "Hide navigation"}>
                {collapsed ? "»" : "«"}
              </button>
              <div className="brand-text" style={{ minWidth: 0 }}>
                <div className="brand-name">Dedie</div>
                <div className="brand-org" title={session.organization}>{session.organization}</div>
              </div>
              {!confined && <Bell />}
              {!confined && (
                <button className="btn small ghost search-icon" onClick={() => setSearching(true)}
                  aria-label={`Go to anything (${shortcut})`} title={`Go to anything (${shortcut})`}>
                  <SearchIcon />
                </button>
              )}
              <button className="btn small menu-toggle" onClick={() => setMenuOpen((v) => !v)} aria-expanded={menuOpen}>
                Menu
              </button>
            </div>
            {!confined && (
              <button className="sidebar-search" onClick={() => setSearching(true)} aria-haspopup="dialog"
                title="Go to a page, connection or migration, or start something new">
                <SearchIcon />
                <span>Search…</span>
                <kbd>{shortcut}</kbd>
              </button>
            )}
            <nav className="nav" aria-label="Main">
              {sections.map((section) => (
                <div key={section}>
                  <div className="nav-section">{section}</div>
                  {items.filter((i) => i.section === section).map((item) => (
                    <Link key={item.href} href={item.href} className={isActive(pathname, item.href) ? "active" : ""}>
                      {item.label}
                    </Link>
                  ))}
                </div>
              ))}
            </nav>
            <div className="sidebar-user">
              <div className="name" title={session.user.name}>{session.user.name}</div>
              <div className="meta" title={session.user.email}>{session.user.email} · {session.user.role}</div>
              <div className="row-actions" style={{ justifyContent: "flex-start" }}>
                <Link className={`btn small ${isActive(pathname, "/account/") ? "primary" : ""}`} href="/account/">Account</Link>
                <button className="btn small" onClick={signOut}>Sign out</button>
              </div>
            </div>
          </aside>
          <main className="main">{children}</main>
        </div>
        {searching && !confined && <CommandPalette pages={pages} onClose={() => setSearching(false)} />}
        <Toasts />
      </NotificationsProvider>
    </SessionContext.Provider>
  );
}
