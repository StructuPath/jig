// status.ts — the one place a job's display status is derived. The job list,
// the board card and column, the run header, and the job badge all read it,
// so one job never reads "Unpublished" in one view and "Needs review" or
// "Done" in another. The latest attempt carries the reason a job is
// unpublished (held, nothing to publish, kept local, delivery stopped), so
// the status is folded from the attempt, and from the job state only before
// any attempt exists.
import { parseAttemptSummary } from "./api";
import { publishAction } from "./outcomes";
import type { Attempt, Delivery, JobDetail } from "./types";

export type Tone = "good" | "live" | "warn" | "bad" | "idle";
export type ColumnID = "waiting" | "running" | "review" | "done" | "stopped";

export interface JobStatus {
  word: string;
  tone: Tone;
  /** The board column the job's run lands in. */
  column: ColumnID;
}

/**
 * runTone is the colour of a run's own state, for a run over several jobs
 * that no single job status speaks for. It matches the badge dot colours,
 * so the job list, the board card and the run header agree on such a run.
 */
export function runTone(state: string): Tone {
  if (["accepted", "complete", "ready"].includes(state)) return "good";
  if (["failed", "lost"].includes(state)) return "bad";
  if (["mixed", "accepted_unpublished"].includes(state)) return "warn";
  if (state === "active") return "live";
  return "idle";
}

/** isLocalDelivery is the task form's own hold: work kept local on purpose. */
export function isLocalDelivery(delivery: Delivery | undefined): boolean {
  return (delivery?.hold_when ?? "").includes("jig_ui_delivery");
}

export function jobStatus({ jobState, attempt, delivery, ask = false }: {
  jobState?: string;
  attempt?: Attempt;
  delivery?: Delivery;
  /** An answer-only task: kept-local work is complete, not a change to review. */
  ask?: boolean;
}): JobStatus {
  if (!attempt) {
    if (jobState === "active") return { word: "Running", tone: "live", column: "running" };
    if (jobState === "cancelled") return { word: "Cancelled", tone: "idle", column: "stopped" };
    if (jobState === "failed") return { word: "Stopped", tone: "bad", column: "stopped" };
    return { word: "Queued", tone: "idle", column: "waiting" };
  }
  const publish = parseAttemptSummary(attempt.result)?.publish;
  const outcome = typeof publish === "string" ? { state: publish, code: undefined } : { state: publish?.state, code: publish?.code };
  switch (attempt.state) {
    case "queued": return { word: "Queued", tone: "idle", column: "waiting" };
    case "preparing": return { word: "Preparing", tone: "live", column: "running" };
    case "running": return { word: "Running", tone: "live", column: "running" };
    case "accepted": return { word: "Accepted", tone: "good", column: "done" };
    case "accepted_unpublished":
      if (outcome.state === "held" && isLocalDelivery(delivery)) {
        return ask ? { word: "Complete", tone: "good", column: "done" } : { word: "Ready to review", tone: "good", column: "review" };
      }
      // A hold and a delivery that stopped short both wait on a person; the
      // checks passed, so neither is a stop.
      if (outcome.state === "held") return { word: "Held", tone: "warn", column: "review" };
      if (outcome.code === "publish_empty_changeset" || outcome.state === "not_attempted") return { word: "Done", tone: "good", column: "done" };
      return { word: "Not published", tone: "warn", column: "review" };
    case "cancelled": return { word: "Cancelled", tone: "idle", column: "stopped" };
    case "lost": return { word: "Lost", tone: "bad", column: "stopped" };
    default: return { word: "Stopped", tone: "bad", column: "stopped" };
  }
}

/**
 * jobOpen says whether a job's status can still change while its run state
 * stays put: it is queued or running, a publish request waits on its worker,
 * or it is unpublished work an operator can still release or retry. A publish
 * retry leaves the run `mixed`, so a view that stops polling on the run state
 * alone keeps saying "Held" after the job has moved to "Not published".
 */
export function jobOpen(detail: Pick<JobDetail, "job" | "attempts" | "delivery">): boolean {
  if (["queued", "active"].includes(detail.job.state)) return true;
  if (detail.job.publish_requested_at) return true;
  return publishAction(detail.job.state, detail.attempts?.at(-1) ?? undefined, detail.delivery) !== null;
}
