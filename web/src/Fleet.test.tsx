// Fleet.test.tsx — the two things the fleet view must not get wrong: it
// reports the SERVER's liveness verdict rather than deriving one from the
// browser clock, and it measures heartbeat age against the payload's own
// observation time so a drifted clock cannot invent staleness.
import { render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { Fleet, heartbeatAge } from "./Fleet";
import type { FleetMember, FleetView } from "./types";

const OBSERVED = "2026-08-06T10:00:00.000Z";
const at = (offsetMs: number) => new Date(Date.parse(OBSERVED) + offsetMs).toISOString();

function member(overrides: Partial<FleetMember> = {}): FleetMember {
  return {
    id: "worker-a",
    name: "local",
    worker_version: "dev",
    capacity: 2,
    active_count: 0,
    available: 2,
    live: true,
    env_names: ["GITHUB_TOKEN", "PATH"],
    runtimes: [{ name: "claude-code", version: "2.1.4", can_resume: true, reports_cost: true }],
    registered_at: at(-60_000),
    last_heartbeat: at(-2_000),
    ...overrides,
  };
}

function mockFleet(view: Partial<FleetView> & { workers: FleetMember[] | null }) {
  vi.stubGlobal(
    "fetch",
    vi.fn(
      async () =>
        new Response(
          JSON.stringify({
            live_count: 0,
            stale_count: 0,
            available_slots: 0,
            observed_at: OBSERVED,
            ...view,
          }),
          { status: 200, headers: { "Content-Type": "application/json" } },
        ),
    ),
  );
}

beforeEach(() => {
  vi.useFakeTimers({ shouldAdvanceTime: true });
  // Deliberately hours away from the payload's observation time: nothing the
  // view renders may move because the browser clock disagrees.
  vi.setSystemTime(new Date(Date.parse(OBSERVED) + 6 * 3_600_000));
});

afterEach(() => {
  vi.useRealTimers();
});

it("reports the server's liveness verdict and the runtime each worker owns", async () => {
  mockFleet({
    workers: [
      member({ id: "worker-a", name: "local", live: true, active_count: 1, available: 1 }),
      member({
        id: "worker-b",
        name: "spare",
        live: false,
        capacity: 4,
        active_count: 0,
        available: 4,
        last_heartbeat: at(-90_000),
        runtimes: [{ name: "codex", version: "0.9.1", can_resume: true, reports_cost: false }],
      }),
    ],
    live_count: 1,
    stale_count: 1,
    available_slots: 1,
  });

  render(<Fleet />);

  const local = await waitFor(() => screen.getByText("local").closest("tr")!);
  expect(within(local).getByText("Online")).toBeInTheDocument();
  expect(within(local).getByText("claude-code 2.1.4")).toBeInTheDocument();
  expect(within(local).getByText("1 / 2")).toBeInTheDocument();
  expect(within(local).getByText("2.0s ago")).toBeInTheDocument();

  // Stale is the server's word, not an inference: worker-b's four idle slots
  // must not be counted as capacity anywhere in the view.
  const spare = screen.getByText("spare").closest("tr")!;
  expect(within(spare).getByText("Offline")).toBeInTheDocument();
  expect(within(spare).getByText("codex 0.9.1")).toBeInTheDocument();
  expect(screen.getByText("free slots").previousSibling).toHaveTextContent("1");

  // A real worker advertises dozens of env names, so the cell counts them
  // and keeps the list in its title rather than swamping the row.
  const env = within(local).getByText("2 names");
  expect(env).toHaveAttribute("title", "GITHUB_TOKEN PATH");
});

it("names the attempts stranded on a worker that stopped heartbeating", async () => {
  mockFleet({
    workers: [member({ live: false, active_count: 2, available: 0 })],
    stale_count: 1,
  });

  render(<Fleet />);

  expect(await screen.findByText(/2 attempts are leased to a worker/)).toBeInTheDocument();
});

it("sends the operator to the worker they have not started", async () => {
  mockFleet({ workers: [] });

  render(<Fleet />);

  expect(await screen.findByText("No workers registered")).toBeInTheDocument();
});

it("measures heartbeat age against the server's observation, never the browser clock", () => {
  expect(heartbeatAge(member({ last_heartbeat: at(-45_000) }), OBSERVED)).toBe("45s ago");
  // A heartbeat stamped a shade ahead of the observation is clock jitter
  // inside one process, not a negative age.
  expect(heartbeatAge(member({ last_heartbeat: at(500) }), OBSERVED)).toBe("0ms ago");
});
