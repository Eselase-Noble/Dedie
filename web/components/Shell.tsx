"use client";

import Link from "next/link";
import { usePathname, useRouter } from "next/navigation";
import { useEffect, useMemo, useRef, useState, type ReactNode } from "react";
import { api, errorMessage } from "@/lib/api";
import { isMac, isPaletteShortcut, OPEN_PALETTE, type PaletteEntry } from "@/lib/palette";
import { GO_TO, SEQUENCE_MS, isTyping } from "@/lib/shortcuts";
import { useStackedTables } from "@/lib/stack";
import type { Permission, Session } from "@/lib/types";
import {
  ArchiveIcon, HomeIcon, KeyIcon, KeyboardIcon, ListIcon, Logo, MenuIcon, PlugIcon, PulseIcon, ServerIcon, SidebarIcon,
  SignOutIcon, TerminalIcon, TransferIcon, UserIcon, UsersIcon,
} from "./icons";
import Bell from "./notifications/Bell";
import { NotificationsProvider } from "./notifications/NotificationsProvider";
import Toasts from "./notifications/Toasts";
import CommandPalette, { SearchIcon } from "./palette/CommandPalette";
import { SessionContext } from "./session";
import ShortcutsDialog from "./shortcuts/ShortcutsDialog";
import { Loading } from "./ui";

interface NavItem {
  href: string;
  label: string;
  needs?: Permission;
  section: string;
  icon: ReactNode;
}

// Items the user's role cannot use are hidden; the API enforces the same
// rules, so hiding is a courtesy rather than a control.
const NAV: NavItem[] = [
  { href: "/", icon: <HomeIcon />, label: "Overview", section: "Workspace" },
  { href: "/connections/", icon: <PlugIcon />, label: "Connections", needs: "connections.view", section: "Workspace" },
  { href: "/query/", icon: <TerminalIcon />, label: "Query browser", needs: "query.read", section: "Workspace" },
  { href: "/migrations/", icon: <TransferIcon />, label: "Migrations", needs: "migrations.view", section: "Workspace" },
  { href: "/backups/", icon: <ArchiveIcon />, label: "Backups", needs: "backups.create", section: "Administration" },
  { href: "/monitoring/", icon: <PulseIcon />, label: "Monitoring", needs: "server.admin", section: "Administration" },
  { href: "/server/", icon: <ServerIcon />, label: "Server activity", needs: "server.admin", section: "Administration" },
  { href: "/dbusers/", icon: <KeyIcon />, label: "Database users", needs: "server.admin", section: "Administration" },
  { href: "/users/", icon: <UsersIcon />, label: "Dedie users", needs: "users.manage", section: "Administration" },
  { href: "/audit/", icon: <ListIcon />, label: "Audit log", needs: "audit.view", section: "Administration" },
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
  useStackedTables();

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

  // "?" opens the keyboard reference, and G followed by a letter goes to a
  // page. Neither applies while something is being typed, or while a
  // dialog has the keyboard.
  const router = useRouter();
  const [showingKeys, setShowingKeys] = useState(false);
  const allowed = useRef(new Set<string>());
  useEffect(() => {
    let armed = 0;
    const onKey = (e: KeyboardEvent) => {
      if (e.defaultPrevented || e.ctrlKey || e.metaKey || e.altKey || isTyping(e)) return;
      if (document.documentElement.classList.contains("dialog-open")) return;
      if (e.key === "?") {
        e.preventDefault();
        setShowingKeys(true);
        return;
      }
      const key = e.key.toLowerCase();
      if (key === "g") {
        armed = Date.now();
        return;
      }
      const target = Date.now() - armed < SEQUENCE_MS ? GO_TO.find((g) => g.key === key) : undefined;
      armed = 0;
      if (target && allowed.current.has(target.href)) {
        e.preventDefault();
        router.push(target.href);
      }
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [router]);
  useEffect(() => setShowingKeys(false), [pathname]);

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
    return error ? <div className="content"><div className="banner error" role="alert">{error}</div></div> : <Loading page lines={5} />;
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
  allowed.current = new Set(confined ? [] : pages.map((p) => p.href));
  const initials = session.user.name.split(/\s+/).filter(Boolean).slice(0, 2).map((w) => w[0]?.toUpperCase()).join("") || "?";

  return (
    <SessionContext.Provider value={value}>
      <NotificationsProvider paused={confined}>
        <div className="shell">
          <aside className={`sidebar ${menuOpen ? "" : "closed"} ${collapsed ? "collapsed" : ""}`}>
            <div className="brand">
              <button className="icon-btn collapse-toggle" onClick={toggleSidebar} aria-pressed={collapsed}
                aria-label={collapsed ? "Show navigation" : "Hide navigation"} title={collapsed ? "Show navigation" : "Hide navigation"}>
                <SidebarIcon />
              </button>
              <Link className="brand-home" href={confined ? "/account/" : "/"} title={session.organization}>
                <span className="brand-mark"><Logo /></span>
                <span className="brand-text">
                  <span className="brand-name">Dedie</span>
                  <span className="brand-org" style={{ display: "block" }}>{session.organization}</span>
                </span>
              </Link>
              {!confined && <Bell />}
              {!confined && (
                <button className="icon-btn search-icon" onClick={() => setSearching(true)}
                  aria-label={`Go to anything (${shortcut})`} title={`Go to anything (${shortcut})`}>
                  <SearchIcon />
                </button>
              )}
              <button className="icon-btn menu-toggle" onClick={() => setMenuOpen((v) => !v)} aria-expanded={menuOpen}
                aria-controls="main-navigation" aria-label={menuOpen ? "Close menu" : "Open menu"}>
                <MenuIcon />
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
            <nav className="nav" id="main-navigation" aria-label="Main">
              {sections.map((section) => (
                <div key={section}>
                  <div className="nav-section">{section}</div>
                  {items.filter((i) => i.section === section).map((item) => {
                    const here = isActive(pathname, item.href);
                    return (
                      <Link key={item.href} href={item.href} className={here ? "active" : ""} aria-current={here ? "page" : undefined}
                        title={collapsed ? item.label : undefined}>
                        {item.icon}
                        <span>{item.label}</span>
                      </Link>
                    );
                  })}
                </div>
              ))}
            </nav>
            <div className="sidebar-user">
              <div className="who">
                <span className="avatar" aria-hidden="true">{initials}</span>
                <div>
                  <div className="name" title={session.user.name}>{session.user.name}</div>
                  <div className="meta" title={session.user.email}>{session.user.role} · {session.user.email}</div>
                </div>
              </div>
              <div className="row-actions">
                <Link className={`btn small ${isActive(pathname, "/account/") ? "primary" : ""}`} href="/account/"
                  aria-current={isActive(pathname, "/account/") ? "page" : undefined} title="Account">
                  <UserIcon size={14} /><span>Account</span>
                </Link>
                {!confined && (
                  <button className="icon-btn" onClick={() => setShowingKeys(true)} aria-label="Keyboard shortcuts" title="Keyboard shortcuts (?)">
                    <KeyboardIcon />
                  </button>
                )}
                <span className="spacer" />
                <button className="icon-btn" onClick={signOut} aria-label="Sign out" title="Sign out"><SignOutIcon /></button>
              </div>
            </div>
          </aside>
          <main className="main">{children}</main>
        </div>
        {searching && !confined && <CommandPalette pages={pages} onClose={() => setSearching(false)} />}
        {showingKeys && !confined && <ShortcutsDialog mac={mac} pages={allowed.current} onClose={() => setShowingKeys(false)} />}
        <Toasts />
      </NotificationsProvider>
    </SessionContext.Provider>
  );
}
