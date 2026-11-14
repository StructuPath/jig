// Queue.tsx — the work queue with its depth. Depth is the first number an
// operator wants ("is anything waiting?") and the one a run list cannot
// answer, so it is the headline, not a column.
import { useCallback, useState } from "react";
import { api } from "./api";
import { TaskBoard } from "./TaskBoard";
import { Floor } from "./Floor";
import { formatAge, formatClock, formatDateTime } from "./format";
import { useNow, usePolled } from "./polling";
import { Empty, ErrorBanner, Link, Loading, StatusBadge } from "./ui";

export function Queue() {
  const load = useCallback(() => api.queue(), []);
  const { data, error, loading } = usePolled(load, 2000, "queue");
  const loadFleet = useCallback(() => api.fleet(), []);
  const fleet = usePolled(loadFleet, 3000, "queue-workers");
  const nowMs = useNow(30000);

  if (loading && !data) return <Loading label="Loading queue…" />;
  if (!data) return <ErrorBanner error={error} />;
  const entries = data.entries ?? [];

  return (
    <div className="view">
      <header className="view-header">
        <div>
          <h1>Work queue</h1>
          <p className="subtle">Live work appears here. Finished jobs are in Jobs.</p>
          <p className="subtle">Observed {formatClock(data.observed_at)}</p>
        </div>
        <div className="counters">
          <div className="counter">
            <span className="counter-value">{data.depth}</span>
            <span className="counter-label">queued</span>
          </div>
          <div className="counter">
            <span className="counter-value">{data.active_count}</span>
            <span className="counter-label">running</span>
          </div>
        </div>
      </header>
      <ErrorBanner error={error} />
      <section className="panel readiness" aria-label="Worker readiness">
        <h2>{fleet.data ? `${fleet.data.live_count} worker${fleet.data.live_count === 1 ? "" : "s"} online` : "Checking workers…"}</h2>
        <p>{fleet.data?.live_count ?
          `Ready runtimes: ${(fleet.data.workers ?? []).filter((worker) => worker.live).flatMap((worker) => (worker.runtimes ?? []).map((runtime) => runtime.name)).join(", ")}. Jobs start when a compatible worker has a free slot.` :
          "Jobs need an online worker before they can start."}</p>
        <Link href="/fleet">View workers →</Link>
        <ErrorBanner error={fleet.error} />
      </section>

      {entries.length === 0 ? (
        <section className="panel">
          <h2>No work is running</h2>
          <p>The queue is empty. Opening an old run does not restart it.</p>
          <p><Link href="/">Start a job on the Floor →</Link></p>
          <Link href="/runs">Browse finished jobs →</Link>
        </section>
      ) : (
        <table className="table">
          <thead>
            <tr>
              <th>#</th>
              <th>Repository</th>
              <th>Definition</th>
              <th>State</th>
              <th>Attempt</th>
              <th>Enqueued</th>
            </tr>
          </thead>
          <tbody>
            {entries.map((entry) => (
              <tr key={entry.job_id}>
                <td>{entry.position ?? "—"}</td>
                <td>
                  <Link href={`/jobs/${entry.job_id}`}>{entry.repository}</Link>
                </td>
                <td>{entry.definition ?? "—"}</td>
                <td>
                  <StatusBadge state={entry.state} />
                  {entry.state === "queued" && entry.requires_merge && <small>Requires a worker with --allow-merge</small>}
                </td>
                <td>
                  {entry.attempt ? (
                    <>
                      #{entry.attempt.attempt_number} <StatusBadge state={entry.attempt.state} />
                    </>
                  ) : (
                    "—"
                  )}
                </td>
                <td className="cell-nowrap" title={formatDateTime(entry.enqueued_at)}>{formatAge(entry.enqueued_at, nowMs)}</td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
    </div>
  );
}

type Layout = "list" | "board";

// The layout is a viewing preference, so it lives in browser storage rather
// than the URL: deep links stay /runs (2026-10-02-001 KTD4).
function storedLayout(): Layout {
  try { return localStorage.getItem("jig-tasks-layout") === "board" ? "board" : "list"; } catch { return "list"; /* Browser storage can be disabled. */ }
}

export function Runs() {
  const load = useCallback(() => api.runs(), []);
  const { data, error, loading } = usePolled(load, 4000, "runs");
  const [layout, setLayout] = useState(storedLayout);
  const nowMs = useNow(1000);
  const choose = (next: Layout) => {
    setLayout(next);
    try { localStorage.setItem("jig-tasks-layout", next); } catch { /* The switch still applies for this visit. */ }
  };
  if (loading && !data) return <Loading label="Loading jobs…" />;
  if (!data) return <ErrorBanner error={error} />;

  return (
    <div className="view">
      <header className="view-header">
        <div>
          <h1>Jobs</h1>
          <p className="subtle">Every job, newest first. Open one to follow its stations or read the result.</p>
        </div>
        <div className="layout-switch" role="group" aria-label="Layout">
          <button type="button" aria-pressed={layout === "list"} onClick={() => choose("list")}>List</button>
          <button type="button" aria-pressed={layout === "board"} onClick={() => choose("board")}>Board</button>
        </div>
      </header>
      <ErrorBanner error={error} />
      {data.length === 0 ? (
        <Empty title="No jobs yet" detail="Start a job from the Floor." />
      ) : layout === "board" ? (
        <TaskBoard runs={data} />
      ) : (
        <Floor runs={data} nowMs={nowMs} />
      )}
    </div>
  );
}
