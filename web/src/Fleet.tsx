// Fleet.tsx — the worker fleet: what compute is registered, what it can run,
// and whether the claim transaction still counts it live.
//
// Two things this view refuses to compute for itself. Liveness is the
// server's verdict (the same predicate the claim transaction applies), not a
// timestamp comparison done here. And heartbeat age is measured against the
// payload's own `observed_at` rather than the browser clock, so a laptop
// whose clock has drifted reads the same age as the control plane does.
//
// Env names are counted, not listed: a worker advertises every name in the
// operator's environment (values never leave it, R17), which runs to dozens.
// The count is what an operator reads — a worker started from a stripped
// shell shows a suspiciously small one — and the full list is the cell's
// title for when the answer is "which one is missing".
import { useCallback } from "react";
import { api } from "./api";
import { formatClock, formatDuration, parseTime } from "./format";
import { usePolled } from "./polling";
import type { FleetMember } from "./types";
import { Empty, ErrorBanner, Loading, StatusBadge } from "./ui";

/** heartbeatAge renders the gap between a worker's last beat and the server's own clock. */
export function heartbeatAge(member: FleetMember, observedAt: string): string {
  const observed = parseTime(observedAt);
  const beat = parseTime(member.last_heartbeat);
  if (observed === null || beat === null) return "—";
  return `${formatDuration(Math.max(observed - beat, 0))} ago`;
}

function runtimeLabel(member: FleetMember): string {
  const runtimes = member.runtimes ?? [];
  if (runtimes.length === 0) return "—";
  return runtimes.map((runtime) => `${runtime.name} ${runtime.version}`).join(", ");
}

export function Fleet() {
  const load = useCallback(() => api.fleet(), []);
  const { data, error, loading } = usePolled(load, 3000, "fleet");

  if (loading && !data) return <Loading label="Loading fleet…" />;
  if (!data) return <ErrorBanner error={error} />;
  const workers = data.workers ?? [];
  // Attempts leased to a worker that stopped beating are the sweeper's next
  // move, and nothing else in the UI says so.
  const strandedAttempts = workers
    .filter((member) => !member.live)
    .reduce((total, member) => total + member.active_count, 0);

  return (
    <div className="view">
      <header className="view-header">
        <div>
          <h1>Worker fleet</h1>
          <p className="subtle">Observed {formatClock(data.observed_at)}</p>
        </div>
        <div className="counters">
          <div className="counter">
            <span className="counter-value">{data.live_count}</span>
            <span className="counter-label">live</span>
          </div>
          <div className="counter">
            <span className="counter-value">{data.stale_count}</span>
            <span className="counter-label">stale</span>
          </div>
          <div className="counter">
            <span className="counter-value">{data.available_slots}</span>
            <span className="counter-label">free slots</span>
          </div>
        </div>
      </header>
      <ErrorBanner error={error} />

      {workers.length === 0 ? (
        <Empty
          title="No workers registered"
          detail="Nothing can claim queued work until a worker registers. Start one with `jig worker`."
        />
      ) : (
        <>
          <table className="table">
            <thead>
              <tr>
                <th>Worker</th>
                <th>State</th>
                <th>Runtime</th>
                <th>Slots</th>
                <th>Env</th>
                <th>Last heartbeat</th>
              </tr>
            </thead>
            <tbody>
              {workers.map((member) => (
                <tr key={member.id}>
                  <td>
                    {member.name}
                    <p className="subtle">
                      <code>{member.id}</code> · {member.worker_version}
                    </p>
                  </td>
                  <td>
                    <StatusBadge state={member.live ? "live" : "stale"} />
                  </td>
                  <td>{runtimeLabel(member)}</td>
                  <td>
                    {member.active_count} / {member.capacity}
                  </td>
                  <td title={(member.env_names ?? []).join(" ")}>
                    {(member.env_names ?? []).length} names
                  </td>
                  <td>{heartbeatAge(member, data.observed_at)}</td>
                </tr>
              ))}
            </tbody>
          </table>
          {strandedAttempts > 0 && (
            <p className="subtle">
              {strandedAttempts} attempt{strandedAttempts === 1 ? " is" : "s are"} leased to a
              worker that stopped heartbeating. The sweeper marks them lost after three missed
              beats of server uptime, and their jobs are then retryable.
            </p>
          )}
        </>
      )}
    </div>
  );
}
