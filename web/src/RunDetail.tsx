// RunDetail.tsx — the inside of a run: one panel per job, one swim lane per
// attempt, and the evidence behind a verdict (envelope, gate checks, invalid
// emissions) one click away (R15).
//
// Each attempt lane owns its own seq cursor, so attempt 2 of a retried job is
// its own lane and attempt 1 stays fully inspectable beside it — the cursors
// cannot interleave because they are not shared.
import { useCallback, useMemo, useState } from "react";
import { api, parseAttemptSummary } from "./api";
import { formatClock, formatDateTime, formatDuration, formatUSD, isTerminalAttempt, projectName, runtimeLabel, shortID, taskHeading, taskState, taskTitle, workflowName } from "./format";
import { buildLane } from "./lanes";
import { attemptOutcome, publishAction } from "./outcomes";
import { useAttemptEvents, useNow, usePolled } from "./polling";
import { StationLine, TimingStrip } from "./StationLine";
import { buildFeed, buildStations, verdictFor } from "./stations";
import { SwimLane } from "./SwimLane";
import { taskSteps } from "./starters";
import type { TaskMode } from "./starters";
import type { Attempt, Delivery, PhaseResult, PublishRecord, Station } from "./types";
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
          <h1 className="task-heading">{taskHeading(data.run)}</h1>
          <p className="subtle">
            {data.run.targets.map((target) => projectName(target.repository)).join(", ")} · {formatDateTime(data.run.created_at)}
          </p>
        </div>
        <StatusBadge state={taskState(data.run)} />
      </header>
      <ErrorBanner error={error} />
      <section className="panel directive"><h2>{data.run.parameters?.task_mode === "auto" ? "Agents choose the directive" : data.run.parameters?.task_mode === "build" ? "Directive · make changes" : data.run.parameters?.task_mode === "ask" ? "Question · get an answer" : `Workflow · ${workflowName(data.run) ?? "saved definition"}`}</h2><details open={taskTitle(data.run).length <= 250}><summary>View your instructions</summary><p>{taskTitle(data.run)}</p></details>
        <p className="subtle">{data.run.parameters?.task_mode === "auto" || data.run.parameters?.task_mode === "build" ? "Agents plan, build, and test automatically. Failed tests return to planning for up to three repair rounds. Changes stay in the saved working folder." : data.run.parameters?.task_mode === "ask" ? "Codex reads this project and returns an answer. This task does not implement the recommendations." : "This run follows a saved workflow. Its phases, checks, and delivery are the workflow's own; each job below shows them as they run."}</p>
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

// PublishPending says where an operator's publish request stands: it waits
// for the one worker that holds the attempt's retained worktree, so whether
// that worker is running is the whole answer to "why has nothing happened".
function PublishPending({ workerID }: { workerID?: string }) {
  const { data } = usePolled(api.fleet, 5000, "fleet:publish-pending");
  const owner = (data?.workers ?? []).find((worker) => worker.id === workerID);
  const where = workerID ? `worker ${shortID(workerID)}` : "the worker that ran it";
  const standing = !data ? "" : owner?.live
    ? " It is running and takes the request on its next poll, or when it has a free slot."
    : " That worker is not running. Start it and it takes the request; nothing is lost while it is stopped.";
  return <p className="subtle" role="status">Publish requested · waiting for {where}.{standing}</p>;
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
  const [confirmingPublish, setConfirmingPublish] = useState(false);

  const act = async (action: "retry" | "cancel" | "publish" | "withdraw") => {
    setPending(true);
    setConfirmingPublish(false);
    try {
      if (action === "retry") await api.retryJob(jobID);
      else if (action === "cancel") await api.cancelJob(jobID);
      else if (action === "publish") await api.requestPublish(jobID);
      else await api.withdrawPublishRequest(jobID);
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
  const publish = publishAction(data.job.state, latest, data.delivery);
  const publishRequested = Boolean(data.job.publish_requested_at);

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
          {publish && !publishRequested && (confirmingPublish ? (
            <span className="confirm">
              <button type="button" disabled={pending} onClick={() => void act("publish")}>Confirm {publish.label.toLowerCase()}</button>
              <button type="button" onClick={() => setConfirmingPublish(false)}>Keep as is</button>
            </span>
          ) : (
            <button type="button" disabled={pending} onClick={() => setConfirmingPublish(true)}>{publish.label}…</button>
          ))}
          {publish && publishRequested &&
            <button type="button" disabled={pending} onClick={() => void act("withdraw")}>Withdraw request</button>}
        </div>
      </header>
      {publish && confirmingPublish && !publishRequested &&
        <p className="subtle" role="status">{publish.label} on {data.job.repository} {publish.consequence}</p>}
      {publish && publishRequested && <PublishPending workerID={latest?.worker_id} />}
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

      {!compact && (data.publish ?? []).length > 0 && (
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
          <AttemptLane key={attempt.id} attempt={attempt} nowMs={nowMs} compact={compact} taskMode={taskMode} stations={data.stations} delivery={data.delivery} publish={data.publish ?? []} jobID={data.job.id} />
        ))
      )}
      {compact && latest && isTerminalAttempt(latest.state) && <section className="next-action">
        <h3>What next?</h3>
        <p>{taskMode === "ask" ? "Turn a recommendation into work. Write one clear change you want made." : "Review the result above. You can also start a follow-up job on this project."}</p>
        <p><Link href={`/?project=${encodeURIComponent(data.job.repository)}&mode=build&continue=${encodeURIComponent(data.run_id)}`}>Continue from this result →</Link> <span className="subtle">Passes this task's directive and result to the next one.</span></p>
        <Link href={`/?project=${encodeURIComponent(data.job.repository)}&mode=build`}>Make changes to this project →</Link>
        <span> · </span><Link href={`/?project=${encodeURIComponent(data.job.repository)}&mode=ask`}>Ask another question →</Link>
        <p><Link href={`/?project=${encodeURIComponent(data.job.repository)}&mode=auto`}>Let agents choose the next improvement →</Link></p>
      </section>}
    </section>
  );
}

export function AttemptLane({ attempt, nowMs, compact = false, taskMode, stations: chain, delivery, publish = [], jobID }: { attempt: Attempt; nowMs: number; compact?: boolean; taskMode?: TaskMode; stations?: Station[]; delivery?: Delivery; publish?: PublishRecord[]; jobID?: string }) {
  const { events, error } = useAttemptEvents(attempt.id, attempt.state);
  const lane = useMemo(() => buildLane(attempt, events), [attempt, events]);
  const [openPhase, setOpenPhase] = useState<string | null>(null);
  const outcome = attemptOutcome(attempt);
  const terminal = isTerminalAttempt(attempt.state);
  const summary = useMemo(() => parseAttemptSummary(attempt.result), [attempt.result]);
  // The task form's starters carry bookkeeping phases (keep-local, deliver)
  // that are not stations a person follows; a saved workflow shows its chain.
  const visibleChain = useMemo(() => taskMode
    ? taskSteps[taskMode].map((name) => chain?.find((station) => station.name === name) ?? { name, kind: "agent" })
    : chain, [chain, taskMode]);
  const stations = useMemo(() => buildStations(visibleChain, delivery, attempt, lane, events, publish, nowMs),
    [visibleChain, delivery, attempt, lane, events, publish, nowMs]);
  const verdict = verdictFor(attempt, stations, delivery);
  const feed = useMemo(() => buildFeed(events), [events]);
  const timeline = <SwimLane lane={lane} nowMs={nowMs} onSelectPhase={setOpenPhase} />;
  const lastEvent = events.at(-1);
  const planHandoff = events.slice().reverse().find((event) => event.type === "handoff" && event.phase === "plan");
  const planSummary = planHandoff?.payload && typeof planHandoff.payload === "object" && "summary" in planHandoff.payload && typeof planHandoff.payload.summary === "string" ? planHandoff.payload.summary : null;
  const focus = stations.find((station) => station.name === openPhase)
    ?? stations.find((station) => station.state === "live")
    ?? stations.find((station) => station.state === "bad" || station.state === "held")
    ?? stations.slice().reverse().find((station) => station.state === "pass");
  // A delivery station (publish, CI) is no phase: no gate row or envelope
  // carries its name, so it must not filter the evidence down to nothing.
  const evidencePhase = focus?.kind === "delivery" ? null : openPhase;
  const focusResult = summary?.phases?.slice().reverse().find((phase) => phase.phase === focus?.name);
  const said = useMemo(() => {
    const last = events.slice().reverse().find((event) => event.type === "log" && event.name === "agent_text" && event.phase === focus?.name);
    const text = last?.payload && typeof last.payload === "object" ? (last.payload as Record<string, unknown>).text : undefined;
    return typeof text === "string" && text.trim() !== "" && !/^(```|\{)/.test(text.trim()) ? text.trim() : null;
  }, [events, focus?.name]);
  const focusFields = focusResult?.envelope && typeof focusResult.envelope === "object" ? focusResult.envelope as Record<string, unknown> : {};
  const proof = publish.find((record) => record.attempt_id === attempt.id && record.step === "proof");
  const pullRequest = publish.find((record) => record.attempt_id === attempt.id && record.pr_url)?.pr_url;
  const elapsed = (attempt.completed_at ? Date.parse(attempt.completed_at) : nowMs) - Date.parse(attempt.started_at ?? attempt.created_at);

  if (!compact) {
    return (
      <div className="attempt">
        <section className={`outcome outcome-${attempt.state}`} aria-label={`Attempt ${attempt.attempt_number} outcome`}>
          <h3>{outcome.title}</h3>
          <p>{outcome.detail}</p>
          <p><strong>Next step:</strong> {outcome.next}</p>
          <p className="subtle">Attempt {attempt.attempt_number} · {attempt.runtime_name ?? "Runtime not recorded"} · {formatDateTime(attempt.started_at ?? attempt.created_at)}</p>
        </section>
        {timeline}
        {openPhase && <button type="button" onClick={() => setOpenPhase(null)}>Clear phase filter</button>}
        <ErrorBanner error={error} />
        <AttemptEvidence attempt={attempt} openPhase={openPhase} />
      </div>
    );
  }

  return (
    <div className="attempt">
      <section className={`jobhead outcome-${attempt.state}`} aria-label={`Attempt ${attempt.attempt_number} outcome`}>
        <div>
          <h3>{outcome.title === "Task complete · kept local" ? taskMode === "ask" ? "Your answer is ready" : "Changes are ready to review" : outcome.title}</h3>
          <p>{outcome.detail}</p>
          <p className="subtle">Attempt {attempt.attempt_number} · {attempt.runtime_name ? runtimeLabel(attempt.runtime_name) : "Runtime not recorded"} · {formatDateTime(attempt.started_at ?? attempt.created_at)}</p>
        </div>
        <p className={`verdict verdict-${verdict.tone}`}>
          {verdict.word}
          <small>{formatDuration(elapsed)} elapsed{lastEvent?.started_at && !terminal ? ` · Last activity ${formatDuration(Math.max(0, nowMs - Date.parse(lastEvent.started_at)))} ago` : ""}</small>
        </p>
      </section>

      {verdict.tone === "bad" && <div className="stop-banner" role="status">
        <p><strong>{verdict.word === "Held" ? "Held for a person" : outcome.title}</strong>
          {verdict.word === "Held" ? summary?.publish_hold ?? "The workflow held publication." : attempt.error?.split("\n")[0] ?? outcome.detail}</p>
        <p className="subtle">{verdict.word === "Held" && jobID
          ? <>To release it, use <strong>Release hold</strong> above, or run <code>jig job release {jobID}</code>. The worker that ran this job publishes it as a pull request; Jig does not merge held work.</>
          : outcome.next}</p>
      </div>}

      <StationLine stations={stations} selected={focus?.name ?? null} onSelect={setOpenPhase} moving={!terminal}
        label={`Attempt ${attempt.attempt_number} stations`} />
      <TimingStrip stations={stations} />
      {(pullRequest || proof?.remote_ref) && <p className="proof">
        {pullRequest && <a href={pullRequest} rel="noreferrer noopener" target="_blank">Pull request {/\/pull\/(\d+)/.exec(pullRequest)?.[1] ? `#${/\/pull\/(\d+)/.exec(pullRequest)?.[1]}` : ""} →</a>}
        {proof?.remote_ref && <span>remote ref <code>{proof.remote_ref.slice(0, 12)}</code> verified</span>}
      </p>}

      <div className="lower">
        <section aria-label="Recent activity">
          <h4>{terminal ? "Activity history" : "Live activity"}</h4>
          {feed.length === 0 ? <p className="subtle">Waiting for the worker's first activity. This page checks for updates automatically.</p>
            : <ol className="feed">{feed.map((line) => <li key={line.seq}><time>{formatClock(line.at)}</time><b>{line.verb}</b><span title={line.detail}>{line.detail}</span></li>)}</ol>}
        </section>
        <section aria-label="Station detail">
          <h4>{focus ? `Station · ${focus.label} · ${focus.note || focus.state}` : "Station detail"}</h4>
          {!focus && <p className="subtle">No station has started.</p>}
          {/* A finished attempt's final report is the Result below; a station's
              own report is shown here only once the operator picks it. */}
          {focus && openPhase && typeof focusFields.summary === "string" && <blockquote className={`station-quote station-quote-${focus.state}`}>{focusFields.summary}</blockquote>}
          {focus && !openPhase && typeof focusFields.summary === "string" && <p className="subtle">Select a station on the line to read its report.</p>}
          {/* A live station has no report yet; what its agent last said is the
              nearest thing, and it is whole here where the feed cuts it. */}
          {focus && focus.state === "live" && said && <blockquote className="station-quote station-quote-live">{said}</blockquote>}
          {focus && typeof focusFields.summary !== "string" && !(focus.state === "live" && said) && <p className="subtle">{focus.state === "live" ? "This station is running. Its report arrives when it ends." : focus.state === "wait" ? "This station has not run." : focus.kind === "delivery" ? "Delivery step. Its record is in the proof line above and in the evidence below." : "This station recorded no written report."}</p>}
          {focus && <ul className="station-checks">
            {(focusResult?.gates?.checks ?? []).map((check, index) => <li key={`${check.item}-${index}`} className={check.ok ? "ok" : "not-ok"}>{check.item}{check.note ? `: ${check.note}` : ""}</li>)}
            {focus.durationMs !== null && <li className="plain">{formatDuration(focus.durationMs)}{focus.rounds > 1 ? ` over ${focus.rounds} rounds` : ""}{focus.costUSD !== null ? ` · ${formatUSD(focus.costUSD)}` : ""}</li>}
          </ul>}
          {openPhase && <button type="button" onClick={() => setOpenPhase(null)}>Clear station filter</button>}
        </section>
      </div>

      {taskMode === "auto" && planSummary && <section className="directive"><h3>Agent's directive</h3><p>{planSummary}</p><p className="subtle">The builder receives the full plan automatically. No input is needed here.</p></section>}
      <details className="evidence"><summary>Technical activity</summary>{timeline}</details>
      <ErrorBanner error={error} />
      <AttemptEvidence attempt={attempt} openPhase={evidencePhase} compact />
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
