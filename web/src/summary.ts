// summary.ts — list rows, Floor rows and board cards from the run list alone.
// GET /api/runs carries a summary of every job (U8 read surface), so a row no
// longer reads GET /api/runs/:id and GET /api/jobs/:id for itself: 40 jobs
// were 77 requests, and now they are one. The job page still reads the full
// detail.
//
// A summary is folded back into the JobDetail shape the shared folds read
// (status.ts jobStatus, stations.ts buildStations, outcomes.ts), so a row and
// the job page derive their words from one set of functions. The summary
// drops the attempt's result body, so the fold rebuilds the slice of it those
// functions read — each phase's status, the publish outcome and the hold —
// from the fields the server extracted.
import { runColumn } from "./board";
import { taskHeading, taskTitle, workflowName } from "./format";
import { jobOpen, jobStatus } from "./status";
import type { ColumnID, JobStatus } from "./status";
import type { Attempt, AttemptBrief, JobDetail, JobSummary, PhaseProgress, RunListItem } from "./types";

/** summaryAttempt is the latest attempt with a result rebuilt from its summary. */
export function summaryAttempt(brief: AttemptBrief, phases: PhaseProgress[]): Attempt {
  const { result_excerpt: _result, result_truncated: _cut, error_excerpt, error_truncated: _errorCut,
    publish_state, publish_code, ...attempt } = brief;
  const result: Record<string, unknown> = {};
  if (phases.length > 0) {
    result.phases = phases.map((phase) => ({ phase: phase.phase, kind: "", status: phase.status, phase_attempt: phase.rounds }));
  }
  if (publish_state) result.publish = publish_code ? { state: publish_state, code: publish_code } : publish_state;
  if (attempt.publish_hold) result.publish_hold = attempt.publish_hold;
  return {
    ...attempt,
    ...(Object.keys(result).length > 0 ? { result: JSON.stringify(result) } : {}),
    ...(error_excerpt ? { error: error_excerpt } : {}),
  };
}

/** summaryDetail is a job summary in the JobDetail shape, latest attempt only. */
export function summaryDetail(summary: JobSummary): JobDetail {
  const attempt = summary.latest_attempt ? summaryAttempt(summary.latest_attempt, summary.phases ?? []) : undefined;
  return {
    job: summary.job,
    run_id: summary.job.run_id,
    attempts: attempt ? [attempt] : [],
    publish: summary.publish ?? [],
    stations: summary.stations,
    delivery: summary.delivery,
  };
}

/** onlyJob is the run's job summary when the run has exactly one job. */
export function onlyJob(run: RunListItem): JobSummary | undefined {
  const summaries = run.job_summaries ?? [];
  return summaries.length === 1 ? summaries[0] : undefined;
}

/**
 * runJobStatus is a one-job run's shared job status, the word its list row,
 * board card and run header all show. A run over several jobs, or with no job
 * yet, has none and falls back to its run state.
 */
export function runJobStatus(run: RunListItem): JobStatus | undefined {
  const summary = onlyJob(run);
  if (!summary) return undefined;
  const detail = summaryDetail(summary);
  return jobStatus({ jobState: summary.job.state, attempt: detail.attempts?.at(-1), delivery: summary.delivery, ask: run.parameters?.task_mode === "ask" });
}

/** runIsOpen says whether anything the run's rows show can still change. */
export function runIsOpen(run: RunListItem): boolean {
  if (run.state === "active") return true;
  return (run.job_summaries ?? []).some((summary) => jobOpen(summaryDetail(summary)));
}

/** jobStates are the run's job states, the signal that splits Waiting from Running. */
export function jobStates(run: RunListItem): string[] {
  return (run.job_summaries ?? []).map((summary) => summary.job.state);
}

/** runColumnOf is the board column — and the status group — a run belongs to. */
export function runColumnOf(run: RunListItem): ColumnID {
  const states = jobStates(run);
  return runColumn(run, states.length ? states : undefined, runJobStatus(run));
}

// The Jobs list leads with what needs a person: work waiting on review (held,
// not published, kept local to review), then work that stopped, then work in
// motion, then work not started, and settled work last.
export const attentionOrder: readonly ColumnID[] = ["review", "stopped", "running", "waiting", "done"];

/** sortByAttention orders runs by attentionOrder, newest first within a group. */
export function sortByAttention<T extends RunListItem>(runs: readonly T[]): T[] {
  const rank = new Map(attentionOrder.map((id, index) => [id, index]));
  return runs
    .map((run) => ({ run, rank: rank.get(runColumnOf(run)) ?? 0, created: Date.parse(run.created_at) || 0 }))
    .sort((left, right) => left.rank - right.rank || right.created - left.created || left.run.id.localeCompare(right.run.id))
    .map((entry) => entry.run);
}

export interface RunFilter {
  status: ColumnID | null;
  repo: string;
  q: string;
}

/** runRepositories are the repositories a run targets. */
export function runRepositories(run: RunListItem): string[] {
  return (run.targets ?? []).map((target) => target.repository);
}

/**
 * runMatches applies the list's filters. The text filter matches the job's
 * title, its workflow (definition) name, its repository, and a run or job ID
 * prefix, case-insensitively.
 */
export function runMatches(run: RunListItem, filter: RunFilter): boolean {
  if (filter.status && runColumnOf(run) !== filter.status) return false;
  if (filter.repo && !runRepositories(run).includes(filter.repo)) return false;
  const needle = filter.q.trim().toLowerCase();
  if (!needle) return true;
  const text = [taskHeading(run), taskTitle(run), workflowName(run) ?? "", ...runRepositories(run)]
    .some((value) => value.toLowerCase().includes(needle));
  const ids = [run.id, ...(run.job_summaries ?? []).map((summary) => summary.job.id)]
    .some((id) => id.toLowerCase().startsWith(needle));
  return text || ids;
}
