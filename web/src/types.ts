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
  /** Why publish.hold_when held this attempt; kept after a release. */
  publish_hold?: string;
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
  /** Set while an operator's publish request waits for the owning worker. */
  publish_requested_at?: string;
  created_at: string;
  updated_at: string;
}

export interface RunTarget {
  repository: string;
  base_sha: string;
}

export interface Definition {
  id: string;
  name: string;
  source: string;
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
  stations?: Station[];
  delivery?: Delivery;
}

// Station is one phase of the run's frozen definition, in declared order.
export interface Station {
  name: string;
  kind: string;
  owner?: string;
  model?: string;
  if?: string;
  gates?: string[];
  parallel?: boolean;
}

// Delivery is the frozen publish policy the job runs under.
export interface Delivery {
	merge_method?: string;
  hold_when?: string;
  ci_wait: boolean;
}

// Report is the slice of GET /api/report the plant bar reads. Rates are
// fractions, and null when there was nothing to divide by.
export interface Report {
  jobs: { total: number; accepted: number; accepted_unpublished: number; failed: number; cancelled: number };
  accepts: { clean: number };
  publish: { held: number };
  ci: { waited: number; first_pass_green: number };
  spend: { total_usd: number; per_clean_accept_usd: number | null };
}

export interface QueueEntry {
	requires_merge?: boolean;
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
  publish?: string | {
    state: string;
    code?: string;
    detail?: string;
    branch?: string;
    pr_url?: string;
  };
  publish_hold?: string;
}

// RunListItem is one row of GET /api/runs: the run with a summary of each of
// its jobs (internal/controlplane/runs_list.go). The summaries are what list
// rows, Floor rows and board cards are built from, so a list of 40 jobs is
// one request rather than one or two per row.
export interface RunListItem extends Run {
  job_summaries?: JobSummary[] | null;
}

// RunListPage is one page of GET /api/runs. `next` is the run ID to pass as
// `before` for the next page (the X-Next-Before header); null on the last.
export interface RunListPage {
  runs: RunListItem[];
  next: string | null;
}

// JobSummary is the slice of JobDetail a list row needs: the job, the frozen
// chain and policy, the latest attempt without its result and error bodies,
// where each of that attempt's phases stands, and its publish records.
export interface JobSummary {
  job: Job;
  stations?: Station[];
  delivery?: Delivery;
  attempt_count: number;
  latest_attempt?: AttemptBrief;
  phases: PhaseProgress[] | null;
  publish: PublishRecord[] | null;
}

// AttemptBrief is an attempt with first-line excerpts in place of its result
// and error, and the result's publish outcome read out server-side.
export interface AttemptBrief extends Omit<Attempt, "result" | "error"> {
  result_excerpt?: string;
  result_truncated?: boolean;
  error_excerpt?: string;
  error_truncated?: boolean;
  publish_state?: string;
  publish_code?: string;
}

// PhaseProgress is one phase of an attempt in the trace's vocabulary:
// running, death, or phase_end's status (success, fail, skipped).
export interface PhaseProgress {
  phase: string;
  status: string;
  rounds: number;
}
