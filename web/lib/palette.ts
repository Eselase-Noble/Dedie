// The entries of the command palette and how typed text is matched to them.

export type PaletteGroup = "Page" | "Action" | "Connection" | "Migration";

export interface PaletteEntry {
  /** Unique among the entries shown together. */
  id: string;
  group: PaletteGroup;
  label: string;
  /** Shown beside the label, and searched with it: a status, a database. */
  detail?: string;
  href: string;
}

/**
 * The entries that match what was typed, best first. Every word typed has
 * to appear somewhere in an entry, in any order and without regard to
 * case. Entries whose label starts with the text come first, then those
 * where each word starts a word of the label, then the rest; within each
 * the given order is kept.
 */
export function matchEntries(entries: PaletteEntry[], typed: string): PaletteEntry[] {
  const text = typed.trim().toLowerCase();
  if (!text) return entries;
  const words = text.split(/\s+/);
  const ranked: { entry: PaletteEntry; rank: number; at: number }[] = [];
  entries.forEach((entry, at) => {
    const label = entry.label.toLowerCase();
    const all = `${label} ${entry.detail ?? ""} ${entry.group}`.toLowerCase();
    if (!words.every((w) => all.includes(w))) return;
    const starts = label.split(/[^a-z0-9]+/);
    const rank = label.startsWith(text) ? 0
      : words.every((w) => starts.some((s) => s.startsWith(w))) ? 1
        : words.every((w) => label.includes(w)) ? 2 : 3;
    ranked.push({ entry, rank, at });
  });
  return ranked.sort((a, b) => a.rank - b.rank || a.at - b.at).map((r) => r.entry);
}

/** Whether a key press is the palette's shortcut: Cmd+K on macOS, Ctrl+K elsewhere. */
export function isPaletteShortcut(e: KeyboardEvent, mac: boolean): boolean {
  // Ctrl+K on macOS deletes to the end of the line in text editors, and
  // with Shift the SQL editor deletes the line, so neither opens it.
  if (e.key.toLowerCase() !== "k" || e.shiftKey || e.altKey) return false;
  return mac ? e.metaKey && !e.ctrlKey : e.ctrlKey && !e.metaKey;
}

export function isMac(): boolean {
  return typeof navigator !== "undefined" && /Mac|iPhone|iPad/.test(navigator.platform);
}

/** The name of a function on the window that opens the palette, for the desktop application's menu. */
export const OPEN_PALETTE = "dedieOpenPalette";
