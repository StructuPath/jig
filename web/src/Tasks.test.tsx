import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { webcrypto } from "node:crypto";
import { execFileSync } from "node:child_process";
import { mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { afterAll, beforeAll, beforeEach, expect, it, vi } from "vitest";
import { Tasks } from "./Tasks";
import { sourceHash, starterSource } from "./starters";

beforeEach(() => {
  vi.stubGlobal("crypto", webcrypto);
  vi.stubGlobal("localStorage", { getItem: () => null, setItem: vi.fn() });
});

// Build jig once; `go run` per starter recompiles each time and outlasts the
// test timeout on a cold CI cache.
const binDirectory = mkdtempSync(join(tmpdir(), "jig-bin-"));
const jig = join(binDirectory, "jig");
beforeAll(() => {
  execFileSync("go", ["build", "-o", jig, "./cmd/jig"], { cwd: ".." });
}, 180000);
afterAll(() => rmSync(binDirectory, { recursive: true, force: true }));

it("generates Codex starters accepted by Jig's actual definition validator", async () => {
  const directory = mkdtempSync(join(tmpdir(), "jig-starters-"));
  try {
    for (const [mode, delivery] of [["ask", "local"], ["build", "local"], ["auto", "local"], ["build", "publish"], ["auto", "publish"]] as const) {
      const source = await starterSource(mode, "gpt-6.1-sol", "printf '$&'; go test ./...", delivery);
      const path = join(directory, `${mode}-${delivery}.yaml`);
      writeFileSync(path, source);
      expect(execFileSync(jig, ["def", "validate", path], { cwd: "..", encoding: "utf8" })).toMatch(/^ok /);
      expect(source).toContain('hold_when: "jig_ui_delivery != publish"');
      expect(source).toContain(delivery === "publish" ? '{\\\"jig_ui_delivery\\\":\\\"publish\\\"}' : '{\\\"jig_ui_delivery\\\":\\\"local\\\"}');
      expect(source).toMatch(/^runtime: codex$/m);
      expect(source).not.toMatch(/model: (haiku|sonnet|opus)/);
      if (mode !== "ask") { expect(source).toContain("printf '$&'; go test ./..."); expect(source).toContain("run: plan, then: rerun-chain"); }
    }
  } finally {
    rmSync(directory, { recursive: true });
  }
}, 30000);

it("names each starter by a hash of its final source, runtime pin included", async () => {
  for (const mode of ["ask", "build", "auto"] as const) {
    const source = await starterSource(mode, "gpt-6.1-sol", "go test ./...");
    const name = source.match(/^name: jig-task-\w+-([0-9a-f]{16})$/m);
    expect(name).not.toBeNull();
    const hashed = source.replace(/^name: .+$/m, `name: ${mode === "ask" ? "smoke" : "plan-build-test"}`);
    expect(hashed).toMatch(/^runtime: codex$/m);
    expect(await sourceHash(hashed)).toBe(name![1]);
    // A starter saved before the pin hashed the same source without it; its
    // name must not collide with the pinned one (definition_exists).
    expect(await sourceHash(hashed.replace("runtime: codex\n", ""))).not.toBe(name![1]);
  }
});

function mockServer({ online = true, fail = false } = {}) {
  const fetch = vi.fn(async (url: string, init?: RequestInit) => {
    const json = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });
    if (url === "/api/workers") return json({ workers: online ? [{ live: true, runtimes: [{ name: "codex" }] }] : [] });
    if (url === "/api/definitions" && init?.method === "POST") return json({ id: "definition-1" });
    if (url === "/api/definitions") return json([]);
    if (url === "/api/runs" && init?.method === "POST") return fail ? json({ error: { code: "invalid_repository", message: "Project folder was not found." } }, 400) : json({ run: { id: "run-1" }, jobs: [] });
    return json([]);
  });
  vi.stubGlobal("fetch", fetch);
  return fetch;
}

it("starts a task from the form and opens its progress without terminal steps", async () => {
  const fetch = mockServer();
  render(<Tasks />);
  fireEvent.click(screen.getByRole("radio", { name: /Get an answer/ }));
  fireEvent.change(screen.getByLabelText("1. Project"), { target: { value: "/tmp/project" } });
  fireEvent.change(screen.getByLabelText("3. What should Codex do?"), { target: { value: "Explain this project" } });
  const start = screen.getByRole("button", { name: "Start task" });
  await waitFor(() => expect(start).toBeEnabled());
  fireEvent.click(start);
  await waitFor(() => expect(window.location.pathname).toBe("/runs/run-1"));
  const invocation = fetch.mock.calls.find(([url, init]) => url === "/api/runs" && init?.method === "POST");
  expect(JSON.parse(invocation?.[1]?.body as string)).toEqual({
    definition_id: "definition-1", instructions: "Explain this project",
    parameters: { task_title: "Explain this project", task_mode: "ask", task_delivery: "local" }, targets: [{ repository: "/tmp/project" }],
  });
});

it("keeps the task text when admission fails", async () => {
  mockServer({ fail: true });
  render(<Tasks />);
  fireEvent.click(screen.getByRole("radio", { name: /Get an answer/ }));
  fireEvent.change(screen.getByLabelText("1. Project"), { target: { value: "/missing" } });
  fireEvent.change(screen.getByLabelText("3. What should Codex do?"), { target: { value: "My task" } });
  await waitFor(() => expect(screen.getByRole("button", { name: "Start task" })).toBeEnabled());
  fireEvent.click(screen.getByRole("button", { name: "Start task" }));
  expect(await screen.findByRole("alert")).toHaveTextContent("Project folder was not found");
  expect(screen.getByLabelText("3. What should Codex do?")).toHaveValue("My task");
  expect(window.location.pathname).toBe("/");
});

it("lets agents choose the directive without making the user fill the task box", async () => {
  const fetch = mockServer();
  render(<Tasks />);
  fireEvent.change(screen.getByLabelText("1. Project"), { target: { value: "/tmp/project" } });
  const start = screen.getByRole("button", { name: "Start task" });
  await waitFor(() => expect(start).toBeEnabled());
  fireEvent.click(start);
  await waitFor(() => expect(window.location.pathname).toBe("/runs/run-1"));
  const call = fetch.mock.calls.find(([url, init]) => url === "/api/runs" && init?.method === "POST");
  const body = JSON.parse(call?.[1]?.body as string);
  expect(body.parameters.task_mode).toBe("auto");
  expect(body.instructions).toContain("choose one useful improvement");
});

it("does not submit a task when no Codex worker is online", async () => {
  mockServer({ online: false });
  render(<Tasks />);
  expect(await screen.findByText("Codex is offline. Check Workers before starting.")).toBeVisible();
  expect(screen.getByRole("button", { name: "Start task" })).toBeDisabled();
});

it("opens a follow-up with the same project and an explicit Make changes choice", async () => {
  mockServer();
  window.history.replaceState({}, "", "/?project=github.com%2Fexample%2Fproject&mode=build");
  render(<Tasks />);
  expect(screen.getByLabelText("1. Project")).toHaveValue("github.com/example/project");
  expect(screen.getByRole("radio", { name: /Make changes/ })).toBeChecked();
  expect(screen.getByText("This task will change files in a separate working folder.")).toBeVisible();
});

it("never lets an ask task publish, whatever delivery is requested", async () => {
  const source = await starterSource("ask", "gpt-6.1-sol", "", "publish");
  expect(source).toContain('{\\\"jig_ui_delivery\\\":\\\"local\\\"}');
  expect(source).toContain("name: keep-local");
});

it("offers a pull request only for a GitHub project", async () => {
  mockServer();
  window.history.replaceState({}, "", "/?mode=build");
  render(<Tasks />);
  const publish = screen.getByRole("radio", { name: /Open a pull request/ });
  fireEvent.change(screen.getByLabelText("1. Project"), { target: { value: "/tmp/project" } });
  expect(publish).toBeDisabled();
  expect(screen.getByRole("radio", { name: /Keep it local/ })).toBeChecked();
  fireEvent.change(screen.getByLabelText("1. Project"), { target: { value: "github.com/example/project" } });
  expect(publish).toBeEnabled();
});

it("starts a pull-request task whose definition releases the publish hold", async () => {
  const fetch = mockServer();
  window.history.replaceState({}, "", "/?project=github.com%2Fexample%2Fproject&mode=build");
  render(<Tasks />);
  fireEvent.click(screen.getByRole("radio", { name: /Open a pull request/ }));
  fireEvent.change(screen.getByLabelText("3. What should Codex do?"), { target: { value: "Fix the bug" } });
  const start = screen.getByRole("button", { name: "Start task" });
  await waitFor(() => expect(start).toBeEnabled());
  fireEvent.click(start);
  await waitFor(() => expect(window.location.pathname).toBe("/runs/run-1"));
  const definition = fetch.mock.calls.find(([url, init]) => url === "/api/definitions" && init?.method === "POST");
  expect(JSON.parse(definition?.[1]?.body as string).source).toContain('{\\\"jig_ui_delivery\\\":\\\"publish\\\"}');
  const invocation = fetch.mock.calls.find(([url, init]) => url === "/api/runs" && init?.method === "POST");
  expect(JSON.parse(invocation?.[1]?.body as string).parameters.task_delivery).toBe("publish");
});

it("continues an earlier task with its result as context and its pushed branch as base", async () => {
  const fetch = mockServer();
  const base = fetch.getMockImplementation()!;
  fetch.mockImplementation(async (url: string, init?: RequestInit) => {
    const json = (body: unknown) => new Response(JSON.stringify(body), { status: 200, headers: { "Content-Type": "application/json" } });
    if (url === "/api/runs/run-0") return json({ run: { id: "run-0", parameters: { task_title: "Fix Unicode backspace", task_delivery: "publish" }, targets: [], state: "accepted", snapshot: "" }, jobs: [{ id: "job-0" }] });
    if (url === "/api/jobs/job-0") return json({
      job: { id: "job-0", repository: "github.com/example/project" }, run_id: "run-0",
      attempts: [{ id: "a", result: JSON.stringify({ phases: [{ phase: "plan", status: "success", envelope: { summary: "Segment graphemes", notes_for_next_agent: "Edit renderer.mjs" } }], changed_paths: ["bin/renderer.mjs"] }) }],
      publish: [{ attempt_id: "a", step: "push", branch: "jig/job-0/1" }],
    });
    return base(url, init);
  });
  window.history.replaceState({}, "", "/?mode=build&continue=run-0");
  render(<Tasks />);
  expect(await screen.findByText(/Starts from the earlier task's branch jig\/job-0\/1/)).toBeVisible();
  expect(screen.getByLabelText("1. Project")).toHaveValue("github.com/example/project");
  fireEvent.change(screen.getByLabelText("3. What should Codex do?"), { target: { value: "Reviewed. Let's keep going." } });
  const start = screen.getByRole("button", { name: "Start task" });
  await waitFor(() => expect(start).toBeEnabled());
  fireEvent.click(start);
  await waitFor(() => expect(window.location.pathname).toBe("/runs/run-1"));
  const invocation = fetch.mock.calls.find(([url, init]) => url === "/api/runs" && init?.method === "POST");
  const body = JSON.parse(invocation?.[1]?.body as string);
  expect(body.targets).toEqual([{ repository: "github.com/example/project", ref: "jig/job-0/1" }]);
  expect(body.parameters.task_continues).toBe("run-0");
  expect(body.context[0]).toEqual({ label: "Previous task", body: "Fix Unicode backspace" });
  expect(body.context[1].body).toContain("Segment graphemes");
  expect(body.context[1].body).toContain("Edit renderer.mjs");
  expect(body.context[1].body).toContain("bin/renderer.mjs");
});
