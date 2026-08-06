package controlplane

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/StructuPath/jig/internal/protocol"
)

// ---- claim idempotency (R4) ----------------------------------------------

func TestReplayingAClaimWithTheSameRequestIDAndTokenReturnsTheIdenticalAnswer(t *testing.T) {
	store, _ := newTestStore(t)
	seedRun(t, store, "run-1", protocol.RunTarget{Repository: repoA, BaseSHA: "pin000"})
	registerTestWorker(t, store, 2)
	if _, err := store.EnqueueJob(context.Background(), "run-1", repoA); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	first := mustClaim(t, store, "req-1", tokenA)
	replayed := mustClaim(t, store, "req-1", tokenA)
	if replayed.Attempt.ID != first.Attempt.ID || replayed.Job.ID != first.Job.ID {
		t.Fatalf("replay answered differently: first (%s, %s), replay (%s, %s)",
			first.Attempt.ID, first.Job.ID, replayed.Attempt.ID, replayed.Job.ID)
	}
	// One claim happened, not two: the attempt is still number 1 and no
	// second attempt exists.
	var attempts int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM attempts WHERE job_id = ?`, first.Job.ID).Scan(&attempts); err != nil {
		t.Fatalf("count attempts: %v", err)
	}
	if attempts != 1 {
		t.Fatalf("replay created a second attempt: %d attempts", attempts)
	}
}

func TestReplayingAClaimRequestIDWithADifferentTokenIsAConflict(t *testing.T) {
	store, _ := newTestStore(t)
	seedRun(t, store, "run-1", protocol.RunTarget{Repository: repoA, BaseSHA: "pin000"})
	registerTestWorker(t, store, 2)
	if _, err := store.EnqueueJob(context.Background(), "run-1", repoA); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	mustClaim(t, store, "req-1", tokenA)
	_, err := store.Claim(context.Background(), workerA, protocol.ClaimRequest{
		RequestID: "req-1", LeaseToken: tokenB,
	})
	if err == nil {
		t.Fatal("the same request id with a different token must conflict")
	}
	if code := serviceCode(t, err); code != "claim_request_conflict" {
		t.Fatalf("conflict code = %q, want claim_request_conflict", code)
	}
}

func TestAnEmptyClaimReplaysEmptyUnderItsRequestID(t *testing.T) {
	store, _ := newTestStore(t)
	registerTestWorker(t, store, 2)
	// No jobs queued at all.
	for i := 0; i < 2; i++ {
		claim, err := store.Claim(context.Background(), workerA, protocol.ClaimRequest{
			RequestID: "req-empty", LeaseToken: tokenA,
		})
		if err != nil || claim != nil {
			t.Fatalf("pass %d: empty claim = (%v, %v), want (nil, nil)", i, claim, err)
		}
	}
}

// ---- claim eligibility (R4, R17) -----------------------------------------

func TestAWorkerPastItsLivenessWindowClaimsNothingEvenWithQueuedWork(t *testing.T) {
	store, clock := newTestStore(t)
	seedRun(t, store, "run-1", protocol.RunTarget{Repository: repoA, BaseSHA: "pin000"})
	registerTestWorker(t, store, 2)
	if _, err := store.EnqueueJob(context.Background(), "run-1", repoA); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	clock.Advance(protocol.WorkerLivenessWindow + time.Second)
	claim, err := store.Claim(context.Background(), workerA, protocol.ClaimRequest{
		RequestID: "req-stale", LeaseToken: tokenA,
	})
	if err != nil || claim != nil {
		t.Fatalf("stale worker claim = (%v, %v), want empty", claim, err)
	}
	// Re-registration refreshes liveness and the same work claims fine.
	registerTestWorker(t, store, 2)
	mustClaim(t, store, "req-fresh", tokenB)
}

func TestAWorkerAtCapacityClaimsNothingEvenWithQueuedWork(t *testing.T) {
	store, _ := newTestStore(t)
	seedRun(t, store, "run-1",
		protocol.RunTarget{Repository: repoA, BaseSHA: "pin000"},
		protocol.RunTarget{Repository: repoB, BaseSHA: "pin111"})
	registerTestWorker(t, store, 1)
	ctx := context.Background()
	if _, err := store.EnqueueJob(ctx, "run-1", repoA); err != nil {
		t.Fatalf("enqueue A: %v", err)
	}
	if _, err := store.EnqueueJob(ctx, "run-1", repoB); err != nil {
		t.Fatalf("enqueue B: %v", err)
	}
	held := mustClaim(t, store, "req-1", tokenA)
	claim, err := store.Claim(ctx, workerA, protocol.ClaimRequest{
		RequestID: "req-2", LeaseToken: tokenB,
	})
	if err != nil || claim != nil {
		t.Fatalf("claim at capacity = (%v, %v), want empty", claim, err)
	}
	// Finishing the held attempt frees the slot.
	if _, err := store.StartAttempt(ctx, held.Attempt.ID, protocol.StartAttemptRequest{LeaseToken: tokenA}); err != nil {
		t.Fatalf("start: %v", err)
	}
	if _, err := store.CompleteAttempt(ctx, held.Attempt.ID, protocol.CompleteAttemptRequest{
		LeaseToken: tokenA, State: protocol.AttemptFailed,
	}); err != nil {
		t.Fatalf("complete: %v", err)
	}
	mustClaim(t, store, "req-3", tokenB)
}

func TestAWorkerMissingARequiredEnvNameClaimsNothingEvenWithQueuedWork(t *testing.T) {
	store, _ := newTestStore(t)
	seedRun(t, store, "run-1", protocol.RunTarget{Repository: repoA, BaseSHA: "pin000"})
	// The fixture definition requires GITHUB_TOKEN; this worker never
	// advertised it (R17, KTD10).
	registerTestWorker(t, store, 2, "PATH", "HOME")
	if _, err := store.EnqueueJob(context.Background(), "run-1", repoA); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	claim, err := store.Claim(context.Background(), workerA, protocol.ClaimRequest{
		RequestID: "req-1", LeaseToken: tokenA,
	})
	if err != nil || claim != nil {
		t.Fatalf("claim without the required env name = (%v, %v), want empty", claim, err)
	}
	// Advertising the name makes the same job claimable.
	registerTestWorker(t, store, 2, "PATH", "HOME", "GITHUB_TOKEN")
	mustClaim(t, store, "req-2", tokenB)
}

// ---- FIFO with retained-worktree skip-over (R4, R16) ---------------------

func TestClaimReturnsRepoBsJobWhenRepoAIsAtItsRetainedWorktreeCap(t *testing.T) {
	store, clock := newTestStore(t)
	seedRun(t, store, "run-1",
		protocol.RunTarget{Repository: repoA, BaseSHA: "pin000"},
		protocol.RunTarget{Repository: repoB, BaseSHA: "pin111"})
	registerTestWorker(t, store, 5)
	ctx := context.Background()

	// repoA holds MaxRetainedWorktreesPerRepo retained worktrees from an
	// older run's failed attempts.
	seedRun(t, store, "run-old", protocol.RunTarget{Repository: repoA, BaseSHA: "old000"})
	now := store.now().UnixMilli()
	if _, err := store.db.Exec(`
		INSERT INTO jobs(id, run_id, repository, base_sha, state, created_at, updated_at)
		VALUES ('job-old', 'run-old', ?, 'old000', 'failed', ?, ?)
	`, repoA, now, now); err != nil {
		t.Fatalf("seed old job: %v", err)
	}
	for i := 1; i <= protocol.MaxRetainedWorktreesPerRepo; i++ {
		attemptID := fmt.Sprintf("old-attempt-%d", i)
		if _, err := store.db.Exec(`
			INSERT INTO attempts(id, job_id, worker_id, attempt_number, state, created_at)
			VALUES (?, 'job-old', ?, ?, 'failed', ?)
		`, attemptID, workerA, i, now); err != nil {
			t.Fatalf("seed old attempt %d: %v", i, err)
		}
		if _, err := store.db.Exec(`
			INSERT INTO retained_worktrees(attempt_id, worker_id, repository, path, reason, state, created_at, updated_at)
			VALUES (?, ?, ?, ?, 'unpublished commits', 'retained', ?, ?)
		`, attemptID, workerA, repoA, "/tmp/wt-"+attemptID, now, now); err != nil {
			t.Fatalf("seed retained worktree %d: %v", i, err)
		}
	}

	// repoA's job is older and would win FIFO; the cap skips it (R4).
	jobA, err := store.EnqueueJob(ctx, "run-1", repoA)
	if err != nil {
		t.Fatalf("enqueue A: %v", err)
	}
	clock.Advance(time.Millisecond)
	jobB, err := store.EnqueueJob(ctx, "run-1", repoB)
	if err != nil {
		t.Fatalf("enqueue B: %v", err)
	}
	claim := mustClaim(t, store, "req-1", tokenA)
	if claim.Job.ID != jobB.ID {
		t.Fatalf("claim returned job for %s, want repoB's job (repoA is at cap)", claim.Job.Repository)
	}
	// Releasing one retained worktree makes repoA's job claimable again.
	if _, err := store.db.Exec(`
		UPDATE retained_worktrees SET state = 'released' WHERE attempt_id = 'old-attempt-1'
	`); err != nil {
		t.Fatalf("release: %v", err)
	}
	second := mustClaim(t, store, "req-2", tokenB)
	if second.Job.ID != jobA.ID {
		t.Fatalf("after release, claim returned %s, want repoA's job", second.Job.Repository)
	}
}

// ---- race-detector interleavings (U2 verification) -----------------------
//
// These tests exist to run under -race (the test-race Justfile recipe): they
// interleave the claim, heartbeat, sweep, complete, and retry paths across
// goroutines and assert the invariants the transactions and constraints
// promise, whatever the interleaving.

func TestConcurrentClaimsWithDistinctRequestIDsNeverDoubleClaimAJob(t *testing.T) {
	store, _ := newTestStore(t)
	targets := []protocol.RunTarget{
		{Repository: repoA, BaseSHA: "pin000"},
		{Repository: repoB, BaseSHA: "pin111"},
		{Repository: "github.com/example/repo-c", BaseSHA: "pin222"},
		{Repository: "github.com/example/repo-d", BaseSHA: "pin333"},
	}
	seedRun(t, store, "run-1", targets...)
	registerTestWorker(t, store, 10)
	ctx := context.Background()
	for _, target := range targets {
		if _, err := store.EnqueueJob(ctx, "run-1", target.Repository); err != nil {
			t.Fatalf("enqueue %s: %v", target.Repository, err)
		}
	}
	const claimers = 8
	results := make([]*protocol.Claim, claimers)
	errs := make([]error, claimers)
	var wg sync.WaitGroup
	for i := 0; i < claimers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			token := fmt.Sprintf("%064d", i)
			results[i], errs[i] = store.Claim(ctx, workerA, protocol.ClaimRequest{
				RequestID: fmt.Sprintf("req-%d", i), LeaseToken: token,
			})
		}(i)
	}
	wg.Wait()
	claimedJobs := make(map[string]int)
	claimed := 0
	for i := range results {
		if errs[i] != nil {
			t.Fatalf("claimer %d: %v", i, errs[i])
		}
		if results[i] != nil {
			claimed++
			claimedJobs[results[i].Job.ID]++
		}
	}
	for jobID, count := range claimedJobs {
		if count > 1 {
			t.Fatalf("job %s was claimed %d times", jobID, count)
		}
	}
	if claimed != len(targets) {
		t.Fatalf("claimed %d jobs of %d queued with %d claimers", claimed, len(targets), claimers)
	}
	var preparing int
	if err := store.db.QueryRow(
		`SELECT COUNT(*) FROM attempts WHERE state = 'preparing'`).Scan(&preparing); err != nil {
		t.Fatalf("count preparing: %v", err)
	}
	if preparing != len(targets) {
		t.Fatalf("%d preparing attempts, want %d", preparing, len(targets))
	}
}

func TestConcurrentRetriesOfOneFailedJobCreateExactlyOneSuccessorAttempt(t *testing.T) {
	store, _ := newTestStore(t)
	seedRun(t, store, "run-1", protocol.RunTarget{Repository: repoA, BaseSHA: "pin000"})
	registerTestWorker(t, store, 2)
	claim := claimAndStart(t, store, "run-1", repoA)
	ctx := context.Background()
	if _, err := store.CompleteAttempt(ctx, claim.Attempt.ID, protocol.CompleteAttemptRequest{
		LeaseToken: tokenA, State: protocol.AttemptFailed,
	}); err != nil {
		t.Fatalf("fail attempt: %v", err)
	}
	const retriers = 8
	errs := make([]error, retriers)
	var wg sync.WaitGroup
	for i := 0; i < retriers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = store.RetryJob(ctx, claim.Job.ID)
		}(i)
	}
	wg.Wait()
	succeeded := 0
	for i, err := range errs {
		if err == nil {
			succeeded++
			continue
		}
		// A loser that raced inside the winner's window sees retry_conflict
		// (the partial index); one that arrived after the winner committed
		// sees retry_not_allowed (the job is queued again). Both are correct.
		if code := serviceCode(t, err); code != "retry_conflict" && code != "retry_not_allowed" {
			t.Fatalf("retrier %d: unexpected code %q (%v)", i, code, err)
		}
	}
	// The partial unique index makes exactly one racer win (KTD3).
	if succeeded != 1 {
		t.Fatalf("%d retries succeeded, want exactly 1", succeeded)
	}
	var attempts int
	if err := store.db.QueryRow(
		`SELECT COUNT(*) FROM attempts WHERE job_id = ?`, claim.Job.ID).Scan(&attempts); err != nil {
		t.Fatalf("count attempts: %v", err)
	}
	if attempts != 2 {
		t.Fatalf("job has %d attempts, want 2", attempts)
	}
}

func TestConcurrentHeartbeatsSweepsAndCompletionKeepExactlyOneOutcome(t *testing.T) {
	store, clock := newTestStore(t)
	seedRun(t, store, "run-1", protocol.RunTarget{Repository: repoA, BaseSHA: "pin000"})
	registerTestWorker(t, store, 2)
	claim := claimAndStart(t, store, "run-1", repoA)
	sweeper := NewSweeper(store)
	ctx := context.Background()

	var wg sync.WaitGroup
	wg.Add(3)
	go func() {
		defer wg.Done()
		for i := 0; i < 40; i++ {
			// Renewal conflicts are legitimate once the attempt is terminal.
			store.Heartbeat(ctx, claim.Attempt.ID, protocol.HeartbeatRequest{LeaseToken: tokenA})
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 40; i++ {
			clock.Advance(protocol.HeartbeatInterval)
			if _, err := sweeper.Tick(ctx); err != nil {
				t.Errorf("sweep tick: %v", err)
				return
			}
		}
	}()
	go func() {
		defer wg.Done()
		// One completion, somewhere in the storm. A conflict means the sweep
		// legitimately won first.
		store.CompleteAttempt(ctx, claim.Attempt.ID, protocol.CompleteAttemptRequest{
			LeaseToken: tokenA, State: protocol.AttemptAccepted, Result: "raced",
		})
	}()
	wg.Wait()

	attempt, err := store.Attempt(ctx, claim.Attempt.ID)
	if err != nil {
		t.Fatalf("read attempt: %v", err)
	}
	job, err := store.Job(ctx, claim.Job.ID)
	if err != nil {
		t.Fatalf("read job: %v", err)
	}
	switch attempt.State {
	case protocol.AttemptAccepted:
		if job.State != protocol.JobAccepted || attempt.Result != "raced" {
			t.Fatalf("accepted attempt with job %q result %q", job.State, attempt.Result)
		}
	case protocol.AttemptLost:
		if job.State != protocol.JobFailed {
			t.Fatalf("lost attempt with job state %q, want failed", job.State)
		}
	case protocol.AttemptRunning:
		if job.State != protocol.JobActive {
			t.Fatalf("running attempt with job state %q, want active", job.State)
		}
	default:
		t.Fatalf("attempt ended in unexpected state %q", attempt.State)
	}
	// Whatever won, a heartbeat against a terminal attempt must now conflict,
	// and a terminal outcome must never flip.
	if attempt.State != protocol.AttemptRunning {
		if _, err := store.Heartbeat(ctx, claim.Attempt.ID, protocol.HeartbeatRequest{LeaseToken: tokenA}); err == nil {
			t.Fatal("heartbeat renewed a terminal attempt")
		}
	}
}
