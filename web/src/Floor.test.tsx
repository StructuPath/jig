import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { Floor } from "./Floor";
import { Tasks } from "./Tasks";
import type { Run } from "./types";

const NOW = Date.parse("2026-11-10T09:00:00Z");

function run(id: string, state: string, title: string): Run {
  return {
    id, definition_id: "definition-1", definition_generation: 1, snapshot: "name: factory\n",
    parameters: { task_title: title }, targets: [{ repository: "github.com/owner/repo", base_sha: "abc" }],
    state, created_at: "2026-11-10T08:00:00Z", updated_at: "2026-11-10T08:10:00Z",
  };
}

const stations = [
  { name: "plan", kind: "agent", model: "sonnet" }, { name: "build", kind: "agent", model: "opus" },
  { name: "classify-risk", kind: "code" },
];
const delivery = { hold_when: "risk != low", ci_wait: true };
const result = (extra: object, risk = "low") => JSON.stringify({
  phases: [
    { phase: "plan", kind: "agent", status: "success", phase_attempt: 1 },
    { phase: "build", kind: "agent", status: "success", phase_attempt: 1 },
    { phase: "classify-risk", kind: "code", status: "success", phase_attempt: 1, envelope: { risk } },
  ], ...extra,
});

const jobs: Record<string, unknown> = {
  "job-done": {
    job: { id: "job-done", repository: "github.com/owner/repo" }, run_id: "run-done", definition: "factory", stations, delivery,
    attempts: [{ id: "a-done", job_id: "job-done", attempt_number: 1, state: "accepted", created_at: "2026-11-10T08:00:00Z",
      started_at: "2026-11-10T08:00:00Z", completed_at: "2026-11-10T08:09:02Z", result: result({ publish: { state: "published" } }) }],
    publish: [
      { attempt_id: "a-done", step: "pull_request", branch: "b", pr_url: "https://github.com/owner/repo/pull/46", completed_at: "2026-11-10T08:03:00Z" },
      { attempt_id: "a-done", step: "proof", branch: "b", completed_at: "2026-11-10T08:03:10Z" },
      { attempt_id: "a-done", step: "ci", branch: "b", completed_at: "2026-11-10T08:09:00Z" },
    ],
  },
  "job-held": {
    job: { id: "job-held", repository: "github.com/owner/repo" }, run_id: "run-held", definition: "factory", stations, delivery,
    attempts: [{ id: "a-held", job_id: "job-held", attempt_number: 1, state: "accepted_unpublished", created_at: "2026-11-10T08:20:00Z",
      started_at: "2026-11-10T08:20:00Z", completed_at: "2026-11-10T08:23:27Z",
      result: result({ publish: { state: "held", code: "publish_held" }, publish_hold: "held (risk = high)" }, "high") }],
    publish: [],
  },
  "job-local": {
    job: { id: "job-local", repository: "github.com/owner/repo" }, run_id: "run-local", definition: "jig-task-build-abc", stations,
    delivery: { hold_when: "jig_ui_delivery != publish", ci_wait: false },
    attempts: [{ id: "a-local", job_id: "job-local", attempt_number: 1, state: "accepted_unpublished", created_at: "2026-11-10T08:30:00Z",
      started_at: "2026-11-10T08:30:00Z", completed_at: "2026-11-10T08:34:00Z",
      result: result({ publish: { state: "held", code: "publish_held" }, publish_hold: 'publish.hold_when "jig_ui_delivery != publish" held (jig_ui_delivery = local)' }) }],
    publish: [],
  },
  "job-0": {
    job: { id: "job-0", repository: "github.com/owner/repo" }, run_id: "run-0",
    attempts: [{ id: "a-0", result: JSON.stringify({ phases: [{ phase: "plan", status: "success", envelope: { summary: "Segment graphemes" } }] }) }],
    publish: [{ attempt_id: "a-0", step: "push", branch: "jig/job-0/1" }],
  },
};

let requests: string[] = [];
let posted: unknown = null;
let refuse = false;
let stored = new Map<string, string>();

beforeEach(() => {
  requests = [];
  posted = null;
  refuse = false;
  stored = new Map();
  window.history.replaceState({}, "", "/");
  vi.stubGlobal("localStorage", {
    getItem: (key: string) => stored.get(key) ?? null,
    setItem: (key: string, value: string) => { stored.set(key, value); },
    removeItem: (key: string) => { stored.delete(key); },
  });
  vi.stubGlobal("fetch", vi.fn(async (url: string, init?: RequestInit) => {
    requests.push(url);
    const json = (body: unknown) => new Response(JSON.stringify(body), { status: 200, headers: { "Content-Type": "application/json" } });
    if (url === "/api/runs" && init?.method === "POST") {
      posted = JSON.parse(init.body as string);
      if (refuse) return new Response(JSON.stringify({ error: { code: "unresolvable_ref", message: `cannot resolve "HEAD" in "file:///Users/v/dev/new": fatal: '/Users/v/dev/new' does not appear to be a git repository` } }), { status: 400, headers: { "Content-Type": "application/json" } });
      return json({ run: { id: "run-new" }, jobs: [] });
    }
    if (url === "/api/runs") return json([run("run-done", "accepted", "Hint when no control plane answers")]);
    if (url.startsWith("/api/runs/")) { const id = url.split("/").pop() ?? ""; return json({ run: run(id, "accepted", "x"), jobs: id === "run-empty" ? [] : [{ id: id.replace("run-", "job-") }] }); }
    if (url.startsWith("/api/jobs/")) return json(jobs[url.split("/").pop() ?? ""] ?? {});
    if (url === "/api/workers") return json({ workers: [{ name: "local", live: true, capacity: 1, active_count: 0, runtimes: [{ name: "claude-code" }] }] });
    if (url === "/api/definitions") return json([
      { id: "d-factory", name: "factory", source: "name: factory\nruntime: claude-code\n" },
      { id: "d-codex", name: "codex-only", source: "name: codex-only\nruntime: codex\n" },
      { id: "d-starter", name: "jig-task-ask-abc", source: "name: jig-task-ask-abc\nruntime: codex\n" },
    ]);
    return json({});
  }));
});

afterEach(() => vi.unstubAllGlobals());

it("lights a finished job's row from its result, without reading its trace", async () => {
  render(<Floor runs={[run("run-done", "accepted", "Hint when no control plane answers")]} nowMs={NOW} />);
  // The row's cells are replaced as its job detail arrives, so the row is
  // found through the verdict that only the finished detail produces.
  const row = within((await screen.findByText("Accepted")).closest("tr") as HTMLElement);
  expect(row.getByText("Hint when no control plane answers")).toBeVisible();
  // Three phases plus publish and CI, all proven.
  expect(row.getByRole("img", { name: "5 of 5 stations passed" })).toBeVisible();
  expect(row.getByText("9m 2s")).toBeVisible();
  expect(row.getByRole("link", { name: "PR #46" })).toHaveAttribute("href", "https://github.com/owner/repo/pull/46");
  expect(requests.some((url) => url.includes("/events"))).toBe(false);
});

it("shows a held job as held on its row", async () => {
  render(<Floor runs={[run("run-held", "mixed", "CI concurrency group")]} nowMs={NOW} />);
  const row = within((await screen.findByText("Held")).closest("tr") as HTMLElement);
  expect(row.getByText("CI concurrency group")).toBeVisible();
  expect(row.getByRole("img", { name: "2 of 5 stations passed" })).toBeVisible();
  expect(row.getByText("held")).toBeVisible();
});

it("keeps a row lit while its run changes state", async () => {
  const view = render(<Floor runs={[run("run-done", "active", "Hint when no control plane answers")]} nowMs={NOW} />);
  expect(await screen.findByText("Accepted")).toBeVisible();
  // The run list's next poll moves the run on; the row must not drop to
  // unlit while it reads the job again.
  view.rerender(<Floor runs={[run("run-done", "accepted", "Hint when no control plane answers")]} nowMs={NOW} />);
  expect(screen.getByText("Accepted")).toBeVisible();
  expect(screen.getByRole("img", { name: "5 of 5 stations passed" })).toBeVisible();
});

it("labels a job the form kept local as kept local, not held", async () => {
  render(<Floor runs={[run("run-local", "accepted", "Local change")]} nowMs={NOW} />);
  const row = within((await screen.findByText("Ready to review")).closest("tr") as HTMLElement);
  expect(row.getByText("kept local")).toBeVisible();
  expect(row.queryByText("held")).toBeNull();
});

it("keeps a row readable when the run has no job yet", async () => {
  render(<Floor runs={[run("run-empty", "active", "Queued work")]} nowMs={NOW} />);
  const row = within((await screen.findByText("Queued work")).closest("tr") as HTMLElement);
  await waitFor(() => expect(requests).toContain("/api/runs/run-empty"));
  expect(row.getByText("active")).toBeVisible();
});

it("starts a saved workflow as itself, on the runtime it pins", async () => {
  render(<Tasks />);
  const saved = await screen.findByRole("radio", { name: /Run a saved workflow/ });
  await waitFor(() => expect(saved).toBeEnabled());
  fireEvent.click(saved);
  // The task form's own starters are not offered as workflows.
  const select = screen.getByLabelText("Workflow");
  expect(within(select).getAllByRole("option").map((option) => option.textContent)).toEqual(["factory", "codex-only"]);
  fireEvent.change(screen.getByLabelText("1. Project"), { target: { value: "github.com/owner/repo" } });
  fireEvent.change(screen.getByLabelText("3. Instructions"), { target: { value: "Add a hint" } });
  const start = screen.getByRole("button", { name: "Start job" });
  await waitFor(() => expect(start).toBeEnabled());

  // A workflow pinned to a runtime no online worker runs still starts: the
  // server queues it, and the note says it waits for that runtime.
  fireEvent.change(select, { target: { value: "d-codex" } });
  expect(start).toBeEnabled();
  expect(screen.getByText("No online worker runs codex — the job waits in the queue until one does.")).toBeVisible();

  fireEvent.change(select, { target: { value: "d-factory" } });
  fireEvent.click(start);
  await waitFor(() => expect(window.location.pathname).toBe("/runs/run-new"));
  expect(posted).toEqual({
    definition_id: "d-factory", instructions: "Add a hint",
    parameters: { task_title: "Add a hint" }, targets: [{ repository: "github.com/owner/repo" }],
  });
});

async function fillSavedWorkflow(project: string, instructions: string) {
  const saved = await screen.findByRole("radio", { name: /Run a saved workflow/ });
  await waitFor(() => expect(saved).toBeEnabled());
  fireEvent.click(saved);
  fireEvent.change(screen.getByLabelText("1. Project"), { target: { value: project } });
  fireEvent.change(screen.getByLabelText("3. Instructions"), { target: { value: instructions } });
  const start = screen.getByRole("button", { name: "Start job" });
  await waitFor(() => expect(start).toBeEnabled());
  return start;
}

it("passes a continuation to a saved workflow: earlier result as context, earlier branch as base", async () => {
  window.history.replaceState({}, "", "/?continue=run-0");
  render(<Tasks />);
  expect(await screen.findByText(/Starts from the earlier job's branch jig\/job-0\/1/)).toBeVisible();
  fireEvent.click(await fillSavedWorkflow("github.com/owner/repo", "Keep going"));
  await waitFor(() => expect(window.location.pathname).toBe("/runs/run-new"));
  expect(posted).toMatchObject({
    definition_id: "d-factory", instructions: "Keep going",
    parameters: { task_title: "Keep going", task_continues: "run-0" },
    targets: [{ repository: "github.com/owner/repo", ref: "jig/job-0/1" }],
  });
  const context = (posted as { context: { label: string; body: string }[] }).context;
  expect(context.map((section) => section.label)).toEqual(["Previous task", "Previous result"]);
  expect(context[1].body).toContain("Segment graphemes");
});

it("explains a refused start in plain words, with the commands, and keeps the raw error", async () => {
  refuse = true;
  render(<Tasks />);
  fireEvent.click(await fillSavedWorkflow("/Users/v/dev/new", "Build the first page"));
  expect(await screen.findByText("This folder is not a Git project yet")).toBeVisible();
  expect(screen.getByText(/git init -b main/)).toBeVisible();
  expect(screen.getByText(/cd \/Users\/v\/dev\/new/)).toBeVisible();
  // The control plane's own code and words stay on the page.
  expect(screen.getByText("unresolvable_ref")).toBeVisible();
  expect(screen.getByLabelText("3. Instructions")).toHaveValue("Build the first page");
  expect(window.location.pathname).toBe("/");
});

it("keeps the form through a refresh, and drops the draft once the job starts", async () => {
  const first = render(<Tasks />);
  await fillSavedWorkflow("github.com/owner/repo", "Build the first page");
  first.unmount();

  // A refresh: a new form, the same browser storage.
  render(<Tasks />);
  expect(screen.getByLabelText("1. Project")).toHaveValue("github.com/owner/repo");
  expect(await screen.findByLabelText("3. Instructions")).toHaveValue("Build the first page");
  const saved = screen.getByRole("radio", { name: /Run a saved workflow/ });
  await waitFor(() => expect(saved).toBeChecked());

  const start = screen.getByRole("button", { name: "Start job" });
  await waitFor(() => expect(start).toBeEnabled());
  fireEvent.click(start);
  await waitFor(() => expect(window.location.pathname).toBe("/runs/run-new"));
  expect(stored.has("jig-draft")).toBe(false);
  expect(stored.get("jig-project")).toBe("github.com/owner/repo");
});

it("lets a link that names a project win over a draft", async () => {
  stored.set("jig-draft", JSON.stringify({ repository: "/old/project", task: "Old text", mode: "build" }));
  window.history.replaceState({}, "", "/?project=%2Fnew%2Fproject&mode=ask");
  render(<Tasks />);
  expect(screen.getByLabelText("1. Project")).toHaveValue("/new/project");
  expect(screen.getByLabelText("3. What should Codex do?")).toHaveValue("");
  expect(screen.getByRole("radio", { name: /Get an answer/ })).toBeChecked();
});
