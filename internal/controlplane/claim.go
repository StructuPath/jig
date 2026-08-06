// claim.go — the claim transaction (U2), ported from factory's shape: one
// idempotent transaction carrying request-id + lease-digest replay semantics,
// in-transaction eligibility, and FIFO candidate selection with skip-over for
// repositories at their retained-worktree cap (R4, KTD3).
package controlplane

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"

	"github.com/StructuPath/jig/internal/protocol"
)

// Claim answers one worker claim request. A nil, nil return is an empty
// claim: nothing eligible right now.
//
// Idempotency (R4): the (worker_id, request_id) pair is recorded with the
// token's SHA-256 digest before the transaction commits. Replaying the same
// pair with the same token returns the identical answer — the same attempt,
// or the same emptiness within EmptyClaimTTL; the same pair with a different
// token is a conflict.
//
// Eligibility is decided inside the transaction: worker liveness
// (last_heartbeat within WorkerLivenessWindow), capacity (leased attempts <
// capacity), and per-job env requirements (the definition's required env
// names must be a subset of the worker's advertised names, KTD10/R17 — jobs
// this worker cannot run are skipped, they do not block the queue).
//
// Ordering is FIFO over queued jobs (jobs_claim_order), skipping jobs whose
// repository already holds MaxRetainedWorktreesPerRepo retained worktrees
// (R4, R16).
func (s *Store) Claim(ctx context.Context, workerID string, input protocol.ClaimRequest) (*protocol.Claim, error) {
	input.RequestID = strings.TrimSpace(input.RequestID)
	if input.RequestID == "" || len(input.RequestID) > 200 {
		return nil, invalid("invalid_request_id", "request_id is required and at most 200 bytes")
	}
	if err := validateLeaseToken(input.LeaseToken); err != nil {
		return nil, err
	}
	digest := digestToken(input.LeaseToken)
	now := s.now()
	nowMillis := now.UnixMilli()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, unavailable(err)
	}
	defer tx.Rollback()

	// Replay path: the request id was seen before.
	var storedDigest []byte
	var storedAttempt sql.NullString
	var claimCreated int64
	err = tx.QueryRowContext(ctx, `
		SELECT lease_digest, attempt_id, created_at FROM claim_requests
		WHERE worker_id = ? AND request_id = ?
	`, workerID, input.RequestID).Scan(&storedDigest, &storedAttempt, &claimCreated)
	if err == nil {
		if !equalDigest(storedDigest, digest) {
			return nil, conflict("claim_request_conflict", "request_id was already used with a different lease token")
		}
		if !storedAttempt.Valid {
			if now.Sub(fromMillis(claimCreated)) <= protocol.EmptyClaimTTL {
				if err := tx.Commit(); err != nil {
					return nil, unavailable(err)
				}
				return nil, nil
			}
			// The empty answer aged out; forget it and claim afresh.
			if _, err := tx.ExecContext(ctx, `
				DELETE FROM claim_requests WHERE worker_id = ? AND request_id = ?
			`, workerID, input.RequestID); err != nil {
				return nil, unavailable(err)
			}
		} else {
			var state string
			var expiry sql.NullInt64
			var attemptDigest []byte
			err := tx.QueryRowContext(ctx, `
				SELECT state, lease_expires_at, lease_digest FROM attempts WHERE id = ?
			`, storedAttempt.String).Scan(&state, &expiry, &attemptDigest)
			if err != nil {
				return nil, unavailable(err)
			}
			if !equalDigest(attemptDigest, digest) || !isLeasedState(state) ||
				!expiry.Valid || expiry.Int64 <= nowMillis {
				return nil, conflict("lease_not_owner", "the claim no longer owns an active lease")
			}
			if err := tx.Commit(); err != nil {
				return nil, unavailable(err)
			}
			claim, err := s.claimDetail(ctx, storedAttempt.String)
			return &claim, err
		}
	} else if !errors.Is(err, sql.ErrNoRows) {
		return nil, unavailable(err)
	}

	// Worker eligibility: liveness and capacity (R4, KTD12).
	var capacity int
	var lastHeartbeat int64
	var envNamesJSON string
	err = tx.QueryRowContext(ctx, `
		SELECT capacity, last_heartbeat, env_names_json FROM workers WHERE id = ?
	`, workerID).Scan(&capacity, &lastHeartbeat, &envNamesJSON)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, unavailable(err)
	}
	var active int
	if err := tx.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM attempts
		WHERE worker_id = ? AND state IN ('preparing', 'running')
	`, workerID).Scan(&active); err != nil {
		return nil, unavailable(err)
	}
	if now.Sub(fromMillis(lastHeartbeat)) > protocol.WorkerLivenessWindow || active >= capacity {
		return s.commitEmptyClaim(ctx, tx, workerID, input.RequestID, digest, nowMillis)
	}
	var advertisedNames []string
	if err := json.Unmarshal([]byte(envNamesJSON), &advertisedNames); err != nil {
		return nil, unavailable(err)
	}
	advertised := make(map[string]bool, len(advertisedNames))
	for _, name := range advertisedNames {
		advertised[name] = true
	}

	// Candidate selection: FIFO with retained-worktree skip-over (R4). The
	// ORDER BY matches the jobs_claim_order index (state, created_at, id).
	type candidate struct {
		jobID    string
		snapshot string
	}
	rows, err := tx.QueryContext(ctx, `
		SELECT j.id, r.snapshot
		FROM jobs j JOIN runs r ON r.id = j.run_id
		WHERE j.state = 'queued'
		  AND (SELECT COUNT(*) FROM retained_worktrees rw
		       WHERE rw.repository = j.repository AND rw.state = 'retained') < ?
		ORDER BY j.created_at, j.id
	`, protocol.MaxRetainedWorktreesPerRepo)
	if err != nil {
		return nil, unavailable(err)
	}
	var candidates []candidate
	for rows.Next() {
		var value candidate
		if err := rows.Scan(&value.jobID, &value.snapshot); err != nil {
			rows.Close()
			return nil, unavailable(err)
		}
		candidates = append(candidates, value)
	}
	if err := rows.Close(); err != nil {
		return nil, unavailable(err)
	}
	chosenJob := ""
	for _, value := range candidates {
		spec, err := protocol.ParseDefinition([]byte(value.snapshot))
		if err != nil {
			// A frozen snapshot that no longer parses cannot be executed;
			// skip it rather than blocking the queue behind it.
			continue
		}
		if !envNamesSubset(spec.RequiredEnvNames(), advertised) {
			// The definition requires an env name this worker did not
			// advertise: ineligible at claim, not N phases deep (R17).
			continue
		}
		chosenJob = value.jobID
		break
	}
	if chosenJob == "" {
		return s.commitEmptyClaim(ctx, tx, workerID, input.RequestID, digest, nowMillis)
	}

	// Claim the job's queued attempt: fill worker, lease digest, and expiry
	// (U1 contract: attempt rows exist pre-claim with NULLs there). The
	// guarded UPDATEs and the partial unique index together make a lost race
	// a conflict, never a double claim (KTD3).
	var attemptID string
	err = tx.QueryRowContext(ctx, `
		SELECT id FROM attempts WHERE job_id = ? AND state = 'queued'
	`, chosenJob).Scan(&attemptID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, conflict("claim_conflict", "the job has no queued attempt to claim")
	}
	if err != nil {
		return nil, unavailable(err)
	}
	expiry := now.Add(protocol.LeaseDuration).UnixMilli()
	result, err := tx.ExecContext(ctx, `
		UPDATE attempts SET state = 'preparing', worker_id = ?, lease_digest = ?, lease_expires_at = ?
		WHERE id = ? AND state = 'queued'
	`, workerID, digest, expiry, attemptID)
	if err != nil {
		if isConstraintViolation(err) {
			return nil, conflict("claim_conflict", "a concurrent claim already took this job")
		}
		return nil, unavailable(err)
	}
	if changed, _ := result.RowsAffected(); changed != 1 {
		return nil, conflict("claim_conflict", "a concurrent claim already took this job")
	}
	result, err = tx.ExecContext(ctx, `
		UPDATE jobs SET state = 'active', cancellation_requested = 0, updated_at = ?
		WHERE id = ? AND state = 'queued'
	`, nowMillis, chosenJob)
	if err != nil {
		return nil, unavailable(err)
	}
	if changed, _ := result.RowsAffected(); changed != 1 {
		return nil, conflict("claim_conflict", "the job is no longer queued")
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO claim_requests(worker_id, request_id, lease_digest, attempt_id, created_at)
		VALUES (?, ?, ?, ?, ?)
	`, workerID, input.RequestID, digest, attemptID, nowMillis); err != nil {
		if isConstraintViolation(err) {
			return nil, conflict("claim_request_conflict", "a concurrent claim already used this request id")
		}
		return nil, unavailable(err)
	}
	if err := tx.Commit(); err != nil {
		return nil, unavailable(err)
	}
	claim, err := s.claimDetail(ctx, attemptID)
	return &claim, err
}

// commitEmptyClaim records the empty answer so a replay of the same request
// id stays empty (within EmptyClaimTTL) instead of racing a second claim.
func (s *Store) commitEmptyClaim(ctx context.Context, tx *sql.Tx, workerID, requestID string, digest []byte, nowMillis int64) (*protocol.Claim, error) {
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO claim_requests(worker_id, request_id, lease_digest, attempt_id, created_at)
		VALUES (?, ?, ?, NULL, ?)
	`, workerID, requestID, digest, nowMillis); err != nil {
		if isConstraintViolation(err) {
			return nil, conflict("claim_request_conflict", "a concurrent claim already used this request id")
		}
		return nil, unavailable(err)
	}
	if err := tx.Commit(); err != nil {
		return nil, unavailable(err)
	}
	return nil, nil
}

func envNamesSubset(required []string, advertised map[string]bool) bool {
	for _, name := range required {
		if !advertised[name] {
			return false
		}
	}
	return true
}

// claimDetail assembles the claim answer: attempt, job, and the run's frozen
// snapshot and parameters (R2). Reads run after commit, like factory.
func (s *Store) claimDetail(ctx context.Context, attemptID string) (protocol.Claim, error) {
	var claim protocol.Claim
	attempt, err := s.Attempt(ctx, attemptID)
	if err != nil {
		return claim, err
	}
	claim.Attempt = attempt
	job, err := s.Job(ctx, attempt.JobID)
	if err != nil {
		return claim, err
	}
	claim.Job = job
	var parametersJSON string
	err = s.db.QueryRowContext(ctx, `
		SELECT snapshot, parameters FROM runs WHERE id = ?
	`, job.RunID).Scan(&claim.Snapshot, &parametersJSON)
	if err != nil {
		return claim, unavailable(err)
	}
	if err := json.Unmarshal([]byte(parametersJSON), &claim.Parameters); err != nil {
		return claim, unavailable(err)
	}
	return claim, nil
}
