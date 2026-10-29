// publish_retry_test.go — `jig worker publish-retry` end to end against a
// live `jig serve`, and the data-directory lock it shares with `jig worker`.
// Everything is production except the runtime (scripted, and removed before
// the retry) and GitHub (a fake gateway): a held release is published by the
// retry, red CI leaves it accepted_unpublished, and a second retry on green
// CI accepts it — the same attempt, branch, and pull request throughout, and
// not one more agent call.
package main

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/StructuPath/jig/internal/controlplane"
	"github.com/StructuPath/jig/internal/protocol"
	"github.com/StructuPath/jig/internal/worker"
)

const publishRetryDefinition = `name: publish-retry-e2e
roster:
  builder:
    model: test-model
    system_prompt: Build.
    user_prompt: "Build."
    env: [PATH]
    writes: ["jig-note.txt"]
phases:
  - {name: build, kind: agent, owner: builder}
  - name: classify
    kind: code
    reports_fields: true
    command: |
      echo '{"risk":"high"}'
acceptance: [all_phases_passed]
publish:
  hold_when: "risk != low"
  ci:
    wait: true
`

// retryGateway is the fake GitHub: one pull request per head, CI red until
// the test turns it green, and every judged head remembered.
type retryGateway struct {
	mutex   sync.Mutex
	green   bool
	prs     map[string]worker.PullRequest
	created int
	found   int
	judged  []string
}

func (g *retryGateway) FindOrCreatePullRequest(_ context.Context, request worker.PullRequestRequest) (worker.PullRequest, error) {
	g.mutex.Lock()
	defer g.mutex.Unlock()
	if pr, exists := g.prs[request.Head]; exists {
		g.found++
		return pr, nil
	}
	g.created++
	pr := worker.PullRequest{Number: g.created, URL: fmt.Sprintf("https://github.com/example/repo/pull/%d", g.created), State: "open"}
	g.prs[request.Head] = pr
	return pr, nil
}

func (g *retryGateway) CommitChecks(_ context.Context, _ string, sha string) ([]worker.CICheck, error) {
	g.mutex.Lock()
	defer g.mutex.Unlock()
	g.judged = append(g.judged, sha)
	if g.green {
		return []worker.CICheck{{Name: "test", Verdict: worker.CIPass, Conclusion: "success"}}, nil
	}
	return []worker.CICheck{{Name: "test", Verdict: worker.CIFail, Conclusion: "failure"}}, nil
}

func (g *retryGateway) FailedCheckLogs(_ context.Context, _ string, checks []worker.CICheck) []worker.CICheck {
	return checks
}

func (g *retryGateway) ActionsJobRun(context.Context, string, int64) (int64, error) {
	return 0, fmt.Errorf("unexpected workflow run lookup")
}

func (g *retryGateway) RerunFailedJobs(context.Context, string, int64) error {
	return fmt.Errorf("unexpected re-run request")
}

func (g *retryGateway) counts() (created, found int, judged []string) {
	g.mutex.Lock()
	defer g.mutex.Unlock()
	return g.created, g.found, append([]string(nil), g.judged...)
}

// useGateway substitutes the fake GitHub for every command in this test.
func useGateway(t *testing.T, gateway worker.PullRequestGateway) {
	t.Helper()
	previous := newPublishGateway
	newPublishGateway = func() worker.PullRequestGateway { return gateway }
	t.Cleanup(func() { newPublishGateway = previous })
}

func publishRetry(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	stdout, stderr := &syncBuffer{}, &syncBuffer{}
	code := workerCommand(context.Background(), append([]string{"publish-retry"}, args...), stdout, stderr)
	t.Logf("jig worker publish-retry %v\nexit: %d\nstdout:\n%s\nstderr:\n%s", args, code, stdout, stderr)
	return code, stdout.String(), stderr.String()
}

// withoutAnyRuntime leaves PATH holding git alone and switches the scripted
// runtime off: a retry that tried to probe, seed, or start an agent would
// find nothing to run.
func withoutAnyRuntime(t *testing.T) {
	t.Helper()
	git, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	if err := os.Symlink(git, filepath.Join(bin, "git")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	t.Setenv(scriptedRuntimeEnv, "")
}

func agentStarts(t *testing.T, base, attemptID string) int {
	t.Helper()
	starts := 0
	for _, event := range attemptEvents(t, base, attemptID) {
		if event.Type == protocol.EventAgentStart {
			starts++
		}
	}
	return starts
}

func TestPublishRetryPublishesAHeldReleaseOnTheSameAttemptWithoutAnyRuntime(t *testing.T) {
	repo := initRepo(t)
	serverData, workerData := t.TempDir(), t.TempDir()
	scriptRuntime(t, map[string]any{
		"files": map[string]any{"jig-note.txt": "held\n"},
		"text":  envelopeJSON(t, map[string]any{"status": "success", "summary": "wrote the note"}),
	})
	gateway := &retryGateway{prs: map[string]worker.PullRequest{}}
	useGateway(t, gateway)
	base, stopServe := startServe(t, serverData, "--no-github-poll")
	defer stopServe()

	var definition protocol.Definition
	if err := apiCall(context.Background(), base, "POST", "/api/definitions",
		controlplane.DefinitionInput{Source: publishRetryDefinition}, &definition); err != nil {
		t.Fatal(err)
	}
	var view controlplane.RunView
	if err := apiCall(context.Background(), base, "POST", "/api/runs", controlplane.RunInvocation{
		DefinitionID: definition.ID, Instructions: "write the note",
		Targets: []controlplane.InvocationTarget{{Repository: repo}},
	}, &view); err != nil {
		t.Fatal(err)
	}
	jobID := view.Jobs[0].ID

	workerCtx, interrupt := context.WithCancel(context.Background())
	workerOut, workerErr := &syncBuffer{}, &syncBuffer{}
	exited := make(chan int, 1)
	go func() {
		exited <- workerCommand(workerCtx, []string{"--server", base, "--data", workerData}, workerOut, workerErr)
	}()
	job := awaitTerminalJob(t, base, view.Run.ID, jobID)
	if job.State != protocol.JobAcceptedUnpublished {
		t.Fatalf("job = %q, want accepted_unpublished (held)\nworker stderr:\n%s", job.State, workerErr)
	}
	created, _, _ := gateway.counts()
	branch := protocol.PublishBranch(jobID, 1)
	if created != 0 || strings.Contains(gitIn(t, repo, "branch", "--list", branch), branch) {
		t.Fatal("a held release was published before anyone retried it")
	}

	// The retry refuses to run beside the worker that owns the directory —
	// through an alias of the directory too.
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(workerData, alias); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{workerData, alias} {
		code, _, stderr := publishRetry(t, "--server", base, "--data", dir, jobID)
		if code != exitInfraFailed || !strings.Contains(stderr, "in use by another jig worker") {
			t.Fatalf("retry beside a running worker on %s: exit %d, want %d naming the lock", dir, code, exitInfraFailed)
		}
	}
	interrupt()
	select {
	case code := <-exited:
		if code != exitAccepted {
			t.Fatalf("jig worker exited %d\nstderr:\n%s", code, workerErr)
		}
	case <-time.After(60 * time.Second):
		t.Fatal("jig worker did not stop on interrupt")
	}

	attemptID := latestAttempt(t, base, jobID).ID
	startsBefore := agentStarts(t, base, attemptID)
	if startsBefore != 1 {
		t.Fatalf("the chain started %d agents, want 1", startsBefore)
	}
	withoutAnyRuntime(t)

	// Another worker's data directory is refused: only the owner has the
	// attempt's retained state.
	code, _, stderr := publishRetry(t, "--server", base, "--data", t.TempDir(), jobID)
	if code != exitInfraFailed || !strings.Contains(stderr, "publish_retry_foreign_worker") ||
		!strings.Contains(stderr, "--data") {
		t.Fatalf("foreign retry: exit %d, want %d naming the owning worker's --data", code, exitInfraFailed)
	}

	// Retry 1 releases the hold: push, pull request, CI — red.
	code, stdout, _ := publishRetry(t, "--server", base, "--data", workerData,
		"--commit-author-name", "jig-test", "--commit-author-email", "jig@test", jobID)
	if code != exitRejected || stdoutField(t, stdout, "attempt") != attemptID ||
		stdoutField(t, stdout, "state") != protocol.AttemptAcceptedUnpublished ||
		stdoutField(t, stdout, "publish_code") != "ci_failed" {
		t.Fatalf("red-CI retry: exit %d, want %d with the same attempt left accepted_unpublished on ci_failed",
			code, exitRejected)
	}
	pullRequest := stdoutField(t, stdout, "pull_request")
	pushed := strings.TrimSpace(gitIn(t, repo, "rev-parse", "refs/heads/"+branch))

	// Retry 2 on green CI accepts — the same pull request, judged on the
	// branch's current head.
	gateway.mutex.Lock()
	gateway.green = true
	gateway.mutex.Unlock()
	code, stdout, _ = publishRetry(t, "--server", base, "--data", workerData,
		"--commit-author-name", "jig-test", "--commit-author-email", "jig@test", jobID)
	if code != exitAccepted || stdoutField(t, stdout, "attempt") != attemptID ||
		stdoutField(t, stdout, "state") != protocol.AttemptAccepted ||
		stdoutField(t, stdout, "pull_request") != pullRequest ||
		stdoutField(t, stdout, "branch") != branch {
		t.Fatalf("green-CI retry: exit %d, want %d accepting the same attempt, branch, and pull request",
			code, exitAccepted)
	}
	// The second retry re-enters on the ledger's proven pull_request step,
	// so it neither creates nor even looks up a second one.
	created, _, judged := gateway.counts()
	if created != 1 {
		t.Fatalf("pull requests created %d across the retries, want exactly 1", created)
	}
	if len(judged) == 0 || judged[len(judged)-1] != pushed {
		t.Fatalf("CI judged %v, want the branch's current head %s last", judged, pushed)
	}
	if head := strings.TrimSpace(gitIn(t, repo, "rev-parse", "refs/heads/"+branch)); head != pushed {
		t.Fatalf("the branch moved from %s to %s across the retries", pushed, head)
	}
	if starts := agentStarts(t, base, attemptID); starts != startsBefore {
		t.Fatalf("agent starts went from %d to %d across publish-only retries", startsBefore, starts)
	}
	if attempt := latestAttempt(t, base, jobID); attempt.ID != attemptID || attempt.AttemptNumber != 1 {
		t.Fatalf("the job's latest attempt is %s (#%d), want the original %s", attempt.ID, attempt.AttemptNumber, attemptID)
	}

	// An accepted job has nothing left to publish.
	code, _, stderr = publishRetry(t, "--server", base, "--data", workerData, jobID)
	if code != exitInfraFailed || !strings.Contains(stderr, "publish_retry_not_allowed") {
		t.Fatalf("retry of an accepted job: exit %d, want %d naming publish_retry_not_allowed", code, exitInfraFailed)
	}
}

func latestAttempt(t *testing.T, base, jobID string) protocol.Attempt {
	t.Helper()
	var detail controlplane.JobDetail
	if err := apiCall(context.Background(), base, "GET", "/api/jobs/"+jobID, nil, &detail); err != nil {
		t.Fatal(err)
	}
	attempts := detail.Attempts
	if len(attempts) != 1 {
		t.Fatalf("job %s has %d attempts, want exactly one: a publish-only retry never adds one", jobID, len(attempts))
	}
	latest := attempts[0]
	for _, attempt := range attempts[1:] {
		if attempt.AttemptNumber > latest.AttemptNumber {
			latest = attempt
		}
	}
	return latest
}

func TestPublishRetryRequiresExactlyOneJobID(t *testing.T) {
	for _, args := range [][]string{{}, {"job-1", "job-2"}, {"--data", t.TempDir()}} {
		if code, _, _ := publishRetry(t, args...); code != exitUsage {
			t.Fatalf("publish-retry %v exited %d, want %d", args, code, exitUsage)
		}
	}
}

// ---- the data-directory lock across processes -------------------------------

// helperEnv selects what TestHelperProcess does when this test binary is
// re-executed as a child: hold the data lock, or run jig. Unset, it skips.
const helperEnv = "JIG_TEST_HELPER"

func TestHelperProcess(t *testing.T) {
	switch os.Getenv(helperEnv) {
	case "":
		t.Skip("runs only as a child of another test")
	case "hold-lock":
		lock, err := lockWorkerData(os.Getenv("JIG_TEST_HELPER_DIR"))
		if err != nil {
			fmt.Println("error:", err)
			os.Exit(1)
		}
		defer lock.release()
		fmt.Println("locked")
		select {}
	case "jig":
		os.Exit(run(context.Background(), strings.Split(os.Getenv("JIG_TEST_HELPER_ARGS"), "\x1f")))
	}
}

// startHelper re-executes this test binary as a child in the given helper
// mode. The child is killed at cleanup if it is still running.
func startHelper(t *testing.T, mode string, env ...string) (*exec.Cmd, *bufio.Scanner) {
	t.Helper()
	command := exec.Command(os.Args[0], "-test.run=^TestHelperProcess$")
	command.Env = append(append(os.Environ(), helperEnv+"="+mode), env...)
	command.Stderr = os.Stderr
	stdout, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = command.Process.Kill()
		_ = command.Wait()
	})
	return command, bufio.NewScanner(stdout)
}

// Another PROCESS holding the lock refuses both a worker and a retry, by the
// directory's path or an alias of it; once that process is SIGKILLed, the
// kernel has dropped the lock and the directory is usable at once.
func TestTheWorkerDataLockHoldsAcrossProcessesAndDiesWithItsHolder(t *testing.T) {
	dataDir := t.TempDir()
	holder, lines := startHelper(t, "hold-lock", "JIG_TEST_HELPER_DIR="+dataDir)
	if !lines.Scan() || lines.Text() != "locked" {
		t.Fatalf("the holder did not take the lock: %q", lines.Text())
	}
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(dataDir, alias); err != nil {
		t.Fatal(err)
	}
	relative, err := filepath.Rel(mustGetwd(t), dataDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{dataDir, alias, relative} {
		stdout, stderr := &syncBuffer{}, &syncBuffer{}
		if code := workerCommand(context.Background(), []string{"--server", "http://127.0.0.1:1", "--data", dir},
			stdout, stderr); code != exitInfraFailed || !strings.Contains(stderr.String(), "in use by another jig worker") {
			t.Fatalf("worker on %s beside the holder: exit %d stderr %q, want a lock refusal", dir, code, stderr)
		}
		if code, _, stderr := publishRetry(t, "--server", "http://127.0.0.1:1", "--data", dir, "job-1"); code != exitInfraFailed ||
			!strings.Contains(stderr, "in use by another jig worker") {
			t.Fatalf("retry on %s beside the holder: exit %d, want a lock refusal", dir, code)
		}
	}

	if err := holder.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	_ = holder.Wait()
	lock, err := lockWorkerData(alias)
	if err != nil {
		t.Fatalf("the lock outlived its SIGKILLed holder: %v", err)
	}
	lock.release()
	// And a clean exit releases it the same way.
	if lock, err = lockWorkerData(dataDir); err != nil {
		t.Fatalf("a released lock could not be taken again: %v", err)
	}
	lock.release()
}

func mustGetwd(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	return dir
}
