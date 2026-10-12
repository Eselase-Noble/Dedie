"use client";

import { Fragment } from "react";
import { shortcutGroups } from "@/lib/shortcuts";
import { Modal } from "@/components/ui";

/**
 * The keyboard reference, opened with "?" or from the sidebar. `pages`
 * holds the addresses the signed-in role may open, so that no shortcut is
 * listed for a page it cannot reach.
 */
export default function ShortcutsDialog({ mac, pages, onClose }: { mac: boolean; pages: Set<string>; onClose: () => void }) {
  const groups = shortcutGroups(mac ? "⌘" : "Ctrl", pages).filter((g) => g.items.length > 0);
  return (
    <Modal title="Keyboard shortcuts" onClose={onClose}>
      <div className="shortcuts">
        {groups.map((group) => (
          <section key={group.title}>
            <h3>{group.title}</h3>
            <dl>
              {group.items.map((item) => (
                <div key={item.label}>
                  <dt>{item.label}</dt>
                  <dd>
                    {item.keys.map((k, i) => (
                      <Fragment key={k}>
                        {i > 0 && group.title === "Go to" && <span>then</span>}
                        <kbd>{k}</kbd>
                      </Fragment>
                    ))}
                  </dd>
                </div>
              ))}
            </dl>
          </section>
        ))}
      </div>
    </Modal>
  );
}
