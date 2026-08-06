// format.ts — presentation helpers. Durations are the operator's main unit
// here: a swim lane is only useful if "how long did that take" is readable at
// a glance.

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
  return state.replace(/_/g, " ");
}
