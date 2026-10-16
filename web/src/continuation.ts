// continuation.ts — what a follow-up task needs from the task it continues.
// Every task is a fresh run, so a reply like "Reviewed. Let's keep going."
// reached the planner with nothing to continue and failed. A continuation
// hands the earlier directive and result over as untrusted context, and
// starts from the earlier task's pushed branch when there is one.
import { api, parseAttemptSummary } from "./api";
import { taskDelivery, taskTitle } from "./format";
import type { JobDetail, Run } from "./types";

// Each section is truncated server-side at 16 KiB; staying under it keeps the
// end of a long plan from being cut silently.
const maxSectionChars = 12000;

export interface Continuation {
  runID: string;
  repository: string;
  title: string;
  context: { label: string; body: string }[];
  // ref is the earlier task's pushed branch. Without it the follow-up starts
  // from the project's latest commit and cannot see the earlier changes.
  ref?: string;
  keptLocal: boolean;
}

function clip(text: string): string {
  return text.length <= maxSectionChars ? text : `${text.slice(0, maxSectionChars)}\n[truncated]`;
}

function envelopeText(envelope: unknown): string {
  if (!envelope || typeof envelope !== "object") return "";
  const { summary, notes_for_next_agent: notes } = envelope as Record<string, unknown>;
  return [typeof summary === "string" ? summary : "", typeof notes === "string" ? notes : ""]
    .filter(Boolean).join("\n\n");
}

export function buildContinuation(run: Run, job: JobDetail): Continuation {
  const latest = (job.attempts ?? []).at(-1);
  const summary = parseAttemptSummary(latest?.result);
  const phases = (summary?.phases ?? [])
    .map((phase) => ({ phase: phase.phase, text: envelopeText(phase.envelope) }))
    .filter((phase) => phase.text);
  const result = [
    ...phases.map((phase) => `## ${phase.phase}\n${phase.text}`),
    summary?.changed_paths?.length ? `## Changed files\n${summary.changed_paths.join("\n")}` : "",
  ].filter(Boolean).join("\n\n");
  const pushed = (job.publish ?? []).find((record) => record.step === "push" && record.branch);
  return {
    runID: run.id,
    repository: job.job.repository,
    title: taskTitle(run),
    context: [
      { label: "Previous task", body: clip(taskTitle(run)) },
      ...(result ? [{ label: "Previous result", body: clip(result) }] : []),
    ],
    ref: pushed?.branch,
    keptLocal: taskDelivery(run) === "local" && !pushed,
  };
}

export async function loadContinuation(runID: string): Promise<Continuation> {
  const view = await api.run(runID);
  const first = view.jobs[0];
  if (!first) throw new Error("The earlier task has no job to continue from.");
  return buildContinuation(view.run, await api.job(first.id));
}
