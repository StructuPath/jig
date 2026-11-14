// TaskBoard.test.tsx — the List | Board switch on Jobs (2026-10-02-001 U2):
// List stays the default, the layout lives in the URL with the stored
// preference as fallback, the choice survives disabled storage, and the
// board is built from the run list's job summaries with no request of its
// own.
import { act, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { Runs } from "./Queue";
import type { JobSummary, RunListItem } from "./types";

function summary(runID: string, jobState: string, attemptState?: string): JobSummary {
  return {
    job: { id: `${runID}-job`, run_id: runID, repository: "/tmp/project", base_sha: "abc", state: jobState, cancellation_requested: false, created_at: "2026-10-02T10:00:00Z", updated_at: "2026-10-02T10:00:00Z" },
    attempt_count: attemptState ? 1 : 0,
    latest_attempt: attemptState ? { id: `${runID}-a1`, job_id: `${runID}-job`, attempt_number: 1, state: attemptState, created_at: "2026-10-02T10:00:00Z" } : undefined,
    phases: [], publish: [],
  };
}

function run(id: string, state: string, title = id, jobs: JobSummary[] = []): RunListItem {
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
    job_summaries: jobs,
  };
}

const RUNS = [
  run("run-waiting", "active", "Waiting task", [summary("run-waiting", "queued")]),
  run("run-running", "active", "Running task", [summary("run-running", "active", "running")]),
  run("run-done", "accepted", "Done task", [summary("run-done", "accepted", "accepted")]),
  run("run-failed", "failed", "Failed task", [summary("run-failed", "failed", "failed")]),
];

/** requests records every URL fetched, so a test can count them. */
let requests: string[] = [];

function mockAPI({ runs = RUNS, next = null as string | null, older = [] as RunListItem[] } = {}) {
  vi.stubGlobal("fetch", vi.fn(async (url: string) => {
    requests.push(url);
    const json = (body: unknown, headers: Record<string, string> = {}) => new Response(JSON.stringify(body), { status: 200, headers: { "Content-Type": "application/json", ...headers } });
    if (url === "/api/runs") return json(runs, next ? { "X-Next-Before": next } : {});
    if (url.startsWith("/api/runs?before=")) return json(older);
    return json({});
  }));
}

function mockStorage(stored: string | null = null) {
  const storage = { getItem: vi.fn(() => stored), setItem: vi.fn() };
  vi.stubGlobal("localStorage", storage);
  return storage;
}

function column(label: string): HTMLElement {
  return screen.getByRole("region", { name: new RegExp(`^${label} \\d+$`) });
}

beforeEach(() => {
  requests = [];
  window.history.replaceState({}, "", "/runs");
});

afterEach(() => {
  vi.useRealTimers();
  vi.unstubAllGlobals();
});

it("defaults to the List layout", async () => {
  mockStorage();
  mockAPI();
  render(<Runs />);
  expect(await screen.findByText("Waiting task")).toBeInTheDocument();
  expect(screen.getByRole("button", { name: "List" })).toHaveAttribute("aria-pressed", "true");
  expect(screen.queryByRole("region", { name: /^Waiting/ })).not.toBeInTheDocument();
  expect(document.title).toBe("Jobs · jig");
});

it("switches to five ordered columns with counts, names the layout in the URL, and remembers it", async () => {
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
  expect(window.location.search).toBe("?view=board");
  expect(document.title).toBe("Board · jig");
});

it("opens on the board when the stored layout says so", async () => {
  mockStorage("board");
  mockAPI();
  render(<Runs />);
  expect(await screen.findByRole("region", { name: /^Waiting/ })).toBeInTheDocument();
  expect(screen.getAllByRole("region")).toHaveLength(5);
});

it("lets a layout in the URL win over the stored one", async () => {
  mockStorage("board");
  mockAPI();
  window.history.replaceState({}, "", "/runs?view=list");
  render(<Runs />);
  expect(await screen.findByText("Waiting task")).toBeInTheDocument();
  expect(screen.queryByRole("region", { name: /^Waiting/ })).not.toBeInTheDocument();
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

it("shows the empty state with the next command, not five empty columns, when there are no jobs", async () => {
  for (const layout of [null, "board"]) {
    mockStorage(layout);
    mockAPI({ runs: [] });
    const { unmount } = render(<Runs />);
    expect(await screen.findByText("No jobs yet")).toBeInTheDocument();
    expect(screen.getByText(/jig def invoke/, { selector: "pre" })).toBeInTheDocument();
    expect(screen.queryAllByRole("region")).toHaveLength(0);
    unmount();
  }
});

it("reads the board from one request per poll, whatever the number of jobs", async () => {
  vi.useFakeTimers({ shouldAdvanceTime: true });
  mockStorage("board");
  mockAPI();
  render(<Runs />);
  await screen.findByRole("region", { name: /^Waiting/ });
  await act(() => vi.advanceTimersByTimeAsync(8500));
  expect(new Set(requests)).toEqual(new Set(["/api/runs"]));
  expect(requests.length).toBeLessThanOrEqual(4);
});

it("loads older jobs with the X-Next-Before cursor and keeps them", async () => {
  mockStorage();
  mockAPI({ next: "run-failed", older: [run("run-old", "accepted", "Old task", [summary("run-old", "accepted", "accepted")])] });
  render(<Runs />);
  fireEvent.click(await screen.findByRole("button", { name: "Load older jobs" }));
  expect(await screen.findByText("Old task")).toBeInTheDocument();
  expect(requests).toContain("/api/runs?before=run-failed");
  expect(screen.getByText("These are all the jobs.", { exact: false })).toBeInTheDocument();
});
