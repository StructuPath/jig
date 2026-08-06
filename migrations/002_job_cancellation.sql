-- 002_job_cancellation.sql — cancellation rides the heartbeat response (R5, U2).
--
-- The lifecycle's `running -> cancelled: cancel via heartbeat` edge needs a
-- durable request flag: the operator sets it, the worker observes it on its
-- next heartbeat, and the worker (never the server) performs the transition.
-- Claim and retry reset it so a stale request never leaks into a new attempt.

ALTER TABLE jobs ADD COLUMN cancellation_requested INTEGER NOT NULL DEFAULT 0
    CHECK (cancellation_requested IN (0, 1));
