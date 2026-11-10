// Floor.tsx — the jobs on the line, one row each. A row carries the job's
// stations as lamps, so a job that is held, stopped, or still three stations
// from done reads at a glance, without opening it.
//
// A row is built from the job detail alone when the job is finished (the
// attempt result records every phase), and follows the event stream only
// while an attempt is live. Spend is not on the row: it lives in the event
// stream, and reading every finished job's trace to print one figure per row
// is not worth the requests — the plant bar carries the day's spend, and the
// job page carries each station's.
import { useCallback, useMemo } from "react";
import type { ReactNode } from "react";
import { api, parseAttemptSummary } from "./api";
import { formatDuration, isTerminalAttempt, projectName, taskHeading } from "./format";
import { buildLane } from "./lanes";
import { useAttemptEvents, usePolled } from "./polling";
import { buildStations, verdictFor } from "./stations";
import type { Attempt, JobDetail, Run, TraceEvent } from "./types";
import { Link } from "./ui";

const SETTLED_RUNS = ["accepted", "failed", "cancelled", "mixed"];
const NO_EVENTS: TraceEvent[] = [];

export function Floor({ runs, nowMs }: { runs: Run[]; nowMs: number }) {
  return (
    <table className="floor" aria-label="Jobs on the line">
      <thead>
        <tr><th>State</th><th>Job</th><th>Stations</th><th>Time</th><th>Out</th></tr>
      </thead>
      <tbody>
        {runs.map((run) => <FloorRow key={run.id} run={run} nowMs={nowMs} />)}
      </tbody>
    </table>
  );
}

function FloorRow({ run, nowMs }: { run: Run; nowMs: number }) {
  const settled = SETTLED_RUNS.includes(run.state);
  const loadView = useCallback(() => api.run(run.id), [run.id]);
  const view = usePolled(loadView, settled ? 0 : 4000, `floor:run:${run.id}:${run.state}`);
  const jobID = view.data?.jobs?.[0]?.id;
  const title = taskHeading(run);
  const project = (run.targets ?? []).map((target) => projectName(target.repository)).join(", ");
  const name = <td className="floor-job"><Link href={`/runs/${run.id}`}>{title}</Link><small>{project}</small></td>;

  return (
    <tr className={`floor-row floor-row-${run.state}`}>
      {jobID
        ? <FloorJob jobID={jobID} run={run} name={name} settled={settled} nowMs={nowMs} />
        : <Unlit state={run.state} name={name} />}
    </tr>
  );
}

/** Unlit is a row with nothing to light yet: no job, or no attempt. */
function Unlit({ state, name }: { state: string; name: ReactNode }) {
  return (
    <>
      <td><span className="floor-state floor-state-idle">{state}</span></td>
      {name}
      <td><span className="floor-none">—</span></td>
      <td className="floor-mono">—</td>
      <td className="floor-mono">—</td>
    </>
  );
}

function FloorJob({ jobID, run, name, settled, nowMs }: {
  jobID: string; run: Run; name: ReactNode; settled: boolean; nowMs: number;
}) {
  const loadJob = useCallback(() => api.job(jobID), [jobID]);
  const job = usePolled(loadJob, settled ? 0 : 4000, `floor:job:${jobID}:${run.state}`);
  const attempt = job.data?.attempts?.at(-1);
  if (!job.data || !attempt) return <Unlit state={job.data ? "queued" : run.state} name={name} />;
  // Only a live attempt follows its trace; a finished one is lit from its
  // result, so a floor of finished jobs polls nothing.
  return isTerminalAttempt(attempt.state)
    ? <Cells detail={job.data} attempt={attempt} events={NO_EVENTS} name={name} nowMs={nowMs} />
    : <LiveCells detail={job.data} attempt={attempt} name={name} nowMs={nowMs} />;
}

function LiveCells(props: { detail: JobDetail; attempt: Attempt; name: ReactNode; nowMs: number }) {
  const { events } = useAttemptEvents(props.attempt.id, props.attempt.state);
  return <Cells {...props} events={events} />;
}

function Cells({ detail, attempt, events, name, nowMs }: {
  detail: JobDetail; attempt: Attempt; events: TraceEvent[]; name: ReactNode; nowMs: number;
}) {
  const lane = useMemo(() => buildLane(attempt, events), [attempt, events]);
  const stations = useMemo(
    () => buildStations(detail.stations, detail.delivery, attempt, lane, events, detail.publish ?? [], nowMs),
    [detail, attempt, lane, events, nowMs],
  );
  const verdict = verdictFor(attempt, stations, detail.delivery);
  const started = Date.parse(attempt.started_at ?? attempt.created_at);
  const ended = attempt.completed_at ? Date.parse(attempt.completed_at) : nowMs;
  const pullRequest = (detail.publish ?? []).find((record) => record.attempt_id === attempt.id && record.pr_url)?.pr_url;
  const number = /\/pull\/(\d+)/.exec(pullRequest ?? "")?.[1];
  const publish = parseAttemptSummary(attempt.result)?.publish;
  const publishState = typeof publish === "string" ? publish : publish?.state;
  const passed = stations.filter((station) => station.state === "pass").length;

  return (
    <>
      <td><span className={`floor-state floor-state-${verdict.tone}`}>{verdict.word}</span></td>
      {name}
      <td>
        {stations.length === 0 ? <span className="floor-none">—</span> : <span className="floor-lamps" role="img" aria-label={`${passed} of ${stations.length} stations passed`}>
          {stations.map((station) => <i key={station.name} className={`floor-lamp floor-lamp-${station.state}`} title={`${station.label}: ${station.note || station.state}`} />)}
        </span>}
      </td>
      <td className="floor-mono">{Number.isFinite(started) ? formatDuration(Math.max(0, ended - started)) : "—"}</td>
      <td className="floor-mono">
        {pullRequest ? <a href={pullRequest} rel="noreferrer noopener" target="_blank">PR{number ? ` #${number}` : ""}</a>
          : publishState === "held" ? (detail.delivery?.hold_when ?? "").includes("jig_ui_delivery") ? "kept local" : "held"
          : "—"}
      </td>
    </>
  );
}
