// definitions_test.go — the U9 stock-library suite: every shipped definition
// passes save-time validation AND executes end to end against a fixture
// repository on the scripted runtime. Validation alone would not catch a
// definition whose phases never compose — a repair edge that dispatches a
// phase that already ran, a guard keyed on a field nobody sets — so each one
// is run, and the trace is asserted against what the definition claims to do.
package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/StructuPath/jig/internal/protocol"
)

// stockDefinition resolves one shipped definition path.
func stockDefinition(name string) string {
	return filepath.Join("..", "..", "examples", "definitions", name)
}

// ---- every stock definition validates -------------------------------------

func TestEveryStockDefinitionPassesSaveTimeValidation(t *testing.T) {
	entries, err := os.ReadDir(filepath.Join("..", "..", "examples", "definitions"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) < 5 {
		t.Fatalf("the stock library has %d definitions; the plan ships five", len(entries))
	}
	for _, entry := range entries {
		source, err := os.ReadFile(stockDefinition(entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		spec, err := protocol.ParseDefinition(source)
		if err != nil {
			t.Errorf("%s does not validate: %v", entry.Name(), err)
			continue
		}
		// The file name is how an operator refers to it; a definition whose
		// declared name disagrees is a definition that lists confusingly.
		if want := strings.TrimSuffix(entry.Name(), ".yaml"); spec.Name != want {
			t.Errorf("%s declares name %q", entry.Name(), spec.Name)
		}
	}
}

// ---- scout: two read-only agent phases, handoff between them --------------

func TestScoutRunsTwoReadOnlyPhasesAndLeavesTheRepositoryUntouched(t *testing.T) {
	repo := initRepo(t)
	data := t.TempDir()
	scriptRuntime(t,
		map[string]any{"text": envelopeJSON(t, map[string]any{
			"status": "success", "summary": "one README, no build files",
			"notes_for_next_agent": "read README.md",
		})},
		map[string]any{"text": envelopeJSON(t, map[string]any{
			"status": "success", "summary": "yes — nothing pins an older toolchain",
			"approved": true, "blocking": []any{},
			"findings": []any{map[string]any{
				"requirement": "no toolchain pin", "met": true, "evidence": "no go.mod",
			}},
		})},
	)

	code, report, stderr := runJigJSON(t, "--def", stockDefinition("scout.yaml"),
		"--data", data, repo, "can this repository adopt a newer toolchain?")
	if code != exitAccepted {
		t.Fatalf("exit = %d, want %d (stderr: %s)", code, exitAccepted, stderr)
	}
	events := traceEvents(t, report.TracePath)
	assertPhasesRan(t, events, "survey", "assess")
	// Both phases handed off through the envelope contract — the seam the
	// second phase exists to prove (R8).
	if handoffs := eventTypes(events, protocol.EventHandoff); handoffs != 2 {
		t.Fatalf("trace records %d handoff event(s), want 2: %v", handoffs, eventNames(events))
	}
	assertRepositoryClean(t, repo)
}

// ---- plan-build-test: the code-phase repair edge ---------------------------

// makeFixture is a repository whose `make test` passes only once the builder
// has written the marker file — so the test phase's verdict is a fact about
// the build, not about the fixture.
func makeFixture(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("make"); err != nil {
		t.Skip("make is not installed; the stock test command detects it first")
	}
	repo := initRepo(t)
	makefile := "test:\n\t@test -f built.txt\n"
	if err := os.WriteFile(filepath.Join(repo, "Makefile"), []byte(makefile), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, repo, "add", "Makefile")
	gitIn(t, repo, "commit", "--quiet", "-m", "make test")
	return repo
}

func TestPlanBuildTestPassesWhenTheBuildSatisfiesTheTestCommand(t *testing.T) {
	repo := makeFixture(t)
	scriptRuntime(t,
		map[string]any{"text": envelopeJSON(t, map[string]any{
			"status": "success", "summary": "plan: write built.txt",
			"notes_for_next_agent": "create built.txt",
		})},
		map[string]any{
			"files": map[string]any{"built.txt": "ok\n"},
			"text": envelopeJSON(t, map[string]any{
				"status": "success", "summary": "wrote built.txt",
				"artifacts": []any{"built.txt"}, "changed_files": []any{"built.txt"},
			}),
		},
	)

	code, report, stderr := runJigJSON(t, "--def", stockDefinition("plan-build-test.yaml"),
		"--data", t.TempDir(), repo, "make the build marker")
	if code != exitAccepted {
		t.Fatalf("exit = %d, want %d (stderr: %s)", code, exitAccepted, stderr)
	}
	assertPhasesRan(t, traceEvents(t, report.TracePath), "plan", "build", "test")
}

func TestPlanBuildTestReplansThenBuildsAndRetestsAFailingSuite(t *testing.T) {
	repo := makeFixture(t)
	scriptRuntime(t,
		map[string]any{"text": envelopeJSON(t, map[string]any{
			"status": "success", "summary": "plan: write built.txt",
			"notes_for_next_agent": "create built.txt",
		})},
		// The first build writes the wrong file: `make test` fails, which is
		// what the repair edge exists for.
		map[string]any{
			"files": map[string]any{"notes.txt": "wrong file\n"},
			"text": envelopeJSON(t, map[string]any{
				"status": "success", "summary": "wrote notes.txt",
				"artifacts": []any{"notes.txt"}, "changed_files": []any{"notes.txt"},
			}),
		},
		// Dispatched by the edge with the failing adapter envelope in hand.
		map[string]any{"text": envelopeJSON(t, map[string]any{
			"status": "success", "summary": "revised plan: create the missing marker",
			"notes_for_next_agent": "create built.txt and preserve notes.txt",
		})},
		map[string]any{
			"files": map[string]any{"built.txt": "ok\n"},
			"text": envelopeJSON(t, map[string]any{
				"status": "success", "summary": "fixed: wrote built.txt",
				"artifacts": []any{"built.txt"}, "changed_files": []any{"built.txt"},
			}),
		},
	)

	code, report, stderr := runJigJSON(t, "--def", stockDefinition("plan-build-test.yaml"),
		"--data", t.TempDir(), repo, "make the build marker")
	if code != exitAccepted {
		t.Fatalf("exit = %d, want %d (stderr: %s)", code, exitAccepted, stderr)
	}
	events := traceEvents(t, report.TracePath)
	if uses := eventPayloads(events, "repair_edge"); len(uses) != 1 {
		t.Fatalf("repair edge fired %d time(s), want exactly 1", len(uses))
	}
	if entries := phaseStarts(events, "build"); entries != 2 {
		t.Fatalf("build ran %d time(s), want 2 (the original and the repair)", entries)
	}
	if entries := phaseStarts(events, "plan"); entries != 2 {
		t.Fatalf("plan ran %d time(s), want initial plan and repair plan", entries)
	}
	if entries := phaseStarts(events, "test"); entries != 2 {
		t.Fatalf("test ran %d time(s), want 2 (the failure and the rerun)", entries)
	}
}

// ---- simple-sdlc: the agent-phase edge and the conditional retest ----------

// The plan's acceptance criterion for the stock library: the conditional
// retest fires exactly when a revision occurred. These two tests are the
// two sides of "exactly".

func TestSimpleSdlcSkipsTheRetestWhenTheReviewApprovesFirstTime(t *testing.T) {
	repo := makeFixture(t)
	scriptRuntime(t,
		planStep(t),
		buildStep(t, "first build\n", false, "add the build marker\n\nThe build phase authored this.\n"),
		reviewStep(t, true),
	)

	code, report, stderr := runJigJSON(t, "--def", stockDefinition("simple-sdlc.yaml"),
		"--data", t.TempDir(), repo, "make the build marker")
	if code != exitAccepted {
		t.Fatalf("exit = %d, want %d (stderr: %s)", code, exitAccepted, stderr)
	}
	events := traceEvents(t, report.TracePath)
	assertPhasesRan(t, events, "plan", "build", "commit-build", "test", "review")
	assertPhasesSkipped(t, events, "commit-revision", "retest")
	if fired := len(eventPayloads(events, "repair_edge")); fired != 0 {
		t.Fatalf("repair edge fired %d time(s) on an approving review", fired)
	}
	// Per-phase commit message: the commit that carries the work is the one
	// the build phase authored, not a jig-composed fallback.
	if subject := headSubject(t, repo); subject != "add the build marker" {
		t.Fatalf("HEAD subject = %q, want the build phase's own message", subject)
	}
}

func TestSimpleSdlcRunsTheRetestExactlyWhenTheReviewForcedARevision(t *testing.T) {
	repo := makeFixture(t)
	scriptRuntime(t,
		planStep(t),
		buildStep(t, "first build\n", false, "add the build marker\n"),
		// The review rejects, naming what blocks it.
		reviewStep(t, false),
		// The builder is dispatched with that review in hand and reports the
		// revision — the field the retest guard reads.
		buildStep(t, "revised build\n", true, "fix the marker after review\n\nAddresses the review.\n"),
		reviewStep(t, true),
	)

	code, report, stderr := runJigJSON(t, "--def", stockDefinition("simple-sdlc.yaml"),
		"--data", t.TempDir(), repo, "make the build marker")
	if code != exitAccepted {
		t.Fatalf("exit = %d, want %d (stderr: %s)", code, exitAccepted, stderr)
	}
	events := traceEvents(t, report.TracePath)
	if fired := len(eventPayloads(events, "repair_edge")); fired != 1 {
		t.Fatalf("repair edge fired %d time(s), want exactly 1", fired)
	}
	assertPhasesRan(t, events, "commit-revision", "retest")
	if entries := phaseStarts(events, "retest"); entries != 1 {
		t.Fatalf("retest ran %d time(s), want exactly 1", entries)
	}
	if entries := phaseStarts(events, "review"); entries != 2 {
		t.Fatalf("review ran %d time(s), want 2 (the rejection and the re-review)", entries)
	}
	// Each writing phase's own message reached its own commit: the revision
	// commit carries the revising build's subject, and the build commit is
	// still underneath it.
	if subject := headSubject(t, repo); subject != "fix the marker after review" {
		t.Fatalf("HEAD subject = %q, want the revising phase's own message", subject)
	}
	if subject := commitSubject(t, repo, "HEAD~1"); subject != "add the build marker" {
		t.Fatalf("HEAD~1 subject = %q, want the build phase's own message", subject)
	}
}

// ---- scripted steps for simple-sdlc ---------------------------------------

func planStep(t *testing.T) map[string]any {
	t.Helper()
	return map[string]any{"text": envelopeJSON(t, map[string]any{
		"status": "success", "summary": "plan: write built.txt",
		"notes_for_next_agent": "create built.txt",
	})}
}

// buildStep writes the marker the fixture's `make test` looks for, plus the
// phase's own commit message into the EPHEMERAL HOME (the "~/" prefix is the
// scripted runtime's HOME-relative form) — the exact path the definition's
// commit phase reads with `git commit -F`.
func buildStep(t *testing.T, content string, revised bool, commitMessage string) map[string]any {
	t.Helper()
	return map[string]any{
		"files": map[string]any{
			"built.txt":        content,
			"~/commit-message": commitMessage,
		},
		"text": envelopeJSON(t, map[string]any{
			"status": "success", "summary": "wrote built.txt",
			"artifacts": []any{"built.txt"}, "changed_files": []any{"built.txt"},
			"revised": revised,
		}),
	}
}

func reviewStep(t *testing.T, approved bool) map[string]any {
	t.Helper()
	fields := map[string]any{
		"status": "success", "summary": "reviewed the change",
		"approved": approved, "blocking": []any{},
		"findings": []any{map[string]any{
			"requirement": "the marker exists", "met": approved, "evidence": "built.txt",
		}},
	}
	if !approved {
		fields["blocking"] = []any{"built.txt does not say what the task asked for"}
	}
	return map[string]any{"text": envelopeJSON(t, fields)}
}

// ---- trace assertions ------------------------------------------------------

func traceEvents(t *testing.T, path string) []protocol.Event {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("trace %s is unreadable: %v", path, err)
	}
	var events []protocol.Event
	for _, line := range strings.Split(strings.TrimSpace(string(body)), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var event protocol.Event
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatalf("trace line does not parse: %v", err)
		}
		events = append(events, event)
	}
	return events
}

func eventNames(events []protocol.Event) []string {
	names := make([]string, 0, len(events))
	for _, event := range events {
		names = append(names, event.Name)
	}
	return names
}

func eventPayloads(events []protocol.Event, name string) []protocol.Event {
	var matched []protocol.Event
	for _, event := range events {
		if event.Name == name {
			matched = append(matched, event)
		}
	}
	return matched
}

func eventTypes(events []protocol.Event, eventType string) int {
	count := 0
	for _, event := range events {
		if event.Type == eventType {
			count++
		}
	}
	return count
}

func phaseStarts(events []protocol.Event, phase string) int {
	count := 0
	for _, event := range events {
		if event.Type == protocol.EventPhaseStart && event.Phase == phase {
			count++
		}
	}
	return count
}

func assertPhasesRan(t *testing.T, events []protocol.Event, phases ...string) {
	t.Helper()
	for _, phase := range phases {
		if phaseStarts(events, phase) == 0 {
			t.Errorf("phase %q never ran", phase)
		}
	}
}

func assertPhasesSkipped(t *testing.T, events []protocol.Event, phases ...string) {
	t.Helper()
	skipped := make(map[string]bool)
	for _, event := range eventPayloads(events, "phase_skipped") {
		skipped[event.Phase] = true
	}
	for _, phase := range phases {
		if !skipped[phase] {
			t.Errorf("phase %q was not skipped; its guard should not have held", phase)
		}
		if phaseStarts(events, phase) != 0 {
			t.Errorf("phase %q ran despite its guard", phase)
		}
	}
}

// ---- repository helpers ----------------------------------------------------

func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	command := exec.Command("git", args...)
	command.Dir = dir
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, output)
	}
	return strings.TrimSpace(string(output))
}

func headSubject(t *testing.T, repo string) string {
	t.Helper()
	return commitSubject(t, repo, "HEAD")
}

func commitSubject(t *testing.T, repo, revision string) string {
	t.Helper()
	return gitIn(t, repo, "log", "-1", "--format=%s", revision)
}

func assertRepositoryClean(t *testing.T, repo string) {
	t.Helper()
	if status := gitIn(t, repo, "status", "--porcelain"); status != "" {
		t.Fatalf("a read-only definition dirtied the repository: %q", status)
	}
}
