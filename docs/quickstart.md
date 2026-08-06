# jig quickstart

From a fresh clone to a browsable accepted run. Every command below is meant
to be typed verbatim; nothing here needs Node, Docker, or an account with
anyone.

**You need:** Go 1.26+, git, and one agent CLI (Claude Code or Codex) that is
already logged in. `just` is optional — every recipe has a plain `go`
equivalent.

---

## 1. Build

```sh
git clone https://github.com/StructuPath/jig
cd jig
go build -o bin/jig ./cmd/jig
./bin/jig version
```

If that printed a version, the whole toolchain requirement is satisfied. The
UI is committed and embedded, and the SQLite driver is pure Go, so there is
no second build step and no native dependency to install.

---

## 2. The smoke run

```sh
./bin/jig run --def examples/definitions/smoke.yaml . "what is this repository?"
```

Expected output — identifiers, a trace path, and a verdict:

```
run: ee31d5e7-baae-415d-b421-11d6b135419e
job: 91b61254-2b26-4cb2-90e2-b92da62d2884
attempt: e047d914-88de-44e6-87b8-eec988c516af
trace: ~/.jig/traces/e047d914-88de-44e6-87b8-eec988c516af.jsonl
outcome: accepted (publish: not_attempted)
acceptance:
  ok   all_phases_passed — 1 phase(s) passed
  ok   verdict_consistent — 3 check(s) verified
```

`echo $?` says `0`. That green run is the contract: the agent CLI is
installed and authenticated, jig can seed its credentials into an ephemeral
HOME, the phase engine ran, the envelope parsed, the gates verified their
claims, and the acceptance predicate held. If it is green, the whole path
works.

**If it is not green**, the exit code says which half failed:

| Exit | Meaning | First thing to check |
|---|---|---|
| `2` | usage or definition error | the message names the offending element |
| `1` | infrastructure | is the agent CLI on your `PATH` and logged in? |
| `3` | the agent's verdict was no | open the trace path it printed |

Common first-run failures:

- *“seed claude auth: no credentials file and no keychain item”* — log in
  with `claude`, or pass `--no-seed-auth` if your CLI authenticates from an
  environment variable (and add that variable to the role's `env` allowlist).
- *“seed codex auth: … is unreadable”* — run `codex login`, then
  `jig run --runtime codex …`. Also change the roster's `model:` values: the
  stock definitions name Claude Code models (`haiku`, `sonnet`), and the value
  is passed to whichever CLI you selected.

Everything the run did is in the trace file, one JSON event per line:

```sh
jq -r '[.seq, .type, .phase, .name] | @tsv' ~/.jig/traces/<attempt>.jsonl | head -20
```

For scripting (or for an agent driving jig), add `--json`:

```sh
./bin/jig run --json --def examples/definitions/smoke.yaml . "what is this repository?" | jq .
```

---

## 3. Write a definition

Start from a stock one. `scout.yaml` is read-only recon; `two-phase.yaml`
writes a file and verifies it with a code phase:

```sh
./bin/jig run --def examples/definitions/two-phase.yaml . "a note about steel"
git status --porcelain      # jig-note.txt, written by the build phase
git checkout .              # undo it
```

Now copy one and edit it:

```sh
cp examples/definitions/plan-build-test.yaml my-workflow.yaml
$EDITOR my-workflow.yaml
./bin/jig def validate my-workflow.yaml
```

Two things to change in any copied stock definition:

1. **The test phase's command.** The stock one detects `Justfile`,
   `Makefile`, `go.mod`, and `package.json`, and fails loudly when it finds
   none. Replace it with your repository's real command.
2. **The builder's `writes` allowlist.** `["**"]` permits every path in the
   worktree. It is the honest default for an example that must run anywhere,
   and the weakest write boundary jig can enforce. Narrow it to the
   directories the work belongs in — that allowlist is what rolls back and
   aborts an agent that goes somewhere it should not.

`jig def validate` is the same check the control plane applies at save time,
so a definition that validates here will save there. It runs offline, which
makes it a good pre-commit hook.

---

## 4. Bring up the control plane

```sh
./bin/jig serve
```

```
jig serve: listening on http://127.0.0.1:8383
```

Open that URL: the embedded UI is served from the same origin as the API.
`serve` binds loopback only and refuses anything else without
`--allow-non-loopback` — there is no authentication here, by design.

If `gh` is not installed or not authenticated, start it with
`--no-github-poll`: schedules keep firing, and only GitHub trigger polling is
turned off.

In a second terminal, save a definition and admit a run:

```sh
./bin/jig def create examples/definitions/two-phase.yaml
./bin/jig def list

./bin/jig def invoke <definition-id> \
    --instructions "write the note this repository is missing" \
    --repo "$PWD"
```

The run fans out into one job per target repository, each pinned to that
repository's HEAD at admission. Nothing runs yet: jobs are queued until a
worker claims them.

---

## 5. Run a worker

In a third terminal:

```sh
./bin/jig worker
```

```
jig worker: 4f1c…
  runtime: claude-code 2.1.4 (resume=true, cost=true)
  retained worktrees: 0, orphan paths: 0, missing: 0
jig worker: claiming from http://127.0.0.1:8383
```

Within a couple of seconds it claims the queued job, materializes a worktree
at the pinned SHA, runs the chain, and publishes. Watch it live in the UI:
one lane per attempt, phases and tool calls as they happen.

Publishing needs a GitHub remote and an authenticated `gh`. Against a local
repository the push succeeds and the pull request cannot be created, which
lands the job in `accepted_unpublished` — a distinct state with a
publish-only retry, not a failure: the phases passed and the work is on the
branch `jig/<job-id>/<attempt>`.

`Ctrl-C` stops the worker in order: no new claims, in-flight attempts
cancelled, terminal states recorded, ephemeral scratch destroyed.

---

## 6. Make it unattended

A trigger admits runs without you. A nightly scout:

```sh
./bin/jig trigger create \
    --def <definition-id> \
    --name nightly-scout \
    --kind schedule \
    --cron "0 3 * * *" \
    --timezone America/Denver \
    --repo "$PWD" \
    --instructions "survey this repository and report what changed"
```

Or one that works GitHub issues as they appear:

```sh
./bin/jig trigger create \
    --def <definition-id> \
    --name triage \
    --kind github_issue \
    --repo github.com/you/your-repo \
    --state open \
    --label jig \
    --instructions "implement the issue described in the context section"
```

```sh
./bin/jig trigger list --json
./bin/jig trigger disable <trigger-id>     # the stop button
```

Two counters on every trigger are worth watching. `admitted` is how many runs
it created. `skipped` is how many firings were dropped because the previous
run was still active — a schedule whose skip count climbs is firing faster
than its work finishes.

Issue and pull-request text arrives as *untrusted context*, held separate
from the operator-authored instructions the whole way through. That
separation, plus the ephemeral HOME and the write boundary, is what makes it
survivable to let a GitHub issue start an agent on your machine.

---

## Where to go next

- `README.md` — the operational notes: the permission posture, auth seeding
  for both runtimes, exit codes, the loopback stance.
- `examples/definitions/simple-sdlc.yaml` — the fullest worked example:
  review/revise repair, a conditional retest, per-phase commit messages.
- `docs/plans/2026-08-05-001-feat-jig-software-factory-plan.md` — why any of
  it is shaped the way it is.
