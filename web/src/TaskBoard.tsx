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
  /** A run over several jobs has none and falls back to its run state. */
  status: JobStatus | null;
  /** The job can still change (jobOpen), so its status is read again. */
  open: boolean;
}

// runStatus is a one-job run's shared job status (status.ts), read from the
// same job detail the job list and the run header read, so the card and its
// column say what they say — queued, preparing, running or settled. A run
// over several jobs has none and falls back to its run state; so does one
// whose detail fails to load, which stays open so the read is tried again.
async function runStatus(run: Run): Promise<RunStatus> {
  const view = await api.run(run.id);
  if (view.jobs?.length !== 1) return { status: null, open: run.state === "active" };
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
  // no longer change is kept per run, state and update time and not read
  // again; a run that turns active drops what was kept, so a retried run
  // that settles again is read afresh. The poll runs while any run is
  // active, any job is open or any read failed, and stops when none is.
  const runsRef = useRef(runs);
  runsRef.current = runs;
  const closed = useRef(new Map<string, { key: string; status: JobStatus | null }>());
  const runsKey = runs.map((run) => `${run.id}:${run.state}`).join(",");
  const loadStatuses = useCallback(async (): Promise<Statuses> => {
    const list = runsRef.current;
    const keyOf = (run: Run) => `${run.state}:${run.updated_at}`;
    const results = await Promise.allSettled(list.map(async (run): Promise<RunStatus & { kept?: boolean }> => {
      if (run.state === "active") closed.current.delete(run.id);
      const kept = closed.current.get(run.id);
      return kept?.key === keyOf(run) ? { status: kept.status, open: false, kept: true } : runStatus(run);
    }));
    // A read that began before its run changed (a retry made it active) is
    // stale and is not kept.
    const current = new Map(runsRef.current.map((run) => [run.id, keyOf(run)]));
    const byRun = new Map<string, JobStatus>();
    let open = false;
    results.forEach((result, index) => {
      const run = list[index];
      if (result.status !== "fulfilled") {
        open = true;
        return;
      }
      if (result.value.status) byRun.set(run.id, result.value.status);
      if (result.value.open) open = true;
      else if (!result.value.kept && current.get(run.id) === keyOf(run)) {
        closed.current.set(run.id, { key: keyOf(run), status: result.value.status });
      }
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
