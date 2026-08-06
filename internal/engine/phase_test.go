// phase_test.go — the U4 engine suite, built against the scripted fake
// runtime (the execution note's test-first mandate). Each test is one plan
// scenario, named as the behavioral sentence it proves.
package engine_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/StructuPath/jig/internal/engine"
	"github.com/StructuPath/jig/internal/engine/enginetest"
	"github.com/StructuPath/jig/internal/protocol"
	"github.com/StructuPath/jig/internal/runtime"
	"github.com/StructuPath/jig/internal/runtime/claudecode"
)

// ---- fixtures --------------------------------------------------------------

func initRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		command := exec.Command("git", args...)
		command.Dir = dir
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, output)
		}
	}
	git("init", "--quiet")
	git("config", "user.email", "test@jig.local")
	git("config", "user.name", "jig test")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", "README.md")
	git("commit", "--quiet", "-m", "init")
	return dir
}

func testAttempt(snapshot string, parameters map[string]string, worktree string) engine.Attempt {
	return engine.Attempt{
		Claim: protocol.Claim{
			Attempt:    protocol.Attempt{ID: "attempt-1", JobID: "job-1", AttemptNumber: 1},
			Job:        protocol.Job{ID: "job-1", Repository: "fixture"},
			Snapshot:   snapshot,
			Parameters: parameters,
		},
		WorktreePath: worktree,
	}
}

type recordingSink struct {
	mutex  sync.Mutex
	events []protocol.Event
}

func (s *recordingSink) Emit(event protocol.Event) error {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	s.events = append(s.events, event)
	return nil
}

func (s *recordingSink) all() []protocol.Event {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	return append([]protocol.Event(nil), s.events...)
}

func (s *recordingSink) count(eventType, name string) int {
	total := 0
	for _, event := range s.all() {
		if event.Type == eventType && (name == "" || event.Name == name) {
			total++
		}
	}
	return total
}

func (s *recordingSink) has(eventType, name string) bool { return s.count(eventType, name) > 0 }

func newTestRunner(t *testing.T, rt runtime.Runtime, sink engine.EventSink, mutate func(*engine.Config)) *engine.Runner {
	t.Helper()
	config := engine.Config{
		Runtime:         rt,
		Sink:            sink,
		ScratchRoot:     t.TempDir(),
		BaseEnv:         []string{"PATH=" + os.Getenv("PATH"), "FOO=bar", "SECRET=hush"},
		PhaseTimeout:    time.Minute,
		NoOutputTimeout: time.Minute,
		AttemptCeiling:  2 * time.Minute,
	}
	if mutate != nil {
		mutate(&config)
	}
	runner, err := engine.New(config)
	if err != nil {
		t.Fatal(err)
	}
	return runner
}

func envelope(fields map[string]any) string { return enginetest.EnvelopeText(fields) }

const oneWriterRoster = `
roster:
  writer:
    model: test-model
    system_prompt: You write files.
    user_prompt: "Do the work."
    env: [FOO]
    writes: ["out.txt", "src/", "plan.md"]
`

// ---- scenario: parsing (R7) ------------------------------------------------

func TestProseWrappedValidJSONParses(t *testing.T) {
	repo := initRepo(t)
	fake := enginetest.New(enginetest.Step{
		Text: "Sure, here is my report:\n```json\n" +
			envelope(map[string]any{"status": "success", "summary": "done"}) + "\n```\nHope that helps!",
	})
	sink := &recordingSink{}
	runner := newTestRunner(t, fake, sink, nil)

	snapshot := "name: t\n" + oneWriterRoster + `
phases:
  - {name: build, kind: agent, owner: writer}
acceptance: [all_phases_passed]
`
	outcome := runner.Execute(context.Background(), testAttempt(snapshot, nil, repo))
	if outcome.State != protocol.AttemptAcceptedUnpublished {
		t.Fatalf("state = %q (%s), want accepted_unpublished", outcome.State, outcome.Error)
	}
	if fake.Remaining() != 0 {
		t.Fatalf("unconsumed scripted steps: %d", fake.Remaining())
	}
}

func TestInvalidJSONPastBudgetPersistsEveryInvalidEnvelopeAndFailsThePhase(t *testing.T) {
	repo := initRepo(t)
	steps := make([]enginetest.Step, 0, protocol.ParseBudgetPerEmission+1)
	for i := 0; i <= protocol.ParseBudgetPerEmission; i++ {
		steps = append(steps, enginetest.Step{Text: "still not json, attempt " + fmt.Sprint(i)})
	}
	fake := enginetest.New(steps...)
	sink := &recordingSink{}
	runner := newTestRunner(t, fake, sink, nil)

	snapshot := "name: t\n" + oneWriterRoster + `
phases:
  - {name: build, kind: agent, owner: writer}
`
	outcome := runner.Execute(context.Background(), testAttempt(snapshot, nil, repo))
	if outcome.State != protocol.AttemptFailed {
		t.Fatalf("state = %q, want failed", outcome.State)
	}
	if !strings.Contains(outcome.Error, "never produced a valid envelope") {
		t.Fatalf("error %q does not name the parse exhaustion", outcome.Error)
	}
	// Every invalid attempt persists: initial + ParseBudgetPerEmission corrections.
	wantInvalid := protocol.ParseBudgetPerEmission + 1
	if got := sink.count(protocol.EventLog, "invalid_envelope"); got != wantInvalid {
		t.Fatalf("invalid_envelope events = %d, want %d", got, wantInvalid)
	}
	// Corrections re-entered the SAME live session.
	calls := fake.Calls()
	for i := 1; i < len(calls); i++ {
		if calls[i].NativeID != calls[0].NativeID {
			t.Fatalf("correction %d used session %q, want the original %q",
				i, calls[i].NativeID, calls[0].NativeID)
		}
	}
	if fake.Remaining() != 0 {
		t.Fatalf("unconsumed scripted steps: %d", fake.Remaining())
	}
}

func TestGateCorrectedEmissionGetsItsOwnFreshParseBudget(t *testing.T) {
	repo := initRepo(t)
	valid := envelope(map[string]any{"status": "success", "summary": "wrote it",
		"artifacts": []any{"out.txt"}})
	fake := enginetest.New(
		enginetest.Step{Text: valid},              // parses, but out.txt does not exist -> gate fails
		enginetest.Step{Text: "sorry, malformed"}, // corrected emission, parse failure 1
		enginetest.Step{Text: "still malformed"},  // parse failure 2 — inside a FRESH budget
		enginetest.Step{Files: map[string]string{"out.txt": "data"}, Text: valid},
	)
	sink := &recordingSink{}
	runner := newTestRunner(t, fake, sink, nil)

	snapshot := "name: t\n" + oneWriterRoster + `
phases:
  - name: build
    kind: agent
    owner: writer
    gates:
      - {name: artifacts_exist}
acceptance: [all_phases_passed]
`
	outcome := runner.Execute(context.Background(), testAttempt(snapshot, nil, repo))
	if outcome.State != protocol.AttemptAcceptedUnpublished {
		t.Fatalf("state = %q (%s), want accepted_unpublished", outcome.State, outcome.Error)
	}
	if got := sink.count(protocol.EventLog, "invalid_envelope"); got != 2 {
		t.Fatalf("invalid_envelope events = %d, want 2", got)
	}
	if fake.Remaining() != 0 {
		t.Fatalf("unconsumed scripted steps: %d", fake.Remaining())
	}
}

// ---- scenario: gates (R9) --------------------------------------------------

func TestGateViolationCorrectsSameSessionAndRerunsAllGates(t *testing.T) {
	repo := initRepo(t)
	claim := envelope(map[string]any{"status": "success", "summary": "wrote a.txt",
		"artifacts": []any{"out.txt"}})
	fake := enginetest.New(
		// out.txt exists but is empty: artifacts_exist passes, files_non_empty fails.
		enginetest.Step{Files: map[string]string{"out.txt": ""}, Text: claim},
		enginetest.Step{Files: map[string]string{"out.txt": "content"}, Text: claim},
	)
	sink := &recordingSink{}
	runner := newTestRunner(t, fake, sink, nil)

	snapshot := "name: t\n" + oneWriterRoster + `
phases:
  - name: build
    kind: agent
    owner: writer
    gates:
      - {name: artifacts_exist}
      - {name: files_non_empty}
acceptance: [all_phases_passed]
`
	outcome := runner.Execute(context.Background(), testAttempt(snapshot, nil, repo))
	if outcome.State != protocol.AttemptAcceptedUnpublished {
		t.Fatalf("state = %q (%s), want accepted_unpublished", outcome.State, outcome.Error)
	}
	calls := fake.Calls()
	if len(calls) != 2 {
		t.Fatalf("calls = %d, want 2", len(calls))
	}
	if calls[1].NativeID != calls[0].NativeID {
		t.Fatalf("gate correction went to session %q, want the live session %q",
			calls[1].NativeID, calls[0].NativeID)
	}
	if !strings.Contains(calls[1].Prompt, "failed validation") {
		t.Fatalf("correction prompt does not name the violations: %.120q", calls[1].Prompt)
	}
	// The corrected envelope re-ran ALL gates, not just the failed one.
	if got := sink.count(protocol.EventGatePass, "artifacts_exist"); got != 2 {
		t.Fatalf("artifacts_exist ran %d time(s), want 2 (re-run after correction)", got)
	}
	if sink.count(protocol.EventGateFail, "files_non_empty") != 1 ||
		sink.count(protocol.EventGatePass, "files_non_empty") != 1 {
		t.Fatal("files_non_empty should fail once then pass once")
	}
}

func TestEnvelopeStatusFailFailsThePhaseEvenThoughItParsedAndGatedClean(t *testing.T) {
	repo := initRepo(t)
	fake := enginetest.New(enginetest.Step{
		Text: envelope(map[string]any{"status": "fail", "summary": "I could not do it"}),
	})
	runner := newTestRunner(t, fake, &recordingSink{}, nil)

	snapshot := "name: t\n" + oneWriterRoster + `
phases:
  - {name: build, kind: agent, owner: writer}
`
	outcome := runner.Execute(context.Background(), testAttempt(snapshot, nil, repo))
	if outcome.State != protocol.AttemptFailed {
		t.Fatalf("state = %q, want failed", outcome.State)
	}
	if !strings.Contains(outcome.Error, `status="fail"`) {
		t.Fatalf("error %q does not name the agent's own verdict", outcome.Error)
	}
}

// ---- scenario: repair edges (KTD2) -----------------------------------------

const repairRoster = `
roster:
  builder:
    model: test-model
    system_prompt: You fix things.
    user_prompt: "Fix it."
    writes: ["src/"]
`

func TestCodePhaseFailureRoutesAdapterEnvelopeThroughItsRepairEdge(t *testing.T) {
	repo := initRepo(t)
	fake := enginetest.New(
		enginetest.Step{Text: envelope(map[string]any{"status": "success", "summary": "built"})},
		// Repair dispatch: the builder receives the adapter envelope and fixes.
		enginetest.Step{Files: map[string]string{"src/fixed.txt": "ok"},
			Text: envelope(map[string]any{"status": "success", "summary": "fixed"})},
	)
	sink := &recordingSink{}
	runner := newTestRunner(t, fake, sink, nil)

	snapshot := "name: t\n" + repairRoster + `
phases:
  - {name: build, kind: agent, owner: builder}
  - name: test
    kind: code
    command: "test -f src/fixed.txt"
    on_fail: {run: build, then: rerun-self, budget: 2}
acceptance: [all_phases_passed]
`
	outcome := runner.Execute(context.Background(), testAttempt(snapshot, nil, repo))
	if outcome.State != protocol.AttemptAcceptedUnpublished {
		t.Fatalf("state = %q (%s), want accepted_unpublished", outcome.State, outcome.Error)
	}
	// The repair-dispatched builder saw the adapter envelope as its previous.
	calls := fake.Calls()
	if len(calls) != 2 {
		t.Fatalf("calls = %d, want 2", len(calls))
	}
	if !strings.Contains(calls[1].Prompt, `"passed":false`) &&
		!strings.Contains(calls[1].Prompt, `"passed": false`) {
		t.Fatalf("repair prompt does not carry the adapter envelope: %.400q", calls[1].Prompt)
	}
	if !sink.has(protocol.EventLog, "repair_edge") {
		t.Fatal("no repair_edge trace event")
	}
}

func TestRepairEdgeExhaustionHonorsFailJobVersusProceed(t *testing.T) {
	buildStep := func() enginetest.Step {
		return enginetest.Step{Text: envelope(map[string]any{"status": "success", "summary": "tried"})}
	}
	reportStep := enginetest.Step{Text: envelope(map[string]any{"status": "success", "summary": "report"})}

	chain := func(exhausted string) string {
		return "name: t\n" + repairRoster + `
phases:
  - {name: build, kind: agent, owner: builder}
  - name: test
    kind: code
    command: "false"
    on_fail: {run: build, then: rerun-self, budget: 1, exhausted: ` + exhausted + `}
  - {name: report, kind: agent, owner: builder}
`
	}

	t.Run("fail-job stops the chain", func(t *testing.T) {
		repo := initRepo(t)
		fake := enginetest.New(buildStep(), buildStep()) // chain build + one repair dispatch
		sink := &recordingSink{}
		runner := newTestRunner(t, fake, sink, nil)
		outcome := runner.Execute(context.Background(), testAttempt(chain("fail-job"), nil, repo))
		if outcome.State != protocol.AttemptFailed {
			t.Fatalf("state = %q, want failed", outcome.State)
		}
		if !strings.Contains(outcome.Error, "repair budget") {
			t.Fatalf("error %q does not name the exhausted budget", outcome.Error)
		}
		if fake.Remaining() != 0 {
			t.Fatalf("report phase ran after fail-job exhaustion (remaining %d)", fake.Remaining())
		}
	})

	t.Run("proceed continues the chain", func(t *testing.T) {
		repo := initRepo(t)
		fake := enginetest.New(buildStep(), buildStep(), reportStep)
		sink := &recordingSink{}
		runner := newTestRunner(t, fake, sink, nil)
		outcome := runner.Execute(context.Background(), testAttempt(chain("proceed"), nil, repo))
		// The chain proceeded (report ran), but R12 still holds: a failed
		// phase can never yield an accepted job.
		if fake.Remaining() != 0 {
			t.Fatalf("report phase did not run after proceed exhaustion (remaining %d)", fake.Remaining())
		}
		if outcome.State != protocol.AttemptFailed {
			t.Fatalf("state = %q, want failed (all_phases_passed cannot hold)", outcome.State)
		}
		if !strings.Contains(outcome.Error, "acceptance predicate failed") {
			t.Fatalf("error %q should be the acceptance verdict, not a chain stop", outcome.Error)
		}
		if !sink.has(protocol.EventLog, "repair_exhausted") {
			t.Fatal("no repair_exhausted trace event")
		}
	})
}

// ---- scenario: the simple-sdlc shape ---------------------------------------

const sdlcSnapshot = `
name: sdlc
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
  - {name: build, kind: agent, owner: builder}
  - {name: test, kind: code, command: "test -f src/app.txt"}
  - name: review
    kind: agent
    owner: reviewer
    on_fail: {when: "approved == false", run: build, then: rerun-self, budget: 2, exhausted: proceed}
  - {name: retest, kind: code, command: "test -f src/app.txt", if: revised}
acceptance: [all_phases_passed]
`

func TestSimpleSdlcChainRunsEndToEndOnTheScriptedRuntime(t *testing.T) {
	repo := initRepo(t)
	fake := enginetest.New(
		enginetest.Step{Files: map[string]string{"src/app.txt": "v1"},
			Text: envelope(map[string]any{"status": "success", "summary": "built v1"})},
		enginetest.Step{Text: envelope(map[string]any{"status": "success", "approved": false,
			"summary": "needs work", "blocking": []any{"missing error handling"}})},
		// Review rejection dispatches the builder (revision)...
		enginetest.Step{Files: map[string]string{"src/app.txt": "v2"},
			Text: envelope(map[string]any{"status": "success", "revised": true, "summary": "revised"})},
		// ...then re-reviews.
		enginetest.Step{Text: envelope(map[string]any{"status": "success", "approved": true,
			"summary": "looks good"})},
	)
	sink := &recordingSink{}
	runner := newTestRunner(t, fake, sink, nil)
	outcome := runner.Execute(context.Background(), testAttempt(sdlcSnapshot, nil, repo))
	if outcome.State != protocol.AttemptAcceptedUnpublished {
		t.Fatalf("state = %q (%s), want accepted_unpublished", outcome.State, outcome.Error)
	}
	if fake.Remaining() != 0 {
		t.Fatalf("unconsumed scripted steps: %d", fake.Remaining())
	}
	// `if: revised` fired: retest ran, and was not skipped.
	retestStarted := false
	for _, event := range sink.all() {
		if event.Type == protocol.EventPhaseStart && event.Phase == "retest" {
			retestStarted = true
		}
	}
	if !retestStarted {
		t.Fatal("retest did not run although a revision occurred")
	}
	if sink.has(protocol.EventLog, "phase_skipped") {
		t.Fatal("no phase should have been skipped in the revision path")
	}
}

func TestSimpleSdlcRetestIsSkippedWhenNoRevisionOccurred(t *testing.T) {
	repo := initRepo(t)
	fake := enginetest.New(
		enginetest.Step{Files: map[string]string{"src/app.txt": "v1"},
			Text: envelope(map[string]any{"status": "success", "summary": "built"})},
		enginetest.Step{Text: envelope(map[string]any{"status": "success", "approved": true,
			"summary": "approved first pass"})},
	)
	sink := &recordingSink{}
	runner := newTestRunner(t, fake, sink, nil)
	outcome := runner.Execute(context.Background(), testAttempt(sdlcSnapshot, nil, repo))
	if outcome.State != protocol.AttemptAcceptedUnpublished {
		t.Fatalf("state = %q (%s), want accepted_unpublished", outcome.State, outcome.Error)
	}
	if !sink.has(protocol.EventLog, "phase_skipped") {
		t.Fatal("retest should have been skipped: no revision occurred")
	}
	for _, event := range sink.all() {
		if event.Type == protocol.EventPhaseStart && event.Phase == "retest" {
			t.Fatal("retest ran although no revision occurred")
		}
	}
}

// ---- scenario: write boundary (R10) ----------------------------------------

func TestOutOfAllowlistWritesRollBackAndAbortTheAttemptWithoutRetry(t *testing.T) {
	repo := initRepo(t)
	// Pre-existing operator dirt: README.md modified before the phase.
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("operator edit\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	fake := enginetest.New(enginetest.Step{
		Files: map[string]string{
			"src/ok.txt": "legit",   // inside the allowlist
			"stray.txt":  "sneaky",  // outside — must be rolled back
			"README.md":  "hello\n", // reverts the operator's dirty file — a reversion is a modification
		},
		Text: envelope(map[string]any{"status": "success", "summary": "did things"}),
	})
	sink := &recordingSink{}
	runner := newTestRunner(t, fake, sink, nil)

	snapshot := "name: t\n" + repairRoster + `
phases:
  - {name: build, kind: agent, owner: builder}
`
	outcome := runner.Execute(context.Background(), testAttempt(snapshot, nil, repo))
	if outcome.State != protocol.AttemptFailed {
		t.Fatalf("state = %q, want failed (aborted)", outcome.State)
	}
	if !strings.Contains(outcome.Error, "outside its write allowlist") {
		t.Fatalf("error %q does not name the breach", outcome.Error)
	}
	if !strings.Contains(outcome.Error, "stray.txt") || !strings.Contains(outcome.Error, "README.md") {
		t.Fatalf("error %q does not name every offending path", outcome.Error)
	}
	if _, err := os.Stat(filepath.Join(repo, "stray.txt")); !os.IsNotExist(err) {
		t.Fatal("stray.txt was not rolled back")
	}
	// Breach is never retried: exactly one phase entry, one send.
	if got := sink.count(protocol.EventPhaseStart, ""); got != 1 {
		t.Fatalf("phase entries = %d, want 1 (breach consumes no retry)", got)
	}
	if !sink.has(protocol.EventError, "write_boundary_breach") {
		t.Fatal("no write_boundary_breach trace event")
	}
}

// ---- scenario: ephemeral HOME and env (R17, R21) ---------------------------

func TestAgentHomeIsEphemeralAndEnvIsOnlyTheRoleAllowlist(t *testing.T) {
	repo := initRepo(t)
	scratchRoot := t.TempDir()
	var seededRole string
	fake := enginetest.New(
		enginetest.Step{
			Files: map[string]string{"~/.gitconfig": "[user]\n\tname = agent\n"},
			Text:  envelope(map[string]any{"status": "success", "summary": "configured"}),
		},
	)
	sink := &recordingSink{}
	runner := newTestRunner(t, fake, sink, func(config *engine.Config) {
		config.ScratchRoot = scratchRoot
		config.SeedHome = func(home, role string, _ protocol.RoleSpec) error {
			seededRole = role
			return os.WriteFile(filepath.Join(home, ".credentials"), []byte("fake-token"), 0o600)
		}
	})

	// The code phase proves, DURING the attempt, that both the agent's write
	// and the seeded credential landed in the ephemeral HOME.
	snapshot := "name: t\n" + oneWriterRoster + `
phases:
  - {name: build, kind: agent, owner: writer}
  - {name: verify-home, kind: code, command: "test -f \"$HOME/.gitconfig\" && test -f \"$HOME/.credentials\""}
acceptance: [all_phases_passed]
`
	outcome := runner.Execute(context.Background(), testAttempt(snapshot, nil, repo))
	if outcome.State != protocol.AttemptAcceptedUnpublished {
		t.Fatalf("state = %q (%s), want accepted_unpublished", outcome.State, outcome.Error)
	}
	if seededRole != "writer" {
		t.Fatalf("seeding hook saw role %q, want writer", seededRole)
	}

	env := fake.Calls()[0].Options.Env
	joined := strings.Join(env, "\n")
	if !strings.Contains(joined, "FOO=bar") {
		t.Fatal("allowlisted FOO missing from the subprocess env")
	}
	if strings.Contains(joined, "SECRET=") {
		t.Fatal("SECRET leaked into the subprocess env — the allowlist is not exclusive")
	}
	var home string
	for _, entry := range env {
		if value, found := strings.CutPrefix(entry, "HOME="); found {
			home = value
		}
	}
	if home == "" || !strings.HasPrefix(home, scratchRoot) {
		t.Fatalf("subprocess HOME %q is not the jig-managed ephemeral home", home)
	}
	if realHome, _ := os.UserHomeDir(); home == realHome {
		t.Fatal("subprocess HOME is the operator's real home")
	}
	// Destroyed with the attempt: nothing the agent wrote to its home survives.
	if _, err := os.Stat(filepath.Join(scratchRoot, "attempt-1")); !os.IsNotExist(err) {
		t.Fatal("ephemeral HOME survived the attempt")
	}
}

// ---- scenario: watchdogs and ceiling (R11) ---------------------------------

func TestSilentRuntimeIsKilledPhaseReentersOnceWithCleanWorktreeAndFreshSession(t *testing.T) {
	repo := initRepo(t)
	fake := enginetest.New(
		enginetest.Step{Files: map[string]string{"src/junk.txt": "half-done"}, Hang: true},
		enginetest.Step{Text: envelope(map[string]any{"status": "success", "summary": "recovered"})},
	)
	sink := &recordingSink{}
	runner := newTestRunner(t, fake, sink, func(config *engine.Config) {
		config.NoOutputTimeout = 100 * time.Millisecond
	})

	snapshot := "name: t\n" + repairRoster + `
phases:
  - {name: build, kind: agent, owner: builder}
acceptance: [all_phases_passed]
`
	outcome := runner.Execute(context.Background(), testAttempt(snapshot, nil, repo))
	if outcome.State != protocol.AttemptAcceptedUnpublished {
		t.Fatalf("state = %q (%s), want accepted_unpublished", outcome.State, outcome.Error)
	}
	// The trace shows both phase attempts and the envelope-less death.
	if got := sink.count(protocol.EventPhaseStart, ""); got != 2 {
		t.Fatalf("phase_start events = %d, want 2 (both attempts in the trace)", got)
	}
	if sink.count(protocol.EventPhaseDeath, "") != 1 {
		t.Fatal("no phase_death terminal event for the killed attempt")
	}
	// Clean worktree: the dead attempt's half-done write was rolled back.
	if _, err := os.Stat(filepath.Join(repo, "src", "junk.txt")); !os.IsNotExist(err) {
		t.Fatal("dead phase's write survived the rollback")
	}
	// Fresh session: the retry did not resume the killed conversation.
	calls := fake.Calls()
	if len(calls) != 2 {
		t.Fatalf("calls = %d, want 2", len(calls))
	}
	if calls[1].SessionKey == calls[0].SessionKey || calls[1].Send != 1 {
		t.Fatalf("retry reused the killed session (%q -> %q, send %d)",
			calls[0].SessionKey, calls[1].SessionKey, calls[1].Send)
	}
}

func TestAttemptExceedingItsCeilingTerminatesWithItsOwnTerminalEvent(t *testing.T) {
	repo := initRepo(t)
	fake := enginetest.New(enginetest.Step{Hang: true})
	sink := &recordingSink{}
	runner := newTestRunner(t, fake, sink, func(config *engine.Config) {
		config.AttemptCeiling = 100 * time.Millisecond
		config.NoOutputTimeout = time.Minute // the ceiling must fire first
	})

	snapshot := "name: t\n" + repairRoster + `
phases:
  - {name: build, kind: agent, owner: builder}
`
	outcome := runner.Execute(context.Background(), testAttempt(snapshot, nil, repo))
	if outcome.State != protocol.AttemptFailed {
		t.Fatalf("state = %q, want failed", outcome.State)
	}
	if !strings.Contains(outcome.Error, "ceiling") {
		t.Fatalf("error %q does not name the ceiling", outcome.Error)
	}
	if !sink.has(protocol.EventError, "attempt_ceiling_exceeded") {
		t.Fatal("the ceiling did not get its own terminal event")
	}
}

// ---- scenario: acceptance predicate (R12) ----------------------------------

func TestAllPhasesPassButAcceptancePredicateFailsWithEvidence(t *testing.T) {
	repo := initRepo(t)
	fake := enginetest.New(enginetest.Step{
		Text: envelope(map[string]any{"status": "success", "summary": "built, nothing revised"}),
	})
	sink := &recordingSink{}
	runner := newTestRunner(t, fake, sink, nil)

	// retest is guarded and never runs, so its declared tests_pass gate never
	// records evidence — every phase passed, the declared bar did not.
	snapshot := "name: t\n" + repairRoster + `
phases:
  - {name: build, kind: agent, owner: builder}
  - name: retest
    kind: code
    command: "true"
    if: revised
    gates:
      - {name: tests_pass, command: "true"}
acceptance: [all_phases_passed, tests_pass]
`
	outcome := runner.Execute(context.Background(), testAttempt(snapshot, nil, repo))
	if outcome.State != protocol.AttemptFailed {
		t.Fatalf("state = %q, want failed", outcome.State)
	}
	if !strings.HasPrefix(outcome.Error, "acceptance predicate failed") {
		t.Fatalf("error %q is not the acceptance verdict — it must be distinct from a phase failure",
			outcome.Error)
	}
	if !strings.Contains(outcome.Error, "tests_pass") {
		t.Fatalf("error %q does not carry the failing check's evidence", outcome.Error)
	}
	if !strings.Contains(outcome.Result, `"all_phases_passed"`) {
		t.Fatalf("result payload does not carry the acceptance evidence: %.400q", outcome.Result)
	}
	if !sink.has(protocol.EventLog, "acceptance") {
		t.Fatal("no acceptance trace event")
	}
}

// ---- scenario: can-resume=false degraded path (R7) -------------------------

func TestCanResumeFalseCorrectionsReplayTranscriptDigestsIntoFreshSessions(t *testing.T) {
	repo := initRepo(t)
	fake := enginetest.New(
		enginetest.Step{Text: "not json at all"},
		enginetest.Step{Text: envelope(map[string]any{"status": "success", "summary": "second try"})},
	)
	fake.CanResume = false
	sink := &recordingSink{}
	runner := newTestRunner(t, fake, sink, nil)

	snapshot := "name: t\n" + repairRoster + `
phases:
  - {name: build, kind: agent, owner: builder}
acceptance: [all_phases_passed]
`
	outcome := runner.Execute(context.Background(), testAttempt(snapshot, nil, repo))
	if outcome.State != protocol.AttemptAcceptedUnpublished {
		t.Fatalf("state = %q (%s), want accepted_unpublished", outcome.State, outcome.Error)
	}
	calls := fake.Calls()
	if len(calls) != 2 {
		t.Fatalf("calls = %d, want 2", len(calls))
	}
	if calls[1].SessionKey == calls[0].SessionKey {
		t.Fatal("correction reused the session although the runtime cannot resume")
	}
	if calls[1].Send != 1 {
		t.Fatalf("correction send = %d, want 1 (fresh session)", calls[1].Send)
	}
	if !strings.Contains(calls[1].Prompt, "Transcript digest") ||
		!strings.Contains(calls[1].Prompt, "not json at all") {
		t.Fatalf("correction prompt is not a transcript-digest replay: %.200q", calls[1].Prompt)
	}
	if !sink.has(protocol.EventLog, "degraded_correction") {
		t.Fatal("the trace does not mark the elevated-cost path")
	}
}

// ---- verification: three-phase chain, exact event sequence -----------------

func TestThreePhaseChainEmitsTheExactExpectedEventSequence(t *testing.T) {
	repo := initRepo(t)
	planEnvelope := envelope(map[string]any{"status": "success", "summary": "planned",
		"artifacts": []any{"plan.md"}})
	fixEnvelope := envelope(map[string]any{"status": "success", "summary": "fixed",
		"artifacts": []any{"src/fixed.txt"}})
	fake := enginetest.New(
		enginetest.Step{
			Files:  map[string]string{"plan.md": "the plan"},
			Events: []runtime.Event{{Kind: runtime.EventToolCall, Name: "Write"}},
			Text:   planEnvelope,
		},
		enginetest.Step{Files: map[string]string{"src/fixed.txt": "ok"}, Text: fixEnvelope},
		enginetest.Step{Text: envelope(map[string]any{"status": "success", "summary": "reported"})},
	)
	sink := &recordingSink{}
	runner := newTestRunner(t, fake, sink, nil)

	snapshot := "name: t\n" + oneWriterRoster + `
phases:
  - name: plan
    kind: agent
    owner: writer
    gates:
      - {name: artifacts_exist}
  - name: test
    kind: code
    command: "test -f src/fixed.txt"
    on_fail: {run: plan, then: rerun-self, budget: 1}
  - {name: report, kind: agent, owner: writer}
acceptance: [all_phases_passed]
`
	outcome := runner.Execute(context.Background(), testAttempt(snapshot, nil, repo))
	if outcome.State != protocol.AttemptAcceptedUnpublished {
		t.Fatalf("state = %q (%s), want accepted_unpublished", outcome.State, outcome.Error)
	}

	type triple struct{ Type, Phase, Name string }
	want := []triple{
		{protocol.EventLog, "", "attempt_start"},
		{protocol.EventPhaseStart, "plan", ""},
		{protocol.EventAgentStart, "plan", "writer"},
		{protocol.EventToolCall, "plan", "Write"},
		{protocol.EventGatePass, "plan", "artifacts_exist"},
		{protocol.EventLog, "plan", "paths_touched"},
		{protocol.EventHandoff, "plan", "writer"},
		{protocol.EventAgentEnd, "plan", "writer"},
		{protocol.EventPhaseEnd, "plan", ""},
		{protocol.EventPhaseStart, "test", ""},
		{protocol.EventLog, "test", "command_result"},
		{protocol.EventPhaseEnd, "test", ""},
		{protocol.EventLog, "test", "repair_edge"},
		{protocol.EventPhaseStart, "plan", ""},
		{protocol.EventAgentStart, "plan", "writer"},
		{protocol.EventGatePass, "plan", "artifacts_exist"},
		{protocol.EventLog, "plan", "paths_touched"},
		{protocol.EventHandoff, "plan", "writer"},
		{protocol.EventAgentEnd, "plan", "writer"},
		{protocol.EventPhaseEnd, "plan", ""},
		{protocol.EventPhaseStart, "test", ""},
		{protocol.EventLog, "test", "command_result"},
		{protocol.EventPhaseEnd, "test", ""},
		{protocol.EventPhaseStart, "report", ""},
		{protocol.EventAgentStart, "report", "writer"},
		{protocol.EventHandoff, "report", "writer"},
		{protocol.EventAgentEnd, "report", "writer"},
		{protocol.EventPhaseEnd, "report", ""},
		{protocol.EventLog, "", "acceptance"},
	}
	events := sink.all()
	if len(events) != len(want) {
		var got []string
		for _, event := range events {
			got = append(got, fmt.Sprintf("%s/%s/%s", event.Type, event.Phase, event.Name))
		}
		t.Fatalf("event count = %d, want %d:\n%s", len(events), len(want), strings.Join(got, "\n"))
	}
	for i, event := range events {
		if event.Type != want[i].Type || event.Phase != want[i].Phase || event.Name != want[i].Name {
			t.Fatalf("event %d = %s/%s/%s, want %s/%s/%s",
				i, event.Type, event.Phase, event.Name, want[i].Type, want[i].Phase, want[i].Name)
		}
		if event.Seq != int64(i+1) {
			t.Fatalf("event %d seq = %d, want %d (per-attempt monotonic)", i, event.Seq, i+1)
		}
	}
}

// ---- cancellation ----------------------------------------------------------

func TestCancellationDuringAnAgentPhaseKillsTheSendAndReportsCancelled(t *testing.T) {
	repo := initRepo(t)
	// The step hangs, so cancellation must interrupt a send in flight — the
	// "during phases" half of the cancellation contract.
	fake := enginetest.New(enginetest.Step{Hang: true})
	cancelled := make(chan struct{})
	go func() {
		time.Sleep(100 * time.Millisecond)
		close(cancelled)
	}()
	runner := newTestRunner(t, fake, &recordingSink{}, nil)

	snapshot := "name: t\n" + repairRoster + `
phases:
  - {name: build, kind: agent, owner: builder}
`
	attempt := testAttempt(snapshot, nil, repo)
	attempt.Cancelled = cancelled
	outcome := runner.Execute(context.Background(), attempt)
	if outcome.State != protocol.AttemptCancelled {
		t.Fatalf("state = %q, want cancelled", outcome.State)
	}
	if !strings.Contains(outcome.Error, "during phase") {
		t.Fatalf("error %q should show the cancel landed mid-phase", outcome.Error)
	}
}

// ---- JSONL sink ------------------------------------------------------------

func TestJSONLSinkWritesOneOrderedLinePerEvent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "trace.jsonl")
	sink, err := engine.NewJSONLSink(path)
	if err != nil {
		t.Fatal(err)
	}
	for seq := int64(1); seq <= 3; seq++ {
		if err := sink.Emit(protocol.Event{Seq: seq, Type: protocol.EventLog, Name: "n"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := sink.Close(); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(body)), "\n")
	if len(lines) != 3 {
		t.Fatalf("lines = %d, want 3", len(lines))
	}
	for i, line := range lines {
		if !strings.Contains(line, fmt.Sprintf(`"seq":%d`, i+1)) {
			t.Fatalf("line %d out of order: %s", i, line)
		}
	}
}

// ---- live smoke (real Claude Code CLI) -------------------------------------

// TestLiveSmokeRealClaudeCode is the U4 live smoke: one two-phase chain —
// an agent phase writing a file, a code phase checking it — against the
// real CLI. Guarded so CI never pays for it: JIG_LIVE_SMOKE=1 to run.
func TestLiveSmokeRealClaudeCode(t *testing.T) {
	if os.Getenv("JIG_LIVE_SMOKE") != "1" {
		t.Skip("set JIG_LIVE_SMOKE=1 to run the real-CLI smoke")
	}
	adapter, err := claudecode.New()
	if err != nil {
		t.Skipf("claude CLI unavailable: %v", err)
	}
	capability, err := adapter.Probe(context.Background())
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	t.Logf("claude %s (can_resume=%v)", capability.Version, capability.CanResume)

	repo := initRepo(t)
	tracePath := filepath.Join(t.TempDir(), "trace.jsonl")
	sink, err := engine.NewJSONLSink(tracePath)
	if err != nil {
		t.Fatal(err)
	}
	defer sink.Close()

	seedClaudeAuth := func(home, _ string, _ protocol.RoleSpec) error {
		real, err := os.UserHomeDir()
		if err != nil {
			return err
		}
		if mkErr := os.MkdirAll(filepath.Join(home, ".claude"), 0o700); mkErr != nil {
			return mkErr
		}
		// The minimum auth material the CLI needs (KTD11): the credentials
		// file when it exists, otherwise the macOS keychain secret extracted
		// into the ephemeral HOME's credentials file — the CLI's keychain
		// lookup does not survive a HOME change. Onboarding state rides
		// along so print mode skips first-run prompts.
		credentials, readErr := os.ReadFile(filepath.Join(real, ".claude", ".credentials.json"))
		if readErr != nil {
			extracted, keychainErr := exec.Command("security",
				"find-generic-password", "-s", "Claude Code-credentials", "-w").Output()
			if keychainErr != nil {
				return fmt.Errorf("no credentials file and no keychain item: %v", keychainErr)
			}
			credentials = extracted
		}
		if writeErr := os.WriteFile(
			filepath.Join(home, ".claude", ".credentials.json"), credentials, 0o600); writeErr != nil {
			return writeErr
		}
		if onboarding, onboardErr := os.ReadFile(filepath.Join(real, ".claude.json")); onboardErr == nil {
			if writeErr := os.WriteFile(
				filepath.Join(home, ".claude.json"), onboarding, 0o600); writeErr != nil {
				return writeErr
			}
		}
		return nil
	}

	runner, err := engine.New(engine.Config{
		Runtime:         adapter,
		Capability:      capability,
		Sink:            sink,
		ScratchRoot:     t.TempDir(),
		SeedHome:        seedClaudeAuth,
		BaseEnv:         os.Environ(),
		PhaseTimeout:    5 * time.Minute,
		NoOutputTimeout: 3 * time.Minute,
		AttemptCeiling:  8 * time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}

	snapshot := `
name: live-smoke
roster:
  writer:
    model: haiku
    system_prompt: You are a careful engineer working in a git repository.
    user_prompt: "Create a file named hello.txt in the repository root containing exactly the text: hello jig"
    env: [PATH, ANTHROPIC_API_KEY, TERM]
    writes: ["hello.txt"]
phases:
  - name: write
    kind: agent
    owner: writer
    gates:
      - {name: artifacts_exist}
  - {name: check, kind: code, command: "grep -q 'hello jig' hello.txt"}
acceptance: [all_phases_passed]
`
	outcome := runner.Execute(context.Background(), testAttempt(snapshot, nil, repo))
	t.Logf("outcome: %s %s (trace: %s)", outcome.State, outcome.Error, tracePath)
	if outcome.State != protocol.AttemptAcceptedUnpublished {
		t.Fatalf("live smoke: state = %q (%s)", outcome.State, outcome.Error)
	}
}

// ---- scenario: gate containment (KTD10, KTD11) -----------------------------

// A gate command runs `go test`/`npm test` inside the worktree the agent just
// wrote, so it executes agent-authored code. It gets the role's environment
// and the ephemeral HOME, never jig's own environment and the operator's real
// dotfiles.
func TestGateCommandsRunUnderTheRoleAllowlistAndTheEphemeralHome(t *testing.T) {
	repo := initRepo(t)
	scratchRoot := t.TempDir()
	dump := filepath.Join(t.TempDir(), "gate-env.txt")
	fake := enginetest.New(enginetest.Step{
		Text: envelope(map[string]any{"status": "success", "summary": "done"}),
	})
	runner := newTestRunner(t, fake, &recordingSink{}, func(config *engine.Config) {
		config.ScratchRoot = scratchRoot
	})

	snapshot := "name: t\n" + oneWriterRoster + `
phases:
  - name: build
    kind: agent
    owner: writer
    gates:
      - {name: tests_pass, command: "env > '` + dump + `'"}
acceptance: [all_phases_passed]
`
	outcome := runner.Execute(context.Background(), testAttempt(snapshot, nil, repo))
	if outcome.State != protocol.AttemptAcceptedUnpublished {
		t.Fatalf("state = %q (%s), want accepted_unpublished", outcome.State, outcome.Error)
	}

	body, err := os.ReadFile(dump)
	if err != nil {
		t.Fatal(err)
	}
	values := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(body)), "\n") {
		if name, value, found := strings.Cut(line, "="); found {
			values[name] = value
		}
	}
	// `sh` itself contributes these three; everything else must have been
	// composed by the engine.
	allowed := map[string]bool{
		"PATH": true, "FOO": true, "HOME": true,
		"XDG_CONFIG_HOME": true, "XDG_CACHE_HOME": true,
		"XDG_DATA_HOME": true, "XDG_STATE_HOME": true,
		"PWD": true, "SHLVL": true, "_": true,
	}
	for name := range values {
		if !allowed[name] {
			t.Errorf("gate subprocess env carries %q, which is not the allowlist plus the HOME/XDG family", name)
		}
	}
	if values["FOO"] != "bar" {
		t.Errorf("allowlisted FOO = %q, want bar", values["FOO"])
	}
	if _, leaked := values["SECRET"]; leaked {
		t.Error("SECRET leaked into the gate subprocess env")
	}
	if !strings.HasPrefix(values["HOME"], scratchRoot) {
		t.Fatalf("gate subprocess HOME = %q, want the jig-managed ephemeral home under %q",
			values["HOME"], scratchRoot)
	}
	if realHome, _ := os.UserHomeDir(); values["HOME"] == realHome {
		t.Fatal("gate subprocess HOME is the operator's real home")
	}
	if !strings.HasPrefix(values["XDG_CONFIG_HOME"], values["HOME"]) {
		t.Errorf("XDG_CONFIG_HOME = %q, want it inside the ephemeral HOME", values["XDG_CONFIG_HOME"])
	}
}

// ---- scenario: boundary enforcement on the failure path (R10) --------------

// A phase that fails on parse exhaustion still had a live agent in the
// worktree: its out-of-allowlist writes must not survive into the retained
// worktree or the next phase.
func TestAPhaseFailingOnItsParseBudgetLeavesNoOutOfAllowlistWrites(t *testing.T) {
	repo := initRepo(t)
	steps := []enginetest.Step{{
		Files: map[string]string{"src/ok.txt": "allowed", "stray.txt": "sneaky"},
		Text:  "not json at all",
	}}
	for i := 0; i < protocol.ParseBudgetPerEmission; i++ {
		steps = append(steps, enginetest.Step{Text: "still not json " + fmt.Sprint(i)})
	}
	fake := enginetest.New(steps...)
	sink := &recordingSink{}
	runner := newTestRunner(t, fake, sink, nil)

	snapshot := "name: t\n" + repairRoster + `
phases:
  - {name: build, kind: agent, owner: builder}
`
	outcome := runner.Execute(context.Background(), testAttempt(snapshot, nil, repo))
	if outcome.State != protocol.AttemptFailed {
		t.Fatalf("state = %q, want failed", outcome.State)
	}
	if _, err := os.Stat(filepath.Join(repo, "stray.txt")); !os.IsNotExist(err) {
		t.Fatal("an out-of-allowlist write survived a phase that failed on its parse budget")
	}
	if !strings.Contains(outcome.Error, "never produced a valid envelope") {
		t.Fatalf("error %q lost the original failure", outcome.Error)
	}
	if !strings.Contains(outcome.Error, "outside its write allowlist") {
		t.Fatalf("error %q does not report the breach found on the failure path", outcome.Error)
	}
	if !sink.has(protocol.EventError, "write_boundary_breach") {
		t.Fatal("no write_boundary_breach trace event on the failure path")
	}
	if fake.Remaining() != 0 {
		t.Fatalf("unconsumed scripted steps: %d", fake.Remaining())
	}
}

// ---- scenario: the send ladder is really bounded ---------------------------

func TestASendLadderDesignedToBurnItselfStopsAtTheEnforcedBound(t *testing.T) {
	repo := initRepo(t)
	const cap = 3
	steps := make([]enginetest.Step, 0, cap)
	for i := 0; i < cap; i++ {
		steps = append(steps, enginetest.Step{Text: "never parseable " + fmt.Sprint(i)})
	}
	fake := enginetest.New(steps...)
	sink := &recordingSink{}
	runner := newTestRunner(t, fake, sink, func(config *engine.Config) {
		config.MaxAttemptSends = cap
	})

	// Every budget is raised so the parse × gate × crash × repair ladder would
	// otherwise run for hundreds of sends.
	snapshot := "name: t\n" + repairRoster + `
phases:
  - name: build
    kind: agent
    owner: builder
    gates:
      - {name: artifacts_exist, budget: 9}
    on_fail: {when: "status == fail", run: fix, then: rerun-self, budget: 9}
  - {name: fix, kind: agent, owner: builder}
`
	outcome := runner.Execute(context.Background(), testAttempt(snapshot, nil, repo))
	if outcome.State != protocol.AttemptFailed {
		t.Fatalf("state = %q, want failed", outcome.State)
	}
	if !strings.Contains(outcome.Error, "send budget") {
		t.Fatalf("error %q is not the send-budget terminal cause", outcome.Error)
	}
	if !sink.has(protocol.EventError, "attempt_send_budget_exhausted") {
		t.Fatal("the send bound did not get its own terminal event")
	}
	if fake.Remaining() != 0 {
		t.Fatalf("the ladder stopped early or late: %d unconsumed steps", fake.Remaining())
	}
}

// ---- scenario: the result payload always parses ----------------------------

// Cutting serialized JSON at MaxResultBytes always yields JSON that does not
// parse, which destroys the acceptance evidence and the changed_paths U7
// consumes. Oversized inputs must degrade to a smaller VALID document.
func TestOversizedEnvelopesStillYieldParseableResultJSONWithChangedPaths(t *testing.T) {
	repo := initRepo(t)
	const phases = 12
	evidence := strings.Repeat("e", 12<<10)

	var steps []enginetest.Step
	chain := ""
	for i := 0; i < phases; i++ {
		fields := map[string]any{
			"status": "success", "summary": "phase " + fmt.Sprint(i), "evidence": evidence,
		}
		step := enginetest.Step{Text: envelope(fields)}
		if i == 0 {
			step.Files = map[string]string{"out.txt": "content"}
		}
		steps = append(steps, step)
		chain += fmt.Sprintf("  - {name: p%d, kind: agent, owner: writer}\n", i)
	}
	fake := enginetest.New(steps...)
	runner := newTestRunner(t, fake, &recordingSink{}, nil)

	snapshot := "name: t\n" + oneWriterRoster + "\nphases:\n" + chain + "acceptance: [all_phases_passed]\n"
	outcome := runner.Execute(context.Background(), testAttempt(snapshot, nil, repo))
	if outcome.State != protocol.AttemptAcceptedUnpublished {
		t.Fatalf("state = %q (%s), want accepted_unpublished", outcome.State, outcome.Error)
	}
	if len(outcome.Result) > protocol.MaxResultBytes {
		t.Fatalf("result payload is %d bytes, over the %d cap", len(outcome.Result), protocol.MaxResultBytes)
	}
	var summary map[string]any
	if err := json.Unmarshal([]byte(outcome.Result), &summary); err != nil {
		t.Fatalf("result payload does not parse: %v\n%.400q", err, outcome.Result)
	}
	if truncated, _ := summary["truncated"].(bool); !truncated {
		t.Fatalf("an oversized summary was not marked truncated: %v", summary["truncated"])
	}
	paths, ok := summary["changed_paths"].([]any)
	if !ok || len(paths) == 0 || paths[0] != "out.txt" {
		t.Fatalf("changed_paths did not survive truncation: %v", summary["changed_paths"])
	}
	if _, present := summary["acceptance"]; !present {
		t.Fatal("the acceptance verdict did not survive truncation")
	}
}

// ---- scenario: the runtime's own error flag (R7) ---------------------------

// A CLI can exit 0 and still have failed — auth rejected, rate limited. Its
// error prose is not an envelope, and re-prompting it 3x per emission, then
// gate-correcting, then counting deaths, then rerunning the repair edge is a
// retry storm with the wrong diagnosis.
func TestARuntimeErrorEndsThePhaseWithoutWalkingTheParseLadder(t *testing.T) {
	repo := initRepo(t)
	fake := enginetest.New(enginetest.Step{
		IsError: true,
		Text:    "API Error: 401 {\"error\":{\"message\":\"invalid x-api-key\"}}",
	})
	sink := &recordingSink{}
	runner := newTestRunner(t, fake, sink, nil)

	snapshot := "name: t\n" + repairRoster + `
phases:
  - name: build
    kind: agent
    owner: builder
    on_fail: {when: "status == fail", run: fix, then: rerun-self, budget: 2}
  - {name: fix, kind: agent, owner: builder}
`
	outcome := runner.Execute(context.Background(), testAttempt(snapshot, nil, repo))
	if outcome.State != protocol.AttemptFailed {
		t.Fatalf("state = %q, want failed", outcome.State)
	}
	if !strings.Contains(outcome.Error, "runtime reported a terminal error") {
		t.Fatalf("error %q does not diagnose the runtime error", outcome.Error)
	}
	if !strings.Contains(outcome.Error, "invalid x-api-key") {
		t.Fatalf("error %q does not surface the CLI's own message", outcome.Error)
	}
	// Exactly one send: no parse ladder, no gate correction, no death retry,
	// no repair-edge rerun.
	if calls := fake.Calls(); len(calls) != 1 {
		t.Fatalf("sends = %d, want 1 — the runtime error was retried", len(calls))
	}
	if got := sink.count(protocol.EventLog, "invalid_envelope"); got != 0 {
		t.Fatalf("invalid_envelope events = %d, want 0 — error prose was fed to the parser", got)
	}
	if sink.count(protocol.EventPhaseDeath, "") != 0 {
		t.Fatal("a runtime error was counted as a subprocess death")
	}
}
