// worker_test.go — the U9 `jig worker` scenario, end to end against a live
// `jig serve`: the worker registers, claims a queued job, runs its chain,
// publishes the attempt-scoped branch to the target repository, and reaps its
// ephemeral scratch when the operator interrupts it.
//
// Publish stops short of a pull request here on purpose. The push and the
// remote-ref proof are the halves that need no GitHub account, and they are
// the halves that prove the wiring: the branch either exists on the remote or
// it does not. Pull-request creation is GitHub-coupled and belongs to the
// milestone's manual exit gate, so this run legitimately ends
// `accepted_unpublished` with its push recorded.
package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/StructuPath/jig/internal/controlplane"
	"github.com/StructuPath/jig/internal/protocol"
)

func TestWorkerRegistersClaimsRunsPublishesAndReapsItsScratchOnInterrupt(t *testing.T) {
	repo := initRepo(t)
	serverData := t.TempDir()
	workerData := t.TempDir()
	scriptRuntime(t, map[string]any{
		"files": map[string]any{"jig-note.txt": "steel is real\n"},
		"text": envelopeJSON(t, map[string]any{
			"status": "success", "summary": "wrote the note",
			"artifacts": []any{"jig-note.txt"}, "changed_files": []any{"jig-note.txt"},
		}),
	})

	base, stopServe := startServe(t, serverData, "--no-github-poll")
	defer stopServe()

	// Author and admit through the CLI's own API surface, which is the path
	// an operator (or an agent driving jig) actually takes.
	source, err := os.ReadFile(stockDefinition("two-phase.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var definition protocol.Definition
	if err := apiCall(context.Background(), base, "POST", "/api/definitions",
		controlplane.DefinitionInput{Source: string(source)}, &definition); err != nil {
		t.Fatal(err)
	}
	var view controlplane.RunView
	if err := apiCall(context.Background(), base, "POST", "/api/runs", controlplane.RunInvocation{
		DefinitionID: definition.ID,
		Instructions: "write the note",
		Targets:      []controlplane.InvocationTarget{{Repository: repo}},
	}, &view); err != nil {
		t.Fatal(err)
	}
	if len(view.Jobs) != 1 {
		t.Fatalf("run fanned out into %d job(s), want 1", len(view.Jobs))
	}
	jobID := view.Jobs[0].ID

	workerCtx, interrupt := context.WithCancel(context.Background())
	stdout, stderr := &syncBuffer{}, &syncBuffer{}
	exited := make(chan int, 1)
	go func() {
		exited <- workerCommand(workerCtx,
			[]string{"--server", base, "--data", workerData}, stdout, stderr)
	}()

	job := awaitTerminalJob(t, base, view.Run.ID, jobID)
	// The chain passed and the predicate held; publish then ran and could not
	// open a pull request against a local repository, which is exactly the
	// `accepted_unpublished` state (R12/R14) rather than a failure.
	if job.State != protocol.JobAcceptedUnpublished && job.State != protocol.JobAccepted {
		t.Fatalf("job state = %q, want accepted or accepted_unpublished\nworker stderr:\n%s",
			job.State, stderr.String())
	}
	// The push half of publish really ran: the attempt-scoped branch is on
	// the remote, which is the only proof that counts (R14).
	branch := protocol.PublishBranch(jobID, 1)
	if branches := gitIn(t, repo, "branch", "--list", branch); !strings.Contains(branches, branch) {
		t.Fatalf("publish did not push %s to the target repository (branches: %q)\nstderr:\n%s",
			branch, branches, stderr.String())
	}
	if !strings.Contains(stdout.String(), "jig worker: ") {
		t.Fatalf("the worker printed no startup report:\n%s", stdout.String())
	}
	// The trace reached the control plane, which is the UI's only source
	// (KTD8), and the attempt-local JSONL raw record is on the worker's disk.
	assertTraceStreamed(t, base, workerData, job.ID)

	interrupt()
	select {
	case code := <-exited:
		if code != exitAccepted {
			t.Fatalf("jig worker exited %d on interrupt\nstderr:\n%s", code, stderr.String())
		}
	case <-time.After(60 * time.Second):
		t.Fatal("jig worker did not stop on interrupt")
	}
	// KTD11: the ephemeral HOME held seeded credentials. Nothing of it
	// survives the attempt, let alone the process.
	entries, err := os.ReadDir(filepath.Join(workerData, "scratch"))
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("attempt scratch survived: %v", entries)
	}
}

// assertTraceStreamed proves the worker's event transport is wired: the
// control plane holds the attempt's events in seq order, and the worker kept
// the raw JSONL record they were replayed from.
func assertTraceStreamed(t *testing.T, base, workerData, jobID string) {
	t.Helper()
	var jobView struct {
		Attempts []protocol.Attempt `json:"attempts"`
	}
	if err := apiCall(context.Background(), base, "GET", "/api/jobs/"+jobID, nil, &jobView); err != nil {
		t.Fatalf("GET job: %v", err)
	}
	if len(jobView.Attempts) != 1 {
		t.Fatalf("job carries %d attempt(s), want 1", len(jobView.Attempts))
	}
	attemptID := jobView.Attempts[0].ID

	var page struct {
		Events []protocol.Event `json:"events"`
	}
	if err := apiCall(context.Background(), base, "GET",
		"/api/attempts/"+attemptID+"/events?limit=500", nil, &page); err != nil {
		t.Fatalf("GET attempt events: %v", err)
	}
	if len(page.Events) == 0 {
		t.Fatal("the control plane holds no events: the worker's trace stream is not wired")
	}
	for i, event := range page.Events {
		if event.Seq != int64(i+1) {
			t.Fatalf("event %d has seq %d: the stream is not seq-ordered", i, event.Seq)
		}
	}
	if _, err := os.Stat(filepath.Join(workerData, "traces", attemptID+".jsonl")); err != nil {
		t.Fatalf("the attempt-local JSONL raw record is missing: %v", err)
	}
}

// awaitTerminalJob polls the run until its single job leaves the active
// states, which is what the worker having finished looks like from outside.
func awaitTerminalJob(t *testing.T, base, runID, jobID string) protocol.Job {
	t.Helper()
	active := map[string]bool{protocol.JobQueued: true, protocol.JobActive: true}
	deadline := time.Now().Add(90 * time.Second)
	var last protocol.Job
	for time.Now().Before(deadline) {
		var view controlplane.RunView
		if err := apiCall(context.Background(), base, "GET",
			"/api/runs/"+runID, nil, &view); err != nil {
			t.Fatalf("GET run: %v", err)
		}
		for _, job := range view.Jobs {
			if job.ID != jobID {
				continue
			}
			last = job
			if !active[job.State] {
				return job
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("job %s never reached a terminal state (last: %q)", jobID, last.State)
	return last
}

// ---- the def and trigger CLI surfaces --------------------------------------

func TestDefCommandsCreateListShowAndInvokeAgainstTheAPIWithJSONOnEveryRead(t *testing.T) {
	repo := initRepo(t)
	base, stop := startServe(t, t.TempDir(), "--no-github-poll")
	defer stop()

	created := runDef(t, exitAccepted, "create", "--server", base, stockDefinition("scout.yaml"))
	definitionID, _, _ := strings.Cut(created, "  ")
	if definitionID == "" {
		t.Fatalf("create printed no id: %q", created)
	}

	// --json on every read: the agent-native contract. Each of these parses
	// as the API's own object, not as prose.
	listed := runDef(t, exitAccepted, "list", "--server", base, "--json")
	if !strings.Contains(listed, `"name": "scout"`) {
		t.Fatalf("list --json does not carry the API object:\n%s", listed)
	}
	shown := runDef(t, exitAccepted, "show", "--server", base, "--json", definitionID)
	if !strings.Contains(shown, `"source"`) {
		t.Fatalf("show --json omits the definition source:\n%s", shown)
	}

	invoked := runDef(t, exitAccepted, "invoke", "--server", base, "--json",
		"--instructions", "survey this repository", "--repo", repo, definitionID)
	if !strings.Contains(invoked, `"jobs"`) || !strings.Contains(invoked, `"base_sha"`) {
		t.Fatalf("invoke --json does not report the fanned-out jobs with pinned SHAs:\n%s", invoked)
	}

	// An unknown definition is the control plane's own diagnostic, verbatim.
	failure := runDefStderr(t, exitInfraFailed, "show", "--server", base, "no-such-definition")
	if !strings.Contains(failure, "not_found") {
		t.Fatalf("stderr does not carry the control plane's code:\n%s", failure)
	}
}

func TestTriggerCommandsCreateListAndToggleEnablement(t *testing.T) {
	repo := initRepo(t)
	base, stop := startServe(t, t.TempDir(), "--no-github-poll")
	defer stop()

	created := runDef(t, exitAccepted, "create", "--server", base, stockDefinition("scout.yaml"))
	definitionID, _, _ := strings.Cut(created, "  ")

	var stdout, stderr syncBuffer
	code := triggerCommand(context.Background(), []string{"create",
		"--server", base, "--json", "--def", definitionID, "--name", "nightly-scout",
		"--kind", controlplane.TriggerSchedule, "--cron", "0 3 * * *",
		"--timezone", "America/Denver", "--repo", repo,
		"--instructions", "survey this repository every night"}, &stdout, &stderr)
	if code != exitAccepted {
		t.Fatalf("trigger create exited %d\nstderr: %s", code, stderr.String())
	}
	triggerID := jsonField(t, stdout.String(), "id")

	listed := runTrigger(t, exitAccepted, "list", "--server", base, "--json")
	if !strings.Contains(listed, triggerID) {
		t.Fatalf("list --json omits the trigger:\n%s", listed)
	}
	// A new trigger is live; disabling and re-enabling it is the operator's
	// stop button for a schedule that is misbehaving.
	disabled := runTrigger(t, exitAccepted, "disable", "--server", base, "--json", triggerID)
	if !strings.Contains(disabled, `"enabled": false`) {
		t.Fatalf("disable did not disable the trigger:\n%s", disabled)
	}
	enabled := runTrigger(t, exitAccepted, "enable", "--server", base, "--json", triggerID)
	if !strings.Contains(enabled, `"enabled": true`) {
		t.Fatalf("enable did not enable the trigger:\n%s", enabled)
	}

	// A GitHub trigger watches exactly one repository — never a URL read out
	// of a ticket (R17) — and says so rather than dropping the extras.
	var out, errs syncBuffer
	code = triggerCommand(context.Background(), []string{"create",
		"--server", base, "--def", definitionID, "--name", "issues",
		"--kind", controlplane.TriggerGitHubIssue,
		"--repo", "github.com/StructuPath/jig", "--repo", "github.com/StructuPath/other",
		"--instructions", "work the issue"}, &out, &errs)
	if code != exitUsage {
		t.Fatalf("two --repo flags on a github trigger exited %d, want %d", code, exitUsage)
	}
	if !strings.Contains(errs.String(), "exactly one --repo") {
		t.Fatalf("stderr does not name the rule:\n%s", errs.String())
	}
}

// ---- helpers ---------------------------------------------------------------

func runDef(t *testing.T, want int, args ...string) string {
	t.Helper()
	var stdout, stderr syncBuffer
	code := defCommand(context.Background(), args, &stdout, &stderr)
	if code != want {
		t.Fatalf("jig def %v exited %d, want %d\nstdout:\n%s\nstderr:\n%s",
			args, code, want, stdout.String(), stderr.String())
	}
	return stdout.String()
}

func runDefStderr(t *testing.T, want int, args ...string) string {
	t.Helper()
	var stdout, stderr syncBuffer
	code := defCommand(context.Background(), args, &stdout, &stderr)
	if code != want {
		t.Fatalf("jig def %v exited %d, want %d\nstderr:\n%s", args, code, want, stderr.String())
	}
	return stderr.String()
}

func runTrigger(t *testing.T, want int, args ...string) string {
	t.Helper()
	var stdout, stderr syncBuffer
	code := triggerCommand(context.Background(), args, &stdout, &stderr)
	if code != want {
		t.Fatalf("jig trigger %v exited %d, want %d\nstdout:\n%s\nstderr:\n%s",
			args, code, want, stdout.String(), stderr.String())
	}
	return stdout.String()
}

// jsonField pulls one top-level string field out of a --json payload.
func jsonField(t *testing.T, body, field string) string {
	t.Helper()
	marker := fmt.Sprintf("%q: ", field)
	index := strings.Index(body, marker)
	if index < 0 {
		t.Fatalf("payload has no %q field:\n%s", field, body)
	}
	rest := body[index+len(marker):]
	value, _, _ := strings.Cut(strings.TrimPrefix(strings.TrimSpace(rest), `"`), `"`)
	if value == "" {
		t.Fatalf("field %q is empty:\n%s", field, body)
	}
	return value
}
