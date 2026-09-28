// publish_test.go — the U7 plan scenarios, verbatim, against a real control
// plane behind a real HTTP server and real git repositories. The "remote" is
// a local bare repository (the U3 fixture shape), so push and remote-ref
// proof are exercised for real; the `gh` seam is a fake, because a unit test
// that reaches GitHub is not a unit test.
package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/StructuPath/jig/internal/protocol"
)

// ---- fake gh ---------------------------------------------------------------

// fakeGateway is the substituted `gh`. It counts creations, so "no duplicate
// pull request" is an assertion about behaviour rather than about output, and
// it is find-or-create like the real one: a second call for the same head ref
// returns the first pull request.
type fakeGateway struct {
	mutex      sync.Mutex
	created    int
	adopted    int
	failCreate error
	byHead     map[string]PullRequest
	requests   []PullRequestRequest
	// checks scripts CI: it is called once per poll with the commit being
	// judged and the 1-based poll count. Nil reports no checks at all.
	checks     func(sha string, poll int) ([]CICheck, error)
	checkPolls int
	checkedSHA []string
}

func newFakeGateway() *fakeGateway {
	return &fakeGateway{byHead: make(map[string]PullRequest)}
}

func (g *fakeGateway) FindOrCreatePullRequest(_ context.Context, request PullRequestRequest) (PullRequest, error) {
	g.mutex.Lock()
	defer g.mutex.Unlock()
	g.requests = append(g.requests, request)
	if existing, found := g.byHead[request.Head]; found {
		g.adopted++
		return existing, nil
	}
	if g.failCreate != nil {
		return PullRequest{}, g.failCreate
	}
	g.created++
	pullRequest := PullRequest{
		Number: g.created,
		URL:    fmt.Sprintf("https://github.com/example/repo/pull/%d", g.created),
		State:  "open",
	}
	g.byHead[request.Head] = pullRequest
	return pullRequest, nil
}

func (g *fakeGateway) CommitChecks(_ context.Context, _ string, sha string) ([]CICheck, error) {
	g.mutex.Lock()
	defer g.mutex.Unlock()
	g.checkPolls++
	g.checkedSHA = append(g.checkedSHA, sha)
	if g.checks == nil {
		return nil, nil
	}
	return g.checks(sha, g.checkPolls)
}

func (g *fakeGateway) counts() (created, adopted int) {
	g.mutex.Lock()
	defer g.mutex.Unlock()
	return g.created, g.adopted
}

// ---- fixtures --------------------------------------------------------------

// newPublishingWorker builds a worker whose runner is wrapped by the publish
// pipeline — the worker path an accepted attempt actually travels.
func newPublishingWorker(
	t *testing.T, h *harness, dataDir string, inner AttemptRunner, gateway PullRequestGateway,
) (*Worker, *PublishingRunner) {
	t.Helper()
	runner := NewPublishingRunner(inner, gateway, PublishOptions{
		CommitAuthorName: "jig-test", CommitAuthorEmail: "jig@test",
	})
	w := newTestWorker(t, h, dataDir, 1, runner)
	runner.Bind(w)
	return w, runner
}

// writeAndDeclare is the standard inner runner: it writes files into the
// worktree and returns the engine-shaped result that declares them, leaving
// the commit to publish (which is where the changed-path discipline lives).
func writeAndDeclare(t *testing.T, files map[string]string) AttemptRunner {
	t.Helper()
	return RunnerFunc(func(_ context.Context, attempt *PreparedAttempt) Outcome {
		paths := make([]string, 0, len(files))
		for name, body := range files {
			full := filepath.Join(attempt.WorktreePath, name)
			if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
				t.Errorf("create directory for %s: %v", name, err)
			}
			if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
				t.Errorf("write %s: %v", name, err)
			}
			paths = append(paths, name)
		}
		return Outcome{State: protocol.AttemptAcceptedUnpublished, Result: engineResult(t, paths)}
	})
}

// engineResult is the shape the phase engine hands the worker: acceptance
// evidence, the computed changed paths, and the explicit no-publish marker
// publish replaces.
func engineResult(t *testing.T, paths []string) string {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"phases":        []any{},
		"acceptance":    map[string]any{"passed": true},
		"changed_paths": paths,
		"publish":       "not_attempted",
	})
	if err != nil {
		t.Fatalf("encode engine result: %v", err)
	}
	return string(body)
}

func publishSummaryOf(t *testing.T, result string) PublishSummary {
	t.Helper()
	var document struct {
		Publish PublishSummary `json:"publish"`
	}
	if err := json.Unmarshal([]byte(result), &document); err != nil {
		t.Fatalf("decode publish summary from %q: %v", result, err)
	}
	return document.Publish
}

// remoteBranches lists the attempt-scoped branches the bare "remote" holds.
func remoteBranches(t *testing.T, originDir string) map[string]string {
	t.Helper()
	stdout := gitRun(t, originDir, "for-each-ref", "--format=%(refname:short) %(objectname)", "refs/heads/jig")
	branches := make(map[string]string)
	for _, line := range strings.Split(stdout, "\n") {
		name, sha, found := strings.Cut(strings.TrimSpace(line), " ")
		if !found {
			continue
		}
		branches[name] = sha
	}
	return branches
}

func expireLease(t *testing.T, h *harness, attemptID string) {
	t.Helper()
	if _, err := h.db.Exec(`UPDATE attempts SET lease_expires_at = ? WHERE id = ?`,
		time.Now().Add(-time.Hour).UnixMilli(), attemptID); err != nil {
		t.Fatalf("expire lease: %v", err)
	}
}

// ---- scenario: the whole pipeline, end to end ------------------------------

// The base case the other scenarios vary from: an accepted attempt reaches
// `accepted` only with all three steps proven, the branch is on the remote at
// the published commit, and the worktree — now provably published — is
// cleaned up (R12, R14, R16).
func TestAnAcceptedAttemptPublishesItsBranchPullRequestAndProof(t *testing.T) {
	h := newHarness(t)
	originDir, head, identity := newOriginRepo(t)
	h.seedRun("run-1", protocol.RunTarget{Repository: identity, BaseSHA: head})
	job := h.enqueue("run-1", identity)

	gateway := newFakeGateway()
	dataDir := filepath.Join(t.TempDir(), "worker")
	w, _ := newPublishingWorker(t, h, dataDir,
		writeAndDeclare(t, map[string]string{"work.txt": "published work\n"}), gateway)

	attempt, err := w.ClaimOnce(context.Background())
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if attempt == nil || attempt.State != protocol.AttemptAccepted {
		t.Fatalf("attempt = %+v, want accepted", attempt)
	}
	summary := publishSummaryOf(t, attempt.Result)
	if !summary.Published() {
		t.Fatalf("publish summary = %+v, want published", summary)
	}
	branch := protocol.PublishBranch(job.ID, 1)
	if summary.Branch != branch || summary.PullRequestURL == "" || summary.RemoteRef == "" {
		t.Fatalf("publish summary = %+v, want branch %s with a pull request and a remote ref",
			summary, branch)
	}
	branches := remoteBranches(t, originDir)
	if branches[branch] != summary.RemoteRef {
		t.Fatalf("remote holds %v, want %s at %s", branches, branch, summary.RemoteRef)
	}
	if created, _ := gateway.counts(); created != 1 {
		t.Fatalf("gateway created %d pull requests, want 1", created)
	}
	records, err := w.client.AttemptPublishRecords(context.Background(), attempt.ID)
	if err != nil {
		t.Fatalf("read publish records: %v", err)
	}
	if len(records) != 3 {
		t.Fatalf("publish records = %+v, want push, pull_request, and proof", records)
	}
	// Only the declared path was committed: the tree the remote holds is
	// exactly the base plus work.txt, never `git add -A`'s idea of the tree.
	files := gitRun(t, originDir, "ls-tree", "-r", "--name-only", branch)
	if files != "README.md\nwork.txt" && files != "work.txt\nREADME.md" {
		t.Fatalf("published tree = %q, want exactly README.md and work.txt", files)
	}
	// Published work is provably on the remote, so cleanup may delete it.
	if remaining := listDirNames(t, filepath.Join(dataDir, "worktrees")); len(remaining) != 0 {
		t.Fatalf("a published worktree survived cleanup: %v", remaining)
	}
	jobAfter, err := h.store.Job(context.Background(), job.ID)
	if err != nil {
		t.Fatalf("read job: %v", err)
	}
	if jobAfter.State != protocol.JobAccepted {
		t.Fatalf("job state = %q, want accepted", jobAfter.State)
	}
}

// Publish stages the engine's computed changed paths and nothing else. When
// the worktree holds changes the attempt never declared, they stay out of the
// commit — and when they are all that is left, the index escape check refuses
// rather than committing a stranger's file. This is the SSSF `.pyc` lesson
// (fifteen stray files, one `git add -A`) as a check.
func TestPublishCommitsOnlyTheDeclaredChangedPaths(t *testing.T) {
	h := newHarness(t)
	originDir, head, identity := newOriginRepo(t)
	h.seedRun("run-1", protocol.RunTarget{Repository: identity, BaseSHA: head})
	job := h.enqueue("run-1", identity)

	inner := RunnerFunc(func(_ context.Context, attempt *PreparedAttempt) Outcome {
		for name, body := range map[string]string{
			"declared.txt": "declared\n",
			"stray.pyc":    "undeclared bytecode\n",
		} {
			if err := os.WriteFile(filepath.Join(attempt.WorktreePath, name), []byte(body), 0o644); err != nil {
				t.Errorf("write %s: %v", name, err)
			}
		}
		return Outcome{State: protocol.AttemptAcceptedUnpublished,
			Result: engineResult(t, []string{"declared.txt"})}
	})
	gateway := newFakeGateway()
	dataDir := filepath.Join(t.TempDir(), "worker")
	w, _ := newPublishingWorker(t, h, dataDir, inner, gateway)

	attempt, err := w.ClaimOnce(context.Background())
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if attempt == nil || attempt.State != protocol.AttemptAccepted {
		t.Fatalf("attempt = %+v, want accepted", attempt)
	}
	branch := protocol.PublishBranch(job.ID, 1)
	files := gitRun(t, originDir, "ls-tree", "-r", "--name-only", branch)
	if strings.Contains(files, "stray.pyc") {
		t.Fatalf("published tree %q carries the undeclared stray.pyc", files)
	}
	if !strings.Contains(files, "declared.txt") {
		t.Fatalf("published tree %q is missing the declared path", files)
	}
	// The undeclared file is still in the worktree, which is therefore dirty
	// and retained rather than deleted — surfaced, not swept away.
	if remaining := listDirNames(t, filepath.Join(dataDir, "worktrees")); len(remaining) != 1 {
		t.Fatalf("worktrees = %v, want the dirty worktree retained", remaining)
	}
}

// ---- scenario 1 ------------------------------------------------------------

// A zombie attempt (expired lease) reaching a publish step is rejected by the
// fence; the successor attempt publishes its own branch cleanly; a stray
// branch pushed just before expiry appears in the reconcile report.
//
// The stray branch is the honest limit of KTD6 made visible: the fence cannot
// fence GitHub, so the branch a zombie pushed a moment before its lease died
// still exists. What jig guarantees is that it can only ever be that
// attempt's own branch, and that reconciliation names it.
func TestAZombieIsFencedOutWhileItsSuccessorPublishesAndItsStrayBranchIsReported(t *testing.T) {
	h := newHarness(t)
	originDir, head, identity := newOriginRepo(t)
	h.seedRun("run-1", protocol.RunTarget{Repository: identity, BaseSHA: head})
	job := h.enqueue("run-1", identity)

	inner := RunnerFunc(func(_ context.Context, attempt *PreparedAttempt) Outcome {
		if attempt.Claim.Attempt.AttemptNumber == 1 {
			// The zombie: it does its work and gets its branch onto the remote
			// — this is the push that lands microseconds before the lease dies
			// — and then the lease expires under it.
			if err := os.WriteFile(filepath.Join(attempt.WorktreePath, "zombie.txt"),
				[]byte("pushed before expiry\n"), 0o644); err != nil {
				t.Errorf("write zombie file: %v", err)
			}
			gitRun(t, attempt.WorktreePath, "add", "zombie.txt")
			gitRun(t, attempt.WorktreePath, "-c", "user.name=jig-test", "-c", "user.email=jig@test",
				"commit", "-m", "zombie work")
			gitRun(t, attempt.WorktreePath, "push", "origin",
				attempt.Branch+":refs/heads/"+attempt.Branch)
			expireLease(t, h, attempt.Claim.Attempt.ID)
			return Outcome{State: protocol.AttemptAcceptedUnpublished,
				Result: engineResult(t, []string{"zombie.txt"})}
		}
		if err := os.WriteFile(filepath.Join(attempt.WorktreePath, "successor.txt"),
			[]byte("clean work\n"), 0o644); err != nil {
			t.Errorf("write successor file: %v", err)
		}
		return Outcome{State: protocol.AttemptAcceptedUnpublished,
			Result: engineResult(t, []string{"successor.txt"})}
	})
	gateway := newFakeGateway()
	dataDir := filepath.Join(t.TempDir(), "worker")
	w, _ := newPublishingWorker(t, h, dataDir, inner, gateway)

	zombieAttempt, err := w.ClaimOnce(context.Background())
	if err == nil {
		t.Fatalf("the zombie attempt completed as %+v; an expired lease must record nothing", zombieAttempt)
	}
	if !strings.Contains(err.Error(), "lease_not_owner") {
		t.Fatalf("zombie attempt failed with %v, want a lease_not_owner rejection", err)
	}
	var zombieID string
	if err := h.db.QueryRow(`SELECT id FROM attempts WHERE job_id = ? AND attempt_number = 1`,
		job.ID).Scan(&zombieID); err != nil {
		t.Fatalf("read zombie attempt: %v", err)
	}
	zombieRecords, err := w.client.AttemptPublishRecords(context.Background(), zombieID)
	if err != nil {
		t.Fatalf("read zombie publish records: %v", err)
	}
	if len(zombieRecords) != 0 {
		t.Fatalf("the fenced-out zombie recorded %+v; nothing it did may be proven", zombieRecords)
	}
	// The fence stopped the pipeline at its first step: had the zombie been
	// authorized, it would have gone on to open a pull request for its branch.
	if created, _ := gateway.counts(); created != 0 {
		t.Fatalf("the zombie opened %d pull requests; the fence must refuse before any step", created)
	}

	// The sweeper's verdict, applied directly: the attempt is lost and the job
	// failed, which is the state an operator retry starts from (R5, KTD5).
	now := time.Now().UnixMilli()
	if _, err := h.db.Exec(`UPDATE attempts SET state = 'lost', completed_at = ? WHERE id = ?`,
		now, zombieID); err != nil {
		t.Fatalf("sweep zombie: %v", err)
	}
	if _, err := h.db.Exec(`UPDATE jobs SET state = 'failed', updated_at = ? WHERE id = ?`,
		now, job.ID); err != nil {
		t.Fatalf("fail job: %v", err)
	}
	if _, err := h.store.RetryJob(context.Background(), job.ID); err != nil {
		t.Fatalf("retry job: %v", err)
	}

	successor, err := w.ClaimOnce(context.Background())
	if err != nil {
		t.Fatalf("successor claim: %v", err)
	}
	if successor == nil || successor.State != protocol.AttemptAccepted {
		t.Fatalf("successor attempt = %+v, want accepted", successor)
	}
	zombieBranch := protocol.PublishBranch(job.ID, 1)
	successorBranch := protocol.PublishBranch(job.ID, 2)
	branches := remoteBranches(t, originDir)
	if _, exists := branches[successorBranch]; !exists {
		t.Fatalf("remote holds %v, want the successor's own branch %s", branches, successorBranch)
	}
	if _, exists := branches[zombieBranch]; !exists {
		t.Fatalf("remote lost the zombie's branch %s; the scenario needs it to still be there", zombieBranch)
	}

	if created, _ := gateway.counts(); created != 1 {
		t.Fatalf("gateway created %d pull requests, want only the successor's", created)
	}

	report, err := w.ReconcileIncludingPublish(context.Background())
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(report.StrayBranches) != 1 {
		t.Fatalf("reconcile reported stray branches %+v, want exactly the zombie's %s",
			report.StrayBranches, zombieBranch)
	}
	stray := report.StrayBranches[0]
	if stray.Branch != zombieBranch || stray.JobID != job.ID || stray.AttemptNumber != 1 {
		t.Fatalf("stray branch = %+v, want %s of job %s attempt 1", stray, zombieBranch, job.ID)
	}
	if stray.RemoteRef == "" || stray.Reason == "" {
		t.Fatalf("stray branch = %+v, want the remote ref and a reason an operator can act on", stray)
	}
}

// A remote that no longer exists is skipped, not reported as an incomplete
// scan. The scan's input is attempt manifests, which outlive the repository
// they name, so without this a retired remote makes every worker start report
// the same permanent failure — and the strays an operator can still act on
// drown in it.
func TestAScanSkipsARepositoryWhoseRemoteHasBeenRetired(t *testing.T) {
	h := newHarness(t)
	originDir, head, identity := newOriginRepo(t)
	h.seedRun("run-1", protocol.RunTarget{Repository: identity, BaseSHA: head})
	h.enqueue("run-1", identity)

	gateway := newFakeGateway()
	dataDir := filepath.Join(t.TempDir(), "worker")
	w, _ := newPublishingWorker(t, h, dataDir,
		writeAndDeclare(t, map[string]string{"work.txt": "published work\n"}), gateway)

	attempt, err := w.ClaimOnce(context.Background())
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if attempt == nil || attempt.State != protocol.AttemptAccepted {
		t.Fatalf("attempt = %+v, want accepted", attempt)
	}

	// The remote retires under the worker; the manifest naming it stays behind.
	if err := os.RemoveAll(originDir); err != nil {
		t.Fatalf("retire the remote: %v", err)
	}

	stray, scanErr := w.StrayPublishBranches(context.Background())
	if scanErr != nil {
		t.Fatalf("scan reported %v; a retired remote is a skip, not an incomplete scan", scanErr)
	}
	if len(stray) != 0 {
		t.Fatalf("scan reported strays %+v for a retired remote, want none", stray)
	}
	report, err := w.ReconcileIncludingPublish(context.Background())
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(report.StrayBranches) != 0 {
		t.Fatalf("reconcile reported strays %+v, want none", report.StrayBranches)
	}
}

// The classifier must stay narrow: anything that means "ask again later"
// keeps its place in the scan errors, because silently skipping a live
// repository is how a real stray branch goes unreported forever.
func TestOnlyAGoneRemoteCountsAsRetired(t *testing.T) {
	cases := []struct {
		name    string
		detail  string
		retired bool
	}{
		{"github deleted the repository", "remote: Repository not found.", true},
		{"the remote path is gone", "fatal: '/tmp/o.git' does not appear to be a git repository", true},
		{"dns failure", "fatal: unable to access: Could not resolve host: github.com", false},
		{"refused credential prompt", "fatal: could not read Username: terminal prompts disabled", false},
		{"permission denied", "ERROR: Permission to owner/repo denied to jig", false},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			err := fmt.Errorf("git ls-remote --heads origin: exit status 128: %s", testCase.detail)
			if got := remoteRetired(err); got != testCase.retired {
				t.Fatalf("remoteRetired(%q) = %v, want %v", testCase.detail, got, testCase.retired)
			}
		})
	}
	if remoteRetired(nil) {
		t.Fatal("remoteRetired(nil) = true, want false")
	}
	// A file:// remote retires by vanishing, which arrives typed.
	gone := fmt.Errorf("repository cache entry unavailable: %w",
		&fs.PathError{Op: "lstat", Path: "/tmp/o.git", Err: fs.ErrNotExist})
	if !remoteRetired(gone) {
		t.Fatalf("remoteRetired(%v) = false, want true for a vanished local remote", gone)
	}
	// But a missing git binary is not a missing repository.
	if remoteRetired(&exec.Error{Name: "git", Err: exec.ErrNotFound}) {
		t.Fatal("a missing git binary was classified as a retired remote")
	}
}

// ---- scenarios 2 and 3 -----------------------------------------------------

// Pull-request creation failing after the push yields accepted_unpublished
// with the push proven; the publish-only retry re-enters at the failed step
// and completes without re-running any phase, and without creating a duplicate
// branch or a duplicate pull request (R12, R14).
func TestPullRequestFailureLeavesAcceptedUnpublishedAndThePublishOnlyRetryFinishesIt(t *testing.T) {
	h := newHarness(t)
	originDir, head, identity := newOriginRepo(t)
	h.seedRun("run-1", protocol.RunTarget{Repository: identity, BaseSHA: head})
	job := h.enqueue("run-1", identity)

	var phaseRuns int
	inner := RunnerFunc(func(ctx context.Context, attempt *PreparedAttempt) Outcome {
		phaseRuns++
		return writeAndDeclare(t, map[string]string{"work.txt": "retried work\n"}).Run(ctx, attempt)
	})
	gateway := newFakeGateway()
	gateway.failCreate = publishFailure("gh_failed", "gh pr create failed: simulated outage")
	dataDir := filepath.Join(t.TempDir(), "worker")
	w, _ := newPublishingWorker(t, h, dataDir, inner, gateway)

	attempt, err := w.ClaimOnce(context.Background())
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if attempt == nil || attempt.State != protocol.AttemptAcceptedUnpublished {
		t.Fatalf("attempt = %+v, want accepted_unpublished", attempt)
	}
	summary := publishSummaryOf(t, attempt.Result)
	if summary.Published() || summary.Code != "gh_failed" {
		t.Fatalf("publish summary = %+v, want a failed pull_request step with its diagnostic", summary)
	}
	if len(summary.Performed) != 1 || summary.Performed[0] != protocol.PublishStepPush {
		t.Fatalf("performed steps = %v, want the push alone", summary.Performed)
	}
	branch := protocol.PublishBranch(job.ID, 1)
	pushedSHA := remoteBranches(t, originDir)[branch]
	if pushedSHA == "" {
		t.Fatalf("the push step succeeded but the remote has no %s", branch)
	}
	jobAfter, err := h.store.Job(context.Background(), job.ID)
	if err != nil {
		t.Fatalf("read job: %v", err)
	}
	if jobAfter.State != protocol.JobAcceptedUnpublished {
		t.Fatalf("job state = %q, want accepted_unpublished", jobAfter.State)
	}

	// The publish-only retry: the outage is over, nothing else changed.
	gateway.failCreate = nil
	retried, err := w.RetryPublish(context.Background(), job.ID, gateway, PublishOptions{
		CommitAuthorName: "jig-test", CommitAuthorEmail: "jig@test",
	})
	if err != nil {
		t.Fatalf("publish retry: %v", err)
	}
	if retried.ID != attempt.ID {
		t.Fatalf("retry produced attempt %s, want the same attempt %s", retried.ID, attempt.ID)
	}
	if retried.State != protocol.AttemptAccepted {
		t.Fatalf("retried attempt state = %q, want accepted", retried.State)
	}
	if phaseRuns != 1 {
		t.Fatalf("the runner executed %d times; a publish-only retry re-runs no phase", phaseRuns)
	}
	retrySummary := publishSummaryOf(t, retried.Result)
	if !retrySummary.Published() {
		t.Fatalf("retry summary = %+v, want published", retrySummary)
	}
	if len(retrySummary.Reused) != 1 || retrySummary.Reused[0] != protocol.PublishStepPush {
		t.Fatalf("retry reused %v, want the already-proven push — re-pushing is the duplicate this avoids",
			retrySummary.Reused)
	}
	branches := remoteBranches(t, originDir)
	if len(branches) != 1 || branches[branch] != pushedSHA {
		t.Fatalf("remote holds %v, want exactly %s still at %s", branches, branch, pushedSHA)
	}
	created, adopted := gateway.counts()
	if created != 1 {
		t.Fatalf("gateway created %d pull requests across the failure and the retry, want 1", created)
	}
	if adopted != 0 {
		t.Fatalf("gateway adopted %d existing pull requests, want 0 — the first create failed outright", adopted)
	}
	records, err := w.client.AttemptPublishRecords(context.Background(), attempt.ID)
	if err != nil {
		t.Fatalf("read publish records: %v", err)
	}
	if len(records) != 3 {
		t.Fatalf("publish records = %+v, want the three proven steps", records)
	}
	jobAfter, err = h.store.Job(context.Background(), job.ID)
	if err != nil {
		t.Fatalf("read job: %v", err)
	}
	if jobAfter.State != protocol.JobAccepted {
		t.Fatalf("job state = %q after a completed publish retry, want accepted", jobAfter.State)
	}
}

// Re-entering publish after the push has already been recorded must not push
// again and must not open a second pull request — even when the pull request
// this time is found rather than created. This is the same idempotency from
// the other direction: the ledger, not the remote, is what publish re-enters
// on.
func TestRepublishingAProvenPushCreatesNoDuplicateBranchOrPullRequest(t *testing.T) {
	h := newHarness(t)
	originDir, head, identity := newOriginRepo(t)
	h.seedRun("run-1", protocol.RunTarget{Repository: identity, BaseSHA: head})
	job := h.enqueue("run-1", identity)

	gateway := newFakeGateway()
	dataDir := filepath.Join(t.TempDir(), "worker")
	w, _ := newPublishingWorker(t, h, dataDir,
		writeAndDeclare(t, map[string]string{"work.txt": "work\n"}), gateway)

	attempt, err := w.ClaimOnce(context.Background())
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if attempt == nil || attempt.State != protocol.AttemptAccepted {
		t.Fatalf("attempt = %+v, want accepted", attempt)
	}
	branch := protocol.PublishBranch(job.ID, 1)
	before := remoteBranches(t, originDir)

	// A second publish of the same attempt: every step is already proven, so
	// every step is adopted from the ledger and nothing is repeated.
	target := publishTarget{
		attemptID:     attempt.ID,
		jobID:         job.ID,
		attemptNumber: 1,
		repository:    identity,
		branch:        branch,
		baseSHA:       head,
		lease:         newAttemptLease(w.client, attempt.ID, "unused-token-for-a-ledger-that-answers-first"),
	}
	summary := w.publish(context.Background(), gateway, PublishOptions{}, target, []string{"work.txt"}, ciPolicy{})
	if !summary.Published() {
		t.Fatalf("re-publish summary = %+v, want published from the ledger alone", summary)
	}
	if len(summary.Performed) != 0 {
		t.Fatalf("re-publish performed %v, want nothing repeated", summary.Performed)
	}
	if len(summary.Reused) != 3 {
		t.Fatalf("re-publish reused %v, want all three proven steps", summary.Reused)
	}
	after := remoteBranches(t, originDir)
	if len(after) != len(before) || after[branch] != before[branch] {
		t.Fatalf("remote branches changed from %v to %v across a re-publish", before, after)
	}
	if created, _ := gateway.counts(); created != 1 {
		t.Fatalf("gateway created %d pull requests, want 1", created)
	}
}

// ---- scenario 4 ------------------------------------------------------------

// Proof of publish checks the REMOTE ref. A local ref or reflog entry —
// which any local operation can manufacture — proves nothing and does not
// unblock cleanup: the worktree survives until the origin itself says the
// commit is there (R14, R16).
func TestProofChecksTheRemoteRefAndALocalReflogEntryDoesNotUnblockCleanup(t *testing.T) {
	h := newHarness(t)
	_, head, identity := newOriginRepo(t)
	h.seedRun("run-1", protocol.RunTarget{Repository: identity, BaseSHA: head})
	h.enqueue("run-1", identity)

	// The attempt commits work and fails, so nothing is published: exactly the
	// state where a local ref could be mistaken for proof.
	inner := RunnerFunc(func(_ context.Context, attempt *PreparedAttempt) Outcome {
		if err := os.WriteFile(filepath.Join(attempt.WorktreePath, "work.txt"),
			[]byte("local only\n"), 0o644); err != nil {
			t.Errorf("write work file: %v", err)
		}
		gitRun(t, attempt.WorktreePath, "add", "work.txt")
		gitRun(t, attempt.WorktreePath, "-c", "user.name=jig-test", "-c", "user.email=jig@test",
			"commit", "-m", "local work")
		return Outcome{State: protocol.AttemptFailed, Error: "phase failed after committing"}
	})
	dataDir := filepath.Join(t.TempDir(), "worker")
	w, _ := newPublishingWorker(t, h, dataDir, inner, newFakeGateway())

	attempt, err := w.ClaimOnce(context.Background())
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if attempt == nil || attempt.State != protocol.AttemptFailed {
		t.Fatalf("attempt = %+v, want failed", attempt)
	}
	manifest, err := w.manifests.load(attempt.ID)
	if err != nil {
		t.Fatalf("load manifest: %v", err)
	}
	worktreePath := manifest.WorktreePath
	localHead := gitRun(t, worktreePath, "rev-parse", "HEAD")

	// Every local marker of "this commit exists" that is not the remote: a
	// local branch, and the reflog entry that comes with it.
	gitRun(t, worktreePath, "update-ref", "refs/heads/looks-published", localHead)
	if reflog := gitRun(t, worktreePath, "reflog", "show", "--all"); reflog == "" {
		t.Fatal("the fixture produced no reflog entry to be fooled by")
	}

	target := publishTarget{
		attemptID: attempt.ID, jobID: attempt.JobID, attemptNumber: 1,
		repository: identity, branch: manifest.Branch,
		worktreePath: worktreePath, baseSHA: head,
	}
	err = w.verifyRemoteRef(context.Background(), target, manifest.Branch, localHead)
	if err == nil {
		t.Fatal("proof passed on local refs alone; publish would be provable without a remote")
	}
	if publishCode(err) != "publish_proof_missing" {
		t.Fatalf("proof failure code = %q, want publish_proof_missing (%v)", publishCode(err), err)
	}
	if eligible := w.cleanupEligible(context.Background(), manifest); eligible == nil {
		t.Fatal("cleanup was unblocked by a local ref; only remote-ref proof may delete work")
	}
	if _, err := os.Stat(worktreePath); err != nil {
		t.Fatalf("the unpublished worktree was deleted: %v", err)
	}

	// The remote now genuinely has it: the same checks flip, and only then.
	gitRun(t, worktreePath, "push", "origin", manifest.Branch+":refs/heads/"+manifest.Branch)
	if err := w.verifyRemoteRef(context.Background(), target, manifest.Branch, localHead); err != nil {
		t.Fatalf("proof failed against a branch the remote really holds: %v", err)
	}
	if eligible := w.cleanupEligible(context.Background(), manifest); eligible != nil {
		t.Fatalf("cleanup stayed blocked after remote-ref proof: %v", eligible)
	}

	// And a remote ref pointing somewhere else is not proof of THIS commit.
	if err := w.verifyRemoteRef(context.Background(), target, manifest.Branch, head); err == nil ||
		publishCode(err) != "publish_proof_mismatch" {
		t.Fatalf("proof against the wrong commit: err=%v, want publish_proof_mismatch", err)
	}
}

// ---- cancellation ----------------------------------------------------------

// Publish is a critical section: cancellation is a no-op inside it, because
// abandoning a half-published attempt — a branch with no pull request, a pull
// request with no proof — costs more than finishing. The attempt is run here
// on a context that is already cancelled, so any consultation of it at all
// would stop the pipeline before its first step.
func TestCancellationIsANoOpInsidePublish(t *testing.T) {
	h := newHarness(t)
	originDir, head, identity := newOriginRepo(t)
	h.seedRun("run-1", protocol.RunTarget{Repository: identity, BaseSHA: head})
	job := h.enqueue("run-1", identity)

	gateway := newFakeGateway()
	dataDir := filepath.Join(t.TempDir(), "worker")
	w, runner := newPublishingWorker(t, h, dataDir,
		writeAndDeclare(t, map[string]string{"work.txt": "work\n"}), gateway)

	token, err := mintLeaseToken()
	if err != nil {
		t.Fatalf("mint lease token: %v", err)
	}
	claim, err := w.client.Claim(context.Background(), protocol.ClaimRequest{
		RequestID: "cancellation-scenario", LeaseToken: token,
	})
	if err != nil || claim == nil {
		t.Fatalf("claim: claim=%v err=%v", claim, err)
	}
	lease := newAttemptLease(w.client, claim.Attempt.ID, token)
	prepared, err := w.prepareAttempt(context.Background(), claim, lease)
	if err != nil {
		t.Fatalf("prepare attempt: %v", err)
	}
	if _, err := w.client.StartAttempt(context.Background(), claim.Attempt.ID,
		protocol.StartAttemptRequest{LeaseToken: token}); err != nil {
		t.Fatalf("start attempt: %v", err)
	}

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	outcome := runner.Run(cancelled, prepared)
	if outcome.State != protocol.AttemptAccepted {
		t.Fatalf("outcome = %+v, want accepted despite the cancelled context", outcome)
	}
	summary := publishSummaryOf(t, outcome.Result)
	if len(summary.Performed) != 3 {
		t.Fatalf("performed steps = %v, want all three under cancellation", summary.Performed)
	}
	branch := protocol.PublishBranch(job.ID, 1)
	if remoteBranches(t, originDir)[branch] == "" {
		t.Fatalf("the branch never reached the remote under cancellation")
	}
}

// ---- milestone 2 exit gate -------------------------------------------------

// TestMilestone2ExitGate is the plan's Milestone 2 exit gate: an accepted run
// through the worker path publishing a real branch and a real pull request on
// a real GitHub repository, with cleanup deleting the worktree only after
// remote proof.
//
// It is skipped unless JIG_M2_EXIT_GATE names a scratch repository
// (owner/repo) — a test that opens pull requests is not something `go test
// ./...` may do on its own. Everything in it is real except the phase engine:
// the inner runner stands in for the agent chain (whose own end-to-end proof
// is U11's harness), so what this gate exercises is the publish pipeline —
// clone, worktree, staged commit, push, `gh` pull request, remote-ref proof,
// fenced ledger, and fail-closed cleanup.
func TestMilestone2ExitGate(t *testing.T) {
	project := strings.TrimSpace(os.Getenv("JIG_M2_EXIT_GATE"))
	if project == "" {
		t.Skip("set JIG_M2_EXIT_GATE=owner/repo to run the milestone 2 exit gate")
	}
	ctx := context.Background()
	identity := "github.com/" + project
	cloneURL := "https://github.com/" + project + ".git"

	stdout, err := runGit(ctx, "", "ls-remote", cloneURL, "HEAD")
	if err != nil {
		t.Fatalf("read %s HEAD: %v", cloneURL, err)
	}
	baseSHA, _, found := strings.Cut(strings.TrimSpace(stdout), "\t")
	if !found || len(baseSHA) < 40 {
		t.Fatalf("unreadable HEAD for %s: %q", project, stdout)
	}

	h := newHarness(t)
	h.seedRun("run-gate", protocol.RunTarget{Repository: identity, BaseSHA: baseSHA})
	job := h.enqueue("run-gate", identity)

	stamp := time.Now().UTC().Format("20060102-150405")
	dataDir := filepath.Join(t.TempDir(), "worker")
	w, _ := newPublishingWorker(t, h, dataDir, writeAndDeclare(t, map[string]string{
		"jig-exit-gate.md": "# jig milestone 2 exit gate\n\nPublished by the U7 pipeline at " + stamp + ".\n",
	}), NewGitHubCLIGateway())

	attempt, err := w.ClaimOnce(ctx)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if attempt == nil || attempt.State != protocol.AttemptAccepted {
		t.Fatalf("attempt = %+v, want accepted", attempt)
	}
	summary := publishSummaryOf(t, attempt.Result)
	if !summary.Published() {
		t.Fatalf("publish summary = %+v, want published", summary)
	}
	branch := protocol.PublishBranch(job.ID, 1)
	t.Logf("exit gate: branch=%s head=%s pull request=%s", branch, summary.RemoteRef, summary.PullRequestURL)

	// The branch is really on the real remote, at the published commit.
	stdout, err = runGit(ctx, "", "ls-remote", cloneURL, "refs/heads/"+branch)
	if err != nil {
		t.Fatalf("read published branch: %v", err)
	}
	remoteSHA, _, found := strings.Cut(strings.TrimSpace(stdout), "\t")
	if !found || remoteSHA != summary.RemoteRef {
		t.Fatalf("remote %s = %q, want the published %s", branch, stdout, summary.RemoteRef)
	}

	// The pull request really exists, and there is exactly one for this head.
	listed, _, _, _, err := runBoundedCommand(ctx, "gh", "pr", "list", "--repo", project,
		"--head", branch, "--state", "all", "--json", "url,state,headRefName")
	if err != nil {
		t.Fatalf("gh pr list: %v", err)
	}
	var pullRequests []struct {
		URL         string `json:"url"`
		State       string `json:"state"`
		HeadRefName string `json:"headRefName"`
	}
	if err := json.Unmarshal(listed, &pullRequests); err != nil {
		t.Fatalf("decode gh pr list: %v", err)
	}
	if len(pullRequests) != 1 || pullRequests[0].URL != summary.PullRequestURL {
		t.Fatalf("gh reports %+v, want exactly the recorded pull request %s",
			pullRequests, summary.PullRequestURL)
	}

	records, err := w.client.AttemptPublishRecords(ctx, attempt.ID)
	if err != nil {
		t.Fatalf("read publish records: %v", err)
	}
	if len(records) != 3 {
		t.Fatalf("publish records = %+v, want the three proven steps", records)
	}
	if remaining := listDirNames(t, filepath.Join(dataDir, "worktrees")); len(remaining) != 0 {
		t.Fatalf("the published worktree survived cleanup: %v", remaining)
	}
	jobAfter, err := h.store.Job(ctx, job.ID)
	if err != nil {
		t.Fatalf("read job: %v", err)
	}
	if jobAfter.State != protocol.JobAccepted {
		t.Fatalf("job state = %q, want accepted", jobAfter.State)
	}
}

func TestAPublishHoldEndsAcceptedUnpublishedWithoutTouchingTheRemote(t *testing.T) {
	gateway := newFakeGateway()
	inner := RunnerFunc(func(context.Context, *PreparedAttempt) Outcome {
		return Outcome{State: protocol.AttemptAcceptedUnpublished,
			Result: engineResult(t, []string{"src/auth.txt"}), PublishHold: "risk == high held"}
	})
	// Deliberately unbound: a hold must never reach the publish pipeline, so
	// an unbound runner is the proof — bound or not, nothing is pushed.
	runner := NewPublishingRunner(inner, gateway, PublishOptions{})
	outcome := runner.Run(context.Background(), &PreparedAttempt{})
	if outcome.State != protocol.AttemptAcceptedUnpublished || outcome.Error != "" {
		t.Fatalf("outcome = %+v, want accepted_unpublished with no error", outcome)
	}
	var document struct {
		Publish PublishSummary `json:"publish"`
	}
	if err := json.Unmarshal([]byte(outcome.Result), &document); err != nil {
		t.Fatal(err)
	}
	if document.Publish.State != PublishStateHeld || document.Publish.Code != "publish_held" ||
		document.Publish.Detail != "risk == high held" {
		t.Fatalf("publish summary = %+v, want held with the reason", document.Publish)
	}
	if created, adopted := gateway.counts(); created+adopted != 0 {
		t.Fatal("a held publish must not create or adopt a pull request")
	}
}
