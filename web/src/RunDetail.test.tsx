// RunDetail.test.tsx — the two things the UI must not get wrong: swim-lane
// rendering (one lane per attempt, tool calls with real durations, live
// before any envelope exists) and cursor behaviour (per-attempt seq cursors
// that never interleave and never skip replayed events).
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { AttemptLane, JobPanel } from "./RunDetail";
import { mergeEvents } from "./polling";
import type { Attempt, EventPage, TraceEvent } from "./types";

const BASE = Date.parse("2026-08-06T10:00:00.000Z");
const at = (offsetMs: number) => new Date(BASE + offsetMs).toISOString();

function attempt(overrides: Partial<Attempt> = {}): Attempt {
  return {
    id: "attempt-1",
    job_id: "job-1",
    attempt_number: 1,
    state: "running",
    created_at: at(0),
    started_at: at(0),
    ...overrides,
  };
}

function event(
  seq: number,
  type: string,
  offsetMs: number,
  extra: Partial<TraceEvent> = {},
): TraceEvent {
  return { seq, type, started_at: at(offsetMs), ...extra };
}

function page(attemptID: string, events: TraceEvent[], after = 0): EventPage {
  return {
    attempt_id: attemptID,
    after,
    next_cursor: events.length ? events[events.length - 1].seq : after,
    events,
  };
}

/** requests records every URL the component fetched, in order. */
let requests: string[] = [];

/**
 * mockAPI serves the read routes from a table of responses. Event routes take
 * a queue per attempt so a test can script an outage: an empty page followed
 * by the replayed events.
 */
function mockAPI(routes: {
  job?: unknown;
  events?: Record<string, EventPage[]>;
  gates?: unknown;
  envelopes?: unknown;
}) {
  const queues = routes.events ?? {};
  vi.stubGlobal(
    "fetch",
    vi.fn(async (input: string) => {
      requests.push(input);
      const url = new URL(input, "http://127.0.0.1");
      const json = (body: unknown) =>
        new Response(JSON.stringify(body), {
          status: 200,
          headers: { "Content-Type": "application/json" },
        });
      const eventMatch = url.pathname.match(/^\/api\/attempts\/([^/]+)\/events$/);
      if (eventMatch) {
        const queue = queues[eventMatch[1]] ?? [];
        const after = Number(url.searchParams.get("after") ?? 0);
        const next = queue.shift() ?? page(eventMatch[1], [], after);
        return json({ ...next, after });
      }
      if (/\/gates$/.test(url.pathname)) return json(routes.gates ?? []);
      if (/\/envelopes$/.test(url.pathname)) return json(routes.envelopes ?? []);
      if (/^\/api\/jobs\//.test(url.pathname)) return json(routes.job ?? {});
      return json({});
    }),
  );
}

beforeEach(() => {
  requests = [];
  vi.useFakeTimers({ shouldAdvanceTime: true });
  vi.setSystemTime(new Date(BASE + 60_000));
});

afterEach(() => {
  vi.useRealTimers();
});

describe("swim lanes", () => {
  it("shows the agent's directive and automatic repair activity", async () => {
    mockAPI({ events: { "attempt-1": [page("attempt-1", [
      event(1, "phase_start", 0, { phase: "plan" }),
      event(2, "handoff", 1000, { phase: "plan", payload: { summary: "Add validation to the existing input parser." } }),
      event(3, "log", 2000, { phase: "test", name: "repair_edge", payload: { run: "plan", use: 1, budget: 3 } }),
    ])] } });
    render(<AttemptLane compact taskMode="auto" attempt={attempt()} nowMs={BASE + 10000} />);
    expect(await screen.findByText("Add validation to the existing input parser.")).toBeVisible();
    expect(screen.getByText("Check failed → agents are repairing and retesting automatically")).toBeVisible();
  });

  it("shows live progress messages and activity without opening technical details", async () => {
    mockAPI({ events: { "attempt-1": [page("attempt-1", [
      event(1, "phase_start", 0, { phase: "scout" }),
      event(2, "log", 1000, { phase: "scout", name: "agent_text", payload: { text: "I am checking the installation instructions." } }),
      event(3, "tool_call", 2000, { phase: "scout", name: "command_execution" }),
    ])] } });
    render(<AttemptLane compact taskMode="ask" attempt={attempt()} nowMs={BASE + 10000} />);
    expect(await screen.findByText("I am checking the installation instructions.")).toBeVisible();
    expect(screen.getByText("Codex used command_execution")).toBeVisible();
    expect(screen.getByText(/Last activity 8.0s ago/)).toBeVisible();
  });

  it("keeps the answer visible when a code check follows it", async () => {
    mockAPI({});
    render(<AttemptLane compact taskMode="ask" attempt={attempt({
      state: "accepted_unpublished",
      result: JSON.stringify({ phases: [
        { phase: "scout", kind: "agent", status: "success", envelope: { summary: "Your answer is here." } },
        { phase: "keep-local", kind: "code", status: "success", envelope: { summary: "command exited 0" } },
      ] }),
    })} nowMs={BASE} />);
    expect(await screen.findByText("Your answer is here.")).toBeVisible();
    expect(screen.getByText("command exited 0")).not.toBeVisible();
  });

  it("shows a phase's readable result without opening technical evidence", async () => {
    mockAPI({});
    render(<AttemptLane attempt={attempt({
      state: "accepted_unpublished",
      result: JSON.stringify({ phases: [{ phase: "assess", phase_attempt: 1, status: "success", envelope: { summary: "The setup guide references a missing script." } }] }),
    })} nowMs={BASE + 10_000} />);
    expect(await screen.findByText("The setup guide references a missing script.")).toBeVisible();
    expect(screen.getByText("Accepted · not published")).toBeVisible();
  });

  it("keeps the agent's rejection visible even when Jig's checks passed", async () => {
    mockAPI({});
    render(<AttemptLane attempt={attempt({
      state: "accepted_unpublished",
      result: JSON.stringify({
        publish: { state: "failed", code: "publish_empty_changeset" },
        acceptance: { passed: true, checks: [{ item: "verdict_consistent", ok: true }] },
        phases: [{ phase: "assess", phase_attempt: 1, status: "success", envelope: { approved: false, summary: "Setup cannot complete.", blocking: ["Missing setup script"] } }],
      }),
    })} nowMs={BASE + 10_000} />);
    expect(await screen.findByText("Checks passed · nothing to publish")).toBeVisible();
    expect(screen.getByText("Agent assessment: Not approved")).toBeVisible();
    expect(screen.getByText("Missing setup script")).toBeVisible();
  });

  it("opens a selected phase's evidence and preserves unrecognized raw results", async () => {
    mockAPI({ events: { "attempt-1": [page("attempt-1", [
      event(1, "phase_start", 0, { phase: "plan" }),
    ])] } });
    render(<AttemptLane attempt={attempt({ result: "historical non-JSON result" })} nowMs={BASE + 10_000} />);
    fireEvent.click(await screen.findByLabelText(/phase plan, running/));
    expect(screen.getByText(/Evidence · plan/).closest("details")).toHaveAttribute("open");
    expect(screen.getByText("historical non-JSON result")).toBeInTheDocument();
  });

  it("renders a running agent's tool calls with durations before an envelope exists", async () => {
    mockAPI({
      events: {
        "attempt-1": [
          page("attempt-1", [
            event(1, "phase_start", 0, { phase: "plan" }),
            event(2, "agent_start", 500, { phase: "plan", name: "writer" }),
            event(3, "tool_call", 1_000, { phase: "plan", name: "Read" }),
            event(4, "tool_call", 3_500, { phase: "plan", name: "Write" }),
          ]),
        ],
      },
    });

    render(<AttemptLane attempt={attempt()} nowMs={BASE + 10_000} />);

    const lane = await screen.findByLabelText("attempt 1 swim lane");
    // The phase is still open and the attempt has no result: no envelope yet.
    expect(within(lane).getByLabelText(/phase plan, running/)).toBeInTheDocument();

    const tools = within(lane).getByLabelText("attempt 1 tool calls");
    const rows = within(tools).getAllByRole("listitem");
    expect(rows).toHaveLength(2);
    // Read ran from 1s to 3.5s — a real wall-clock span folded into one row.
    expect(rows[0]).toHaveTextContent("Read");
    expect(rows[0]).toHaveTextContent("2.5s");
    // Write is still running, so its duration is measured against now.
    expect(rows[1]).toHaveTextContent("Write");
    expect(rows[1]).toHaveTextContent("6.5s");
    expect(rows[1]).toHaveTextContent("running");
  });

  it("gives attempt 2 of a retried job its own lane while attempt 1 stays inspectable", async () => {
    mockAPI({
      job: {
        job: {
          id: "job-1",
          run_id: "run-1",
          repository: "github.com/example/repo",
          base_sha: "abcdef1234567890",
          state: "active",
          cancellation_requested: false,
          created_at: at(0),
          updated_at: at(0),
        },
        run_id: "run-1",
        definition: "plan-build-test",
        attempts: [
          attempt({
            id: "attempt-1",
            attempt_number: 1,
            state: "failed",
            completed_at: at(9_000),
            error: "gate files_non_empty failed",
          }),
          attempt({ id: "attempt-2", attempt_number: 2, state: "running", started_at: at(20_000) }),
        ],
        publish: [],
      },
      events: {
        "attempt-1": [
          page("attempt-1", [
            event(1, "phase_start", 0, { phase: "plan" }),
            event(2, "tool_call", 1_000, { phase: "plan", name: "LegacyRead" }),
            event(3, "phase_end", 8_000, { phase: "plan", payload: { status: "fail" } }),
          ]),
        ],
        "attempt-2": [
          page("attempt-2", [
            event(1, "phase_start", 20_000, { phase: "plan" }),
            event(2, "tool_call", 21_000, { phase: "plan", name: "FreshWrite" }),
          ]),
        ],
      },
    });

    render(<JobPanel jobID="job-1" nowMs={BASE + 30_000} />);

    const first = await screen.findByLabelText("attempt 1 swim lane");
    const second = await screen.findByLabelText("attempt 2 swim lane");
    expect(within(first).getByText("Attempt 1")).toBeInTheDocument();
    expect(within(second).getByText("Attempt 2")).toBeInTheDocument();

    // Each lane shows only its own events: the cursors are per-attempt, so
    // one lane's history can never leak into the other's.
    expect(within(first).getByText("LegacyRead")).toBeInTheDocument();
    expect(within(first).queryByText("FreshWrite")).toBeNull();
    expect(within(second).getByText("FreshWrite")).toBeInTheDocument();
    expect(within(second).queryByText("LegacyRead")).toBeNull();

    // Attempt 1 stays inspectable after attempt 2 exists.
    expect(within(first).getByLabelText(/phase plan, fail/)).toBeInTheDocument();
    expect(screen.getByText("gate files_non_empty failed")).toBeInTheDocument();
    fireEvent.click(screen.getByText("Compare 2 attempts"));
    const comparison = screen.getByRole("table");
    expect(within(comparison).getAllByRole("row")).toHaveLength(3);
    expect(within(comparison).getByText("Work did not pass")).toBeVisible();
    expect(within(comparison).getByText("Work is running")).toBeVisible();

    const cursored = requests.filter((url) => url.includes("/events?"));
    expect(cursored.some((url) => url.includes("attempt-1") && url.includes("after=0"))).toBe(true);
    expect(cursored.some((url) => url.includes("attempt-2") && url.includes("after=0"))).toBe(true);
  });
});

it.each(["failed", "accepted_unpublished", "active", "queued"])("only offers API-supported actions for a %s job", async (state) => {
  mockAPI({ job: {
    job: { id: "job-1", run_id: "run-1", repository: "example/repo", base_sha: "abcdef", state, cancellation_requested: false },
    run_id: "run-1", attempts: [], publish: [],
  } });
  render(<JobPanel jobID="job-1" nowMs={BASE} />);
  await screen.findByText("example/repo");
  expect(Boolean(screen.queryByRole("button", { name: "Retry same workflow" }))).toBe(state === "failed");
  expect(Boolean(screen.queryByRole("button", { name: "Cancel" }))).toBe(["queued", "active"].includes(state));
});

describe("seq cursors", () => {
  it("advances only over delivered events and renders replayed events in order", async () => {
    mockAPI({
      events: {
        "attempt-1": [
          // First poll: two events land, cursor moves to 2.
          page("attempt-1", [
            event(1, "phase_start", 0, { phase: "plan" }),
            event(2, "tool_call", 1_000, { phase: "plan", name: "Read" }),
          ]),
          // The ingest outage: nothing new, so the cursor must not move.
          page("attempt-1", [], 2),
          // Recovery: the worker's replayed buffer arrives, in seq order.
          page("attempt-1", [
            event(3, "tool_call", 2_000, { phase: "plan", name: "Edit" }),
            event(4, "tool_call", 3_000, { phase: "plan", name: "Bash" }),
            event(5, "phase_end", 4_000, { phase: "plan", payload: { status: "success" } }),
          ]),
        ],
      },
    });

    render(<AttemptLane attempt={attempt()} nowMs={BASE + 10_000} />);
    await screen.findByText("Read");

    await vi.advanceTimersByTimeAsync(1_000);
    await vi.advanceTimersByTimeAsync(1_000);

    await waitFor(() => expect(screen.getByText("Bash")).toBeInTheDocument());

    // One more tick so the cursor's post-replay position is observable.
    await vi.advanceTimersByTimeAsync(1_000);
    const cursors = requests
      .filter((url) => url.includes("/events?"))
      .map((url) => Number(new URL(url, "http://127.0.0.1").searchParams.get("after")));
    // 0 for the first poll, then 2 for every poll until events beyond 2 land:
    // the cursor never advanced past an event that had not arrived, so the
    // replay could not be skipped. Only delivery moves it, and only forward.
    expect(cursors[0]).toBe(0);
    expect(cursors.slice(1, -1).every((value) => value === 2)).toBe(true);
    expect(cursors.at(-1)).toBe(5);
    expect([...cursors].sort((left, right) => left - right)).toEqual(cursors);

    const tools = screen.getByLabelText("attempt 1 tool calls");
    const names = within(tools)
      .getAllByRole("listitem")
      .map((row) => row.textContent ?? "");
    expect(names[0]).toContain("Read");
    expect(names[1]).toContain("Edit");
    expect(names[2]).toContain("Bash");
  });

  it("renders a replayed event exactly once", () => {
    const existing = [event(1, "phase_start", 0), event(2, "tool_call", 100, { name: "Read" })];
    const replayed = [
      event(2, "tool_call", 100, { name: "Read" }),
      event(3, "tool_call", 200, { name: "Write" }),
    ];
    const merged = mergeEvents(existing, replayed);
    expect(merged.map((item) => item.seq)).toEqual([1, 2, 3]);
  });

  it("keeps out-of-order arrivals in seq order", () => {
    const merged = mergeEvents(
      [event(3, "log", 300)],
      [event(1, "log", 100), event(2, "log", 200)],
    );
    expect(merged.map((item) => item.seq)).toEqual([1, 2, 3]);
  });
});
