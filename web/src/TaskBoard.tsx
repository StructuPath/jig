// TaskBoard.tsx — Jobs as five read-only columns (2026-10-02-001 U2). The
// mapping lives in board.ts; this component only polls the queue and lays
// the columns out. The queue poll is owned here, not by Runs, so it runs
// only while the board is on screen (R6).
import { useCallback, useRef } from "react";
import { api } from "./api";
import { buildBoard } from "./board";
import { usePolled } from "./polling";
import { jobOpen, jobStatus } from "./status";
import type { JobStatus } from "./status";
import { TaskCard } from "./Tasks";
import type { Run } from "./types";
import { ErrorBanner } from "./ui";

interface RunStatus {
  status: JobStatus;
  /** The job can still change (jobOpen), so its status is read again. */
  open: boolean;
}

// runStatus is a one-job run's shared job status (status.ts), read from the
// same job detail the job list and the run header read, so the card and its
// column say what they say — queued, preparing, running or settled. A run
// over several jobs, or one whose detail fails to load, has none and falls
// back to its run state.
async function runStatus(run: Run): Promise<RunStatus | null> {
  const view = await api.run(run.id);
  if (view.jobs?.length !== 1) return null;
  const detail = await api.job(view.jobs[0].id);
  const status = jobStatus({ jobState: detail.job.state, attempt: detail.attempts?.at(-1), delivery: detail.delivery, ask: run.parameters?.task_mode === "ask" });
  return { status, open: run.state === "active" || jobOpen(detail) };
}

interface Statuses {
  byRun: Map<string, JobStatus>;
  open: boolean;
}

export function TaskBoard({ runs }: { runs: Run[] }) {
  const load = useCallback(() => api.queue(), []);
  const queue = usePolled(load, 2000, "board-queue");
  // A failed queue poll is not a blank board: with no queue data the fold
  // shows active runs as Running and the banner says why (KTD6).
  //
  // Every one-job run reads its job status, active runs too, so a card never
  // says "In progress" beside a header that says "Queued". A status that can
  // no longer change is kept per run and state and not read again; the poll
  // runs while any run is active or any job is open, and stops when none is.
  const runsRef = useRef(runs);
  runsRef.current = runs;
  const closed = useRef(new Map<string, JobStatus>());
  const runsKey = runs.map((run) => `${run.id}:${run.state}`).join(",");
  const loadStatuses = useCallback(async (): Promise<Statuses> => {
    const list = runsRef.current;
    const results = await Promise.allSettled(list.map(async (run) => {
      const kept = closed.current.get(`${run.id}:${run.state}`);
      return kept ? { status: kept, open: false } : runStatus(run);
    }));
    const byRun = new Map<string, JobStatus>();
    let open = false;
    results.forEach((result, index) => {
      const run = list[index];
      if (result.status !== "fulfilled" || !result.value) return;
      byRun.set(run.id, result.value.status);
      if (result.value.open) open = true;
      else closed.current.set(`${run.id}:${run.state}`, result.value.status);
    });
    return { byRun, open };
  }, []);
  const active = runs.some((run) => run.state === "active");
  const statuses = usePolled(loadStatuses, (data) => !data || data.open || active ? 2000 : 0, `board-statuses:${runsKey}`);
  const byRun = statuses.data?.byRun;
  const board = buildBoard(runs, queue.data ? queue.data.entries ?? [] : null, byRun);

  return <>
    <ErrorBanner error={queue.error} />
    <div className="board">
      {board.map((column) => (
        <section className={`board-column board-column-${column.id}`} key={column.id} aria-labelledby={`board-${column.id}`}>
          <h2 id={`board-${column.id}`}>{column.label} <span className="board-count">{column.runs.length}</span></h2>
          {column.runs.length === 0 ?
            <p className="subtle board-empty">Nothing here.</p> :
            column.runs.map((run) => <TaskCard key={run.id} run={run} status={byRun?.get(run.id)} />)}
        </section>
      ))}
    </div>
  </>;
}
