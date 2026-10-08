# Research: `jig job publish-retry` and definition-level build outputs

**Date:** 2026-10-08
**Status:** research record, not a plan. No requirements or KTDs are decided here.
**Method:** read-only code and docs survey of `main` at `b12821b`. Every claim
below cites the file and line it was read from; `[INFERENCE]` marks the one
claim that was reasoned rather than observed.
**Scope:** the two changes being planned —

- **A.** an operator CLI command `jig job publish-retry <job-id>` that releases a
  held job (`accepted_unpublished`) by running the worker-side publish retry;
- **B.** a way for a definition to declare build-output paths (e.g. `bin/**`)
  that ANY role may write without breaching the write boundary, so read-only
  reviewers can run the repo's own check command (`just check` writes
  `bin/jig`).

---

## 1. What the publish retry needs today

`Worker.RetryPublish` (`internal/worker/publish.go:1102-1163`) does six things:

| Step | Line | Dependency |
|---|---|---|
| mint a fresh lease token | `:1105` → `claiming.go:23-29` | none (local CSPRNG) |
| `POST /api/jobs/{id}/publish-retry` with `WorkerID` + `LeaseToken` | `:1109-1111` → `publish.go:1999-2005`, `protocol/publish.go:180-183` | reachable control plane |
| attach a lease, keep it alive in the background | `:1113-1116` | heartbeat fence: token ownership, no successor, leased state (`sweep.go:195-230`) |
| re-run `publish` with the ledger's proven steps, the stored `changed_paths`, and `ciPolicyFor(retry.Snapshot)` | `:1136-1139` → `publish.go:432-533`, `:902-916`, `:263-276` | control plane, git, `gh`, frozen snapshot |
| map onto a terminal state (`publishedOutcome`) and `completeAttempt` | `:1140-1144` | control plane |
| dispose the worktree only when published, otherwise retain | `:1145-1156` | manifests under the data dir |

### Needed

- **Worker identity equal to `attempts.worker_id`.** Anything else is refused as
  `publish_retry_foreign_worker` (`internal/controlplane/publish_ledger.go:388-390`).
  The identity is the persistent file `<data>/worker-id`
  (`internal/worker/registration.go:333-363`).
- **A worktree on disk — needed only by the push step.** The path is
  `<DataDir>/worktrees/<attempt-id>` when it exists (`publish.go:1132-1135`; root
  at `registration.go:501`). When the push is already proven, `publishStep`
  reuses the record and the worktree is never touched (`publish.go:540-543`);
  the push step itself refuses to run without one (`publish.go:597-601`,
  `publish_worktree_missing`). A **held** job never pushed, so the retained
  worktree — with the agent's uncommitted changes — is what the retry pushes.
  Retention deliberately survives a worker restart (`reconcile.go:5,175-183`;
  `worktree.go:160-168` retains a dirty worktree, or one whose commits are not
  on a remote ref).
- **A repository cache entry** under `<DataDir>/repos` (`publish.go:605-628`,
  `:712-750`; `repocache.go:61-141`); a cold cache re-clones from
  `protocol.CloneSourceForIdentity` (`internal/protocol/repository.go:87-106`).
- **The operator's ambient git credentials and `gh` auth.** Worker git runs with
  `os.Environ()` plus `GIT_TERMINAL_PROMPT=0` (`repocache.go:287-306`) — the
  operator's own credential helper, never the attempt's ephemeral HOME. `gh` is
  exec'd from `PATH` for pull requests and CI (`publish.go:1397-1399`, `:1419+`,
  `:1502+`), and its absence is reported with the fix (`publish.go:1424-1427`).
- **The frozen definition**, carried in the retry response
  (`protocol/publish.go:187-193`), for the CI policy only (`ciPolicyFor`,
  `publish.go:263-276`). The retry **never repairs**: it calls `publish`
  directly, not `repairCI` (`publish.go:1136-1139`; plan `2026-09-27-001` R11,
  line 39).

### Not needed

- **No agent phase, runtime adapter, engine, scratch directory, or ephemeral
  HOME.** `internal/worker/publish.go` imports only the standard library plus
  `internal/protocol` — no `engine`, no `runtime`.
- **No registration.** `Reconcile` requires a registered worker
  (`reconcile.go:100-103`); the retry path never calls it.
- **No continuation.** Retry-repair is deliberately impossible off-process
  (plan `2026-09-28-001` KTD1, lines 88-89), so nothing here needs one.

### Process binding

The binding is to the worker **instance's data directory**, not to the process:
the `worker_id` column (`publish_ledger.go:388-390`) plus the retained worktree,
cache, and manifests under one `--data` directory. The same PID is not required;
the same data directory is.

### Hazard in the current state

The store's retry transaction re-leases the attempt (`state='running'`, job
`active`, `publish_ledger.go:393-412`). Nothing in the codebase then runs
`Worker.RetryPublish`, so a bare API call leaves an attempt leased with no
publisher: the sweeper marks it `lost` and the job `failed` after three missed
heartbeats (`sweep.go:74-142,162`; `limits.go:14,25`). Any CLI change must close
this, and must not widen it.

---

## 2. Operator-facing paths today: none

- The only callers of `Worker.RetryPublish` are tests: `publish_test.go:583`,
  `publish_ci_test.go:184,234`, `publish_repair_test.go:306`,
  `publish_rerun_test.go:559`. `Client.RetryPublish` has exactly one caller:
  `Worker.RetryPublish`.
- Nothing under `cmd/`, `web/src/`, `scripts/`, or `docs/` reaches
  `publish-retry`. The UI's retry is the **cold** path — `api.ts:88-89` →
  `POST /api/jobs/{id}/retry` → `Store.RetryJob` (`controlplane/store.go:606`)
  — alongside `releaseWorktree` (`api.ts:83-84`,
  `controlplane/worktree_ledger.go:128-165`) and `cancelJob`.
  `RunDetail.tsx:140-146` renders only "Retry same workflow" (failed) and
  "Cancel" (queued/active), and `RunDetail.test.tsx:271-279` asserts exactly
  that set: an `accepted_unpublished` job has **no** button.
- Docs claim one anyway:
  - `docs/dogfooding.md:368-370` — "A publish-only retry is
    `POST /api/jobs/<job-id>/publish-retry`, **or the button in the UI**. It
    judges CI on the branch's current head, and it never repairs." The button
    does not exist.
  - `docs/quickstart.md:219-225` ("a distinct state with a publish-only retry";
    "then the publish retry is your sign-off") and `:233-235` ("push a fix to
    the `jig/<job-id>/<attempt>` branch yourself and the publish retry re-checks
    CI on the new head").
  - `README.md:281-284`, `:297`, `:316-317`, `:349`, and `:175` in the sample
    definition ("the publish-only retry is the human sign-off that releases
    it", "The publish-only retry judges CI but never repairs").

So change A is the first operator path for an action the docs already describe.

---

## 3. CLI conventions and the best template

- **Dispatch:** `cmd/jig/main.go:56-90`, a plain `switch args[0]` over
  `start|run|serve|worker|def|trigger|report|version|help`; usage at
  `main.go:22-36`. There is **no `job` group and no `retry`/`cancel` CLI**.
- **Grouped subcommands** (the shape `jig job <verb>` should copy):
  `defCommand`, `triggerCommand` switches at `def.go:52-…` and `:390-410`;
  an unknown verb is a usage error.
- **Shared plumbing:** `defFlags` (`def.go:576-583`) supplies `--server`
  defaulting to `"http://" + defaultServerAuthority` and `--json`;
  `defaultServerAuthority = "127.0.0.1:8383"` (`worker.go:187`); `emitJSON`
  `def.go:585-597`; `apiCall` `def.go:599-644`; `apiRejection` turns the store's
  `{code,message}` back into its actionable form (`def.go:646-661`).
- **Exit codes:** `run.go:66-69` — `0/1/2/3` (accepted / infrastructure / usage /
  rejected). `report.go:5-6` uses only `0/1/2` because it has no verdict. A
  publish retry does have a verdict (published versus still held or still red),
  so `0` and `3` are both meaningful.
- **Best template: `cmd/jig/worker.go`**, not `run.go`. `workerCommand` already
  builds exactly what `Worker.RetryPublish` needs: `defaultDataDir()` and the
  `--data` default `~/.jig/worker` (`worker.go:80-90`),
  `worker.New(Config{ServerURL, DataDir, Name, …})` (`worker.go:126-141`),
  `worker.NewGitHubCLIGateway()` with `PublishOptions` (`worker.go:108-116`),
  and `publisher.Bind(host)` (`worker.go:144`). `worker.go:40` is the only
  `cmd/jig` file importing `internal/worker` today. `run.go` imports
  `controlplane`/`engine`/`runtime` only (`run.go:19-27`) and has no worker at
  all: it is the right model for flag/JSON/exit conventions, the wrong one for
  worker-like behaviour.

---

## 4. Ask a running worker, or run the retry in the CLI?

**Neither existing channel can deliver it.** The heartbeat response carries only
lease expiry and cancellation (`protocol/types.go:380-383`); claims select
`jobs.state='queued'` / `attempts.state='queued'` only
(`controlplane/claim.go:150-151,210,221,234`), and a re-leased attempt is
`running`, so no claim can pick it up. The production loops are a registration
ticker and a claim ticker (`cmd/jig/worker.go:203-240`); reconcile runs once at
startup (`worker.go:165-175`).

**There is a precedent for control-plane → worker actions, and it is
reconcile-time.** Retained-worktree release is an operator POST (`http.go:64`,
`worktree_ledger.go:128-165`; UI `web/src/Worktrees.tsx:1-80`) that the worker
applies on its next reconcile (`reconcile.go:163-178`).

Architecture:

- **KTD1** — "`internal/worker` may never import `internal/controlplane`"
  (plan `2026-08-05-001` line 68; enforced by `Justfile:39-40`) — is not
  violated by either option. `cmd/jig` may import `internal/worker` and already
  does (`worker.go:40`).
- **Fencing and leases** (KTD6, plan line 76; R5 line 34; R6): the retry
  re-leases *the same attempt* and supersedes any earlier token
  (`publish_ledger.go:329-340`), so both options are fenced identically by the
  control plane. The difference is liveness. A CLI that dies mid-publish has no
  heartbeat owner, so the attempt is swept `lost` and the job `failed`, and the
  zombie residue KTD6 cannot prevent is left for `StrayPublishBranches` to
  surface (`publish.go:36-42`, `:1211+`). A running worker has the drain path
  (`workerDrainGrace`, `cmd/jig/worker.go:56-59`).
- **Practical decider:** the CLI must open the *same* `--data` directory as the
  worker and would share `worktrees/`, `manifests/`, and `repos/` with a live
  worker process; those mutexes are per-process. The only real fence is the
  control-plane lease, so in-CLI execution is only safe when the worker is
  stopped or the two never touch the same attempt. **In-process in the CLI** is
  the cheaper fit today (no protocol change, no new worker loop); a
  **heartbeat/reconcile-picked flag** is architecturally cleaner and matches the
  worktree-release precedent, but needs a new protocol field plus a worker loop
  handler and answers for capacity semantics.

---

## 5. Write boundary: validation, enforcement, and where build outputs plug in

### Validation today

**`writes` is not validated at all.** `RoleSpec.validate`
(`internal/protocol/definition.go:582-608`) checks model, effort, budget, and
prompts only — no pattern syntax, no `**` rejection, no dangerous-prefix check.
Unknown YAML keys are rejected by `Decoder.KnownFields(true)`
(`definition.go:336-347`), so a new field must be added to `DefinitionSpec`
(`definition.go:70-78`). `writes: nil` is unrestricted and `writes: []` is
read-only (`boundary.go:356-368`; test `boundary_test.go:105`).

### Enforcement (`internal/engine/boundary.go`, R10)

- `snapshotTree` (`:79-121`) fingerprints tracked-against-HEAD
  (`git diff HEAD --name-only -z --no-renames`), untracked non-ignored files, and
  **ignored-but-present** paths via
  `ls-files --others --ignored --exclude-standard --directory -z` (`:107-119`).
  A wholly ignored tree collapses to **one entry with a trailing slash** —
  verified on this repository, where the same command reports `bin/`,
  `.claude/`, `web/node_modules/`. `.git`, hooks, and config live in a separate
  absolute-path map, `snapshotGitMeta` (`:145-186`).
- `enforceBoundary` (`:447-475`) diffs the snapshot and sends every changed path
  through `writePermitted`; a violation goes to `rollBackPath` (`:399-431`) with
  one of these outcomes: `rolled back` (restore from HEAD), `deleted`,
  `reverted-by-agent (uncommitted work lost, cannot restore)`, and
  **`left as-is (was already modified)`** (`:405-413`) — a breach on a path that
  was already dirty is reported but cannot be undone. The attempt aborts on any
  breach regardless of outcome (`phase.go:1116-1131`), so that outcome is
  evidence, not tolerance.
- `gitMeta` is outside every allowlist by construction — even `writes: ["**"]`
  (`boundary.go:463-476`; tests `boundary_test.go:379,407`).
- Call sites: the agent phase at `phase.go:1111` (through
  `enforceWriteBoundary`, `:1105-1131`) and `:1345` (passing `role.Writes`);
  residual touched paths accumulate at `:1350-1351` into `changed_paths`
  (`:452,459`). A crashed phase is rolled back by `restoreSnapshot`
  (`boundary.go:481-503`), called at `phase.go:1218`. A parallel group takes one
  snapshot before all members and enforces once after, with a **hardcoded empty
  allowlist** (`parallel.go:166`, `:306`, `:318`): members are read-only by
  construction, and validation forces it (`definition.go:450-456`; R8 doc
  `definition.go:88-96`).

### Where build outputs plug in

1. **Protocol.** A definition-level list (e.g. `build_outputs`) on
   `DefinitionSpec` (`definition.go:70-78`), with a `Validate` rule
   (`definition.go:352`). The engine sees it as `e.spec` (`phase.go:237`).
2. **Engine.** The effective allowlist becomes `role.Writes ∪ spec.BuildOutputs`
   at `phase.go:1345` **and** at `parallel.go:306/318`, replacing the hardcoded
   `[]string{}`; `restoreSnapshot` at `phase.go:1218` needs an explicit decision
   (today it rolls back everything a dead phase introduced, which would delete a
   build artifact).
3. **Nil-versus-empty.** `writePermitted` treats `nil` as unrestricted
   (`boundary.go:356-368`), so the union must be built such that a `writes: []`
   role receives exactly the build-output list and never `nil`.
4. **The collapsed directory entry.** `compileGlob("bin/**")` produces
   `^bin/.*$` (`boundary.go:318-338`), which matches the collapsed `bin/` entry
   (empty tail). A test should pin this rather than assuming it.

### What must stay forbidden, or this is a bypass

- **Git metadata, hooks, `.git`** — never route build-output globs into the
  absolute-path map (`boundary.go:463-476`, `:145-186`).
- **Tracked files.** Nothing today stops `writes: ["**"]` from rewriting the
  whole repository; a build-output list must not become a second unrestricted
  allowlist for `writes: []` roles. The permission must be
  `matches(glob) && !existsInHEAD(path)` (`existsInHEAD` `boundary.go:380-386`).
- **Save-time pattern hygiene** — refuse empty, absolute, `..`, leading-`-`, and
  `:`-prefixed patterns, and literal `.git` prefixes; the same class
  `validateChangedPaths` guards for publish (`publish.go:919-943`). The
  tracked-file check is the runtime half and cannot be done at save time.
- **Publish interaction.** Touched paths become `changed_paths` (`phase.go:1350-1351`,
  `:452`) and publish stages them (`publish.go:631-693`);
  `validateChangedPaths` permits ignored paths (`:919-943`) and
  `stagingEscapes` already treats a declared directory as covering its children
  (`:945-977`), so a collapsed `bin/` entry is covered. `git add -- bin/` on an
  ignored path is refused by git without `-f` `[INFERENCE]` — confirm at
  implementation time. A definition whose phases produce only gitignored build
  output ends `publish_empty_changeset` (`publish.go:676-680`).
- **The read-only claim weakens.** R8's message — "every member's role must
  declare `writes: []`" (`definition.go:452-456`) — and README's review-panel
  wording need revisiting, because a member could then write declared build
  outputs.

---

## 6. Glob syntax and what validation rejects

- `writes` uses `compileGlob`/`matchesPattern` (`boundary.go:318-354`): `**`
  becomes `.*` and crosses directories, `*` becomes `[^/]*` and stops at `/`,
  `?` becomes `[^/]`, a trailing `/` is a directory prefix (`strings.HasPrefix`),
  and a pattern with no glob characters is an exact path. Pinned by
  `boundary_test.go:77-104`.
- **No dangerous-glob rejection exists for `writes`.** The only glob validator in
  the protocol package is `validateGateAllow` (`definition.go:805-824`) for
  `tests_intact`'s `allow`: entries must be non-empty, the gate must be
  `tests_intact`, `**` is refused (because `path.Match` would silently narrow
  it), and `path.Match` must parse. That is the precedent to copy for a
  `build_outputs` validator.

---

## 7. Tests to model on

- **Boundary:** `internal/engine/boundary_test.go` —
  `TestMatchesPatternSemantics:77`, `TestWritePermittedDistinguishesNilFromEmpty:105`,
  `TestEnforceBoundaryRollsBackOnlyWhatTheAgentIntroduced:137` (the
  "left as-is" assertion is `:166-169`),
  `TestRestoreSnapshotUndoesACrashedPhase:172`,
  `TestStagedAndUnstagedRenamesInsideTheAllowlistAreLiteralPaths:199`,
  `TestRenameOutOfTheAllowlistBreachesAndRollsBack:238`,
  `TestWritesToGitignoredPathsAreDetectedAndRolledBack:351` (the `buildcache/`
  case at `:372-377` is exactly the `bin/` shape),
  `TestPlantingAGitHookIsABreachEvenWithAnUnrestrictedAllowlist:379`,
  `TestRewritingGitConfigIsABreachThatCannotBeRestored:407`,
  `TestJigSideGitCommandsDoNotRunRepositoryHooks:436`.
- **Group boundary:** `internal/engine/parallel_test.go:424`
  `TestAMemberWriteIsABreachThatAbortsWithNoPartialMerge`, `:453`
  `TestADeadMembersLeftoverWriteIsABreach`, `:666`
  `TestAPanickingMemberStillHasItsSiblingsWritesRolledBack`, `:803`
  `TestABreachOutranksTheSendBudgetThatEndedTheGroup`; engine-level allowlist
  abort `phase_test.go:551`
  `TestOutOfAllowlistWritesRollBackAndAbortTheAttemptWithoutRetry`.
- **Definition validation:** `internal/protocol/definition_test.go` —
  `:327 TestTestsIntactAllowIsValidatedAtSaveTime`,
  `:368 TestUnknownYAMLFieldIsRejectedAtParseTime`, `:699/:712/:730` for the
  parallel group, `:599 TestNegativeRoleBudgetIsRejected` as the pattern for a
  new save-time rejection; stock definitions stay honest through
  `cmd/jig/definitions_test.go:27` and `Justfile:36-37`.
- **CLI:** `cmd/jig/worker_test.go:27`
  `TestWorkerRegistersClaimsRunsPublishesAndReapsItsScratchOnInterrupt` is the
  closest end-to-end template (helpers `startServe` `serve_test.go:43`,
  `scriptRuntime` `run_test.go:46`, `envelopeJSON` `run_test.go:104`,
  `awaitTerminalJob` `worker_test.go:154`, `stockDefinition`
  `definitions_test.go:21`, `syncBuffer` `serve_test.go:24`);
  `worker_test.go:182 TestDefCommandsCreateListShowAndInvokeAgainstTheAPIWithJSONOnEveryRead`
  for flag/JSON/exit conventions; `report_test.go:178,219` for flag parsing and
  usage exits; `run_test.go:300` for documented JSON fields and distinct exit
  codes.
- **Publish retry:** `internal/controlplane/publish_ledger_test.go:204`
  `TestPublishOnlyRetryReLeasesTheSameAttemptWithItsProvenSteps` (the
  foreign-worker refusal is `:228-232`), `:294`
  `TestPublishRetryIsRefusedForJobsThatAreNotAcceptedUnpublished`, `:411`
  `TestPublishRetryCarriesTheRunSnapshot`; worker side
  `internal/worker/publish_test.go:538`, `:638`, `:930`, and
  `publish_ci_test.go:183`.

---

## Open decisions

1. **Delivery (A):** in-CLI execution sharing the worker's `--data` directory
   versus a control-plane flag the running worker picks up. See §4; the
   liveness/drain gap is the deciding difference.
2. **Command shape (A):** a new `jig job` group versus a top-level verb; §3
   gives the grouped-subcommand template and the exit-code convention.
3. **Scope of build outputs (B):** whether the list applies only to agent
   phases (where the boundary runs) and whether a crashed phase's build output is
   preserved or rolled back (`restoreSnapshot`, §5).
4. **Tracked-file refusal (B):** confirm `!existsInHEAD` is part of the
   permission, not just of save-time validation, so a build-output glob cannot
   become a second unrestricted allowlist.
5. **Docs:** `docs/dogfooding.md:368-370` currently claims a UI button that does
   not exist; correct it as part of A.
