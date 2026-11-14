// consistency.test.tsx — one job, one status word, in every view that shows
// it: the job list row and the board card (built from the run list's job
// summary) and the run header (built from the job detail). A running job's
// current station is a separate label on the row, never the status word.
// A run that stays `mixed` while its job moves on (a publish retry) moves
// every view with it.
import { act, render, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { Floor } from "./Floor";
import { RunDetail } from "./RunDetail";
import { TaskBoard } from "./TaskBoard";
import type { Attempt, AttemptSummary, JobDetail, JobSummary, Run, RunListItem, TraceEvent } from "./types";

const BASE = Date.parse("2026-11-14T10:00:00.000Z");
const at = (offsetMs: number) => new Date(BASE + offsetMs).toISOString();

function run(state: string): Run {
  return {
    id: "run-1", definition_id: "definition-1", definition_generation: 1, snapshot: "name: factory\n",
    parameters: { task_title: "One change" }, targets: [{ repository: "github.com/owner/repo", base_sha: "abc" }],
    state, created_at: at(0), updated_at: at(0),
  };
}

function attempt(state: string, result?: object): Attempt {
  return {
    id: "attempt-1", job_id: "job-1", attempt_number: 1, state, created_at: at(0), started_at: at(0),
    ...(result ? { result: JSON.stringify(result), completed_at: at(5000) } : {}),
  };
}

function detail(jobState: string, attempts: Attempt[]): JobDetail {
  return {
    job: { id: "job-1", run_id: "run-1", repository: "github.com/owner/repo", base_sha: "abcdef", state: jobState, cancellation_requested: false, created_at: at(0), updated_at: at(0) },
    run_id: "run-1", definition: "factory", attempts, publish: [],
    stations: [{ name: "plan", kind: "agent" }, { name: "build", kind: "agent" }],
    delivery: { hold_when: "risk != low", ci_wait: true },
  };
}

// The plan phase has ended and build is open: build is the live station.
const RUNNING_EVENTS: TraceEvent[] = [
  { seq: 1, type: "phase_start", phase: "plan", started_at: at(0) },
  { seq: 2, type: "phase_end", phase: "plan", started_at: at(1000), payload: { status: "success" } },
  { seq: 3, type: "phase_start", phase: "build", started_at: at(1500) },
];

let current: { run: Run; job: JobDetail; events: TraceEvent[] };
let requests: string[] = [];

beforeEach(() => {
  requests = [];
  vi.useFakeTimers({ shouldAdvanceTime: true });
  vi.setSystemTime(new Date(BASE + 60_000));
  vi.stubGlobal("fetch", vi.fn(async (input: string) => {
    requests.push(input);
    const url = new URL(input, "http://127.0.0.1");
    const json = (body: unknown) => new Response(JSON.stringify(body), { status: 200, headers: { "Content-Type": "application/json" } });
    const events = url.pathname.match(/^\/api\/attempts\/([^/]+)\/events$/);
    if (events) {
      const after = Number(url.searchParams.get("after") ?? 0);
      const arrived = current.events.filter((event) => event.seq > after);
      return json({ attempt_id: events[1], after, next_cursor: arrived.at(-1)?.seq ?? after, events: arrived });
    }
    if (/\/(gates|envelopes)$/.test(url.pathname)) return json([]);
    if (url.pathname === "/api/runs/run-1") return json({ run: current.run, jobs: [current.job.job] });
    if (url.pathname === "/api/jobs/job-1") return json(current.job);
    if (url.pathname === "/api/queue") {
      const live = ["queued", "active"].includes(current.job.job.state);
      return json({ depth: 0, active_count: 0, observed_at: at(0), entries: live ? [{ job_id: "job-1", run_id: "run-1", repository: "github.com/owner/repo", base_sha: "abc", state: current.job.job.state, enqueued_at: at(0) }] : [] });
    }
    if (url.pathname === "/api/workers") return json({ workers: [] });
    return json({});
  }));
});

afterEach(() => {
  vi.useRealTimers();
  vi.unstubAllGlobals();
});

/**
 * listed is the run as GET /api/runs lists it: the job summary the server
 * folds from the same ledger the job detail reads — the latest attempt with
 * its publish outcome, and each phase's status from the trace, then the
 * result.
 */
function listed(): RunListItem {
  const attempt = current.job.attempts?.at(-1);
  const parsed = attempt?.result ? JSON.parse(attempt.result) as AttemptSummary : null;
  const publish = parsed?.publish;
  const phases = new Map<string, string>();
  for (const event of current.events) {
    if (event.type === "phase_start" && event.phase) phases.set(event.phase, "running");
    if (event.type === "phase_end" && event.phase) phases.set(event.phase, String((event.payload as { status?: string })?.status ?? "unknown"));
  }
  for (const phase of parsed?.phases ?? []) phases.set(phase.phase, phase.status);
  const summary: JobSummary = {
    job: current.job.job, stations: current.job.stations, delivery: current.job.delivery,
    attempt_count: current.job.attempts?.length ?? 0,
    latest_attempt: attempt ? (({ result: _result, error: _error, ...rest }) => ({
      ...rest,
      publish_state: typeof publish === "string" ? publish : publish?.state,
      publish_code: typeof publish === "string" ? undefined : publish?.code,
    }))(attempt) : undefined,
    phases: [...phases].map(([phase, status]) => ({ phase, status, rounds: 1 })),
    publish: current.job.publish ?? [],
  };
  return { ...current.run, job_summaries: [summary] };
}

function Views() {
  const item = listed();
  return <>
    <Floor runs={[item]} nowMs={BASE + 60_000} />
    <TaskBoard runs={[item]} />
    <RunDetail runID="run-1" />
  </>;
}

function views() {
  return render(<Views />);
}

/** words reads the status word each view shows for the one job. */
function words(container: HTMLElement) {
  const text = (selector: string) => container.querySelector(selector)?.textContent?.trim() ?? null;
  return {
    row: text(".floor-row .floor-state"),
    card: text(".task-card .badge"),
    header: text(".view-header .badge"),
  };
}

describe("one status word across the list row, the board card and the run header", () => {
  it.each([
    ["queued", "active", detail("queued", []), [], "Queued"],
    ["preparing", "active", detail("active", [attempt("preparing")]), [], "Preparing"],
    ["running", "active", detail("active", [attempt("running")]), RUNNING_EVENTS, "Running"],
    ["settled", "accepted", detail("accepted", [attempt("accepted", { phases: [], publish: { state: "published" } })]), [], "Accepted"],
  ] as const)("a %s job", async (_, runState, job, events, word) => {
    current = { run: run(runState), job, events: [...events] };
    const { container } = views();
    await waitFor(() => expect(words(container)).toEqual({ row: word, card: word, header: word }));
    // A running job names its live station beside the status word, not in it.
    const station = container.querySelector(".floor-row .floor-station")?.textContent ?? null;
    if (word === "Running") await waitFor(() => expect(container.querySelector(".floor-row .floor-station")?.textContent).toBe("build"));
    else expect(station).toBeNull();
  });
});

describe("a run that stays mixed while its job moves on", () => {
  it("follows a held job to not published, then stops polling once nothing can change", async () => {
    const result = (publish: object) => ({ phases: [], publish });
    current = { run: run("mixed"), job: detail("accepted_unpublished", [attempt("accepted_unpublished", result({ state: "held", code: "publish_held" }))]), events: [] };
    const view = views();
    const { container } = view;
    await waitFor(() => expect(words(container)).toEqual({ row: "Held", card: "Held", header: "Held" }));

    // The release ran and its publish failed: the job is unpublished for a new
    // reason, and the run is still `mixed`. The row and card move with the
    // run list's next poll; the header with its own.
    current.job = detail("accepted_unpublished", [attempt("accepted_unpublished", result({ state: "failed", code: "push_failed" }))]);
    view.rerender(<Views />);
    await act(() => vi.advanceTimersByTimeAsync(4500));
    await waitFor(() => expect(words(container)).toEqual({ row: "Not published", card: "Not published", header: "Not published" }));

    // A retry that finds nothing to publish leaves no action open; every view
    // settles on Done.
    current.job = detail("accepted_unpublished", [attempt("accepted_unpublished", result({ state: "failed", code: "publish_empty_changeset" }))]);
    view.rerender(<Views />);
    await act(() => vi.advanceTimersByTimeAsync(4500));
    await waitFor(() => expect(words(container)).toEqual({ row: "Done", card: "Done", header: "Done" }));
    // The row and the card read nothing of their own: every request is the
    // run header's or its job panel's.
    expect(requests.filter((url) => url.startsWith("/api/runs") && url !== "/api/runs/run-1")).toEqual([]);
  });
});
