// summary.test.ts — rows from the run list's job summaries, the Jobs list's
// attention order, and its filters.
import { describe, expect, it } from "vitest";
import { jobStatus } from "./status";
import { runColumnOf, runIsOpen, runJobStatus, runMatches, sortByAttention, summaryAttempt, summaryDetail } from "./summary";
import type { JobSummary, RunListItem } from "./types";

const delivery = { hold_when: "risk != low", ci_wait: true };

function summary(runID: string, jobState: string, attempt?: Partial<NonNullable<JobSummary["latest_attempt"]>>, extra: Partial<JobSummary> = {}): JobSummary {
  return {
    job: { id: `job-${runID}`, run_id: runID, repository: "github.com/owner/repo", base_sha: "abc", state: jobState, cancellation_requested: false, created_at: "2026-11-10T08:00:00Z", updated_at: "2026-11-10T08:00:00Z" },
    delivery, attempt_count: attempt ? 1 : 0,
    latest_attempt: attempt ? { id: `a-${runID}`, job_id: `job-${runID}`, attempt_number: 1, state: "queued", created_at: "2026-11-10T08:00:00Z", ...attempt } : undefined,
    phases: [], publish: [], ...extra,
  };
}

function run(id: string, state: string, created: string, jobs: JobSummary[], overrides: Partial<RunListItem> = {}): RunListItem {
  return {
    id, definition_id: "d", definition_generation: 1, snapshot: "name: factory\n",
    parameters: { task_title: `Task ${id}` }, targets: [{ repository: "github.com/owner/repo", base_sha: "abc" }],
    state, created_at: created, updated_at: created, job_summaries: jobs, ...overrides,
  };
}

describe("summaryAttempt", () => {
  it("rebuilds the slice of the result the status and station folds read", () => {
    const attempt = summaryAttempt({
      id: "a", job_id: "j", attempt_number: 2, state: "accepted_unpublished", created_at: "2026-11-10T08:00:00Z",
      publish_state: "held", publish_code: "publish_held", publish_hold: "held (risk = high)",
      result_excerpt: "{\"phases\":[", result_truncated: true, error_excerpt: "push failed",
    }, [{ phase: "plan", status: "success", rounds: 1 }, { phase: "build", status: "fail", rounds: 3 }]);
    expect(JSON.parse(attempt.result!)).toEqual({
      phases: [{ phase: "plan", kind: "", status: "success", phase_attempt: 1 }, { phase: "build", kind: "", status: "fail", phase_attempt: 3 }],
      publish: { state: "held", code: "publish_held" },
      publish_hold: "held (risk = high)",
    });
    expect(attempt.error).toBe("push failed");
    expect(attempt).not.toHaveProperty("result_excerpt");
    expect(attempt).not.toHaveProperty("publish_state");
  });

  it("gives a summary the same status its job detail gives", () => {
    const cases: [string, Partial<NonNullable<JobSummary["latest_attempt"]>>, string][] = [
      ["accepted_unpublished", { state: "accepted_unpublished", publish_state: "held", publish_code: "publish_held" }, "Held"],
      ["accepted_unpublished", { state: "accepted_unpublished", publish_state: "failed", publish_code: "publish_empty_changeset" }, "Done"],
      ["accepted_unpublished", { state: "accepted_unpublished", publish_state: "failed", publish_code: "push_failed" }, "Not published"],
      ["accepted", { state: "accepted", publish_state: "published" }, "Accepted"],
      ["failed", { state: "failed" }, "Stopped"],
      ["active", { state: "running" }, "Running"],
    ];
    for (const [jobState, attempt, word] of cases) {
      const detail = summaryDetail(summary("r", jobState, attempt));
      expect(jobStatus({ jobState, attempt: detail.attempts![0], delivery }).word).toBe(word);
    }
  });
});

describe("runJobStatus and runIsOpen", () => {
  it("speaks for a one-job run only", () => {
    expect(runJobStatus(run("r", "active", "2026-11-10T08:00:00Z", [summary("r", "queued")]))?.word).toBe("Queued");
    expect(runJobStatus(run("r", "active", "2026-11-10T08:00:00Z", [summary("r", "queued"), summary("s", "queued")]))).toBeUndefined();
    expect(runJobStatus(run("r", "active", "2026-11-10T08:00:00Z", []))).toBeUndefined();
  });

  it("keeps a settled run open while its job can still change", () => {
    const held = run("r", "mixed", "2026-11-10T08:00:00Z", [summary("r", "accepted_unpublished", { state: "accepted_unpublished", publish_state: "held" })]);
    const done = run("r", "accepted", "2026-11-10T08:00:00Z", [summary("r", "accepted", { state: "accepted" })]);
    expect(runIsOpen(held)).toBe(true);
    expect(runIsOpen(done)).toBe(false);
  });
});

describe("sortByAttention", () => {
  const at = (hour: number) => `2026-11-10T${String(hour).padStart(2, "0")}:00:00Z`;
  const runs = [
    run("done-new", "accepted", at(9), [summary("done-new", "accepted", { state: "accepted" })]),
    run("running", "active", at(8), [summary("running", "active", { state: "running" })]),
    run("waiting", "active", at(7), [summary("waiting", "queued")]),
    run("stopped-old", "failed", at(1), [summary("stopped-old", "failed", { state: "failed" })]),
    run("held", "mixed", at(2), [summary("held", "accepted_unpublished", { state: "accepted_unpublished", publish_state: "held" })]),
    run("stopped-new", "failed", at(6), [summary("stopped-new", "failed", { state: "failed" })]),
    run("unpublished", "mixed", at(3), [summary("unpublished", "accepted_unpublished", { state: "accepted_unpublished", publish_state: "failed", publish_code: "push_failed" })]),
    run("done-old", "accepted", at(0), [summary("done-old", "accepted", { state: "accepted" })]),
  ];

  it("puts what needs a person first and settled work last, newest first within a group", () => {
    expect(sortByAttention(runs).map((item) => item.id)).toEqual([
      "unpublished", "held", "stopped-new", "stopped-old", "running", "waiting", "done-new", "done-old",
    ]);
  });

  it("does not reorder its input", () => {
    const before = runs.map((item) => item.id);
    sortByAttention(runs);
    expect(runs.map((item) => item.id)).toEqual(before);
  });

  it("groups a run over several jobs by its run state", () => {
    const many = run("many", "active", at(5), [summary("many", "queued"), summary("many-2", "active", { state: "running" })]);
    expect(runColumnOf(many)).toBe("running");
    const allQueued = run("q", "active", at(5), [summary("q", "queued"), summary("q2", "queued")]);
    expect(runColumnOf(allQueued)).toBe("waiting");
  });
});

describe("runMatches", () => {
  const item = run("01HXYZRUN", "failed", "2026-11-10T08:00:00Z", [summary("01HXYZRUN", "failed", { state: "failed" })], {
    snapshot: "name: nightly-audit\n", parameters: { task_title: "Fix the flaky retry test\n\nMore detail" },
    targets: [{ repository: "/Users/v/dev/structupath/jig", base_sha: "abc" }],
  });
  const none = { status: null, repo: "", q: "" };

  it("matches the title, workflow, repository and an ID prefix, ignoring case", () => {
    expect(runMatches(item, { ...none, q: "FLAKY" })).toBe(true);
    expect(runMatches(item, { ...none, q: "nightly" })).toBe(true);
    expect(runMatches(item, { ...none, q: "structupath/jig" })).toBe(true);
    expect(runMatches(item, { ...none, q: "01hxyz" })).toBe(true);
    expect(runMatches(item, { ...none, q: "job-01HX" })).toBe(true);
    expect(runMatches(item, { ...none, q: "HXYZ" })).toBe(false);
    expect(runMatches(item, { ...none, q: "deploy" })).toBe(false);
  });

  it("applies the status and repository filters", () => {
    expect(runMatches(item, { ...none, status: "stopped" })).toBe(true);
    expect(runMatches(item, { ...none, status: "done" })).toBe(false);
    expect(runMatches(item, { ...none, repo: "/Users/v/dev/structupath/jig" })).toBe(true);
    expect(runMatches(item, { ...none, repo: "github.com/owner/repo" })).toBe(false);
  });
});
