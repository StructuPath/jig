// board.test.ts — the column mapping of 2026-10-02-001 KTD2, and the R4
// invariant that every run lands in exactly one column whatever its state.
import { describe, expect, it } from "vitest";
import { buildBoard, columns } from "./board";
import type { Column } from "./board";
import type { QueueEntry, Run } from "./types";

const HELD = 'hold_when: "jig_ui_delivery != publish"';

function run(id: string, state: string, overrides: Partial<Run> = {}): Run {
  return {
    id,
    definition_id: "definition-1",
    definition_generation: 1,
    snapshot: "",
    targets: [{ repository: "/tmp/project", base_sha: "abc" }],
    state,
    created_at: "2026-10-02T10:00:00Z",
    updated_at: "2026-10-02T10:00:00Z",
    ...overrides,
  };
}

function entry(runID: string, state: string, jobID = `${runID}-job`): QueueEntry {
  return { job_id: jobID, run_id: runID, repository: "/tmp/project", base_sha: "abc", state, enqueued_at: "2026-10-02T10:00:00Z" };
}

function placed(board: Column[]): Record<string, string[]> {
  return Object.fromEntries(board.map((column) => [column.id, column.runs.map((item) => item.id)]));
}

function expectEveryRunOnce(board: Column[], runs: Run[]) {
  const ids = board.flatMap((column) => column.runs.map((item) => item.id));
  expect(ids).toHaveLength(runs.length);
  expect(new Set(ids)).toEqual(new Set(runs.map((item) => item.id)));
}

describe("buildBoard", () => {
  it("places one run per state in its column", () => {
    const runs = [
      run("waiting", "active"),
      run("running", "active"),
      run("accepted", "accepted"),
      run("failed", "failed"),
      run("cancelled", "cancelled"),
    ];
    const board = buildBoard(runs, [entry("waiting", "queued"), entry("running", "active")]);
    expect(placed(board)).toEqual({
      waiting: ["waiting"],
      running: ["running"],
      review: [],
      done: ["accepted"],
      stopped: ["failed", "cancelled"],
    });
    expectEveryRunOnce(board, runs);
  });

  it("shows a multi-repo run Running when any of its jobs is active", () => {
    const runs = [run("multi", "active")];
    const board = buildBoard(runs, [entry("multi", "queued", "job-a"), entry("multi", "active", "job-b")]);
    expect(placed(board).running).toEqual(["multi"]);
    expect(placed(board).waiting).toEqual([]);
  });

  it("falls back to Running for an active run the queue does not list", () => {
    const board = buildBoard([run("skew", "active")], [entry("other", "queued")]);
    expect(placed(board).running).toEqual(["skew"]);
  });

  it("degrades to Running for active runs when the queue poll failed", () => {
    const runs = [run("live", "active"), run("done", "accepted"), run("stopped", "failed")];
    const board = buildBoard(runs, null);
    expect(placed(board)).toMatchObject({ waiting: [], running: ["live"], done: ["done"], stopped: ["stopped"] });
    expectEveryRunOnce(board, runs);
  });

  it("reads held-publication runs through taskState", () => {
    const ask = run("ask", "mixed", { snapshot: HELD, parameters: { task_mode: "ask" } });
    const build = run("build", "mixed", { snapshot: HELD, parameters: { task_mode: "build" } });
    const board = buildBoard([ask, build], []);
    expect(placed(board).done).toEqual(["ask"]);
    expect(placed(board).review).toEqual(["build"]);
  });

  it("sends a plain mixed multi-target run to Needs review", () => {
    const mixed = run("mixed", "mixed", {
      snapshot: HELD,
      targets: [{ repository: "/tmp/a", base_sha: "abc" }, { repository: "/tmp/b", base_sha: "def" }],
    });
    expect(placed(buildBoard([mixed], [])).review).toEqual(["mixed"]);
  });

  it("sends an unrecognised state to Needs review rather than dropping it", () => {
    const runs = [run("paused", "paused"), run("ok", "accepted")];
    const board = buildBoard(runs, []);
    expect(placed(board).review).toEqual(["paused"]);
    expectEveryRunOnce(board, runs);
  });

  it("returns five empty columns in fixed order for no runs", () => {
    const board = buildBoard([], null);
    expect(board.map((column) => column.label)).toEqual(["Waiting", "Running", "Needs review", "Done", "Stopped"]);
    expect(board.map((column) => column.id)).toEqual(columns.map((column) => column.id));
    expect(board.every((column) => column.runs.length === 0)).toBe(true);
  });

  it("keeps the incoming order within a column", () => {
    const runs = [run("c", "failed"), run("a", "cancelled"), run("b", "failed")];
    expect(placed(buildBoard(runs, [])).stopped).toEqual(["c", "a", "b"]);
  });
});
