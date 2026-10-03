// TaskBoard.test.tsx — the List | Board switch in My tasks (2026-10-02-001 U2):
// List stays the default, the choice survives disabled storage, the board
// degrades rather than blanks when the queue fails, and the queue is polled
// only while the board is on screen.
import { act, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { Runs } from "./Queue";
import type { QueueEntry, Run } from "./types";

function run(id: string, state: string, title = id): Run {
  return {
    id,
    definition_id: "definition-1",
    definition_generation: 1,
    snapshot: "",
    parameters: { task_title: title },
    targets: [{ repository: "/tmp/project", base_sha: "abc" }],
    state,
    created_at: "2026-10-02T10:00:00Z",
    updated_at: "2026-10-02T10:00:00Z",
  };
}

function entry(runID: string, state: string): QueueEntry {
  return { job_id: `${runID}-job`, run_id: runID, repository: "/tmp/project", base_sha: "abc", state, enqueued_at: "2026-10-02T10:00:00Z" };
}

const RUNS = [
  run("run-waiting", "active", "Waiting task"),
  run("run-running", "active", "Running task"),
  run("run-done", "accepted", "Done task"),
  run("run-failed", "failed", "Failed task"),
];

/** requests records every URL fetched, so a test can count queue polls. */
let requests: string[] = [];

function mockAPI({ runs = RUNS, queueFails = false }: { runs?: Run[]; queueFails?: boolean } = {}) {
  vi.stubGlobal("fetch", vi.fn(async (url: string) => {
    requests.push(url);
    const json = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });
    if (url === "/api/runs") return json(runs);
    if (url === "/api/queue") {
      if (queueFails) return json({ error: { code: "queue_unavailable", message: "Queue is unavailable." } }, 500);
      return json({ depth: 1, active_count: 1, entries: [entry("run-waiting", "queued"), entry("run-running", "active")], observed_at: "2026-10-02T10:00:00Z" });
    }
    return json({});
  }));
}

function mockStorage(stored: string | null = null) {
  const storage = { getItem: vi.fn(() => stored), setItem: vi.fn() };
  vi.stubGlobal("localStorage", storage);
  return storage;
}

const queueCalls = () => requests.filter((url) => url === "/api/queue").length;

function column(label: string): HTMLElement {
  return screen.getByRole("region", { name: new RegExp(`^${label} \\d+$`) });
}

beforeEach(() => {
  requests = [];
});

afterEach(() => {
  vi.useRealTimers();
  vi.unstubAllGlobals();
});

it("defaults to the List layout and never asks for the queue", async () => {
  mockStorage();
  mockAPI();
  render(<Runs />);
  expect(await screen.findByText("Waiting task")).toBeInTheDocument();
  expect(screen.getByRole("button", { name: "List" })).toHaveAttribute("aria-pressed", "true");
  expect(screen.queryByRole("region", { name: /^Waiting/ })).not.toBeInTheDocument();
  expect(queueCalls()).toBe(0);
});

it("switches to five ordered columns with counts and remembers the choice", async () => {
  const storage = mockStorage();
  mockAPI();
  render(<Runs />);
  fireEvent.click(await screen.findByRole("button", { name: "Board" }));

  await waitFor(() => expect(within(column("Waiting")).getByText("Waiting task")).toBeInTheDocument());
  const headings = screen.getAllByRole("region").map((region) => within(region).getByRole("heading", { level: 2 }).textContent);
  expect(headings).toEqual(["Waiting 1", "Running 1", "Needs review 0", "Done 1", "Stopped 1"]);
  expect(within(column("Running")).getByText("Running task")).toBeInTheDocument();
  expect(within(column("Needs review")).getByText("Nothing here.")).toBeInTheDocument();
  expect(within(column("Done")).getByRole("link", { name: "Done task" })).toHaveAttribute("href", "/runs/run-done");
  expect(within(column("Stopped")).getByRole("link", { name: "Failed task" })).toHaveAttribute("href", "/runs/run-failed");
  expect(screen.getByRole("button", { name: "Board" })).toHaveAttribute("aria-pressed", "true");
  expect(storage.setItem).toHaveBeenCalledWith("jig-tasks-layout", "board");
});

it("opens on the board when the stored layout says so", async () => {
  mockStorage("board");
  mockAPI();
  render(<Runs />);
  expect(await screen.findByRole("region", { name: /^Waiting/ })).toBeInTheDocument();
  expect(screen.getAllByRole("region")).toHaveLength(5);
});

it("falls back to List when storage cannot be read, and still switches when it cannot be written", async () => {
  vi.stubGlobal("localStorage", {
    getItem: () => { throw new Error("storage disabled"); },
    setItem: () => { throw new Error("storage disabled"); },
  });
  mockAPI();
  render(<Runs />);
  expect(await screen.findByText("Waiting task")).toBeInTheDocument();
  expect(screen.getByRole("button", { name: "List" })).toHaveAttribute("aria-pressed", "true");

  fireEvent.click(screen.getByRole("button", { name: "Board" }));
  expect(await screen.findByRole("region", { name: /^Waiting/ })).toBeInTheDocument();
});

it("renders from runs with active work under Running when the queue fails", async () => {
  mockStorage("board");
  mockAPI({ queueFails: true });
  render(<Runs />);
  expect(await screen.findByRole("alert")).toHaveTextContent("Queue is unavailable.");
  expect(within(column("Running")).getByText("Waiting task")).toBeInTheDocument();
  expect(within(column("Running")).getByText("Running task")).toBeInTheDocument();
  expect(within(column("Waiting")).getByText("Nothing here.")).toBeInTheDocument();
  expect(within(column("Done")).getByText("Done task")).toBeInTheDocument();
});

it("shows the empty state, not five empty columns, when there are no tasks", async () => {
  for (const layout of [null, "board"]) {
    mockStorage(layout);
    mockAPI({ runs: [] });
    const { unmount } = render(<Runs />);
    expect(await screen.findByText("No tasks yet")).toBeInTheDocument();
    expect(screen.queryAllByRole("region")).toHaveLength(0);
    unmount();
  }
});

it("stops polling the queue once the List layout is back", async () => {
  vi.useFakeTimers({ shouldAdvanceTime: true });
  mockStorage("board");
  mockAPI();
  render(<Runs />);
  await screen.findByRole("region", { name: /^Waiting/ });
  await act(() => vi.advanceTimersByTimeAsync(2500));
  expect(queueCalls()).toBeGreaterThanOrEqual(2);

  fireEvent.click(screen.getByRole("button", { name: "List" }));
  const after = queueCalls();
  await act(() => vi.advanceTimersByTimeAsync(10_000));
  expect(queueCalls()).toBe(after);
  expect(screen.queryByRole("region", { name: /^Waiting/ })).not.toBeInTheDocument();
});
