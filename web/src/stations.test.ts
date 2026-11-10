import { describe, expect, it } from "vitest";
import { buildLane } from "./lanes";
import { buildFeed, buildStations, stationLabel, verdictFor } from "./stations";
import type { Attempt, Delivery, PublishRecord, Station, TraceEvent } from "./types";

const BASE = Date.parse("2026-11-10T08:00:00Z");
const at = (seconds: number) => new Date(BASE + seconds * 1000).toISOString();

const chain: Station[] = [
  { name: "plan", kind: "agent", owner: "planner", model: "sonnet" },
  { name: "build", kind: "agent", owner: "builder", model: "opus", gates: ["tests_intact"] },
  { name: "test", kind: "code" },
  { name: "review-security", kind: "agent", owner: "security-reviewer", model: "opus" },
  { name: "classify-risk", kind: "code" },
  { name: "review-risk", kind: "agent", owner: "risk-reviewer", model: "sonnet", if: "risk != low" },
];
const factory: Delivery = { hold_when: "risk != low", ci_wait: true };

function attempt(overrides: Partial<Attempt> = {}): Attempt {
  return { id: "attempt-1", job_id: "job-1", attempt_number: 1, state: "running", created_at: at(0), started_at: at(0), ...overrides };
}

function phase(name: string, start: number, end: number, status = "success"): TraceEvent[] {
  return [
    { seq: start * 10, type: "phase_start", phase: name, started_at: at(start) },
    { seq: end * 10 + 1, type: "phase_end", phase: name, started_at: at(end), payload: { status } },
  ];
}

function result(phases: { phase: string; status: string; envelope?: unknown }[], extra: object = {}): string {
  return JSON.stringify({ phases: phases.map((entry) => ({ kind: "agent", phase_attempt: 1, ...entry })), ...extra });
}

function fold(a: Attempt, events: TraceEvent[], publish: PublishRecord[] = [], delivery: Delivery | undefined = factory, nowMs = BASE + 600_000) {
  return buildStations(chain, delivery, a, buildLane(a, events), events, publish, nowMs);
}

const states = (views: { name: string; state: string }[]) => Object.fromEntries(views.map((view) => [view.name, view.state]));

describe("buildStations", () => {
  it("shows the whole chain while the second station is still running", () => {
    const events = [...phase("plan", 0, 12), { seq: 125, type: "phase_start", phase: "build", started_at: at(12) }];
    const views = fold(attempt(), events, [], factory, BASE + 60_000);
    expect(states(views)).toEqual({
      plan: "pass", build: "live", test: "wait", "review-security": "wait",
      "classify-risk": "wait", "review-risk": "wait", publish: "wait", ci: "wait",
    });
    expect(views[0].durationMs).toBe(12_000);
    expect(views[1].durationMs).toBe(48_000);
    expect(views[5].note).toBe("if risk != low");
  });

  it("sums spend and wall clock across the rounds of a repaired station", () => {
    const events: TraceEvent[] = [
      ...phase("build", 0, 60),
      { seq: 602, type: "agent_end", phase: "build", payload: { cost: 0.2 } },
      ...phase("test", 60, 70, "fail"),
      ...phase("build", 70, 100),
      { seq: 1002, type: "agent_end", phase: "build", payload: { cost: 0.1 } },
    ];
    const build = fold(attempt(), events).find((view) => view.name === "build");
    expect(build?.rounds).toBe(2);
    expect(build?.durationMs).toBe(90_000);
    expect(build?.costUSD).toBeCloseTo(0.3);
  });

  it("marks the station that reported the held field, and holds delivery behind it", () => {
    const held = attempt({
      state: "accepted_unpublished", completed_at: at(250),
      result: result([
        { phase: "plan", status: "success" }, { phase: "build", status: "success" }, { phase: "test", status: "success" },
        { phase: "review-security", status: "success", envelope: { approved: true } },
        { phase: "classify-risk", status: "success", envelope: { risk: "high" } },
        { phase: "review-risk", status: "success", envelope: { approved: true } },
      ], { publish: { state: "held", code: "publish_held" }, publish_hold: 'publish.hold_when "risk != low" held (risk = high)' }),
    });
    const views = fold(held, []);
    expect(states(views)).toMatchObject({ "classify-risk": "held", "review-risk": "pass", publish: "held", ci: "wait" });
    expect(views.find((view) => view.name === "classify-risk")?.note).toBe("risk high");
    expect(views.find((view) => view.name === "ci")?.note).toBe("held");
    expect(verdictFor(held, views, factory)).toEqual({ word: "Held", tone: "bad" });
  });

  it("does not alarm on the task form's own local delivery", () => {
    const local: Delivery = { hold_when: "jig_ui_delivery != publish", ci_wait: false };
    const kept = attempt({
      state: "accepted_unpublished", completed_at: at(90),
      result: result([{ phase: "plan", status: "success" }, { phase: "build", status: "success" }],
        { publish: { state: "held", code: "publish_held" }, publish_hold: 'publish.hold_when "jig_ui_delivery != publish" held (jig_ui_delivery = local)' }),
    });
    const views = buildStations(chain.slice(0, 2), local, kept, buildLane(kept, []), [], [], BASE);
    expect(states(views)).toEqual({ plan: "pass", build: "pass", publish: "skip" });
    expect(views.at(-1)?.note).toBe("kept local");
    expect(verdictFor(kept, views, local)).toEqual({ word: "Ready", tone: "good" });
  });

  it("stops at the station that failed and leaves the rest not run", () => {
    const failed = attempt({
      state: "failed", completed_at: at(100), error: "phase \"build\": role \"builder\" modified 1 path(s) outside its write allowlist",
      result: result([{ phase: "plan", status: "success" }, { phase: "build", status: "fail" }]),
    });
    const views = fold(failed, [...phase("plan", 0, 15), ...phase("build", 15, 100, "fail")]);
    expect(states(views)).toMatchObject({ plan: "pass", build: "bad", test: "wait", publish: "wait", ci: "wait" });
    expect(views.find((view) => view.name === "test")?.note).toBe("not run");
    expect(verdictFor(failed, views, factory)).toEqual({ word: "Stopped", tone: "bad" });
  });

  it("proves publish and CI from the publish records, with the pull request number", () => {
    const done = attempt({
      state: "accepted", completed_at: at(540),
      result: result(chain.map((station) => ({ phase: station.name, status: station.name === "review-risk" ? "skipped" : "success" })),
        { publish: { state: "published" } }),
    });
    const record = (step: string, seconds: number, extra: Partial<PublishRecord> = {}): PublishRecord =>
      ({ attempt_id: "attempt-1", step, branch: "jig/job-1/1", completed_at: at(seconds), ...extra });
    const publish = [
      record("push", 212), record("pull_request", 214, { pr_url: "https://github.com/StructuPath/jig/pull/46" }),
      record("proof", 216, { remote_ref: "1e7dfcf94402" }), record("ci", 540),
      // Another attempt's record never lights this attempt's stations.
      { attempt_id: "attempt-0", step: "proof", branch: "jig/job-1/0", completed_at: at(1) },
    ];
    const views = fold(done, phase("classify-risk", 205, 210), publish);
    const byName = Object.fromEntries(views.map((view) => [view.name, view]));
    expect(byName["review-risk"].state).toBe("skip");
    expect(byName.publish).toMatchObject({ state: "pass", note: "#46", durationMs: 6000 });
    expect(byName.ci).toMatchObject({ state: "pass", note: "green", durationMs: 324_000 });
    expect(verdictFor(done, views, factory)).toEqual({ word: "Accepted", tone: "good" });
  });

  it("shows no delivery stations for a job with no policy on record, and falls back to the trace", () => {
    const events = [...phase("scout", 0, 9)];
    const a = attempt();
    const views = buildStations(undefined, undefined, a, buildLane(a, events), events, [], BASE + 20_000);
    expect(views.map((view) => view.name)).toEqual(["scout"]);
    expect(views[0].label).toBe("read");
  });
});

describe("stationLabel", () => {
  it("shortens the stock phase names", () => {
    expect(["review-correctness", "classify-risk", "review-risk", "commit-build", "ci", "plan", "review-docs", "my-step"].map(stationLabel))
      .toEqual(["correct", "risk", "risk review", "commit", "CI", "plan", "docs", "my step"]);
  });
});

describe("buildFeed", () => {
  it("reads what a tool was pointed at and drops the task form's bookkeeping phases", () => {
    const feed = buildFeed([
      { seq: 1, type: "tool_call", phase: "build", name: "Bash", payload: { command: "just test\nmore", description: "Run tests" } },
      { seq: 2, type: "tool_call", phase: "build", name: "Grep", payload: { pattern: "ECONNREFUSED" } },
      { seq: 3, type: "log", phase: "build", name: "agent_text", payload: { text: "Tests pass." } },
      { seq: 4, type: "phase_end", phase: "review-security", payload: { status: "success" } },
      { seq: 5, type: "phase_start", phase: "keep-local" },
      { seq: 6, type: "gate_fail", phase: "build", name: "tests_intact" },
    ]);
    expect(feed.map((line) => [line.verb, line.detail])).toEqual([
      ["Bash", "just test"], ["Grep", "ECONNREFUSED"], ["say", "Tests pass."],
      ["done", "secure · success"], ["gate", "tests_intact failed in build"],
    ]);
  });

  it("prints an agent's closing envelope as its summary, never as raw JSON", () => {
    const feed = buildFeed([
      { seq: 1, type: "log", phase: "review-security", name: "agent_text",
        payload: { text: '{"status":"success","summary":"I approve commit 1e7dfcf.\\nNo defects.","approved":true}' } },
      { seq: 2, type: "log", phase: "build", name: "agent_text", payload: { text: "{not json, just prose in braces}" } },
    ]);
    expect(feed.map((line) => [line.verb, line.detail])).toEqual([
      ["report", "I approve commit 1e7dfcf."], ["say", "{not json, just prose in braces}"],
    ]);
  });

  it("keeps only the tail", () => {
    const events = Array.from({ length: 20 }, (_, index) => ({ seq: index, type: "tool_call", phase: "build", name: `T${index}` }));
    expect(buildFeed(events, 3).map((line) => line.verb)).toEqual(["T17", "T18", "T19"]);
  });
});
