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

**From a release**: download the `jig_<version>_<os>_<arch>.tar.gz` for your
platform from the releases page, verify it against `SHA256SUMS`, and put
`jig` on your `PATH`.

You also need at least one agent CLI, authenticated:

| Runtime | `--runtime` | Auth | Roster `model:` values |
|---|---|---|---|
| Claude Code | `claude-code` (default) | `claude` logged in | `haiku`, `sonnet`, `opus`, … |
| Codex | `codex` | `codex login` | Codex model ids, e.g. `gpt-5.6-sol` |

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

## The three surfaces

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

The shape:

```yaml
name: my-workflow
roster:                      # one entry per agent role
  builder:
    model: sonnet
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
acceptance: [all_phases_passed, diff_matches_claims]
```

Rules worth knowing before you write one:

- **Envelopes are the only inter-phase contract.** Every agent phase ends
  with one JSON object carrying at least `status` (`success` or `fail`).
  Extra fields — `approved`, `changed_files`, `revised` — are what gates,
  `if:` guards, and repair predicates read. Code phases get an adapter
  envelope built from their exit status, so a failing test suite enters the
  repair loop through the same door as a failing agent report.
- **Gates verify claims, they do not judge quality.** The registry is
  `artifacts_exist`, `files_non_empty`, `diff_matches_claims`,
  `verdict_consistent`, and `tests_pass(command)`. Repo-specific
  verification is a code phase, not a new gate.
- **Repair loops must be declared and bounded.** `on_fail` is the only loop
  construct; a cycle or a missing budget is rejected at save time, not
  discovered at 2 a.m.
- **Validation happens before anything runs.** `jig def validate <file>`
  is the same check the store applies at save time, offline.

```sh
jig def validate examples/definitions/simple-sdlc.yaml
jig def create examples/definitions/simple-sdlc.yaml
jig def list --json
jig def invoke <definition-id> --instructions "add a --version flag" --repo github.com/you/repo
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

`Ctrl-C` is orderly: it stops new claims, cancels in-flight attempts, records
their terminal state, and destroys their ephemeral scratch.

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
| Claude Code | `~/.claude/.credentials.json`, or the macOS keychain item `Claude Code-credentials`; plus `~/.claude.json` onboarding state | `~/.claude/.credentials.json`, `~/.claude.json` |
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
