"use client";

import { useCallback, useEffect, useRef, useState } from "react";
import { errorMessage } from "./api";

interface Loaded<T> {
  data: T | null;
  error: string;
  loading: boolean;
  reload: () => Promise<void>;
}

/**
 * Loads data when the component mounts and whenever deps change. With
 * pollMs set, it reloads on that interval for as long as `shouldPoll`
 * returns true for the latest data (or always, when it is omitted).
 */
export function useLoad<T>(
  loader: () => Promise<T>,
  deps: unknown[],
  pollMs = 0,
  shouldPoll?: (data: T | null) => boolean,
): Loaded<T> {
  const [data, setData] = useState<T | null>(null);
  const [error, setError] = useState("");
  const [loading, setLoading] = useState(true);
  const loaderRef = useRef(loader);
  loaderRef.current = loader;
  const dataRef = useRef<T | null>(null);
  const pollRef = useRef(shouldPoll);
  pollRef.current = shouldPoll;

  const reload = useCallback(async () => {
    try {
      const next = await loaderRef.current();
      dataRef.current = next;
      setData(next);
      setError("");
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => {
    let active = true;
    setLoading(true);
    void reload();
    if (!pollMs) return;
    const timer = window.setInterval(() => {
      if (!active || document.hidden) return;
      if (pollRef.current && !pollRef.current(dataRef.current)) return;
      void reload();
    }, pollMs);
    return () => {
      active = false;
      window.clearInterval(timer);
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [...deps, pollMs, reload]);

  return { data, error, loading, reload };
}
