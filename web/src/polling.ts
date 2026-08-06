// polling.ts — the UI's two polling primitives. The control plane is a
// loopback SQLite server: polling is the honest transport, and the cost of a
// poll is a millisecond of local I/O.
import { useCallback, useEffect, useRef, useState } from "react";
import { api } from "./api";
import { isTerminalAttempt } from "./format";
import type { TraceEvent } from "./types";

/** useVisible reports whether the tab is in the foreground; a hidden tab polls nothing. */
export function useVisible(): boolean {
  const [visible, setVisible] = useState(
    () => typeof document === "undefined" || document.visibilityState !== "hidden",
  );
  useEffect(() => {
    const update = () => setVisible(document.visibilityState !== "hidden");
    document.addEventListener("visibilitychange", update);
    return () => document.removeEventListener("visibilitychange", update);
  }, []);
  return visible;
}

/**
 * useNow ticks a clock so live spans grow on screen. It is a hook rather than
 * a Date.now() call inside render so a lane re-renders while a tool call is
 * still running — that is what "durations before the envelope exists" means.
 */
export function useNow(intervalMs = 1000): number {
  const [now, setNow] = useState(() => Date.now());
  const visible = useVisible();
  useEffect(() => {
    if (!visible || intervalMs <= 0) return;
    const timer = setInterval(() => setNow(Date.now()), intervalMs);
    return () => clearInterval(timer);
  }, [intervalMs, visible]);
  return now;
}

export interface Polled<T> {
  data: T | null;
  error: Error | null;
  loading: boolean;
  refresh: () => void;
}

/**
 * usePolled fetches on mount and on an interval, keeping the last good value
 * when a refresh fails — a stale view with a visible error beats a blank one.
 */
export function usePolled<T>(load: () => Promise<T>, intervalMs: number, key: string): Polled<T> {
  const [data, setData] = useState<T | null>(null);
  const [error, setError] = useState<Error | null>(null);
  const [loading, setLoading] = useState(true);
  const [tick, setTick] = useState(0);
  const visible = useVisible();
  const loadRef = useRef(load);
  loadRef.current = load;

  useEffect(() => {
    setData(null);
    setLoading(true);
    setError(null);
  }, [key]);

  useEffect(() => {
    let cancelled = false;
    const run = async () => {
      try {
        const value = await loadRef.current();
        if (cancelled) return;
        setData(value);
        setError(null);
      } catch (failure) {
        if (!cancelled) setError(failure as Error);
      } finally {
        if (!cancelled) setLoading(false);
      }
    };
    void run();
    if (!visible || intervalMs <= 0) return () => {
      cancelled = true;
    };
    const timer = setInterval(run, intervalMs);
    return () => {
      cancelled = true;
      clearInterval(timer);
    };
  }, [key, intervalMs, visible, tick]);

  const refresh = useCallback(() => setTick((value) => value + 1), []);
  return { data, error, loading, refresh };
}

export interface AttemptTrace {
  events: TraceEvent[];
  cursor: number;
  error: Error | null;
}

/**
 * useAttemptEvents polls ONE attempt's trace by seq cursor. The cursor is
 * per-attempt state, which is what keeps two lanes of the same job from ever
 * interleaving: each hook instance owns its own cursor and its own event list.
 *
 * The cursor only ever moves forward over events that actually arrived, so an
 * ingest outage cannot advance it past undelivered events — when the worker
 * replays, those events arrive after this cursor and render in seq order.
 * A terminal attempt keeps polling briefly: its trace tail may still be in
 * flight from the worker's buffer.
 */
export function useAttemptEvents(
  attemptID: string,
  attemptState: string,
  intervalMs = 1000,
): AttemptTrace {
  const [events, setEvents] = useState<TraceEvent[]>([]);
  const [error, setError] = useState<Error | null>(null);
  const cursorRef = useRef(0);
  const idleRef = useRef(0);
  const [cursor, setCursor] = useState(0);
  const visible = useVisible();
  const terminal = isTerminalAttempt(attemptState);

  useEffect(() => {
    cursorRef.current = 0;
    idleRef.current = 0;
    setCursor(0);
    setEvents([]);
    setError(null);
  }, [attemptID]);

  useEffect(() => {
    let cancelled = false;
    let timer: ReturnType<typeof setInterval> | null = null;

    const poll = async () => {
      try {
        const page = await api.attemptEvents(attemptID, cursorRef.current);
        if (cancelled) return;
        const arrived = page.events ?? [];
        setError(null);
        if (arrived.length === 0) {
          idleRef.current += 1;
          // A finished attempt whose tail has stopped arriving needs no more
          // polling; a running one keeps its interval.
          if (terminal && idleRef.current >= 2 && timer) {
            clearInterval(timer);
            timer = null;
          }
          return;
        }
        idleRef.current = 0;
        const next = Math.max(cursorRef.current, page.next_cursor);
        cursorRef.current = next;
        setCursor(next);
        setEvents((previous) => mergeEvents(previous, arrived));
      } catch (failure) {
        if (!cancelled) setError(failure as Error);
      }
    };

    void poll();
    if (visible && intervalMs > 0) {
      timer = setInterval(() => void poll(), intervalMs);
    }
    return () => {
      cancelled = true;
      if (timer) clearInterval(timer);
    };
  }, [attemptID, terminal, intervalMs, visible]);

  return { events, cursor, error };
}

/**
 * mergeEvents appends arrivals in seq order and drops any seq already held.
 * Replay after an outage re-sends events the store already had; the UI must
 * render each exactly once, in order, whatever it is handed.
 */
export function mergeEvents(existing: TraceEvent[], arrived: TraceEvent[]): TraceEvent[] {
  if (arrived.length === 0) return existing;
  const bySeq = new Map<number, TraceEvent>();
  for (const event of existing) bySeq.set(event.seq, event);
  for (const event of arrived) if (!bySeq.has(event.seq)) bySeq.set(event.seq, event);
  return [...bySeq.values()].sort((left, right) => left.seq - right.seq);
}
