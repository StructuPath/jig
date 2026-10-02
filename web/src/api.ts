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
  Definition,
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
  definitions: () => request<Definition[]>("/api/definitions"),
  createDefinition: (source: string) => request<Definition>("/api/definitions", {
    method: "POST", body: JSON.stringify({ source }),
  }),
  startTask: (definitionID: string, repository: string, task: string, mode: string) => request<RunView>("/api/runs", {
    method: "POST", body: JSON.stringify({ definition_id: definitionID, instructions: task,
      parameters: { task_title: task, task_mode: mode }, targets: [{ repository }] }),
  }),
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

export async function ensureDefinition(source: string): Promise<Definition> {
  const existing = (await api.definitions()).find((definition) => definition.source === source);
  if (existing) return existing;
  try {
    return await api.createDefinition(source);
  } catch (error) {
    if (!(error instanceof APIError) || error.code !== "definition_exists") throw error;
    const concurrent = (await api.definitions()).find((definition) => definition.source === source);
    if (!concurrent) throw error;
    return concurrent;
  }
}

// parseAttemptSummary reads the engine's result payload. A result that does
// not parse is not an error the operator can act on — it is raw text to show.
export function parseAttemptSummary(result: string | undefined): AttemptSummary | null {
  if (!result) return null;
  try {
    const parsed = JSON.parse(result) as AttemptSummary;
    if (typeof parsed !== "object" || parsed === null || Array.isArray(parsed)) return null;
    if (parsed.phases != null && (!Array.isArray(parsed.phases) || parsed.phases.some(
      (phase) => !phase || typeof phase !== "object" || typeof phase.phase !== "string" || typeof phase.status !== "string",
    ))) return null;
    const checks = [parsed.acceptance?.checks, ...(parsed.phases ?? []).map((phase) => phase.gates?.checks)];
    if (checks.some((rows) => rows != null && (!Array.isArray(rows) || rows.some(
      (check) => !check || typeof check !== "object" || typeof check.item !== "string" || typeof check.ok !== "boolean",
    )))) return null;
    return parsed;
  } catch {
    return null;
  }
}
