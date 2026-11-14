// TaskBoard.tsx — Jobs as five read-only columns (2026-10-02-001 U2). The
// mapping lives in board.ts; this component only polls the queue and lays
// the columns out. The queue poll is owned here, not by Runs, so it runs
// only while the board is on screen (R6).
import { useCallback, useRef } from "react";
import { api } from "./api";
import { buildBoard } from "./board";
import { usePolled } from "./polling";
import { jobStatus } from "./status";
import type { JobStatus } from "./status";
import { TaskCard } from "./Tasks";
import type { Run } from "./types";
import { ErrorBanner } from "./ui";

// runStatus is a settled one-job run's shared job status (status.ts), read
// from the same job detail the job list reads, so the card and its column say
// what the list says. A run over several jobs, or one whose detail fails to
// load, has none and falls back to its run state.
async function runStatus(run: Run): Promise<JobStatus | null> {
  const view = await api.run(run.id);
  if (view.jobs?.length !== 1) return null;
  const detail = await api.job(view.jobs[0].id);
  return jobStatus({ jobState: detail.job.state, attempt: detail.attempts?.at(-1), delivery: detail.delivery, ask: run.parameters?.task_mode === "ask" });
}

export function TaskBoard({ runs }: { runs: Run[] }) {
  const load = useCallback(() => api.queue(), []);
  const queue = usePolled(load, 2000, "board-queue");
  // A failed queue poll is not a blank board: with no queue data the fold
  // shows active runs as Running and the banner says why (KTD6).
  //
  // A settled run's status does not change, so it is read once per run and
  // state, not polled. Active runs are placed from the queue.
  const settled = runs.filter((run) => run.state !== "active");
  const settledRef = useRef(settled);
  settledRef.current = settled;
  const settledKey = settled.map((run) => `${run.id}:${run.state}`).join(",");
  const loadStatuses = useCallback(async () => {
    const list = settledRef.current;
    const results = await Promise.allSettled(list.map(runStatus));
    const statuses = new Map<string, JobStatus>();
    results.forEach((result, index) => {
      if (result.status === "fulfilled" && result.value) statuses.set(list[index].id, result.value);
    });
    return statuses;
  }, [settledKey]); // settledKey names the runs the ref holds; a new set reloads.
  const statuses = usePolled(loadStatuses, 0, `board-statuses:${settledKey}`);
  const board = buildBoard(runs, queue.data ? queue.data.entries ?? [] : null, statuses.data ?? undefined);

  return <>
    <ErrorBanner error={queue.error} />
    <div className="board">
      {board.map((column) => (
        <section className={`board-column board-column-${column.id}`} key={column.id} aria-labelledby={`board-${column.id}`}>
          <h2 id={`board-${column.id}`}>{column.label} <span className="board-count">{column.runs.length}</span></h2>
          {column.runs.length === 0 ?
            <p className="subtle board-empty">Nothing here.</p> :
            column.runs.map((run) => <TaskCard key={run.id} run={run} status={statuses.data?.get(run.id)} />)}
        </section>
      ))}
    </div>
  </>;
}
