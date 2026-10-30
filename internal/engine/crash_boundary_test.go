// crash_boundary_test.go — a dead phase is retried only after its writes
// pass the write boundary (R10/R11). Rollback alone cannot be trusted with
// this: it deliberately leaves a path that was dirty before the phase in its
// post-agent state, so an edit to a builder's uncommitted file — or a write
// the retry quietly reverts — would otherwise ride the retry unseen.
package engine_test

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/StructuPath/jig/internal/engine/enginetest"
	"github.com/StructuPath/jig/internal/protocol"
)

// builderReaderSnapshot is a builder that may write src/ followed by a
// read-only reviewer.
const builderReaderSnapshot = `name: crash
roster:
  builder: {model: test, system_prompt: Build., user_prompt: Build., writes: ["src/"]}
  reader: {model: test, system_prompt: Read., user_prompt: Read., writes: []}
phases:
  - {name: build, kind: agent, owner: builder}
  - {name: review, kind: agent, owner: reader}
acceptance: [all_phases_passed]
`

func readFile(t *testing.T, path string) string {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(content)
}

// A read-only reviewer that edits the builder's uncommitted work and dies is
// a breach, not a death to retry: the retry would start from the edit, and a
// retry that puts the content back would erase every trace of it.
func TestAReadOnlyRoleThatEditsBuilderWIPAndCrashesIsNeverRetried(t *testing.T) {
	repo := initRepo(t)
	fake := enginetest.New(
		writes(map[string]string{"src/app.txt": "app"}, "built"),
		enginetest.Step{Crash: true, Files: map[string]string{"src/app.txt": "reviewer edit"}},
		// The retry would restore the builder's content and pass.
		writes(map[string]string{"src/app.txt": "app"}, "looks right"),
	)
	sink := &recordingSink{}
	runner := newTestRunner(t, fake, sink, nil)

	outcome := runner.Execute(context.Background(), testAttempt(builderReaderSnapshot, nil, repo))
	if outcome.State != protocol.AttemptFailed || !strings.Contains(outcome.Error, "src/app.txt") {
		t.Fatalf("state = %s (%s), want failed on a breach naming src/app.txt", outcome.State, outcome.Error)
	}
	if calls := len(fake.Calls()); calls != 2 {
		t.Fatalf("calls = %d, want build plus one review: the dead reviewer must not be retried", calls)
	}
	if !sink.has(protocol.EventError, "write_boundary_breach") {
		t.Fatal("no write_boundary_breach event")
	}
	// The death is still in the trace, and the entry closes exactly once.
	if got := sink.count(protocol.EventPhaseDeath, ""); got != 1 {
		t.Fatalf("phase_death events = %d, want 1", got)
	}
	if got := sink.count(protocol.EventAgentEnd, "reader"); got != 1 {
		t.Fatalf("reviewer agent_end events = %d, want 1", got)
	}
	if got := phaseStatuses(t, outcome.Result)["review"]; !reflect.DeepEqual(got, []string{protocol.EnvelopeFail}) {
		t.Fatalf("review results = %v, want one failed breach result", got)
	}
	// The edit to pre-existing work is reported, never discarded.
	if got := readFile(t, filepath.Join(repo, "src", "app.txt")); got != "reviewer edit" {
		t.Fatalf("src/app.txt = %q, want the reviewer's edit left for inspection", got)
	}
}

// The builder's own death over its own uncommitted work stays what it was:
// rolled back as far as rollback goes, then retried.
func TestAnAuthorizedRoleThatCrashesOverItsWIPIsStillRetried(t *testing.T) {
	repo := initRepo(t)
	if err := os.MkdirAll(filepath.Join(repo, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "src", "app.txt"), []byte("wip"), 0o644); err != nil {
		t.Fatal(err)
	}
	fake := enginetest.New(
		enginetest.Step{Crash: true, Files: map[string]string{"src/app.txt": "half done", "src/new.txt": "half"}},
		writes(map[string]string{"src/app.txt": "done"}, "built"),
	)
	sink := &recordingSink{}
	runner := newTestRunner(t, fake, sink, nil)

	snapshot := "name: t\n" + repairRoster + `
phases:
  - {name: build, kind: agent, owner: builder}
acceptance: [all_phases_passed]
`
	outcome := runner.Execute(context.Background(), testAttempt(snapshot, nil, repo))
	if outcome.State != protocol.AttemptAcceptedUnpublished {
		t.Fatalf("state = %s (%s), want accepted after the retry", outcome.State, outcome.Error)
	}
	if calls := len(fake.Calls()); calls != 2 {
		t.Fatalf("calls = %d, want the death and its retry", calls)
	}
	if sink.has(protocol.EventError, "write_boundary_breach") {
		t.Fatal("an authorized write raised a breach")
	}
	if _, err := os.Stat(filepath.Join(repo, "src", "new.txt")); !os.IsNotExist(err) {
		t.Fatalf("the dead entry's new file survived the rollback: %v", err)
	}
}

// A declared build output a dead read-only phase wrote is neither a breach
// nor carried into the retry: rollback removes it and the phase re-enters.
func TestAFreshBuildOutputFromADeadReadOnlyPhaseIsRemovedAndRetried(t *testing.T) {
	repo := initRepo(t)
	ignoreBin(t, repo)
	fake := enginetest.New(
		enginetest.Step{Crash: true, Files: map[string]string{"bin/x": "half a build"}},
		success("read", nil),
	)
	sink := &recordingSink{}
	runner := newTestRunner(t, fake, sink, nil)

	snapshot := `name: readonly
roster:
  reader: {model: test, system_prompt: Read., user_prompt: Read., writes: []}
phases:
  - {name: read, kind: agent, owner: reader}
acceptance: [all_phases_passed]
` + buildOutputsLine
	outcome := runner.Execute(context.Background(), testAttempt(snapshot, nil, repo))
	if outcome.State != protocol.AttemptAcceptedUnpublished {
		t.Fatalf("state = %s (%s), want accepted after the retry", outcome.State, outcome.Error)
	}
	if calls := len(fake.Calls()); calls != 2 {
		t.Fatalf("calls = %d, want the death and its retry", calls)
	}
	if sink.has(protocol.EventError, "write_boundary_breach") {
		t.Fatal("a declared build output raised a breach")
	}
	if _, err := os.Stat(filepath.Join(repo, "bin", "x")); !os.IsNotExist(err) {
		t.Fatalf("the dead entry's build output survived the rollback: %v", err)
	}
}

// hideEdit and withDo script a retry that would erase the dead entry's trace.
func hideEdit(step enginetest.Step, files map[string]string) enginetest.Step {
	step.Files = files
	return step
}

func withDo(step enginetest.Step, do func(enginetest.Call)) enginetest.Step {
	step.Do = do
	return step
}

// A dead member's edit to the builder's uncommitted work must not become the
// next wave's baseline: a retry that reverts it leaves the group's final
// enforcement an unchanged tree.
func TestADeadMembersEditToBuilderWIPEndsTheGroupBeforeARetry(t *testing.T) {
	f := newRepairFixture(t, panelSnapshot(nil, panelGroup), nil)
	app := filepath.Join(f.repo, "src", "app.txt")
	f.fake.Route(buildRole, writes(map[string]string{"src/app.txt": "app"}, "built the app"))
	f.fake.Route(correctnessRole,
		enginetest.Step{Crash: true, Do: func(enginetest.Call) {
			if err := os.WriteFile(app, []byte("reviewer edit"), 0o644); err != nil {
				t.Error(err)
			}
		}},
		hideEdit(approve("would hide the edit", nil), map[string]string{"src/app.txt": "app"}))
	f.fake.Route(securityRole, approve("secure", nil))
	f.fake.Route(maintainabilityRole, approve("maintainable", nil))
	f.fake.Route(closerRole, success("wrapped up", nil))

	outcome := executeWithin(t, f, 20*time.Second)
	if outcome.State != protocol.AttemptFailed || !strings.Contains(outcome.Error, "src/app.txt") {
		t.Fatalf("outcome = %s (%s), want failed on a breach naming src/app.txt", outcome.State, outcome.Error)
	}
	if got := len(callsFor(f.fake, correctnessRole)); got != 1 {
		t.Fatalf("correctness invoked %d times, want one: no wave may run over a breach", got)
	}
	if got := readFile(t, app); got != "reviewer edit" {
		t.Fatalf("src/app.txt = %q, want the reviewer's edit left for inspection", got)
	}
}

// The same for repository metadata: a hook a dead member planted is a breach
// before the next wave, not something a retry can delete unseen.
func TestADeadMembersPlantedHookEndsTheGroupBeforeARetry(t *testing.T) {
	f := newRepairFixture(t, panelSnapshot(nil, panelGroup), nil)
	hook := filepath.Join(f.repo, ".git", "hooks", "post-checkout")
	f.fake.Route(buildRole, writes(map[string]string{"src/app.txt": "app"}, "built the app"))
	f.fake.Route(correctnessRole,
		enginetest.Step{Crash: true, Do: func(enginetest.Call) {
			if err := os.WriteFile(hook, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
				t.Error(err)
			}
		}},
		withDo(approve("would hide the hook", nil), func(enginetest.Call) { _ = os.Remove(hook) }))
	f.fake.Route(securityRole, approve("secure", nil))
	f.fake.Route(maintainabilityRole, approve("maintainable", nil))
	f.fake.Route(closerRole, success("wrapped up", nil))

	outcome := executeWithin(t, f, 20*time.Second)
	if outcome.State != protocol.AttemptFailed || !strings.Contains(outcome.Error, "post-checkout") {
		t.Fatalf("outcome = %s (%s), want failed on a breach naming the hook", outcome.State, outcome.Error)
	}
	if got := len(callsFor(f.fake, correctnessRole)); got != 1 {
		t.Fatalf("correctness invoked %d times, want one: no wave may run over a breach", got)
	}
	if _, err := os.Stat(hook); !os.IsNotExist(err) {
		t.Fatalf("the planted hook survived enforcement: %v", err)
	}
}
