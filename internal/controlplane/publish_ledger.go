// publish_ledger.go — the control-plane side of publish (U7, R6, R14). Two
// things live here and nothing else: the fenced, idempotent publish ledger
// over publish_records, and the publish-only retry action.
//
// The ledger is the fence AND the proof. Every step is authorized before its
// irreversible side effect and recorded after it, both under the attempt's
// lease token, so a zombie attempt whose lease expired is refused at the
// authorization and — if it slipped through and pushed anyway — refused again
// at the record, leaving a branch with no proof behind it. That leftover is
// visible (the worker's stray-branch report reads this ledger to find it),
// which is the honest half of KTD6: the fence cannot fence GitHub, so damage
// is confined by attempt-scoped naming and surfaced rather than prevented.
//
// The branch name is not taken on trust. The control plane knows the job id
// and the attempt number, so it computes the attempt-scoped branch itself and
// refuses any other: an attempt can only ever publish its own branch.
package controlplane

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"

	"github.com/StructuPath/jig/internal/protocol"
)

var publishRefPattern = regexp.MustCompile(`^[0-9a-f]{40}([0-9a-f]{24})?$`)

// publishContext is everything a fenced publish transaction needs about the
// attempt it is deciding for, loaded inside that transaction.
type publishContext struct {
	lease          leaseState
	repository     string
	baseSHA        string
	expectedBranch string
}

func loadPublishContext(ctx context.Context, tx *sql.Tx, attemptID string) (publishContext, error) {
	var value publishContext
	lease, err := loadLease(ctx, tx, attemptID)
	if err != nil {
		return value, err
	}
	value.lease = lease
	if err := tx.QueryRowContext(ctx, `
		SELECT repository, base_sha FROM jobs WHERE id = ?
	`, lease.jobID).Scan(&value.repository, &value.baseSHA); err != nil {
		return value, unavailable(err)
	}
	value.expectedBranch = protocol.PublishBranch(lease.jobID, lease.attemptNumber)
	return value, nil
}

// validatePublishStep checks the step vocabulary and the branch the caller
// intends to write. An empty branch is accepted as "whatever the control
// plane says"; a non-empty one must match exactly.
func validatePublishStep(step, branch, expectedBranch string) error {
	if !protocol.ValidPublishStep(step) {
		return invalid("invalid_publish_step",
			"step must be one of push, pull_request, proof, ci")
	}
	if len(branch) > protocol.MaxPublishBranchBytes {
		return invalid("invalid_publish_branch", "branch exceeds its storage limit")
	}
	if branch != "" && branch != expectedBranch {
		return invalid("publish_branch_mismatch", fmt.Sprintf(
			"an attempt may only publish its own attempt-scoped branch %q", expectedBranch))
	}
	return nil
}

// requirePublishPrerequisites enforces pipeline order inside the transaction:
// no pull request without a recorded push, no proof without both. Order is a
// control-plane invariant because the records are what a retry re-enters on.
func requirePublishPrerequisites(ctx context.Context, tx *sql.Tx, attemptID, step string) error {
	for _, required := range protocol.PublishStepPrerequisites[step] {
		var count int
		if err := tx.QueryRowContext(ctx, `
			SELECT COUNT(*) FROM publish_records WHERE attempt_id = ? AND step = ?
		`, attemptID, required).Scan(&count); err != nil {
			return unavailable(err)
		}
		if count == 0 {
			return conflict("publish_step_out_of_order", fmt.Sprintf(
				"publish step %q requires %q to be recorded first", step, required))
		}
	}
	return nil
}

func readPublishRecord(ctx context.Context, tx *sql.Tx, attemptID, step string) (protocol.PublishRecord, bool, error) {
	var record protocol.PublishRecord
	var completedAt int64
	err := tx.QueryRowContext(ctx, `
		SELECT p.attempt_id, a.job_id, a.attempt_number, p.step, p.branch,
		       p.remote_ref, p.pr_url, p.completed_at
		FROM publish_records p JOIN attempts a ON a.id = p.attempt_id
		WHERE p.attempt_id = ? AND p.step = ?
	`, attemptID, step).Scan(&record.AttemptID, &record.JobID, &record.AttemptNumber,
		&record.Step, &record.Branch, &record.RemoteRef, &record.PullRequestURL, &completedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return record, false, nil
	}
	if err != nil {
		return record, false, unavailable(err)
	}
	record.CompletedAt = fromMillis(completedAt)
	return record, true, nil
}

// AuthorizePublishStep is the pre-effect fence (R6, KTD6). It validates the
// lease token against an unexpired, still-leased attempt, computes the branch
// that attempt is allowed to write, enforces pipeline order, and reports
// whether the step is already recorded — in which case the worker must skip
// the side effect rather than repeat it.
//
// This is deliberately a separate call from RecordPublishStep. Recording
// alone would fence only AFTER the push had already reached GitHub; checking
// first is what keeps a zombie's damage to the cases where its lease died
// mid-command.
func (s *Store) AuthorizePublishStep(
	ctx context.Context, attemptID string, input protocol.PublishAuthorizationRequest,
) (protocol.PublishAuthorization, error) {
	var authorization protocol.PublishAuthorization
	if err := validateLeaseToken(input.LeaseToken); err != nil {
		return authorization, err
	}
	now := s.now().UnixMilli()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return authorization, unavailable(err)
	}
	defer tx.Rollback()
	value, err := loadPublishContext(ctx, tx, attemptID)
	if err != nil {
		return authorization, err
	}
	if err := validatePublishStep(input.Step, input.Branch, value.expectedBranch); err != nil {
		return authorization, err
	}
	if err := verifyActiveLease(value.lease, input.LeaseToken, now); err != nil {
		return authorization, err
	}
	record, exists, err := readPublishRecord(ctx, tx, attemptID, input.Step)
	if err != nil {
		return authorization, err
	}
	if !exists {
		// Order is only enforced for steps that still have to happen; a
		// recorded step replays regardless of what came after it.
		if err := requirePublishPrerequisites(ctx, tx, attemptID, input.Step); err != nil {
			return authorization, err
		}
	}
	if err := tx.Commit(); err != nil {
		return authorization, unavailable(err)
	}
	authorization = protocol.PublishAuthorization{
		AttemptID:     attemptID,
		JobID:         value.lease.jobID,
		AttemptNumber: value.lease.attemptNumber,
		Repository:    value.repository,
		BaseSHA:       value.baseSHA,
		Step:          input.Step,
		Branch:        value.expectedBranch,
	}
	if exists {
		authorization.Completed = &record
	}
	return authorization, nil
}

// RecordPublishStep writes one step's proof under the lease token (R6). It is
// idempotent by primary key: replaying identical values returns the stored
// record. Replaying DIFFERENT values for a recorded step is a conflict — one
// step with two answers is an anomaly worth surfacing, not a retry to
// absorb.
func (s *Store) RecordPublishStep(
	ctx context.Context, attemptID string, input protocol.PublishStepRequest,
) (protocol.PublishRecord, error) {
	var record protocol.PublishRecord
	if err := validateLeaseToken(input.LeaseToken); err != nil {
		return record, err
	}
	if len(input.PullRequestURL) > protocol.MaxPublishURLBytes {
		return record, invalid("invalid_publish_url", "pr_url exceeds its storage limit")
	}
	if len(input.RemoteRef) > protocol.MaxPublishRefBytes {
		return record, invalid("invalid_publish_ref", "remote_ref exceeds its storage limit")
	}
	switch input.Step {
	case protocol.PublishStepPush, protocol.PublishStepProof, protocol.PublishStepCI:
		if !publishRefPattern.MatchString(input.RemoteRef) {
			return record, invalid("invalid_publish_ref",
				"push, proof, and ci records must carry a commit SHA as remote_ref")
		}
	case protocol.PublishStepPullRequest:
		if !strings.HasPrefix(input.PullRequestURL, "https://") {
			return record, invalid("invalid_publish_url",
				"a pull_request record must carry the pull request's https URL")
		}
	}
	now := s.now().UnixMilli()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return record, unavailable(err)
	}
	defer tx.Rollback()
	value, err := loadPublishContext(ctx, tx, attemptID)
	if err != nil {
		return record, err
	}
	if err := validatePublishStep(input.Step, input.Branch, value.expectedBranch); err != nil {
		return record, err
	}
	if err := verifyActiveLease(value.lease, input.LeaseToken, now); err != nil {
		return record, err
	}
	existing, exists, err := readPublishRecord(ctx, tx, attemptID, input.Step)
	if err != nil {
		return record, err
	}
	if exists {
		if existing.Branch != value.expectedBranch ||
			existing.RemoteRef != input.RemoteRef ||
			existing.PullRequestURL != input.PullRequestURL {
			return record, conflict("publish_step_conflict", fmt.Sprintf(
				"publish step %q is already recorded with a different result", input.Step))
		}
		if err := tx.Commit(); err != nil {
			return record, unavailable(err)
		}
		return existing, nil
	}
	if err := requirePublishPrerequisites(ctx, tx, attemptID, input.Step); err != nil {
		return record, err
	}
	if input.Step == protocol.PublishStepCI {
		// A ci row is permanent, so a green record on a head a repair round
		// found red must be refused here: accepted would be refused on it
		// forever, and no further round is allowed once ci exists.
		if err := refuseGreenOnRepairedHead(ctx, tx, attemptID, input.RemoteRef); err != nil {
			return record, err
		}
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO publish_records(attempt_id, step, branch, remote_ref, pr_url, completed_at)
		VALUES (?, ?, ?, ?, ?, ?)
	`, attemptID, input.Step, value.expectedBranch, input.RemoteRef, input.PullRequestURL, now); err != nil {
		if isConstraintViolation(err) {
			return record, conflict("publish_step_conflict",
				"a concurrent publish already recorded this step")
		}
		return record, unavailable(err)
	}
	written, _, err := readPublishRecord(ctx, tx, attemptID, input.Step)
	if err != nil {
		return record, err
	}
	if err := tx.Commit(); err != nil {
		return record, unavailable(err)
	}
	return written, nil
}

// AttemptPublishRecords lists one attempt's proven steps in pipeline order.
func (s *Store) AttemptPublishRecords(ctx context.Context, attemptID string) ([]protocol.PublishRecord, error) {
	var exists int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM attempts WHERE id = ?`, attemptID).Scan(&exists); err != nil {
		return nil, unavailable(err)
	}
	if exists == 0 {
		return nil, ErrNotFound
	}
	return s.publishRecords(ctx, `p.attempt_id = ?`, attemptID)
}

// JobPublishRecords lists every proven step of every attempt of one job. It
// is what the worker's stray-branch report reads: an attempt-scoped branch on
// the remote with no `push` record here was pushed by something the fence
// refused to record.
func (s *Store) JobPublishRecords(ctx context.Context, jobID string) ([]protocol.PublishRecord, error) {
	var exists int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM jobs WHERE id = ?`, jobID).Scan(&exists); err != nil {
		return nil, unavailable(err)
	}
	if exists == 0 {
		return nil, ErrNotFound
	}
	return s.publishRecords(ctx, `a.job_id = ?`, jobID)
}

func (s *Store) publishRecords(ctx context.Context, where string, argument any) ([]protocol.PublishRecord, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT p.attempt_id, a.job_id, a.attempt_number, p.step, p.branch,
		       p.remote_ref, p.pr_url, p.completed_at
		FROM publish_records p JOIN attempts a ON a.id = p.attempt_id
		WHERE `+where+`
		ORDER BY a.attempt_number, p.completed_at, p.step
	`, argument)
	if err != nil {
		return nil, unavailable(err)
	}
	defer rows.Close()
	records := []protocol.PublishRecord{}
	for rows.Next() {
		var record protocol.PublishRecord
		var completedAt int64
		if err := rows.Scan(&record.AttemptID, &record.JobID, &record.AttemptNumber,
			&record.Step, &record.Branch, &record.RemoteRef, &record.PullRequestURL,
			&completedAt); err != nil {
			return nil, unavailable(err)
		}
		record.CompletedAt = fromMillis(completedAt)
		records = append(records, record)
	}
	if err := rows.Err(); err != nil {
		return nil, unavailable(err)
	}
	return records, nil
}

// RetryPublish is the publish-only retry action (R14). It re-leases the job's
// accepted_unpublished attempt — the SAME attempt, so the SAME attempt-scoped
// branch — to the worker that owns its retained state, and returns the steps
// already proven so publish re-enters exactly where it failed.
//
// It deliberately does not create a new attempt. A new attempt means a new
// branch and a second pull request for one piece of work; re-entering the
// existing attempt is what makes "never re-run any phase" true by
// construction rather than by discipline.
//
// Re-leasing supersedes any previous token on that attempt: a stalled
// publisher that wakes up afterwards is fenced out by digest mismatch.
func (s *Store) RetryPublish(
	ctx context.Context, jobID string, input protocol.PublishRetryRequest,
) (protocol.PublishRetry, error) {
	var retry protocol.PublishRetry
	if err := validateLeaseToken(input.LeaseToken); err != nil {
		return retry, err
	}
	if strings.TrimSpace(input.WorkerID) == "" {
		return retry, invalid("invalid_worker_id", "worker_id is required")
	}
	now := s.now()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return retry, unavailable(err)
	}
	defer tx.Rollback()
	var jobState string
	err = tx.QueryRowContext(ctx, `SELECT state FROM jobs WHERE id = ?`, jobID).Scan(&jobState)
	if errors.Is(err, sql.ErrNoRows) {
		return retry, ErrNotFound
	}
	if err != nil {
		return retry, unavailable(err)
	}
	if jobState != protocol.JobAcceptedUnpublished {
		return retry, conflict("publish_retry_not_allowed",
			"only an accepted_unpublished job has a publish-only retry")
	}
	var attemptID, attemptState string
	var workerID sql.NullString
	err = tx.QueryRowContext(ctx, `
		SELECT id, state, worker_id FROM attempts
		WHERE job_id = ? ORDER BY attempt_number DESC LIMIT 1
	`, jobID).Scan(&attemptID, &attemptState, &workerID)
	if errors.Is(err, sql.ErrNoRows) {
		return retry, conflict("publish_retry_not_allowed", "the job has no attempt to re-publish")
	}
	if err != nil {
		return retry, unavailable(err)
	}
	if attemptState != protocol.AttemptAcceptedUnpublished {
		return retry, conflict("publish_retry_not_allowed",
			"the job's latest attempt is not accepted_unpublished")
	}
	if !workerID.Valid || workerID.String != input.WorkerID {
		// The retained worktree, the manifest, and the local branch live on
		// the worker that ran the attempt. Handing the retry to a different
		// worker would republish from state it does not have.
		return retry, conflict("publish_retry_foreign_worker",
			"only the worker that ran the attempt may retry its publish")
	}
	expiry := now.Add(protocol.LeaseDuration).UnixMilli()
	result, err := tx.ExecContext(ctx, `
		UPDATE attempts
		SET state = 'running', lease_digest = ?, lease_expires_at = ?, completed_at = NULL, error = NULL
		WHERE id = ? AND state = 'accepted_unpublished'
	`, digestToken(input.LeaseToken), expiry, attemptID)
	if err != nil {
		if isConstraintViolation(err) {
			return retry, conflict("publish_retry_conflict",
				"the job already has a live attempt")
		}
		return retry, unavailable(err)
	}
	if changed, _ := result.RowsAffected(); changed != 1 {
		return retry, conflict("publish_retry_conflict",
			"the attempt left accepted_unpublished during the retry")
	}
	result, err = tx.ExecContext(ctx, `
		UPDATE jobs SET state = 'active', cancellation_requested = 0, updated_at = ?
		WHERE id = ? AND state = 'accepted_unpublished'
	`, now.UnixMilli(), jobID)
	if err != nil {
		return retry, unavailable(err)
	}
	if changed, _ := result.RowsAffected(); changed != 1 {
		return retry, conflict("publish_retry_conflict",
			"the job left accepted_unpublished during the retry")
	}
	if err := tx.Commit(); err != nil {
		return retry, unavailable(err)
	}
	attempt, err := s.Attempt(ctx, attemptID)
	if err != nil {
		return retry, err
	}
	job, err := s.Job(ctx, jobID)
	if err != nil {
		return retry, err
	}
	records, err := s.AttemptPublishRecords(ctx, attemptID)
	if err != nil {
		return retry, err
	}
	if err := s.db.QueryRowContext(ctx,
		`SELECT snapshot FROM runs WHERE id = ?`, job.RunID).Scan(&retry.Snapshot); err != nil {
		return retry, unavailable(err)
	}
	retry.Attempt = attempt
	retry.Job = job
	retry.Records = records
	return retry, nil
}

// ---- HTTP surface ----------------------------------------------------------

// registerPublishRoutes attaches the publish surface to the control plane's
// routing table. It is the one line publish adds to http.go: every handler
// and every decision below it lives here, beside the store methods it calls.
func (a *API) registerPublishRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/attempts/{attempt_id}/publish/authorize", a.authorizePublishStep)
	mux.HandleFunc("POST /api/attempts/{attempt_id}/publish/record", a.recordPublishStep)
	mux.HandleFunc("GET /api/attempts/{attempt_id}/publish", a.attemptPublishRecords)
	mux.HandleFunc("GET /api/jobs/{job_id}/publish", a.jobPublishRecords)
	mux.HandleFunc("POST /api/jobs/{job_id}/publish-retry", a.retryPublish)
	a.registerCIRepairRoutes(mux)
}

func (a *API) authorizePublishStep(w http.ResponseWriter, r *http.Request) {
	if !a.prepareMutation(w, r) {
		return
	}
	var input protocol.PublishAuthorizationRequest
	if !decodeJSON(w, r, &input) {
		return
	}
	authorization, err := a.store.AuthorizePublishStep(r.Context(), r.PathValue("attempt_id"), input)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, authorization)
}

func (a *API) recordPublishStep(w http.ResponseWriter, r *http.Request) {
	if !a.prepareMutation(w, r) {
		return
	}
	var input protocol.PublishStepRequest
	if !decodeJSON(w, r, &input) {
		return
	}
	record, err := a.store.RecordPublishStep(r.Context(), r.PathValue("attempt_id"), input)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, record)
}

func (a *API) attemptPublishRecords(w http.ResponseWriter, r *http.Request) {
	records, err := a.store.AttemptPublishRecords(r.Context(), r.PathValue("attempt_id"))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, records)
}

func (a *API) jobPublishRecords(w http.ResponseWriter, r *http.Request) {
	records, err := a.store.JobPublishRecords(r.Context(), r.PathValue("job_id"))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, records)
}

func (a *API) retryPublish(w http.ResponseWriter, r *http.Request) {
	if !a.prepareMutation(w, r) {
		return
	}
	var input protocol.PublishRetryRequest
	if !decodeJSON(w, r, &input) {
		return
	}
	retry, err := a.store.RetryPublish(r.Context(), r.PathValue("job_id"), input)
	if err != nil {
		writeError(w, err)
		return
	}
	a.logger.Info("state_change", "resource_type", "job", "resource_id", retry.Job.ID,
		"new_state", retry.Job.State, "action", "publish_retry")
	writeJSON(w, http.StatusOK, retry)
}
