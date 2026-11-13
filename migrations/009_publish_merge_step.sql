-- Merge plan U5: preserve the ledger while widening its step vocabulary.
CREATE TABLE publish_records_v9 (
    attempt_id TEXT NOT NULL REFERENCES attempts(id),
    step TEXT NOT NULL CHECK (step IN ('push', 'pull_request', 'proof', 'ci', 'merge')),
    branch TEXT NOT NULL,
    remote_ref TEXT NOT NULL DEFAULT '',
    pr_url TEXT NOT NULL DEFAULT '',
    completed_at INTEGER NOT NULL,
    PRIMARY KEY (attempt_id, step)
);
INSERT INTO publish_records_v9 SELECT * FROM publish_records;
DROP TABLE publish_records;
ALTER TABLE publish_records_v9 RENAME TO publish_records;

ALTER TABLE workers ADD COLUMN allow_merge INTEGER NOT NULL DEFAULT 0 CHECK (allow_merge IN (0, 1));
