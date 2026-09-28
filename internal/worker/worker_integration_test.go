// worker_integration_test.go — the U3 plan scenarios, verbatim, against a
// real control-plane store behind a real HTTP server and real git
// repositories in t.TempDir(). The test package is `worker`, so it may
// import controlplane for the server side: `go list -deps` (the boundary
// check) never sees test imports, and the shipped worker package still
// speaks only HTTP.
package worker

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/StructuPath/jig/internal/controlplane"
	"github.com/StructuPath/jig/internal/protocol"
	_ "modernc.org/sqlite"
)

// integrationSnapshot is a minimal frozen definition with no required env
// names, so worker eligibility never depends on the test host environment.
const integrationSnapshot = `name: fixture
roster:
  builder:
    model: claude-sonnet
    system_prompt: build
    user_prompt: build it
phases:
  - name: build
    kind: agent
    owner: builder
`

type harness struct {
	t      *testing.T
	store  *controlplane.Store
	server *httptest.Server
	db     *sql.DB
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "jig.db")
	store, err := controlplane.Open(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	server := httptest.NewServer(controlplane.NewHandler(store, "", logger))
	t.Cleanup(server.Close)
	// A second connection onto the same database seeds definitions and runs
	// directly: the definitions/runs API is U5, so U3 tests own the fixtures
	// exactly as the U2 suite does.
	fileURL := url.URL{Scheme: "file", Path: dbPath}
	db, err := sql.Open("sqlite",
		fileURL.String()+"?_pragma=busy_timeout%285000%29&_pragma=foreign_keys%281%29&_pragma=journal_mode%28WAL%29&_txlock=immediate")
	if err != nil {
		t.Fatalf("open seeding connection: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return &harness{t: t, store: store, server: server, db: db}
}

func (h *harness) seedRun(runID string, targets ...protocol.RunTarget) {
	h.t.Helper()
	h.seedRunWithSnapshot(runID, integrationSnapshot, targets...)
}

// seedRunWithSnapshot seeds a run frozen from the given definition source.
func (h *harness) seedRunWithSnapshot(runID, snapshot string, targets ...protocol.RunTarget) {
	h.t.Helper()
	targetsJSON, err := json.Marshal(targets)
	if err != nil {
		h.t.Fatalf("encode targets: %v", err)
	}
	now := time.Now().UnixMilli()
	if _, err := h.db.Exec(`
		INSERT INTO definitions(id, name, generation, source, created_at, updated_at)
		VALUES (?, ?, 1, ?, ?, ?)
	`, "def-"+runID, "fixture-"+runID, snapshot, now, now); err != nil {
		h.t.Fatalf("seed definition: %v", err)
	}
	if _, err := h.db.Exec(`
		INSERT INTO runs(id, definition_id, definition_generation, snapshot, parameters, targets, state, created_at, updated_at)
		VALUES (?, ?, 1, ?, '{}', ?, 'active', ?, ?)
	`, runID, "def-"+runID, snapshot, string(targetsJSON), now, now); err != nil {
		h.t.Fatalf("seed run: %v", err)
	}
}

func (h *harness) enqueue(runID, repository string) protocol.Job {
	h.t.Helper()
	job, err := h.store.EnqueueJob(context.Background(), runID, repository)
	if err != nil {
		h.t.Fatalf("enqueue job for %s: %v", repository, err)
	}
	return job
}

// releaseWorktree drives the operator release action over plain HTTP, the
// way an operator surface would.
func (h *harness) releaseWorktree(attemptID string, body string) (int, string) {
	h.t.Helper()
	response, err := http.Post(
		h.server.URL+"/api/worktrees/"+url.PathEscape(attemptID)+"/release",
		"application/json", bytes.NewReader([]byte(body)))
	if err != nil {
		h.t.Fatalf("release request: %v", err)
	}
	defer response.Body.Close()
	payload, _ := io.ReadAll(response.Body)
	return response.StatusCode, string(payload)
}

func newTestWorker(t *testing.T, h *harness, dataDir string, capacity int, runner AttemptRunner) *Worker {
	t.Helper()
	w, err := New(Config{
		ServerURL: h.server.URL,
		DataDir:   dataDir,
		Capacity:  capacity,
		Environ:   []string{"PATH=" + os.Getenv("PATH"), "GITHUB_TOKEN=test-value"},
		Probes:    []RuntimeProbe{}, // no CLI probing in tests
		Runner:    runner,
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("build worker: %v", err)
	}
	if _, err := w.RegisterOnce(context.Background()); err != nil {
		t.Fatalf("register worker: %v", err)
	}
	return w
}

// ---- git fixtures ----------------------------------------------------------

func gitRun(t *testing.T, dir string, arguments ...string) string {
	t.Helper()
	stdout, err := runGit(context.Background(), dir, arguments...)
	if err != nil {
		t.Fatalf("git %s: %v", strings.Join(arguments, " "), err)
	}
	return strings.TrimSpace(stdout)
}

// newOriginRepo builds the local bare "remote" fixture with one commit on
// main and returns its directory, its head SHA, and its normalized identity
// (the form jobs carry).
func newOriginRepo(t *testing.T) (originDir, headSHA, identity string) {
	t.Helper()
	root := t.TempDir()
	originDir = filepath.Join(root, "origin.git")
	gitRun(t, "", "init", "--bare", "--initial-branch=main", originDir)
	seedDir := filepath.Join(root, "seed")
	gitRun(t, "", "init", "--initial-branch=main", seedDir)
	if err := os.WriteFile(filepath.Join(seedDir, "README.md"), []byte("fixture\n"), 0o644); err != nil {
		t.Fatalf("write fixture file: %v", err)
	}
	gitRun(t, seedDir, "add", "README.md")
	gitRun(t, seedDir, "-c", "user.name=jig-test", "-c", "user.email=jig@test",
		"commit", "-m", "seed")
	gitRun(t, seedDir, "remote", "add", "origin", originDir)
	gitRun(t, seedDir, "push", "origin", "main")
	headSHA = gitRun(t, seedDir, "rev-parse", "HEAD")
	value, err := normalizeRepositoryIdentity(originDir)
	if err != nil {
		t.Fatalf("normalize origin identity: %v", err)
	}
	return originDir, headSHA, value
}

func listDirNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	var names []string
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), ".") {
			names = append(names, entry.Name())
		}
	}
	return names
}

// ---- scenario 1 ------------------------------------------------------------

func TestTwoConcurrentAttemptsOnOneRepositoryGetDisjointWorktreesFromOneCacheEntry(t *testing.T) {
	h := newHarness(t)
	_, head, identity := newOriginRepo(t)
	h.seedRun("run-1", protocol.RunTarget{Repository: identity, BaseSHA: head})
	h.seedRun("run-2", protocol.RunTarget{Repository: identity, BaseSHA: head})
	h.enqueue("run-1", identity)
	h.enqueue("run-2", identity)

	type observation struct {
		path, branch, head string
	}
	var mutex sync.Mutex
	var observed []observation
	var arrivals atomic.Int32
	bothInFlight := make(chan struct{})
	runner := RunnerFunc(func(ctx context.Context, attempt *PreparedAttempt) Outcome {
		if arrivals.Add(1) == 2 {
			close(bothInFlight)
		}
		select {
		case <-bothInFlight:
		case <-time.After(30 * time.Second):
			t.Error("the two attempts never overlapped")
		}
		head := gitRun(t, attempt.WorktreePath, "rev-parse", "HEAD")
		mutex.Lock()
		observed = append(observed, observation{attempt.WorktreePath, attempt.Branch, head})
		mutex.Unlock()
		return Outcome{State: protocol.AttemptFailed, Error: "scenario runner"}
	})
	dataDir := filepath.Join(t.TempDir(), "worker")
	w := newTestWorker(t, h, dataDir, 2, runner)

	var group sync.WaitGroup
	results := make([]*protocol.Attempt, 2)
	errors := make([]error, 2)
	for i := range results {
		group.Add(1)
		go func(i int) {
			defer group.Done()
			results[i], errors[i] = w.ClaimOnce(context.Background())
		}(i)
	}
	group.Wait()
	for i := range results {
		if errors[i] != nil {
			t.Fatalf("claim %d: %v", i, errors[i])
		}
		if results[i] == nil {
			t.Fatalf("claim %d returned no attempt with two jobs queued", i)
		}
	}
	if len(observed) != 2 {
		t.Fatalf("observed %d attempts in flight, want 2", len(observed))
	}
	if observed[0].path == observed[1].path {
		t.Fatalf("both attempts shared worktree %s", observed[0].path)
	}
	if observed[0].branch == observed[1].branch {
		t.Fatalf("both attempts shared branch %s", observed[0].branch)
	}
	for _, value := range observed {
		if value.head != head {
			t.Fatalf("worktree %s was at %s, want the pinned base %s", value.path, value.head, head)
		}
		if !strings.HasPrefix(value.branch, "jig/") || !strings.HasSuffix(value.branch, "/1") {
			t.Fatalf("branch %q is not attempt-scoped jig/<job-id>/<attempt-n>", value.branch)
		}
	}
	cacheEntries := listDirNames(t, filepath.Join(dataDir, "repos"))
	if len(cacheEntries) != 1 {
		t.Fatalf("repository cache holds %d entries (%v), want 1 shared entry", len(cacheEntries), cacheEntries)
	}
	// Both worktrees were clean at the pinned base: disposal proves that and
	// deletes them, so nothing is retained.
	if remaining := listDirNames(t, filepath.Join(dataDir, "worktrees")); len(remaining) != 0 {
		t.Fatalf("clean-at-base worktrees survived disposal: %v", remaining)
	}
}

// ---- scenario 2 ------------------------------------------------------------

func TestAWorktreeWithUnpublishedCommitsSurvivesCleanupAndRequiresConfirmedRelease(t *testing.T) {
	h := newHarness(t)
	_, head, identity := newOriginRepo(t)
	h.seedRun("run-1", protocol.RunTarget{Repository: identity, BaseSHA: head})
	h.enqueue("run-1", identity)

	var worktreePath string
	runner := RunnerFunc(func(ctx context.Context, attempt *PreparedAttempt) Outcome {
		worktreePath = attempt.WorktreePath
		if err := os.WriteFile(filepath.Join(attempt.WorktreePath, "work.txt"), []byte("unpublished\n"), 0o644); err != nil {
			t.Errorf("write work file: %v", err)
		}
		gitRun(t, attempt.WorktreePath, "add", "work.txt")
		gitRun(t, attempt.WorktreePath, "-c", "user.name=jig-test", "-c", "user.email=jig@test",
			"commit", "-m", "unpublished work")
		return Outcome{State: protocol.AttemptFailed, Error: "left unpublished work"}
	})
	dataDir := filepath.Join(t.TempDir(), "worker")
	w := newTestWorker(t, h, dataDir, 1, runner)

	attempt, err := w.ClaimOnce(context.Background())
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if attempt == nil || attempt.State != protocol.AttemptFailed {
		t.Fatalf("attempt = %+v, want a failed attempt", attempt)
	}
	if _, err := os.Stat(worktreePath); err != nil {
		t.Fatalf("worktree with unpublished commits did not survive cleanup: %v", err)
	}
	ledger, err := w.client.Worktrees(context.Background())
	if err != nil {
		t.Fatalf("read ledger: %v", err)
	}
	if len(ledger) != 1 {
		t.Fatalf("ledger holds %d rows, want the retained worktree", len(ledger))
	}
	entry := ledger[0]
	if entry.AttemptID != attempt.ID || entry.State != protocol.WorktreeRetained ||
		!strings.Contains(entry.Reason, "unpublished") {
		t.Fatalf("ledger row = %+v, want retained %s with an unpublished-commits reason", entry, attempt.ID)
	}

	// The release action requires the explicit confirmation flag.
	if status, body := h.releaseWorktree(attempt.ID, `{}`); status != http.StatusBadRequest ||
		!strings.Contains(body, "release_requires_confirmation") {
		t.Fatalf("release without confirm answered %d %s, want 400 release_requires_confirmation", status, body)
	}
	if status, body := h.releaseWorktree(attempt.ID, `{"confirm": false}`); status != http.StatusBadRequest {
		t.Fatalf("release with confirm=false answered %d %s, want 400", status, body)
	}
	if _, err := os.Stat(worktreePath); err != nil {
		t.Fatalf("refused release deleted the worktree: %v", err)
	}
	status, body := h.releaseWorktree(attempt.ID, `{"confirm": true}`)
	if status != http.StatusOK || !strings.Contains(body, `"state":"released"`) {
		t.Fatalf("confirmed release answered %d %s, want a released row", status, body)
	}
	// The worker applies the release at its next reconciliation.
	report, err := w.Reconcile(context.Background())
	if err != nil {
		t.Fatalf("reconcile after release: %v", err)
	}
	if len(report.ReleasedAttemptIDs) != 1 || report.ReleasedAttemptIDs[0] != attempt.ID {
		t.Fatalf("reconcile released %v, want [%s]", report.ReleasedAttemptIDs, attempt.ID)
	}
	if _, err := os.Stat(worktreePath); !os.IsNotExist(err) {
		t.Fatalf("released worktree still on disk (err=%v)", err)
	}
}

// ---- scenario 3 ------------------------------------------------------------

func TestReconcileReportsOrphansUndeletedAndMarksDisklessLedgerRowsLost(t *testing.T) {
	h := newHarness(t)
	_, head, identity := newOriginRepo(t)
	h.seedRun("run-1", protocol.RunTarget{Repository: identity, BaseSHA: head})
	h.enqueue("run-1", identity)

	var worktreePath string
	runner := RunnerFunc(func(ctx context.Context, attempt *PreparedAttempt) Outcome {
		worktreePath = attempt.WorktreePath
		if err := os.WriteFile(filepath.Join(attempt.WorktreePath, "work.txt"), []byte("unpublished\n"), 0o644); err != nil {
			t.Errorf("write work file: %v", err)
		}
		gitRun(t, attempt.WorktreePath, "add", "work.txt")
		gitRun(t, attempt.WorktreePath, "-c", "user.name=jig-test", "-c", "user.email=jig@test",
			"commit", "-m", "unpublished work")
		return Outcome{State: protocol.AttemptFailed, Error: "left unpublished work"}
	})
	dataDir := filepath.Join(t.TempDir(), "worker")
	first := newTestWorker(t, h, dataDir, 1, runner)
	attempt, err := first.ClaimOnce(context.Background())
	if err != nil || attempt == nil {
		t.Fatalf("claim: attempt=%v err=%v", attempt, err)
	}

	// An orphan appears on disk with no manifest; the retained worktree's
	// disk copy vanishes outside the worker's control. The path uses the
	// worker's canonical root, as manifests do.
	orphanPath := filepath.Join(first.worktreeRoot(), "orphan-worktree")
	if err := os.MkdirAll(orphanPath, 0o700); err != nil {
		t.Fatalf("create orphan: %v", err)
	}
	if err := os.RemoveAll(worktreePath); err != nil {
		t.Fatalf("remove retained worktree: %v", err)
	}

	// Worker restart: a fresh instance over the same data directory keeps
	// the same identity and reconciles disk against the ledger.
	restarted := newTestWorker(t, h, dataDir, 1, runner)
	if restarted.ID() != first.ID() {
		t.Fatalf("restart changed the worker identity: %s -> %s", first.ID(), restarted.ID())
	}
	report, err := restarted.Reconcile(context.Background())
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(report.OrphanPaths) != 1 || report.OrphanPaths[0] != orphanPath {
		t.Fatalf("reconcile reported orphans %v, want [%s]", report.OrphanPaths, orphanPath)
	}
	if _, err := os.Stat(orphanPath); err != nil {
		t.Fatalf("reconcile deleted the orphan it should only report: %v", err)
	}
	if len(report.MissingAttemptIDs) != 1 || report.MissingAttemptIDs[0] != attempt.ID {
		t.Fatalf("reconcile reported missing %v, want [%s]", report.MissingAttemptIDs, attempt.ID)
	}
	var ledgerState string
	for _, entry := range report.Ledger {
		if entry.AttemptID == attempt.ID {
			ledgerState = entry.State
		}
	}
	if ledgerState != protocol.WorktreeLost {
		t.Fatalf("ledger row for %s is %q, want lost", attempt.ID, ledgerState)
	}
}

// ---- scenario 4 ------------------------------------------------------------

func TestARepoAtItsRetainedCapAdmitsNoNewAttemptWorktreesUntilRelease(t *testing.T) {
	h := newHarness(t)
	_, head, identity := newOriginRepo(t)
	h.seedRun("run-1", protocol.RunTarget{Repository: identity, BaseSHA: head})
	h.enqueue("run-1", identity)

	var ranAttempts atomic.Int32
	runner := RunnerFunc(func(ctx context.Context, attempt *PreparedAttempt) Outcome {
		ranAttempts.Add(1)
		if _, err := os.Stat(attempt.WorktreePath); err != nil {
			t.Errorf("attempt ran without a worktree: %v", err)
		}
		return Outcome{State: protocol.AttemptFailed, Error: "scenario runner"}
	})
	dataDir := filepath.Join(t.TempDir(), "worker")
	w := newTestWorker(t, h, dataDir, 1, runner)

	// The repository already holds MaxRetainedWorktreesPerRepo retained
	// worktrees from an older run's attempts (seeded directly; growing them
	// through real attempts is the U2-tested claim path's job).
	h.seedRun("run-old", protocol.RunTarget{Repository: identity, BaseSHA: head})
	now := time.Now().UnixMilli()
	if _, err := h.db.Exec(`
		INSERT INTO jobs(id, run_id, repository, base_sha, state, created_at, updated_at)
		VALUES ('job-old', 'run-old', ?, ?, 'failed', ?, ?)
	`, identity, head, now, now); err != nil {
		t.Fatalf("seed old job: %v", err)
	}
	for i := 1; i <= protocol.MaxRetainedWorktreesPerRepo; i++ {
		attemptID := fmt.Sprintf("old-attempt-%d", i)
		if _, err := h.db.Exec(`
			INSERT INTO attempts(id, job_id, worker_id, attempt_number, state, created_at)
			VALUES (?, 'job-old', ?, ?, 'failed', ?)
		`, attemptID, w.ID(), i, now); err != nil {
			t.Fatalf("seed old attempt %d: %v", i, err)
		}
		if _, err := h.db.Exec(`
			INSERT INTO retained_worktrees(attempt_id, worker_id, repository, path, reason, state, created_at, updated_at)
			VALUES (?, ?, ?, ?, 'unpublished commits', 'retained', ?, ?)
		`, attemptID, w.ID(), identity, "/tmp/wt-"+attemptID, now, now); err != nil {
			t.Fatalf("seed retained worktree %d: %v", i, err)
		}
	}

	attempt, err := w.ClaimOnce(context.Background())
	if err != nil {
		t.Fatalf("claim at cap: %v", err)
	}
	if attempt != nil {
		t.Fatalf("claim at cap returned attempt %s; the repo's jobs must be skipped", attempt.ID)
	}
	if ranAttempts.Load() != 0 {
		t.Fatal("an attempt ran while the repository was at its retained cap")
	}
	if worktrees := listDirNames(t, filepath.Join(dataDir, "worktrees")); len(worktrees) != 0 {
		t.Fatalf("worktrees %v were created while the repository was at its retained cap", worktrees)
	}

	if status, body := h.releaseWorktree("old-attempt-1", `{"confirm": true}`); status != http.StatusOK {
		t.Fatalf("release answered %d %s", status, body)
	}
	attempt, err = w.ClaimOnce(context.Background())
	if err != nil {
		t.Fatalf("claim after release: %v", err)
	}
	if attempt == nil {
		t.Fatal("claim after release returned no attempt")
	}
	if ranAttempts.Load() != 1 {
		t.Fatalf("ran %d attempts after release, want 1", ranAttempts.Load())
	}
}

// ---- scenario 5 ------------------------------------------------------------

func TestReRegistrationShrinksTheAdvertisedEnvNameSetWhenAVariableIsRemoved(t *testing.T) {
	h := newHarness(t)
	w, err := New(Config{
		ServerURL: h.server.URL,
		DataDir:   filepath.Join(t.TempDir(), "worker"),
		Capacity:  1,
		Environ:   []string{"ALPHA=1", "BRAVO=2", "PATH=" + os.Getenv("PATH")},
		Probes:    []RuntimeProbe{},
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("build worker: %v", err)
	}
	contains := func(names []string, name string) bool {
		for _, value := range names {
			if value == name {
				return true
			}
		}
		return false
	}
	registered, err := w.RegisterOnce(context.Background())
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if !contains(registered.EnvNames, "ALPHA") || !contains(registered.EnvNames, "BRAVO") {
		t.Fatalf("advertised names %v are missing ALPHA or BRAVO", registered.EnvNames)
	}
	for _, name := range registered.EnvNames {
		if strings.Contains(name, "=") {
			t.Fatalf("advertised name %q leaks a value", name)
		}
	}

	// The variable disappears from the composed environment; re-registration
	// shrinks the advertised set.
	w.SetEnviron([]string{"ALPHA=1", "PATH=" + os.Getenv("PATH")})
	reregistered, err := w.RegisterOnce(context.Background())
	if err != nil {
		t.Fatalf("re-register: %v", err)
	}
	if !contains(reregistered.EnvNames, "ALPHA") {
		t.Fatalf("advertised names %v lost ALPHA, which is still present", reregistered.EnvNames)
	}
	if contains(reregistered.EnvNames, "BRAVO") {
		t.Fatalf("advertised names %v still contain the removed BRAVO", reregistered.EnvNames)
	}
}

// recordingContinuation counts Release calls.
type recordingContinuation struct{ released int }

func (c *recordingContinuation) RepairCI(context.Context, CIFailure) Outcome {
	return Outcome{State: protocol.AttemptFailed, Error: "not used"}
}
func (c *recordingContinuation) Release() { c.released++ }

// A continuation the runner hands back unused is released by the attempt
// loop, so an engine kept alive for CI repair never pins its scratch past
// the attempt.
func TestTheAttemptLoopReleasesAnUnusedContinuation(t *testing.T) {
	h := newHarness(t)
	_, head, identity := newOriginRepo(t)
	h.seedRun("run-1", protocol.RunTarget{Repository: identity, BaseSHA: head})
	h.enqueue("run-1", identity)
	continuation := &recordingContinuation{}
	runner := RunnerFunc(func(context.Context, *PreparedAttempt) Outcome {
		return Outcome{State: protocol.AttemptFailed, Error: "done", Continuation: continuation}
	})
	w := newTestWorker(t, h, filepath.Join(t.TempDir(), "worker"), 1, runner)
	if attempt, err := w.ClaimOnce(context.Background()); err != nil || attempt == nil {
		t.Fatalf("claim once: attempt=%v err=%v", attempt, err)
	}
	if continuation.released != 1 {
		t.Fatalf("continuation released %d times, want exactly once", continuation.released)
	}
}

// DeferToContinuation moves a host's cleanup into the continuation's
// Release: nothing runs while the continuation lives, the inner Release
// runs first, and the cleanup runs exactly once however often Release is
// called. With no continuation it takes nothing.
func TestDeferToContinuationRunsTheHostCleanupOnceAfterRelease(t *testing.T) {
	if DeferToContinuation(&Outcome{State: protocol.AttemptFailed}, func() { t.Fatal("cleanup taken without a continuation") }) {
		t.Fatal("DeferToContinuation kept an outcome with no continuation")
	}
	inner := &recordingContinuation{}
	var order []string
	outcome := Outcome{State: protocol.AttemptAcceptedUnpublished, Continuation: inner}
	if !DeferToContinuation(&outcome, func() {
		order = append(order, fmt.Sprintf("cleanup after %d release(s)", inner.released))
	}) {
		t.Fatal("DeferToContinuation did not keep an outcome with a continuation")
	}
	if len(order) != 0 {
		t.Fatal("the host cleanup ran before Release")
	}
	outcome.Continuation.Release()
	outcome.Continuation.Release()
	if len(order) != 1 || order[0] != "cleanup after 1 release(s)" {
		t.Fatalf("cleanup runs = %v, want exactly one, after the inner Release", order)
	}
}

// A publishing runner that was never bound rebuilds the outcome, so it must
// release the continuation itself rather than drop it.
func TestAnUnboundPublisherReleasesTheContinuationItDrops(t *testing.T) {
	continuation := &recordingContinuation{}
	runner := NewPublishingRunner(RunnerFunc(func(context.Context, *PreparedAttempt) Outcome {
		return Outcome{State: protocol.AttemptAcceptedUnpublished, Continuation: continuation}
	}), newFakeGateway(), PublishOptions{})
	outcome := runner.Run(context.Background(), &PreparedAttempt{})
	if outcome.Continuation != nil || continuation.released != 1 {
		t.Fatalf("outcome continuation=%v released=%d, want dropped and released once",
			outcome.Continuation, continuation.released)
	}
}
