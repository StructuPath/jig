-- 008_publish_request.sql — an operator's request to finish a job's publish.
--
-- The publish-only retry (R14) is performed by the worker that ran the
-- attempt: only its data directory holds the retained worktree. Until now
-- the only way to start one was a CLI that needs that worker stopped. The
-- request is the operator's half, recorded here with no lease and no worker
-- identity; the owning worker reads it while it runs and performs the retry
-- itself. The timestamp is cleared in the same transaction that grants the
-- retry's lease, so one request is at most one retry. NULL means none.

ALTER TABLE jobs ADD COLUMN publish_requested_at INTEGER;
