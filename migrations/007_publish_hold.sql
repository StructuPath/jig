-- 007_publish_hold.sql — a publish hold is a fact on the attempt.
--
-- A hold (publish.hold_when) used to exist only inside the attempt's result
-- summary, which the control plane stores and does not interpret. Rules the
-- control plane must enforce itself — an attempt that was ever held is never
-- merged by jig — need a column it can trust. The reason is written once,
-- when the attempt completes accepted_unpublished with a hold, and is never
-- cleared: the publish-only retry re-leases the SAME attempt row, so the
-- fact outlives the release. NULL means never held, which is also what every
-- attempt completed before this migration reads as.

ALTER TABLE attempts ADD COLUMN publish_hold TEXT;
