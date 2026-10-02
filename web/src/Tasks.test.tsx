import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { webcrypto } from "node:crypto";
import { execFileSync } from "node:child_process";
import { mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { beforeEach, expect, it, vi } from "vitest";
import { Tasks } from "./Tasks";
import { starterSource } from "./starters";

beforeEach(() => {
  vi.stubGlobal("crypto", webcrypto);
  vi.stubGlobal("localStorage", { getItem: () => null, setItem: vi.fn() });
});

it("generates Codex starters accepted by Jig's actual definition validator", async () => {
  const directory = mkdtempSync(join(tmpdir(), "jig-starters-"));
  try {
    for (const mode of ["ask", "build", "auto"] as const) {
      const source = await starterSource(mode, "gpt-6.1-sol", "printf '$&'; go test ./...");
      const path = join(directory, `${mode}.yaml`);
      writeFileSync(path, source);
      expect(execFileSync("go", ["run", "./cmd/jig", "def", "validate", path], { cwd: "..", encoding: "utf8" })).toMatch(/^ok /);
      expect(source).toContain('hold_when: "jig_ui_delivery != publish"');
      expect(source).not.toMatch(/model: (haiku|sonnet|opus)/);
      if (mode !== "ask") { expect(source).toContain("printf '$&'; go test ./..."); expect(source).toContain("run: plan, then: rerun-chain"); }
    }
  } finally {
    rmSync(directory, { recursive: true });
  }
}, 30000);

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
    parameters: { task_title: "Explain this project", task_mode: "ask" }, targets: [{ repository: "/tmp/project" }],
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
