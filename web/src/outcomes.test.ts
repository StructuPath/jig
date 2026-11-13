import { describe, expect, it } from "vitest";
import { parseAttemptSummary } from "./api";
import { attemptOutcome, publishAction } from "./outcomes";
import type { Attempt } from "./types";

function attempt(overrides: Partial<Attempt>): Attempt {
  return { id: "a", job_id: "j", attempt_number: 1, state: "failed", created_at: "2026-08-07T14:20:59Z", ...overrides };
}

describe("trace-based outcomes", () => {
  it("distinguishes an empty publication from an unsuccessful assessment", () => {
    const result = JSON.stringify({
      acceptance: { passed: true },
      publish: { state: "failed", code: "publish_empty_changeset" },
      phases: [{ phase: "assess", status: "success", envelope: { approved: false, summary: "Setup is broken." } }],
    });
    const outcome = attemptOutcome(attempt({ state: "accepted_unpublished", result }));
    expect(outcome.title).toBe("Checks passed · nothing to publish");
    expect(outcome.next).toContain("Read the assessment");
    expect(outcome.next).toContain("retrying publication alone will not fix");
  });

  it("uses a recorded boundary breach over an earlier subprocess error", () => {
    const outcome = attemptOutcome(attempt({
      error: 'phase "survey": role "surveyor" modified 1 path(s) outside its write allowlist: scripts/__pycache__/ — deleted',
      result: JSON.stringify({ phases: [{ phase: "survey", status: "fail", error: "agent subprocess: claude exited" }] }),
    }));
    expect(outcome.title).toBe("Stopped at the write boundary");
    expect(outcome.next).toContain("remove commands that write caches");
  });

  it("keeps human holds distinct from publication errors and direct runs", () => {
    expect(attemptOutcome(attempt({ state: "accepted_unpublished", result: JSON.stringify({ publish: { state: "held" } }) })).title).toContain("awaiting review");
    expect(attemptOutcome(attempt({ state: "accepted_unpublished", result: JSON.stringify({ publish: "not_attempted" }) })).title).toContain("not attempted");
    expect(attemptOutcome(attempt({ state: "accepted_unpublished", error: "push failed", result: JSON.stringify({ publish: { state: "failed", code: "publish_push" } }) })).title).toContain("incomplete");
  });

  it("does not crash or hide the original record when a result is malformed", () => {
    for (const result of ["broken JSON", "null", "[]", '{"phases":{}}', '{"phases":[null]}', '{"acceptance":{"checks":"bad"}}']) {
      expect(parseAttemptSummary(result)).toBeNull();
      expect(attemptOutcome(attempt({ result })).title).toBe("Work did not pass");
    }
  });
});

describe("the publish action", () => {
  const unpublished = (result: string) => attempt({ state: "accepted_unpublished", result });

  it("is a release for a hold and a retry for a publish that failed", () => {
    expect(publishAction("accepted_unpublished", unpublished(JSON.stringify({ publish: { state: "held" } })))?.label).toBe("Release hold");
    expect(publishAction("accepted_unpublished", unpublished(JSON.stringify({ publish: { state: "failed", code: "ci_failed" } })))?.label).toBe("Retry publication");
  });

  it("is absent where a publish-only retry cannot help", () => {
    const keptLocal = JSON.stringify({ publish: "held", publish_hold: 'publish.hold_when "jig_ui_delivery != publish" held' });
    expect(publishAction("accepted_unpublished", unpublished(keptLocal))).toBeNull();
    expect(publishAction("accepted_unpublished", unpublished(JSON.stringify({ publish: { state: "failed", code: "publish_empty_changeset" } })))).toBeNull();
    expect(publishAction("accepted_unpublished", unpublished(JSON.stringify({ publish: "not_attempted" })))).toBeNull();
  });

  it("is absent unless both the job and its latest attempt are unpublished", () => {
    const held = unpublished(JSON.stringify({ publish: { state: "held" } }));
    expect(publishAction("active", held)).toBeNull();
    expect(publishAction("accepted", held)).toBeNull();
    expect(publishAction("accepted_unpublished", undefined)).toBeNull();
    expect(publishAction("accepted_unpublished", attempt({ state: "running" }))).toBeNull();
  });

  it("states that retry can merge only never-held work under a merge policy", () => {
    const failed = unpublished(JSON.stringify({ publish: { state: "failed", code: "merge_blocked" } }));
    const delivery = { ci_wait: true, merge_method: "squash" };
    expect(publishAction("accepted_unpublished", failed, delivery)?.consequence).toContain("may merge");
    expect(publishAction("accepted_unpublished", { ...failed, publish_hold: "risk" }, delivery)?.consequence).not.toContain("may merge");
  });
});
