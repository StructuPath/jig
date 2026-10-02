// lanes.test.ts — the fold itself, without a DOM: a tool call's span ends at
// the next event in its phase, a phase's span ends at its phase_end, and a
// terminal attempt closes whatever the trace left open.
import { describe, expect, it } from "vitest";
import { buildLane, laneEnd, spanDuration } from "./lanes";
import type { Attempt, TraceEvent } from "./types";

const BASE = Date.parse("2026-08-06T10:00:00.000Z");
const at = (offsetMs: number) => new Date(BASE + offsetMs).toISOString();

const attempt: Attempt = {
  id: "attempt-1",
  job_id: "job-1",
  attempt_number: 1,
  state: "running",
  created_at: at(0),
  started_at: at(0),
};

function event(seq: number, type: string, offsetMs: number, extra: Partial<TraceEvent> = {}) {
  return { seq, type, started_at: at(offsetMs), ...extra } as TraceEvent;
}

describe("buildLane", () => {
  it("folds each tool call into one span ending at the next event in its phase", () => {
    const lane = buildLane(attempt, [
      event(1, "phase_start", 0, { phase: "plan" }),
      event(2, "tool_call", 1_000, { phase: "plan", name: "Read" }),
      event(3, "tool_call", 4_000, { phase: "plan", name: "Write" }),
      event(4, "handoff", 6_000, { phase: "plan", payload: { summary: "done" } }),
      event(5, "phase_end", 6_500, { phase: "plan", payload: { status: "success" } }),
    ]);

    expect(lane.tools).toHaveLength(2);
    expect(spanDuration(lane.tools[0], BASE + 99_999)).toBe(3_000);
    expect(spanDuration(lane.tools[1], BASE + 99_999)).toBe(2_000);
    expect(lane.phases).toHaveLength(1);
    expect(lane.phases[0].status).toBe("success");
    expect(spanDuration(lane.phases[0], BASE)).toBe(6_500);
    expect(lane.marks.map((mark) => mark.kind)).toEqual(["handoff"]);
  });

  it("leaves the last tool call open while the attempt runs, and measures it against now", () => {
    const lane = buildLane(attempt, [
      event(1, "phase_start", 0, { phase: "build" }),
      event(2, "tool_call", 2_000, { phase: "build", name: "Bash" }),
    ]);
    expect(lane.tools[0].endMs).toBeNull();
    expect(spanDuration(lane.tools[0], BASE + 12_000)).toBe(10_000);
    expect(laneEnd(lane, BASE + 12_000)).toBe(BASE + 12_000);
    expect(lane.endMs).toBeNull();
  });

  it("closes open spans when the attempt reached a terminal state", () => {
    const lane = buildLane(
      { ...attempt, state: "failed", completed_at: at(9_000) },
      [
        event(1, "phase_start", 0, { phase: "build" }),
        event(2, "tool_call", 2_000, { phase: "build", name: "Bash" }),
      ],
    );
    expect(lane.endMs).toBe(BASE + 9_000);
    expect(lane.tools[0].endMs).toBe(BASE + 9_000);
    expect(lane.phases[0].endMs).toBe(BASE + 9_000);
    expect(lane.phases[0].status).toBe("stopped");
  });

  it("uses the recorded failure when a boundary breach omits the phase-end event", () => {
    const lane = buildLane({
      ...attempt, state: "failed", completed_at: at(9_000),
      result: JSON.stringify({ phases: [
        { phase: "survey", status: "success", phase_attempt: 1 },
        { phase: "survey", status: "fail", phase_attempt: 2 },
      ] }),
    }, [event(1, "phase_start", 0, { phase: "survey" })]);
    expect(lane.phases[0].status).toBe("fail");
    expect(lane.phases[0].endMs).toBe(BASE + 9_000);
  });

  it("keeps gate and error marks on the axis", () => {
    const lane = buildLane(attempt, [
      event(1, "phase_start", 0, { phase: "test" }),
      event(2, "gate_fail", 3_000, { phase: "test", name: "tests_pass" }),
      event(3, "error", 4_000, { phase: "test", name: "trace_gap", payload: { reason: "overflow" } }),
    ]);
    expect(lane.marks.map((mark) => mark.kind)).toEqual(["gate_fail", "error"]);
    expect(lane.marks[1].detail).toBe("overflow");
  });
});
