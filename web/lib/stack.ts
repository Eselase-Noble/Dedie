"use client";

import { useEffect } from "react";

/**
 * On a phone a table marked "stack" is shown as one block per row, each
 * cell on a line of its own (see globals.css). A line needs the name of its
 * column in front of it, and that name lives in the table's header, so it
 * is copied onto the cells here: once for every table, whichever page or
 * dialog it belongs to, and again whenever its rows change.
 */
export function useStackedTables() {
  useEffect(() => {
    const label = () => {
      document.querySelectorAll<HTMLTableElement>("table.data.stack").forEach((table) => {
        const heads = Array.from(table.querySelectorAll("thead th")).map((th) => th.textContent?.trim() ?? "");
        if (heads.length === 0) return;
        table.querySelectorAll("tbody tr").forEach((row) => {
          const cells = row.children;
          // A row that spans columns, such as a note under another row,
          // has no column names to take.
          if (cells.length !== heads.length) return;
          for (let i = 1; i < cells.length; i++) {
            const cell = cells[i] as HTMLElement;
            if (heads[i] && cell.dataset.label !== heads[i]) cell.dataset.label = heads[i];
          }
        });
      });
    };
    let queued = 0;
    const observer = new MutationObserver(() => {
      if (queued) return;
      queued = window.requestAnimationFrame(() => {
        queued = 0;
        label();
      });
    });
    label();
    observer.observe(document.body, { childList: true, subtree: true });
    return () => {
      observer.disconnect();
      if (queued) window.cancelAnimationFrame(queued);
    };
  }, []);
}
