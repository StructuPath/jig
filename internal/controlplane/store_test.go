package controlplane

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/StructuPath/jig/internal/protocol"
)

const (
	workerA = "worker-a"
	tokenA  = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	tokenB  = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

	repoA = "github.com/example/repo-a"
	repoB = "github.com/example/repo-b"
)

// fixtureSnapshot is a minimal valid frozen definition. The builder role's
// env allowlist makes GITHUB_TOKEN a claim-eligibility requirement (R17).
const fixtureSnapshot = `name: fixture
roster:
  builder:
    model: claude-sonnet
    system_prompt: build
    user_prompt: build it
    env: [GITHUB_TOKEN]
phases:
  - name: build
    kind: agent
    owner: builder
`

// testClock is a mutex-guarded fake clock so race tests can advance time
// while store transactions read it.
type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *testClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func newTestStore(t *testing.T) (*Store, *testClock) {
	t.Helper()
	store, err := Open(context.Background(), filepath.Join(t.TempDir(), "jig.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})
	clock := &testClock{now: time.Date(2026, 8, 5, 8, 0, 0, 0, time.UTC)}
	store.now = clock.Now
	return store, clock
}

// seedRun inserts the definition + run rows directly — the definitions/runs
// API is U5; U2 tests own the fixtures (plan, U2 approach note).
func seedRun(t *testing.T, store *Store, runID string, targets ...protocol.RunTarget) {
	t.Helper()
	seedRunSnapshot(t, store, runID, fixtureSnapshot, targets...)
}

// seedRunSnapshot is seedRun with a caller-chosen frozen definition.
func seedRunSnapshot(t *testing.T, store *Store, runID, snapshot string, targets ...protocol.RunTarget) {
	t.Helper()
	targetsJSON := "["
	for i, target := range targets {
		if i > 0 {
			targetsJSON += ","
		}
		targetsJSON += fmt.Sprintf(`{"repository":%q,"base_sha":%q}`, target.Repository, target.BaseSHA)
	}
	targetsJSON += "]"
	now := store.now().UnixMilli()
	if _, err := store.db.Exec(`
		INSERT INTO definitions(id, name, generation, source, created_at, updated_at)
		VALUES (?, ?, 1, ?, ?, ?)
	`, "def-"+runID, "fixture-"+runID, snapshot, now, now); err != nil {
		t.Fatalf("seed definition: %v", err)
	}
	if _, err := store.db.Exec(`
		INSERT INTO runs(id, definition_id, definition_generation, snapshot, parameters, targets, state, created_at, updated_at)
		VALUES (?, ?, 1, ?, '{}', ?, 'active', ?, ?)
	`, runID, "def-"+runID, snapshot, targetsJSON, now, now); err != nil {
		t.Fatalf("seed run: %v", err)
	}
}

func registerTestWorker(t *testing.T, store *Store, capacity int, envNames ...string) {
	t.Helper()
	if envNames == nil {
		envNames = []string{"GITHUB_TOKEN", "HOME", "PATH"}
	}
	_, err := store.RegisterWorker(context.Background(), workerA, protocol.WorkerRegistration{
		Name:          "local",
		WorkerVersion: "dev",
		Capacity:      capacity,
		EnvNames:      envNames,
	})
	if err != nil {
		t.Fatalf("register worker: %v", err)
	}
}

func mustClaim(t *testing.T, store *Store, requestID, token string) *protocol.Claim {
	t.Helper()
	claim, err := store.Claim(context.Background(), workerA, protocol.ClaimRequest{
		RequestID: requestID, LeaseToken: token,
	})
	if err != nil {
		t.Fatalf("claim %s: %v", requestID, err)
	}
	if claim == nil {
		t.Fatalf("claim %s: expected a claim, got empty", requestID)
	}
	return claim
}

func serviceCode(t *testing.T, err error) string {
	t.Helper()
	var service *ServiceError
	if !errors.As(err, &service) {
		t.Fatalf("expected a ServiceError, got %v", err)
	}
	return service.Code
}

// proveThePublish records the three publish steps an attempt needs before it
// may legitimately complete `accepted`. R12 is a conjunction — phases passed
// AND predicate held AND publish proven — and the store enforces the third
// conjunct inside the completion transaction, so any test that ends an
// attempt in `accepted` has to have published first, exactly as the worker's
// publish pipeline does.
func proveThePublish(t *testing.T, store *Store, claim *protocol.Claim, token string) {
	t.Helper()
	ctx := context.Background()
	branch := protocol.PublishBranch(claim.Job.ID, claim.Attempt.AttemptNumber)
	for _, step := range []protocol.PublishStepRequest{
		{Step: protocol.PublishStepPush, RemoteRef: shaA},
		{Step: protocol.PublishStepPullRequest, PullRequestURL: "https://github.com/example/repo/pull/1"},
		{Step: protocol.PublishStepProof, RemoteRef: shaA},
	} {
		step.LeaseToken, step.Branch = token, branch
		if _, err := store.RecordPublishStep(ctx, claim.Attempt.ID, step); err != nil {
			t.Fatalf("record publish step %s: %v", step.Step, err)
		}
	}
}

// claimAndStart is the standard path to a running, leased attempt.
func claimAndStart(t *testing.T, store *Store, runID, repository string) *protocol.Claim {
	t.Helper()
	job, err := store.EnqueueJob(context.Background(), runID, repository)
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	claim := mustClaim(t, store, "req-"+job.ID, tokenA)
	if _, err := store.StartAttempt(context.Background(), claim.Attempt.ID, protocol.StartAttemptRequest{
		LeaseToken: tokenA, RuntimeName: "claude-code", RuntimeVersion: "test",
	}); err != nil {
		t.Fatalf("start attempt: %v", err)
	}
	return claim
}

// ---- attempt lifecycle and fencing ---------------------------------------

func TestEnqueueCreatesAQueuedJobWithAQueuedUnleasedFirstAttempt(t *testing.T) {
	store, _ := newTestStore(t)
	seedRun(t, store, "run-1", protocol.RunTarget{Repository: repoA, BaseSHA: "pin000"})
	job, err := store.EnqueueJob(context.Background(), "run-1", repoA)
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if job.State != protocol.JobQueued || job.BaseSHA != "pin000" {
		t.Fatalf("job = %+v, want queued at pin000", job)
	}
	var attemptState string
	var workerID, digest, expiry any
	if err := store.db.QueryRow(`
		SELECT state, worker_id, lease_digest, lease_expires_at FROM attempts WHERE job_id = ?
	`, job.ID).Scan(&attemptState, &workerID, &digest, &expiry); err != nil {
		t.Fatalf("read attempt: %v", err)
	}
	if attemptState != protocol.AttemptQueued || workerID != nil || digest != nil || expiry != nil {
		t.Fatalf("pre-claim attempt = (%s, %v, %v, %v), want queued with NULL lease fields",
			attemptState, workerID, digest, expiry)
	}
}

func TestEveryAttemptStateWriteValidatesTheLeaseDigest(t *testing.T) {
	store, _ := newTestStore(t)
	seedRun(t, store, "run-1", protocol.RunTarget{Repository: repoA, BaseSHA: "pin000"})
	registerTestWorker(t, store, 2)
	if _, err := store.EnqueueJob(context.Background(), "run-1", repoA); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	claim := mustClaim(t, store, "req-1", tokenA)
	ctx := context.Background()
	if _, err := store.StartAttempt(ctx, claim.Attempt.ID, protocol.StartAttemptRequest{LeaseToken: tokenB}); err == nil {
		t.Fatal("start with a foreign token must be rejected")
	} else if serviceCode(t, err) != "lease_not_owner" {
		t.Fatalf("start with foreign token: unexpected code %q", serviceCode(t, err))
	}
	if _, err := store.CompleteAttempt(ctx, claim.Attempt.ID, protocol.CompleteAttemptRequest{
		LeaseToken: tokenB, State: protocol.AttemptFailed,
	}); err == nil {
		t.Fatal("complete with a foreign token must be rejected")
	}
	if _, err := store.Heartbeat(ctx, claim.Attempt.ID, protocol.HeartbeatRequest{LeaseToken: tokenB}); err == nil {
		t.Fatal("heartbeat with a foreign token must be rejected")
	}
	attempt, err := store.Attempt(ctx, claim.Attempt.ID)
	if err != nil {
		t.Fatalf("read attempt: %v", err)
	}
	if attempt.State != protocol.AttemptPreparing {
		t.Fatalf("fenced-out writes changed state to %q", attempt.State)
	}
}

func TestTerminalCompletionReplayedWithTheOriginalTokenReturnsTheStoredOutcome(t *testing.T) {
	store, _ := newTestStore(t)
	seedRun(t, store, "run-1", protocol.RunTarget{Repository: repoA, BaseSHA: "pin000"})
	registerTestWorker(t, store, 2)
	claim := claimAndStart(t, store, "run-1", repoA)
	ctx := context.Background()
	proveThePublish(t, store, claim, tokenA)
	first, err := store.CompleteAttempt(ctx, claim.Attempt.ID, protocol.CompleteAttemptRequest{
		LeaseToken: tokenA, State: protocol.AttemptAccepted, Result: "published PR #7",
	})
	if err != nil {
		t.Fatalf("complete: %v", err)
	}
	// The replay even disagrees about the outcome; the stored outcome wins.
	replayed, err := store.CompleteAttempt(ctx, claim.Attempt.ID, protocol.CompleteAttemptRequest{
		LeaseToken: tokenA, State: protocol.AttemptFailed, Error: "should be ignored",
	})
	if err != nil {
		t.Fatalf("replayed completion: %v", err)
	}
	if replayed.State != protocol.AttemptAccepted || replayed.Result != first.Result || replayed.Error != "" {
		t.Fatalf("replay changed the stored outcome: %+v", replayed)
	}
	if _, err := store.CompleteAttempt(ctx, claim.Attempt.ID, protocol.CompleteAttemptRequest{
		LeaseToken: tokenB, State: protocol.AttemptAccepted,
	}); err == nil {
		t.Fatal("terminal replay with a foreign token must be rejected")
	}
	job, err := store.Job(ctx, claim.Job.ID)
	if err != nil {
		t.Fatalf("read job: %v", err)
	}
	if job.State != protocol.JobAccepted {
		t.Fatalf("job state = %q, want accepted", job.State)
	}
}

// R12's "published" conjunct is a control-plane invariant, not a worker
// convention: `accepted` means phases passed AND the predicate held AND
// publish completed with remote proof. The first two are worker judgements
// the store cannot re-derive; the third is a row in its own database, so it
// checks — inside the completion transaction, against a fenced, still-leased
// attempt. Without it, any worker bug, stale build, or hand-driven API call
// could mark unpublished work `accepted`, and R14's publish-only retry would
// never be offered for work that genuinely needs it.
func TestCompletingAsAcceptedIsRefusedWithoutARecordedProofOfPublish(t *testing.T) {
	store, _ := newTestStore(t)
	seedRun(t, store, "run-1", protocol.RunTarget{Repository: repoA, BaseSHA: shaA})
	registerTestWorker(t, store, 2)
	claim := claimAndStart(t, store, "run-1", repoA)
	ctx := context.Background()

	_, err := store.CompleteAttempt(ctx, claim.Attempt.ID, protocol.CompleteAttemptRequest{
		LeaseToken: tokenA, State: protocol.AttemptAccepted, Result: "claims to be published",
	})
	if err == nil {
		t.Fatal("an attempt with no publish record completed as accepted")
	}
	if code := serviceCode(t, err); code != "publish_proof_required" {
		t.Fatalf("error code = %q, want publish_proof_required", code)
	}
	attempt, err := store.Attempt(ctx, claim.Attempt.ID)
	if err != nil {
		t.Fatalf("read attempt: %v", err)
	}
	if attempt.State != protocol.AttemptRunning {
		t.Fatalf("the refused completion moved the attempt to %q", attempt.State)
	}

	// A push and a pull request are not proof either: proof is the step that
	// verified the remote ref, and it is the one the check requires.
	branch := protocol.PublishBranch(claim.Job.ID, claim.Attempt.AttemptNumber)
	if _, err := recordStep(store, claim, tokenA, protocol.PublishStepPush, shaA, ""); err != nil {
		t.Fatalf("record push: %v", err)
	}
	if _, err := recordStep(store, claim, tokenA, protocol.PublishStepPullRequest, "",
		"https://github.com/example/repo-a/pull/7"); err != nil {
		t.Fatalf("record pull request: %v", err)
	}
	if _, err := store.CompleteAttempt(ctx, claim.Attempt.ID, protocol.CompleteAttemptRequest{
		LeaseToken: tokenA, State: protocol.AttemptAccepted,
	}); err == nil || serviceCode(t, err) != "publish_proof_required" {
		t.Fatalf("accepted with a push but no proof: err=%v, want publish_proof_required", err)
	}

	// accepted_unpublished is the state that IS available, and it is what
	// carries the publish-only retry (R14).
	unpublished, err := store.CompleteAttempt(ctx, claim.Attempt.ID, protocol.CompleteAttemptRequest{
		LeaseToken: tokenA, State: protocol.AttemptAcceptedUnpublished,
	})
	if err != nil {
		t.Fatalf("complete as accepted_unpublished: %v", err)
	}
	if unpublished.State != protocol.AttemptAcceptedUnpublished {
		t.Fatalf("attempt state = %q, want accepted_unpublished", unpublished.State)
	}
	if branch == "" {
		t.Fatal("the attempt-scoped branch is empty")
	}
}

// ---- co-sleep and sweep (R5) ---------------------------------------------

func TestFirstHeartbeatAfterAnEightHourCoSleepRenewsTheExpiredLease(t *testing.T) {
	store, clock := newTestStore(t)
	seedRun(t, store, "run-1", protocol.RunTarget{Repository: repoA, BaseSHA: "pin000"})
	registerTestWorker(t, store, 2)
	claim := claimAndStart(t, store, "run-1", repoA)
	sweeper := NewSweeper(store)
	if _, err := sweeper.Tick(context.Background()); err != nil {
		t.Fatalf("pre-sleep tick: %v", err)
	}

	// Server and worker sleep together for 8 hours; nobody ticks, nobody
	// heartbeats. The lease is long expired on wake.
	clock.Advance(8 * time.Hour)

	// The server's first tick after wake sees its own gap, resets the
	// missed-heartbeat counts, and must not sweep the attempt (R5).
	if _, err := sweeper.Tick(context.Background()); err != nil {
		t.Fatalf("wake tick: %v", err)
	}
	attempt, err := store.Attempt(context.Background(), claim.Attempt.ID)
	if err != nil {
		t.Fatalf("read attempt: %v", err)
	}
	if attempt.State != protocol.AttemptRunning {
		t.Fatalf("attempt was swept on the first wake tick: state %q", attempt.State)
	}

	// The worker's first heartbeat renews the expired-but-unswept lease...
	response, err := store.Heartbeat(context.Background(), claim.Attempt.ID,
		protocol.HeartbeatRequest{LeaseToken: tokenA})
	if err != nil {
		t.Fatalf("first heartbeat after wake must renew the expired lease: %v", err)
	}
	if want := store.now().Add(protocol.LeaseDuration).UTC(); !response.LeaseExpiresAt.Equal(want) {
		t.Fatalf("renewed expiry = %v, want %v", response.LeaseExpiresAt, want)
	}
	// ...and the attempt continues to a normal completion.
	proveThePublish(t, store, claim, tokenA)
	if _, err := store.CompleteAttempt(context.Background(), claim.Attempt.ID, protocol.CompleteAttemptRequest{
		LeaseToken: tokenA, State: protocol.AttemptAccepted, Result: "survived the sleep",
	}); err != nil {
		t.Fatalf("attempt could not continue after renewal: %v", err)
	}
}

func TestAHeartbeatOnAnExpiredLeaseIsRejectedWhenASuccessorAttemptExists(t *testing.T) {
	store, clock := newTestStore(t)
	seedRun(t, store, "run-1", protocol.RunTarget{Repository: repoA, BaseSHA: "pin000"})
	registerTestWorker(t, store, 2)
	claim := claimAndStart(t, store, "run-1", repoA)
	sweepToLost(t, store, clock, claim.Attempt.ID)
	if _, err := store.RetryJob(context.Background(), claim.Job.ID); err != nil {
		t.Fatalf("retry: %v", err)
	}
	_, err := store.Heartbeat(context.Background(), claim.Attempt.ID,
		protocol.HeartbeatRequest{LeaseToken: tokenA})
	if err == nil {
		t.Fatal("a zombie heartbeat must not renew once a successor attempt exists")
	}
	if code := serviceCode(t, err); code != "lease_superseded" {
		t.Fatalf("zombie heartbeat code = %q, want lease_superseded", code)
	}
}

// sweepToLost drives the sweeper through MissedHeartbeatsBeforeSweep
// consecutive missed heartbeats at heartbeat cadence.
func sweepToLost(t *testing.T, store *Store, clock *testClock, attemptID string) {
	t.Helper()
	sweeper := NewSweeper(store)
	maxTicks := int(protocol.LeaseDuration/protocol.HeartbeatInterval) +
		protocol.MissedHeartbeatsBeforeSweep + 2
	for i := 0; i < maxTicks; i++ {
		clock.Advance(protocol.HeartbeatInterval)
		if _, err := sweeper.Tick(context.Background()); err != nil {
			t.Fatalf("sweep tick: %v", err)
		}
		attempt, err := store.Attempt(context.Background(), attemptID)
		if err != nil {
			t.Fatalf("read attempt: %v", err)
		}
		if attempt.State == protocol.AttemptLost {
			return
		}
	}
	t.Fatal("sweeper never marked the attempt lost")
}

func TestNMissedHeartbeatsSweepTheAttemptToLostAndAStaleCompletionIsRejected(t *testing.T) {
	store, clock := newTestStore(t)
	seedRun(t, store, "run-1", protocol.RunTarget{Repository: repoA, BaseSHA: "pin000"})
	registerTestWorker(t, store, 2)
	claim := claimAndStart(t, store, "run-1", repoA)
	sweeper := NewSweeper(store)
	ctx := context.Background()

	// The lease lasts LeaseDuration; ticks run at heartbeat cadence. Misses
	// only start counting once the lease is expired, so the attempt must
	// survive every tick before expiry plus the first
	// MissedHeartbeatsBeforeSweep-1 ticks after it.
	ticksBeforeExpiry := int(protocol.LeaseDuration / protocol.HeartbeatInterval)
	for i := 0; i < ticksBeforeExpiry-1; i++ {
		clock.Advance(protocol.HeartbeatInterval)
		if lost, err := sweeper.Tick(ctx); err != nil || len(lost) != 0 {
			t.Fatalf("tick %d before expiry: lost=%v err=%v", i, lost, err)
		}
	}
	for i := 0; i < protocol.MissedHeartbeatsBeforeSweep-1; i++ {
		clock.Advance(protocol.HeartbeatInterval)
		if lost, err := sweeper.Tick(ctx); err != nil || len(lost) != 0 {
			t.Fatalf("miss %d must not sweep yet: lost=%v err=%v", i+1, lost, err)
		}
	}
	clock.Advance(protocol.HeartbeatInterval)
	lost, err := sweeper.Tick(ctx)
	if err != nil {
		t.Fatalf("final tick: %v", err)
	}
	if len(lost) != 1 || lost[0].AttemptID != claim.Attempt.ID {
		t.Fatalf("final miss must sweep exactly this attempt, got %v", lost)
	}
	attempt, err := store.Attempt(ctx, claim.Attempt.ID)
	if err != nil {
		t.Fatalf("read attempt: %v", err)
	}
	if attempt.State != protocol.AttemptLost {
		t.Fatalf("attempt state = %q, want lost", attempt.State)
	}
	job, err := store.Job(ctx, claim.Job.ID)
	if err != nil {
		t.Fatalf("read job: %v", err)
	}
	if job.State != protocol.JobFailed {
		t.Fatalf("job state = %q, want failed after sweep", job.State)
	}
	// The zombie wakes and tries to complete with its stale token.
	_, err = store.CompleteAttempt(ctx, claim.Attempt.ID, protocol.CompleteAttemptRequest{
		LeaseToken: tokenA, State: protocol.AttemptAccepted, Result: "zombie work",
	})
	if err == nil {
		t.Fatal("a completion with the stale token after sweep must be rejected")
	}
	if code := serviceCode(t, err); code != "lease_not_owner" {
		t.Fatalf("stale completion code = %q, want lease_not_owner", code)
	}
}

// ---- retry (cold, pinned) ------------------------------------------------

func TestRetryOfAFailedJobCreatesAttemptTwoTargetingThePinnedBaseSHA(t *testing.T) {
	store, clock := newTestStore(t)
	// The run pinned repoA at pin000 at admission (KTD9). The repository's
	// head moving afterwards is invisible here by design: nothing in the
	// claim or retry path resolves a ref — the pinned SHA on the job row is
	// the only source.
	seedRun(t, store, "run-1", protocol.RunTarget{Repository: repoA, BaseSHA: "pin000"})
	registerTestWorker(t, store, 2)
	claim := claimAndStart(t, store, "run-1", repoA)
	ctx := context.Background()
	if _, err := store.CompleteAttempt(ctx, claim.Attempt.ID, protocol.CompleteAttemptRequest{
		LeaseToken: tokenA, State: protocol.AttemptFailed, Error: "gate budget exhausted",
	}); err != nil {
		t.Fatalf("fail attempt: %v", err)
	}
	clock.Advance(time.Minute)      // head "moves" during the gap; jig never looks
	registerTestWorker(t, store, 2) // the worker re-registers, refreshing liveness
	job, err := store.RetryJob(ctx, claim.Job.ID)
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if job.State != protocol.JobQueued {
		t.Fatalf("retried job state = %q, want queued", job.State)
	}
	second := mustClaim(t, store, "req-retry", tokenB)
	if second.Attempt.AttemptNumber != 2 {
		t.Fatalf("attempt number = %d, want 2 (cold successor)", second.Attempt.AttemptNumber)
	}
	if second.Job.BaseSHA != "pin000" {
		t.Fatalf("retry base SHA = %q, want the pinned pin000", second.Job.BaseSHA)
	}
	if second.Attempt.ID == claim.Attempt.ID {
		t.Fatal("retry must create a new attempt, not resurrect the old one")
	}
	if second.Snapshot != fixtureSnapshot {
		t.Fatal("retry must execute the run's frozen snapshot")
	}
}

func TestRetryOfANonFailedJobIsRejected(t *testing.T) {
	store, _ := newTestStore(t)
	seedRun(t, store, "run-1", protocol.RunTarget{Repository: repoA, BaseSHA: "pin000"})
	registerTestWorker(t, store, 2)
	job, err := store.EnqueueJob(context.Background(), "run-1", repoA)
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if _, err := store.RetryJob(context.Background(), job.ID); err == nil {
		t.Fatal("retrying a queued job must be rejected")
	} else if code := serviceCode(t, err); code != "retry_not_allowed" {
		t.Fatalf("retry code = %q, want retry_not_allowed", code)
	}
}

// ---- cancellation rides the heartbeat (R5) -------------------------------

func TestCancellationRequestedRidesTheHeartbeatResponse(t *testing.T) {
	store, _ := newTestStore(t)
	seedRun(t, store, "run-1", protocol.RunTarget{Repository: repoA, BaseSHA: "pin000"})
	registerTestWorker(t, store, 2)
	claim := claimAndStart(t, store, "run-1", repoA)
	ctx := context.Background()
	response, err := store.Heartbeat(ctx, claim.Attempt.ID, protocol.HeartbeatRequest{LeaseToken: tokenA})
	if err != nil {
		t.Fatalf("heartbeat: %v", err)
	}
	if response.CancellationRequested {
		t.Fatal("no cancellation was requested yet")
	}
	if _, err := store.CancelJob(ctx, claim.Job.ID); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	response, err = store.Heartbeat(ctx, claim.Attempt.ID, protocol.HeartbeatRequest{LeaseToken: tokenA})
	if err != nil {
		t.Fatalf("heartbeat after cancel: %v", err)
	}
	if !response.CancellationRequested {
		t.Fatal("the heartbeat response must carry the cancellation request")
	}
	// The worker, not the server, performs the transition.
	if _, err := store.CompleteAttempt(ctx, claim.Attempt.ID, protocol.CompleteAttemptRequest{
		LeaseToken: tokenA, State: protocol.AttemptCancelled,
	}); err != nil {
		t.Fatalf("worker-confirmed cancellation: %v", err)
	}
	job, err := store.Job(ctx, claim.Job.ID)
	if err != nil {
		t.Fatalf("read job: %v", err)
	}
	if job.State != protocol.JobCancelled {
		t.Fatalf("job state = %q, want cancelled", job.State)
	}
}

// ---- HTTP surface (R20) --------------------------------------------------

func TestAStateChangingRequestCarryingAForeignOriginIsRejected(t *testing.T) {
	store, _ := newTestStore(t)
	handler := NewHandler(store, "ui-token-fixture", nil)

	post := func(origin, token string) *httptest.ResponseRecorder {
		request := httptest.NewRequest("POST", "http://127.0.0.1:8383/api/jobs/missing/retry", nil)
		if origin != "" {
			request.Header.Set("Origin", origin)
		}
		if token != "" {
			request.Header.Set("X-Jig-UI-Token", token)
		}
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		return recorder
	}

	if got := post("http://evil.example", "").Code; got != http.StatusForbidden {
		t.Fatalf("foreign Origin: status %d, want 403", got)
	}
	if body := post("http://evil.example", "").Body.String(); !strings.Contains(body, "cross_origin_request") {
		t.Fatalf("foreign Origin body = %s, want cross_origin_request", body)
	}
	// Same-origin passes the gate (and then 404s on the missing job).
	if got := post("http://127.0.0.1:8383", "").Code; got != http.StatusNotFound {
		t.Fatalf("same-origin: status %d, want 404 past the gate", got)
	}
	// The embedded UI's per-process token substitutes for same-origin.
	if got := post("http://evil.example", "ui-token-fixture").Code; got != http.StatusNotFound {
		t.Fatalf("UI token: status %d, want 404 past the gate", got)
	}
	if got := post("http://evil.example", "wrong-token").Code; got != http.StatusForbidden {
		t.Fatalf("wrong UI token: status %d, want 403", got)
	}
	// An originless client (the worker, curl) is inside the loopback
	// perimeter and passes.
	if got := post("", "").Code; got != http.StatusNotFound {
		t.Fatalf("no Origin: status %d, want 404 past the gate", got)
	}
}

// TestDNSRebindingHostIsRejectedEvenWithAMatchingOrigin covers R20's other
// half: r.Host is attacker-controlled, so a page served from a domain the
// attacker points at 127.0.0.1 makes the browser send both Origin and Host
// as that domain. They agree with each other, but the Host itself must
// still name loopback before that agreement is trusted.
func TestDNSRebindingHostIsRejectedEvenWithAMatchingOrigin(t *testing.T) {
	store, _ := newTestStore(t)
	handler := NewHandler(store, "", nil)

	post := func(host, origin string) *httptest.ResponseRecorder {
		request := httptest.NewRequest("POST", "http://placeholder/api/jobs/missing/retry", nil)
		request.Host = host
		if origin != "" {
			request.Header.Set("Origin", origin)
		}
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		return recorder
	}

	// The rebound domain resolves to the loopback listener, so Origin and
	// Host agree — the old check alone would let this through.
	rebound := post("evil.test:8383", "http://evil.test:8383")
	if rebound.Code != http.StatusForbidden {
		t.Fatalf("DNS-rebound Host: status %d, want 403", rebound.Code)
	}
	if !strings.Contains(rebound.Body.String(), "untrusted_host_authority") {
		t.Fatalf("DNS-rebound Host body = %s, want untrusted_host_authority", rebound.Body.String())
	}
	// A portless rebound name must be caught too.
	if got := post("evil.test", "http://evil.test").Code; got != http.StatusForbidden {
		t.Fatalf("portless DNS-rebound Host: status %d, want 403", got)
	}

	// Loopback Host variants, with and without a port, pass the Host-
	// authority check and reach the (still-required) Origin match, landing
	// on 404 past the gate exactly like a legitimate same-origin request.
	for _, host := range []string{
		"127.0.0.1", "127.0.0.1:8383",
		"[::1]", "[::1]:8383",
		"localhost", "localhost:8383", "LOCALHOST",
	} {
		if got := post(host, "http://"+host).Code; got != http.StatusNotFound {
			t.Fatalf("loopback Host %q: status %d, want 404 past the gate", host, got)
		}
	}
}

func TestBindingToANonLoopbackAddressWithoutTheOptInFlagRefusesToStart(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()
	for _, address := range []string{"0.0.0.0:0", "192.168.1.10:8383", ":8383"} {
		if _, err := NewServer(ctx, store, ServerConfig{Address: address}); err == nil {
			t.Fatalf("NewServer(%q) without AllowNonLoopback must refuse", address)
		}
	}
	// The explicit flag is the only door (R20).
	if _, err := NewServer(ctx, store, ServerConfig{Address: "0.0.0.0:0", AllowNonLoopback: true}); err != nil {
		t.Fatalf("NewServer with AllowNonLoopback: %v", err)
	}
	// The loopback default binds and serves.
	server, err := NewServer(ctx, store, ServerConfig{Address: "127.0.0.1:0"})
	if err != nil {
		t.Fatalf("NewServer loopback: %v", err)
	}
	if err := server.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer server.Shutdown(context.Background())
	if server.UIToken() == "" {
		t.Fatal("the server must mint a per-process UI token")
	}
	response, err := http.Get("http://" + server.Addr().String() + "/api/health")
	if err != nil {
		t.Fatalf("health: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("health status = %d", response.StatusCode)
	}
}

// Startup resolves a name in two places — the R20 loopback refusal and the
// bind itself. Both take the caller's context so a resolver that cannot
// answer promptly is a startup that fails rather than one that hangs.
//
// Only the bind is asserted. Name resolution is deliberately not: a hosts
// file entry for "localhost" is answered without a context-aware lookup on
// platforms using the pure-Go resolver, so NewServer legitimately succeeds
// on a dead context there while the cgo resolver path returns its error.
// Asserting that difference tests the platform's resolver, not jig.
func TestServerStartupBindAnswersToItsContext(t *testing.T) {
	store, _ := newTestStore(t)
	dead, cancel := context.WithCancel(context.Background())
	cancel()

	server, err := NewServer(context.Background(), store, ServerConfig{Address: "localhost:0"})
	if err != nil {
		t.Fatalf("NewServer localhost: %v", err)
	}
	if err := server.Start(dead); err == nil {
		server.Shutdown(context.Background())
		t.Fatal("Start bound the listener without consulting its context")
	}
}

func TestWorkerRoutesDriveAFullClaimHeartbeatCompleteCycleOverHTTP(t *testing.T) {
	store, _ := newTestStore(t)
	seedRun(t, store, "run-1", protocol.RunTarget{Repository: repoA, BaseSHA: "pin000"})
	handler := NewHandler(store, "", nil)
	server := httptest.NewServer(handler)
	defer server.Close()

	do := func(method, path, body string) (*http.Response, error) {
		request, err := http.NewRequest(method, server.URL+path, strings.NewReader(body))
		if err != nil {
			return nil, err
		}
		if body != "" {
			request.Header.Set("Content-Type", "application/json")
		}
		return http.DefaultClient.Do(request)
	}
	expectStatus := func(t *testing.T, response *http.Response, err error, want int) {
		t.Helper()
		if err != nil {
			t.Fatalf("request: %v", err)
		}
		defer response.Body.Close()
		if response.StatusCode != want {
			t.Fatalf("status = %d, want %d", response.StatusCode, want)
		}
	}

	registration := `{"name":"local","worker_version":"dev","capacity":2,` +
		`"env_names":["GITHUB_TOKEN","PATH"],"runtimes":[],"active_count":0,"retained_worktrees":[]}`
	response, err := do("PUT", "/api/workers/"+workerA, registration)
	expectStatus(t, response, err, http.StatusOK)

	job, err := store.EnqueueJob(context.Background(), "run-1", repoA)
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	response, err = do("POST", "/api/workers/"+workerA+"/claims",
		fmt.Sprintf(`{"request_id":"req-http","lease_token":%q}`, tokenA))
	expectStatus(t, response, err, http.StatusOK)

	claim, err := store.Claim(context.Background(), workerA, protocol.ClaimRequest{
		RequestID: "req-http", LeaseToken: tokenA,
	})
	if err != nil || claim == nil || claim.Job.ID != job.ID {
		t.Fatalf("replayed claim = %v, %v", claim, err)
	}

	response, err = do("POST", "/api/attempts/"+claim.Attempt.ID+"/start",
		fmt.Sprintf(`{"lease_token":%q}`, tokenA))
	expectStatus(t, response, err, http.StatusOK)

	response, err = do("PUT", "/api/attempts/"+claim.Attempt.ID+"/heartbeat",
		fmt.Sprintf(`{"lease_token":%q}`, tokenA))
	expectStatus(t, response, err, http.StatusOK)

	// Publish before acceptance: `accepted` is refused without a proof record
	// (R12), and the worker's real cycle records the steps at this point too.
	branch := protocol.PublishBranch(claim.Job.ID, claim.Attempt.AttemptNumber)
	for _, body := range []string{
		fmt.Sprintf(`{"lease_token":%q,"step":"push","branch":%q,"remote_ref":%q}`, tokenA, branch, shaA),
		fmt.Sprintf(`{"lease_token":%q,"step":"pull_request","branch":%q,`+
			`"pr_url":"https://github.com/example/repo-a/pull/7"}`, tokenA, branch),
		fmt.Sprintf(`{"lease_token":%q,"step":"proof","branch":%q,"remote_ref":%q}`, tokenA, branch, shaA),
	} {
		response, err = do("POST", "/api/attempts/"+claim.Attempt.ID+"/publish/record", body)
		expectStatus(t, response, err, http.StatusOK)
	}

	response, err = do("POST", "/api/attempts/"+claim.Attempt.ID+"/complete",
		fmt.Sprintf(`{"lease_token":%q,"state":"accepted","result":"done"}`, tokenA))
	expectStatus(t, response, err, http.StatusOK)

	final, err := store.Job(context.Background(), job.ID)
	if err != nil {
		t.Fatalf("read job: %v", err)
	}
	if final.State != protocol.JobAccepted {
		t.Fatalf("job state over HTTP cycle = %q, want accepted", final.State)
	}
}

// ---- vocabulary lockstep -------------------------------------------------

// The partial unique index's WHERE clause must treat exactly
// protocol.ActiveAttemptStates as "active" (U1 contract). This test fails if
// either side changes without the other.
func TestActiveAttemptStatesStayInLockstepWithThePartialIndex(t *testing.T) {
	store, _ := newTestStore(t)
	var indexSQL string
	if err := store.db.QueryRow(`
		SELECT sql FROM sqlite_master WHERE type = 'index' AND name = 'one_active_attempt_per_job'
	`).Scan(&indexSQL); err != nil {
		t.Fatalf("read index definition: %v", err)
	}
	for _, state := range protocol.ActiveAttemptStates {
		if !strings.Contains(indexSQL, "'"+state+"'") {
			t.Fatalf("index %q does not cover active state %q", indexSQL, state)
		}
	}
	quoted := 0
	for _, state := range []string{"queued", "preparing", "running", "accepted",
		"accepted_unpublished", "failed", "cancelled", "lost"} {
		if strings.Contains(indexSQL, "'"+state+"'") {
			quoted++
		}
	}
	if quoted != len(protocol.ActiveAttemptStates) {
		t.Fatalf("index covers %d states, protocol declares %d active", quoted, len(protocol.ActiveAttemptStates))
	}
}
