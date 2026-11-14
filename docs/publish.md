# Publish: holds, CI, repair, re-runs, and merge

What happens after a chain is accepted. Every behavior here is opt-in through
the definition's `publish:` block; the field table — types, defaults, ranges —
is the [Publish block](definition-reference.md#publish-block) in the
definition reference. Without a `publish:` block, accepted work is published
as a branch and a pull request and the job is `accepted` once the remote ref
is proven; nothing waits for CI and nothing merges.

A job that stops short of `accepted` — held, its pull request not opened, or
CI red — is `accepted_unpublished`, and is finished by the publish-only
retry: `jig job release <job-id>` (or the job's page in the UI) while the
worker that ran it is running, `jig worker publish-retry` when it is stopped.
See [Running a worker](../README.md#running-a-worker).

## Publish can be held

`publish: {hold_when: "<predicate>"}` is judged after acceptance passes.
When it holds, the attempt ends `accepted_unpublished` with the hold
recorded, nothing is pushed, and the worktree and branch are retained — the
publish-only retry is the human sign-off that releases it. Low-risk work
never waits. Write the predicate to fail closed — `risk != low` holds work
whose risk was never reported, where `risk == high` would ship it. The
sign-off is only as strong as access to the control plane: jig has one
trusted operator and no authentication (see [Loopback
only](../README.md#loopback-only)), so the operator's retry is the approval,
with no separate approver identity.

## Publish can wait for CI

`publish: {ci: {wait: true, timeout: 30m}}` adds a fourth publish step after
proof: the worker polls the check runs and commit statuses on the branch's
head through `gh api`, and the job is `accepted` only when every one is
green — the control plane enforces this from the frozen definition, not the
worker's word. A red check fails fast; checks still pending at the timeout
(default 30m, 1m–6h) fail too; a head with no checks at all passes after two
minutes, since that repository runs no CI.

Any of these ends the job `accepted_unpublished` with the branch and pull
request in place and the red checks named in the result. Fix the branch —
push to it yourself — and the publish-only retry judges CI on whatever its
head is by then. Neutral and skipped checks count as green. The wait holds a
worker slot, and a cancel during it ends the job the same way. Omit `ci` and
publish does not wait, exactly as before.

## Red CI can be repaired inside the attempt

`publish: {ci: {wait: true, on_fail: {run: build, budget: 2}}}` gives red CI
back to the `run` agent phase instead of ending the job. A round hands it
the failing checks and the last 16 KiB of each failed GitHub Actions job's
log (at most four logs), framed as data, never instructions; then **every
phase after `run`** runs again, gates and repair edges included, and
acceptance and `hold_when` are judged anew.

Only a fix that passes all of that, is not held, and actually changed
something is pushed, non-force, to the same pull request, and CI is awaited
again. There is no way to skip phases: `run` must be an agent phase and not
the last one.

Rounds share the attempt's send budget and wall-clock ceiling, run in a
freshly wiped ephemeral HOME, and are fenced by the lease again right before
each push. jig never builds on someone else's commit: if CI is red on a head
it did not push, no round runs.

When the budget runs out, or a round fails, is held, is cancelled, or
changes nothing, the job ends `accepted_unpublished` exactly as red CI did
before, with every round's heads, changed paths, and outcome in the result.
The publish-only retry judges CI but never repairs — a continuation lives
only in the process that ran the chain. Budget is 1–3.

A fix that weakens a check instead of the code is the risk to design for:
`factory.yaml` refuses lost tests, new skips, and new checker suppressions
at the build with `tests_intact`, and scores edits to CI or lint
configuration, and test files that lose more lines than they gain, as not
low, so such a "fix" is held for a person.

## A flaky Actions job can be re-run before a round is spent

`publish: {ci: {wait: true, rerun: {budget: 1}}}` re-runs the failed GitHub
Actions jobs on the same head when CI is red on a head jig pushed, before
any repair round (or before the job ends red, with no `on_fail`). It first
waits until nothing on the head is still running, since GitHub will not
re-run a job in a running workflow; then, for each workflow run the failed
jobs belong to, it makes one `gh api -X POST
repos/{owner}/{repo}/actions/runs/{id}/rerun-failed-jobs` request (per run,
not per job: re-running one job would put the run in progress and GitHub
would refuse its siblings), checking the lease and cancellation right before
each.

It then judges that same head again, reading each re-run job as pending
until its new run replaces the old red one; if the branch moves meanwhile,
the re-run ends `ci_rerun_head_moved`, so a person's fix is never reported
as jig's flaky pass. The whole re-run — waiting, requests, and judgement —
fits in one CI timeout.

GitHub refusing because a workflow run is in progress is waited out and does
not spend the budget; any other refusal is recorded and falls through to
repair or the end. A re-run GitHub accepted that never finishes, or whose CI
cannot be read, is recorded and leaves CI red as it was, so repair still
runs. If any red check is not an Actions job (a commit status, another app),
no re-run happens. Re-runs never move the branch. The budget, 1–3, is per
attempt: a repair round's new head gets only what is left.

Every re-run is in the result's `ci_reruns` (`attempt`, `head`, `jobs`,
`outcome`), and a pass after one sets `ci_flaky` — it is reported as flaky,
never as a clean pass. Re-run waits run on wall-clock time, so time spent on
them is time a later repair round no longer has under the attempt's ceiling.
The publish-only retry never re-runs. `gh` needs permission to re-run
Actions jobs on the repository. `factory.yaml` and `factory-parallel.yaml`
both declare `rerun: {budget: 1}`.

## Publish can merge

Add `merge: {method: squash}` under `publish` and start the worker with
`--allow-merge` (also supported by `jig start`). `merge`, `squash`, and
`rebase` are the supported methods; `ci.wait: true` is required. Both
switches default off. A worker without consent leaves these jobs queued.
After green CI, Jig merges only its own exact pushed head, including a CI
repair's head. Any attempt ever held ends at a PR, even after release; a
person's commit is never automatically merged.

The immediate GitHub merge API preserves branch protection and refuses
required reviews, queues, conflicts, and changed heads. Jig never queues a
future merge, rebases a blocked branch, or bypasses protection. A failed
merge remains `accepted_unpublished`; use the existing publication retry
after the cause is resolved. A stopped worker's `publish-retry` also needs
`--allow-merge`, and its registration must retain merge consent. `jig
report` counts merged accepts and their share of accepted jobs from the
ledger; merge failures appear in the existing publish failure codes.

Test on a scratch repository first. There is no post-merge verification or
automatic revert. An incorrect change can pass every pre-merge check.
