import { describe, expect, it } from "vitest";
import { parseAttemptSummary } from "./api";
import { attemptOutcome } from "./outcomes";
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
