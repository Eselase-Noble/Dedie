// The keyboard shortcuts of the application: the one list that the shell
// acts on and the reference dialog shows, so the two cannot drift apart.

/** Pages reached by pressing G and then a letter. */
export const GO_TO: { key: string; href: string; label: string }[] = [
  { key: "o", href: "/", label: "Overview" },
  { key: "c", href: "/connections/", label: "Connections" },
  { key: "q", href: "/query/", label: "Query browser" },
  { key: "m", href: "/migrations/", label: "Migrations" },
  { key: "b", href: "/backups/", label: "Backups" },
  { key: "h", href: "/monitoring/", label: "Monitoring" },
  { key: "s", href: "/server/", label: "Server activity" },
  { key: "u", href: "/dbusers/", label: "Database users" },
  { key: "a", href: "/audit/", label: "Audit log" },
  { key: "n", href: "/notifications/", label: "Notifications" },
];

/** How long after G its second key is still taken as part of the pair. */
export const SEQUENCE_MS = 1200;

/**
 * Whether a key press belongs to something the user is typing into. Single
 * letters are shortcuts only when it does not.
 */
export function isTyping(e: KeyboardEvent): boolean {
  const el = e.target as HTMLElement | null;
  if (!el || !el.tagName) return false;
  return el.isContentEditable || ["INPUT", "TEXTAREA", "SELECT"].includes(el.tagName);
}

export interface ShortcutGroup {
  title: string;
  items: { keys: string[]; label: string }[];
}

/** Everything the reference dialog lists. `mod` is "⌘" or "Ctrl". */
export function shortcutGroups(mod: string, pages: Set<string>): ShortcutGroup[] {
  return [
    {
      title: "Anywhere",
      items: [
        { keys: [mod, "K"], label: "Go to a page, connection or migration, or start something new" },
        { keys: ["?"], label: "Show this list" },
        { keys: ["Esc"], label: "Close a dialog, panel or menu" },
      ],
    },
    {
      title: "Go to",
      items: GO_TO.filter((g) => pages.has(g.href)).map((g) => ({ keys: ["G", g.key.toUpperCase()], label: g.label })),
    },
    {
      title: "Query browser",
      items: [
        { keys: [mod, "Enter"], label: "Run the statement, or only the selected text" },
        { keys: [mod, "Space"], label: "Complete a table or column name" },
        { keys: [mod, "/"], label: "Comment or uncomment the line" },
        { keys: [mod, "F"], label: "Find in the editor" },
        { keys: [mod, "Z"], label: "Undo" },
      ],
    },
    {
      title: "Lists and the palette",
      items: [
        { keys: ["↑", "↓"], label: "Move through the results" },
        { keys: ["Enter"], label: "Open the highlighted one" },
        { keys: ["Tab"], label: "Move between the controls of a dialog" },
      ],
    },
  ];
}
