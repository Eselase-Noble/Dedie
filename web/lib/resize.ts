"use client";

import { useCallback, useEffect, useState, type PointerEvent as ReactPointerEvent } from "react";

/**
 * A pane size in pixels that the user can drag and that is remembered
 * between visits. Storage is optional: without it the size simply resets.
 */
export function useStoredSize(key: string, initial: number, min: number, max: () => number) {
  const [size, setSize] = useState(initial);

  useEffect(() => {
    try {
      const saved = Number(window.localStorage.getItem(key));
      if (saved > 0) setSize(Math.min(Math.max(saved, min), max()));
    } catch {
      // Keep the default.
    }
    // Read once on mount.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [key]);

  const update = useCallback((next: number) => {
    const clamped = Math.round(Math.min(Math.max(next, min), max()));
    setSize(clamped);
    try {
      window.localStorage.setItem(key, String(clamped));
    } catch {
      // Remembering the size is optional.
    }
  }, [key, min, max]);

  return [size, update] as const;
}

/**
 * Starts a drag on a splitter. `axis` is the direction of movement, `start`
 * the pane's size when the drag began, and `onSize` receives each new size.
 */
export function beginDrag(
  e: ReactPointerEvent<HTMLElement>,
  axis: "x" | "y",
  start: number,
  onSize: (size: number) => void,
) {
  e.preventDefault();
  const origin = axis === "x" ? e.clientX : e.clientY;
  const move = (ev: PointerEvent) => onSize(start + (axis === "x" ? ev.clientX : ev.clientY) - origin);
  const stop = () => {
    window.removeEventListener("pointermove", move);
    window.removeEventListener("pointerup", stop);
    document.body.classList.remove("dragging", "dragging-x", "dragging-y");
  };
  // The body class keeps the resize cursor and stops text selection while
  // the pointer travels over other elements.
  document.body.classList.add("dragging", `dragging-${axis}`);
  window.addEventListener("pointermove", move);
  window.addEventListener("pointerup", stop);
}
