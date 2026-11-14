// Floor.tsx — the jobs on the line, one row each. A row carries the job's
// stations as lamps, so a job that is held, stopped, or still three stations
// from done reads at a glance, without opening it.
//
// A row is built from the run list's job summary alone (summary.ts): the
// latest attempt, where each of its phases stands, and its publish records.
// It makes no request of its own — a list of 40 jobs is the one GET
// /api/runs its view already polls — and a live row moves on that poll, not
// on an event stream per row. Spend is not on the row: it lives in the event
// stream; the plant bar carries the day's spend, and the job page each
// station's.
//
// A row is one line, project inline beside the title, so the Floor shows 25
// or more jobs on a 1080p screen.
import { useMemo } from "react";
import type { ReactNode } from "react";
import { parseAttemptSummary } from "./api";
import { formatAge, formatDateTime, formatDuration, projectName, stateLabel, taskHeading, taskState } from "./format";
import { buildLane } from "./lanes";
import { buildStations, verdictFor } from "./stations";
import type { StationView } from "./stations";
import { isLocalDelivery, jobStatus, runTone } from "./status";
import type { JobStatus } from "./status";
import { onlyJob, summaryDetail } from "./summary";
import type { Attempt, JobDetail, RunListItem } from "./types";
import { Link } from "./ui";

export function Floor({ runs, nowMs }: { runs: RunListItem[]; nowMs: number }) {
  return (
    <table className="floor" aria-label="Jobs on the line">
      <thead>
        <tr><th>State</th><th>Job</th><th>Stations</th><th>Time</th><th>Age</th><th>Out</th></tr>
      </thead>
      <tbody>
        {runs.map((run) => <FloorRow key={run.id} run={run} nowMs={nowMs} />)}
      </tbody>
    </table>
  );
}

function FloorRow({ run, nowMs }: { run: RunListItem; nowMs: number }) {
  const summary = onlyJob(run);
  const count = (run.job_summaries ?? []).length;
  const title = taskHeading(run);
  const project = (run.targets ?? []).map((target) => projectName(target.repository)).join(", ");
  const name = <td className="floor-job"><span className="floor-job-line"><Link href={`/runs/${run.id}`}>{title}</Link><small title={(run.targets ?? []).map((target) => target.repository).join(", ")}>{project}</small></span></td>;
  const age = <td className="floor-mono floor-age" title={formatDateTime(run.created_at)}>{formatAge(run.created_at, nowMs)}</td>;
  const detail = useMemo(() => summary ? summaryDetail(summary) : undefined, [summary]);
  const attempt = detail?.attempts?.at(-1);

  return (
    <tr className={`floor-row floor-row-${run.state}`}>
      {detail && attempt ? <Cells detail={detail} run={run} attempt={attempt} name={name} age={age} nowMs={nowMs} />
        : count > 1 ? <ManyJobs run={run} count={count} name={name} age={age} />
        : <Unlit status={detail ? jobStatus({ jobState: detail.job.state }) : runStatus(run)} name={name} age={age} />}
    </tr>
  );
}

/**
 * ManyJobs is a run over several repositories. No one job speaks for it, so
 * the row shows the run's state in the same words and colour as its board
 * card and run header, and counts the jobs rather than lighting one job's
 * stations.
 */
function ManyJobs({ run, count, name, age }: { run: RunListItem; count: number; name: ReactNode; age: ReactNode }) {
  const state = taskState(run);
  return (
    <>
      <td><span className={`floor-state floor-state-${runTone(state)}`}>{stateLabel(state)}</span></td>
      {name}
      <td className="floor-mono">{count} jobs</td>
      <td className="floor-mono">—</td>
      {age}
      <td className="floor-mono">—</td>
    </>
  );
}

/**
 * runStatus is a row's status when no job speaks for it: the run state in the
 * words and colour its board card and run header use for the same case.
 */
function runStatus(run: RunListItem): Pick<JobStatus, "word" | "tone"> {
  const state = taskState(run);
  return { word: stateLabel(state), tone: runTone(state) };
}

/** Unlit is a row with nothing to light yet: no job, or no attempt. */
function Unlit({ status, name, age }: { status: Pick<JobStatus, "word" | "tone">; name: ReactNode; age: ReactNode }) {
  return (
    <>
      <td><span className={`floor-state floor-state-${status.tone}`}>{status.word}</span></td>
      {name}
      <td><span className="floor-none">—</span></td>
      <td className="floor-mono">—</td>
      {age}
      <td className="floor-mono">—</td>
    </>
  );
}

/**
 * lampsLabel names the lamps in words. A skipped station is not a failed
 * one, so it is counted apart rather than as "not passed": a done job that
 * skipped a conditional station reads "1 of 1 run stations passed, 1
 * skipped", not "1 of 2". A stop is named in words, not only in red: a screen
 * reader and a colour-blind operator both need to know which station failed.
 */
export function lampsLabel(stations: Pick<StationView, "state" | "label">[]): string {
  const passed = stations.filter((station) => station.state === "pass").length;
  const skipped = stations.filter((station) => station.state === "skip").length;
  const failed = stations.filter((station) => station.state === "bad").map((station) => station.label);
  const counted = stations.length - skipped;
  const passedText = skipped > 0 ? `${passed} of ${counted} run stations passed, ${skipped} skipped` : `${passed} of ${counted} stations passed`;
  return `${passedText}${failed.length ? `; failed: ${failed.join(", ")}` : ""}`;
}

function Cells({ detail, run, attempt, name, age, nowMs }: {
  detail: JobDetail; run: RunListItem; attempt: Attempt; name: ReactNode; age: ReactNode; nowMs: number;
}) {
  const lane = useMemo(() => buildLane(attempt, []), [attempt]);
  const stations = useMemo(
    () => buildStations(detail.stations, detail.delivery, attempt, lane, [], detail.publish ?? [], nowMs),
    [detail, attempt, lane, nowMs],
  );
  const verdict = verdictFor(attempt, stations, detail.delivery, run.parameters?.task_mode === "ask");
  const started = Date.parse(attempt.started_at ?? attempt.created_at);
  const ended = attempt.completed_at ? Date.parse(attempt.completed_at) : nowMs;
  const pullRequest = (detail.publish ?? []).find((record) => record.attempt_id === attempt.id && record.pr_url)?.pr_url;
  const number = /\/pull\/(\d+)/.exec(pullRequest ?? "")?.[1];
  const publish = parseAttemptSummary(attempt.result)?.publish;
  const publishState = typeof publish === "string" ? publish : publish?.state;

  return (
    <>
      <td className="floor-state-cell">
        <span className={`floor-state floor-state-${verdict.tone}`}>{verdict.word}</span>
        {verdict.station && <small className="floor-station">{verdict.station}</small>}
      </td>
      {name}
      <td>
        {stations.length === 0 ? <span className="floor-none">—</span> : <span className="floor-lamps" role="img" aria-label={lampsLabel(stations)}>
          {stations.map((station) => <i key={station.name} className={`floor-lamp floor-lamp-${station.state}`} title={`${station.label}: ${station.note || station.state}`}>{station.state === "bad" ? "✕" : null}</i>)}
        </span>}
      </td>
      <td className="floor-mono">{Number.isFinite(started) ? formatDuration(Math.max(0, ended - started)) : "—"}</td>
      {age}
      <td className="floor-mono">
        {pullRequest ? <a href={pullRequest} rel="noreferrer noopener" target="_blank">PR{number ? ` #${number}` : ""}</a>
          : publishState === "held" ? isLocalDelivery(detail.delivery) ? "kept local" : "held"
          : "—"}
      </td>
    </>
  );
}
