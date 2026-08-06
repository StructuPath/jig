// embedded.go — the serverless direct-run path behind `jig run` (U11).
//
// A direct run is the server path with the HTTP layer removed, never a
// parallel implementation: it opens the same store, freezes the same run
// shape (definition snapshot by value, one target pinned at the local
// repo's current HEAD), enqueues a job, and then drives the attempt through
// the SAME fenced transactions the worker would — RegisterWorker, Claim,
// StartAttempt, Heartbeat, CompleteAttempt. Events land in the `events`
// table under the per-attempt monotonic seq, so a direct run's record is
// indistinguishable in shape from a server-path run (the U11 scenario:
// same tables, seq-ordered).
//
// The phase engine itself is injected as the Execute callback (wired by
// cmd/jig/run.go): the control plane stays engine-free, the same KTD1
// layering the boundary check enforces from the worker's side — and the
// worker's in-package integration tests can keep importing this package
// without an import cycle.
//
// Direct runs end at predicate evaluation: no standing server, no worker
// loop, no publish. The engine's Outcome.Result already carries the
// explicit no-publish marker ("publish": "not_attempted").
package controlplane

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/StructuPath/jig/internal/protocol"
)

// directWorkerID is the single implicit worker a direct run registers as.
// It is a stable id so repeated `jig run` invocations against one data dir
// upsert the same worker row instead of accumulating ghosts (KTD12).
const directWorkerID = "embedded-direct"

// directWorkerCapacity is how many direct runs may hold an attempt against
// one data directory at once. All `jig run` processes share the embedded
// worker row (KTD12), so its capacity is the concurrency of the whole direct
// path, not of one process: at capacity 1 a single abandoned attempt made
// every later run unclaimable. Abandoned slots are reclaimed at startup
// (reclaimAbandonedDirectRuns), so the capacity can never wedge either.
const directWorkerCapacity = 8

// maxDirectClaimAttempts bounds the claim loop that skips over queued jobs
// an interrupted earlier direct run left behind.
const maxDirectClaimAttempts = 8

// directClaimLockTimeout bounds the wait for the data directory's
// enqueue-and-claim lock. Concurrent direct runs hold it for one enqueue plus
// one claim, so anything near this bound is a stuck process, not contention.
const directClaimLockTimeout = 60 * time.Second

// directClaimLockPoll is how often a waiter retries the claim lock.
const directClaimLockPoll = 25 * time.Millisecond

// directAttemptStaleAfter is how long past its lease expiry a leased
// embedded-worker attempt with NO liveness marker must be before startup
// reclaims it. Markers cover every run this build starts; the wall clock is
// the backstop for an attempt whose marker was never written or was deleted
// out from under it.
const directAttemptStaleAfter = 4 * protocol.LeaseDuration

// DirectOutcome is what the injected executor declares when the chain
// finishes — the worker Outcome shape without the import (KTD1). State must
// be a worker-declarable terminal attempt state.
type DirectOutcome struct {
	State  string
	Result string
	Error  string
}

// DirectExecution is everything the injected executor needs to run the
// frozen chain: the claim (snapshot, parameters, attempt identity), the
// trace location, the store-side event persister, and the lease freshener.
type DirectExecution struct {
	Claim protocol.Claim
	// TracePath is where the attempt-local JSONL trace belongs; the
	// executor opens and owns it.
	TracePath string
	// Persist records one trace event into the events table, keyed on the
	// per-attempt monotonic seq (KTD8). Replay is benign.
	Persist func(event protocol.Event) error
	// FreshenLease heartbeats the attempt's lease before fenced writes.
	FreshenLease func(ctx context.Context) error
}

// DirectRunConfig wires one direct run. Source, RepoPath, HeadSHA, DataDir,
// Capability, and Execute are required; the rest defaults.
type DirectRunConfig struct {
	// DataDir holds the store (jig.db) and traces/ families.
	DataDir string
	// Source is the definition YAML, frozen by value into the run snapshot.
	// It must already validate; DirectRun re-checks and refuses if not.
	Source []byte
	// RepoPath is the absolute local repository the chain executes against —
	// direct runs use the repo in place, no worktree materialization.
	RepoPath string
	// HeadSHA is the repo's current HEAD, pinned as the run target's base.
	HeadSHA string
	// Parameters are the invocation inputs ({{name}} prompt substitutions).
	Parameters map[string]string
	// Capability is the probed runtime record, advertised in the worker
	// registration and stamped on the attempt (KTD4).
	Capability protocol.RuntimeCapability
	// BaseEnvNames are the environment variable NAMES available to the run
	// (never values, R17); nil means the names of os.Environ().
	BaseEnvNames []string
	// Execute runs the frozen chain — the engine, wired by the caller — and
	// returns the terminal outcome.
	Execute func(ctx context.Context, execution DirectExecution) DirectOutcome
	Logger  *slog.Logger
}

// DirectRunResult is what `jig run` reports: the terminal attempt (its
// Result carries phases, acceptance evidence, changed paths, and the
// no-publish marker), its job, the frozen run's id, and the trace location.
type DirectRunResult struct {
	RunID     string
	Job       protocol.Job
	Attempt   protocol.Attempt
	TracePath string
}

// DirectRun executes one definition against one local repository through
// the embedded store and returns the terminal attempt. An error return
// means the run infrastructure failed; a failed ATTEMPT is a successful
// DirectRun whose Attempt.State says failed.
func DirectRun(ctx context.Context, config DirectRunConfig) (DirectRunResult, error) {
	var zero DirectRunResult
	spec, err := protocol.ParseDefinition(config.Source)
	if err != nil {
		return zero, err
	}
	if config.Execute == nil {
		return zero, errors.New("direct run: an executor is required")
	}
	if config.Capability.Name == "" {
		return zero, errors.New("direct run: a probed runtime capability is required")
	}
	if config.RepoPath == "" || config.HeadSHA == "" {
		return zero, errors.New("direct run: repository path and HEAD SHA are required")
	}
	if config.DataDir == "" {
		return zero, errors.New("direct run: data directory is required")
	}
	if config.BaseEnvNames == nil {
		config.BaseEnvNames = environNames(os.Environ())
	}
	if config.Logger == nil {
		config.Logger = slog.Default()
	}

	tracesDir := filepath.Join(config.DataDir, "traces")
	markerDir := directMarkerDir(config.DataDir)
	for _, dir := range []string{config.DataDir, tracesDir, markerDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return zero, fmt.Errorf("create data directory: %w", err)
		}
	}
	store, err := Open(ctx, filepath.Join(config.DataDir, "jig.db"))
	if err != nil {
		return zero, err
	}
	defer store.Close()

	// Completion must survive cancellation: a Ctrl-C'd run still has to leave
	// its attempt terminal, or the next run inherits a wedged data directory.
	// The store writes after execution therefore ride a context detached from
	// the caller's (R5's "an attempt is never left leased by a live process").
	completionCtx := context.WithoutCancel(ctx)

	// Env eligibility is checked here with a named answer instead of letting
	// the claim transaction silently skip the only job (R17): the operator
	// learns WHICH variable is missing, before any phase runs.
	available := make(map[string]bool, len(config.BaseEnvNames))
	for _, name := range config.BaseEnvNames {
		available[name] = true
	}
	var missing []string
	for _, name := range spec.RequiredEnvNames() {
		if !available[name] {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		return zero, fmt.Errorf("definition %q requires environment variables not present: %s",
			spec.Name, strings.Join(missing, ", "))
	}

	// Enqueue-and-claim is a critical section across direct runs against one
	// data directory. Serializing it is what makes the orphan heuristic below
	// safe: while the lock is held, every OTHER queued job is either a job an
	// interrupted run abandoned (its liveness marker has no holder) or a job
	// this path did not create at all (no marker — the server path's).
	claimLock, err := lockDirectClaim(ctx, config.DataDir)
	if err != nil {
		return zero, err
	}
	claimLockHeld := true
	releaseClaimLock := func() {
		if claimLockHeld {
			claimLockHeld = false
			claimLock.release()
		}
	}
	defer releaseClaimLock()

	// Self-healing (R5's direct-path half): an ungraceful death — Ctrl-C
	// before the signal handler, kill -9, a panic — leaves an attempt leased
	// by a process that no longer exists. Startup reclaims exactly those,
	// evidenced by a liveness marker with no live holder, so the data
	// directory can never wedge permanently.
	if reclaimed, err := store.reclaimAbandonedDirectRuns(ctx, config.DataDir); err != nil {
		// Reclamation is hygiene, not the run: a failure is surfaced and the
		// run proceeds (it will simply see the slot still taken).
		config.Logger.Warn("direct_run_reclaim_failed", "error", err)
	} else {
		for _, value := range reclaimed {
			config.Logger.Info("state_change", "resource_type", "attempt",
				"resource_id", value.AttemptID, "job_id", value.JobID, "new_state", protocol.AttemptLost)
		}
	}

	if _, err := store.RegisterWorker(ctx, directWorkerID, protocol.WorkerRegistration{
		Name:          "embedded direct runner",
		WorkerVersion: "jig-run",
		Capacity:      directWorkerCapacity,
		EnvNames:      config.BaseEnvNames,
		Runtimes:      []protocol.RuntimeCapability{config.Capability},
	}); err != nil {
		return zero, err
	}

	// Authoring, freezing, and fan-out are U5's, used here unchanged: a
	// direct run is a one-target admission, not a parallel implementation of
	// one (the definition upserts by name, the run freezes by value, the
	// single job is the fan-out of a single target).
	definition, err := store.saveDefinition(ctx, string(config.Source))
	if err != nil {
		return zero, err
	}
	targets, err := store.resolveTargets(ctx, []InvocationTarget{
		{Repository: config.RepoPath, BaseSHA: config.HeadSHA},
	})
	if err != nil {
		return zero, err
	}
	view, err := store.admitRun(ctx, preparedInvocation{
		definitionID: definition.ID,
		generation:   definition.Generation,
		snapshot:     string(config.Source),
		parameters:   config.Parameters,
		targets:      targets,
	})
	if err != nil {
		return zero, err
	}
	runID := view.Run.ID
	job := view.Jobs[0]
	// The marker publishes this process as the job's live owner before the
	// job can ever be seen queued by another run, and holds until this run
	// exits — however it exits.
	marker, err := acquireDirectMarker(config.DataDir, job.ID)
	if err != nil {
		store.abandonQueuedDirectJob(completionCtx, job.ID, config.Logger)
		return zero, err
	}
	defer marker.release()

	claim, token, err := store.claimDirectJob(ctx, config.DataDir, job.ID)
	if err != nil {
		// Never leave our own job queued behind a failed claim: an unowned
		// queued job is exactly the leak that made repeated runs pile up.
		store.abandonQueuedDirectJob(completionCtx, job.ID, config.Logger)
		return zero, err
	}
	releaseClaimLock()
	attemptID := claim.Attempt.ID
	if _, err := store.StartAttempt(ctx, attemptID, protocol.StartAttemptRequest{
		LeaseToken:     token,
		RuntimeName:    config.Capability.Name,
		RuntimeVersion: config.Capability.Version,
	}); err != nil {
		return zero, err
	}

	// The lease still fences the direct path (R6): CompleteAttempt refuses an
	// expired lease, and real-CLI phases run far longer than LeaseDuration, so
	// a background heartbeat renews it — the same rhythm the worker keeps. It
	// rides the detached context so an interrupted run keeps its lease alive
	// through teardown and can still record its own cancellation.
	beatCtx, stopBeats := context.WithCancel(completionCtx)
	defer stopBeats()
	go func() {
		ticker := time.NewTicker(protocol.HeartbeatInterval)
		defer ticker.Stop()
		for {
			select {
			case <-beatCtx.Done():
				return
			case <-ticker.C:
				if _, err := store.Heartbeat(beatCtx, attemptID,
					protocol.HeartbeatRequest{LeaseToken: token}); err != nil {
					config.Logger.Warn("direct_run_heartbeat_failed", "error", err)
				}
			}
		}
	}()

	tracePath := filepath.Join(tracesDir, attemptID+".jsonl")
	outcome := config.Execute(ctx, DirectExecution{
		Claim:     *claim,
		TracePath: tracePath,
		Persist: func(event protocol.Event) error {
			// Detached for the same reason completion is: the events an
			// interrupted attempt emits while unwinding — rollback, boundary
			// enforcement, the terminal phase events — are exactly the ones
			// the record needs, and on the caller's cancelled context every
			// one of them would fail to reach the events table while still
			// reaching the JSONL trace.
			return store.AppendEvents(completionCtx, attemptID, []protocol.Event{event})
		},
		FreshenLease: func(ctx context.Context) error {
			_, err := store.Heartbeat(ctx, attemptID, protocol.HeartbeatRequest{LeaseToken: token})
			return err
		},
	})
	stopBeats()

	// An interrupted run still owes the store a verdict. Cancellation is the
	// honest one when the executor could not declare a terminal state itself.
	if ctx.Err() != nil && !isWorkerDeclarableState(outcome.State) {
		outcome.State = protocol.AttemptCancelled
		if outcome.Error == "" {
			outcome.Error = "the direct run was interrupted: " + ctx.Err().Error()
		}
	}

	attempt, err := store.CompleteAttempt(completionCtx, attemptID, protocol.CompleteAttemptRequest{
		LeaseToken: token,
		State:      outcome.State,
		Result:     outcome.Result,
		Error:      outcome.Error,
	})
	if err != nil {
		return zero, fmt.Errorf("record outcome (engine said %s): %w", outcome.State, err)
	}
	// The run's state is not written here: CompleteAttempt already aggregated
	// it from this job inside the same fenced transaction (R12, U5). A direct
	// run has no second rule of its own — an `accepted_unpublished` job makes
	// its run `mixed`, and the attempt's no-publish marker says why.
	job, err = store.Job(completionCtx, job.ID)
	if err != nil {
		return zero, err
	}
	return DirectRunResult{RunID: runID, Job: job, Attempt: attempt, TracePath: tracePath}, nil
}

// isWorkerDeclarableState reports whether a state is one a worker (or the
// injected executor) may declare at completion — the CompleteAttempt
// vocabulary, which excludes `lost` (only sweep declares that).
func isWorkerDeclarableState(state string) bool {
	switch state {
	case protocol.AttemptAccepted, protocol.AttemptAcceptedUnpublished,
		protocol.AttemptFailed, protocol.AttemptCancelled:
		return true
	}
	return false
}

// claimDirectJob claims until it holds THE job this run enqueued. FIFO can
// hand an older queued job over first; what happens to that job depends on
// evidence, never on assumption:
//
//   - a job whose liveness marker has no holder was abandoned by an
//     interrupted direct run — it is failed explicitly, with its cause;
//   - anything else (another live run's job, a server-path job with no
//     marker at all) is put back exactly as it was found and the claim is
//     abandoned. A direct run never destroys work it cannot prove is dead.
func (s *Store) claimDirectJob(ctx context.Context, dataDir, jobID string) (*protocol.Claim, string, error) {
	for range maxDirectClaimAttempts {
		token, err := mintDirectLeaseToken()
		if err != nil {
			return nil, "", err
		}
		requestID, err := newID()
		if err != nil {
			return nil, "", unavailable(err)
		}
		claim, err := s.Claim(ctx, directWorkerID, protocol.ClaimRequest{
			RequestID:  requestID,
			LeaseToken: token,
		})
		if err != nil {
			return nil, "", err
		}
		if claim == nil {
			return nil, "", errors.New(
				"the embedded worker could not claim the enqueued job (nothing eligible)")
		}
		if claim.Job.ID == jobID {
			return claim, token, nil
		}
		owned, err := directRunAlive(dataDir, claim.Job.ID)
		if err != nil {
			return nil, "", err
		}
		if owned {
			if releaseErr := s.releaseDirectClaim(ctx, claim.Attempt.ID, token); releaseErr != nil {
				return nil, "", releaseErr
			}
			return nil, "", fmt.Errorf(
				"job %s is queued ahead of this run and owned by another live run or the server path; "+
					"it was released untouched — retry once it is claimed", claim.Job.ID)
		}
		if _, err := s.CompleteAttempt(ctx, claim.Attempt.ID, protocol.CompleteAttemptRequest{
			LeaseToken: token,
			State:      protocol.AttemptFailed,
			Error:      "queued job abandoned by an interrupted direct run; failed by a later direct run",
		}); err != nil {
			return nil, "", err
		}
		if err := removeDirectMarker(dataDir, claim.Job.ID); err != nil {
			return nil, "", err
		}
	}
	return nil, "", fmt.Errorf("could not claim job %s within %d claims", jobID, maxDirectClaimAttempts)
}

// releaseDirectClaim puts a claim back: the attempt returns to `queued` with
// no worker and no lease, and its job returns to `queued`. Both writes are
// guarded on the state and the lease digest we hold, so a release can never
// disturb an attempt someone else already moved on.
func (s *Store) releaseDirectClaim(ctx context.Context, attemptID, token string) error {
	now := s.now().UnixMilli()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return unavailable(err)
	}
	defer tx.Rollback()
	var jobID string
	if err := tx.QueryRowContext(ctx,
		`SELECT job_id FROM attempts WHERE id = ?`, attemptID).Scan(&jobID); err != nil {
		return unavailable(err)
	}
	result, err := tx.ExecContext(ctx, `
		UPDATE attempts SET state = 'queued', worker_id = NULL, lease_digest = NULL, lease_expires_at = NULL
		WHERE id = ? AND state = 'preparing' AND lease_digest = ?
	`, attemptID, digestToken(token))
	if err != nil {
		return unavailable(err)
	}
	if changed, _ := result.RowsAffected(); changed != 1 {
		return conflict("release_conflict", "the claim to release is no longer ours to release")
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE jobs SET state = 'queued', updated_at = ? WHERE id = ? AND state = 'active'
	`, now, jobID); err != nil {
		return unavailable(err)
	}
	if err := tx.Commit(); err != nil {
		return unavailable(err)
	}
	return nil
}

// abandonQueuedDirectJob cancels a job this run enqueued but never claimed.
// Without it, every failed claim would leave one more unowned queued job in
// the store for the next run to trip over.
func (s *Store) abandonQueuedDirectJob(ctx context.Context, jobID string, logger *slog.Logger) {
	if _, err := s.CancelJob(ctx, jobID); err != nil {
		logger.Warn("direct_run_job_abandon_failed", "job_id", jobID, "error", err)
	}
}

// ---- direct-run liveness ---------------------------------------------------
//
// A direct run's liveness is a flock-held marker file named for the job it
// enqueued. The lock lives on the open file description, so it is released by
// the kernel however the process dies — SIGKILL included — which is what lets
// a later run tell "still running" from "abandoned" without trusting a pid
// (pids recycle; a held flock does not).

// directMarkerDir is where per-job liveness markers live.
func directMarkerDir(dataDir string) string { return filepath.Join(dataDir, "direct-runs") }

func directMarkerPath(dataDir, jobID string) string {
	return filepath.Join(directMarkerDir(dataDir), jobID+".lock")
}

// directMarker is one held liveness marker.
type directMarker struct {
	file *os.File
	path string
}

// acquireDirectMarker creates and locks this run's marker. A marker that is
// already held means an id collision, which cannot happen for a freshly
// minted job id — so it is an error, never a silent overwrite.
func acquireDirectMarker(dataDir, jobID string) (*directMarker, error) {
	if err := os.MkdirAll(directMarkerDir(dataDir), 0o700); err != nil {
		return nil, fmt.Errorf("create direct-run marker directory: %w", err)
	}
	path := directMarkerPath(dataDir, jobID)
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("create direct-run marker: %w", err)
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		file.Close()
		return nil, fmt.Errorf("hold direct-run marker %s: %w", path, err)
	}
	if err := file.Truncate(0); err == nil {
		_, _ = file.WriteString(strconv.Itoa(os.Getpid()) + "\n")
	}
	return &directMarker{file: file, path: path}, nil
}

// release drops the marker. Closing alone would suffice for the lock; the
// file goes too so a data directory does not accumulate dead markers.
func (m *directMarker) release() {
	if m == nil || m.file == nil {
		return
	}
	_ = os.Remove(m.path)
	_ = m.file.Close()
}

func removeDirectMarker(dataDir, jobID string) error {
	if err := os.Remove(directMarkerPath(dataDir, jobID)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove direct-run marker: %w", err)
	}
	return nil
}

// directRunAlive reports whether a live process still owns the job's marker.
// A missing marker answers TRUE — "not provably dead": jobs this path never
// created (the server path's) have no marker, and destroying them is exactly
// the mistake this function exists to prevent.
func directRunAlive(dataDir, jobID string) (bool, error) {
	file, err := os.OpenFile(directMarkerPath(dataDir, jobID), os.O_RDWR, 0o600)
	if errors.Is(err, os.ErrNotExist) {
		return true, nil
	}
	if err != nil {
		return true, fmt.Errorf("inspect direct-run marker: %w", err)
	}
	defer file.Close()
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return true, nil
		}
		return true, fmt.Errorf("test direct-run marker: %w", err)
	}
	// We took the lock, so nobody held it: the owner is gone.
	_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
	return false, nil
}

// directClaimLock is the data directory's enqueue-and-claim mutex.
type directClaimLock struct{ file *os.File }

func lockDirectClaim(ctx context.Context, dataDir string) (*directClaimLock, error) {
	path := filepath.Join(dataDir, "direct-claim.lock")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open direct claim lock: %w", err)
	}
	deadline := time.Now().Add(directClaimLockTimeout)
	for {
		err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return &directClaimLock{file: file}, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			file.Close()
			return nil, fmt.Errorf("take direct claim lock: %w", err)
		}
		if ctx.Err() != nil {
			file.Close()
			return nil, ctx.Err()
		}
		if time.Now().After(deadline) {
			file.Close()
			return nil, fmt.Errorf(
				"another direct run held the claim lock on %s for longer than %s", dataDir, directClaimLockTimeout)
		}
		time.Sleep(directClaimLockPoll)
	}
}

func (l *directClaimLock) release() {
	if l == nil || l.file == nil {
		return
	}
	_ = syscall.Flock(int(l.file.Fd()), syscall.LOCK_UN)
	_ = l.file.Close()
}

// ---- startup reclamation ---------------------------------------------------

// reclaimAbandonedDirectRuns marks terminal every job of this data directory
// whose direct run is provably gone: its liveness marker exists with no
// holder, or (the backstop) its attempt is leased by the embedded worker,
// carries no marker at all, and its lease has been expired far longer than
// any live run would allow. Jobs with a live owner and jobs this path never
// created are never touched.
func (s *Store) reclaimAbandonedDirectRuns(ctx context.Context, dataDir string) ([]LostAttempt, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT a.id, a.job_id, a.worker_id, a.lease_expires_at
		FROM attempts a JOIN jobs j ON j.id = a.job_id
		WHERE a.state IN ('queued', 'preparing', 'running') AND j.state IN ('queued', 'active')
	`)
	if err != nil {
		return nil, unavailable(err)
	}
	type candidate struct {
		attemptID, jobID string
		workerID         sql.NullString
		expiry           sql.NullInt64
	}
	var candidates []candidate
	for rows.Next() {
		var value candidate
		if err := rows.Scan(&value.attemptID, &value.jobID, &value.workerID, &value.expiry); err != nil {
			rows.Close()
			return nil, unavailable(err)
		}
		candidates = append(candidates, value)
	}
	if err := rows.Close(); err != nil {
		return nil, unavailable(err)
	}
	// A truncated candidate list is indistinguishable from a complete one
	// unless the iteration error is asked for: reclamation would silently
	// skip the abandoned attempts it exists to find.
	if err := rows.Err(); err != nil {
		return nil, unavailable(err)
	}

	staleBefore := s.now().Add(-directAttemptStaleAfter).UnixMilli()
	var reclaimed []LostAttempt
	var failures []error
	for _, value := range candidates {
		_, markerErr := os.Stat(directMarkerPath(dataDir, value.jobID))
		reason := ""
		switch {
		case markerErr == nil:
			alive, err := directRunAlive(dataDir, value.jobID)
			if err != nil {
				failures = append(failures, err)
				continue
			}
			if alive {
				continue
			}
			reason = "the direct run that owned this job is gone (its liveness marker has no holder)"
		case errors.Is(markerErr, os.ErrNotExist):
			// No marker: only the embedded worker's own leased attempts are
			// ours to judge, and only on wall-clock evidence.
			if value.workerID.String != directWorkerID || !value.expiry.Valid || value.expiry.Int64 > staleBefore {
				continue
			}
			reason = fmt.Sprintf(
				"the embedded worker's attempt has been leased with no live owner for more than %s", directAttemptStaleAfter)
		default:
			failures = append(failures, fmt.Errorf("inspect direct-run marker: %w", markerErr))
			continue
		}
		if err := s.loseAbandonedAttempt(ctx, value.attemptID, value.jobID, reason); err != nil {
			failures = append(failures, err)
			continue
		}
		if err := removeDirectMarker(dataDir, value.jobID); err != nil {
			failures = append(failures, err)
		}
		reclaimed = append(reclaimed, LostAttempt{AttemptID: value.attemptID, JobID: value.jobID})
	}
	return reclaimed, errors.Join(failures...)
}

// loseAbandonedAttempt marks one abandoned attempt `lost` and its job and run
// failed, in one transaction. `lost` — not `failed` — because no worker token
// declared this outcome: the same vocabulary sweep uses server-side (R5).
func (s *Store) loseAbandonedAttempt(ctx context.Context, attemptID, jobID, reason string) error {
	now := s.now().UnixMilli()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return unavailable(err)
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `
		UPDATE attempts SET state = 'lost', error = ?, completed_at = ?
		WHERE id = ? AND state IN ('queued', 'preparing', 'running')
	`, boundedError(reason), now, attemptID)
	if err != nil {
		return unavailable(err)
	}
	if changed, _ := result.RowsAffected(); changed != 1 {
		// The attempt moved on between our read and this write: leave it alone.
		return tx.Commit()
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE jobs SET state = 'failed', updated_at = ? WHERE id = ? AND state IN ('queued', 'active')
	`, now, jobID); err != nil {
		return unavailable(err)
	}
	if err := applyRunAggregationForJob(ctx, tx, jobID, now); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return unavailable(err)
	}
	return nil
}

func boundedError(value string) string {
	if len(value) > protocol.MaxErrorBytes {
		return value[:protocol.MaxErrorBytes]
	}
	return value
}

// mintDirectLeaseToken mints the embedded worker's fencing token — same
// contract as the real worker's: random, and stored server-side only as a
// digest.
func mintDirectLeaseToken() (string, error) {
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("mint lease token: %w", err)
	}
	return hex.EncodeToString(raw[:]), nil
}

// AppendEvents persists trace events under the per-attempt monotonic seq
// (KTD8). The stored payload is the FULL event JSON — type, phase, name,
// payload, timestamps — so a database row reconstructs the exact JSONL
// line; INSERT OR IGNORE on the (attempt_id, seq) primary key makes replay
// after an outage land exactly once. U8's HTTP ingestion feeds this same
// method in batches.
func (s *Store) AppendEvents(ctx context.Context, attemptID string, events []protocol.Event) error {
	if len(events) == 0 {
		return nil
	}
	if len(events) > protocol.MaxEventsPerBatch {
		return invalid("batch_too_large",
			fmt.Sprintf("event batches are capped at %d", protocol.MaxEventsPerBatch))
	}
	now := s.now().UnixMilli()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return unavailable(err)
	}
	defer tx.Rollback()
	for _, event := range events {
		body, err := json.Marshal(event)
		if err != nil {
			return unavailable(err)
		}
		result, err := tx.ExecContext(ctx, `
			INSERT OR IGNORE INTO events(attempt_id, seq, type, phase, payload, payload_bytes, server_time)
			VALUES (?, ?, ?, ?, ?, ?, ?)
		`, attemptID, event.Seq, event.Type, event.Phase, body, len(body), now)
		if err != nil {
			return unavailable(err)
		}
		// Project only what this call actually inserted, so a replay adds no
		// second copy of the evidence. Direct runs reach the same gate and
		// envelope views as the worker path (ingest.go) rather than degrading
		// to the attempt result alone.
		inserted, err := result.RowsAffected()
		if err != nil {
			return unavailable(err)
		}
		if inserted == 0 {
			continue
		}
		if err := projectEvidence(ctx, tx, attemptID, event, now); err != nil {
			return unavailable(err)
		}
	}
	if err := tx.Commit(); err != nil {
		return unavailable(err)
	}
	return nil
}

// AttemptEvents reads an attempt's stored trace in seq order — the read
// half U9's UI cursors page over.
func (s *Store) AttemptEvents(ctx context.Context, attemptID string) ([]protocol.Event, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT payload FROM events WHERE attempt_id = ? ORDER BY seq
	`, attemptID)
	if err != nil {
		return nil, unavailable(err)
	}
	defer rows.Close()
	var events []protocol.Event
	for rows.Next() {
		var body []byte
		if err := rows.Scan(&body); err != nil {
			return nil, unavailable(err)
		}
		var event protocol.Event
		if err := json.Unmarshal(body, &event); err != nil {
			return nil, unavailable(err)
		}
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		return nil, unavailable(err)
	}
	return events, nil
}

// environNames extracts the variable NAMES (never values, R17) a direct
// run's worker registration advertises.
func environNames(base []string) []string {
	var names []string
	for _, entry := range base {
		name, _, found := strings.Cut(entry, "=")
		if found && name != "" {
			names = append(names, name)
		}
	}
	return names
}
