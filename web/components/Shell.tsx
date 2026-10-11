"use client";

import Link from "next/link";
import { usePathname } from "next/navigation";
import { useEffect, useMemo, useState, type ReactNode } from "react";
import { api, errorMessage } from "@/lib/api";
import type { Permission, Session } from "@/lib/types";
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
  { href: "/server/", label: "Server activity", needs: "server.admin", section: "Administration" },
  { href: "/users/", label: "Users", needs: "users.manage", section: "Administration" },
  { href: "/audit/", label: "Audit log", needs: "audit.view", section: "Administration" },
];

function isActive(pathname: string, href: string): boolean {
  const path = pathname.endsWith("/") ? pathname : pathname + "/";
  return href === "/" ? path === "/" : path.startsWith(href);
}

export default function Shell({ children }: { children: ReactNode }) {
  const pathname = usePathname() ?? "/";
  const [session, setSession] = useState<Session | null>(null);
  const [error, setError] = useState("");
  const [menuOpen, setMenuOpen] = useState(false);

  useEffect(() => {
    // An expired or missing session redirects to the sign-in page from
    // within the API client.
    api.me().then(setSession).catch((err) => setError(errorMessage(err)));
  }, []);

  useEffect(() => setMenuOpen(false), [pathname]);

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

  const items = NAV.filter((item) => !item.needs || value.can(item.needs));
  const sections = Array.from(new Set(items.map((i) => i.section)));

  return (
    <SessionContext.Provider value={value}>
      <div className="shell">
        <aside className={`sidebar ${menuOpen ? "" : "closed"}`}>
          <div className="brand">
            <div style={{ minWidth: 0 }}>
              <div className="brand-name">Dedie</div>
              <div className="brand-org" title={session.organization}>{session.organization}</div>
            </div>
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
            <button className="btn small" onClick={signOut}>Sign out</button>
          </div>
        </aside>
        <main className="main">{children}</main>
      </div>
    </SessionContext.Provider>
  );
}
