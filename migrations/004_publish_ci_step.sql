-- 004_publish_ci_step.sql — the publish ledger learns the `ci` step.
--
-- A definition whose publish.ci waits is `accepted` only once CI is green on
-- the pull request's head, and that fact is a ledger row like every other
-- publish step. SQLite cannot alter a CHECK constraint in place, so the table
-- is rebuilt with the widened vocabulary. Nothing references publish_records,
-- so the rebuild needs no foreign-key choreography.

CREATE TABLE publish_records_v4 (
    attempt_id TEXT NOT NULL REFERENCES attempts(id),
    step TEXT NOT NULL CHECK (step IN ('push', 'pull_request', 'proof', 'ci')),
    branch TEXT NOT NULL,
    remote_ref TEXT NOT NULL DEFAULT '',
    pr_url TEXT NOT NULL DEFAULT '',
    completed_at INTEGER NOT NULL,
    PRIMARY KEY (attempt_id, step)
);

INSERT INTO publish_records_v4(attempt_id, step, branch, remote_ref, pr_url, completed_at)
SELECT attempt_id, step, branch, remote_ref, pr_url, completed_at FROM publish_records;

DROP TABLE publish_records;

ALTER TABLE publish_records_v4 RENAME TO publish_records;
