-- 001_core.sql — the full jig persistent model (U1).
--
-- Constraint style follows factory: every state column carries a CHECK over
-- the exact state vocabulary, every one-at-a-time invariant is a (partial)
-- unique index, and idempotency keys are UNIQUE columns — schema constraints
-- pair with single-transaction state changes, never one half alone (KTD3).
-- Timestamps are integer Unix milliseconds.

CREATE TABLE IF NOT EXISTS schema_migrations (
    version INTEGER PRIMARY KEY,
    applied_at INTEGER NOT NULL
);

-- Definitions edit in place with a generation counter; runs freeze the
-- source by value, so no revision history lives here (R1).
CREATE TABLE definitions (
    id TEXT PRIMARY KEY,
    name TEXT NOT NULL UNIQUE,
    generation INTEGER NOT NULL CHECK (generation > 0),
    source TEXT NOT NULL,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
);

-- A run freezes everything by value at admission: the definition snapshot,
-- the parameters, and the per-target pinned base SHAs (R2, KTD9).
CREATE TABLE runs (
    id TEXT PRIMARY KEY,
    definition_id TEXT NOT NULL REFERENCES definitions(id),
    definition_generation INTEGER NOT NULL CHECK (definition_generation > 0),
    snapshot TEXT NOT NULL,
    parameters TEXT NOT NULL DEFAULT '{}',
    targets TEXT NOT NULL DEFAULT '[]',
    state TEXT NOT NULL CHECK (state IN ('active', 'accepted', 'failed', 'mixed', 'cancelled')),
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
);

-- One job per target repository; failure, retry, and cancellation are
-- per-job, never per-run (R3).
CREATE TABLE jobs (
    id TEXT PRIMARY KEY,
    run_id TEXT NOT NULL REFERENCES runs(id),
    repository TEXT NOT NULL,
    base_sha TEXT NOT NULL,
    state TEXT NOT NULL CHECK (state IN ('queued', 'active', 'accepted', 'accepted_unpublished', 'failed', 'cancelled')),
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    UNIQUE (run_id, repository)
);

CREATE INDEX jobs_claim_order
ON jobs(state, created_at, id);

-- Workers advertise env-var NAMES (never values, R17) and probed runtime
-- capabilities with CLI versions (KTD4) as JSON arrays.
CREATE TABLE workers (
    id TEXT PRIMARY KEY,
    name TEXT NOT NULL,
    worker_version TEXT NOT NULL,
    capacity INTEGER NOT NULL CHECK (capacity BETWEEN 1 AND 100),
    active_count INTEGER NOT NULL DEFAULT 0 CHECK (active_count >= 0),
    env_names_json TEXT NOT NULL DEFAULT '[]',
    runtimes_json TEXT NOT NULL DEFAULT '[]',
    registered_at INTEGER NOT NULL,
    last_heartbeat INTEGER NOT NULL
);

CREATE TABLE attempts (
    id TEXT PRIMARY KEY,
    job_id TEXT NOT NULL REFERENCES jobs(id),
    worker_id TEXT REFERENCES workers(id),
    attempt_number INTEGER NOT NULL CHECK (attempt_number > 0),
    state TEXT NOT NULL CHECK (state IN ('queued', 'preparing', 'running', 'accepted', 'accepted_unpublished', 'failed', 'cancelled', 'lost')),
    lease_digest BLOB,
    lease_expires_at INTEGER,
    runtime_name TEXT,
    runtime_version TEXT,
    result TEXT,
    error TEXT,
    started_at INTEGER,
    completed_at INTEGER,
    created_at INTEGER NOT NULL,
    UNIQUE (job_id, attempt_number)
);

-- The one-at-a-time invariant: a job never has two live attempts (KTD3).
CREATE UNIQUE INDEX one_active_attempt_per_job
ON attempts(job_id)
WHERE state IN ('queued', 'preparing', 'running');

CREATE INDEX attempts_expiry
ON attempts(state, lease_expires_at);

-- Claim idempotency: replaying (worker_id, request_id) returns the stored
-- answer instead of claiming again (R4).
CREATE TABLE claim_requests (
    worker_id TEXT NOT NULL REFERENCES workers(id),
    request_id TEXT NOT NULL,
    lease_digest BLOB NOT NULL,
    attempt_id TEXT REFERENCES attempts(id),
    created_at INTEGER NOT NULL,
    PRIMARY KEY (worker_id, request_id)
);

-- Trace events: per-attempt monotonic seq assigned by the worker. The
-- primary key IS the UNIQUE(attempt_id, seq) contract — replay after an
-- ingest outage lands with INSERT OR IGNORE, exactly once (KTD8).
CREATE TABLE events (
    attempt_id TEXT NOT NULL REFERENCES attempts(id),
    seq INTEGER NOT NULL CHECK (seq >= 0),
    type TEXT NOT NULL,
    phase TEXT NOT NULL DEFAULT '',
    payload BLOB NOT NULL,
    payload_bytes INTEGER NOT NULL,
    server_time INTEGER NOT NULL,
    PRIMARY KEY (attempt_id, seq)
);

-- Every emission persists, valid or not (R7). Invalid rows are size-capped
-- at MaxInvalidEnvelopeBytes; the full form lives in attempt-local JSONL.
CREATE TABLE envelopes (
    attempt_id TEXT NOT NULL REFERENCES attempts(id),
    phase TEXT NOT NULL,
    emission INTEGER NOT NULL CHECK (emission > 0),
    valid INTEGER NOT NULL CHECK (valid IN (0, 1)),
    body TEXT NOT NULL,
    parse_error TEXT NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL,
    PRIMARY KEY (attempt_id, phase, emission)
);

-- Gate evidence: one row per check, so a green gate says what it checked (R9).
CREATE TABLE gate_results (
    attempt_id TEXT NOT NULL REFERENCES attempts(id),
    phase TEXT NOT NULL,
    gate TEXT NOT NULL,
    emission INTEGER NOT NULL CHECK (emission > 0),
    item TEXT NOT NULL,
    ok INTEGER NOT NULL CHECK (ok IN (0, 1)),
    note TEXT NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL
);

CREATE INDEX gate_results_by_attempt
ON gate_results(attempt_id, phase, emission);

-- The control-plane-side retained-worktree ledger (R16), reconciled with
-- worker disk at start. Rows leave 'retained' only through an operator
-- release or a reconcile that proves them gone.
CREATE TABLE retained_worktrees (
    attempt_id TEXT PRIMARY KEY REFERENCES attempts(id),
    worker_id TEXT NOT NULL REFERENCES workers(id),
    repository TEXT NOT NULL,
    path TEXT NOT NULL,
    reason TEXT NOT NULL,
    state TEXT NOT NULL CHECK (state IN ('retained', 'released', 'lost')),
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
);

CREATE INDEX retained_worktrees_by_repository
ON retained_worktrees(repository, state);

-- Publish proof, one row per idempotent step (R14): push the attempt-scoped
-- branch, find-or-create the PR, verify the remote ref.
CREATE TABLE publish_records (
    attempt_id TEXT NOT NULL REFERENCES attempts(id),
    step TEXT NOT NULL CHECK (step IN ('push', 'pull_request', 'proof')),
    branch TEXT NOT NULL,
    remote_ref TEXT NOT NULL DEFAULT '',
    pr_url TEXT NOT NULL DEFAULT '',
    completed_at INTEGER NOT NULL,
    PRIMARY KEY (attempt_id, step)
);
