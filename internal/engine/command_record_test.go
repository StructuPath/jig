// command_record_test.go — jig-side commands record their process groups
// like agent sends do (U4): a code phase's command and a tests_pass gate's
// are recorded live while they run and cleared once they have exited, so a
// worker that dies hard mid-command leaves a manifest its restart can stop
// the group from. A group that cannot be recorded does not run on: it is
// killed, and the attempt fails as infrastructure — never as a failing test
// that a repair edge or a gate correction would spend model budget on.
package engine_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/StructuPath/jig/internal/engine"
	"github.com/StructuPath/jig/internal/engine/enginetest"
	"github.com/StructuPath/jig/internal/protocol"
	"github.com/StructuPath/jig/internal/worker"
)

// groupRecorder is a RecordProcess fake: the live set per attempt, and an
// optional failure for every active record.
type groupRecorder struct {
	mutex   sync.Mutex
	live    map[int64]bool
	calls   []string
	failure error
}

func (r *groupRecorder) record(attemptID string, groupID int64, active bool) error {
	r.mutex.Lock()
	defer r.mutex.Unlock()
	if attemptID != "attempt-1" {
		return errors.New("recorded under the wrong attempt: " + attemptID)
	}
	r.calls = append(r.calls, strconv.FormatInt(groupID, 10)+"="+strconv.FormatBool(active))
	if active {
		// A failed write may still have half-landed: the group is recorded
		// either way, so only a clear afterwards leaves the set honest.
		r.live[groupID] = true
		return r.failure
	}
	delete(r.live, groupID)
	return nil
}

func (r *groupRecorder) isLive(groupID int64) bool {
	r.mutex.Lock()
	defer r.mutex.Unlock()
	return r.live[groupID]
}

func (r *groupRecorder) liveCount() int {
	r.mutex.Lock()
	defer r.mutex.Unlock()
	return len(r.live)
}

// heldCommand is a shell command that writes its own process id — its
// group's id, since it leads its group — and then waits for release.
func heldCommand(t *testing.T) (command, pidFile, release string) {
	t.Helper()
	dir := t.TempDir()
	pidFile = filepath.Join(dir, "pid")
	release = filepath.Join(dir, "release")
	command = "echo $$ > '" + pidFile + "'; while [ ! -f '" + release + "' ]; do sleep 0.05; done"
	// A failing assertion must not leave the loop running: release it, and
	// stop THIS command's own group — never 0, 1, or the test's own.
	t.Cleanup(func() {
		_ = os.WriteFile(release, nil, 0o644)
		body, err := os.ReadFile(pidFile)
		if err != nil {
			return
		}
		if group, err := strconv.Atoi(strings.TrimSpace(string(body))); err == nil &&
			group > 1 && group != syscall.Getpgrp() {
			_ = syscall.Kill(-group, syscall.SIGKILL)
		}
	})
	return command, pidFile, release
}

func awaitPID(t *testing.T, pidFile string) int64 {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if body, err := os.ReadFile(pidFile); err == nil && strings.HasSuffix(string(body), "\n") {
			pid, err := strconv.ParseInt(strings.TrimSpace(string(body)), 10, 64)
			if err != nil {
				t.Fatal(err)
			}
			return pid
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("the command never started")
	return 0
}

// Both jig-side command sites record their group live while it runs and
// clear it once it has exited.
func TestCodeAndGateCommandsAreRecordedLiveWhileTheyRun(t *testing.T) {
	for name, phases := range map[string]func(command string) string{
		"code phase": func(command string) string {
			return "  - name: verify\n    kind: code\n    command: \"" + command + "\"\n"
		},
		"tests_pass gate": func(command string) string {
			return "  - name: build\n    kind: agent\n    owner: writer\n    gates:\n" +
				"      - {name: tests_pass, command: \"" + command + "\"}\n"
		},
	} {
		t.Run(name, func(t *testing.T) {
			repo := initRepo(t)
			recorder := &groupRecorder{live: map[int64]bool{}}
			fake := enginetest.New(enginetest.Step{Text: envelope(map[string]any{"status": "success", "summary": "done"})})
			runner := newTestRunner(t, fake, &recordingSink{}, func(config *engine.Config) {
				config.RecordProcess = recorder.record
			})
			command, pidFile, release := heldCommand(t)
			snapshot := "name: t\n" + oneWriterRoster + "phases:\n" + phases(command) + "acceptance: [all_phases_passed]\n"

			outcomes := make(chan worker.Outcome, 1)
			go func() { outcomes <- runner.Execute(context.Background(), testAttempt(snapshot, nil, repo)) }()
			pid := awaitPID(t, pidFile)
			if !recorder.isLive(pid) {
				t.Fatalf("the running command's group %d is not recorded live (calls %v)", pid, recorder.calls)
			}
			if err := os.WriteFile(release, nil, 0o644); err != nil {
				t.Fatal(err)
			}
			outcome := <-outcomes
			if outcome.State != protocol.AttemptAcceptedUnpublished {
				t.Fatalf("state = %q (%s), want accepted_unpublished", outcome.State, outcome.Error)
			}
			if recorder.liveCount() != 0 {
				t.Fatalf("groups still recorded live after the command exited: calls %v", recorder.calls)
			}
		})
	}
}

// A group that cannot be recorded is killed before it does anything worth
// judging — the delayed marker never appears — and is cleared again. The
// attempt fails as infrastructure: the code phase's repair edge is not
// followed, and the gate sends the agent no correction.
func TestACommandWhoseGroupCannotBeRecordedIsKilledAndFailsAsInfrastructure(t *testing.T) {
	for name, phases := range map[string]func(command string) string{
		"code phase": func(command string) string {
			return "  - {name: build, kind: agent, owner: writer}\n" +
				"  - name: verify\n    kind: code\n    command: \"" + command + "\"\n" +
				"    on_fail: {run: build, then: rerun-self, budget: 2}\n"
		},
		"tests_pass gate": func(command string) string {
			return "  - name: build\n    kind: agent\n    owner: writer\n    gates:\n" +
				"      - {name: tests_pass, command: \"" + command + "\"}\n"
		},
	} {
		t.Run(name, func(t *testing.T) {
			repo := initRepo(t)
			recorder := &groupRecorder{live: map[int64]bool{}, failure: errors.New("manifest write failed")}
			done := envelope(map[string]any{"status": "success", "summary": "done"})
			fake := enginetest.New(enginetest.Step{Text: done}, enginetest.Step{Text: done}, enginetest.Step{Text: done})
			sink := &recordingSink{}
			runner := newTestRunner(t, fake, sink, func(config *engine.Config) {
				config.RecordProcess = recorder.record
			})
			marker := filepath.Join(t.TempDir(), "ran")
			command := "sleep 0.5; touch '" + marker + "'"
			snapshot := "name: t\n" + oneWriterRoster + "phases:\n" + phases(command) + "acceptance: [all_phases_passed]\n"

			outcome := runner.Execute(context.Background(), testAttempt(snapshot, nil, repo))
			if outcome.State != protocol.AttemptFailed || !strings.Contains(outcome.Error, "manifest write failed") {
				t.Fatalf("state = %q (%s), want failed naming the record failure", outcome.State, outcome.Error)
			}
			if calls := len(fake.Calls()); calls != 1 {
				t.Fatalf("the runtime was called %d times, want 1: no repair and no gate correction", calls)
			}
			if sink.has(protocol.EventLog, "repair_edge") {
				t.Fatal("a record failure was routed through the repair edge")
			}
			if !sink.has(protocol.EventError, "engine_error") && name == "code phase" {
				t.Fatal("the code phase's record failure was not traced as an engine error")
			}
			time.Sleep(time.Second)
			if _, err := os.Stat(marker); !os.IsNotExist(err) {
				t.Fatalf("the unrecorded command kept running and wrote its marker (stat: %v)", err)
			}
			if recorder.liveCount() != 0 {
				t.Fatalf("the failed record was never cleared: calls %v", recorder.calls)
			}
		})
	}
}

// An agent phase whose own verdict fires its on_fail edge must still not be
// repaired when its tests_pass gate could not run: the record failure is
// jig's, not the agent's, so it spends neither a gate correction nor the
// repair budget — serially or inside a parallel group. Each case first runs
// with recording working, which proves the edge is live: the same envelope
// does dispatch the repair there.
func TestAGateRecordFailureNeverFollowsTheAgentPhasesRepairEdge(t *testing.T) {
	rejecting := enginetest.Step{Text: envelope(map[string]any{
		"status": "success", "summary": "not yet", "approved": false, "blocking": []any{"not yet"},
	})}
	done := enginetest.Step{Text: envelope(map[string]any{"status": "success", "summary": "done"})}
	serial := "name: t\n" + oneWriterRoster + `phases:
  - {name: plan, kind: agent, owner: writer}
  - name: build
    kind: agent
    owner: writer
    gates: [{name: tests_pass, command: "true"}]
    on_fail: {when: "approved == false", run: plan, then: rerun-self, budget: 2, exhausted: fail-job}
acceptance: [all_phases_passed]
`
	grouped := strings.Replace(panelSnapshot(nil, panelGroup),
		"    owner: security\n    gates: [{name: verdict_consistent}]\n",
		"    owner: security\n    gates: [{name: verdict_consistent}, {name: tests_pass, command: \"true\"}]\n", 1)
	if grouped == panelSnapshot(nil, panelGroup) {
		t.Fatal("the panel fixture changed; the tests_pass gate was not added to a member")
	}

	run := func(t *testing.T, snapshot string, failure error, route func(*enginetest.Runtime)) (*repairFixture, worker.Outcome) {
		recorder := &groupRecorder{live: map[int64]bool{}, failure: failure}
		f := newRepairFixture(t, snapshot, func(config *engine.Config) {
			config.RecordProcess = recorder.record
		}, done, rejecting, done, rejecting, done, rejecting)
		if route != nil {
			route(f.fake)
		}
		return f, executeWithin(t, f, 30*time.Second)
	}
	routePanel := func(fake *enginetest.Runtime) {
		fake.Route(buildRole, writes(map[string]string{"src/app.txt": "v1"}, "built"),
			writes(map[string]string{"src/app.txt": "v2"}, "rebuilt"))
		fake.Route(correctnessRole, approve("correct", nil), approve("correct", nil))
		fake.Route(securityRole, reject("auth bypass"), approve("secure now", nil))
		fake.Route(maintainabilityRole, approve("maintainable", nil), approve("maintainable", nil))
		fake.Route(closerRole, success("wrapped up", nil))
	}
	for name, tc := range map[string]struct {
		snapshot string
		route    func(*enginetest.Runtime)
		phase    string
		calls    int // runtime calls up to and including the failing phase
	}{
		"serial":          {serial, nil, "build", 2},
		"parallel member": {grouped, routePanel, "review-security", 4},
	} {
		t.Run(name, func(t *testing.T) {
			control, _ := run(t, tc.snapshot, nil, tc.route)
			if !control.sink.has(protocol.EventLog, "repair_edge") {
				t.Fatal("control: with recording working the edge never fired; the fixture proves nothing")
			}

			f, outcome := run(t, tc.snapshot, errors.New("manifest write failed"), tc.route)
			if outcome.State != protocol.AttemptFailed || !strings.Contains(outcome.Error, "manifest write failed") {
				t.Fatalf("state = %q (%s), want failed naming the record failure", outcome.State, outcome.Error)
			}
			if f.sink.has(protocol.EventLog, "repair_edge") {
				t.Fatal("a gate record failure dispatched the agent phase's repair edge")
			}
			if calls := len(f.fake.Calls()); calls != tc.calls {
				t.Fatalf("runtime calls = %d, want %d: no correction send and no repair dispatch", calls, tc.calls)
			}
			ends, failedEnds := 0, 0
			for _, event := range f.sink.all() {
				if event.Phase != tc.phase {
					continue
				}
				if event.Type == protocol.EventAgentEnd {
					ends++
				}
				if event.Type == protocol.EventPhaseEnd && strings.Contains(string(event.Payload), protocol.EnvelopeFail) {
					failedEnds++
				}
			}
			if ends != 1 || failedEnds != 1 {
				t.Fatalf("phase %s closed with %d agent_end and %d failed phase_end, want exactly one each",
					tc.phase, ends, failedEnds)
			}
		})
	}
}

// That exit is still a live agent's exit: the write boundary is enforced on
// it, so an out-of-allowlist write is rolled back and ends the attempt as a
// breach rather than surviving into the retained worktree.
func TestAGateRecordFailureStillEnforcesTheWriteBoundary(t *testing.T) {
	snapshot := "name: t\n" + oneWriterRoster + `phases:
  - name: build
    kind: agent
    owner: writer
    gates: [{name: tests_pass, command: "true"}]
acceptance: [all_phases_passed]
`
	recorder := &groupRecorder{live: map[int64]bool{}, failure: errors.New("manifest write failed")}
	breaching := enginetest.Step{Files: map[string]string{"evil.txt": "planted"},
		Text: envelope(map[string]any{"status": "success", "summary": "done"})}
	f := newRepairFixture(t, snapshot, func(config *engine.Config) {
		config.RecordProcess = recorder.record
	}, breaching, breaching)
	outcome := executeWithin(t, f, 30*time.Second)
	if outcome.State != protocol.AttemptFailed || !strings.Contains(outcome.Error, "manifest write failed") ||
		!f.sink.has(protocol.EventError, "write_boundary_breach") {
		t.Fatalf("state = %q (%s), want a failure naming the record failure and the breach", outcome.State, outcome.Error)
	}
	if _, err := os.Stat(filepath.Join(f.repo, "evil.txt")); !os.IsNotExist(err) {
		t.Fatalf("the out-of-allowlist write survived (stat: %v)", err)
	}
	if calls := len(f.fake.Calls()); calls != 1 {
		t.Fatalf("runtime calls = %d, want 1", calls)
	}
}
