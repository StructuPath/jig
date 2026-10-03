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
import { taskState } from "./format";
import type { QueueEntry, Run } from "./types";

export type ColumnID = "waiting" | "running" | "review" | "done" | "stopped";

/** columns is the board's fixed display order. */
export const columns: ReadonlyArray<{ id: ColumnID; label: string }> = [
  { id: "waiting", label: "Waiting" },
  { id: "running", label: "Running" },
  { id: "review", label: "Needs review" },
  { id: "done", label: "Done" },
  { id: "stopped", label: "Stopped" },
];

export interface Column {
  id: ColumnID;
  label: string;
  runs: Run[];
}

/**
 * runColumn places one run. `jobStates` are the queue's job states for that
 * run, or undefined when the queue does not list it. taskState is the single
 * source for the held-publication `mixed` mapping, so terminal placement
 * reads it rather than `run.state`.
 */
export function runColumn(run: Run, jobStates: string[] | undefined): ColumnID {
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
 * order (the API lists newest first) within each column. A null queue means
 * the queue poll failed; active runs then fall to Running (KTD6).
 */
export function buildBoard(runs: Run[], queue: QueueEntry[] | null): Column[] {
  const jobStates = new Map<string, string[]>();
  for (const entry of queue ?? []) {
    const states = jobStates.get(entry.run_id);
    if (states) states.push(entry.state);
    else jobStates.set(entry.run_id, [entry.state]);
  }
  const board = columns.map((column) => ({ ...column, runs: [] as Run[] }));
  const byID = new Map(board.map((column) => [column.id, column]));
  for (const run of runs) byID.get(runColumn(run, jobStates.get(run.id)))!.runs.push(run);
  return board;
}
