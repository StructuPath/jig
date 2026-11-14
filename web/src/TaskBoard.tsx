// TaskBoard.tsx — Jobs as five read-only columns (2026-10-02-001 U2). The
// mapping lives in board.ts; this component only lays the columns out.
//
// Every card is built from the run list's job summaries (summary.ts): a
// one-job run's card and column read the same job status its list row and
// run header read, and the job states split Waiting from Running for a run
// over several jobs. So the board makes no request of its own — it moves on
// the run list's poll — where it used to read the queue and two details per
// card.
import { buildBoard } from "./board";
import { useNow } from "./polling";
import type { JobStatus } from "./status";
import { jobStates, runJobStatus } from "./summary";
import { TaskCard } from "./Tasks";
import type { RunListItem } from "./types";

export function TaskBoard({ runs }: { runs: RunListItem[] }) {
  const nowMs = useNow(30000);
  const byRun = new Map<string, JobStatus>();
  const states: { run_id: string; state: string }[] = [];
  for (const run of runs) {
    const status = runJobStatus(run);
    if (status) byRun.set(run.id, status);
    for (const state of jobStates(run)) states.push({ run_id: run.id, state });
  }
  const board = buildBoard(runs, states, byRun);

  return <div className="board">
    {board.map((column) => (
      <section className={`board-column board-column-${column.id}`} key={column.id} aria-labelledby={`board-${column.id}`}>
        <h2 id={`board-${column.id}`}>{column.label} <span className="board-count">{column.runs.length}</span></h2>
        {column.runs.length === 0 ?
          <p className="subtle board-empty">Nothing here.</p> :
          column.runs.map((run) => <TaskCard key={run.id} run={run} status={byRun.get(run.id)} nowMs={nowMs} />)}
      </section>
    ))}
  </div>;
}
