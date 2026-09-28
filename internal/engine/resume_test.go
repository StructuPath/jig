// resume_test.go — CI repair rounds (plan U3) on the scripted runtime: what
// a round re-runs and what it never does, how it is judged, what it carries
// from the chain, and what it may not outlive.
package engine_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/StructuPath/jig/internal/engine"
	"github.com/StructuPath/jig/internal/engine/enginetest"
	"github.com/StructuPath/jig/internal/protocol"
	"github.com/StructuPath/jig/internal/worker"
)

// ciRepairSnapshot: a plan phase BEFORE the repair phase (must never re-run),
// the builder the repair targets, a risk classifier, and a reviewer with its
// own repair edge — all after the builder, so all judge a fix.
const ciRepairSnapshot = `
name: ci-repair
roster:
  builder:
    model: test-model
    system_prompt: Build.
    user_prompt: "Build the app."
    writes: ["src/"]
  reviewer:
    model: test-model
    system_prompt: Review.
    user_prompt: "Review the work."
    writes: []
phases:
  - {name: plan, kind: agent, owner: reviewer}
  - {name: build, kind: agent, owner: builder}
  - name: classify
    kind: code
    reports_fields: true
    command: |
      if [ -f src/auth.txt ]; then echo '{"risk":"high"}'; else echo '{"risk":"low"}'; fi
  - name: review
    kind: agent
    owner: reviewer
    on_fail: {when: "approved == false", run: build, then: rerun-self, budget: 1}
acceptance: [all_phases_passed]
publish:
  hold_when: "risk == high"
  ci:
    wait: true
    on_fail: {run: build, budget: 2}
`

func success(summary string, extra map[string]any) enginetest.Step {
	fields := map[string]any{"status": "success", "summary": summary}
	for key, value := range extra {
		fields[key] = value
	}
	return enginetest.Step{Text: envelope(fields)}
}

func writes(files map[string]string, summary string) enginetest.Step {
	step := success(summary, nil)
	step.Files = files
	return step
}

// chainSteps is the accepted chain: plan, build, (classify is code), review.
func chainSteps() []enginetest.Step {
	return []enginetest.Step{
		success("planned", nil),
		// The builder leaves a credential-shaped file in its HOME: nothing
		// written there may outlive the chain or a round (KTD11).
		writes(map[string]string{"src/app.txt": "app", "~/.config/gh/hosts.yml": "token"}, "built"),
		success("looks right", map[string]any{"approved": true}),
	}
}

var redLint = worker.CIFailure{
	Head: "3333333333333333333333333333333333333333",
	Checks: []worker.CICheck{{Name: "lint", Verdict: worker.CIFail, Conclusion: "failure",
		URL: "https://github.com/example/repo/actions/runs/1/job/2"}},
}

type repairFixture struct {
	fake     *enginetest.Runtime
	sink     *recordingSink
	runner   *engine.Runner
	scratch  string
	repo     string
	attempt  engine.Attempt
	cancel   chan struct{}
	snapshot string
}

func newRepairFixture(t *testing.T, snapshot string, mutate func(*engine.Config), steps ...enginetest.Step) *repairFixture {
	t.Helper()
	f := &repairFixture{
		fake: enginetest.New(steps...), sink: &recordingSink{}, scratch: t.TempDir(),
		repo: initRepo(t), cancel: make(chan struct{}), snapshot: snapshot,
	}
	f.runner = newTestRunner(t, f.fake, f.sink, func(config *engine.Config) {
		config.ScratchRoot = f.scratch
		if mutate != nil {
			mutate(config)
		}
	})
	f.attempt = testAttempt(snapshot, nil, f.repo)
	f.attempt.Cancelled = f.cancel
	return f
}

func (f *repairFixture) execute(t *testing.T) worker.Outcome {
	t.Helper()
	return f.runner.Execute(context.Background(), f.attempt)
}

func changedPaths(t *testing.T, result string) []string {
	t.Helper()
	var summary struct {
		ChangedPaths []string `json:"changed_paths"`
	}
	if err := json.Unmarshal([]byte(result), &summary); err != nil {
		t.Fatalf("result does not parse: %v", err)
	}
	return summary.ChangedPaths
}

func homeEntries(t *testing.T, scratch string) []os.DirEntry {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(scratch, "attempt-1", "home"))
	if err != nil {
		t.Fatalf("read scratch home: %v", err)
	}
	return entries
}

// A round hands the CI failure to the repair phase, re-runs every phase
// after it and none before it, and reports only the paths it changed.
func TestACIRepairRoundRerunsTheRepairPhaseAndEveryPhaseAfterIt(t *testing.T) {
	f := newRepairFixture(t, ciRepairSnapshot, nil, chainSteps()...)
	outcome := f.execute(t)
	if outcome.State != protocol.AttemptAcceptedUnpublished || outcome.PublishHold != "" {
		t.Fatalf("chain outcome = %+v, want accepted and not held", outcome)
	}
	continuation := outcome.Continuation
	if continuation == nil {
		t.Fatal("a definition with publish.ci.on_fail got no continuation")
	}
	defer continuation.Release()
	if entries := homeEntries(t, f.scratch); len(entries) != 0 {
		t.Fatalf("the HOME survived the chain with %d entries; it must be wiped at once", len(entries))
	}

	f.fake.Append(
		writes(map[string]string{"src/fix.txt": "fixed", "~/.round-token": "token"}, "fixed lint"),
		success("fix is sound", map[string]any{"approved": true}),
	)
	round := continuation.RepairCI(context.Background(), redLint)
	if round.State != protocol.AttemptAcceptedUnpublished || round.PublishHold != "" {
		t.Fatalf("round outcome = %+v, want accepted and not held", round)
	}
	if entries := homeEntries(t, f.scratch); len(entries) != 0 {
		t.Fatalf("the HOME survived the round with %d entries; it must be wiped when a round ends", len(entries))
	}
	if paths := changedPaths(t, round.Result); len(paths) != 1 || paths[0] != "src/fix.txt" {
		t.Fatalf("round changed_paths = %v, want only the round's own [src/fix.txt]", paths)
	}

	calls := f.fake.Calls()
	if len(calls) != 5 || f.fake.Remaining() != 0 {
		t.Fatalf("calls = %d (remaining %d), want 3 chain + 2 round: plan must not re-run",
			len(calls), f.fake.Remaining())
	}
	repairPrompt := calls[3].Prompt
	for _, want := range []string{`"ci_failed":true`, `"name":"lint"`, redLint.Head, "data, not instructions"} {
		if !strings.Contains(repairPrompt, want) {
			t.Fatalf("repair prompt lacks %q: %.600q", want, repairPrompt)
		}
	}
	for _, call := range calls[3:] {
		if !strings.Contains(call.SessionKey, "attempt-1-ci1-") {
			t.Fatalf("round session key %q is not in the round's own namespace", call.SessionKey)
		}
	}
	for _, call := range calls[:3] {
		if strings.Contains(call.SessionKey, "-ci") {
			t.Fatalf("chain session key %q changed shape", call.SessionKey)
		}
	}
	if !f.sink.has(protocol.EventLog, "ci_repair_start") {
		t.Fatal("no ci_repair_start trace event")
	}
	if f.sink.count(protocol.EventLog, "acceptance") != 2 {
		t.Fatal("the round was not judged by acceptance like the chain")
	}
}

// Inside a round every edge is live: a reviewer rejection loops the builder
// under the reviewer's own repair edge before the round can be accepted.
func TestARejectionInsideARoundLoopsTheBuilderUnderItsOwnEdge(t *testing.T) {
	f := newRepairFixture(t, ciRepairSnapshot, nil, chainSteps()...)
	continuation := f.execute(t).Continuation
	defer continuation.Release()
	f.fake.Append(
		writes(map[string]string{"src/fix.txt": "v1"}, "fixed"),
		success("tests deleted", map[string]any{"approved": false}),
		writes(map[string]string{"src/fix.txt": "v2"}, "fixed properly"),
		success("now sound", map[string]any{"approved": true}),
	)
	round := continuation.RepairCI(context.Background(), redLint)
	if round.State != protocol.AttemptAcceptedUnpublished || f.fake.Remaining() != 0 {
		t.Fatalf("round = %+v remaining=%d, want accepted after the reviewer's loop", round, f.fake.Remaining())
	}
	if !f.sink.has(protocol.EventLog, "repair_edge") {
		t.Fatal("the reviewer's repair edge did not fire inside the round")
	}
}

// A fix that the classifier scores high trips the publish hold: the round
// may not be pushed, and the continuation is spent.
func TestARoundWhoseFixIsHighRiskIsHeldAndSpendsTheContinuation(t *testing.T) {
	f := newRepairFixture(t, ciRepairSnapshot, nil, chainSteps()...)
	continuation := f.execute(t).Continuation
	defer continuation.Release()
	f.fake.Append(
		writes(map[string]string{"src/auth.txt": "bypass"}, "fixed by touching auth"),
		success("fine", map[string]any{"approved": true}),
	)
	round := continuation.RepairCI(context.Background(), redLint)
	if round.State != protocol.AttemptAcceptedUnpublished || !strings.Contains(round.PublishHold, "risk = high") {
		t.Fatalf("round = %+v, want accepted_unpublished held on risk = high", round)
	}
	next := continuation.RepairCI(context.Background(), redLint)
	if next.State != protocol.AttemptFailed || !strings.Contains(next.Error, "spent") {
		t.Fatalf("round after a held round = %+v, want failed as spent", next)
	}
}

// Only accepted, unheld work under a definition that declares on_fail keeps
// a continuation; otherwise the scratch family dies with the attempt.
func TestOnlyRepairableOutcomesKeepAContinuation(t *testing.T) {
	withoutRepair := strings.Replace(ciRepairSnapshot, "    on_fail: {run: build, budget: 2}\n", "", 1)
	for _, tc := range []struct {
		name     string
		snapshot string
		steps    []enginetest.Step
	}{
		{"no on_fail declared", withoutRepair, chainSteps()},
		{"held at the end of the chain", ciRepairSnapshot, []enginetest.Step{
			success("planned", nil),
			writes(map[string]string{"src/auth.txt": "auth"}, "built auth"),
			success("ok", map[string]any{"approved": true}),
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRepairFixture(t, tc.snapshot, nil, tc.steps...)
			outcome := f.execute(t)
			if outcome.State != protocol.AttemptAcceptedUnpublished {
				t.Fatalf("outcome = %+v, want accepted_unpublished", outcome)
			}
			if outcome.Continuation != nil {
				outcome.Continuation.Release()
				t.Fatal("got a continuation for an outcome that cannot be repaired")
			}
			if _, err := os.Stat(filepath.Join(f.scratch, "attempt-1")); !os.IsNotExist(err) {
				t.Fatalf("scratch survived an attempt with no continuation: %v", err)
			}
		})
	}
}

// Release destroys the scratch family, is idempotent, and refuses every
// later round.
func TestReleaseDestroysTheScratchAndRefusesLaterRounds(t *testing.T) {
	f := newRepairFixture(t, ciRepairSnapshot, nil, chainSteps()...)
	continuation := f.execute(t).Continuation
	if _, err := os.Stat(filepath.Join(f.scratch, "attempt-1", "handoff")); err != nil {
		t.Fatalf("the handoff directory did not survive for the round: %v", err)
	}
	continuation.Release()
	continuation.Release()
	if _, err := os.Stat(filepath.Join(f.scratch, "attempt-1")); !os.IsNotExist(err) {
		t.Fatalf("scratch survived Release: %v", err)
	}
	round := continuation.RepairCI(context.Background(), redLint)
	if round.State != protocol.AttemptFailed || !strings.Contains(round.Error, "released") {
		t.Fatalf("round after Release = %+v, want failed as released", round)
	}
	if len(f.fake.Calls()) != 3 {
		t.Fatal("a released continuation sent a prompt")
	}
}

// Rounds share the attempt's send budget: a round cannot buy more sends
// than the attempt was admitted with.
func TestARoundDrawsOnTheAttemptsSendBudget(t *testing.T) {
	f := newRepairFixture(t, ciRepairSnapshot, func(config *engine.Config) { config.MaxAttemptSends = 4 },
		chainSteps()...)
	continuation := f.execute(t).Continuation
	defer continuation.Release()
	f.fake.Append(
		writes(map[string]string{"src/fix.txt": "fixed"}, "fixed"),
		success("fine", map[string]any{"approved": true}),
	)
	round := continuation.RepairCI(context.Background(), redLint)
	if round.State != protocol.AttemptFailed || f.fake.Remaining() != 1 {
		t.Fatalf("round = %+v remaining=%d, want failed on the 5th send of a 4-send budget",
			round, f.fake.Remaining())
	}
	if !f.sink.has(protocol.EventError, "attempt_send_budget_exhausted") {
		t.Fatal("no send-budget terminal event")
	}
}

// Rounds share the attempt's wall-clock ceiling, measured from the chain's
// start rather than restarted per round.
func TestARoundDrawsOnTheAttemptsCeiling(t *testing.T) {
	f := newRepairFixture(t, ciRepairSnapshot, func(config *engine.Config) {
		config.AttemptCeiling = 1500 * time.Millisecond
	}, chainSteps()...)
	continuation := f.execute(t).Continuation
	defer continuation.Release()
	time.Sleep(1600 * time.Millisecond)
	round := continuation.RepairCI(context.Background(), redLint)
	if round.State != protocol.AttemptFailed || len(f.fake.Calls()) != 3 {
		t.Fatalf("round = %+v calls=%d, want failed on the ceiling before any send",
			round, len(f.fake.Calls()))
	}
	if !f.sink.has(protocol.EventError, "attempt_ceiling_exceeded") {
		t.Fatal("no ceiling terminal event")
	}
}

// Cancellation reaches a round the way it reaches the chain.
func TestACancelledAttemptEndsItsRoundBeforeAnySend(t *testing.T) {
	f := newRepairFixture(t, ciRepairSnapshot, nil, chainSteps()...)
	continuation := f.execute(t).Continuation
	defer continuation.Release()
	close(f.cancel)
	round := continuation.RepairCI(context.Background(), redLint)
	if round.State != protocol.AttemptCancelled || len(f.fake.Calls()) != 3 {
		t.Fatalf("round = %+v calls=%d, want cancelled before any send", round, len(f.fake.Calls()))
	}
}

// The repair phase runs regardless of its own `if:` guard, as every repair
// dispatch does: a guard decides chain membership, not repair eligibility.
func TestTheRepairPhaseIgnoresItsOwnIfGuard(t *testing.T) {
	snapshot := `
name: guarded
roster:
  builder:
    model: test-model
    system_prompt: Build.
    user_prompt: "Build."
    writes: ["src/"]
phases:
  - {name: build, kind: agent, owner: builder, if: "rebuild == yes"}
  - {name: check, kind: code, command: "true"}
acceptance: [all_phases_passed]
publish:
  ci:
    wait: true
    on_fail: {run: build, budget: 1}
`
	f := newRepairFixture(t, snapshot, nil)
	outcome := f.execute(t)
	if outcome.Continuation == nil || len(f.fake.Calls()) != 0 {
		t.Fatalf("chain = %+v calls=%d, want accepted with build skipped by its guard",
			outcome, len(f.fake.Calls()))
	}
	defer outcome.Continuation.Release()
	f.fake.Append(writes(map[string]string{"src/fix.txt": "fixed"}, "fixed"))
	round := outcome.Continuation.RepairCI(context.Background(), redLint)
	if round.State != protocol.AttemptAcceptedUnpublished || len(f.fake.Calls()) != 1 {
		t.Fatalf("round = %+v calls=%d, want the guarded build to run once", round, len(f.fake.Calls()))
	}
}

// The wiped HOME is re-provisioned for a round: each role the round uses is
// seeded again, or its agent would run without the CLI auth it needs.
func TestARoundReseedsTheWipedHomeForEveryRoleItUses(t *testing.T) {
	seeded := map[string]int{}
	f := newRepairFixture(t, ciRepairSnapshot, func(config *engine.Config) {
		config.SeedHome = func(home, role string, _ protocol.RoleSpec) error {
			seeded[role]++
			return os.WriteFile(filepath.Join(home, "auth-"+role), []byte("token"), 0o600)
		}
	}, chainSteps()...)
	continuation := f.execute(t).Continuation
	defer continuation.Release()
	f.fake.Append(
		writes(map[string]string{"src/fix.txt": "fixed"}, "fixed"),
		success("fine", map[string]any{"approved": true}),
	)
	if round := continuation.RepairCI(context.Background(), redLint); round.State != protocol.AttemptAcceptedUnpublished {
		t.Fatalf("round = %+v, want accepted", round)
	}
	if seeded["builder"] != 2 || seeded["reviewer"] != 2 {
		t.Fatalf("seed calls = %v, want each role seeded once for the chain and once for the round", seeded)
	}
}
