// run_test.go — the U11 suite: the three plan scenarios for `jig run`,
// driven through runCommand in-process with the scripted-runtime test hook
// (JIG_SCRIPTED_RUNTIME) substituting the enginetest fake for the real
// adapter.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/StructuPath/jig/internal/controlplane"
	"github.com/StructuPath/jig/internal/protocol"
)

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

// scriptRuntime writes a scripted-runtime JSON file and points the test
// hook at it.
func scriptRuntime(t *testing.T, steps ...map[string]any) {
	t.Helper()
	body, err := json.Marshal(map[string]any{"steps": steps})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "script.json")
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(scriptedRuntimeEnv, path)
}

func runJig(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := runCommand(args, &stdout, &stderr)
	t.Logf("jig run %v\nexit: %d\nstdout:\n%s\nstderr:\n%s",
		args, code, stdout.String(), stderr.String())
	return code, stdout.String(), stderr.String()
}

func stdoutField(t *testing.T, stdout, field string) string {
	t.Helper()
	for _, line := range strings.Split(stdout, "\n") {
		if value, found := strings.CutPrefix(line, field+": "); found {
			return value
		}
	}
	t.Fatalf("stdout has no %q line:\n%s", field, stdout)
	return ""
}

func envelopeJSON(t *testing.T, fields map[string]any) string {
	t.Helper()
	body, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

// ---- scenario: smoke on a fixture repo completes green ---------------------

func TestRunSmokeDefinitionOnFixtureRepoCompletesGreenAndPrintsTheTracePath(t *testing.T) {
	repo := initRepo(t)
	data := t.TempDir()
	scriptRuntime(t, map[string]any{
		"text": envelopeJSON(t, map[string]any{
			"status": "success", "summary": "a tiny fixture repo with one README",
			"approved": true, "blocking": []any{},
			"notes_for_next_agent": "nothing to hand off",
		}),
	})

	code, stdout, stderr := runJig(t,
		"--def", filepath.Join("..", "..", "examples", "definitions", "smoke.yaml"),
		"--data", data, repo, "what is this repository?")
	if code != 0 {
		t.Fatalf("exit = %d, want 0 (stderr: %s)", code, stderr)
	}
	if !strings.Contains(stdout, "outcome: accepted (publish: not_attempted)") {
		t.Fatalf("stdout does not report the accepted no-publish outcome:\n%s", stdout)
	}
	tracePath := stdoutField(t, stdout, "trace")
	body, err := os.ReadFile(tracePath)
	if err != nil {
		t.Fatalf("printed trace path is not readable: %v", err)
	}
	if !strings.Contains(string(body), "attempt_start") ||
		!strings.Contains(string(body), "acceptance") {
		t.Fatalf("trace at %s does not carry the attempt's events", tracePath)
	}
	// Genuinely read-only: the smoke run left the repository untouched.
	status := exec.Command("git", "status", "--porcelain")
	status.Dir = repo
	if output, err := status.Output(); err != nil || len(bytes.TrimSpace(output)) != 0 {
		t.Fatalf("smoke run dirtied the repository: %q (%v)", output, err)
	}
}

// ---- scenario: invalid definition fails before any subprocess spawns -------

func TestRunWithInvalidDefinitionFailsBeforeAnySubprocessSpawnsNamingTheElement(t *testing.T) {
	// No scripted runtime, an empty PATH, and a nonexistent repository: if
	// validation were not strictly first, the command would fail on git, on
	// the missing claude CLI, or on the repo — with a different message and
	// after spawning something. The definition error must win.
	t.Setenv(scriptedRuntimeEnv, "")
	t.Setenv("PATH", t.TempDir())
	defPath := filepath.Join(t.TempDir(), "bad.yaml")
	source := `
name: bad
roster:
  writer:
    model: haiku
    system_prompt: s
    user_prompt: u
phases:
  - name: build
    kind: agent
    owner: writer
    gates:
      - {name: artifacts_exists}
acceptance: [all_phases_passed]
`
	if err := os.WriteFile(defPath, []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}

	code, _, stderr := runJig(t, "--def", defPath, "--data", t.TempDir(),
		filepath.Join(t.TempDir(), "no-such-repo"), "prompt")
	if code != 2 {
		t.Fatalf("exit = %d, want 2 (usage/validation)", code)
	}
	if !strings.Contains(stderr, `"artifacts_exists"`) || !strings.Contains(stderr, `"build"`) {
		t.Fatalf("stderr does not name the offending element:\n%s", stderr)
	}
	if strings.Contains(stderr, "resolve HEAD") || strings.Contains(stderr, "claude") {
		t.Fatalf("something ran before validation:\n%s", stderr)
	}
}

// ---- scenario: same event stream shape as a server-path run ----------------

func TestDirectRunRecordsTheSameEventStreamShapeAsAServerPathRun(t *testing.T) {
	repo := initRepo(t)
	data := t.TempDir()
	scriptRuntime(t, map[string]any{
		"files": map[string]any{"jig-note.txt": "steel is real\n"},
		"text": envelopeJSON(t, map[string]any{
			"status": "success", "summary": "wrote the note",
			"artifacts":     []any{"jig-note.txt"},
			"changed_files": []any{"jig-note.txt"},
		}),
	})

	code, stdout, stderr := runJig(t,
		"--def", filepath.Join("..", "..", "examples", "definitions", "two-phase.yaml"),
		"--data", data, repo, "steel is real")
	if code != 0 {
		t.Fatalf("exit = %d, want 0 (stderr: %s)", code, stderr)
	}
	attemptID := stdoutField(t, stdout, "attempt")
	tracePath := stdoutField(t, stdout, "trace")

	store, err := controlplane.Open(context.Background(), filepath.Join(data, "jig.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	// Same tables as the server path: the attempt row is terminal with the
	// engine's result (carrying the explicit no-publish marker), and the job
	// mirrors it — recorded through the same fenced transitions.
	attempt, err := store.Attempt(context.Background(), attemptID)
	if err != nil {
		t.Fatal(err)
	}
	if attempt.State != protocol.AttemptAcceptedUnpublished {
		t.Fatalf("attempt state = %q, want accepted_unpublished", attempt.State)
	}
	if !strings.Contains(attempt.Result, `"publish":"not_attempted"`) {
		t.Fatalf("attempt result has no explicit no-publish marker: %.400q", attempt.Result)
	}
	if attempt.RuntimeName != "scripted" {
		t.Fatalf("attempt runtime = %q, want the probed runtime name", attempt.RuntimeName)
	}
	job, err := store.Job(context.Background(), attempt.JobID)
	if err != nil {
		t.Fatal(err)
	}
	if job.State != protocol.JobAcceptedUnpublished {
		t.Fatalf("job state = %q, want accepted_unpublished", job.State)
	}
	if job.BaseSHA == "" {
		t.Fatal("job has no pinned base SHA")
	}

	// Same event stream: the events table holds the attempt's trace under
	// the per-attempt monotonic seq, in order, one row per JSONL line.
	events, err := store.AttemptEvents(context.Background(), attemptID)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) == 0 {
		t.Fatal("no events recorded in the store")
	}
	for i, event := range events {
		if event.Seq != int64(i+1) {
			t.Fatalf("event %d seq = %d, want %d (per-attempt monotonic)", i, event.Seq, i+1)
		}
	}
	if events[0].Name != "attempt_start" {
		t.Fatalf("first event = %q, want attempt_start", events[0].Name)
	}
	if last := events[len(events)-1]; last.Name != "acceptance" {
		t.Fatalf("last event = %q, want acceptance (direct runs end at the predicate)", last.Name)
	}
	traceBody, err := os.ReadFile(tracePath)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(traceBody)), "\n")
	if len(lines) != len(events) {
		t.Fatalf("store has %d events, JSONL trace has %d lines — the two records diverge",
			len(events), len(lines))
	}
	for i, line := range lines {
		var fromTrace protocol.Event
		if err := json.Unmarshal([]byte(line), &fromTrace); err != nil {
			t.Fatalf("trace line %d is not an event: %v", i, err)
		}
		if fromTrace.Seq != events[i].Seq || fromTrace.Type != events[i].Type ||
			fromTrace.Name != events[i].Name || fromTrace.Phase != events[i].Phase {
			t.Fatalf("event %d differs between store (%s/%s/%s seq %d) and trace (%s/%s/%s seq %d)",
				i, events[i].Type, events[i].Phase, events[i].Name, events[i].Seq,
				fromTrace.Type, fromTrace.Phase, fromTrace.Name, fromTrace.Seq)
		}
	}
	// And the run under it froze the definition by value against the pinned
	// HEAD — the same frozen-run shape the server path executes.
	if _, err := store.EnqueueJob(context.Background(), stdoutField(t, stdout, "run"), repo); err == nil {
		t.Fatal("re-enqueueing the run's job should conflict — the run already has this target")
	}
}

// ---- the forcing variant validates ----------------------------------------

func TestForcedFixtureDefinitionValidates(t *testing.T) {
	source, err := os.ReadFile(filepath.Join("testdata", "two-phase-forced.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := protocol.ParseDefinition(source); err != nil {
		t.Fatalf("the exit-gate forcing variant does not validate: %v", err)
	}
}
