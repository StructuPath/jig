// Worktrees.tsx — the retained-worktree ledger and the release action (R16).
// Release deletes work the worker could not prove published, so it is gated
// on an explicit inspection confirmation here exactly as it is server-side:
// the operator has to say they looked.
import { useCallback, useState } from "react";
import { api } from "./api";
import { formatClock, shortID } from "./format";
import { usePolled } from "./polling";
import { Empty, ErrorBanner, Link, Loading } from "./ui";

export function Worktrees() {
  const load = useCallback(() => api.worktrees(), []);
  const { data, error, loading, refresh } = usePolled(load, 5000, "worktrees");
  const [confirming, setConfirming] = useState<string | null>(null);
  const [actionError, setActionError] = useState<Error | null>(null);

  const release = async (attemptID: string) => {
    try {
      await api.releaseWorktree(attemptID);
      setActionError(null);
      setConfirming(null);
      refresh();
    } catch (failure) {
      setActionError(failure as Error);
    }
  };

  if (loading && !data) return <Loading label="Loading worktrees…" />;
  if (!data) return <ErrorBanner error={error} />;

  return (
    <div className="view">
      <header className="view-header">
        <div>
          <h1>Retained worktrees</h1>
          <p className="subtle">
            Kept because publish could not be proven. Inspect the path, then release.
          </p>
        </div>
        <div className="counters">
          <div className="counter">
            <span className="counter-value">{data.length}</span>
            <span className="counter-label">retained</span>
          </div>
        </div>
      </header>
      <ErrorBanner error={error} />
      <ErrorBanner error={actionError} />

      {data.length === 0 ? (
        <Empty title="Nothing retained" detail="Every attempt's worktree was disposed cleanly." />
      ) : (
        <table className="table">
          <thead>
            <tr>
              <th>Repository</th>
              <th>Path</th>
              <th>Reason</th>
              <th>Attempt</th>
              <th>Since</th>
              <th />
            </tr>
          </thead>
          <tbody>
            {data.map((entry) => (
              <tr key={entry.attempt_id}>
                <td>{entry.repository}</td>
                <td>
                  <code>{entry.path}</code>
                </td>
                <td>{entry.reason}</td>
                <td>
                  <Link href={`/jobs/${entry.attempt_id}`}>{shortID(entry.attempt_id)}</Link>
                </td>
                <td>{formatClock(entry.created_at)}</td>
                <td>
                  {confirming === entry.attempt_id ? (
                    <span className="confirm">
                      <button type="button" onClick={() => void release(entry.attempt_id)}>
                        Confirm release
                      </button>
                      <button type="button" onClick={() => setConfirming(null)}>
                        Keep
                      </button>
                    </span>
                  ) : (
                    <button type="button" onClick={() => setConfirming(entry.attempt_id)}>
                      Release…
                    </button>
                  )}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
    </div>
  );
}
