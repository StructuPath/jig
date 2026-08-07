// store.go — store lifecycle, typed errors, and the fenced attempt/job state
// transitions (U2). Every state change is one transaction that validates the
// lease digest inside it, paired with the schema constraints from
// migrations/001_core.sql — the transaction+constraint pairing is the whole
// reliability story (KTD3, R6).
package controlplane

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/StructuPath/jig/internal/protocol"
	_ "modernc.org/sqlite"
)

// ErrNotFound is the one shared not-found error; handlers map it to 404.
var ErrNotFound = &ServiceError{Code: "not_found", Message: "resource not found", Status: 404}

// ServiceError is the typed error every store method returns: a stable code
// the worker can branch on, a human message, and the HTTP status the handler
// writes.
type ServiceError struct {
	Code    string
	Message string
	Status  int
	Err     error
}

func (e *ServiceError) Error() string {
	if e.Err != nil {
		return e.Message + ": " + e.Err.Error()
	}
	return e.Message
}

func (e *ServiceError) Unwrap() error { return e.Err }

func invalid(code, message string) error {
	return &ServiceError{Code: code, Message: message, Status: 400}
}

func conflict(code, message string) error {
	return &ServiceError{Code: code, Message: message, Status: 409}
}

func unavailable(err error) error {
	return &ServiceError{Code: "storage_unavailable", Message: "storage is unavailable", Status: 503, Err: err}
}

// Store is the control-plane state machine over one SQLite database.
type Store struct {
	db *sql.DB
	// now is injectable so tests can simulate sleeps and clock jumps.
	now func() time.Time
	// resolveRef is the base-SHA resolver, injectable for the same reason:
	// pinning touches the network (KTD9). Nil means the real one.
	resolveRef func(ctx context.Context, identity, ref string) (string, error)
}

const sqlitePragmas = "_pragma=busy_timeout%285000%29&_pragma=foreign_keys%281%29&_pragma=journal_mode%28WAL%29&_txlock=immediate"

// Open opens (creating if necessary) the control-plane database at path with
// factory's operating pragmas — WAL journaling, foreign keys on, a busy
// timeout so concurrent writers queue instead of failing, immediate
// transactions so write intent is declared at BEGIN — a bounded connection
// pool, and all migrations applied.
func Open(ctx context.Context, path string) (*Store, error) {
	if path == "" {
		return nil, errors.New("database path is required")
	}
	dsn := "file::memory:?cache=shared&" + sqlitePragmas
	if path != ":memory:" {
		absolute, err := filepath.Abs(path)
		if err != nil {
			return nil, fmt.Errorf("resolve database path: %w", err)
		}
		fileURL := url.URL{Scheme: "file", Path: absolute}
		dsn = fileURL.String() + "?" + sqlitePragmas
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	db.SetMaxOpenConns(8)
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("ping sqlite: %w", err)
	}
	if err := Migrate(ctx, db); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db, now: time.Now}, nil
}

// Close releases the database.
func (s *Store) Close() error { return s.db.Close() }

// ---- lease-token helpers -------------------------------------------------
//
// Lease tokens are minted by workers and stored server-side only as SHA-256
// digests, on attempts and claim_requests alike — a database read never
// yields a usable fencing token (U1 contract).

func validateLeaseToken(token string) error {
	if len(token) < 32 || len(token) > 1024 {
		return invalid("invalid_lease_token", "lease_token must contain between 32 and 1024 bytes")
	}
	return nil
}

func digestToken(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}

func equalDigest(a, b []byte) bool { return len(a) > 0 && bytes.Equal(a, b) }

func newID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}

// ---- state vocabulary helpers --------------------------------------------

// isLeasedState reports whether an attempt currently holds a lease. This is
// the strict subset of protocol.ActiveAttemptStates that a claim has filled
// in: `queued` is active for the one-active-attempt index but holds no lease.
func isLeasedState(state string) bool {
	return state == protocol.AttemptPreparing || state == protocol.AttemptRunning
}

func isTerminalAttemptState(state string) bool {
	switch state {
	case protocol.AttemptAccepted, protocol.AttemptAcceptedUnpublished,
		protocol.AttemptFailed, protocol.AttemptCancelled, protocol.AttemptLost:
		return true
	}
	return false
}

// isConstraintViolation detects a SQLite constraint failure — the partial
// unique index, a UNIQUE key, a CHECK — which under concurrency means two
// transactions raced for the same invariant; callers surface it as a
// conflict, never a 500 (KTD3).
func isConstraintViolation(err error) bool {
	var coded interface{ Code() int }
	if errors.As(err, &coded) {
		// Extended result codes keep the primary code in the low byte;
		// 19 is SQLITE_CONSTRAINT.
		return coded.Code()&0xff == 19
	}
	return err != nil && strings.Contains(err.Error(), "constraint")
}

func fromMillis(value int64) time.Time { return time.UnixMilli(value).UTC() }

func timePointer(value sql.NullInt64) *time.Time {
	if !value.Valid {
		return nil
	}
	t := fromMillis(value.Int64)
	return &t
}

// ---- lease loading and verification --------------------------------------

// leaseState is everything the fenced transitions need to know about an
// attempt and its job, loaded inside the transaction that will change them.
type leaseState struct {
	attemptState  string
	attemptNumber int
	jobID         string
	jobState      string
	workerID      sql.NullString
	digest        []byte
	expiry        sql.NullInt64
	cancel        bool
}

func loadLease(ctx context.Context, tx *sql.Tx, attemptID string) (leaseState, error) {
	var value leaseState
	var cancel int
	err := tx.QueryRowContext(ctx, `
		SELECT a.state, a.attempt_number, a.job_id, j.state, a.worker_id,
		       a.lease_digest, a.lease_expires_at, j.cancellation_requested
		FROM attempts a JOIN jobs j ON j.id = a.job_id
		WHERE a.id = ?
	`, attemptID).Scan(&value.attemptState, &value.attemptNumber, &value.jobID, &value.jobState,
		&value.workerID, &value.digest, &value.expiry, &cancel)
	if errors.Is(err, sql.ErrNoRows) {
		return value, ErrNotFound
	}
	if err != nil {
		return value, unavailable(err)
	}
	value.cancel = cancel != 0
	return value, nil
}

// verifyActiveLease is the fence (R6): the presented token's digest must own
// the attempt, the attempt must still be leased, and the lease must be
// unexpired. Heartbeat deliberately does not use the expiry half — an
// expired-but-unswept lease is renewable under R5's conditions.
func verifyActiveLease(value leaseState, token string, nowMillis int64) error {
	if err := validateLeaseToken(token); err != nil {
		return err
	}
	if !equalDigest(value.digest, digestToken(token)) || !isLeasedState(value.attemptState) ||
		!value.expiry.Valid || value.expiry.Int64 <= nowMillis {
		return conflict("lease_not_owner", "the lease token does not own an active attempt")
	}
	return nil
}

// ---- worker registration -------------------------------------------------

// RegisterWorker upserts the single implicit worker (KTD12). Registration
// carries env-var names only — never values (R17) — and refreshes liveness:
// a re-registration is also a heartbeat. The payload's retained worktrees
// upsert into the control-plane ledger in the same transaction (R16), which
// is what feeds the claim transaction's per-repository skip-over cap (R4);
// reports for attempts this worker does not own are skipped, never written.
func (s *Store) RegisterWorker(ctx context.Context, workerID string, input protocol.WorkerRegistration) (protocol.Worker, error) {
	workerID = strings.TrimSpace(workerID)
	if workerID == "" || len(workerID) > 200 {
		return protocol.Worker{}, invalid("invalid_worker_id", "worker id is required and at most 200 bytes")
	}
	if strings.TrimSpace(input.Name) == "" {
		return protocol.Worker{}, invalid("invalid_worker_name", "worker name is required")
	}
	if input.Capacity < 1 || input.Capacity > 100 {
		return protocol.Worker{}, invalid("invalid_capacity", "capacity must be between 1 and 100")
	}
	envNames := normalizeEnvNames(input.EnvNames)
	envJSON, err := json.Marshal(envNames)
	if err != nil {
		return protocol.Worker{}, unavailable(err)
	}
	runtimes := input.Runtimes
	if runtimes == nil {
		runtimes = []protocol.RuntimeCapability{}
	}
	runtimesJSON, err := json.Marshal(runtimes)
	if err != nil {
		return protocol.Worker{}, unavailable(err)
	}
	now := s.now().UnixMilli()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return protocol.Worker{}, unavailable(err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO workers(id, name, worker_version, capacity, env_names_json, runtimes_json, registered_at, last_heartbeat)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			name = excluded.name,
			worker_version = excluded.worker_version,
			capacity = excluded.capacity,
			env_names_json = excluded.env_names_json,
			runtimes_json = excluded.runtimes_json,
			last_heartbeat = excluded.last_heartbeat
	`, workerID, input.Name, input.WorkerVersion, input.Capacity,
		string(envJSON), string(runtimesJSON), now, now); err != nil {
		return protocol.Worker{}, unavailable(err)
	}
	if _, err := s.applyRetainedWorktrees(ctx, tx, workerID, input.RetainedWorktrees); err != nil {
		return protocol.Worker{}, err
	}
	if err := tx.Commit(); err != nil {
		return protocol.Worker{}, unavailable(err)
	}
	return s.Worker(ctx, workerID)
}

func normalizeEnvNames(names []string) []string {
	seen := make(map[string]bool)
	normalized := []string{}
	for _, name := range names {
		name = strings.TrimSpace(name)
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		normalized = append(normalized, name)
	}
	sort.Strings(normalized)
	return normalized
}

// workerSelect is the SELECT the singular read and the fleet read (fleet.go)
// share. ActiveCount is computed from leased attempts rather than read from
// the stored counter, so the two reads can never disagree about how busy a
// worker is.
const workerSelect = `
	SELECT id, name, worker_version, capacity, env_names_json, runtimes_json,
	       registered_at, last_heartbeat,
	       (SELECT COUNT(*) FROM attempts
	        WHERE worker_id = workers.id AND state IN ('preparing', 'running'))
	FROM workers`

// Worker reads one worker record, computing ActiveCount from leased attempts.
func (s *Store) Worker(ctx context.Context, workerID string) (protocol.Worker, error) {
	value, err := scanWorker(s.db.QueryRowContext(ctx, workerSelect+` WHERE id = ?`, workerID))
	if errors.Is(err, sql.ErrNoRows) {
		return protocol.Worker{}, ErrNotFound
	}
	return value, err
}

// scanWorker reads one workerSelect row. sql.ErrNoRows passes through
// unwrapped so a single-row caller can map it to ErrNotFound.
func scanWorker(row rowScanner) (protocol.Worker, error) {
	var value protocol.Worker
	var envJSON, runtimesJSON string
	var registeredAt, lastHeartbeat int64
	err := row.Scan(&value.ID, &value.Name, &value.WorkerVersion, &value.Capacity,
		&envJSON, &runtimesJSON, &registeredAt, &lastHeartbeat, &value.ActiveCount)
	if errors.Is(err, sql.ErrNoRows) {
		return value, err
	}
	if err != nil {
		return value, unavailable(err)
	}
	if err := json.Unmarshal([]byte(envJSON), &value.EnvNames); err != nil {
		return value, unavailable(err)
	}
	if err := json.Unmarshal([]byte(runtimesJSON), &value.Runtimes); err != nil {
		return value, unavailable(err)
	}
	value.RegisteredAt = fromMillis(registeredAt)
	value.LastHeartbeat = fromMillis(lastHeartbeat)
	return value, nil
}

// ---- job enqueue ---------------------------------------------------------

// EnqueueJob creates one queued job for a run target, plus its first attempt
// in `queued` with NULL worker and lease — claim fills those (U1 contract).
// The base SHA comes from the run's frozen target, pinned at admission
// (KTD9); enqueue never resolves a ref.
func (s *Store) EnqueueJob(ctx context.Context, runID, repository string) (protocol.Job, error) {
	if strings.TrimSpace(runID) == "" || strings.TrimSpace(repository) == "" {
		return protocol.Job{}, invalid("invalid_job", "run id and repository are required")
	}
	now := s.now().UnixMilli()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return protocol.Job{}, unavailable(err)
	}
	defer tx.Rollback()
	var targetsJSON string
	err = tx.QueryRowContext(ctx, `SELECT targets FROM runs WHERE id = ?`, runID).Scan(&targetsJSON)
	if errors.Is(err, sql.ErrNoRows) {
		return protocol.Job{}, ErrNotFound
	}
	if err != nil {
		return protocol.Job{}, unavailable(err)
	}
	var targets []protocol.RunTarget
	if err := json.Unmarshal([]byte(targetsJSON), &targets); err != nil {
		return protocol.Job{}, unavailable(err)
	}
	baseSHA := ""
	for _, target := range targets {
		if target.Repository == repository {
			baseSHA = target.BaseSHA
			break
		}
	}
	if baseSHA == "" {
		return protocol.Job{}, invalid("unknown_target",
			fmt.Sprintf("repository %q is not a target of run %q", repository, runID))
	}
	jobID, err := newID()
	if err != nil {
		return protocol.Job{}, unavailable(err)
	}
	attemptID, err := newID()
	if err != nil {
		return protocol.Job{}, unavailable(err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO jobs(id, run_id, repository, base_sha, state, created_at, updated_at)
		VALUES (?, ?, ?, ?, 'queued', ?, ?)
	`, jobID, runID, repository, baseSHA, now, now); err != nil {
		if isConstraintViolation(err) {
			return protocol.Job{}, conflict("job_exists", "the run already has a job for this repository")
		}
		return protocol.Job{}, unavailable(err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO attempts(id, job_id, attempt_number, state, created_at)
		VALUES (?, ?, 1, 'queued', ?)
	`, attemptID, jobID, now); err != nil {
		if isConstraintViolation(err) {
			return protocol.Job{}, conflict("attempt_conflict", "the job already has a live attempt")
		}
		return protocol.Job{}, unavailable(err)
	}
	if err := tx.Commit(); err != nil {
		return protocol.Job{}, unavailable(err)
	}
	return s.Job(ctx, jobID)
}

// ---- attempt transitions (fenced) ----------------------------------------

// StartAttempt moves a claimed attempt preparing -> running in one fenced
// transaction (R6), recording the probed runtime for the trace (KTD4). A
// replay against a running attempt is a no-op success.
func (s *Store) StartAttempt(ctx context.Context, attemptID string, input protocol.StartAttemptRequest) (protocol.Attempt, error) {
	now := s.now().UnixMilli()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return protocol.Attempt{}, unavailable(err)
	}
	defer tx.Rollback()
	lease, err := loadLease(ctx, tx, attemptID)
	if err != nil {
		return protocol.Attempt{}, err
	}
	if err := verifyActiveLease(lease, input.LeaseToken, now); err != nil {
		return protocol.Attempt{}, err
	}
	if lease.attemptState == protocol.AttemptPreparing {
		if _, err := tx.ExecContext(ctx, `
			UPDATE attempts SET state = 'running', runtime_name = ?, runtime_version = ?, started_at = ?
			WHERE id = ? AND state = 'preparing'
		`, nullString(input.RuntimeName), nullString(input.RuntimeVersion), now, attemptID); err != nil {
			return protocol.Attempt{}, unavailable(err)
		}
	} else if lease.attemptState != protocol.AttemptRunning {
		return protocol.Attempt{}, conflict("invalid_transition", "attempt cannot start from its current state")
	}
	if err := tx.Commit(); err != nil {
		return protocol.Attempt{}, unavailable(err)
	}
	return s.Attempt(ctx, attemptID)
}

// CompleteAttempt records an attempt's terminal outcome in one fenced
// transaction (R6) and mirrors it onto the job. Completion is replayable:
// a terminal attempt replayed with the original token returns the stored
// outcome unchanged — the stored outcome always wins. A stale token — a
// swept-lost attempt, a superseded lease — is rejected.
func (s *Store) CompleteAttempt(ctx context.Context, attemptID string, input protocol.CompleteAttemptRequest) (protocol.Attempt, error) {
	switch input.State {
	case protocol.AttemptAccepted, protocol.AttemptAcceptedUnpublished,
		protocol.AttemptFailed, protocol.AttemptCancelled:
	default:
		return protocol.Attempt{}, invalid("invalid_terminal_state",
			"state must be accepted, accepted_unpublished, failed, or cancelled")
	}
	if len(input.Result) > protocol.MaxResultBytes || len(input.Error) > protocol.MaxErrorBytes {
		return protocol.Attempt{}, &ServiceError{Code: "result_too_large",
			Message: "result or error exceeds its storage limit", Status: 413}
	}
	if err := validateLeaseToken(input.LeaseToken); err != nil {
		return protocol.Attempt{}, err
	}
	now := s.now().UnixMilli()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return protocol.Attempt{}, unavailable(err)
	}
	defer tx.Rollback()
	lease, err := loadLease(ctx, tx, attemptID)
	if err != nil {
		return protocol.Attempt{}, err
	}
	if isTerminalAttemptState(lease.attemptState) {
		// Replay of a terminal completion: the stored outcome wins — but only
		// for the token that owned the attempt, and never for `lost`, which
		// no worker token ever legitimately produced.
		if lease.attemptState == protocol.AttemptLost || !equalDigest(lease.digest, digestToken(input.LeaseToken)) {
			return protocol.Attempt{}, conflict("lease_not_owner", "the lease token does not own this terminal attempt")
		}
		if err := tx.Commit(); err != nil {
			return protocol.Attempt{}, unavailable(err)
		}
		return s.Attempt(ctx, attemptID)
	}
	if err := verifyActiveLease(lease, input.LeaseToken, now); err != nil {
		return protocol.Attempt{}, err
	}
	if (input.State == protocol.AttemptAccepted || input.State == protocol.AttemptAcceptedUnpublished) &&
		lease.attemptState != protocol.AttemptRunning {
		return protocol.Attempt{}, conflict("invalid_transition", "only a running attempt can be accepted")
	}
	if input.State == protocol.AttemptAccepted {
		// R12's third conjunct, enforced HERE rather than only worker-side:
		// `accepted` means phases passed AND the predicate held AND publish
		// completed with remote proof. The first two are worker judgements the
		// control plane cannot re-derive, but the third is a row in its own
		// database — so it checks, inside the same transaction that would
		// write the state. A worker with a bug, a stale build, or an operator
		// driving the API by hand cannot declare published work that was never
		// published; `accepted_unpublished` is the state for that, and it has
		// its own retry (R14).
		var proven int
		if err := tx.QueryRowContext(ctx, `
			SELECT COUNT(*) FROM publish_records WHERE attempt_id = ? AND step = ?
		`, attemptID, protocol.PublishStepProof).Scan(&proven); err != nil {
			return protocol.Attempt{}, unavailable(err)
		}
		if proven == 0 {
			return protocol.Attempt{}, conflict("publish_proof_required",
				"an attempt is `accepted` only with a recorded proof-of-publish step (R12); "+
					"complete as accepted_unpublished when publish did not finish")
		}
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE attempts SET state = ?, result = ?, error = ?, completed_at = ?
		WHERE id = ? AND state IN ('preparing', 'running')
	`, input.State, nullString(input.Result), nullString(input.Error), now, attemptID); err != nil {
		return protocol.Attempt{}, unavailable(err)
	}
	// Attempt terminal states map one-to-one onto job states (R12's job half).
	if _, err := tx.ExecContext(ctx, `
		UPDATE jobs SET state = ?, updated_at = ? WHERE id = ? AND state = 'active'
	`, input.State, now, lease.jobID); err != nil {
		return protocol.Attempt{}, unavailable(err)
	}
	// The run aggregate rides the same transaction as the job state it
	// summarizes (R12): there is no window in which a run's state disagrees
	// with its jobs (U5).
	if err := applyRunAggregationForJob(ctx, tx, lease.jobID, now); err != nil {
		return protocol.Attempt{}, err
	}
	if err := tx.Commit(); err != nil {
		return protocol.Attempt{}, unavailable(err)
	}
	return s.Attempt(ctx, attemptID)
}

// ---- retry and cancel ----------------------------------------------------

// RetryJob re-queues a failed job as attempt N+1 — always cold: fresh
// worktree, fresh sessions, from phase 1 (KTD5), against the run's pinned
// base SHA (KTD9), which lives on the job row and is never re-resolved. A
// job whose attempt was swept `lost` is `failed` and retries the same way.
func (s *Store) RetryJob(ctx context.Context, jobID string) (protocol.Job, error) {
	now := s.now().UnixMilli()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return protocol.Job{}, unavailable(err)
	}
	defer tx.Rollback()
	var state string
	err = tx.QueryRowContext(ctx, `SELECT state FROM jobs WHERE id = ?`, jobID).Scan(&state)
	if errors.Is(err, sql.ErrNoRows) {
		return protocol.Job{}, ErrNotFound
	}
	if err != nil {
		return protocol.Job{}, unavailable(err)
	}
	if state != protocol.JobFailed {
		return protocol.Job{}, conflict("retry_not_allowed", "only a failed job can be retried")
	}
	var attemptNumber int
	if err := tx.QueryRowContext(ctx, `
		SELECT COALESCE(MAX(attempt_number), 0) + 1 FROM attempts WHERE job_id = ?
	`, jobID).Scan(&attemptNumber); err != nil {
		return protocol.Job{}, unavailable(err)
	}
	attemptID, err := newID()
	if err != nil {
		return protocol.Job{}, unavailable(err)
	}
	// The partial unique index guarantees single-active-attempt: two racing
	// retries both compute N+1 and exactly one insert survives (KTD3).
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO attempts(id, job_id, attempt_number, state, created_at)
		VALUES (?, ?, ?, 'queued', ?)
	`, attemptID, jobID, attemptNumber, now); err != nil {
		if isConstraintViolation(err) {
			return protocol.Job{}, conflict("retry_conflict", "a concurrent retry already re-queued this job")
		}
		return protocol.Job{}, unavailable(err)
	}
	result, err := tx.ExecContext(ctx, `
		UPDATE jobs SET state = 'queued', cancellation_requested = 0, updated_at = ?
		WHERE id = ? AND state = 'failed'
	`, now, jobID)
	if err != nil {
		return protocol.Job{}, unavailable(err)
	}
	if changed, _ := result.RowsAffected(); changed != 1 {
		return protocol.Job{}, conflict("retry_conflict", "the job left the failed state during retry")
	}
	// A retried job is live again, so its run returns to `active` — the
	// aggregate is recomputed, never latched (R12, U5).
	if err := applyRunAggregationForJob(ctx, tx, jobID, now); err != nil {
		return protocol.Job{}, err
	}
	if err := tx.Commit(); err != nil {
		return protocol.Job{}, unavailable(err)
	}
	return s.Job(ctx, jobID)
}

// CancelJob cancels a queued job outright (job and its unclaimed attempt) or
// requests cancellation of an active one — the flag rides the next heartbeat
// response and the worker performs the transition (R5). Cancelling a
// terminal job is a no-op returning the stored state.
func (s *Store) CancelJob(ctx context.Context, jobID string) (protocol.Job, error) {
	now := s.now().UnixMilli()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return protocol.Job{}, unavailable(err)
	}
	defer tx.Rollback()
	var state string
	err = tx.QueryRowContext(ctx, `SELECT state FROM jobs WHERE id = ?`, jobID).Scan(&state)
	if errors.Is(err, sql.ErrNoRows) {
		return protocol.Job{}, ErrNotFound
	}
	if err != nil {
		return protocol.Job{}, unavailable(err)
	}
	switch state {
	case protocol.JobQueued:
		if _, err := tx.ExecContext(ctx, `
			UPDATE jobs SET state = 'cancelled', updated_at = ? WHERE id = ? AND state = 'queued'
		`, now, jobID); err != nil {
			return protocol.Job{}, unavailable(err)
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE attempts SET state = 'cancelled', completed_at = ?
			WHERE job_id = ? AND state = 'queued'
		`, now, jobID); err != nil {
			return protocol.Job{}, unavailable(err)
		}
	case protocol.JobActive:
		if _, err := tx.ExecContext(ctx, `
			UPDATE jobs SET cancellation_requested = 1, updated_at = ? WHERE id = ? AND state = 'active'
		`, now, jobID); err != nil {
			return protocol.Job{}, unavailable(err)
		}
	}
	if err := applyRunAggregationForJob(ctx, tx, jobID, now); err != nil {
		return protocol.Job{}, err
	}
	if err := tx.Commit(); err != nil {
		return protocol.Job{}, unavailable(err)
	}
	return s.Job(ctx, jobID)
}

// ---- reads ---------------------------------------------------------------

type rowScanner interface{ Scan(dest ...any) error }

const attemptColumns = `id, job_id, worker_id, attempt_number, state, lease_expires_at,
	runtime_name, runtime_version, result, error, started_at, completed_at, created_at`

func scanAttempt(row rowScanner) (protocol.Attempt, error) {
	var value protocol.Attempt
	var workerID, runtimeName, runtimeVersion, result, errorText sql.NullString
	var leaseExpires, startedAt, completedAt sql.NullInt64
	var createdAt int64
	err := row.Scan(&value.ID, &value.JobID, &workerID, &value.AttemptNumber, &value.State,
		&leaseExpires, &runtimeName, &runtimeVersion, &result, &errorText,
		&startedAt, &completedAt, &createdAt)
	if err != nil {
		return value, err
	}
	value.WorkerID = workerID.String
	value.LeaseExpiresAt = timePointer(leaseExpires)
	value.RuntimeName = runtimeName.String
	value.RuntimeVersion = runtimeVersion.String
	value.Result = result.String
	value.Error = errorText.String
	value.StartedAt = timePointer(startedAt)
	value.CompletedAt = timePointer(completedAt)
	value.CreatedAt = fromMillis(createdAt)
	return value, nil
}

// Attempt reads one attempt. The lease digest never leaves the store.
func (s *Store) Attempt(ctx context.Context, attemptID string) (protocol.Attempt, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+attemptColumns+` FROM attempts WHERE id = ?`, attemptID)
	value, err := scanAttempt(row)
	if errors.Is(err, sql.ErrNoRows) {
		return value, ErrNotFound
	}
	if err != nil {
		return value, unavailable(err)
	}
	return value, nil
}

// Job reads one job.
func (s *Store) Job(ctx context.Context, jobID string) (protocol.Job, error) {
	var value protocol.Job
	var cancel int
	var createdAt, updatedAt int64
	err := s.db.QueryRowContext(ctx, `
		SELECT id, run_id, repository, base_sha, state, cancellation_requested, created_at, updated_at
		FROM jobs WHERE id = ?
	`, jobID).Scan(&value.ID, &value.RunID, &value.Repository, &value.BaseSHA,
		&value.State, &cancel, &createdAt, &updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return value, ErrNotFound
	}
	if err != nil {
		return value, unavailable(err)
	}
	value.CancellationRequested = cancel != 0
	value.CreatedAt = fromMillis(createdAt)
	value.UpdatedAt = fromMillis(updatedAt)
	return value, nil
}

func nullString(value string) any {
	if value == "" {
		return nil
	}
	return value
}
