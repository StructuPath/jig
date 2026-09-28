---
title: "feat: Repair red CI inside the attempt (publish.ci.on_fail)"
type: feat
date: 2026-09-27
---

# feat: Repair red CI inside the attempt (publish.ci.on_fail)

**Target repo:** `StructuPath/jig`. Builds on the CI gate (be9b795, `publish.ci`) and the risk gate and publish hold (9242460). Everything cited as `file:line` is at `origin/main` b2b6325.

---

## Summary

A definition that waits for CI can declare what to do when CI is red: send the failing checks to a named agent phase, then re-run every phase after it (tests, reviewers, risk classifier and all), re-judge acceptance and the publish hold, push the fix to the same pull request, and wait for CI again. The loop has a declared budget. When the budget runs out or any step fails, the attempt ends exactly where red CI ends it today: `accepted_unpublished`, branch and pull request in place, failures named in the result.

## Problem Frame

Today the `ci` publish step is a judge, not a fixer. `awaitCI` (`internal/worker/publish.go:699`) returns `ci_failed` with the red checks named, and the job stops at `accepted_unpublished` until a person pushes a fix and runs the publish-only retry. So "done" still depends on a person whenever CI catches something the local `tests_pass` gate did not, such as a lint rule, another platform, or an integration suite that only runs in CI. That is the gap jig exists to close.

The obvious fix is wrong. A repair agent that runs at publish time and pushes straight to the pull request would ship code that no gate, reviewer or risk classifier has seen, under a job the ledger calls `accepted`. The bar for this feature is that **`accepted` keeps meaning every declared gate passed on the exact code CI went green on.**

The outside reference is OpenAI's account of Codex babysitting a PR until CI is green (Pragmatic Engineer, 2026-09-15). That account gives no defect or rollback data, so it is a direction, not evidence of a safe design. The safety argument here rests on jig's own gates.

---

## Requirements

- R1. A definition may declare `publish.ci.on_fail: {run: <phase>, budget: N}`. It is valid only when `publish.ci.wait` is true. `run` must be an agent phase and must not be the last phase. `budget` must be between 1 and `MaxCIRepairRounds`. Anything else is rejected at save time. There is no field for resuming later in the chain; see KTD1.
- R2. A repair round starts only on `ci_failed`. `ci_timeout`, `ci_unavailable` and `ci_wait_cancelled` end the attempt exactly as they do today.
- R3. A round sends the failing checks to the `run` phase as its input envelope, then runs every phase after `run` to the end of the chain, with every `if:` guard, gate and `on_fail` edge live. Phases before `run` do not re-run; their envelope fields carry over.
- R4. After the chain, acceptance and `publish.hold_when` are judged again over the merged field view. If acceptance fails or the hold now holds, nothing is pushed.
- R5. A fix is pushed to the same attempt-scoped branch without force, and the push is fenced and recorded as a numbered round on the control plane before and after the side effect.
- R6. The control plane enforces the round budget against the frozen snapshot, and refuses `accepted` unless the `ci` record was written after the last recorded round and names none of the heads any round repaired (each of those was judged red). It does not require the green head to equal the last round's `head_after`: a person may push a fix after the rounds run out, and the publish-only retry must still be able to accept it.
- R7. Engine wall-clock and prompt sends are shared with the attempt: rounds draw from the same `AttemptCeiling` and `MaxAttemptSends`, so a repair cannot buy an attempt more budget than it was admitted with.
- R8. Cancellation stops a round at the next phase boundary. The engine part of a round is never inside the publish critical section, which runs under `context.WithoutCancel` (`internal/worker/publish.go:337`).
- R9. If the branch head on the remote is not the head jig last pushed, a person has taken over the branch: no round runs, and the result says so.
- R10. Every way a round can end short of green leaves the attempt `accepted_unpublished`, with the round number, the step it stopped at, and the red checks in the result, plus a trace event per round.
- R11. The publish-only retry stays a judge. It re-reads CI on the current head as it does today and never starts a repair round.

---

## Key Technical Decisions

- **KTD1 — A dedicated `publish.ci.on_fail` block, not a new phase-edge `then:` value.** Publish is not a phase, and `RepairEdge.Then` accepts only `rerun-self` (`internal/protocol/definition.go:388`). A CI failure has no envelope to predicate on and no phase to re-run itself. Reusing the struct would force invalid combinations through validation, so this adds a small `CIRepairSpec{Run, Budget}` beside `CISpec`. It follows the edge vocabulary (`run`, `budget`) where it means the same thing. An earlier draft had a `resume_from` field; review of the U1 PR showed it let `run: build, resume_from: review` skip a `test` between them, carrying `test`'s verdict on the old code into acceptance of the new. The only safe value was the phase right after `run`, so the field was removed rather than constrained.

- **KTD2 — Rounds re-enter the engine through an in-process continuation, handed across the package seam as an interface.** The engine destroys its scratch directory and ephemeral HOME when `Execute` returns (`internal/engine/phase.go:185`), and `summaryJSON` caps each envelope (`maxPhaseEnvelopeBytes`), so the stored result is not a faithful field view to resume from. `engine` imports `worker`, never the reverse, and the boundary check enforces that. So `worker.Outcome` gains `Continuation worker.Continuation`, an interface with `RepairCI(ctx, CIFailure) Outcome` and `Release()`. The engine implements it by keeping its merged field view, `reportedBy`, results, gate reports, touched paths, send count and deadline. Each round builds a fresh scratch and HOME (KTD11 holds per round) and fresh agent sessions. Because the continuation exists only in the process that ran the chain, **the publish-only retry cannot repair (R11)**. That is deliberate: persisting full envelopes to resume across processes is a larger change with its own confidentiality questions, and retry already accepts a person's fix.

- **KTD3 — A round is split: engine (cancellable) → push (critical section) → CI wait (cancellable).** Today the whole publish runs under `WithoutCancel` because each step is a short `gh`/`git` command. A round can run agent phases for tens of minutes, so only the fenced push and record sit in the critical section. The engine part honours `lease.cancelled` like any attempt, and the CI wait already does (`publish.go:767`).

- **KTD4 — Rounds are their own ledger table, not more `publish_records`.** `publish_records` holds one record per step, and replaying a step with different values is a conflict by design (`internal/protocol/publish.go:163`). A second push with a new SHA is not a replay, it is a new fact. Migration 005 adds `publish_ci_repairs(attempt_id, round, head_before, head_after, failed_checks, completed_at)` with `UNIQUE(attempt_id, round)`. Rounds are authorized before the push (the fence runs before the irreversible side effect, KTD6 of the parent plan) and recorded after it, under the lease token, with the same idempotent-replay rule as steps. The `push` step record keeps the first push, and `ci` keeps its existing one-record-when-green semantics; red CI already writes no `ci` record, so there is nothing to supersede.

- **KTD5 — The failing checks arrive through the existing "failing envelope" door, with logs treated as untrusted data.** Repair dispatch already hands a target the failing envelope (`runPhaseOnce(ctx, repairPhase, run.envelopeRef())`, `internal/engine/phase.go:593`). A round synthesizes one: `{ci_failed: true, head, failed_checks: [{name, conclusion, url, log_tail}]}`. `log_tail` exists only for GitHub Actions check runs, whose check-run id is the job id, so the gateway reads `repos/{o}/{r}/actions/jobs/{id}/logs` via `gh api` and keeps a bounded tail (`MaxCIRepairLogBytesPerCheck` 16 KiB, `MaxCIRepairLogBytes` 64 KiB total). Other status checks carry name, conclusion and URL only. CI output can echo text an attacker wrote (test fixtures, issue text), and it is flowing into an agent with write access. That is the same indirect-injection path KTD11 names for issue text, so the log is fenced in the prompt as data. The role's write allowlist, env allowlist and ephemeral HOME apply unchanged, and every phase after `run` re-checks the result.

- **KTD6 — Every failure degrades to today's red-CI outcome.** A round that fails acceptance, trips the hold, makes no change, exhausts the budget, breaches a boundary, loses its lease, or finds the head moved pushes nothing further and ends `accepted_unpublished` with the pull request at the last pushed head. The worst case of opting in is therefore today's behaviour plus fix commits that each passed the full downstream chain before they were pushed.

- **KTD7 — A person's push ends automation for that attempt.** Before a round, the worker compares the red head with the head it last pushed (the `push` record or the last round's `head_after`). If they differ, a person has pushed, and jig does not stack an agent's commits on top. The existing retry path already re-judges whatever a person pushed. The non-force push is the second line of defence if a person pushes mid-round.

---

## High-Level Technical Design

```
engine chain ──► accepted ──► push ─► PR ─► proof ─► await CI
                                                      │
                                         green ◄──────┤
                                   record ci, accepted│ red (ci_failed) and budget left
                                                      ▼
                           head moved? ── yes ──► stop (KTD7)
                                  │ no
                    authorize round N (control plane: budget, lease)
                                  │
             Continuation.RepairCI: fresh scratch + HOME + sessions
               run: <phase> with {ci_failed, failed_checks}
               then every phase after it (guards, gates, edges live)
               acceptance + hold_when again
                                  │ passes, has changes
                commit changed paths, push (non-force)   ◄── critical section
                record round N (head_before, head_after)
                                  │
                              await CI ──► (loop)
```

Round state lives in the worker's `PublishingRunner.Run` loop. The engine owns chain semantics, the worker owns git, the gateway and the ledger, the same split as today.

---

## Scope Boundaries

**In scope:** everything in R1–R11, the `factory.yaml` opt-in, README and quickstart updates, trace events.

**Deferred to follow-up work:**
- Re-running flaky checks (`gh run rerun --failed`) instead of changing code. A round whose `run` phase makes no change currently ends the attempt (`ci_repair_no_change`); a re-run policy should be a separate declared option, since re-running until green hides real flakiness.
- Repair on the publish-only retry (needs persisted continuations; see KTD2).
- Repairing `ci_timeout` (pending too long is an infrastructure problem, not a code problem).
- UI for rounds beyond what the trace timeline already shows from the new events.
- CI providers other than GitHub (v1 is GitHub-coupled).

**Outside this feature's identity:** watching production after merge, deploy agents, incident response. Those are post-merge stages; jig ends at a green pull request.

---

## Implementation Units

### U1. Protocol: `publish.ci.on_fail` and its save-time contract

- **Files:** `internal/protocol/definition.go`, `internal/protocol/definition_test.go`
- **Approach:** add `CIRepairSpec` as `CISpec.OnFail`, and `MaxCIRepairRounds` (3) beside the existing CI bounds in `definition.go`, where `DefaultCITimeout` and friends already live. `validatePublish` enforces R1: `ci.wait` must be true, `run` must name a defined agent phase that is not the last phase, and budget must be in `1..MaxCIRepairRounds`. Nothing else lands in U1: the round ledger types go in with U2, which first uses them, and the log limits go in with U4. Until U5 lands, a declared `on_fail` validates but never fires, so red CI ends the attempt exactly as it does today (KTD6). The PR for U1 must say so.
- **Tests:** each rejection named in R1; a valid block parses with its fields intact; a definition without `on_fail` parses as it did before.

### U2. Control plane: round ledger and the accepted rule

- **Files:** `internal/protocol/publish.go` (round record, authorization and record request types), `migrations/005_publish_ci_repairs.sql`, `internal/controlplane/publish_ledger.go`, `internal/controlplane/store.go`, `internal/controlplane/http.go`, `internal/worker/client.go`, tests beside each.
- **Approach:** authorize round N only when the lease is valid, the snapshot declares `on_fail`, N = last round + 1, N ≤ budget, and `proof` is recorded. Record with idempotent replay (same values return the stored row; different values conflict). Extend `CompleteAttempt` (`store.go:555`) so that when rounds exist, `accepted` needs a `ci` record completed after the last round whose `remote_ref` equals no round's `head_before` (R6).
- **Tests:** over-budget authorization refused; zombie lease refused; replay idempotence; `accepted` refused when `ci` predates the last round; migration from 004 keeps existing rows.

### U3. Engine: continuation and `RepairCI`

- **Files:** `internal/engine/phase.go`, new `internal/engine/resume.go`, `internal/engine/resume_test.go`, `internal/worker/registration.go` (`Outcome` and `AttemptRunner` live there, lines 88 and 101; `Continuation` joins them).
- **Approach:** on the accepted path only, `Execute` returns a continuation holding the execution state named in KTD2 minus scratch and sessions. `RepairCI` creates a fresh scratch, runs `run` with the synthesized envelope, runs every phase after `run` through the existing `runPhaseWithEdge`, then re-runs acceptance and `publishHold`. It returns an `Outcome` whose result lists only the round's touched paths, with `ci_repair_no_change` when there are none. The deadline and send counter carry over (R7). `Release` drops the state; `PublishingRunner` always calls it. Two semantics are fixed here, matching existing repair dispatch: the `run` phase runs regardless of its own `if:` guard, because guards are judged only in `runChain` (`phase.go:513`) and repair targets go through `runPhaseOnce`; and each round's resumed segment starts with fresh per-edge budgets (`edgeUses` is local to a `runChain` call), while the attempt-wide send count and ceiling stay shared.
- **Tests (scripted runtime):** a round re-runs exactly `run` and the phases after it, and none before; a reviewer rejection inside a round loops the builder under its own edge; a classifier field now `high` trips the hold and yields no push; the send budget and ceiling are shared across rounds; cancellation between phases ends the round.

### U4. Gateway: failing-check detail

- **Files:** `internal/protocol/publish.go` (`MaxCIRepairLogBytesPerCheck`, `MaxCIRepairLogBytes`), `internal/worker/publish.go` (`CICheck`, `GitHubCLIGateway`), `internal/worker/publish_ci_test.go`
- **Approach:** `CICheck` gains `CheckRunID` and `App`. A new `FailedCheckLogs(ctx, repository, checks)` fetches Actions job logs via `gh api` under `PublishCommandTimeout` and the stdout cap, keeping the bounded tail (KTD5). A log that cannot be read degrades to name, conclusion and URL with a note; it never fails the round.
- **Tests:** tail bounding per check and in total; non-Actions checks carry no log; `gh` failure degrades without error.

### U5. Worker: the round loop

- **Files:** `internal/worker/publish.go`, new `internal/worker/publish_repair.go`, `internal/worker/publish_repair_test.go`
- **Approach:** in `PublishingRunner.Run`, when `publish` returns `ci_failed` and the continuation is present with budget left: the head-moved check (KTD7), authorize the round, `RepairCI` under the attempt's own cancellable context, then commit and non-force push in the critical section, record the round, and call `awaitCI` again as a fresh `ci` step. `PublishSummary` gains `CIRepairs []{Round, HeadBefore, HeadAfter, FailedChecks, Outcome}`. Events: `ci_repair_round_start`, `ci_repair_round_end`, `ci_repair_exhausted`.
- **Tests (fake gateway plus scripted runtime):** red → fix → green ends `accepted` with one round recorded; red twice at budget 1 ends `accepted_unpublished` naming both heads; a person's push before round 1 means no round; non-force push rejected mid-round ends cleanly; hold tripped in a round pushes nothing; cancel mid-round leaves the pushed head intact.

### U6. Docs and the stock factory

- **Files:** `examples/definitions/factory.yaml`, `README.md`, `docs/quickstart.md`
- **Approach:** opt `factory.yaml` in with `on_fail: {run: build, budget: 2}`. The round then runs `commit-build`, so the fix is committed by the definition's own commit phase and then tested, reviewed and classified. The README gets a short section saying what a round re-runs, what it never skips, and that retry never repairs.

**Exit gate:** on a scratch GitHub repository whose CI runs a check the local `tests_pass` gate does not (for example `gofmt -l` failing on purpose), three real `factory.yaml` runs with Claude Code: one goes red and then green within budget, one exhausts the budget and ends `accepted_unpublished` naming both heads, and one has a person push during the CI wait and runs no round. All three have trace timelines that name every round.

---

## Risks & Dependencies

| Risk | Mitigation |
|---|---|
| Injection through CI logs into an agent with write access | Logs bounded and fenced as data (KTD5); write allowlist, env allowlist and ephemeral HOME unchanged; every downstream gate and reviewer re-runs before any push |
| An agent "fixes" CI by weakening the check (deleting a test, disabling a lint rule) | Same exposure as the existing test `on_fail → build` loop. The reviewers after `run` see the diff; `factory.yaml`'s correctness reviewer prompt gains a line on changes to tests and CI config. The risk classifier should score edits under `.github/` and test directories as not-low, which trips the hold |
| Cost: each round re-runs reviewers | Budget capped at 3 and shared ceiling and send count (R7); `factory.yaml` ships with 2 |
| Flaky CI burns rounds on code that is fine | `ci_repair_no_change` ends the attempt instead of forcing an edit; flaky re-run policy deferred |
| Continuation lifetime leaks memory in a long worker | `Release` on every path out of `PublishingRunner.Run`, asserted in U5 tests |
| Engine and worker coupling grows | The seam stays one interface in `worker`; the package-boundary check stays green |

**Dependencies:** none outside the repo; `gh` must be authenticated for Actions log reads, as it already is for `CommitChecks`.

## Sources & Research

- `internal/worker/publish.go`: `PublishingRunner.Run` (298), `publish` (380), `publishStep` (481), `awaitCI` (699), `CICheck` (178)
- `internal/engine/phase.go`: `Execute` (167), `run` (271), repair dispatch (532–601)
- `internal/protocol/definition.go`: `PublishSpec`, `CISpec`, `RepairEdge`, `validateRepairEdge` (375)
- `internal/protocol/publish.go`: step vocabulary and prerequisites, `PublishRetry.Snapshot`
- `internal/controlplane/store.go:555`: `publish_ci_required`
- Parent plan: `docs/plans/2026-08-05-001-feat-jig-software-factory-plan.md` (KTD2 repair edges, KTD6 fencing, KTD11 ephemeral HOME)
- Gergely Orosz, "Inside OpenAI's agentic software factory", The Pragmatic Engineer, 2026-09-15, stage 4. A direction only; it reports no quality outcomes.
