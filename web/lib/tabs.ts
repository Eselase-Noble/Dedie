// The query browser's open tabs, remembered per connection in the browser.

export interface Tab {
  id: string;
  /** A query tab has an editor; a table tab browses one table's rows. */
  kind: "query" | "table";
  title: string;
  sql: string;
  schema?: string;
  table?: string;
  /** The saved query this tab was opened from, if any. */
  savedId?: string;
}

const key = (connId: string) => `dedie.tabs.${connId}`;

export function newId(): string {
  return Math.random().toString(36).slice(2, 10);
}

export function newQueryTab(n: number, sql = "", title = ""): Tab {
  return { id: newId(), kind: "query", title: title || `Query ${n}`, sql };
}

export function loadTabs(connId: string): { tabs: Tab[]; active: string } {
  try {
    const parsed = JSON.parse(window.localStorage.getItem(key(connId)) ?? "null");
    if (parsed && Array.isArray(parsed.tabs) && parsed.tabs.length > 0) return parsed;
  } catch {
    // Unreadable or unavailable: start with one empty tab.
  }
  const first = newQueryTab(1);
  return { tabs: [first], active: first.id };
}

export function saveTabs(connId: string, tabs: Tab[], active: string): void {
  try {
    window.localStorage.setItem(key(connId), JSON.stringify({ tabs, active }));
  } catch {
    // Remembering tabs is a convenience.
  }
}
