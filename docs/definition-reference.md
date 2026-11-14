# Definition reference

A job definition is one YAML file: a roster of agent roles, an ordered chain
of phases, an acceptance predicate, and optional publish policy. This page
lists every field jig reads and every rule `jig def validate` enforces. It is
derived from `internal/protocol/definition.go` (schema and save-time rules),
`internal/engine/` (prompt composition, gates, repair edges, acceptance), and
the stock files in `examples/definitions/`. When this page and the code
disagree, the code is right and this page has a bug.

## Minimal working example

```yaml
name: hello
roster:
  scout:
    model: haiku
    system_prompt: You are a read-only scout. Never change a file.
    user_prompt: "Task: {{prompt}}"
    env: [PATH]
    writes: []
phases:
  - {name: scout, kind: agent, owner: scout}
```

```sh
jig def validate hello.yaml
# ok hello.yaml — definition "hello", 1 phase(s), 1 role(s)
jig run --def hello.yaml . "what is this repo?"
```

The agent receives `Task: <your prompt>`, then the previous envelope, the
handoff directory, and the envelope instruction, all appended by jig (see
[Prompt composition](#prompt-composition)). Acceptance defaults to
`all_phases_passed`.

## How a file is parsed

- Decoding is strict (`KnownFields(true)`): an unknown or misspelt key such
  as `on_fial` is a parse error, not a silently dropped field.
- `jig def validate <file>...` runs the same `ParseDefinition` the control
  plane runs at save time, offline. Exit code 0 is valid, 2 is a usage or
  validation error. `--json` prints the verdicts as JSON. A definition whose
  user prompts reference [parameters](#template-variables) gets a second
  line, `params: focus, scope`, naming what every invocation must pass.
- Validation stops at the first error. Every message names the element at
  fault.

## Top-level block

| Field | Type | Required | Default | Meaning | Example |
|---|---|---|---|---|---|
| `name` | string | yes | — | Definition name. Must not be blank. | `name: factory` |
| `runtime` | string | no | `""` (any worker) | Restricts the job to workers that advertise this runtime. One of `claude-code`, `codex`. | `runtime: claude-code` |
| `roster` | map of role name → [role](#roster-role-block) | yes, in practice | — | The agent roles. Every agent phase's `owner` must be a key here. Every entry is validated, used or not. | see below |
| `phases` | list of [phase](#phase-block) | yes (≥ 1) | — | The chain, run in order. | see below |
| `parallel` | list of phase names | no | none | The one [parallel group](#parallel-group). A flat list; a list of lists is refused ("only one parallel group per definition"). | `parallel: [review-a, review-b]` |
| `acceptance` | list of strings | no | `[]` (only `all_phases_passed` is judged) | The acceptance predicate. Each entry is `all_phases_passed` or the name of a gate declared on some phase. `all_phases_passed` is always evaluated, listed or not. | `acceptance: [all_phases_passed, verdict_consistent]` |
| `publish` | [publish block](#publish-block) | no | none (publish a PR, never wait, never merge) | Hold, CI wait, and merge policy. | see below |
| `build_outputs` | list of directory patterns | no | none (no grant) | Gitignored directories any role may write, read-only roles included. See [build_outputs](#build_outputs). | `build_outputs: ["bin/**"]` |

## Roster role block

One entry per agent role under `roster:`.

| Field | Type | Required | Default | Meaning | Example |
|---|---|---|---|---|---|
| `model` | string | yes | — | Model name handed to the runtime CLI (`--model`). | `model: opus` |
| `effort` | string | no | `""` (CLI default) | One of `low`, `medium`, `high`, `xhigh`, `max`. Claude Code gets `--effort`; the Codex adapter maps it to its reasoning-effort setting. | `effort: high` |
| `budget_usd` | number | no | `0` (no cap) | When positive, caps what one send by this role may spend (Claude Code `--max-budget-usd`). Must not be negative. Refused under `runtime: codex`. | `budget_usd: 4` |
| `thinking` | string | no | `""` | Accepted by the parser. No code in this tree reads it, so it has no effect today. | — |
| `system_prompt` | string | one of the pair | — | System prompt content, passed verbatim. No `{{…}}` substitution happens here. | `system_prompt: You are a reviewer.` |
| `system_prompt_path` | string | one of the pair | — | Worktree-relative path to the system prompt, read when the phase starts. Absolute paths, `..`, and symlinks out of the worktree are refused at run time. Existence is not checked at validate time. | `system_prompt_path: prompts/reviewer.md` |
| `user_prompt` | string | one of the pair | — | User prompt template. See [Template variables](#template-variables). | `user_prompt: "Task: {{prompt}}"` |
| `user_prompt_path` | string | one of the pair | — | Worktree-relative path to the user prompt template, same rules as `system_prompt_path`. The file content gets the same substitution as `user_prompt`. | `user_prompt_path: prompts/build.md` |
| `tools` | list of strings | no | none (CLI default tool set) | Tool names, joined with commas into Claude Code's `--tools`. A capability list, not a sandbox: the write boundary is `writes`. Refused under `runtime: codex`. | `tools: [Read, Grep, Glob, Bash]` |
| `writes` | list of globs | no | omitted = unrestricted | Write allowlist, checked after the phase against the worktree. Omitted (`nil`) permits every path; `[]` is read-only; otherwise only matching paths may change. A breach rolls back and ends the attempt. | `writes: ["src/**", "docs/**"]` |
| `env` | list of names | no | none | Environment allowlist: the only variables the agent receives (plus an ephemeral `HOME`). List `PATH` yourself. The union over all roles must be advertised by a worker for it to claim the job. | `env: [PATH, GH_TOKEN]` |
| `sensitive_env` | list of names | no | none | Names whose values are redacted from every persisted trace — the attempt JSONL and the events table, on worker runs and `jig run` alike (both persist through the worker's trace stream). Values shorter than the redactor's minimum length are not redacted. | `sensitive_env: [GH_TOKEN]` |

Exactly one of `system_prompt` / `system_prompt_path` is required, and exactly
one of `user_prompt` / `user_prompt_path` is required. Setting both of a pair
is an error.

**Effort and spend.** `effort` sets how much a role thinks and verifies, and
thinking is billed as output. Codex tops out at `xhigh`, so `max` runs there
as `xhigh`. A useful split: `medium` for a builder working to a plan, `high`
for a reviewer hunting the edge cases the build missed. Omit it to keep the
CLI's default for the model, and omit it for `haiku`, which does not take an
effort level. `budget_usd` is the most one send by that role may cost.
Codex has no such flag, and a cap it cannot enforce fails rather than run
uncapped — the same posture as a `tools` allowlist there.

## Phase block

One entry per link under `phases:`.

| Field | Type | Required | Default | Meaning | Example |
|---|---|---|---|---|---|
| `name` | string | yes | — | Unique phase name. | `name: build` |
| `kind` | string | yes | — | `agent` or `code`. | `kind: agent` |
| `owner` | string | agent: yes; code: no | — | A roster role. On an agent phase, the role that runs. On a code phase, only lends its `env` allowlist to the command and its gates. | `owner: builder` |
| `command` | string | code: yes; agent: forbidden | — | Shell command run in the worktree. Exit 0 passes. Env is `PATH`, the owner's `env`, an ephemeral `HOME`, and `JIG_BASE_SHA` (the attempt's pinned base commit). | `command: go test ./...` |
| `if` | string | no | `""` (always run) | Guard over the chain's merged field view. A bare field name runs the phase only when that field is truthy (not `false`, `0`, `""`, `null`, empty list, or empty object). Any multi-token value must parse as `<field> == <literal>` or `<field> != <literal>`. A guard that does not hold records the phase as skipped, which passes `all_phases_passed`. A gate named in `acceptance` that only a skipped phase declares never ran, and fails acceptance (`gate never ran`). | `if: revised`, `if: "risk != low"` |
| `reports_fields` | bool | no | `false` | Code phases only. The last non-empty line of the command's output must be a JSON object. Output is stdout and stderr combined, in the order written, so a command that logs to stderr after its report line must redirect that output (`2>/dev/null`) or print the report last; its keys join the envelope and the field view. The keys `status`, `summary`, `passed`, `failures`, `exit_code`, `output_tail` are reserved. A later agent envelope cannot overwrite a reported field. | `reports_fields: true` |
| `gates` | list of [gate](#gate-block) | no | none | Claim verifiers run after the phase. | `gates: [{name: artifacts_exist}]` |
| `on_fail` | [repair edge](#repair-edge-block) | no | none | The phase's one repair loop. | `on_fail: {run: build, then: rerun-self, budget: 2}` |

## Gate block

One entry per gate under a phase's `gates:`.

| Field | Type | Required | Default | Meaning | Example |
|---|---|---|---|---|---|
| `name` | string | yes | — | A built-in gate (see [Gates](#gates)). | `name: tests_pass` |
| `command` | string | `tests_pass`: yes | — | The command `tests_pass` runs. Ignored by every other gate. | `command: go test ./...` |
| `budget` | int ≥ 0 | no | `0` → 2 | Gate-correction budget. The phase uses the largest `budget` among its gates; if all are 0, it uses `DefaultPhaseRetryBudget` = 2. | `budget: 3` |
| `allow` | list of globs | `tests_intact` only | none | `path.Match` globs naming files where `tests_intact` permits deleting or weakening tests or adding suppressions. `**` is refused (`*` stops at `/`). Any other gate with `allow` is an error. | `allow: ["testdata/*"]` |

On an **agent** phase, a failed gate sends the agent a correction prompt in
the same session, and every corrected envelope re-runs all of the phase's
gates. When the budget is spent, the phase fails. On a **code** phase there
is no session to correct: a failed gate fails the phase, and the phase's
`on_fail` edge (if any) routes it.

## Gates

A gate verifies the envelope's claims against the worktree. It does not
judge quality. Source: `internal/engine/gates.go`, `tests_intact.go`.

| Gate | What it checks | Envelope fields it reads | Failure messages (check note) |
|---|---|---|---|
| `artifacts_exist` | Every path in `artifacts` exists inside the worktree. An empty or absent `artifacts` list records no check and passes. | `artifacts` | `declared artifact path is not inside the worktree`; `declared artifact does not exist` |
| `files_non_empty` | Every existing, non-directory path in `artifacts` has size > 0. Missing paths are left to `artifacts_exist`. | `artifacts` | `declared artifact is empty` |
| `diff_matches_claims` | `changed_files` is present and is a list, and each entry exists inside the worktree. It checks existence on disk; it does not compare against `git diff`. `[]` passes ("the phase claims no changed files"). | `changed_files` | `the envelope claims no changed_files`; `changed_files must be a list of paths, got <type>`; `claimed changed file is not a path string`; `claimed changed file is not inside the worktree`; `claimed changed file does not exist` |
| `verdict_consistent` | The verdict agrees with itself: no `approved: true` with blocking items or unmet findings, and a rejection names a problem. The order matters: with any blocking item or unmet finding, the gate passes unless `approved` is literally `true`, so a missing or non-boolean `approved` passes there as a rejection. Only with no blocking item and no unmet finding does a missing `approved` fail, and a non-boolean `approved` fail. Ask the reviewer's prompt for an explicit boolean `approved` every time. | `approved` (bool), `blocking` (list), `findings[].met` (bool) | `<n> blocking item(s) while approved=true`; `<n> unmet requirement(s) while approved=true`; `"approved" is missing: state the verdict explicitly as true or false. …`; `"approved" must be a JSON boolean, got <type>: …`; `approved=false but no blocking item or unmet requirement was given` |
| `tests_pass` | The gate's `command` exits 0 in the worktree, with the phase's env. | none | `exit <n>` plus an output tail; `timed out` plus an output tail |
| `tests_intact` | Diffs every changed file against the attempt's base commit. Test files (`_test.go`, `_test.py`, `test_*.py`, `conftest.py`, `*.test.*`/`*.spec.*` JS/TS, `_spec.rb`, and anything under `test/`, `tests/`, `__tests__/`, `spec/`) must not be deleted, lose tests or assertions, or gain skip/focus markers. Changed source files must not gain checker suppressions or stubs. A test file the gate cannot inspect (binary, over 4 MiB, a lexer edge case) fails closed. A source file it cannot inspect records a passing `not inspected for suppressions or stubs: …` check. | none (reads git, not the envelope) | e.g. `test file deleted since base <sha>; restore it …`; `net loss of <n> <what> …`; `adds a skip/focus marker (…)…`; `the attempt has no pinned base commit to diff against` |

The envelope base fields (`internal/engine/envelope.go`): `status` (required,
`"success"` or `"fail"`), `summary` (string), `artifacts` (list of strings),
`notes_for_next_agent` (string). Every other key is a free field that gates,
`if:` guards, `on_fail.when`, and `publish.hold_when` may read.

## Repair edge block

`on_fail:` on a phase. Source: `RepairEdge` in `definition.go`, `followEdge`
in `internal/engine/phase.go`.

| Field | Type | Required | Default | Meaning | Example |
|---|---|---|---|---|---|
| `when` | predicate | agent: yes; code: forbidden | — | Agent phases: the envelope predicate that fires the edge, e.g. `approved == false`. Code phases fire on failure (nonzero exit or a failed gate), so `when` is refused there. | `when: "approved == false"` |
| `run` | phase name | yes | — | The repair target. It must exist and must not be the phase itself. | `run: build` |
| `then` | string | yes | — | `rerun-self`: run the target, then rerun this phase. `rerun-chain`: run the target and every phase between it and this phase, in chain order and honoring their `if:` guards, then rerun this phase. A `rerun-chain` target must precede this phase, and the replayed range must not contain a parallel-group member. | `then: rerun-chain` |
| `budget` | int > 0 | yes | — | How many times this edge may dispatch. Zero or negative is refused ("unbounded repair loops are rejected"). | `budget: 3` |
| `exhausted` | string | no | `fail-job` | What happens when the edge fires with the budget spent. `fail-job` ends the attempt failed. `proceed` continues the chain with the failing envelope; the phase's last recorded status still counts toward `all_phases_passed`. | `exhausted: proceed` |

The repair target receives the failing envelope as its previous envelope. The
target's own `on_fail` does not fire on a dispatched run. If a dispatched
repair phase fails, the attempt fails. Repair targets must not form a cycle
across phases.

### Predicate language

Used by `if:` (multi-token form), `on_fail.when`, and `publish.hold_when`.
Exactly three whitespace-separated tokens: `<field> <op> <literal>`, where
`<op>` is `==` or `!=`. Surrounding `"` or `'` are trimmed from the literal.
The comparison follows the field's JSON type: booleans against `true`/`false`,
numbers numerically, strings as strings. A missing field equals only `null`.
Nothing richer exists; use a code phase with `reports_fields` instead.

## Parallel group

`parallel:` names two or more phases that run concurrently as one step.
Members merge their fields in declared order, and the phase after the group
receives the last member's envelope. Rules (`validateParallel`):

- At least two members, each a phase in the chain, none listed twice.
- Members are consecutive phases, listed in chain order.
- Every member is an `agent` phase.
- Every member's role declares `writes: []`. Members are read-only apart
  from `build_outputs`.
- Every member has its own owner role.
- No member's `if:` guard reads a field a sibling reports (a base envelope
  field, or the field the sibling's `on_fail.when` reads), because every
  member is judged against the view from before the group.
- `publish.ci.on_fail.run` must not be a member.
- No `rerun-chain` edge may replay a member.

How the group runs: each member runs in its own ephemeral HOME and session
and is handed the envelope from before the group, never a sibling's. Any
other worktree change while the group runs — including one a crashed member
left — rolls back and aborts the attempt. Rejections resolve after every
member finishes, in declared order. A member whose repair edge did not
trigger and that did not pass ends the attempt there, as it would
sequentially. The first member whose edge triggered with budget left
dispatches its repair target with its own envelope and charges only its own
budget; later members are not judged on that run. Then the whole group runs
again from the repair's envelope: every member that ran before runs again,
including a reviewer that already approved, and spends its time and cost
again. A member skipped by its guard on the first run stays skipped. A
triggered member with its budget spent follows its `exhausted` rule:
`fail-job` ends the attempt, `proceed` moves on to the next member. The
group runs at most 1 + the sum of its members' budgets times.
`examples/definitions/factory-parallel.yaml` is the stock factory with its
review panel grouped; `factory.yaml` stays sequential.

## build_outputs

Each entry is a directory grant of exactly one shape, with a literal `<dir>`:
`<dir>/**`, `**/<dir>/**` (root and nested), or `<dir>/`. A grant covers only
gitignored paths that are not tracked in `HEAD`. A non-ignored path under a
grant is ordinary content judged by `writes`. jig never stages an output. It
is never merged into `writes`.

Why it exists: a read-only reviewer that runs the repository's own check
command still writes what it builds, and `writes: []` would breach on it. A
grant lets every role — read-only reviewers and parallel-group members
included — write those directories without breaching. A path under a grant
that git does not ignore breaches a read-only role with `declared build
output is not gitignored`, and is a changed path (committed like any other)
for a writing one.

Declare the directory git ignores: with `cache/` ignored, `cache/**` covers
`cache/bin/x` but `cache/bin/**` does not. Git reports a wholly ignored
directory as one entry, and a grant matches paths as git enumerates them.
A path tracked in `HEAD` is never an output, and git metadata never is.
`git add -A` in a commit phase never stages an ignored path; a role that
commits an output itself (`git add -f`) publishes it under its own `writes`,
and from then on it is tracked content. Outputs written during a parallel
group survive the group whatever happened to the member that wrote them,
and the trace attributes them to the group, not a role. A crashed
sequential phase is still rolled back to its snapshot, its new outputs
included. Declare only directories no later phase executes: a reviewer could
otherwise plant what a test phase runs. Refused: empty entries, leading or
trailing whitespace, NUL or line breaks, absolute paths, a leading `-` or
`:`, any `..` or `.git` segment, all-wildcard patterns, file-level patterns
(`bin/*.o`), and wildcards inside `<dir>` (`*/bin/**`, `b?n/**`).

## Publish block

`publish:` is the definition's say over delivery. Source: `PublishSpec`,
`validatePublish`; `publishHold` in `phase.go`; `internal/worker/publish_merge.go`.

| Field | Type | Required | Default | Meaning | Example |
|---|---|---|---|---|---|
| `hold_when` | predicate | at least one of `hold_when` / `ci` | none | Evaluated against the merged field view after acceptance passes. When it holds, the attempt ends `accepted_unpublished` with a recorded hold instead of a pull request. Release it from the UI or with `jig job release <job-id>`. | `hold_when: "risk == high"` |
| `ci.wait` | bool | no | `false` | Wait for the pull request's CI after publish; green CI on the head becomes part of `accepted`. | `wait: true` |
| `ci.timeout` | Go duration | no | `30m` | Wait budget for one head commit. Must be within `1m`..`6h`. | `timeout: 45m` |
| `ci.rerun.budget` | int | with `rerun` | — | Re-runs of failed GitHub Actions jobs on the same head, per attempt, before any repair round. `1`..`3`. Needs `wait: true`. | `rerun: {budget: 1}` |
| `ci.on_fail.run` | phase name | with `on_fail` | — | Agent phase that receives red CI as its input envelope; every phase after it runs again before a new push. Must not be the last phase. Needs `wait: true`. | `on_fail: {run: build, budget: 2}` |
| `ci.on_fail.budget` | int | with `on_fail` | — | Repair rounds, `1`..`3`. | `budget: 2` |
| `merge.method` | string | with `merge` | — | Opts never-held work into an exact-head merge after green CI. One of `squash`, `merge`, `rebase`. Needs `ci.wait: true`. The worker must also run with `--allow-merge`, or the merge step fails with `merge_not_allowed`. A held job that is released is never merged. | `merge: {method: squash}` |

## Template variables

Substitution applies to the user prompt only (`user_prompt` or the content
of `user_prompt_path`), in one pass over the template. Text that a
substitution inserts is never scanned again, so a parameter value that
contains `{{handoff_dir}}` stays literal. Source: `composePrompt` in
`internal/engine/phase.go`.

Parameters have no defaults. The engine leaves a `{{name}}` with no matching
parameter as it is, so the CLI refuses that run before it is admitted:
`jig def invoke` and `jig trigger create` read the saved definition and stop
with exit 2 and

```
missing_param: role "surveyor" user_prompt references {{focus}}; pass --param focus=<value>
```

for every inline `user_prompt` placeholder that names neither a built-in
below nor a `--param`. A `user_prompt_path` file is read from the worktree
at run time and cannot be checked ahead of it. A placeholder is `{{name}}`
where `name` is any text with no whitespace, braces or `=`, the names
`--param name=value` can pass: `{{focus.area}}`, `{{.Go}}` and
`{{repo/path}}` all count. Forms with spaces, such as `{{ x }}` or
`${{ github.sha }}`, are never checked and reach the agent as written.

| Variable | Value | If the template omits it |
|---|---|---|
| `{{prompt}}` | The run's prompt. `jig run … "<prompt>"` sets it directly. `jig def invoke --instructions …` and triggers compose it from the trusted instructions and any untrusted context, frozen at admission. | Not appended. Reference it, or the agent never sees the task. |
| `{{<param>}}` | The value of `--param <param>=<value>` on `jig def invoke` or `jig trigger create` (repeatable). Required: an unset one is refused at invoke (`missing_param`). `--param prompt=…` together with instructions is refused (`ambiguous_prompt`). | Not appended. |
| `{{previous_envelope}}` | The raw JSON of the previous phase's envelope, or `(none)` for the first phase. After a repair dispatch it is the failing envelope; after a parallel group it is the last member's envelope. | Appended as a `## Previous envelope` section. |
| `{{handoff_dir}}` | A per-attempt directory outside the worktree for notes to later phases. It needs no `writes` entry. | Appended as a `## Handoff directory` section. |

### Prompt composition

After substitution, jig always appends this envelope instruction last:

> When you are done, respond with ONLY a JSON object with these fields:
> status ("success" or "fail"), summary, artifacts (repo-relative paths you
> created or changed), notes_for_next_agent. No prose around it.

Ask for any extra field a gate, guard, or predicate needs (for example
`changed_files` or `approved`) in your own user prompt, as every stock
definition does.

Code phases get no prompt. A code phase's envelope is built by jig from the
command result: `status`, `summary`, `passed`, `failures`, `exit_code`,
`output_tail`, plus any `reports_fields` keys.

## Validation rules

`DefinitionSpec.Validate`, in the order it checks:

1. `name` is not blank.
2. At least one phase.
3. Every roster role: `model` set; `effort` empty or in the vocabulary;
   `budget_usd` ≥ 0; exactly one of `system_prompt` / `system_prompt_path`;
   exactly one of `user_prompt` / `user_prompt_path`.
4. `runtime` is empty, `claude-code`, or `codex`. Under `codex`, no role may
   set `tools` or a positive `budget_usd`.
5. `build_outputs` entries have a valid directory-grant shape.
6. Every phase has a name; no name is declared twice.
7. Per phase: `kind` is `agent` or `code`. Agent phases have an `owner`, no
   `command`, and no `reports_fields`. Code phases have a non-blank
   `command`. A multi-token `if:` parses as a predicate. Any `owner` exists
   in the roster. Every gate is built in; `tests_pass` has a `command`;
   gate `budget` ≥ 0; `allow` only on `tests_intact`, non-empty entries, no
   `**`, valid `path.Match` syntax.
8. Per `on_fail`: `run` names an existing phase other than this one; `then`
   is `rerun-self` or `rerun-chain`; a `rerun-chain` target precedes this
   phase and its replay range holds no parallel member; `budget` > 0;
   `exhausted` is empty, `fail-job`, or `proceed`; agent phases have a
   parseable `when`; code phases have no `when`.
9. No cycle among repair targets.
10. Every `acceptance` entry is `all_phases_passed` or a gate declared on
    some phase.
11. The [parallel group](#parallel-group) rules.
12. `publish`, when present, declares `hold_when`, `ci`, or `merge`; `merge`
    needs `ci.wait: true` and a valid `method`; `hold_when` parses;
    `ci.timeout` parses and is within `1m`..`6h`; `ci.rerun` needs
    `wait: true` and a budget in `1`..`3`; `ci.on_fail` needs `wait: true`,
    targets an existing agent phase that is not last, and has a budget in
    `1`..`3`.

## Complete annotated example

Every block in use. It validates as written; edit the commands, globs, and
models for your repository. `prompts/build.md` must exist in the target
repository when the build phase runs.

```yaml
name: annotated
runtime: claude-code              # only workers advertising claude-code claim it

roster:
  planner:
    model: sonnet
    tools: [Read, Grep, Glob]     # joined into --tools Read,Grep,Glob
    system_prompt: |
      You are a read-only planner. You never change a file.
    # {{target}} comes from `jig def invoke --param target=main`. A `#`
    # inside the block below would be prompt text, not a comment.
    user_prompt: |
      Task: {{prompt}}
      Target branch: {{target}}

      Previous report (a failing test run on a repair round):
      {{previous_envelope}}

      Put the plan in notes_for_next_agent.
    env: [PATH]
    writes: []                    # read-only

  builder:
    model: opus
    effort: medium
    budget_usd: 8                 # per-send spend cap
    system_prompt: |
      You are a builder. Claim only files you actually changed.
    user_prompt_path: prompts/build.md   # worktree-relative, read at phase start
    env: [PATH, GH_TOKEN]
    sensitive_env: [GH_TOKEN]     # redacted from persisted traces
    writes: ["src/**", "docs/**"]

  correctness:
    model: opus
    effort: high
    system_prompt: You review for correctness. You never change a file.
    user_prompt: |
      Review the change. Your final JSON must also include
      "approved" (true or false) and "blocking" (a list of strings).
    env: [PATH]
    writes: []                    # parallel members must be read-only

  security:
    model: opus
    effort: high
    system_prompt: You review for security. You never change a file.
    user_prompt: |
      Review the change. Your final JSON must also include
      "approved" (true or false) and "blocking" (a list of strings).
    env: [PATH]
    writes: []

phases:
  - name: plan
    kind: agent
    owner: planner

  - name: build
    kind: agent
    owner: builder
    gates:
      - {name: artifacts_exist}           # reads artifacts
      - {name: files_non_empty}           # reads artifacts
      - {name: diff_matches_claims}       # reads changed_files
      - {name: tests_intact, allow: ["testdata/*"]}
      - {name: tests_pass, command: "go test ./...", budget: 3}  # phase budget = 3

  - name: test
    kind: code
    owner: builder                        # lends PATH, GH_TOKEN to the command
    command: go test ./...
    # red suite: re-plan, rebuild, retest; fail the job after 2 rounds
    on_fail: {run: plan, then: rerun-chain, budget: 2, exhausted: fail-job}

  - name: review-correctness
    kind: agent
    owner: correctness
    gates: [{name: verdict_consistent}]   # reads approved, blocking
    on_fail: {when: "approved == false", run: build, then: rerun-self, budget: 2}

  - name: review-security
    kind: agent
    owner: security
    gates: [{name: verdict_consistent}]
    # after 2 rounds, continue with the rejection on record
    on_fail: {when: "approved == false", run: build, then: rerun-self, budget: 2, exhausted: proceed}

  - name: classify-risk
    kind: code
    reports_fields: true                  # last output line is a JSON object
    command: |
      echo "classifying"
      printf '{"risk":"low"}\n'

  - name: review-risk
    kind: agent
    owner: security
    if: "risk != low"                     # skipped (and passing) when risk is low

parallel: [review-correctness, review-security]

acceptance: [all_phases_passed, verdict_consistent, diff_matches_claims]

publish:
  hold_when: "risk == high"               # accepted_unpublished until released
  ci:
    wait: true
    timeout: 45m
    rerun: {budget: 1}                    # one re-run of failed jobs per attempt
    on_fail: {run: build, budget: 2}      # red CI goes back to the builder
  merge: {method: squash}                 # needs the worker's --allow-merge

build_outputs: ["bin/**"]                 # gitignored bin/ is writable by every role
```

## See also

- [gates.md](gates.md) — what gates verify, and what `tests_intact` refuses
  in detail.
- [publish.md](publish.md) — holds, CI waits, CI repair, re-runs, and merge.
- [`examples/definitions/`](../examples/definitions/) — the stock definitions,
  validated in CI by `just definitions`.
- [README: Authoring a definition](../README.md#authoring-a-definition).
- [docs/quickstart.md](quickstart.md) — first run.
