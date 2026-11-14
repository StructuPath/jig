-- 011_publish_request_interruptions.sql — a bound on how often one publish
-- request is re-served after its retry was interrupted.
--
-- Migration 008's note that the grant clears the request no longer holds:
-- the request stays on the job until a retry completes, so that a retry the
-- sweeper returns as interrupted is served again (R14). Unbounded, that is a
-- loop: a retry that crashes its worker is swept back, served again, and
-- crashes it again. This counts the interruptions of the pending request;
-- at the limit the sweeper clears the request and records why on the
-- attempt, and the job stays accepted_unpublished for the operator.

ALTER TABLE jobs ADD COLUMN publish_request_interruptions INTEGER NOT NULL DEFAULT 0;
