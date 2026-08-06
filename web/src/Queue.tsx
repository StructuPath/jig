// Queue.tsx — the work queue with its depth. Depth is the first number an
// operator wants ("is anything waiting?") and the one a run list cannot
// answer, so it is the headline, not a column.
import { useCallback } from "react";
import { api } from "./api";
import { formatClock, shortID } from "./format";
import { usePolled } from "./polling";
import type { Run } from "./types";
import { Empty, ErrorBanner, Link, Loading, StatusBadge } from "./ui";

export function Queue() {
  const load = useCallback(() => api.queue(), []);
  const { data, error, loading } = usePolled(load, 2000, "queue");

  if (loading && !data) return <Loading label="Loading queue…" />;
  if (!data) return <ErrorBanner error={error} />;
  const entries = data.entries ?? [];

  return (
    <div className="view">
      <header className="view-header">
        <div>
          <h1>Work queue</h1>
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

      {entries.length === 0 ? (
        <Empty title="Nothing queued" detail="No job is waiting or running right now." />
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
                <td>{formatClock(entry.enqueued_at)}</td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
    </div>
  );
}

export function Runs() {
  const load = useCallback(() => api.runs(), []);
  const { data, error, loading } = usePolled(load, 4000, "runs");
  if (loading && !data) return <Loading label="Loading runs…" />;
  if (!data) return <ErrorBanner error={error} />;

  return (
    <div className="view">
      <header className="view-header">
        <h1>Runs</h1>
      </header>
      <ErrorBanner error={error} />
      {data.length === 0 ? (
        <Empty title="No runs" detail="Invoke a definition to admit one." />
      ) : (
        <table className="table">
          <thead>
            <tr>
              <th>Run</th>
              <th>Targets</th>
              <th>State</th>
              <th>Created</th>
            </tr>
          </thead>
          <tbody>
            {data.map((run: Run) => (
              <tr key={run.id}>
                <td>
                  <Link href={`/runs/${run.id}`}>{shortID(run.id)}</Link>
                </td>
                <td>{run.targets.map((target) => target.repository).join(", ")}</td>
                <td>
                  <StatusBadge state={run.state} />
                </td>
                <td>{formatClock(run.created_at)}</td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
    </div>
  );
}
