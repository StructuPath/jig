// embedded_test.go — the direct-run path's lifecycle guarantees (U11): an
// ungracefully killed run must never wedge the data directory, and two
// direct runs against one data directory must not destroy each other's work.
package controlplane

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/StructuPath/jig/internal/protocol"
)

// directSnapshot is a minimal valid definition with no required env names,
// so direct-run eligibility never depends on the test host's environment.
const directSnapshot = `name: direct-fixture
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

const directHeadSHA = "0123456789abcdef0123456789abcdef01234567"

func directCapability() protocol.RuntimeCapability {
	return protocol.RuntimeCapability{Name: "scripted", Version: "0.0.0", CanResume: true}
}

// acceptingExecutor is the injected engine seam: it declares the accepted
// outcome without running anything.
func acceptingExecutor(before func(DirectExecution)) func(context.Context, DirectExecution) DirectOutcome {
	return func(_ context.Context, execution DirectExecution) DirectOutcome {
		if before != nil {
			before(execution)
		}
		return DirectOutcome{
			State:  protocol.AttemptAcceptedUnpublished,
			Result: `{"publish":"not_attempted","changed_paths":[]}`,
		}
	}
}

func directConfig(dataDir, repoPath string, execute func(context.Context, DirectExecution) DirectOutcome) DirectRunConfig {
	return DirectRunConfig{
		DataDir:      dataDir,
		Source:       []byte(directSnapshot),
		RepoPath:     repoPath,
		HeadSHA:      directHeadSHA,
		Capability:   directCapability(),
		BaseEnvNames: []string{"PATH"},
		Execute:      execute,
	}
}

// abandonDirectRun reproduces what an ungracefully killed `jig run` leaves
// behind: a running attempt leased by the embedded worker, and a liveness
// marker file whose holder is gone (the kernel drops the flock when the
// process dies, however it dies).
func abandonDirectRun(t *testing.T, dataDir, repoPath string) (jobID, attemptID string) {
	t.Helper()
	ctx := context.Background()
	if err := os.MkdirAll(directMarkerDir(dataDir), 0o700); err != nil {
		t.Fatal(err)
	}
	store, err := Open(ctx, filepath.Join(dataDir, "jig.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := store.RegisterWorker(ctx, directWorkerID, protocol.WorkerRegistration{
		Name: "embedded direct runner", WorkerVersion: "jig-run",
		Capacity: directWorkerCapacity, EnvNames: []string{"PATH"},
		Runtimes: []protocol.RuntimeCapability{directCapability()},
	}); err != nil {
		t.Fatal(err)
	}
	definitionID, generation, err := store.upsertDefinition(ctx, "direct-fixture", directSnapshot)
	if err != nil {
		t.Fatal(err)
	}
	runID, err := store.insertFrozenRun(ctx, definitionID, generation, directSnapshot, nil,
		protocol.RunTarget{Repository: repoPath, BaseSHA: directHeadSHA})
	if err != nil {
		t.Fatal(err)
	}
	job, err := store.EnqueueJob(ctx, runID, repoPath)
	if err != nil {
		t.Fatal(err)
	}
	marker, err := acquireDirectMarker(dataDir, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	claim, token, err := store.claimDirectJob(ctx, dataDir, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.StartAttempt(ctx, claim.Attempt.ID, protocol.StartAttemptRequest{
		LeaseToken: token, RuntimeName: "scripted", RuntimeVersion: "0.0.0",
	}); err != nil {
		t.Fatal(err)
	}
	// The process dies here: the lock goes, the marker file stays.
	_ = marker.file.Close()
	if _, err := os.Stat(directMarkerPath(dataDir, job.ID)); err != nil {
		t.Fatalf("the abandoned run left no liveness marker: %v", err)
	}
	return job.ID, claim.Attempt.ID
}

// A killed run leaves a running attempt holding an embedded-worker slot. The
// next run must reclaim it — deleting the data directory was the only
// recovery before, and that throws away every trace with it.
func TestADirectRunReclaimsTheAttemptAnUngracefullyKilledRunAbandoned(t *testing.T) {
	dataDir := t.TempDir()
	repoPath := filepath.Join(t.TempDir(), "repo")
	abandonedJob, abandonedAttempt := abandonDirectRun(t, dataDir, repoPath)

	result, err := DirectRun(context.Background(), directConfig(dataDir, repoPath, acceptingExecutor(nil)))
	if err != nil {
		t.Fatalf("the run after an abandoned one failed: %v", err)
	}
	if result.Attempt.State != protocol.AttemptAcceptedUnpublished {
		t.Fatalf("attempt state = %q, want accepted_unpublished", result.Attempt.State)
	}

	store, err := Open(context.Background(), filepath.Join(dataDir, "jig.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	abandoned, err := store.Attempt(context.Background(), abandonedAttempt)
	if err != nil {
		t.Fatal(err)
	}
	if abandoned.State != protocol.AttemptLost {
		t.Fatalf("abandoned attempt state = %q, want lost — it must not hold a slot forever", abandoned.State)
	}
	if !strings.Contains(abandoned.Error, "marker") {
		t.Fatalf("abandoned attempt error = %q, want the evidence that reclaimed it", abandoned.Error)
	}
	job, err := store.Job(context.Background(), abandonedJob)
	if err != nil {
		t.Fatal(err)
	}
	if job.State != protocol.JobFailed {
		t.Fatalf("abandoned job state = %q, want failed", job.State)
	}
	if _, err := os.Stat(directMarkerPath(dataDir, abandonedJob)); !os.IsNotExist(err) {
		t.Fatalf("the reclaimed run's liveness marker survived (err=%v)", err)
	}
}

// Two direct runs sharing one data directory must both complete. Neither may
// force-fail the other's job: "an older queued job must be a crash leftover"
// is an assumption, and it was destroying live work.
func TestTwoConcurrentDirectRunsBothCompleteAndNeitherKillsTheOthersJob(t *testing.T) {
	dataDir := t.TempDir()
	// Initialize the store first: this test is about two runs racing for the
	// queue, not about two processes racing to create the database file.
	initial, err := Open(context.Background(), filepath.Join(dataDir, "jig.db"))
	if err != nil {
		t.Fatal(err)
	}
	initial.Close()

	bothClaimed := make(chan struct{})
	var once sync.Once
	var arrived int
	var mutex sync.Mutex
	// Each run blocks in its executor until BOTH hold their claims, so the
	// two runs really do overlap in the store.
	overlap := func(DirectExecution) {
		mutex.Lock()
		arrived++
		if arrived == 2 {
			once.Do(func() { close(bothClaimed) })
		}
		mutex.Unlock()
		select {
		case <-bothClaimed:
		case <-time.After(30 * time.Second):
			t.Error("the two direct runs never overlapped")
		}
	}

	results := make([]DirectRunResult, 2)
	errs := make([]error, 2)
	var group sync.WaitGroup
	for i := range results {
		group.Add(1)
		go func(i int) {
			defer group.Done()
			repoPath := filepath.Join(t.TempDir(), "repo")
			results[i], errs[i] = DirectRun(context.Background(),
				directConfig(dataDir, repoPath, acceptingExecutor(overlap)))
		}(i)
	}
	group.Wait()

	store, err := Open(context.Background(), filepath.Join(dataDir, "jig.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	for i := range results {
		if errs[i] != nil {
			t.Fatalf("concurrent direct run %d failed: %v", i, errs[i])
		}
		if results[i].Attempt.State != protocol.AttemptAcceptedUnpublished {
			t.Fatalf("run %d attempt state = %q, want accepted_unpublished (its job was destroyed)",
				i, results[i].Attempt.State)
		}
		job, err := store.Job(context.Background(), results[i].Job.ID)
		if err != nil {
			t.Fatal(err)
		}
		if job.State != protocol.JobAcceptedUnpublished {
			t.Fatalf("run %d job state = %q, want accepted_unpublished", i, job.State)
		}
	}
	if results[0].Job.ID == results[1].Job.ID {
		t.Fatal("both runs reported the same job")
	}
}

// A queued job this path never created — the server path's — is never
// touched, even when FIFO hands it over first. It goes back exactly as it
// was found.
func TestADirectRunReleasesAForeignQueuedJobInsteadOfFailingIt(t *testing.T) {
	dataDir := t.TempDir()
	repoPath := filepath.Join(t.TempDir(), "repo")
	ctx := context.Background()

	store, err := Open(ctx, filepath.Join(dataDir, "jig.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.RegisterWorker(ctx, directWorkerID, protocol.WorkerRegistration{
		Name: "embedded direct runner", WorkerVersion: "jig-run",
		Capacity: directWorkerCapacity, EnvNames: []string{"PATH"},
		Runtimes: []protocol.RuntimeCapability{directCapability()},
	}); err != nil {
		t.Fatal(err)
	}
	definitionID, generation, err := store.upsertDefinition(ctx, "server-path", directSnapshot)
	if err != nil {
		t.Fatal(err)
	}
	foreignRun, err := store.insertFrozenRun(ctx, definitionID, generation, directSnapshot, nil,
		protocol.RunTarget{Repository: repoPath, BaseSHA: directHeadSHA})
	if err != nil {
		t.Fatal(err)
	}
	// Queued by another path entirely: no liveness marker, so nothing about
	// it is ours to judge.
	foreignJob, err := store.EnqueueJob(ctx, foreignRun, repoPath)
	if err != nil {
		t.Fatal(err)
	}
	store.Close()

	_, runErr := DirectRun(ctx, directConfig(dataDir, repoPath, acceptingExecutor(nil)))
	if runErr == nil {
		t.Fatal("the direct run claimed past a foreign queued job")
	}
	if !strings.Contains(runErr.Error(), foreignJob.ID) {
		t.Fatalf("error %q does not name the job it refused to take", runErr)
	}

	store, err = Open(ctx, filepath.Join(dataDir, "jig.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	job, err := store.Job(ctx, foreignJob.ID)
	if err != nil {
		t.Fatal(err)
	}
	if job.State != protocol.JobQueued {
		t.Fatalf("foreign job state = %q, want it left queued and untouched", job.State)
	}
	var attemptState string
	if err := store.db.QueryRowContext(ctx,
		`SELECT state FROM attempts WHERE job_id = ?`, foreignJob.ID).Scan(&attemptState); err != nil {
		t.Fatal(err)
	}
	if attemptState != protocol.AttemptQueued {
		t.Fatalf("foreign attempt state = %q, want queued — the claim must be released, not consumed", attemptState)
	}
	// And the direct run cleaned up after itself: its own job is not left
	// queued for the next run to trip over.
	var stragglers int
	if err := store.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM jobs WHERE state = 'queued' AND id != ?
	`, foreignJob.ID).Scan(&stragglers); err != nil {
		t.Fatal(err)
	}
	if stragglers != 0 {
		t.Fatalf("%d queued job(s) leaked from the failed direct run", stragglers)
	}
}
