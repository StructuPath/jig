// status.test.ts — the one job status every view reads. An unpublished job
// that passed its checks waits on a person (amber, Needs review), a job with
// nothing to publish is done (green), and a job before its first attempt is
// read from the job state.
import { describe, expect, it } from "vitest";
import { buildBoard } from "./board";
import { jobStatus, runTone } from "./status";
import type { Attempt, Delivery, Run } from "./types";

const factory: Delivery = { hold_when: "risk != low", ci_wait: true };
const local: Delivery = { hold_when: "jig_ui_delivery != publish", ci_wait: false };

function attempt(state: string, publish?: unknown): Attempt {
  return {
    id: "attempt-1", job_id: "job-1", attempt_number: 1, state, created_at: "2026-11-14T10:00:00Z",
    result: publish === undefined ? undefined : JSON.stringify({ phases: [], publish }),
  };
}

describe("jobStatus", () => {
  it("names an unpublished job by why it is unpublished", () => {
    expect(jobStatus({ attempt: attempt("accepted_unpublished", { state: "failed", code: "push_failed" }), delivery: factory }))
      .toEqual({ word: "Not published", tone: "warn", column: "review" });
    expect(jobStatus({ attempt: attempt("accepted_unpublished", { state: "held", code: "publish_held" }), delivery: factory }))
      .toEqual({ word: "Held", tone: "warn", column: "review" });
    expect(jobStatus({ attempt: attempt("accepted_unpublished", { state: "skipped", code: "publish_empty_changeset" }), delivery: factory }))
      .toEqual({ word: "Done", tone: "good", column: "done" });
    expect(jobStatus({ attempt: attempt("accepted_unpublished", "not_attempted") }))
      .toEqual({ word: "Done", tone: "good", column: "done" });
  });

  it("reads work kept local as ready to review, or complete for an answer", () => {
    const kept = attempt("accepted_unpublished", { state: "held", code: "publish_held" });
    expect(jobStatus({ attempt: kept, delivery: local })).toEqual({ word: "Ready to review", tone: "good", column: "review" });
    expect(jobStatus({ attempt: kept, delivery: local, ask: true })).toEqual({ word: "Complete", tone: "good", column: "done" });
  });

  it("reads the job state before the first attempt", () => {
    expect(jobStatus({ jobState: "queued" })).toMatchObject({ word: "Queued", column: "waiting" });
    expect(jobStatus({ jobState: "active" })).toMatchObject({ word: "Running", column: "running" });
    expect(jobStatus({ jobState: "failed" })).toMatchObject({ word: "Stopped", tone: "bad", column: "stopped" });
  });

  it("puts a settled run in its job status's column, not its run state's", () => {
    const run = (id: string, state: string): Run => ({
      id, definition_id: "d", definition_generation: 1, snapshot: "", state,
      targets: [{ repository: "/tmp/p", base_sha: "abc" }], created_at: "2026-11-14T10:00:00Z", updated_at: "2026-11-14T10:00:00Z",
    });
    // Both runs are `mixed`; without the job status both would go to review.
    const done = jobStatus({ attempt: attempt("accepted_unpublished", "not_attempted") });
    const unpublished = jobStatus({ attempt: attempt("accepted_unpublished", { state: "failed" }), delivery: factory });
    const board = buildBoard([run("a", "mixed"), run("b", "mixed")], [], new Map([["a", done], ["b", unpublished]]));
    const placed = Object.fromEntries(board.map((column) => [column.id, column.runs.map((item) => item.id)]));
    expect(placed.done).toEqual(["a"]);
    expect(placed.review).toEqual(["b"]);
  });
});

describe("runTone", () => {
  it("colours a run over several jobs as its badge does", () => {
    expect(runTone("mixed")).toBe("warn");
    expect(runTone("accepted")).toBe("good");
    expect(runTone("failed")).toBe("bad");
    expect(runTone("cancelled")).toBe("idle");
  });
});
