import { parseAttemptSummary } from "./api";
import type { Attempt } from "./types";

export function attemptOutcome(attempt: Attempt): { title: string; detail: string; next: string } {
  const summary = parseAttemptSummary(attempt.result);
  const publish = summary?.publish;
  const code = publish && typeof publish === "object" ? publish.code : undefined;

  if (attempt.state === "queued") return {
    title: "Waiting for a worker", detail: "This attempt has not started.",
    next: "Check Workers for an online worker with the workflow's runtime and a free slot.",
  };
  if (attempt.state === "preparing") return {
    title: "Preparing the repository", detail: "The worker is setting up an isolated worktree.",
    next: "Wait for the first phase to start; the page updates automatically.",
  };
  if (attempt.state === "running") return {
    title: "Work is running", detail: "The worker is executing the workflow and its checks.",
    next: "Follow the current phase below. Failed checks can trigger the workflow's bounded repair loop.",
  };
  if (attempt.state === "accepted") return {
    title: "Work accepted", detail: "This attempt passed Jig's acceptance checks.",
    next: "Review the agent's result and any published pull request below.",
  };
  if (attempt.state === "accepted_unpublished") {
    if (summary?.publish_hold?.startsWith('publish.hold_when "jig_ui_delivery != publish" held') &&
      (publish === "held" || (typeof publish === "object" && publish?.state === "held"))) return {
      title: "Task complete · kept local",
      detail: "The task passed its checks. Any changes remain in the separate working folder on this Mac; nothing was pushed to GitHub.",
      next: "Read the result below. For a build task, review the saved changes before applying them, or start the task again with Open a pull request.",
    };
    if (code === "publish_empty_changeset" || attempt.error?.includes("(publish_empty_changeset)")) return {
      title: "Checks passed · nothing to publish",
      detail: "The workflow passed its acceptance checks, but the worktree had no new commit to publish. Read-only assessments can legitimately produce no changes.",
      next: "Read the assessment below. If the task required edits, review why the builder produced no changes; retrying publication alone will not fix that.",
    };
    if (publish === "held" || (typeof publish === "object" && publish?.state === "held")) return {
      title: "Checks passed · awaiting review",
      detail: "The workflow deliberately held publication for human review.",
      next: "Review the result and retained worktree before releasing the publication hold.",
    };
    if (publish === "not_attempted" && !attempt.error) return {
      title: "Checks passed · publication not attempted",
      detail: "The recorded result does not include a completed publication step.",
      next: "Read the result below; a direct run does not publish a branch or pull request.",
    };
    return {
      title: "Checks passed · publication incomplete",
      detail: "The coding workflow passed, but delivery did not finish. The diagnostic below records where it stopped.",
      next: "Resolve the publication or CI problem before retrying publication; rerunning the coding workflow is a separate action.",
    };
  }
  if (attempt.state === "cancelled") return {
    title: "Work cancelled", detail: "This attempt stopped without an accepted result.",
    next: "Inspect the recorded work before submitting a new task.",
  };
  if (attempt.state === "lost") return {
    title: "Worker connection lost", detail: "Jig stopped receiving the worker's heartbeat.",
    next: "Check Workers and the retained worktree. Retry the failed job once the worker is available.",
  };
  if (attempt.error?.includes("outside its write allowlist")) return {
    title: "Stopped at the write boundary",
    detail: "The agent modified a path the workflow did not allow. Jig stopped the attempt; the diagnostic records its rollback result.",
    next: "For a read-only task, remove commands that write caches or files. For an editing task, check the permitted paths before submitting a revised workflow.",
  };
  const failedPhase = summary?.phases?.slice().reverse().find((phase) => phase.status === "fail");
  return {
    title: failedPhase ? `Stopped in ${failedPhase.phase}` : "Work did not pass",
    detail: "The attempt did not meet Jig's acceptance requirements. Inspect the diagnostic and failed checks below.",
    next: "Fix the cause before retrying. A retry uses the same frozen workflow; prompt, model, or workflow changes require a new run.",
  };
}
