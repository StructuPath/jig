// keyboard.test.tsx — the keyboard layer: the binding table, the palette's
// ranking, the URL-held list state, and the flows end to end — `?` opens an
// accessible shortcuts dialog that traps focus and closes on Escape, `/`
// focuses the filter, `b` switches the layout, `g j` navigates, and
// Cmd/Ctrl-K jumps to a job by ID prefix or title.
import { act, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { App } from "./App";
import { isTyping, paletteItems, paletteMatches, shortcutFor } from "./keyboard";
import { layoutOf, listQuerySearch, parseListQuery } from "./listQuery";
import type { RunListItem } from "./types";

function run(id: string, title: string, jobID: string, state = "accepted"): RunListItem {
  return {
    id, definition_id: "d", definition_generation: 1, snapshot: "name: factory\n",
    parameters: { task_title: title }, targets: [{ repository: "/Users/v/dev/jig", base_sha: "abc" }],
    state, created_at: "2026-11-10T08:00:00Z", updated_at: "2026-11-10T08:00:00Z",
    job_summaries: [{
      job: { id: jobID, run_id: id, repository: "/Users/v/dev/jig", base_sha: "abc", state, cancellation_requested: false, created_at: "2026-11-10T08:00:00Z", updated_at: "2026-11-10T08:00:00Z" },
      attempt_count: 1, latest_attempt: { id: `a-${id}`, job_id: jobID, attempt_number: 1, state, created_at: "2026-11-10T08:00:00Z" },
      phases: [], publish: [],
    }],
  };
}

const RUNS = [
  run("run-aaa", "Fix the flaky retry test", "job-7f3a91"),
  run("run-bbb", "Add a palette", "job-2c44e0", "failed"),
  run("run-ccc", "Retry publication on CI red", "job-9d0b11"),
];

describe("shortcutFor", () => {
  const field = document.createElement("input");
  const page = document.createElement("div");

  it("maps single keys outside fields and ignores them while typing", () => {
    expect(shortcutFor({ key: "/", target: page }, null)).toEqual({ kind: "focus-filter" });
    expect(shortcutFor({ key: "?", target: page }, null)).toEqual({ kind: "help" });
    expect(shortcutFor({ key: "b", target: page }, null)).toEqual({ kind: "toggle-layout" });
    expect(shortcutFor({ key: "g", target: page }, null)).toEqual({ kind: "prefix", key: "g" });
    for (const key of ["/", "?", "b", "g"]) expect(shortcutFor({ key, target: field }, null)).toBeNull();
    expect(shortcutFor({ key: "x", target: page }, null)).toBeNull();
    // A modifier makes it someone else's shortcut (Cmd-B, Ctrl-/).
    expect(shortcutFor({ key: "b", metaKey: true, target: page }, null)).toBeNull();
  });

  it("opens the palette with Cmd-K or Ctrl-K, even from a field", () => {
    expect(shortcutFor({ key: "k", metaKey: true, target: field }, null)).toEqual({ kind: "palette" });
    expect(shortcutFor({ key: "K", ctrlKey: true, target: page }, null)).toEqual({ kind: "palette" });
  });

  it("completes the g sequences and drops an unknown second key", () => {
    expect(shortcutFor({ key: "f", target: page }, "g")).toEqual({ kind: "go", path: "/" });
    expect(shortcutFor({ key: "j", target: page }, "g")).toEqual({ kind: "go", path: "/runs" });
    expect(shortcutFor({ key: "w", target: page }, "g")).toEqual({ kind: "go", path: "/fleet" });
    expect(shortcutFor({ key: "x", target: page }, "g")).toBeNull();
  });

  it("knows a field when it sees one", () => {
    const area = document.createElement("textarea");
    const inside = document.createElement("span");
    const editable = document.createElement("div");
    editable.setAttribute("contenteditable", "true");
    editable.appendChild(inside);
    expect([isTyping(field), isTyping(area), isTyping(inside), isTyping(page), isTyping(null)]).toEqual([true, true, true, false, false]);
  });
});

describe("paletteMatches", () => {
  const items = paletteItems(RUNS);

  it("puts a job ID prefix first, then titles that start with the query, then contain it", () => {
    expect(paletteMatches(items, "job-7f")[0].href).toBe("/jobs/job-7f3a91");
    expect(paletteMatches(items, "run-bbb")[0].href).toBe("/jobs/job-2c44e0");
    expect(paletteMatches(items, "retry").map((item) => item.label)).toEqual(["Retry publication on CI red", "Fix the flaky retry test"]);
  });

  it("finds views by name, and lists views then recent jobs for an empty query", () => {
    expect(paletteMatches(items, "work")[0]).toMatchObject({ label: "Workers", href: "/fleet" });
    const empty = paletteMatches(items, "");
    expect(empty[0].label).toBe("Floor");
    expect(empty.some((item) => item.href === "/jobs/job-7f3a91")).toBe(true);
    expect(paletteMatches(items, "zzz")).toEqual([]);
  });

  it("names each job's status and project in its detail line", () => {
    expect(items.find((item) => item.id === "job-2c44e0")?.detail).toBe("jig · Stopped · job-2c44e0");
  });
});

describe("list query", () => {
  it("round-trips the filters and layout and drops what it does not know", () => {
    const query = parseListQuery("?status=stopped&repo=%2FUsers%2Fv%2Fdev%2Fjig&q=flaky%20test&view=board");
    expect(query).toEqual({ status: "stopped", repo: "/Users/v/dev/jig", q: "flaky test", view: "board" });
    expect(parseListQuery(listQuerySearch(query))).toEqual(query);
    expect(parseListQuery("?status=bogus&view=grid")).toEqual({ status: null, repo: "", q: "", view: null });
    expect(listQuerySearch({ status: null, repo: "", q: "", view: null })).toBe("");
  });

  it("uses the stored layout only when the URL names none", () => {
    vi.stubGlobal("localStorage", { getItem: () => "board", setItem: () => undefined });
    expect(layoutOf(parseListQuery(""))).toBe("board");
    expect(layoutOf(parseListQuery("?view=list"))).toBe("list");
    vi.unstubAllGlobals();
  });
});

describe("the keyboard layer in the app", () => {
  let stored: Map<string, string>;
  beforeEach(() => {
    stored = new Map();
    vi.stubGlobal("localStorage", {
      getItem: (key: string) => stored.get(key) ?? null,
      setItem: (key: string, value: string) => { stored.set(key, value); },
      removeItem: (key: string) => { stored.delete(key); },
    });
    vi.stubGlobal("fetch", vi.fn(async (url: string) => {
      const json = (body: unknown) => new Response(JSON.stringify(body), { status: 200, headers: { "Content-Type": "application/json" } });
      if (url === "/api/runs") return json(RUNS);
      if (url === "/api/workers") return json({ workers: [], live_count: 0, stale_count: 0, available_slots: 0, observed_at: "2026-11-10T09:00:00Z" });
      if (url === "/api/queue") return json({ depth: 0, active_count: 0, entries: [], observed_at: "2026-11-10T09:00:00Z" });
      if (url.startsWith("/api/report")) return json({ jobs: { total: 0, accepted: 0 }, ci: { waited: 0, first_pass_green: 0 }, spend: { total_usd: 0, per_clean_accept_usd: null }, publish: { held: 0 } });
      return json({});
    }));
    window.history.replaceState({}, "", "/runs");
  });
  afterEach(() => vi.unstubAllGlobals());

  const press = (key: string, init: KeyboardEventInit = {}, target: EventTarget = document.body) =>
    act(() => { target.dispatchEvent(new KeyboardEvent("keydown", { key, bubbles: true, cancelable: true, ...init })); });

  it("opens the shortcuts on ?, traps focus inside, and closes on Escape", async () => {
    render(<App />);
    await screen.findByText("Fix the flaky retry test");
    const before = document.activeElement;
    press("?");
    const dialog = screen.getByRole("dialog", { name: "Keyboard shortcuts" });
    expect(dialog).toHaveAttribute("aria-modal", "true");
    const close = screen.getByRole("button", { name: "Close" });
    expect(close).toHaveFocus();
    // Close is the only control: Tab and Shift-Tab stay on it.
    fireEvent.keyDown(close, { key: "Tab" });
    expect(close).toHaveFocus();
    fireEvent.keyDown(close, { key: "Tab", shiftKey: true });
    expect(close).toHaveFocus();
    fireEvent.keyDown(close, { key: "Escape" });
    expect(screen.queryByRole("dialog")).toBeNull();
    expect(document.activeElement).toBe(before);
  });

  it("focuses the filter on /, and then / types a slash", async () => {
    render(<App />);
    await screen.findByText("Fix the flaky retry test");
    press("/");
    const filter = screen.getByRole("searchbox");
    expect(filter).toHaveFocus();
    press("b", {}, filter);
    expect(window.location.search).toBe("");
  });

  it("switches the layout on b, in the URL", async () => {
    render(<App />);
    await screen.findByText("Fix the flaky retry test");
    press("b");
    expect(window.location.search).toBe("?view=board");
    expect(await screen.findByRole("region", { name: /^Waiting/ })).toBeInTheDocument();
    press("b");
    expect(window.location.search).toBe("?view=list");
  });

  it("goes to Workers on g w and to Jobs on g j", async () => {
    render(<App />);
    await screen.findByText("Fix the flaky retry test");
    press("g");
    press("w");
    expect(window.location.pathname).toBe("/fleet");
    expect(await screen.findByText("No workers registered")).toBeInTheDocument();
    press("g");
    press("j");
    expect(window.location.pathname).toBe("/runs");
  });

  it("jumps to a job from the palette by ID prefix, with arrows and Enter", async () => {
    render(<App />);
    await screen.findByText("Fix the flaky retry test");
    press("k", { metaKey: true });
    const input = screen.getByRole("combobox", { name: "Job ID, title or view" });
    expect(input).toHaveFocus();
    const options = () => within(screen.getByRole("listbox", { name: "Matches" })).getAllByRole("option");
    await waitFor(() => expect(options().length).toBeGreaterThan(0));
    fireEvent.change(input, { target: { value: "retry" } });
    expect(options().map((option) => option.textContent)).toEqual([
      "Retry publication on CI redjig · Accepted · job-9d0b11", "Fix the flaky retry testjig · Accepted · job-7f3a91",
    ]);
    fireEvent.keyDown(input, { key: "ArrowDown" });
    expect(options()[1]).toHaveAttribute("aria-selected", "true");
    fireEvent.keyDown(input, { key: "Enter" });
    expect(window.location.pathname).toBe("/jobs/job-7f3a91");
    expect(screen.queryByRole("dialog")).toBeNull();
  });
});
