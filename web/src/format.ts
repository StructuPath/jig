// format.ts — presentation helpers. Durations are the operator's main unit
// here: a swim lane is only useful if "how long did that take" is readable at
// a glance.
import type { Run } from "./types";

export function taskTitle(run: Run): string {
  return run.parameters?.task_title ?? run.parameters?.prompt?.replace(/^Trusted instructions:\s*/, "").split("\n\nUntrusted context:")[0].trim() ?? "Previous task";
}

export function projectName(repository: string): string {
  return repository.replace(/\/$/, "").split("/").pop() || repository;
}

// taskDelivery reads the delivery a task was started with. Runs started
// before the choice existed carry no task_delivery and were all kept local.
export function taskDelivery(run: Run): "local" | "publish" {
  return run.parameters?.task_delivery === "publish" ? "publish" : "local";
}

export function taskState(run: Run): string {
  // Only a deliberately local task ends "ready" while unpublished; a publish
  // task that stopped short of its pull request keeps the run's real state.
  if (run.state === "mixed" && run.targets.length === 1 && taskDelivery(run) === "local" &&
    run.snapshot.includes('hold_when: "jig_ui_delivery != publish"')) {
    return run.parameters?.task_mode === "ask" ? "complete" : "ready";
  }
  return run.state;
}

export function parseTime(value: string | undefined | null): number | null {
  if (!value) return null;
  const parsed = Date.parse(value);
  return Number.isNaN(parsed) ? null : parsed;
}

// formatDuration renders a span the way an operator reads it: sub-second in
// milliseconds, minutes and hours once they matter.
export function formatDuration(milliseconds: number): string {
  if (!Number.isFinite(milliseconds) || milliseconds < 0) return "—";
  if (milliseconds < 1000) return `${Math.round(milliseconds)}ms`;
  const seconds = milliseconds / 1000;
  if (seconds < 60) return `${seconds.toFixed(seconds < 10 ? 1 : 0)}s`;
  const minutes = Math.floor(seconds / 60);
  const rest = Math.round(seconds - minutes * 60);
  if (minutes < 60) return `${minutes}m ${rest}s`;
  const hours = Math.floor(minutes / 60);
  return `${hours}h ${minutes - hours * 60}m`;
}

export function formatClock(value: string | undefined | null): string {
  const time = parseTime(value);
  if (time === null) return "—";
  return new Date(time).toLocaleTimeString(undefined, {
    hour: "2-digit",
    minute: "2-digit",
    second: "2-digit",
  });
}

export function shortID(value: string): string {
  return value.length > 12 ? `${value.slice(0, 8)}…` : value;
}

export function formatDateTime(value: string): string {
  const time = parseTime(value);
  return time === null ? "—" : new Date(time).toLocaleString(undefined, {
    year: "numeric", month: "short", day: "numeric", hour: "numeric", minute: "2-digit",
  });
}

const terminalAttemptStates = new Set([
  "accepted",
  "accepted_unpublished",
  "failed",
  "cancelled",
  "lost",
]);

export function isTerminalAttempt(state: string): boolean {
  return terminalAttemptStates.has(state);
}

export function stateLabel(state: string): string {
  const labels: Record<string, string> = {
    accepted: "Accepted", accepted_unpublished: "Accepted · not published",
    mixed: "Mixed results", live: "Online", stale: "Offline", active: "In progress",
    ready: "Ready to review", complete: "Complete",
  };
  return labels[state] ?? state.replace(/_/g, " ");
}
