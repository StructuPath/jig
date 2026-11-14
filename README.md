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
git clone https://github.com/StructuPath/JIG-Software-Factory jig
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

BASE=https://github.com/StructuPath/JIG-Software-Factory/releases/download/$VERSION
curl -fsSLO "$BASE/jig_${VERSION}_${OS}_${ARCH}.tar.gz"
curl -fsSLO "$BASE/SHA256SUMS"

# macOS: shasum -a 256 -c SHA256SUMS --ignore-missing
sha256sum -c SHA256SUMS --ignore-missing
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
in a definition's roster goes straight to that CLI, so a definition written
for one runtime says so with a top-level `runtime: claude-code` or
`runtime: codex`: only a worker that advertises that runtime claims its jobs,
and the rest wait in the queue for one. A definition without `runtime:` runs
on any worker. `runtime: codex` is refused at save time if any role sets
`budget_usd` or `tools`, which `codex exec` cannot enforce. The stock
`factory` definitions pin `claude-code`; the other stock definitions are
unpinned and need their models renamed before they run on Codex.

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

**`jig runs` and `jig job`** — the operator loop after admission. List and
show runs, re-admit one at current HEAD, and show, follow, cancel, retry, or
release a single job.

**`jig report`** — measurement. Jobs, accepts, publish outcomes, CI repair,
re-runs, and spend over a time window, read from the control plane's ledger.

---

## Authoring a definition

Every field, gate, template variable, and validation rule: [docs/definition-reference.md](docs/definition-reference.md).

A definition is data, not a script. Seven stock ones ship in
`examples/definitions/`:

| Definition | What it demonstrates |
|---|---|
| `smoke.yaml` | the install check: one read-only phase, repo-independent |
| `scout.yaml` | read-only recon: two agent phases and the hand-off between them |
| `two-phase.yaml` | an agent phase that writes, verified by a code phase |
| `plan-build-test.yaml` | a **code-phase repair edge**: a red suite returns to planning, rebuilds, and retests |
| `simple-sdlc.yaml` | an **agent-phase repair edge** (review → revise → re-review), a conditional retest, and per-phase commit messages |
| `factory.yaml` | the **software factory**: plan → build → commit → test → a panel of specialist reviewers looping the builder until they approve, then a deterministic **risk gate** that holds high-risk work for a person |
| `factory-parallel.yaml` | the **parallel review panel**: `factory.yaml` with its three specialist reviewers run at once as one opt-in `parallel:` group; the first rejecting reviewer in declared order sends the builder its blocking list, then the whole panel runs again |

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
                             # (build output directories go in build_outputs, below)
  reviewer:
    model: opus
    system_prompt: You review a risky change and state approved true or false.
    user_prompt: "Review the change made for: {{prompt}}"
    env: [PATH]
    writes: []               # read-only
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
build_outputs: ["bin/**"]    # gitignored directories ANY role may write, read-only roles included
```

Rules worth knowing before you write one:

- **Envelopes are the only inter-phase contract**: each agent phase ends with one JSON object, and its fields are what gates, `if:` guards, and `on_fail.when` read.
- **Gates verify claims, not quality**, and `tests_intact` refuses lost tests, new skips, and new checker suppressions — [docs/gates.md](docs/gates.md).
- **Repair loops are declared and bounded**: `on_fail` is the only loop, and a cycle or a missing budget is refused at save time.
- **`writes` is the write boundary**; `build_outputs` grants gitignored build directories to every role — [build_outputs](docs/definition-reference.md#build_outputs).
- **Prompt parameters are required**: `jig def validate` lists a definition's `{{name}}`s as `params:`, and `jig def invoke` refuses a run that does not pass each with `--param`.
- **Publish can hold, wait for CI, repair red CI, re-run flaky jobs, and merge**, each opt-in — [docs/publish.md](docs/publish.md).
- **A read-only review panel can run in parallel** — [parallel group](docs/definition-reference.md#parallel-group).
- **Validation happens before anything runs**: `jig def validate <file>` is the save-time check, offline.

```sh
jig def validate examples/definitions/simple-sdlc.yaml
jig def create examples/definitions/simple-sdlc.yaml
jig def list --json
jig def invoke simple-sdlc --instructions "add a --version flag" --repo github.com/you/repo
```

A definition is named by its id or its name (`trigger create --def` takes
either too), and flags may come before or after the positional arguments.
Every command that talks to the control plane takes `--server <url>`; its
default is `$JIG_SERVER` when set, else `http://127.0.0.1:8383`, so point a
shell at a second instance once with `export JIG_SERVER=http://127.0.0.1:8594`.
When nothing answers there, the command says so and where the address came
from: `no control plane at http://127.0.0.1:8383 (from default) — start one
with: jig serve`.

### Watching and steering runs

```sh
jig runs list                          # newest first; --limit 0 for all
jig runs show <run-id>                 # the run and one row per job
jig job show <job-id>                  # state, pull request, one row per attempt
jig job show <job-id> --follow         # wait for the verdict: exit 0 accepted, 3 otherwise
jig job cancel <job-id>                # queued: cancelled now; active: its worker stops it
jig job retry <job-id>                 # a failed job, as a new attempt from phase 1
jig runs readmit <run-id>              # same frozen definition and params, at current HEAD
jig job release <job-id>               # finish an accepted_unpublished job (see below)
```

`--follow` narrates each state change on stderr and exits 3 when the job
settles `failed`, `cancelled`, or `accepted_unpublished` (held, or not yet
published). Only a failed job can be retried; a cancelled one comes back by
re-admitting its run. Triggers are saved disabled: `jig trigger create`
prints the `jig trigger enable <id>` that starts one, or pass `--enable`.
`jig trigger delete <id>` removes a disabled trigger and keeps the runs it
admitted.

A mistyped command, verb, or flag is answered with the nearest spelling
(`jig def invoke: unknown flag --parm — did you mean --param?`). For shell
completion of every command, verb, and flag:

```sh
jig completion zsh > "${fpath[1]}/_jig"     # or: bash, fish
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

An `accepted_unpublished` job — held, its pull request not opened, or CI
red — is finished by the publish-only retry, run by the worker that ran it.

While the worker that ran the job is running, ask it to finish the publish.
Nothing is stopped:

```sh
jig job release <job-id>
```

or use **Release hold** (a held job) or **Retry publication** (anything else)
on the job's page in the UI. Either one records a request on the control
plane; the worker that ran the job takes it on its next poll, when it has a
free slot, and runs the publish-only retry itself, with the author and base
branch it already publishes with. `jig job release` waits for the outcome and
exits 0 when the job is `accepted`, 3 when publish still did not finish, and
1 when the request was refused or the wait was interrupted; `--no-wait`
returns once the request is recorded. A request made while that worker is
stopped stays pending until it starts, and can be withdrawn from the job's
page. A held job that is released is published as a pull request for you to
merge; releasing it is the sign-off, not a merge.

When that worker is stopped, run the retry yourself, as that worker:

```sh
# stop the worker that ran the job (Ctrl-C), then, as that worker:
jig worker publish-retry --data ~/.jig/worker <job-id>
```

Use the same `--data` directory and the same `--commit-author-name`,
`--commit-author-email`, and `--base-branch` that worker runs with. The
retry runs no agent and spends no inference — it needs no runtime CLI at
all — and re-leases the SAME attempt: same branch, same pull request, CI
judged on the branch's current head, never repaired or re-run. It exits 0
when the job is `accepted`, 3 when publish still did not finish (the job
stays `accepted_unpublished`; retry again), and 1 when it could not run.
The data directory is locked while a worker or a retry holds it, so stop the
worker first. A retry that is interrupted mid-publish is returned to
`accepted_unpublished` by the control plane once its lease expires and the
sweeper notices (about a minute); retry again after that.

One data directory is one worker: `jig worker` holds `<data>/worker.lock`
for as long as it runs, and a second worker or a retry on the same
directory is refused. A worker that dies hard leaves its agents' and its
code and gate commands' process groups in the attempt manifests, and the
next start on that directory stops them before reconciling anything else —
as long as the group's leader is still alive; a background process whose
leader already exited cannot be verified as jig's and is left alone.

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

Comparison and file rollback use the pre-phase commit, so committing an edit
does not hide it. A read-only role that moves `HEAD` aborts even if it later
reverts the edit. Jig retains that history for inspection rather than
rewriting it; a crashed phase that moved `HEAD` is not retried.

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
| `2` | usage or validation error — a bad flag or argument, an invalid definition, or the control plane's `invalid_*` rejection of a request |
| `3` | rejected — jig worked and the answer was no (including a cancelled run) |

`1` and `3` are deliberately distinct. "The agent's work was rejected" and
"jig broke" require different responses, and conflating them makes every
wrapper script wrong.

### Loopback only

`jig serve` binds `127.0.0.1` and refuses a non-loopback address without
`--allow-non-loopback`. There is no authentication and no TLS: the control
plane assumes one trusted operator on one machine. Read routes reject
untrusted Host headers even without an Origin header, preventing DNS-rebound
names from reading local data. An explicitly enabled non-loopback listener
accepts its own IP address; reverse proxies must rewrite Host to the
listener's authority. State-changing routes also reject requests whose
`Origin` is not the server's own, which is what stops a web page you happen to
be visiting from driving your control plane through your browser.

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
