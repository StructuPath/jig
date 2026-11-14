import { describe, expect, it } from "vitest";
import { formatAge, formatDuration, runtimeLabel, taskHeading, workflowName } from "./format";
import { automaticGoal } from "./starters";
import type { Run } from "./types";

function run(overrides: Partial<Run> = {}): Run {
  return {
    id: "run-1",
    definition_id: "definition-1",
    definition_generation: 1,
    snapshot: "name: factory\nroster: {}\n",
    targets: [{ repository: "github.com/owner/repo" }] as Run["targets"],
    state: "active",
    created_at: "2026-11-10T00:00:00Z",
    updated_at: "2026-11-10T00:00:00Z",
    ...overrides,
  };
}

describe("workflowName", () => {
  it("reads the name from a snapshot that opens with comments", () => {
    expect(workflowName(run({ snapshot: "# factory.yaml — header\n#   name: not-this\nname: factory\nruntime: claude-code\n" }))).toBe("factory");
  });

  it("strips quotes and ignores an indented role or phase name", () => {
    expect(workflowName(run({ snapshot: "roster:\n  builder:\n    name: nested\nname: \"my-flow\"  \n" }))).toBe("my-flow");
  });

  it("is null when the snapshot has no top-level name", () => {
    expect(workflowName(run({ snapshot: "phases:\n  - name: build\n" }))).toBeNull();
  });
});

describe("taskHeading", () => {
  it("is the first line of the instructions", () => {
    expect(taskHeading(run({ parameters: { prompt: "> Fix the cursor\n\nDetails follow." } }))).toBe("Fix the cursor");
  });

  it("names the agent-led goal instead of printing it", () => {
    expect(taskHeading(run({ parameters: { task_title: automaticGoal } }))).toBe("Agent-led improvement");
  });
});

describe("runtimeLabel", () => {
  it("names each runtime and credits no CLI when none is recorded", () => {
    expect(runtimeLabel("codex")).toBe("Codex");
    expect(runtimeLabel("claude-code")).toBe("Claude Code");
    expect(runtimeLabel(undefined)).toBe("The agent");
    expect(runtimeLabel("pi")).toBe("The agent");
  });
});

describe("formatDuration", () => {
  it("switches to days past 48 hours", () => {
    expect(formatDuration(47 * 3_600_000 + 5 * 60_000)).toBe("47h 5m");
    expect(formatDuration((2365 * 60 + 28) * 60_000)).toBe("98d 13h");
  });
});

describe("formatAge", () => {
  const now = Date.parse("2026-11-14T12:00:00Z");
  it("reads an old item in days, not as a clock time", () => {
    expect(formatAge("2026-11-14T11:59:30Z", now)).toBe("just now");
    expect(formatAge("2026-11-14T11:15:00Z", now)).toBe("45 min ago");
    expect(formatAge("2026-11-13T09:00:00Z", now)).toBe("27 h ago");
    expect(formatAge("2026-11-11T09:00:00Z", now)).toBe("3 d ago");
    expect(formatAge(null, now)).toBe("—");
  });
});
