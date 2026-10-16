// RunDetail.tsx — the inside of a run: one panel per job, one swim lane per
// attempt, and the evidence behind a verdict (envelope, gate checks, invalid
// emissions) one click away (R15).
//
// Each attempt lane owns its own seq cursor, so attempt 2 of a retried job is
// its own lane and attempt 1 stays fully inspectable beside it — the cursors
// cannot interleave because they are not shared.
import { useCallback, useMemo, useState } from "react";
import { api, parseAttemptSummary } from "./api";
import { formatClock, formatDateTime, formatDuration, isTerminalAttempt, projectName, shortID, taskState, taskTitle } from "./format";
import { buildLane } from "./lanes";
import { attemptOutcome } from "./outcomes";
import { useAttemptEvents, useNow, usePolled } from "./polling";
import { SwimLane } from "./SwimLane";
import { taskSteps } from "./starters";
import type { TaskMode } from "./starters";
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
      <p><Link href="/">← New task</Link> · <Link href="/runs">My tasks</Link></p>
      <header className="view-header">
        <div>
          <h1>{data.run.targets.map((target) => projectName(target.repository)).join(", ")}</h1>
          <p className="subtle">
            {data.run.targets.map((target) => projectName(target.repository)).join(", ")} · {formatDateTime(data.run.created_at)}
          </p>
        </div>
        <StatusBadge state={taskState(data.run)} />
      </header>
      <ErrorBanner error={error} />
      <section className="panel directive"><h2>{data.run.parameters?.task_mode === "auto" ? "Agents choose the directive" : data.run.parameters?.task_mode === "build" ? "Directive · make changes" : "Question · get an answer"}</h2><details open={taskTitle(data.run).length <= 250}><summary>View your instructions</summary><p>{taskTitle(data.run)}</p></details>
        <p className="subtle">{data.run.parameters?.task_mode === "auto" || data.run.parameters?.task_mode === "build" ? "Agents plan, build, and test automatically. Failed tests return to planning for up to three repair rounds. Changes stay in the saved working folder." : "Codex reads this project and returns an answer. This task does not implement the recommendations."}</p>
      </section>

      {parameters.length > 0 && <details className="evidence"><summary>Task settings</summary>
        <dl className="parameters">
          {parameters.map(([name, value]) => (
            <div key={name}>
              <dt>{name}</dt>
              <dd>{value}</dd>
            </div>
          ))}
        </dl>
      </details>}

      {data.jobs.length > 1 && <section className="panel" aria-label="Run overview">
        <h2>{data.jobs.length} repositories</h2>
        <p>{data.jobs.filter((job) => job.state === "active").length} running · {data.jobs.filter((job) => job.state === "queued").length} waiting · {data.jobs.filter((job) => job.state === "failed").length} failed · {data.jobs.filter((job) => job.state === "accepted_unpublished").length} unpublished</p>
        <p className="subtle">Acceptance checks verify the workflow. Read each agent's assessment to see its conclusion about the repository.</p>
        <ul className="repository-list">
          {data.jobs.map((job) => <li key={job.id}><a href={`#job-${job.id}`}>{job.repository}</a> · <StatusBadge state={job.state} /></li>)}
        </ul>
      </section>}

      {data.jobs.length === 0 ? (
        <Empty title="No jobs" detail="This run admitted no targets." />
      ) : (
        data.jobs.map((job) => <JobPanel key={job.id} jobID={job.id} nowMs={nowMs} compact taskMode={data.run.parameters?.task_mode === "auto" ? "auto" : data.run.parameters?.task_mode === "build" ? "build" : data.run.parameters?.task_mode === "ask" ? "ask" : undefined} />)
      )}
    </div>
  );
}

export function JobDetail({ jobID }: { jobID: string }) {
  const nowMs = useNow();
  return (
    <div className="view">
      <JobPanel jobID={jobID} nowMs={nowMs} standalone compact />
    </div>
  );
}

export function JobPanel({
  jobID,
  nowMs,
  standalone = false,
  compact = false,
  taskMode,
}: {
  jobID: string;
  nowMs: number;
  standalone?: boolean;
  compact?: boolean;
  taskMode?: TaskMode;
}) {
  const load = useCallback(() => api.job(jobID), [jobID]);
  const { data, error, loading, refresh } = usePolled(load, 3000, `job:${jobID}`);
  const [actionError, setActionError] = useState<Error | null>(null);
  const [pending, setPending] = useState(false);

  const act = async (action: "retry" | "cancel") => {
    setPending(true);
    try {
      if (action === "retry") await api.retryJob(jobID);
      else await api.cancelJob(jobID);
      setActionError(null);
      refresh();
    } catch (failure) {
      setActionError(failure as Error);
    } finally {
      setPending(false);
    }
  };

  if (loading && !data) return <Loading label="Loading job…" />;
  if (!data) return <ErrorBanner error={error} />;
  const attempts = data.attempts ?? [];
  const latest = attempts.at(-1);
  const ready = latest && attemptOutcome(latest).title === "Task complete · kept local";

  return (
    <section className="panel" id={`job-${jobID}`}>
      <header className="panel-header">
        <div>
          <h2>{compact ? projectName(data.job.repository) : data.job.repository}</h2>
          {compact && <p className="subtle">{data.job.repository}</p>}
          {!compact &&
          <p className="subtle">
            {data.definition ? `${data.definition} · ` : ""}
            base {data.job.base_sha.slice(0, 12)} ·{" "}
            {standalone ? (
              <Link href={`/runs/${data.run_id}`}>run {shortID(data.run_id)}</Link>
            ) : (
              <Link href={`/jobs/${data.job.id}`}>job {shortID(data.job.id)}</Link>
            )}
          </p>}
        </div>
        <div className="panel-actions">
          <StatusBadge state={ready ? taskMode === "ask" ? "complete" : "ready" : data.job.state} />
          {data.job.state === "failed" && <button type="button" disabled={pending} onClick={() => void act("retry")}>Retry same workflow</button>}
          {["queued", "active"].includes(data.job.state) && <button type="button" disabled={pending || data.job.cancellation_requested} onClick={() => void act("cancel")}>
            {data.job.cancellation_requested ? "Cancellation requested" : "Cancel"}
          </button>}
        </div>
      </header>
      <ErrorBanner error={actionError} />
      <ErrorBanner error={error} />

      {attempts.length > 1 && <details className="evidence">
        <summary>Compare {attempts.length} attempts</summary>
        <table className="table">
          <thead><tr><th>Attempt</th><th>Runtime</th><th>Outcome</th><th>Started</th></tr></thead>
          <tbody>{attempts.map((attempt) => <tr key={attempt.id}>
            <td>#{attempt.attempt_number}</td>
            <td>{attempt.runtime_name ?? "Not recorded"}</td>
            <td>{attemptOutcome(attempt).title}</td>
            <td>{formatDateTime(attempt.started_at ?? attempt.created_at)}</td>
          </tr>)}</tbody>
        </table>
        <p className="subtle">A different outcome is evidence to inspect, not proof of better quality. Each attempt's checks and results remain below.</p>
      </details>}

      {data.worktree && (
        <p className="notice">
          {compact ? "Saved working folder: " : "Worktree retained at "}<code>{data.worktree.path}</code>
          {!compact && <> — {data.worktree.reason} <Link href="/worktrees">release it</Link></>}
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
          <AttemptLane key={attempt.id} attempt={attempt} nowMs={nowMs} compact={compact} taskMode={taskMode} />
        ))
      )}
      {compact && latest && isTerminalAttempt(latest.state) && <section className="next-action">
        <h3>What next?</h3>
        <p>{taskMode === "ask" ? "Turn a recommendation into work. Write one clear change you want Codex to make." : "Review the result and saved working folder above. You can also give Codex a new directive."}</p>
        <p><Link href={`/?project=${encodeURIComponent(data.job.repository)}&mode=build&continue=${encodeURIComponent(data.run_id)}`}>Continue from this result →</Link> <span className="subtle">Passes this task's directive and result to the next one.</span></p>
        <Link href={`/?project=${encodeURIComponent(data.job.repository)}&mode=build`}>Make changes to this project →</Link>
        <span> · </span><Link href={`/?project=${encodeURIComponent(data.job.repository)}&mode=ask`}>Ask another question →</Link>
        <p><Link href={`/?project=${encodeURIComponent(data.job.repository)}&mode=auto`}>Let agents choose the next improvement →</Link></p>
      </section>}
    </section>
  );
}

export function AttemptLane({ attempt, nowMs, compact = false, taskMode }: { attempt: Attempt; nowMs: number; compact?: boolean; taskMode?: TaskMode }) {
  const { events, error } = useAttemptEvents(attempt.id, attempt.state);
  const lane = useMemo(() => buildLane(attempt, events), [attempt, events]);
  const [openPhase, setOpenPhase] = useState<string | null>(null);
  const outcome = attemptOutcome(attempt);
  const currentPhase = !isTerminalAttempt(attempt.state) ? lane.phases.slice().reverse().find((phase) => phase.endMs === null) : undefined;
  const recordedPhases = parseAttemptSummary(attempt.result)?.phases ?? [];
  const steps = taskMode ? taskSteps[taskMode] : [...new Set(lane.phases.map((phase) => phase.phase))];
  const timeline = <SwimLane lane={lane} nowMs={nowMs} onSelectPhase={setOpenPhase} />;
  const activity = events.filter((event) => !["keep-local", "deliver"].includes(event.phase ?? "") && (event.type === "tool_call" || (event.type === "log" && ["agent_text", "repair_edge", "repair_exhausted"].includes(event.name ?? "")) || ["phase_start", "phase_end", "gate_fail", "error"].includes(event.type))).slice(-6);
  const lastEvent = events.at(-1);
  const planHandoff = events.slice().reverse().find((event) => event.type === "handoff" && event.phase === "plan");
  const planSummary = planHandoff?.payload && typeof planHandoff.payload === "object" && "summary" in planHandoff.payload && typeof planHandoff.payload.summary === "string" ? planHandoff.payload.summary : null;
  const latestMessage = events.slice().reverse().find((event) => event.type === "log" && event.name === "agent_text");
  const message = latestMessage?.payload && typeof latestMessage.payload === "object" && "text" in latestMessage.payload && typeof latestMessage.payload.text === "string" ? latestMessage.payload.text : null;

  return (
    <div className="attempt">
      <section className={`outcome outcome-${attempt.state}`} aria-label={`Attempt ${attempt.attempt_number} outcome`}>
        <h3>{compact && outcome.title === "Task complete · kept local" ? taskMode === "ask" ? "Your answer is ready" : "Changes are ready to review" : outcome.title}</h3>
        <p>{currentPhase ? `Current phase: ${currentPhase.label}. ` : ""}{outcome.detail}</p>
        <p><strong>Next step:</strong> {outcome.next}</p>
        <p className="subtle">Attempt {attempt.attempt_number} · {attempt.runtime_name ?? "Runtime not recorded"} · {formatDateTime(attempt.started_at ?? attempt.created_at)}</p>
        {compact && <p className="subtle">{formatDuration((attempt.completed_at ? Date.parse(attempt.completed_at) : nowMs) - Date.parse(attempt.started_at ?? attempt.created_at))} elapsed{lastEvent?.started_at && !isTerminalAttempt(attempt.state) ? ` · Last activity ${formatDuration(Math.max(0, nowMs - Date.parse(lastEvent.started_at)))} ago` : ""}</p>}
        {compact && !isTerminalAttempt(attempt.state) && message && <p className="agent-message">{message}</p>}
      </section>
      {compact ? <>
        {taskMode === "auto" && planSummary && <section className="directive"><h3>Agent's directive</h3><p>{planSummary}</p><p className="subtle">The builder receives the full plan automatically. No input is needed here.</p></section>}
        <ol className="task-steps" aria-label="Task progress">{steps.map((name) => {
          const traced = lane.phases.slice().reverse().find((phase) => phase.phase === name);
          const recorded = recordedPhases.slice().reverse().find((phase) => phase.phase === name);
          const state = recorded?.status ?? traced?.status ?? (traced?.endMs === null ? "running" : isTerminalAttempt(attempt.state) ? "not run" : "waiting");
          const runs = lane.phases.filter((phase) => phase.phase === name).length;
          return <li key={name}><span>{name === "scout" ? "Read project" : name}{runs > 1 ? ` · round ${runs}` : ""}</span><StatusBadge state={state} /></li>;
        })}</ol>
        <details className="activity" aria-label="Recent activity" open={!isTerminalAttempt(attempt.state)}><summary>{isTerminalAttempt(attempt.state) ? "Activity history" : "Live activity"}</summary>
          {activity.length === 0 ? <p className="subtle">Waiting for the worker's first activity. This page checks for updates automatically.</p> : <ol>{activity.map((event) => <li key={event.seq}><time>{formatClock(event.started_at)}</time><span>{event.type === "tool_call" ? `Codex used ${event.name ?? "a tool"}` : event.type === "phase_start" ? `Started ${event.phase === "scout" ? "reading the project" : event.phase}` : event.type === "phase_end" ? `Finished ${event.phase === "scout" ? "reading the project" : event.phase}` : event.name === "agent_text" ? "Codex sent a progress update" : event.name === "repair_edge" ? "Check failed → agents are repairing and retesting automatically" : event.name === "repair_exhausted" ? "Repair limit reached; see the diagnostic" : event.type === "gate_fail" ? "A check failed; inspect the workflow's repair activity" : "Worker reported an error"}</span></li>)}</ol>}
        </details>
        <details className="evidence"><summary>Technical activity</summary>{timeline}</details>
      </> : timeline}
      {openPhase && <button type="button" onClick={() => setOpenPhase(null)}>Clear phase filter</button>}
      <ErrorBanner error={error} />
      <AttemptEvidence attempt={attempt} openPhase={openPhase} compact={compact} />
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
  compact = false,
}: {
  attempt: Attempt;
  openPhase: string | null;
  compact?: boolean;
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
  const lastReport = phases.map((phase) => {
    const envelope = phase.envelope;
    return phase.kind !== "code" && envelope !== null && typeof envelope === "object" && "summary" in envelope && typeof envelope.summary === "string";
  }).lastIndexOf(true);
  const gateRows = (gates.data ?? []).filter((row) => !openPhase || row.phase === openPhase);
  const invalidRows = (invalid.data ?? []).filter((row) => !openPhase || row.phase === openPhase);

  if (!attempt.result && gateRows.length === 0 && invalidRows.length === 0 && !attempt.error && !gates.error && !invalid.error) {
    return null;
  }

  return (
    <>
      {compact && summary && <section className="result-facts" aria-label="Result facts">
        <h3>{summary.changed_paths?.length ? "Changed files" : "Files changed"}</h3>
        {summary.changed_paths?.length ? <ul>{summary.changed_paths.map((path) => <li key={path}><code>{path}</code></li>)}</ul> : <p>No file changes recorded.</p>}
        <h3>Verification</h3>
        <p>{phases.some((phase) => phase.phase === "test" && phase.status === "success") ? "Test command passed." : phases.some((phase) => phase.phase === "test" && phase.status === "fail") ? "Test command failed. Review the diagnostic below." : "No test command result recorded."} {summary.acceptance?.passed === true ? "Workflow checks passed." : summary.acceptance?.passed === false ? "Workflow checks did not pass." : ""}</p>
      </section>}
      {phases.map((phase, index) => {
        const envelope = phase.envelope;
        const fields = envelope && typeof envelope === "object" ? envelope : {};
        const report = "summary" in fields ? fields.summary : null;
        if (typeof report !== "string") return null;
        const card = (
          <section className="report" key={`${phase.phase}-${phase.phase_attempt}`}>
            <h4>{compact && index === lastReport ? "Result" : `${phase.phase} · result`}</h4>
            {"approved" in fields && typeof fields.approved === "boolean" && <p><strong>Agent assessment: {fields.approved ? "Approved" : "Not approved"}</strong></p>}
            <p className="report-text">{report}</p>
            {"blocking" in fields && Array.isArray(fields.blocking) && fields.blocking.length > 0 && <ul className="checks">
              {fields.blocking.filter((item): item is string => typeof item === "string").map((item, index) => <li key={index}>{item}</li>)}
            </ul>}
          </section>
        );
        return compact && index !== lastReport ? <details className="evidence" key={`${phase.phase}-${phase.phase_attempt}`}><summary>{phase.kind === "code" ? "Check" : "Earlier step"}: {phase.phase}</summary>{card}</details> : card;
      })}
      {attempt.error && <p className="attempt-error" role="alert">{attempt.error}</p>}
      <ErrorBanner error={gates.error} />
      <ErrorBanner error={invalid.error} />
      {attempt.result && !summary && <details className="evidence"><summary>Unrecognized result · view original record</summary><pre>{attempt.result}</pre></details>}
    <details className="evidence" open={openPhase !== null}>
      <summary>
        Evidence{openPhase ? ` · ${openPhase}` : ""} — {gateRows.length} gate check
        {gateRows.length === 1 ? "" : "s"}
        {invalidRows.length > 0 ? `, ${invalidRows.length} invalid emission` : ""}
      </summary>
      {(summary?.acceptance?.checks ?? []).length > 0 && <>
        <h4>Acceptance checks</h4>
        <ul className="checks">{summary?.acceptance?.checks?.map((check, index) => <li key={index} className={check.ok ? "ok" : "not-ok"}>
          {check.ok ? "Pass" : "Fail"} · {check.item}: {check.note}
        </li>)}</ul>
      </>}

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
    </>
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
