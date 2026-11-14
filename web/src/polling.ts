// polling.ts — the UI's two polling primitives. The control plane is a
// loopback SQLite server: polling is the honest transport, and the cost of a
// poll is a millisecond of local I/O.
import { useCallback, useEffect, useRef, useState, useSyncExternalStore } from "react";
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
 * The interval may be read from the last value, so a view polls until what
 * it shows can no longer change and then stops; 0 means no further polls.
 */
export function usePolled<T>(load: () => Promise<T>, every: number | ((data: T | null) => number), key: string): Polled<T> {
  const [data, setData] = useState<T | null>(null);
  const [error, setError] = useState<Error | null>(null);
  const [loading, setLoading] = useState(true);
  const [tick, setTick] = useState(0);
  const visible = useVisible();
  const loadRef = useRef(load);
  loadRef.current = load;
  const intervalMs = typeof every === "function" ? every(data) : every;

  useEffect(() => {
    setData(null);
    setLoading(true);
    setError(null);
  }, [key]);

  useEffect(() => {
    let cancelled = false;
    let inFlight = false;
    const run = async () => {
      if (inFlight) return;
      inFlight = true;
      try {
        const value = await loadRef.current();
        if (cancelled) return;
        setData(value);
        setError(null);
      } catch (failure) {
        if (!cancelled) setError(failure as Error);
      } finally {
        inFlight = false;
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

interface SharedPoll {
  load: () => Promise<unknown>;
  snapshot: { data: unknown; error: Error | null; loading: boolean };
  listeners: Set<() => void>;
  /** Each subscriber's interval; the poll runs at the shortest. */
  intervals: Map<symbol, number>;
  timer: ReturnType<typeof setInterval> | null;
  timerMs: number;
  inFlight: boolean;
  onVisibility: () => void;
}

const sharedPolls = new Map<string, SharedPoll>();

function sharedFetch(poll: SharedPoll): void {
  if (poll.inFlight) return;
  poll.inFlight = true;
  const publish = (next: Partial<SharedPoll["snapshot"]>) => {
    poll.snapshot = { ...poll.snapshot, ...next, loading: false };
    poll.listeners.forEach((listener) => listener());
  };
  poll.load().then(
    (data) => publish({ data, error: null }),
    (failure: unknown) => publish({ error: failure as Error }),
  ).finally(() => { poll.inFlight = false; });
}

function sharedSchedule(poll: SharedPoll): void {
  const hidden = typeof document !== "undefined" && document.visibilityState === "hidden";
  const every = Math.min(...poll.intervals.values());
  const wanted = hidden || !Number.isFinite(every) || every <= 0 ? 0 : every;
  if (wanted === poll.timerMs && (poll.timer !== null) === (wanted > 0)) return;
  if (poll.timer) clearInterval(poll.timer);
  poll.timer = wanted > 0 ? setInterval(() => sharedFetch(poll), wanted) : null;
  poll.timerMs = wanted;
}

/**
 * useSharedPoll is usePolled for a resource several components on one screen
 * read — the worker fleet in the plant bar and on the Floor, the queue in the
 * plant bar and on Queue. Every subscriber to a key shares one request and
 * one timer, at the shortest interval any of them asked for; the poll stops
 * when the last one unmounts and while the tab is hidden.
 */
export function useSharedPoll<T>(key: string, load: () => Promise<T>, intervalMs: number): Polled<T> {
  const id = useRef(Symbol(key)).current;
  const pollFor = useCallback(() => {
    let poll = sharedPolls.get(key);
    if (!poll) {
      const created: SharedPoll = {
        load, snapshot: { data: null, error: null, loading: true }, listeners: new Set(), intervals: new Map(),
        timer: null, timerMs: 0, inFlight: false,
        onVisibility: () => {
          if (typeof document !== "undefined" && document.visibilityState !== "hidden") sharedFetch(created);
          sharedSchedule(created);
        },
      };
      poll = created;
      sharedPolls.set(key, poll);
    }
    return poll;
  }, [key, load]);
  const subscribe = useCallback((listener: () => void) => {
    const poll = pollFor();
    const first = poll.listeners.size === 0;
    poll.listeners.add(listener);
    poll.intervals.set(id, intervalMs);
    if (first) {
      document.addEventListener("visibilitychange", poll.onVisibility);
      sharedFetch(poll);
    }
    sharedSchedule(poll);
    return () => {
      poll.listeners.delete(listener);
      poll.intervals.delete(id);
      if (poll.listeners.size > 0) { sharedSchedule(poll); return; }
      if (poll.timer) clearInterval(poll.timer);
      document.removeEventListener("visibilitychange", poll.onVisibility);
      sharedPolls.delete(key);
    };
  }, [pollFor, id, intervalMs, key]);
  const snapshot = useSyncExternalStore(subscribe, () => sharedPolls.get(key)?.snapshot ?? EMPTY_POLL);
  const refresh = useCallback(() => { const poll = sharedPolls.get(key); if (poll) sharedFetch(poll); }, [key]);
  return { data: snapshot.data as T | null, error: snapshot.error, loading: snapshot.loading, refresh };
}

const EMPTY_POLL = { data: null, error: null, loading: true };

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
