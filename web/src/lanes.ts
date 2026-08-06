// lanes.ts — the fold from an attempt's seq-ordered event stream to one swim
// lane on a time axis. Pure, so the shape of a lane is testable without a DOM.
//
// Two rules carry the plan's intent:
//
//   - A tool call folds to ONE row with a real wall-clock span. The engine
//     emits a tool_call at the moment the call starts; its end is the next
//     event in the same phase (the agent cannot be doing two things at once
//     in one session). A tool call that has no successor yet is still running
//     — which is exactly what makes durations visible live, before the phase
//     has an envelope.
//   - Everything is derived from the events alone. A lane renders from
//     whatever has arrived so far; nothing waits for a terminal state.
import { parseTime } from "./format";
import type { Attempt, TraceEvent } from "./types";

export interface Span {
  key: string;
  label: string;
  phase: string;
  startMs: number;
  /** null while the span is still open (running). */
  endMs: number | null;
  status?: string;
  kind: "phase" | "tool";
}

export interface Mark {
  key: string;
  label: string;
  phase: string;
  atMs: number;
  kind: "gate_pass" | "gate_fail" | "error" | "handoff";
  detail?: string;
}

export interface Lane {
  attemptId: string;
  attemptNumber: number;
  state: string;
  startMs: number;
  /** null while the attempt is still running. */
  endMs: number | null;
  phases: Span[];
  tools: Span[];
  marks: Mark[];
  eventCount: number;
}

function eventTime(event: TraceEvent, fallback: number): number {
  return parseTime(event.started_at) ?? fallback;
}

function payloadField(event: TraceEvent, field: string): string | undefined {
  const payload = event.payload;
  if (!payload || typeof payload !== "object") return undefined;
  const value = (payload as Record<string, unknown>)[field];
  return typeof value === "string" ? value : undefined;
}

/**
 * buildLane folds one attempt's events into a lane. `attempt` supplies the
 * fallback bounds so a lane exists (and is honest) even before its first
 * event has been ingested.
 */
export function buildLane(attempt: Attempt, events: TraceEvent[]): Lane {
  const ordered = [...events].sort((left, right) => left.seq - right.seq);
  const attemptStart =
    parseTime(attempt.started_at) ?? parseTime(attempt.created_at) ?? 0;
  const firstEvent = ordered.length ? eventTime(ordered[0], attemptStart) : attemptStart;
  const startMs = Math.min(attemptStart || firstEvent, firstEvent);

  const phases: Span[] = [];
  const tools: Span[] = [];
  const marks: Mark[] = [];
  const openPhases = new Map<string, Span>();
  let openTool: Span | null = null;
  let cursor = startMs;

  const closeTool = (at: number) => {
    if (openTool && openTool.endMs === null) openTool.endMs = Math.max(at, openTool.startMs);
    openTool = null;
  };

  for (const event of ordered) {
    const at = eventTime(event, cursor);
    cursor = Math.max(cursor, at);
    const phase = event.phase ?? "";

    // Any event in a phase ends the tool call that was running there: the
    // agent moved on, so the call is over. This is the fold that gives a tool
    // call a real span instead of a zero-width tick.
    if (openTool && (openTool.phase === phase || event.type === "phase_end")) {
      closeTool(at);
    }

    switch (event.type) {
      case "phase_start": {
        const span: Span = {
          key: `phase-${event.seq}`,
          label: phase,
          phase,
          startMs: at,
          endMs: null,
          kind: "phase",
        };
        phases.push(span);
        openPhases.set(phase, span);
        break;
      }
      case "phase_end":
      case "phase_death": {
        const span = openPhases.get(phase);
        if (span) {
          span.endMs = at;
          span.status =
            event.type === "phase_death" ? "death" : payloadField(event, "status") ?? "unknown";
          openPhases.delete(phase);
        }
        if (event.type === "phase_death") {
          marks.push({
            key: `mark-${event.seq}`,
            label: "phase death",
            phase,
            atMs: at,
            kind: "error",
            detail: payloadField(event, "cause") ?? payloadField(event, "error"),
          });
        }
        break;
      }
      case "tool_call": {
        const span: Span = {
          key: `tool-${event.seq}`,
          label: event.name ?? "tool",
          phase,
          startMs: at,
          endMs: null,
          kind: "tool",
        };
        tools.push(span);
        openTool = span;
        break;
      }
      case "gate_pass":
      case "gate_fail":
        marks.push({
          key: `mark-${event.seq}`,
          label: event.name ?? "gate",
          phase,
          atMs: at,
          kind: event.type,
        });
        break;
      case "handoff":
        marks.push({
          key: `mark-${event.seq}`,
          label: "handoff",
          phase,
          atMs: at,
          kind: "handoff",
          detail: payloadField(event, "summary"),
        });
        break;
      case "error":
        marks.push({
          key: `mark-${event.seq}`,
          label: event.name ?? "error",
          phase,
          atMs: at,
          kind: "error",
          detail: payloadField(event, "error") ?? payloadField(event, "reason"),
        });
        break;
      default:
        break;
    }
  }

  const completed = parseTime(attempt.completed_at);
  if (completed !== null) {
    closeTool(Math.max(completed, cursor));
    for (const span of openPhases.values()) {
      if (span.endMs === null) span.endMs = Math.max(completed, cursor);
    }
  }

  return {
    attemptId: attempt.id,
    attemptNumber: attempt.attempt_number,
    state: attempt.state,
    startMs,
    endMs: completed,
    phases,
    tools,
    marks,
    eventCount: ordered.length,
  };
}

/** laneEnd is the right edge of a lane's axis: its end, or now while live. */
export function laneEnd(lane: Lane, nowMs: number): number {
  if (lane.endMs !== null) return Math.max(lane.endMs, lane.startMs + 1);
  const latest = Math.max(
    lane.startMs,
    ...lane.phases.map((span) => span.endMs ?? span.startMs),
    ...lane.tools.map((span) => span.endMs ?? span.startMs),
    ...lane.marks.map((mark) => mark.atMs),
  );
  return Math.max(nowMs, latest, lane.startMs + 1);
}

/** spanDuration is a span's wall clock, live spans measured against now. */
export function spanDuration(span: Span, nowMs: number): number {
  return Math.max(0, (span.endMs ?? nowMs) - span.startMs);
}

/** offsetPercent/widthPercent place a span on a shared 0-100 axis. */
export function offsetPercent(span: Span, startMs: number, endMs: number): number {
  const total = Math.max(1, endMs - startMs);
  return Math.min(100, Math.max(0, ((span.startMs - startMs) / total) * 100));
}

export function widthPercent(span: Span, startMs: number, endMs: number, nowMs: number): number {
  const total = Math.max(1, endMs - startMs);
  const width = (spanDuration(span, nowMs) / total) * 100;
  // A just-started call still needs to be visible and clickable.
  return Math.min(100, Math.max(1.5, width));
}
