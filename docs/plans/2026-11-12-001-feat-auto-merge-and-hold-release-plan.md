---
title: "feat: Merge low-risk work without a person, and release held work without stopping the worker"
type: feat
date: 2026-11-12
---

# feat: Merge low-risk work without a person, and release held work without stopping the worker

**Target repo:** `StructuPath/JIG-Software-Factory`. Builds on the publish pipeline and the publish-only retry (parent plan U7, R14), the risk gate and publish hold (`publish.hold_when`), and the CI gate with repair (`docs/plans/2026-09-27-001-feat-ci-repair-plan.md`). Everything cited as `file:line` is at `origin/main` e35d414.

---

## Summary

Two changes that move a person from *in* the loop to *on* it, without weakening what `accepted` means.

1. **Merge.** A definition may declare `publish.merge`. Work that passed every gate, was not held, and went green in CI on a head jig pushed is merged by jig, as a fifth fenced publish step. Anything else stays exactly where it stops today.
2. **Release.** A held or unpublished job can be released from the UI or the CLI while the worker keeps running. Today the only release path is `jig worker publish-retry`, which requires the worker to be stopped first.

The two ship in that order of dependency, release first: once low-risk work merges on its own, the hold is the only place a person still acts, so that place has to work from the running system.

## Problem Frame

**A person is the last step of every job.** `publish` ends at a green pull request (`internal/worker/publish.go:436`). There is no merge code in the repository. For a `risk: low` change that three reviewers approved and CI passed, the person's merge click adds no information: nothing they see is something a gate did not already judge. It is the most frequent human step and the least valuable one.

**The hold lane cannot be used from the running system.** `factory.yaml` holds everything that is not `risk: low` (`examples/definitions/factory.yaml:462`). The release is the publish-only retry, and it exists only as `jig worker publish-retry` (`cmd/jig/publish_retry.go:33`). That command takes the exclusive lock on the worker data directory (`cmd/jig/publish_retry.go:194`), so the operator must stop the worker that ran the job. Under `jig start`, where one process owns both the server and the worker (`cmd/jig/start.go:17`), that means stopping Jig, running a command in a shell with the right `--data` and author flags, and starting Jig again. The UI tells the operator to "review the result and retained worktree before releasing the publication hold" (`web/src/outcomes.ts:38`) and offers no way to do it.

The first dogfood trial (2026-10-08) recorded this as "no operator path to release a held job". That was accurate for the HTTP API and the UI; the CLI landed since. The gap that remains is the one above.

**The obvious fixes are wrong.**

- *Use `gh pr merge --auto` and let GitHub decide.* That hands the merge decision to branch-protection settings jig cannot read, and it merges whatever head is on the branch when checks pass, including a commit a person or another process pushed. jig already waits for CI itself and knows exactly which head it judged. The merge must name that head.
- *Let the UI call the existing `POST /api/jobs/{job_id}/publish-retry`.* That endpoint is the worker's fenced action: it needs the owning worker's id and a lease token (`internal/protocol/publish.go:180`), and what it grants is a lease the caller must then publish under, from the worker's retained worktree. A browser has none of that.

The bar for this feature: **`accepted` keeps meaning every declared gate passed on the exact code that was delivered, and a person's sign-off is never converted into a merge they did not perform.**

---

## Requirements

### Merge

- R1. A definition may declare `publish.merge: {method: squash | merge | rebase}`. It is valid only when `publish.ci.wait` is true. `method` is required. Anything else is rejected at save time.
- R2. `merge` is a fifth publish step after `ci`, authorized and recorded through the control plane under the attempt's lease like every other step. Its prerequisites are `push`, `pull_request`, `proof` and `ci`.
- R3. jig merges only the head it judged. The head CI went green on must be a head jig pushed (the `push` record, or the last repair round's `head_after`). If it is any other head, a person has pushed, and jig does not merge. The merge command names that head, so GitHub refuses the merge if the branch moved after the CI wait.
- R4. An attempt that was ever held is never merged by jig. A hold that a person released ends at a green pull request, as it does today. The hold becomes a fact on the ledger so the control plane can enforce this without parsing a result.
- R5. For a definition that declares `publish.merge`, an attempt that was never held is `accepted` only with a recorded `merge` step. The control plane enforces this from the frozen snapshot, in the same place it enforces `proof` and `ci` (`internal/controlplane/store.go:531`). An attempt that was held is `accepted` without one.
- R6. A merge that GitHub refuses (conflict, branch protection, required review, merge queue, head moved) pushes nothing, rebases nothing and retries nothing. The attempt ends `accepted_unpublished` with a merge code and GitHub's reason in the result, the pull request open and green.
- R7. The publish-only retry may perform the `merge` step for a never-held attempt, under R3. It is the re-entry path after a crash between green CI and merge, and after a person clears what blocked the merge.
- R8. A worker merges only when its operator allowed it. Workers advertise a `merge` capability, off by default. A job whose frozen definition declares `publish.merge` is claimed only by a worker that advertises it, in the way a `runtime:` pin already restricts claims.
- R9. `jig report` counts merged accepts and merge failures by code, beside the existing publish outcomes.

### Release

- R10. `POST /api/jobs/{job_id}/publish-request` records an operator's request to finish publishing an `accepted_unpublished` job. It needs no lease and no worker id. It is refused for a job in any other state. `DELETE` on the same path withdraws a request that no worker has taken yet.
- R11. The worker that ran the attempt, while it is running, picks the request up and performs the existing publish-only retry in-process. No phase runs and no agent starts. Nothing about the retry's own contract changes: same attempt, same branch, same pull request, CI judged and never repaired.
- R12. The request is consumed in the same transaction that grants the retry's lease, so one request produces at most one retry. A retry that ends `accepted_unpublished` again needs a new request.
- R13. A request the owning worker cannot take stays pending and visible: the job shows that it waits for worker `<id>`, and whether that worker is live.
- R14. `jig job release <job-id>` posts the request and, by default, waits for the outcome with the exit codes `jig worker publish-retry` uses. `--no-wait` returns once the request is recorded.
- R15. The run detail page offers the action on an `accepted_unpublished` attempt: "Release hold" when the publish state is held, "Retry publication" otherwise. Both require a confirmation that names the repository and what will be pushed. Neither is offered for a kept-local delivery (`jig_ui_delivery != publish`) or an empty changeset, where a retry cannot help.
- R16. `jig worker publish-retry` keeps working unchanged, for a worker that is stopped.

---

## Key Technical Decisions

- **KTD1 — Merge is a publish step, not a phase and not a post-accept hook.** The step vocabulary (`internal/protocol/publish.go:18`) already gives every delivery side effect the same shape: authorize before the irreversible action, record after it, skip when the ledger already proves it. A merge is the most irreversible action in the pipeline, so it gets the strongest fence jig has, and the retry re-enters it for free. A post-accept hook would merge work the ledger already calls `accepted` and leave no state for a merge that failed.

- **KTD2 — jig merges directly and names the head; it does not arm GitHub's auto-merge.** By the time the `merge` step runs, jig has waited for CI and recorded the green head. The gateway runs `gh pr merge <url> --<method> --match-head-commit <sha>` with the recorded head. `--auto` is not used: it defers the decision to settings jig cannot see, and it is unavailable on repositories without required checks. `--delete-branch` is not used either: the attempt branch is evidence, and stray-branch reconciliation already treats attempt branches as things to report, never delete (`internal/worker/publish.go:1186`).

- **KTD3 — A hold is recorded on the ledger, once, and never cleared.** Today a hold exists only inside the attempt's result summary (`internal/worker/publish.go:345`), which the control plane does not interpret. R4 and R5 need a fact the control plane can trust. The completion request gains an optional `publish_hold` reason, and migration 007 adds `attempts.publish_hold`. `CompleteAttempt` writes it when an attempt completes `accepted_unpublished` with a hold, and nothing ever sets it back to NULL: a retry re-leases the same attempt row, so the fact survives the release. The engine half already exists as `Outcome.PublishHold` (`internal/worker/registration.go:96`). A CI repair round that trips the hold (`internal/worker/publish_repair.go:114`) is recorded the same way.

- **KTD4 — A released hold is never merged, by construction.** With KTD3, the control plane refuses to authorize the `merge` step for an attempt whose `publish_hold` is set, and does not require a `merge` record to accept it. This is what makes the release endpoint safe to add: the request in R10 is unauthenticated, like the whole API (README, *Loopback only*), so it must not be able to turn "push this held branch" into "merge this held branch". The strongest thing a release request can cause is what the CLI retry already causes: a branch and a pull request.

- **KTD5 — The head rule is enforced on the ledger, not only by the worker.** The control plane authorizes `merge` only when the `ci` record's `remote_ref` equals the `push` record's `remote_ref` or the last row in `publish_ci_repairs`. It already draws this line for reporting (`internal/controlplane/report.go:72`: "green CI recorded on a head jig pushed" against a person's fix). A worker that skipped its own check is refused before it merges. `--match-head-commit` is the last line, for a push that lands between the record and the merge.

- **KTD6 — A blocked merge is a result, not a problem to solve.** A conflict means the base moved under the change. Rebasing would produce code no gate has seen; that is the mistake the CI repair plan refused to make, and the same reasoning applies. So jig stops and says why (R6). The codes are `merge_conflict`, `merge_blocked` (protection, required review, merge queue), `merge_head_moved`, and `merge_failed` for anything else, with `gh`'s diagnostic bounded by `MaxPublishDiagnosticBytes`. Re-admitting the run at the new base head is the existing, explicit way to try again (`POST /api/runs/{run_id}/readmit`).

- **KTD7 — Merge permission is a worker capability, so two keys must turn.** A definition is data that a UI form or a trigger can submit. The decision that *this machine's GitHub identity may merge without a person* belongs to the operator who started the worker. `jig worker --allow-merge` advertises the capability at registration beside `env_names` and `runtimes` (`internal/protocol/types.go:316`); claim eligibility requires it for a snapshot that declares `publish.merge`. `jig start` passes the flag through only when it is given. A job no worker may merge waits in the queue, visibly, as a job pinned to an absent runtime does. The alternative, letting any worker claim it and skip the step, would leave every such job `accepted_unpublished` and teach the operator to ignore that state.

- **KTD8 — The release is a request the owning worker pulls, not a command the server pushes.** The retained worktree, the manifest and the repository cache live in one worker's data directory, and that worker's process already holds the lock on it. So the running worker is the only process that can publish the attempt without a second lock holder. The server records the request on the job (migration 008, `jobs.publish_requested_at`); the worker reads `GET /api/workers/{worker_id}/publish-requests` on its claim tick (`internal/worker/claiming.go:171`) and calls its own `RetryPublish` (`internal/worker/publish.go:1121`) with the gateway and options it already publishes with. That also removes a failure mode of the CLI: the operator can no longer run the retry with different `--commit-author-*` or `--base-branch` values than the worker used. The worker stays ignorant of the control plane's internals; this is one more HTTP read (KTD1 of the parent plan, `just boundary`).

- **KTD9 — A release takes a worker slot.** A retry under `publish.ci.wait` can hold for the CI timeout. `RetryPublish` already tracks the attempt as active (`internal/worker/publish.go:1139`); the request poll runs only while `activeCount() < Capacity`, exactly like a claim. A full worker releases the hold when a slot frees, and R13 makes the wait visible.

- **KTD10 — The stock factory does not merge by default.** `factory.yaml` and `factory-parallel.yaml` gain a commented `merge:` line and a paragraph in their header on what turning it on means. Turning it on is the operator's edit, made after the gate below has been run on their own repository. A third full copy of a 470-line definition would be a third thing to keep in step.

---

## High-Level Technical Design

```
engine chain ──► accepted ──► hold_when?
                                 │
              ┌── holds ─────────┴───────── does not hold ──┐
              ▼                                             ▼
   complete accepted_unpublished                 push ─► PR ─► proof ─► await CI
   attempts.publish_hold = reason (KTD3)                                  │
              │                                            red ◄──────────┤ green
              │                              rerun / repair rounds        │ record ci
   operator: UI button or `jig job release`        (unchanged)            ▼
              │                                              definition declares merge?
   POST /api/jobs/{id}/publish-request                         │ no            │ yes
              │                                                ▼               ▼
   owning worker's claim tick sees it (KTD8)               accepted     authorize merge:
              │                                                          never held (KTD4)
   RetryPublish in-process: same attempt, same branch                    ci head = jig's head (KTD5)
              │                                                                │
   push ─► PR ─► proof ─► await CI ─► accepted                    gh pr merge --match-head-commit
   (no merge step: publish_hold is set)                              │ merged          │ refused
                                                                     ▼                 ▼
                                                              record merge      accepted_unpublished
                                                                accepted        merge_conflict | merge_blocked |
                                                                                merge_head_moved | merge_failed
```

What `accepted` requires, by case:

| Definition | Attempt | Records required for `accepted` |
|---|---|---|
| no `ci`, no `merge` | any | `proof` |
| `ci.wait` | any | `proof`, `ci` |
| `ci.wait` + `merge` | never held | `proof`, `ci`, `merge` |
| `ci.wait` + `merge` | held, then released | `proof`, `ci` |

---

## Implementation Units

Release first (U1–U3), then merge (U4–U8). U1 is shared.

### U1. Control plane: the hold as a ledger fact

- **Files:** `migrations/007_publish_hold.sql`, `internal/protocol/types.go` (completion request, `Attempt`), `internal/controlplane/store.go`, `internal/worker/claiming.go` (`completeAttempt`), `internal/worker/publish.go`, tests beside each.
- **Approach:** `attempts.publish_hold TEXT` (NULL = never held), bounded like other diagnostics. `CompleteAttempt` writes it only on a transition to `accepted_unpublished` and only when it is NULL; a completion that carries a hold with any other state is refused. `Attempt` exposes it read-only. `PublishingRunner.Run` and the repair path pass `Outcome.PublishHold` through. The result summary keeps its `publish_hold` text, so the UI and the report read as before.
- **Tests:** a held completion stores the reason; a later retry's completion as `accepted` leaves it set; a hold on a failed attempt is refused; an attempt completed before the migration reads as never held.

### U2. Control plane and worker: the publish request

- **Files:** `migrations/008_publish_request.sql`, `internal/protocol/publish.go`, `internal/controlplane/publish_ledger.go`, `internal/controlplane/http.go`, `internal/worker/client.go`, `internal/worker/claiming.go`, `internal/worker/publish.go`, tests beside each.
- **Approach:** `jobs.publish_requested_at` (NULL = none). `POST` sets it when the job is `accepted_unpublished` and is idempotent while it is set; `DELETE` clears it. `GET /api/workers/{worker_id}/publish-requests` lists requested jobs whose latest attempt belongs to that worker. `Store.RetryPublish` clears the column in its existing transaction (`internal/controlplane/publish_ledger.go:347`), which gives R12. On each claim tick with a free slot, the worker lists its requests and runs `RetryPublish` for each in a goroutine, tracked like a claimed attempt. The worker is constructed with the publishing runner's gateway and options so the retry publishes exactly as the original attempt would. The job and run views carry `publish_requested_at` and the owning worker id.
- **Tests:** request refused for every state but `accepted_unpublished`; replay idempotent; withdraw before pickup; a request for another worker's attempt is not listed; one request, one retry, under two concurrent polls; a request made while the worker is full is taken when a slot frees; a retry that fails again leaves the job `accepted_unpublished` with no request pending; the sweeper's handling of an interrupted retry is unchanged; the worker integration test drives hold → request → published over the real HTTP handler.

### U3. CLI and UI: the operator's release

- **Files:** `cmd/jig/job.go` (new), `cmd/jig/main.go`, `web/src/api.ts`, `web/src/RunDetail.tsx`, `web/src/outcomes.ts`, `web/src/types.ts`, tests beside each, `web/dist/` rebuilt, `README.md`, `docs/quickstart.md`, `docs/dogfooding.md`.
- **Approach:** `jig job release [--server] [--no-wait] [--json] <job-id>` posts the request and polls the job until it leaves the requested state, mapping the outcome to exit codes 0, 1 and 3 as `jig worker publish-retry` does. The run detail page shows the action per R15 and, once requested, a pending line with the worker's liveness and a withdraw control. `attemptOutcome`'s `next` text names the button. The docs describe the button and `jig job release` as the path for a running worker and keep the stop-the-worker command for a stopped one.
- **Tests:** CLI exit codes against a fake server; the page offers the right label per publish state and none for kept-local and empty-changeset; confirm before post; pending and withdrawn states render; vitest and `tsc -b` green; committed bundle matches the rebuild.

### U4. Protocol: `publish.merge` and its save-time contract

- **Files:** `internal/protocol/definition.go`, `internal/protocol/publish.go`, `internal/protocol/definition_test.go`.
- **Approach:** `MergeSpec{Method}` as `PublishSpec.Merge`. `validatePublish` enforces R1. `PublishStepMerge = "merge"` joins `PublishSteps` with the prerequisites in R2. `Definition.Merges()` beside `WaitsForCI()`. Until U6 lands, a declared `merge` is refused at save time with a message that says it is not yet executable, as the CI repair plan did for `on_fail`.
- **Tests:** each rejection in R1; a valid block parses; a definition without `merge` parses as before; `just definitions` stays green.

### U5. Control plane: authorize, record, and the accepted rule

- **Files:** `migrations/009_publish_merge_step.sql` (the `publish_records` step CHECK, as migration 004 did for `ci`), `internal/controlplane/publish_ledger.go`, `internal/controlplane/store.go`, `internal/controlplane/claim.go`, `internal/protocol/types.go`, tests beside each.
- **Approach:** authorize `merge` only when the lease is valid, the snapshot declares it, `ci` is recorded, `attempts.publish_hold` is NULL (KTD4), and the `ci` head is a head jig pushed (KTD5). The record's `remote_ref` is the merge commit. `CompleteAttempt` requires the `merge` record per the table above. Workers register a `merge` capability; `claim` skips a job whose snapshot merges for a worker without it, and the queue view says why the job waits.
- **Tests:** each refusal, at both authorize and record; `accepted` refused without `merge` for a never-held merging definition and allowed for a released hold; replay idempotence and conflicts; a non-merging worker never claims a merging job and a merging worker does; a foreign or expired lease is refused.

### U6. Worker: the merge step and the gateway

- **Files:** `internal/worker/publish.go`, `internal/worker/publish_test.go`, `cmd/jig/worker.go`, `cmd/jig/start.go`, tests beside each.
- **Approach:** `PullRequestGateway` gains `MergePullRequest(ctx, repository, url, method, head) (mergeCommit string, err error)`. The GitHub implementation runs the command in KTD2 under `PublishCommandTimeout`, then reads the merge commit with `gh pr view --json mergeCommit,state`, so an already-merged pull request is recorded rather than merged twice. `gh` failures map to the codes in KTD6. `publish` runs the step after `ci` when the policy merges; it is inside the existing critical section, where every command is short. On a merge failure the summary keeps `PullRequestURL` and `CIRef`, so the result shows a green pull request and the reason. `--allow-merge` on `jig worker` and `jig start`. The retry path gets the step with no new code, since it calls `publish`.
- **Tests:** against the fake gateway: green → merged → `accepted`; each refusal code → `accepted_unpublished` with the pull request intact; a person's head is never merged; a held-then-released attempt publishes and does not call the gateway's merge; a retry after a simulated crash between `ci` and `merge` merges once; an already-merged pull request is recorded. The U4 save-time refusal is lifted in the same PR.

### U7. Stock definitions, report, docs

- **Files:** `examples/definitions/factory.yaml`, `examples/definitions/factory-parallel.yaml`, `internal/controlplane/report.go`, `cmd/jig/report.go`, `web/src/stations.ts` (a `merge` delivery station), `README.md`, `docs/quickstart.md`, `docs/dogfooding.md`, `web/dist/`.
- **Approach:** the commented opt-in and header text of KTD10. The header states plainly that with `merge` on, the risk classifier is the only thing between a model's change and the default branch, and lists what it scores as not low today (`examples/definitions/factory.yaml:430`). The report adds merged accepts, merge failures by code, and the share of accepts that needed no person. The station line shows `merge` after `ci` when the delivery policy declares it. The README's publish section gains a "Publish can merge" entry and the capability flag.
- **Tests:** report arithmetic over a seeded ledger; the station line for merged, blocked and held-released attempts; stock definitions validate.

### U8. Verification gate

Run on a scratch repository with CI, never first on a repository that matters. The worker runs with `--allow-merge`; the definition is `factory.yaml` with `merge: {method: squash}`.

| # | Task designed to | Expected |
|---|---|---|
| 1 | be low risk, green first pass | merged, `accepted`, `merge` record holds the merge commit |
| 2 | touch `.github/` | held; released from the UI with the worker running; pull request green; **not merged**; `accepted` |
| 3 | go red in CI once | repair round, then merged on the round's head |
| 4 | conflict with a commit pushed to the base during the CI wait | `accepted_unpublished`, `merge_conflict`, pull request open |
| 5 | be low risk, with a person's commit pushed to the branch during the CI wait | `accepted_unpublished`, `merge_head_moved` or the control plane's head refusal; not merged |
| 6 | be low risk, on a worker started without `--allow-merge` | the job waits in the queue and says why |
| 7 | be held, with the worker killed after the request | request stays pending; taken when the worker returns |

The gate passes when every row ends as expected and `jig report` agrees with the ledger. Record the runs, spend and anything the fakes could not see in this section, as the CI repair plan did.

---

## Out of Scope

Named so nobody mistakes this plan for the whole road to an unattended factory. Each needs its own plan.

- **Post-merge verification and automatic revert.** After this plan, a bad merge is found by whatever runs on the default branch. This is the largest remaining risk and the next plan to write.
- **An independent verifier that releases high-risk holds.** Held work still waits for a person.
- **Intake and decomposition.** One prompt is still one chain per repository.
- **Agent-authored definitions.**
- **Rebasing or updating a branch whose base moved.**
- **Operator identity.** One trusted operator, no authentication, as today.

---

## Risks & Dependencies

| Risk | Mitigation |
|---|---|
| A wrong change merges with no person having read it | Opt-in twice (definition and worker, KTD7, KTD10). Only never-held work merges, so everything the classifier scores as not low still stops. Only a head every gate judged merges (R3). The gate in U8 runs before any real repository. The residual risk is a change that is wrong and scores low; post-merge verification is the answer and is out of scope here, so do not turn `merge` on for a repository that deploys from its default branch without its own safeguards. |
| The classifier becomes the only gate between a model and the default branch | It is deterministic and fails closed (`risk != low`). U7 documents its exact rules beside the switch. Tune the pattern and threshold per repository before opting in. |
| A local process posts a release for held work | It could already post a run with any prompt; the API is loopback-only by design. KTD4 bounds the damage at a pull request: a released hold never merges. |
| Merging a branch that is green but behind its base | GitHub merges it if there is no textual conflict, and the combination is untested. Repositories that care should require up-to-date branches or a merge queue; jig then reports `merge_blocked` and stops (KTD6). |
| Required reviews make every merge fail | The pull request author is the operator's own `gh` identity, which cannot approve itself. Such a repository cannot use `merge`; the first job says so with `merge_blocked`. Documented in U7. |
| A crash after GitHub merged and before the record | The retry authorizes `merge`, the gateway finds the pull request already merged, and records it (U6). |
| The request poll adds load on every claim tick | One indexed read per tick, only while a slot is free. |
| Two release paths drift | The UI path and the CLI path both end in `Worker.RetryPublish`; U2's integration test and the existing retry tests cover the same function. |

**Dependencies:** `gh` authenticated with permission to merge on the target repository. No new Go or npm dependency.

## Sources & Research

- `internal/worker/publish.go`: `PublishingRunner.Run` (336), the hold branch (345), `publish` (436), `RetryPublish` (1121), `StrayBranch` (1186)
- `internal/controlplane/publish_ledger.go`: `Store.RetryPublish` (347), the worker-ownership refusal (`publish_retry_foreign_worker`), the retry route (486)
- `internal/controlplane/store.go:531`: `publish_proof_required` and `publish_ci_required`, where the `merge` rule joins
- `internal/controlplane/report.go:72`: "a head jig pushed" as an existing ledger distinction
- `internal/protocol/publish.go`: step vocabulary and prerequisites (18–41), `PublishRetryRequest` (180)
- `internal/protocol/definition.go:176`: `PublishSpec`, `CISpec`
- `internal/worker/claiming.go:171`: `runClaimLoop`
- `cmd/jig/publish_retry.go`: the CLI retry and the data-directory lock (181–255)
- `cmd/jig/start.go:17`: one process, server and worker
- `web/src/outcomes.ts:25`: what the UI says about an unpublished attempt
- `examples/definitions/factory.yaml`: `classify-risk` (424), `publish` (461)
- Parent plan `docs/plans/2026-08-05-001-feat-jig-software-factory-plan.md` (KTD1 boundary, KTD6 fencing, R12, R14) and `docs/plans/2026-09-27-001-feat-ci-repair-plan.md` (the "accepted keeps meaning" bar, the head-moved rule)
- First dogfood trial, 2026-10-08: hold-release gap, held lane not exercised
