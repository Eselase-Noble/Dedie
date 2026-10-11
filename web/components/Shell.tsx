"use client";

import Link from "next/link";
import { usePathname } from "next/navigation";
import { useEffect, useMemo, useState, type ReactNode } from "react";
import { api, errorMessage } from "@/lib/api";
import type { Permission, Session } from "@/lib/types";
import Bell from "./notifications/Bell";
import { NotificationsProvider } from "./notifications/NotificationsProvider";
import Toasts from "./notifications/Toasts";
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
              <button className="btn small menu-toggle" onClick={() => setMenuOpen((v) => !v)} aria-expanded={menuOpen}>
                Menu
              </button>
            </div>
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
        <Toasts />
      </NotificationsProvider>
    </SessionContext.Provider>
  );
}
