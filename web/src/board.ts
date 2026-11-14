// board.ts — the fold from runs plus queue entries to the five Kanban
// columns of My tasks. Pure, so the column a run lands in is testable without
// a DOM (2026-10-02-001 KTD3).
//
// Two rules carry the plan's intent (KTD2):
//
//   - Run state alone cannot split waiting from running: `active` covers
//     both. The queue's job states are the missing signal, so an active run
//     is Waiting only while every one of its jobs is still queued. An active
//     run the queue does not list (the two polls disagree for a tick, or the
//     queue poll failed) is shown Running; the next poll corrects it.
//   - Every run lands in exactly one column (R4). A state the fold does not
//     recognise goes to Needs review — an unknown state should reach a
//     human, not vanish.
//
// A one-job run whose job status is known (status.ts), active or settled,
// lands in that status's column, so the board and the job list never
// disagree about the same job. The queue and the run state are the
// fallback: run state cannot tell a held job from one with nothing to
// publish, since both make the run `mixed`.
import { taskState } from "./format";
import type { ColumnID, JobStatus } from "./status";
import type { QueueEntry, Run } from "./types";

export type { ColumnID };

/** columns is the board's fixed display order. */
export const columns: ReadonlyArray<{ id: ColumnID; label: string }> = [
  { id: "waiting", label: "Waiting" },
  { id: "running", label: "Running" },
  { id: "review", label: "Needs review" },
  { id: "done", label: "Done" },
  { id: "stopped", label: "Stopped" },
];

export interface Column<T extends Run = Run> {
  id: ColumnID;
  label: string;
  runs: T[];
}

/**
 * runColumn places one run. `jobStates` are the queue's job states for that
 * run, or undefined when the queue does not list it. taskState is the single
 * source for the held-publication `mixed` mapping, so terminal placement
 * reads it rather than `run.state`.
 */
export function runColumn(run: Run, jobStates: string[] | undefined, status?: JobStatus): ColumnID {
  if (status) return status.column;
  if (run.state === "active") {
    if (jobStates?.length && jobStates.every((state) => state === "queued")) return "waiting";
    return "running";
  }
  switch (taskState(run)) {
    case "accepted":
    case "complete":
      return "done";
    case "failed":
    case "cancelled":
      return "stopped";
    default:
      return "review";
  }
}

/**
 * buildBoard folds runs into the five columns, preserving the incoming run
 * order (the API lists newest first) within each column. The job states may
 * come from the queue or from the run list's job summaries; null means none
 * are known, and active runs then fall to Running (KTD6).
 */
export function buildBoard<T extends Run>(runs: T[], queue: Pick<QueueEntry, "run_id" | "state">[] | null, statuses?: ReadonlyMap<string, JobStatus>): Column<T>[] {
  const jobStates = new Map<string, string[]>();
  for (const entry of queue ?? []) {
    const states = jobStates.get(entry.run_id);
    if (states) states.push(entry.state);
    else jobStates.set(entry.run_id, [entry.state]);
  }
  const board = columns.map((column) => ({ ...column, runs: [] as T[] }));
  const byID = new Map(board.map((column) => [column.id, column]));
  for (const run of runs) byID.get(runColumn(run, jobStates.get(run.id), statuses?.get(run.id)))!.runs.push(run);
  return board;
}
