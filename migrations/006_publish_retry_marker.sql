-- 006_publish_retry_marker.sql — the publish-only retry is marked on its
-- attempt.
--
-- A publish-only retry re-leases an accepted_unpublished attempt into
-- `running` (R14). Without a marker, an interrupted retry's expired lease is
-- indistinguishable from an interrupted phase run, so the sweeper marks it
-- `lost` and the job's only road left is a cold retry: a new attempt, a new
-- branch, a second pull request. The flag says "this lease is a publish
-- retry": the sweeper returns such an attempt to accepted_unpublished, same
-- attempt and records, instead. Additive, defaulting every existing row to 0.

ALTER TABLE attempts ADD COLUMN publish_retry INTEGER NOT NULL DEFAULT 0 CHECK (publish_retry IN (0, 1));
