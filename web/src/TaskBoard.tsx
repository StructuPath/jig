// TaskBoard.tsx — My tasks as five read-only columns (2026-10-02-001 U2). The
// mapping lives in board.ts; this component only polls the queue and lays
// the columns out. The queue poll is owned here, not by Runs, so it runs
// only while the board is on screen (R6).
import { useCallback } from "react";
import { api } from "./api";
import { buildBoard } from "./board";
import { usePolled } from "./polling";
import { TaskCard } from "./Tasks";
import type { Run } from "./types";
import { ErrorBanner } from "./ui";

export function TaskBoard({ runs }: { runs: Run[] }) {
  const load = useCallback(() => api.queue(), []);
  const queue = usePolled(load, 2000, "board-queue");
  // A failed queue poll is not a blank board: with no queue data the fold
  // shows active runs as Running and the banner says why (KTD6).
  const board = buildBoard(runs, queue.data ? queue.data.entries ?? [] : null);

  return <>
    <ErrorBanner error={queue.error} />
    <div className="board">
      {board.map((column) => (
        <section className={`board-column board-column-${column.id}`} key={column.id} aria-labelledby={`board-${column.id}`}>
          <h2 id={`board-${column.id}`}>{column.label} <span className="board-count">{column.runs.length}</span></h2>
          {column.runs.length === 0 ?
            <p className="subtle board-empty">Nothing here.</p> :
            column.runs.map((run) => <TaskCard key={run.id} run={run} />)}
        </section>
      ))}
    </div>
  </>;
}
