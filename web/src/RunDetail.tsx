// RunDetail.tsx — the inside of a run: one panel per job, one swim lane per
// attempt, and the evidence behind a verdict (envelope, gate checks, invalid
// emissions) one click away (R15).
//
// Each attempt lane owns its own seq cursor, so attempt 2 of a retried job is
// its own lane and attempt 1 stays fully inspectable beside it — the cursors
// cannot interleave because they are not shared.
import { useCallback, useMemo, useState } from "react";
import { api, parseAttemptSummary } from "./api";
import { formatClock, formatDuration, isTerminalAttempt, shortID } from "./format";
import { buildLane } from "./lanes";
import { useAttemptEvents, useNow, usePolled } from "./polling";
import { SwimLane } from "./SwimLane";
import type { Attempt, PhaseResult } from "./types";
import { Empty, ErrorBanner, Link, Loading, StatusBadge } from "./ui";

export function RunDetail({ runID }: { runID: string }) {
  const load = useCallback(() => api.run(runID), [runID]);
  const { data, error, loading } = usePolled(load, 3000, `run:${runID}`);
  const nowMs = useNow();

  if (loading && !data) return <Loading label="Loading run…" />;
  if (!data) return <ErrorBanner error={error} />;

  const parameters = Object.entries(data.run.parameters ?? {});
  return (
    <div className="view">
      <header className="view-header">
        <div>
          <h1>Run {shortID(data.run.id)}</h1>
          <p className="subtle">
            {data.run.targets.length} target{data.run.targets.length === 1 ? "" : "s"} · pinned at
            admission · created {formatClock(data.run.created_at)}
          </p>
        </div>
        <StatusBadge state={data.run.state} />
      </header>
      <ErrorBanner error={error} />

      {parameters.length > 0 && (
        <dl className="parameters">
          {parameters.map(([name, value]) => (
            <div key={name}>
              <dt>{name}</dt>
              <dd>{value}</dd>
            </div>
          ))}
        </dl>
      )}

      {data.jobs.length === 0 ? (
        <Empty title="No jobs" detail="This run admitted no targets." />
      ) : (
        data.jobs.map((job) => <JobPanel key={job.id} jobID={job.id} nowMs={nowMs} />)
      )}
    </div>
  );
}

export function JobDetail({ jobID }: { jobID: string }) {
  const nowMs = useNow();
  return (
    <div className="view">
      <JobPanel jobID={jobID} nowMs={nowMs} standalone />
    </div>
  );
}

export function JobPanel({
  jobID,
  nowMs,
  standalone = false,
}: {
  jobID: string;
  nowMs: number;
  standalone?: boolean;
}) {
  const load = useCallback(() => api.job(jobID), [jobID]);
  const { data, error, loading, refresh } = usePolled(load, 3000, `job:${jobID}`);
  const [actionError, setActionError] = useState<Error | null>(null);

  const act = async (action: "retry" | "cancel") => {
    try {
      if (action === "retry") await api.retryJob(jobID);
      else await api.cancelJob(jobID);
      setActionError(null);
      refresh();
    } catch (failure) {
      setActionError(failure as Error);
    }
  };

  if (loading && !data) return <Loading label="Loading job…" />;
  if (!data) return <ErrorBanner error={error} />;
  const attempts = data.attempts ?? [];

  return (
    <section className="panel">
      <header className="panel-header">
        <div>
          <h2>{data.job.repository}</h2>
          <p className="subtle">
            {data.definition ? `${data.definition} · ` : ""}
            base {data.job.base_sha.slice(0, 12)} ·{" "}
            {standalone ? (
              <Link href={`/runs/${data.run_id}`}>run {shortID(data.run_id)}</Link>
            ) : (
              <Link href={`/jobs/${data.job.id}`}>job {shortID(data.job.id)}</Link>
            )}
          </p>
        </div>
        <div className="panel-actions">
          <StatusBadge state={data.job.state} />
          <button type="button" onClick={() => void act("retry")}>
            Retry
          </button>
          <button type="button" onClick={() => void act("cancel")}>
            Cancel
          </button>
        </div>
      </header>
      <ErrorBanner error={actionError} />

      {data.worktree && (
        <p className="notice">
          Worktree retained at <code>{data.worktree.path}</code> — {data.worktree.reason}{" "}
          <Link href="/worktrees">release it</Link>
        </p>
      )}

      {(data.publish ?? []).length > 0 && (
        <ul className="publish">
          {(data.publish ?? []).map((record) => (
            <li key={`${record.attempt_id}-${record.step}`}>
              <strong>{record.step}</strong> {record.branch}
              {record.pr_url ? (
                <>
                  {" "}
                  <a href={record.pr_url} rel="noreferrer noopener" target="_blank">
                    pull request
                  </a>
                </>
              ) : null}
            </li>
          ))}
        </ul>
      )}

      {attempts.length === 0 ? (
        <Empty title="No attempts yet" detail="This job is waiting for a worker to claim it." />
      ) : (
        attempts.map((attempt) => (
          <AttemptLane key={attempt.id} attempt={attempt} nowMs={nowMs} />
        ))
      )}
    </section>
  );
}

export function AttemptLane({ attempt, nowMs }: { attempt: Attempt; nowMs: number }) {
  const { events, error } = useAttemptEvents(attempt.id, attempt.state);
  const lane = useMemo(() => buildLane(attempt, events), [attempt, events]);
  const [openPhase, setOpenPhase] = useState<string | null>(null);

  return (
    <div className="attempt">
      <SwimLane lane={lane} nowMs={nowMs} onSelectPhase={setOpenPhase} />
      <ErrorBanner error={error} />
      <AttemptEvidence attempt={attempt} openPhase={openPhase} />
    </div>
  );
}

/**
 * AttemptEvidence is the inspection surface: the phase envelopes the engine
 * recorded in the attempt result, the gate checks behind each verdict, and
 * every invalid emission that was persisted (size-capped in the store, whole
 * in the attempt-local JSONL).
 */
export function AttemptEvidence({
  attempt,
  openPhase,
}: {
  attempt: Attempt;
  openPhase: string | null;
}) {
  const terminal = isTerminalAttempt(attempt.state);
  const loadGates = useCallback(() => api.attemptGates(attempt.id), [attempt.id]);
  const loadEnvelopes = useCallback(() => api.attemptEnvelopes(attempt.id), [attempt.id]);
  const gates = usePolled(loadGates, terminal ? 0 : 5000, `gates:${attempt.id}:${attempt.state}`);
  const invalid = usePolled(
    loadEnvelopes,
    terminal ? 0 : 5000,
    `envelopes:${attempt.id}:${attempt.state}`,
  );
  const summary = useMemo(() => parseAttemptSummary(attempt.result), [attempt.result]);
  const phases = summary?.phases ?? [];
  const gateRows = (gates.data ?? []).filter((row) => !openPhase || row.phase === openPhase);
  const invalidRows = (invalid.data ?? []).filter((row) => !openPhase || row.phase === openPhase);

  if (!attempt.result && gateRows.length === 0 && invalidRows.length === 0 && !attempt.error) {
    return null;
  }

  return (
    <details className="evidence">
      <summary>
        Evidence{openPhase ? ` · ${openPhase}` : ""} — {gateRows.length} gate check
        {gateRows.length === 1 ? "" : "s"}
        {invalidRows.length > 0 ? `, ${invalidRows.length} invalid emission` : ""}
      </summary>

      {attempt.error && <p className="attempt-error">{attempt.error}</p>}

      {phases
        .filter((phase) => !openPhase || phase.phase === openPhase)
        .map((phase) => (
          <PhaseEnvelope key={`${phase.phase}-${phase.phase_attempt}`} phase={phase} />
        ))}

      {gateRows.length > 0 && (
        <table className="gates">
          <thead>
            <tr>
              <th>Phase</th>
              <th>Gate</th>
              <th>Item</th>
              <th>Result</th>
              <th>Note</th>
            </tr>
          </thead>
          <tbody>
            {gateRows.map((row, index) => (
              <tr key={`${row.phase}-${row.gate}-${row.item}-${index}`}>
                <td>{row.phase}</td>
                <td>{row.gate}</td>
                <td>{row.item}</td>
                <td className={row.ok ? "ok" : "not-ok"}>{row.ok ? "pass" : "fail"}</td>
                <td>{row.note}</td>
              </tr>
            ))}
          </tbody>
        </table>
      )}

      {invalidRows.map((row) => (
        <div key={`${row.phase}-${row.emission}`} className="invalid-envelope">
          <h4>
            Invalid emission · {row.phase} · seq {row.emission}
          </h4>
          <p className="subtle">{row.parse_error}</p>
          <pre>{row.body}</pre>
          <p className="subtle">
            Stored capped; the complete emission is in this attempt's JSONL trace.
          </p>
        </div>
      ))}
    </details>
  );
}

function PhaseEnvelope({ phase }: { phase: PhaseResult }) {
  const started = phase.started_at ? Date.parse(phase.started_at) : null;
  const ended = phase.ended_at ? Date.parse(phase.ended_at) : null;
  return (
    <div className="envelope">
      <h4>
        {phase.phase} <span className={`badge badge-${phase.status}`}>{phase.status}</span>
        {started && ended ? (
          <span className="subtle"> · {formatDuration(ended - started)}</span>
        ) : null}
      </h4>
      {phase.envelope ? (
        <pre>{JSON.stringify(phase.envelope, null, 2)}</pre>
      ) : (
        <p className="subtle">No envelope recorded for this phase.</p>
      )}
      {(phase.gates?.checks ?? []).length > 0 && (
        <ul className="checks">
          {(phase.gates?.checks ?? []).map((check, index) => (
            <li key={`${check.item}-${index}`} className={check.ok ? "ok" : "not-ok"}>
              {check.item}: {check.note ?? (check.ok ? "ok" : "failed")}
            </li>
          ))}
        </ul>
      )}
    </div>
  );
}
