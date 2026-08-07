// types.ts — the control plane's JSON shapes, mirroring internal/protocol and
// the U8 read routes. Kept hand-written and small: the UI reads a handful of
// endpoints, and a generated client would be more machinery than contract.

export interface APIErrorBody {
  error: { code: string; message: string };
}

export interface TraceEvent {
  seq: number;
  type: string;
  phase?: string;
  name?: string;
  payload?: unknown;
  started_at?: string;
  ended_at?: string;
}

export interface EventPage {
  attempt_id: string;
  after: number;
  next_cursor: number;
  events: TraceEvent[] | null;
}

export interface Attempt {
  id: string;
  job_id: string;
  worker_id?: string;
  attempt_number: number;
  state: string;
  lease_expires_at?: string;
  runtime_name?: string;
  runtime_version?: string;
  result?: string;
  error?: string;
  started_at?: string;
  completed_at?: string;
  created_at: string;
}

export interface Job {
  id: string;
  run_id: string;
  repository: string;
  base_sha: string;
  state: string;
  cancellation_requested: boolean;
  created_at: string;
  updated_at: string;
}

export interface RunTarget {
  repository: string;
  base_sha: string;
}

export interface Run {
  id: string;
  definition_id: string;
  definition_generation: number;
  snapshot: string;
  parameters?: Record<string, string>;
  targets: RunTarget[];
  state: string;
  created_at: string;
  updated_at: string;
}

export interface RunView {
  run: Run;
  jobs: Job[];
}

export interface PublishRecord {
  attempt_id: string;
  step: string;
  branch: string;
  remote_ref?: string;
  pr_url?: string;
  completed_at: string;
}

export interface WorktreeLedgerEntry {
  attempt_id: string;
  worker_id: string;
  repository: string;
  path: string;
  reason: string;
  state: string;
  created_at: string;
  updated_at: string;
}

export interface JobDetail {
  job: Job;
  run_id: string;
  definition?: string;
  attempts: Attempt[] | null;
  publish: PublishRecord[] | null;
  worktree?: WorktreeLedgerEntry;
}

export interface QueueEntry {
  job_id: string;
  run_id: string;
  definition?: string;
  repository: string;
  base_sha: string;
  state: string;
  position?: number;
  attempt?: Attempt;
  enqueued_at: string;
}

export interface QueueView {
  depth: number;
  active_count: number;
  entries: QueueEntry[] | null;
  observed_at: string;
}

export interface RuntimeCapability {
  name: string;
  version: string;
  can_resume: boolean;
  reports_cost: boolean;
}

// FleetMember flattens the control plane's worker record: `live` is the
// claim transaction's own verdict, computed server-side, because the browser
// clock has no business deciding whether work can move.
export interface FleetMember {
  id: string;
  name: string;
  worker_version: string;
  capacity: number;
  active_count: number;
  available: number;
  live: boolean;
  env_names: string[] | null;
  runtimes: RuntimeCapability[] | null;
  registered_at: string;
  last_heartbeat: string;
}

export interface FleetView {
  workers: FleetMember[] | null;
  live_count: number;
  stale_count: number;
  available_slots: number;
  observed_at: string;
}

export interface GateEvidence {
  attempt_id: string;
  phase: string;
  gate: string;
  emission: number;
  item: string;
  ok: boolean;
  note?: string;
  recorded_at: string;
}

export interface EnvelopeRecord {
  attempt_id: string;
  phase: string;
  emission: number;
  valid: boolean;
  body: string;
  parse_error?: string;
  recorded_at: string;
}

// PhaseResult and AttemptSummary are the shape the engine writes into an
// attempt's `result` string (worker.Outcome.Result). It is parsed lazily and
// defensively: a result the UI cannot read must degrade to "show the raw
// text", never to a blank screen.
export interface GateCheck {
  item: string;
  ok: boolean;
  note?: string;
}

export interface PhaseResult {
  phase: string;
  kind: string;
  status: string;
  phase_attempt: number;
  envelope?: unknown;
  gates?: { checks?: GateCheck[] | null };
  error?: string;
  started_at?: string;
  ended_at?: string;
}

export interface AttemptSummary {
  phases?: PhaseResult[] | null;
  acceptance?: { passed?: boolean; checks?: GateCheck[] | null };
  changed_paths?: string[] | null;
  publish?: string;
}
