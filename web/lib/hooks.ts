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

/**
 * The rate at which a counter is rising, per second, over roughly the last
 * `windowMs`. Give it the counter and something that changes with every
 * reading (the polled object will do); it answers null until readings a
 * few seconds apart exist. Passing undefined, or a counter that went
 * backwards, starts the measurement afresh.
 */
export function useRate(value: number | undefined, reading: unknown, windowMs = 10_000): number | null {
  const samples = useRef<{ at: number; value: number }[]>([]);
  const [rate, setRate] = useState<number | null>(null);

  useEffect(() => {
    const list = samples.current;
    if (value === undefined || (list.length > 0 && value < list[list.length - 1].value)) {
      samples.current = value === undefined ? [] : [{ at: Date.now(), value }];
      setRate(null);
      return;
    }
    const now = Date.now();
    list.push({ at: now, value });
    // Keep one reading from before the window so that it is always spanned.
    while (list.length > 2 && now - list[1].at >= windowMs) list.shift();
    const seconds = (now - list[0].at) / 1000;
    setRate(seconds >= 3 ? (value - list[0].value) / seconds : null);
  }, [value, reading, windowMs]);

  return rate;
}
