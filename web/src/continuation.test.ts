import { describe, expect, it } from "vitest";
import { buildContinuation } from "./continuation";
import { taskState } from "./format";
import type { JobDetail, Run } from "./types";

const HELD = 'publish:\n  hold_when: "jig_ui_delivery != publish"\n';

function run(parameters: Record<string, string>, state = "mixed"): Run {
  return { id: "run-0", definition_id: "d", definition_generation: 1, snapshot: HELD, parameters,
    targets: [{ repository: "github.com/example/project" }], state, created_at: "", updated_at: "" } as Run;
}

function job(publish: JobDetail["publish"] = null): JobDetail {
  return { job: { id: "job-0", repository: "github.com/example/project" }, run_id: "run-0",
    attempts: [], publish } as unknown as JobDetail;
}

describe("buildContinuation", () => {
  it("flags kept-local work as unreachable from the next task", () => {
    const continuation = buildContinuation(run({ task_title: "Fix it" }), job());
    expect(continuation.keptLocal).toBe(true);
    expect(continuation.ref).toBeUndefined();
    expect(continuation.context).toEqual([{ label: "Previous task", body: "Fix it" }]);
  });

  it("starts from the pushed branch, not the pull request step", () => {
    const continuation = buildContinuation(run({ task_title: "Fix it", task_delivery: "publish" }), job([
      { attempt_id: "a", step: "pull_request", branch: "", completed_at: "" },
      { attempt_id: "a", step: "push", branch: "jig/job-0/1", completed_at: "" },
    ]));
    expect(continuation.ref).toBe("jig/job-0/1");
    expect(continuation.keptLocal).toBe(false);
  });

  it("clips a long result below the server's section limit", () => {
    const long = { ...job(), attempts: [{ id: "a", result: JSON.stringify({ phases: [{ phase: "plan", status: "success", envelope: { summary: "x".repeat(40000) } }] }) }] } as unknown as JobDetail;
    const body = buildContinuation(run({ task_title: "t" }), long).context[1].body;
    expect(body.length).toBeLessThan(16 << 10);
    expect(body).toMatch(/\[truncated\]$/);
  });
});

describe("taskState", () => {
  it("shows a held local task as ready", () => {
    expect(taskState(run({ task_mode: "build" }))).toBe("ready");
  });

  it("keeps the real state when a pull-request task stopped short of publishing", () => {
    expect(taskState(run({ task_mode: "build", task_delivery: "publish" }))).toBe("mixed");
  });
});
