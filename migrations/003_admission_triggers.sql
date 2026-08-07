-- 003_admission_triggers.sql — unattended admission (U6, R13, KTD7).
--
-- Two tables, one contract: a trigger is a standing intent to invoke a
-- definition, and an occurrence is the durable record that one specific
-- firing was admitted. The UNIQUE request key on occurrences is the whole
-- idempotency story — commit-occurrence-then-dispatch means the key is
-- written BEFORE the run exists, so a crash, a restart, or a second poll
-- observing the same issue collides with the key instead of producing a
-- second run. Constraint style follows 001_core.sql: a CHECK over the exact
-- state vocabulary, idempotency keys as UNIQUE columns, integer Unix
-- milliseconds.

-- Triggers are definition-scoped standing intents. They are created DISABLED
-- (the factory heritage: a trigger that starts firing the moment it is saved
-- admits work the operator has not yet read back), so `enabled` defaults to 0
-- and only the explicit enable action sets it — which is also the moment a
-- schedule's cursor is first computed.
CREATE TABLE triggers (
    id TEXT PRIMARY KEY,
    definition_id TEXT NOT NULL REFERENCES definitions(id),
    name TEXT NOT NULL UNIQUE,
    kind TEXT NOT NULL CHECK (kind IN ('schedule', 'github_issue', 'github_pull_request')),
    enabled INTEGER NOT NULL DEFAULT 0 CHECK (enabled IN (0, 1)),
    config TEXT NOT NULL,
    -- next_due_at is a schedule's ENTIRE memory of the future: exactly one
    -- stored instant, never a backlog. On wake after downtime that one
    -- overdue instant is admitted and the cursor jumps to the next future
    -- match — missed fires are not enumerable because they were never
    -- stored (R13).
    next_due_at INTEGER,
    -- next_poll_at is the GitHub equivalent: when this trigger may next
    -- spend a `gh` invocation.
    next_poll_at INTEGER,
    last_checked_at INTEGER,
    -- admitted_count and skipped_count are the surfaced counters. A schedule
    -- that fires while its prior run is still active increments skipped_count
    -- and creates no run (R13).
    admitted_count INTEGER NOT NULL DEFAULT 0 CHECK (admitted_count >= 0),
    skipped_count INTEGER NOT NULL DEFAULT 0 CHECK (skipped_count >= 0),
    -- The last check's outcome, as an actionable diagnostic code plus its
    -- message (KTD7): `gh_timed_out`, `gh_unauthenticated`, `gh_match_limit`.
    diagnostic_code TEXT NOT NULL DEFAULT '',
    diagnostic TEXT NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
);

CREATE INDEX triggers_due
ON triggers(enabled, next_due_at);

CREATE INDEX triggers_poll_due
ON triggers(enabled, next_poll_at);

-- One occurrence per admitted firing. request_key is UNIQUE across all
-- triggers and carries the trigger's identity plus the event content that
-- makes the firing unique — the scheduled instant, or the repository, issue
-- or pull-request number, and (for pull requests) the head SHA.
--
-- run_id is nullable because the row is committed BEFORE the run exists, and
-- because `skipped` and `failed` occurrences never get one. The CHECK is the
-- other half of that contract: `dispatched` is the one state that requires a
-- run, and no other state may carry one.
CREATE TABLE occurrences (
    id TEXT PRIMARY KEY,
    trigger_id TEXT NOT NULL REFERENCES triggers(id),
    request_key TEXT NOT NULL UNIQUE,
    state TEXT NOT NULL CHECK (state IN ('pending', 'dispatching', 'dispatched', 'skipped', 'failed')),
    run_id TEXT REFERENCES runs(id),
    -- The source snapshot: exactly what was observed, frozen by value, so an
    -- occurrence explains itself after the issue is edited or closed.
    source TEXT NOT NULL DEFAULT '{}',
    scheduled_at INTEGER,
    diagnostic TEXT NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    CHECK ((state = 'dispatched') = (run_id IS NOT NULL))
);

-- The recovery query: undispatched occurrences, oldest first. A crash between
-- occurrence-commit and dispatch leaves rows here, and startup re-drives them.
CREATE INDEX occurrences_undispatched
ON occurrences(state, created_at);

CREATE INDEX occurrences_by_trigger
ON occurrences(trigger_id, created_at);
