# jig

**A local-first software factory.** One Go binary that runs repeatable,
phased coding-agent workflows against Git repositories: you declare a
workflow once, and jig executes it with structure — phases, typed
hand-offs, gates that verify claims, bounded repair loops, an enforced write
boundary, and a durable record of everything that happened.

The problem it solves: a coding agent invoked as one opaque prompt is done
when it stops talking. jig makes "done" a property of the work — every phase
passed, the acceptance predicate held, and the change was published with
remote proof — and makes the whole run inspectable while it happens.

Local-first by design: loopback HTTP, one trusted operator, SQLite, no
service to sign up for. Delivery is GitHub-coupled in v1 — publish means a
branch and a pull request via `gh`.

```
definition (YAML)  →  run (frozen by value)  →  job per repository  →  attempt
                                                                        │
                              phase → phase → phase, each gated ────────┘
```

---

## Install

**From source** (Go 1.26+, git, and one agent CLI):

```sh
git clone https://github.com/StructuPath/jig
cd jig
just build          # or: go build -o bin/jig ./cmd/jig
```

Node is never required: the UI is committed and embedded, and the SQLite
driver is pure Go — which is also why one command cross-compiles every
supported platform (`just release`).

**From a release** (no Go toolchain needed) — pick your platform, check it
against the published checksums, and put `jig` on your `PATH`:

```sh
VERSION=v0.2.0
OS=$(uname -s | tr '[:upper:]' '[:lower:]')          # darwin or linux
ARCH=$(uname -m | sed 's/x86_64/amd64/; s/aarch64/arm64/')

BASE=https://github.com/StructuPath/jig/releases/download/$VERSION
curl -fsSLO "$BASE/jig_${VERSION}_${OS}_${ARCH}.tar.gz"
curl -fsSLO "$BASE/SHA256SUMS"

sha256sum -c SHA256SUMS --ignore-missing            # macOS: shasum -a 256 -c
tar -xzf "jig_${VERSION}_${OS}_${ARCH}.tar.gz"
./jig version
```

Verify the checksum before you run the binary, not after — that line is the
only thing standing between a tampered download and an executable you are
about to trust with your repositories.

You also need at least one agent CLI, authenticated:

| Runtime | `--runtime` | Auth | Roster `model:` values |
|---|---|---|---|
| Claude Code | `claude-code` (default) | `claude` logged in | `haiku`, `sonnet`, `opus`, … |
| Codex | `codex` | `codex login` | Codex model ids, e.g. `gpt-5.6-sol` |

Model aliases (`haiku`, `sonnet`, `opus`) resolve inside the CLI to the
newest model it knows in that tier, so keep the CLI current: `jig run` and
`jig worker` warn at startup when Claude Code is older than 2.1.280, the first
release that serves Opus 5.5.

One process runs one runtime: the engine holds a single adapter per attempt,
so `--runtime` is an operator choice, not a per-role one. The `model:` value
in a definition's roster goes straight to that CLI, which is why the stock
definitions (written against Claude Code) need their models renamed before
they run on Codex.

---

## The 60-second version

```sh
jig run --def examples/definitions/smoke.yaml . "what is this repository?"
```

That is the install check: one genuinely read-only agent phase that validates
and passes on any repository, with no server, no worktree, and no publish. It
prints the attempt's trace path and its verdict. `docs/quickstart.md` walks
the whole path from here to a browsable accepted run.

---

## The four surfaces

**`jig run`** — the direct harness. Loads a definition file, freezes a run
against the local repository at its current HEAD, executes the chain
in-process, prints the trace path. No standing server, no publish. This is
the day-one loop and the fastest way to iterate on a definition.

**`jig serve` + `jig worker`** — the server path. `serve` is the control
plane: HTTP API, embedded UI, lease sweeper, and the admission loop (cron
schedules and GitHub polling) over one SQLite database. `worker` claims
queued work, materializes an isolated worktree per attempt, runs the chain,
and publishes accepted work as a branch and pull request.

**`jig def` and `jig trigger`** — authoring and admission. Validate a
definition offline, save it, invoke it, and stand up the schedules and GitHub
polls that invoke it unattended. Every read command takes `--json`.

**`jig report`** — measurement. Jobs, accepts, publish outcomes, CI repair,
re-runs, and spend over a time window, read from the control plane's ledger.

---

## Authoring a definition

A definition is data, not a script. Five stock ones ship in
`examples/definitions/`:

| Definition | What it demonstrates |
|---|---|
| `smoke.yaml` | the install check: one read-only phase, repo-independent |
| `scout.yaml` | read-only recon: two agent phases and the hand-off between them |
| `two-phase.yaml` | an agent phase that writes, verified by a code phase |
| `plan-build-test.yaml` | a **code-phase repair edge**: a red suite routes back to the builder |
| `simple-sdlc.yaml` | an **agent-phase repair edge** (review → revise → re-review), a conditional retest, and per-phase commit messages |
| `factory.yaml` | the **software factory**: plan → build → commit → test → a panel of specialist reviewers looping the builder until they approve, then a deterministic **risk gate** that holds high-risk work for a person |

The shape:

```yaml
name: my-workflow
roster:                      # one entry per agent role
  builder:
    model: opus
    effort: medium           # optional: low | medium | high | xhigh | max
    budget_usd: 8            # optional: cap on what one send may spend
    system_prompt: |         # or system_prompt_path: <repo-relative file>
      You are a builder …
    user_prompt: |
      Task: {{prompt}}       # the invocation's prompt, frozen at admission
    env: [PATH]              # env allowlist — the ONLY variables it receives
    writes: ["src/**"]       # write allowlist; [] is read-only, omitted is unrestricted
phases:
  - name: build
    kind: agent              # agent | code
    owner: builder
    gates:                   # claim verifiers, run after the phase
      - {name: artifacts_exist}
      - {name: diff_matches_claims}
  - name: test
    kind: code
    command: "go test ./..."
    on_fail: {run: build, then: rerun-self, budget: 2, exhausted: fail-job}
  - name: retest
    kind: code
    if: revised              # skipped unless a previous envelope set it truthy
    command: "go test ./..."
  - name: classify-risk
    kind: code
    reports_fields: true     # the last output line is a JSON object of envelope fields
    command: "scripts/risk.sh"
  - name: review-risk
    kind: agent
    owner: reviewer
    if: "risk == high"       # a comparison guard, same language as on_fail.when
acceptance: [all_phases_passed, diff_matches_claims]
publish:
  hold_when: "risk == high"  # accepted but held for a person; publish retry releases it
  ci: {wait: true, timeout: 30m}  # accepted only once CI on the PR head is green
```

Rules worth knowing before you write one:

- **Envelopes are the only inter-phase contract.** Every agent phase ends
  with one JSON object carrying at least `status` (`success` or `fail`).
  Extra fields — `approved`, `changed_files`, `revised` — are what gates,
  `if:` guards, and repair predicates read. Code phases get an adapter
  envelope built from their exit status, so a failing test suite enters the
  repair loop through the same door as a failing agent report.
- **A verdict is stated, never inferred.** Under `verdict_consistent` a
  reviewer must write `approved` as a JSON boolean; omitting it is its own
  gate failure, not a silent rejection. The distinction matters because
  `on_fail: {when: "approved == false"}` is how a review routes work back to
  a builder, and a verdict nobody stated must not decide that either way.
- **Gates verify claims, they do not judge quality.** The registry is
  `artifacts_exist`, `files_non_empty`, `diff_matches_claims`,
  `verdict_consistent`, and `tests_pass(command)`. Repo-specific
  verification is a code phase, not a new gate.
- **Repair loops must be declared and bounded.** `on_fail` is the only loop
  construct; a cycle or a missing budget is rejected at save time, not
  discovered at 2 a.m.
- **Effort is per role, and optional.** `effort` goes to Claude Code as
  `--effort`, and to Codex as `model_reasoning_effort` (Codex tops out at
  `xhigh`, so `max` runs there). It sets how much a role thinks and verifies,
  and thinking is billed as output. A useful split: `medium` for a builder
  working to a plan, `high` for a reviewer hunting the edge cases the build
  missed. Omit it to keep the CLI's default for the model, and omit it for
  `haiku`, which does not take an effort level.
- **Spend is capped per role, and optional.** `budget_usd` is the most one
  send by that role may cost; Claude Code enforces it as `--max-budget-usd`.
  Codex has no such flag, and a cap it cannot enforce fails the send rather
  than run uncapped — the same posture as a `tools` allowlist there.
- **Code phases can report.** A code phase with `reports_fields: true`
  prints a JSON object as its last output line, and those fields join the
  envelope view that `if:` guards and `publish.hold_when` read. It is how a
  deterministic script — a risk classifier scoring paths and diff size — gets
  a say in routing without a model in the loop. The adapter's own fields
  (`status`, `passed`, `exit_code`, …) are reserved, and a reported field is
  protected: a later agent envelope cannot overwrite it. Code phases and
  gates receive `JIG_BASE_SHA`, the commit the run was pinned to, so a script
  can diff the whole change without guessing a base from history.
- **Guards compare as well as test.** `if: revised` runs a phase when a
  previous envelope set the field truthy; `if: "risk == high"` runs it when
  the comparison holds, in the same `<field> ==|!= <literal>` language as
  `on_fail.when`.
- **Publish can be held.** `publish: {hold_when: "<predicate>"}` is judged
  after acceptance passes. When it holds, the attempt ends
  `accepted_unpublished` with the hold recorded, nothing is pushed, and the
  worktree and branch are retained — the publish-only retry is the human
  sign-off that releases it. Low-risk work never waits. Write the predicate
  to fail closed — `risk != low` holds work whose risk was never reported,
  where `risk == high` would ship it. The sign-off is only as strong as
  access to the control plane: jig has one trusted operator and no
  authentication (see *Loopback only*), so the operator's retry is the
  approval, with no separate approver identity.
- **Publish can wait for CI.** `publish: {ci: {wait: true, timeout: 30m}}`
  adds a fourth publish step after proof: the worker polls the check runs
  and commit statuses on the branch's head through `gh api`, and the job is
  `accepted` only when every one is green — the control plane enforces this
  from the frozen definition, not the worker's word. A red check fails fast;
  checks still pending at the timeout (default 30m, 1m–6h) fail too; a head
  with no checks at all passes after two minutes, since that repository runs
  no CI. Any of these ends the job `accepted_unpublished` with the branch and
  pull request in place and the red checks named in the result. Fix the
  branch — push to it yourself — and the publish-only retry judges CI on
  whatever its head is by then. Neutral and skipped checks count as green.
  The wait holds a worker slot, and a cancel during it ends the job the same
  way. Omit `ci` and publish does not wait, exactly as before.
- **Red CI can be repaired inside the attempt.** `publish: {ci: {wait: true,
  on_fail: {run: build, budget: 2}}}` gives red CI back to the `run` agent
  phase instead of ending the job. A round hands it the failing checks and
  the last 16 KiB of each failed GitHub Actions job's log (at most four logs),
  framed as data, never instructions; then **every phase after `run`** runs
  again, gates and repair edges included, and acceptance and `hold_when` are
  judged anew. Only a fix that passes all of that, is not held, and actually
  changed something is pushed, non-force, to the same pull request, and CI is
  awaited again. There is no way to skip phases: `run` must be an agent phase
  and not the last one. Rounds share the attempt's send budget and wall-clock
  ceiling, run in a freshly wiped ephemeral HOME, and are fenced by the lease
  again right before each push. jig never builds on someone else's commit: if
  CI is red on a head it did not push, no round runs. When the budget runs
  out, or a round fails, is held, is cancelled, or changes nothing, the job
  ends `accepted_unpublished` exactly as red CI did before, with every round's
  heads, changed paths, and outcome in the result. The publish-only retry
  judges CI but never repairs — a continuation lives only in the process that
  ran the chain. Budget is 1–3. A fix that weakens a check instead of the
  code is the risk to design for: `factory.yaml` scores edits to CI or lint
  configuration, and test files that lose more lines than they gain, as not
  low, so such a "fix" is held for a person.
- **A read-only review panel can run in parallel.** `parallel:
  [review-correctness, review-security, review-maintainability]` runs those
  phases at once, as one step of the chain. It is opt-in and narrow: one
  group per definition, two or more consecutive agent phases, each with its
  own role, every role `writes: []`, and no member's `if:` guard reading a
  field a sibling reports. Each member runs in its own ephemeral HOME and
  session and is handed the envelope from BEFORE the group, never a
  sibling's; results merge in declared order, and the next phase gets the
  last member's envelope. Any worktree change while the group runs —
  including one a crashed member left — rolls back and aborts the attempt.
  Rejections resolve after every member finishes: the first member in
  declared order that rejected with budget left dispatches its repair target
  and charges only its own budget, and then the whole group runs again, so
  earlier approvals are re-judged. The group runs at most 1 + the sum of its
  members' budgets times. `examples/definitions/factory-parallel.yaml` is the
  stock factory with its panel grouped; `factory.yaml` itself stays
  sequential until the parallel panel has been watched on real work.
- **Validation happens before anything runs.** `jig def validate <file>`
  is the same check the store applies at save time, offline.

```sh
jig def validate examples/definitions/simple-sdlc.yaml
jig def create examples/definitions/simple-sdlc.yaml
jig def list --json
jig def invoke --instructions "add a --version flag" --repo github.com/you/repo <definition-id>
```

---

## Running a worker

```sh
jig serve                                   # terminal 1
jig worker --runtime claude-code            # terminal 2
```

The worker registers (advertising its capacity, its runtime's probed version
and capabilities, and the *names* — never values — of its environment
variables), reconciles its worktrees against the control-plane ledger, and
then claims. Each attempt gets its own worktree at the run's pinned base SHA
on branch `jig/<job-id>/<attempt>`, and accepted work is published there:
push, find-or-create the pull request by head ref, then verify the **remote**
ref as proof.

Two things a worker prints at startup deserve reading: orphan worktrees
(reported, never deleted) and stray publish branches — attempt-scoped
branches on a remote that no fenced push record explains, which is the
visible residue of a zombie attempt that pushed after its lease expired.

Once it is running, the UI's **Fleet** view (`/fleet`, or `GET /api/workers`)
is where you check on it: which workers are registered, which agent runtime
each one owns, how many attempt slots are busy, and whether the control plane
still counts a worker live. That last one is the usual answer to a queue that
will not drain — a worker whose heartbeat lapsed is skipped by the claim
transaction, and the view reports that transaction's own verdict rather than
a second opinion.

`Ctrl-C` is orderly: it stops new claims, cancels in-flight attempts, records
their terminal state, and destroys their ephemeral scratch.

---

## Measuring the factory

```sh
jig report                          # the last seven days
jig report --since 24h
jig report --since 2026-09-01T00:00:00Z --until 2026-09-15T00:00:00Z
jig report --json                   # the GET /api/report object, verbatim
```

`jig report` reads the control plane's ledger over a window and changes
nothing. A job is in the window when it is terminal and was last updated in
`[since, until)`. `--since` takes a duration back from now (`7d`, `24h`,
`90m`) or an RFC3339 timestamp; `--until` takes a timestamp and defaults to
now. The prose form prints one short table per section:

| Section | What it counts |
|---|---|
| Jobs | terminal jobs by state |
| Accepts | clean accepts (green CI on a head jig pushed, or a definition that does not wait for CI), person-fixed accepts (green CI on a head jig did not push), and accepts the ledger cannot classify |
| Publish | publish outcomes of accepted work, the held rate, and failure codes |
| CI repair | attempts that waited for CI, first-pass greens, repair entry and success rates, rounds, stop codes, and the `ci_timeout` and retried-still-red rates |
| Re-runs | flaky-check re-runs, and passes that came only after one |
| Spend | total agent spend, spend per clean accept, and each outcome's share |

A rate with nothing to divide by prints `n/a`, never `0%`.

Two limits are printed with every report. jig does not watch CI after an
accept, so there is no post-merge CI rate. And spend is a floor: a send
killed before it returned a result has no cost to record (the report counts
these as unmetered sends), and spend recorded by jig v0.2.0 or earlier
omits phases that did not pass.

---

## Operational notes

These are the things that will surprise you if nobody says them out loud.

### The agents run with permissions bypassed

jig invokes Claude Code with `--permission-mode bypassPermissions` and Codex
with `--dangerously-bypass-approvals-and-sandbox`. There is no interactive
approver in an unattended run, and a CLI that stops to ask is a CLI that
hangs until the watchdog kills it.

**jig's own write boundary is the containment layer, not the CLI's prompts.**
Before every agent phase jig fingerprints the worktree's change-set; after
it, it compares. Anything the role's `writes` allowlist does not permit is
rolled back and the attempt aborts — never retried. Reverts count as
modifications, renames are decomposed into literal paths, and gitignored
files, the `.git` directory, and hook paths are all watched, because a
planted hook would execute during jig's own later git commands.

What follows from that: **the `writes` allowlist is the security control you
actually configure.** `writes: ["**"]` (the stock builders' default, so the
examples work anywhere) is the weakest boundary jig can enforce. Narrow it.
And network egress is not restricted in v1 — a per-role `tools` allowlist is
the control there.

### Agents run in an ephemeral HOME, so auth must be seeded

Every attempt's subprocesses get a jig-created `HOME` and XDG directory tree
containing no operator credentials: `~/.ssh`, `~/.config/gh`, and
`~/.gitconfig` are unreachable, and anything an agent writes "to its home"
dies with the attempt. The environment is composed from the role's `env`
allowlist alone — never your shell's environment.

That containment is also why the CLIs cannot log in on their own, so jig
seeds each one's auth material into that HOME:

| Runtime | Seeded from | Into the ephemeral HOME |
|---|---|---|
| Claude Code | on macOS the keychain item `Claude Code-credentials` (where the CLI keeps its live login), falling back to `~/.claude/.credentials.json`; elsewhere that file; plus `~/.claude.json` onboarding state | `~/.claude/.credentials.json`, `~/.claude.json` |
| Codex | `$CODEX_HOME/auth.json` (default `~/.codex/auth.json`), plus `config.toml` when present | `$HOME/.codex/auth.json`, `$HOME/.codex/sessions/` |

Two consequences:

- **`--no-seed-auth`** turns seeding off, for a CLI that authenticates
  through its environment instead (put the variable on the role's `env`
  allowlist — e.g. `ANTHROPIC_API_KEY`).
- **Do not put `CODEX_HOME` on a role's `env` allowlist.** Codex resolves
  its thread rollouts under `$CODEX_HOME/sessions`, and the location must
  stay stable across the sends of one attempt or an id-keyed resume finds no
  rollout. Allowlisting it points the agent back at your real credential
  directory and at sessions that outlive the attempt.

### Exit codes

Every command uses the same four, so `jig` is scriptable:

| Code | Meaning |
|---|---|
| `0` | accepted — the work passed and the verdict is yes |
| `1` | infrastructure failure — jig could not deliver a verdict |
| `2` | usage or definition-validation error |
| `3` | rejected — jig worked and the answer was no (including a cancelled run) |

`1` and `3` are deliberately distinct. "The agent's work was rejected" and
"jig broke" require different responses, and conflating them makes every
wrapper script wrong.

### Loopback only

`jig serve` binds `127.0.0.1` and refuses a non-loopback address without
`--allow-non-loopback`. There is no authentication and no TLS: the control
plane assumes one trusted operator on one machine. State-changing routes also
reject requests whose `Origin` is not the server's own, which is what stops a
web page you happen to be visiting from driving your control plane through
your browser.

---

## Development

```sh
just check        # format, vet, package boundary, definitions, tests, build
just test-race    # the concurrency suites under the race detector
just release      # cross-compiled binaries into dist/
```

The plan this repository was built from is
`docs/plans/2026-08-05-001-feat-jig-software-factory-plan.md`. It carries the
requirements (R1–R21) and key technical decisions (KTD1–KTD12) that the code
comments cite by name.

To trial the factory on a real repository and judge it by the numbers, follow
[`docs/dogfooding.md`](docs/dogfooding.md): preflight, a spend-ceiling check
before each batch, and a results table filled from `jig report`.

---

## License

MIT — see [`LICENSE`](LICENSE).
