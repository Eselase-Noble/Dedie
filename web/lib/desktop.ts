// The desktop application shows these pages in an embedded browser that
// has no download manager, no clipboard and no system notifications. It
// offers them through an object on the window; in an ordinary browser that
// object is absent and the standard web features are used.

interface DesktopBridge {
  save(name: string, text: string): void;
  download(path: string, name: string): void;
  copy(text: string): void;
  openExternal(url: string): void;
  /**
   * Shows a notification outside the window. Clicking it brings the window
   * forward on the page named by link. Older builds of the desktop
   * application do not have it.
   */
  notify?(title: string, body: string, link: string): void;
}

export function desktop(): DesktopBridge | null {
  if (typeof window === "undefined") return null;
  return (window as unknown as { dedieDesktop?: DesktopBridge }).dedieDesktop ?? null;
}

/** Copies text to the clipboard. */
export function copyText(text: string): void {
  const bridge = desktop();
  if (bridge) bridge.copy(text);
  else void navigator.clipboard?.writeText(text);
}
