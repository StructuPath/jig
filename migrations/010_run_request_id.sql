-- 010_run_request_id.sql — an optional client key that makes run admission
-- repeatable.
--
-- Admission resolves refs over the network before it commits, so a client
-- can lose the answer to a run that did commit and send the request again.
-- Without a key the repeat is a second run. With one, the repeat finds the
-- run the key already admitted and answers with it. request_digest
-- fingerprints the request the key was first used for, so the same key on a
-- different request is refused rather than answered with an unrelated run.
-- Both are NULL for runs admitted without a key (triggers, older clients).

ALTER TABLE runs ADD COLUMN request_id TEXT;
ALTER TABLE runs ADD COLUMN request_digest TEXT;
CREATE UNIQUE INDEX runs_request_id ON runs(request_id) WHERE request_id IS NOT NULL;
