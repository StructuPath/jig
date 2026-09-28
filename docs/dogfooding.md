# Dogfooding runbook

This runbook takes an operator from preflight to a filled results table for
a trial of `factory.yaml` on a real repository, and ends with a decision.
Every number in the results table comes from `jig report`. jig does not
enforce the trial's spend ceiling: you set it and check it before each batch
(plan `2026-09-28-001`, KTD8).

It assumes you have already been through `docs/quickstart.md` once, and
that you have read the README's *Operational notes*.

---

## 0. What the build must contain

The report is only as good as the ledger underneath it. Run the trial on a
`jig` built from a `main` that contains:

| Change | Without it |
|---|---|
| `jig report` and `GET /api/report` (PR #24) | there is no report. `jig report --help` must print its usage. |
| Spend on every agent phase exit (PR #25) | spend omits every phase that did not pass. It also overcounts every resumed session, because Claude Code's `total_cost_usd` is a running total per session. |
| Publish history across retries (PR #22) | a publish-only retry erases the stop code it replaces, so the *retried, still red* rate reads zero. |
| Flaky-check re-runs (`publish.ci.rerun`, U4 of the plan) | optional. Without it, the *Re-runs* section is all zeros and section 3's masking threshold does not apply. |

This runbook describes re-runs as the plan specifies them (R5–R7, KTD5). The
PR that implements them had not landed when this was written, so check the
README's `publish.ci` bullets for the syntax as it shipped.

Build once in your jig clone, run every command below from that clone, and
use the same binary for `serve`, `worker`, and `report`:

```sh
just build                 # or: go build -o bin/jig ./cmd/jig
./bin/jig version
./bin/jig report --help
```

The commands below write `jig` for `./bin/jig`.

---

## 1. Preflight

Do every step in this section before the first job, and again if the
machine, the `gh` login, or the Claude login changes during the trial.
`OWNER/REPO` is the target repository throughout.

```sh
REPO=OWNER/REPO
```

### 1.1 Runtime auth works in an ephemeral HOME

Clone the target repository and run the one-send smoke definition against
it. It is read-only, never publishes, and runs one `haiku` send in the same
kind of seeded, ephemeral HOME the worker uses:

```sh
git clone "https://github.com/$REPO" ~/trial/target
jig run --def examples/definitions/smoke.yaml ~/trial/target "what is this repository?"
echo $?                    # must be 0
```

Run it from the same kind of shell the worker will run in: the same user,
and a GUI login session rather than SSH or launchd. On macOS, jig seeds
Claude's login from the keychain item `Claude Code-credentials`, and only a
session that can read the login keychain gets the live token. If the smoke
run fails with *OAuth session expired*, log in again with `claude`, then run
the smoke again. In the CI repair exit gate, a stale
`~/.claude/.credentials.json` killed an attempt before it did any work.

### 1.2 `gh` can do everything jig will do unattended

```sh
gh auth status
```

For a classic or OAuth token, the *Token scopes* line must include `repo`
and `workflow`. `workflow` is needed for a push that touches
`.github/workflows/`. A fine-grained token lists no scopes. It needs
Contents, Pull requests, and Actions set to read and write, plus Checks and Commit statuses read,
on the target repository.

Then prove the two Actions calls jig makes, with the same `gh` invocations,
on a finished job from `main`:

```sh
RUN=$(gh run list --repo "$REPO" --branch main --status completed --limit 1 \
        --json databaseId --jq '.[0].databaseId')
JOB=$(gh run view "$RUN" --repo "$REPO" --json jobs --jq '.jobs[0].databaseId')

# The log read CI repair hands to the builder (the failure's log tail).
gh api --allow-escape-sequences -H "Accept: application/vnd.github+json" \
    "repos/$REPO/actions/jobs/$JOB/logs" | tail -c 2000

# The per-job re-run (KTD5). It really re-runs that job: CI minutes are
# spent and the job's secrets are exercised, which is exactly what jig will do.
gh api -X POST "repos/$REPO/actions/jobs/$JOB/rerun"
```

The first call must print log text, not an error. The second must exit 0.
If it fails with HTTP 403, the token cannot re-run jobs: fix it before
declaring any `rerun` policy. Both calls matter because the two CI repair
gate bugs showed up only against real `gh`. The fakes never saw them.

### 1.3 CI is green on `main`

```sh
gh run list --repo "$REPO" --branch main --limit 5
```

The latest run of every workflow must be green. A red `main` means every
job starts red, so repair rounds and re-runs would be judged on a failure
the task did not cause. Also note how long a typical run takes (`gh run
view <run-id> --repo "$REPO"`), because section 2.3 needs it.

### 1.4 Its Actions secrets are ones you accept being exercised unattended

During the trial, jig pushes branches and opens pull requests that trigger
the target's workflows, re-runs failed jobs, and feeds the tail of each
failed job's log to the builder agent. List what those workflows can reach:

```sh
gh secret list --repo "$REPO"
gh secret list --repo "$REPO" --env <environment>     # for each environment a workflow uses
gh variable list --repo "$REPO"
grep -rn -E 'secrets\.|vars\.|environment:' ~/trial/target/.github/workflows/
```

Go through the output and, for each workflow that runs on `pull_request`,
or on `push` to a branch other than the default, confirm all of the
following:

- **Every secret it uses is acceptable to exercise again, unattended, on
  agent-written code.** A deploy key, a publish token, or a paid API key
  used in a PR workflow is exercised by every jig push and every re-run. If
  that is not acceptable, the repository is not a trial target.
- **No CI input the agent should not see sits in a variable.** GitHub
  prints a workflow's `env:` block, including values that come from
  `vars.*`, in the job log. That log tail goes to the builder. Keep inputs
  like these in secrets, which the log masks as `***`.
- **No step prints a value derived from a secret.** GitHub masks the
  secret's exact value only. A base64-decoded or otherwise transformed copy
  prints in clear unless the workflow masks it with `::add-mask::`.

If you change anything here, re-run 1.3.

---

## 2. Trial protocol

### 2.1 A dedicated control plane

Give the trial its own data directories, so the report counts trial jobs
and nothing else:

```sh
jig serve --data ~/trial/cp                                          # terminal 1
jig worker --data ~/trial/worker --runtime claude-code --capacity 1  # terminal 2
```

Both commands assume the default `http://127.0.0.1:8383`, so stop any
other `jig serve` first. Keep `--capacity 1` for the first batch. Raise it
only after one job has run end to end, and remember that each slot can hold
a CI wait for up to the CI timeout.

Copy `factory.yaml` and edit what its header says to edit (the test
command, the builder's `writes` allowlist, and the risk classifier's
paths), then save it:

To re-run failed Actions jobs before a repair round is spent (requires U4),
the copy's `publish` block gains one line, a re-run budget of 1 to 3. A pass
after a re-run publishes, and the report counts it as flaky, never as a
clean first pass:

```yaml
publish:
  hold_when: "risk != low"
  ci:
    wait: true
    rerun: {budget: 1}                  # the added line
    on_fail: {run: build, budget: 2}
    timeout: 30m
```

```sh
cp examples/definitions/factory.yaml ~/trial/factory.yaml
$EDITOR ~/trial/factory.yaml
jig def validate ~/trial/factory.yaml
jig def create ~/trial/factory.yaml        # prints the definition id
```

Write down the trial's start time and your ceiling:

```sh
TRIAL_START=$(date -u +%Y-%m-%dT%H:%M:%SZ)
CEILING_USD=150            # yours to choose
```

### 2.2 Job selection

A trial measures the factory, so the tasks decide what gets measured. Pick
tasks that:

- **have an outcome you can judge without jig.** You will review every
  accepted PR yourself (section 2.5), so you need to know what a correct
  change looks like.
- **are a mix.** Include small fixes, a feature that needs new tests, and at
  least a few tasks likely to trip a CI check the local `test` phase does
  not run (lint, format, a second platform). Those are the only jobs that
  exercise repair and re-runs.
- **include some that should be held.** A task that has to touch
  `.github/` or delete tests should end `held`. If it publishes, that is a
  finding.
- **are independent.** Two jobs in one batch must not edit the same files,
  or the second PR's CI result says more about the merge order than about
  the work.

Aim for at least 20 jobs that wait for CI. Below that, a single job moves a
rate by 5 points or more, and the thresholds in section 3 are not
meaningful. Report counts, not rates, until you get there.

Invoke each job with flags before the definition id:

```sh
jig def invoke \
    --instructions "fix the off-by-one in the pagination cursor described in issue #41" \
    --repo "github.com/$REPO" \
    <definition-id>
```

The command prints the run and its job id. Record the job id.

### 2.3 Size the chain against the attempt ceiling

One attempt, including its CI waits, re-runs, and repair rounds, has a
fixed wall-clock ceiling of 4 hours (`protocol.MaxAttemptDuration`). You
cannot raise it, but you can size what runs inside it. The ceiling is an
absolute deadline from the chain's start. Every minute spent waiting on CI
or on a re-run is a minute a later repair round no longer has (KTD5).

The worst case is roughly:

```
(1 + K) × T_chain  +  (1 + K + R) × T_ci   ≤  4h
```

| Term | What it is | `factory.yaml` |
|---|---|---|
| `T_chain` | plan → build → test → reviewers → risk, until the PR is opened. Measure it on your first jobs (the UI lane's timeline). A round re-runs everything from `build` on, so it costs about as much. | measure |
| `K` | `publish.ci.on_fail.budget`, the repair rounds | 2 |
| `R` | `publish.ci.rerun.budget`, the re-runs per attempt (0 without U4) | 1 once U4 lands |
| `T_ci` | `publish.ci.timeout`. A re-run first waits for every pending check on the head, so a slow sibling job counts against it too. | 30m |

With the stock values (K=2, R=1, T_ci=30m), the CI waits alone can take 2
hours, which leaves about 40 minutes for each of the three chain runs. If
your chain is slower than that, set `timeout` to the target's real CI
duration plus a margin, or lower `on_fail.budget` to 1. Then
`jig def update <definition-id> ~/trial/factory.yaml`.

A round that starts after the deadline ends `ci_repair_round_failed`, and
its round detail says `attempt wall-clock ceiling exceeded`. Count those
jobs as a sizing error, not as a repair failure, and fix the sizing before
the next batch.

### 2.4 Check the spend ceiling before each batch

Before every batch, and only when no trial job is still running, run:

```sh
jig report --json --since "$TRIAL_START" | jq --argjson ceiling "$CEILING_USD" '{
  jobs:            .jobs.total,
  spent_usd:       .spend.total_usd,
  unmetered_sends: .spend.unmetered_sends,
  worst_case_usd:  (.spend.total_usd + 8 * .spend.unmetered_sends),
  headroom_usd:    ($ceiling - .spend.total_usd - 8 * .spend.unmetered_sends)
}'
```

The report counts a job only once it is terminal. A batch still in flight
is spending money the report cannot see yet, so wait for it to finish before
you check.

Recorded spend is a floor. A send killed before it returned has no cost to
record, so the check charges every unmetered send the most any one
`factory.yaml` send may cost (the builder's `budget_usd: 8`). Change that
8 if your roster's highest `budget_usd` differs.

**Start the next batch only if `headroom_usd` is at least the batch size
times your per-job worst case.** Use twice the most expensive job you have
recorded so far. Before the first job, use $10. The CI repair gate's most
expensive run cost at most $5.03, and that figure is an upper bound, since
v0.2.0 overcounted resumed sessions. If the check fails, the trial is over:
go to section 3 with what you have.

### 2.5 What to record per job

The report gives aggregates. The per-job record gives two things the report
cannot: which checks were flaky, and whether the accepted work was actually
right. For each job, once it is terminal:

```sh
JIG=http://127.0.0.1:8383
JOB=<job-id>

curl -fsS "$JIG/api/jobs/$JOB" | jq '{
  state: .job.state,
  attempts: (.attempts | length),
  publish: (.attempts[-1].result // "{}" | fromjson | .publish
            | if type == "object" then {state, code, pr_url,
                rounds: ((.ci_repairs // []) | map({round, outcome})),
                reruns: (.ci_reruns // [])}
              else . end),
  history: (.attempts[-1].result // "{}" | fromjson | (.publish_history // []) | map({state, code}))
}'
```

For its spend, sum `agent_end` over the trace of every attempt of the job,
which is what the report does. The events API pages by `seq`, at most 1000
events per page:

```sh
job_spend() {
  curl -fsS "$JIG/api/jobs/$1" | jq -r '.attempts[].id' |
    while read -r attempt; do
      after=0
      while :; do
        page=$(curl -fsS "$JIG/api/attempts/$attempt/events?after=$after&limit=1000") || exit 1
        [ "$(printf '%s' "$page" | jq '.events | length')" -eq 0 ] && break
        printf '%s' "$page" | jq -c '.events[] | select(.type == "agent_end") | .payload'
        after=$(printf '%s' "$page" | jq '.next_cursor')
      done
    done |
    jq -s '{cost_usd: (map(.cost // 0) | add // 0),
            unmetered_sends: (map(.unmetered_sends // 0) | add // 0),
            agent_phase_entries: length}'
}

job_spend "$JOB"
```

Keep one row per job:

| Field | From |
|---|---|
| job id, task, category (fix / feature / should-hold / CI-tripping) | you |
| terminal state, publish state and code | the job query above |
| repair rounds and each round's outcome | `rounds` |
| re-runs: the job names and each outcome | `reruns` (after U4) |
| publish-only retries, and what you did before each one (released a hold, pushed a fix) | `attempts`, `history`, you |
| spend and unmetered sends, summed over attempts | `job_spend` |
| **your review of the PR**: merged as is, merged after edits, or closed, and why | you. jig cannot see this. |

The last field is the one the report cannot make. jig does not watch CI or
review after an accept, so a clean accept that you then had to rewrite
counts as clean in the report. Record it here.

A publish-only retry is `POST /api/jobs/<job-id>/publish-retry`, or the
button in the UI. It judges CI on the branch's current head, and it never
repairs.

---

## 3. Results and decisions

### 3.1 The exit table

Fill it from one report over the whole trial, taken after the last job is
terminal:

```sh
jig report --json --since "$TRIAL_START" > ~/trial/report.json
jig report --since "$TRIAL_START"          # the same, as prose, for the record
```

| Row | `jig report --json` field | Value |
|---|---|---|
| Terminal jobs | `.jobs.total` | |
| Accepted / accepted, unpublished / failed / cancelled | `.jobs.accepted`, `.jobs.accepted_unpublished`, `.jobs.failed`, `.jobs.cancelled` | |
| Clean accepts (CI green on a head jig pushed / no CI wait) | `.accepts.clean` (`.accepts.clean_ci_green`, `.accepts.clean_no_ci_wait`) | |
| Person-fixed accepts | `.accepts.person_fixed` | |
| Unverified accepts | `.accepts.unverified` | |
| Held, and held rate | `.publish.held`, `.publish.held_rate` | |
| Publish failures by code | `.publish.failed_codes` | |
| Attempts that waited for CI | `.ci.waited` | |
| First-pass green | `.ci.first_pass_green` | |
| Repair entry rate | `.ci.repair.entered`, `.ci.repair.entry_rate` | |
| Repair success rate | `.ci.repair.succeeded`, `.ci.repair.success_rate` | |
| Rounds pushed | `.ci.repair.rounds` | |
| Repair stop codes | `.ci.repair.stop_codes` | |
| Attempts with a re-run / re-runs / flaky passes | `.ci.reruns.attempts`, `.ci.reruns.reruns`, `.ci.reruns.flaky_passes` | |
| `ci_timeout` rate | `.ci.revisit.ci_timeouts`, `.ci.revisit.ci_timeout_rate` | |
| Retried, still red rate | `.ci.revisit.retry_still_red`, `.ci.revisit.retry_still_red_rate` | |
| Total spend | `.spend.total_usd` | |
| Spend per clean accept | `.spend.per_clean_accept_usd` | |
| Spend by outcome | `.spend.by_outcome` (`clean_accept`, `person_fixed`, `accepted_unverified`, `held`, `unpublished`, `failed`, `cancelled`, each with `jobs`, `cost_usd`, `unmetered_sends`) | |
| Unmetered sends | `.spend.unmetered_sends` | |
| Unreadable results | `.unreadable_results` | |
| PRs merged as is / after edits / closed | your per-job record | |

To print the rows straight from the file:

```sh
jq -r '
  ["terminal jobs", .jobs.total],
  ["clean accepts", .accepts.clean],
  ["person-fixed accepts", .accepts.person_fixed],
  ["held rate", .publish.held_rate],
  ["waited for CI", .ci.waited],
  ["first-pass green", .ci.first_pass_green],
  ["repair entry rate", .ci.repair.entry_rate],
  ["repair success rate", .ci.repair.success_rate],
  ["flaky passes", .ci.reruns.flaky_passes],
  ["ci_timeout rate", .ci.revisit.ci_timeout_rate],
  ["retried, still red rate", .ci.revisit.retry_still_red_rate],
  ["total spend (USD)", .spend.total_usd],
  ["spend per clean accept (USD)", .spend.per_clean_accept_usd],
  ["unmetered sends", .spend.unmetered_sends],
  ["unreadable results", .unreadable_results]
  | @tsv' ~/trial/report.json
```

Rates are fractions (`0.125` is 12.5%). A rate or per-accept figure is
`null` when there was nothing to divide by, and prints as an empty cell in
the `@tsv` output. Write `n/a` for those, never 0.

### 3.2 Decision thresholds

Rates are over `.ci.waited`, the attempts that waited for CI. None of these
thresholds applies below 20 such attempts (section 2.2).

| Signal | Threshold | Decision |
|---|---|---|
| `.ci.revisit.ci_timeout_rate` | > 0.10 | Revisit `ci_timeout` repair, which KTD1 deferred. First rule out sizing: a `timeout` shorter than the target's normal CI duration produces timeouts that repair would not fix. |
| `.ci.revisit.retry_still_red_rate` | > 0.10 | Revisit repair on the publish-only retry (KTD1). People are pushing fixes that stay red, so a repair round on retry would have had work to do. |
| `.ci.reruns.flaky_passes / .ci.waited` | > 0.10 | Re-runs may be masking real failures. Take the flaky check names from the per-job records. A check that is flaky on jig's PRs but not on `main` (`gh run list --repo "$REPO" --branch main --status failure`) points at the change, not the check. Run the next batch with `rerun` removed, and compare. |
| The same check flaky in 3 or more jobs | any | Treat it as a bug in that check or in what the builder produces. Do not keep paying re-runs for it. |
| `.accepts.person_fixed` | greater than the repaired-to-green count (`.ci.repair.succeeded`) | People are fixing more red CI than repair does. Read the stop codes before changing the round budget. |
| Clean accepts you had to edit or close | > 20% of your reviewed clean accepts | The report's *clean* is not clean. Tighten the reviewers or the tests before measuring again. |

KTD1's two thresholds are the plan's own. The others are this runbook's
starting points: change them before the trial if you have a reason, but
not after you have seen the results.

---

## 4. Lessons from the CI repair exit gate

The v0.2.0 exit gate (`docs/plans/2026-09-27-001-feat-ci-repair-plan.md`,
*Exit gate*) passed, but not on its first try. Each of these lessons is a
preflight step above.

- **Check against real `gh`, not only fakes.** `gh` refuses to print
  Actions job logs, which contain escape codes, without
  `--allow-escape-sequences`, so every repair would have lost its log. The
  fakes could not show this. Section 1.2 runs the real calls.
- **Keychain auth on macOS.** jig used to seed Claude's login from a stale
  `~/.claude/.credentials.json` before the keychain, and the first attempt
  died on *OAuth session expired*. The keychain now comes first. The smoke
  run in 1.1 is what proves it on your machine.
- **Keep CI inputs out of logs.** One gate run passed its hidden rule list
  through a repo variable. GitHub printed it in the job log's `env:` block,
  and the builder read it from the log tail and satisfied every rule in one
  round. That was correct agent behaviour, but it made the test wrong. Any
  value in a failed job's log reaches the builder. Section 1.4 checks for
  this.
- **Its spend figures are upper bounds.** v0.2.0 summed Claude Code's
  cumulative per-session `total_cost_usd` once per send, so the gate's
  $1.89–$5.03 per run overstate resumed sessions. Do not compare a trial's
  spend per clean accept to them as if they were exact.
