// api.ts — the one place the UI talks to the control plane. Same-origin
// fetches, so state-changing requests pass the server's Origin check without
// carrying any token (R20); errors surface the control plane's own stable
// code, because "job_at_worktree_cap" is worth showing and "500" is not.
import type {
  Attempt,
  AttemptSummary,
  EnvelopeRecord,
  EventPage,
  FleetView,
  GateEvidence,
  JobDetail,
  QueueView,
  Run,
  RunView,
  WorktreeLedgerEntry,
  APIErrorBody,
} from "./types";

export class APIError extends Error {
  constructor(
    public code: string,
    message: string,
    public status: number,
  ) {
    super(message);
    this.name = "APIError";
  }
}

async function request<T>(path: string, init?: RequestInit): Promise<T> {
  const response = await fetch(path, {
    ...init,
    headers: init?.body ? { "Content-Type": "application/json", ...init.headers } : init?.headers,
  });
  if (!response.ok) {
    let body: APIErrorBody | undefined;
    try {
      body = (await response.json()) as APIErrorBody;
    } catch {
      // A non-JSON failure still has a status worth reporting.
    }
    throw new APIError(
      body?.error?.code ?? "request_failed",
      body?.error?.message ?? `Request failed with status ${response.status}`,
      response.status,
    );
  }
  if (response.status === 204) return undefined as T;
  return (await response.json()) as T;
}

export const api = {
  queue: () => request<QueueView>("/api/queue"),
  runs: () => request<Run[] | null>("/api/runs").then((runs) => runs ?? []),
  run: (id: string) => request<RunView>(`/api/runs/${encodeURIComponent(id)}`),
  job: (id: string) => request<JobDetail>(`/api/jobs/${encodeURIComponent(id)}`),
  attempt: (id: string) => request<Attempt>(`/api/attempts/${encodeURIComponent(id)}`),
  attemptEvents: (id: string, after: number, limit = 200) =>
    request<EventPage>(
      `/api/attempts/${encodeURIComponent(id)}/events?after=${after}&limit=${limit}`,
    ),
  attemptGates: (id: string) =>
    request<GateEvidence[] | null>(`/api/attempts/${encodeURIComponent(id)}/gates`).then(
      (rows) => rows ?? [],
    ),
  attemptEnvelopes: (id: string) =>
    request<EnvelopeRecord[] | null>(`/api/attempts/${encodeURIComponent(id)}/envelopes`).then(
      (rows) => rows ?? [],
    ),
  fleet: () => request<FleetView>("/api/workers"),
  worktrees: () =>
    request<WorktreeLedgerEntry[] | null>("/api/worktrees").then((rows) => rows ?? []),
  releaseWorktree: (attemptID: string) =>
    request<WorktreeLedgerEntry>(`/api/worktrees/${encodeURIComponent(attemptID)}/release`, {
      method: "POST",
      body: JSON.stringify({ confirm: true }),
    }),
  retryJob: (jobID: string) =>
    request<unknown>(`/api/jobs/${encodeURIComponent(jobID)}/retry`, { method: "POST" }),
  cancelJob: (jobID: string) =>
    request<unknown>(`/api/jobs/${encodeURIComponent(jobID)}/cancel`, { method: "POST" }),
};

// parseAttemptSummary reads the engine's result payload. A result that does
// not parse is not an error the operator can act on — it is raw text to show.
export function parseAttemptSummary(result: string | undefined): AttemptSummary | null {
  if (!result) return null;
  try {
    const parsed = JSON.parse(result) as AttemptSummary;
    return typeof parsed === "object" && parsed !== null ? parsed : null;
  } catch {
    return null;
  }
}
