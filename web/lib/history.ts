// Per-connection query history kept in the browser. Storage can be disabled
// or full, so every access is guarded and the page works without it.

const LIMIT = 50;

export interface HistoryItem {
  sql: string;
  at: string;
}

const key = (connId: string) => `dedie.history.${connId}`;

export function loadHistory(connId: string): HistoryItem[] {
  try {
    const raw = window.localStorage.getItem(key(connId));
    const parsed = raw ? JSON.parse(raw) : [];
    return Array.isArray(parsed) ? parsed : [];
  } catch {
    return [];
  }
}

export function pushHistory(connId: string, sql: string): HistoryItem[] {
  const trimmed = sql.trim();
  if (!trimmed) return loadHistory(connId);
  const next = [{ sql: trimmed, at: new Date().toISOString() }, ...loadHistory(connId).filter((h) => h.sql !== trimmed)]
    .slice(0, LIMIT);
  try {
    window.localStorage.setItem(key(connId), JSON.stringify(next));
  } catch {
    // History is a convenience; losing it is harmless.
  }
  return next;
}

export function clearHistory(connId: string): void {
  try {
    window.localStorage.removeItem(key(connId));
  } catch {
    // Nothing to do.
  }
}
