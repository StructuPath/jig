-- 005_publish_ci_repairs.sql — the ledger of CI repair rounds.
--
-- A definition with publish.ci.on_fail may push fixes to the same branch
-- after CI goes red. Each pushed fix is one round: the red head it repaired,
-- the head it pushed, and the checks that were red. publish_records keeps
-- one row per step by design, so rounds get their own table. Rounds chain
-- (round N's head_before is round N-1's head_after), which the control
-- plane enforces; the table enforces only what SQL can say cheaply.

CREATE TABLE publish_ci_repairs (
    attempt_id TEXT NOT NULL REFERENCES attempts(id),
    round INTEGER NOT NULL CHECK (round >= 1),
    branch TEXT NOT NULL,
    head_before TEXT NOT NULL,
    head_after TEXT NOT NULL CHECK (head_after <> head_before),
    failed_checks TEXT NOT NULL DEFAULT '[]',
    completed_at INTEGER NOT NULL,
    PRIMARY KEY (attempt_id, round)
);
