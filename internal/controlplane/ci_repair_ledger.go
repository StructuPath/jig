// ci_repair_ledger.go — the control-plane side of CI repair rounds
// (publish.ci.on_fail). A round is a fix pushed to the attempt's own branch
// after CI went red, and like every publish side effect it is authorized
// before the push and recorded after it, both under the lease token.
//
// The ledger enforces what makes rounds safe to trust:
//   - only a definition that declares on_fail may have rounds, and only as
//     many as its frozen budget allows;
//   - rounds chain: round N repairs exactly the head jig last pushed (the
//     proof ref for round 1, round N-1's head_after after that), so a person's
//     push in between is refused rather than built on;
//   - no round once CI is recorded green, so a ci record always postdates
//     every round, and CompleteAttempt can refuse `accepted` on a green run
//     that names a head a round already found red.
package controlplane

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/StructuPath/jig/internal/protocol"
)

// validateCIRepairRequest checks everything about a round request that needs
// no database: the token, the round number, the branch, and the head shape.
func validateCIRepairRequest(token string, round int, branch, headBefore, expectedBranch string) error {
	if err := validateLeaseToken(token); err != nil {
		return err
	}
	if round < 1 {
		return invalid("invalid_ci_repair_round", "round must be 1 or more")
	}
	if len(branch) > protocol.MaxPublishBranchBytes {
		return invalid("invalid_publish_branch", "branch exceeds its storage limit")
	}
	if branch != "" && branch != expectedBranch {
		return invalid("publish_branch_mismatch", fmt.Sprintf(
			"an attempt may only publish its own attempt-scoped branch %q", expectedBranch))
	}
	if !publishRefPattern.MatchString(headBefore) {
		return invalid("invalid_publish_ref", "head_before must be a commit SHA")
	}
	return nil
}

// ciRepairBudget is the frozen definition's on_fail budget, or a conflict
// when the definition declares no repair.
func ciRepairBudget(ctx context.Context, tx *sql.Tx, attemptID string) (int, error) {
	spec, err := attemptDefinition(ctx, tx, attemptID)
	if err != nil {
		return 0, err
	}
	if !spec.WaitsForCI() || spec.Publish.CI.OnFail == nil {
		return 0, conflict("ci_repair_not_declared",
			"this attempt's definition declares no publish.ci.on_fail, so it has no repair rounds")
	}
	return spec.Publish.CI.OnFail.Budget, nil
}

// requireNextCIRepairRound enforces the chain for a round that is not yet
// recorded: CI not yet green, the round is the next one, within budget, and
// it repairs the head jig last pushed.
func requireNextCIRepairRound(ctx context.Context, tx *sql.Tx, attemptID string, round, budget int, headBefore string) error {
	var green int
	if err := tx.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM publish_records WHERE attempt_id = ? AND step = ?
	`, attemptID, protocol.PublishStepCI).Scan(&green); err != nil {
		return unavailable(err)
	}
	if green > 0 {
		return conflict("ci_repair_after_green", "CI is already recorded green; there is nothing to repair")
	}
	var lastRound int
	var pushed string
	err := tx.QueryRowContext(ctx, `
		SELECT round, head_after FROM publish_ci_repairs
		WHERE attempt_id = ? ORDER BY round DESC LIMIT 1
	`, attemptID).Scan(&lastRound, &pushed)
	if errors.Is(err, sql.ErrNoRows) {
		proof, proven, readErr := readPublishRecord(ctx, tx, attemptID, protocol.PublishStepProof)
		if readErr != nil {
			return readErr
		}
		if !proven {
			return conflict("publish_step_out_of_order",
				"a CI repair round requires the proof step to be recorded first")
		}
		pushed = proof.RemoteRef
	} else if err != nil {
		return unavailable(err)
	}
	if round != lastRound+1 {
		return conflict("ci_repair_out_of_order", fmt.Sprintf(
			"the next CI repair round is %d, not %d", lastRound+1, round))
	}
	if round > budget {
		return conflict("ci_repair_budget_exhausted", fmt.Sprintf(
			"the definition allows %d CI repair round(s)", budget))
	}
	if headBefore != pushed {
		return conflict("ci_repair_head_mismatch", fmt.Sprintf(
			"a round repairs the head jig last pushed (%s), not %s; "+
				"a different head means someone else pushed to the branch", pushed, headBefore))
	}
	return nil
}

func readCIRepair(ctx context.Context, tx *sql.Tx, attemptID string, round int) (protocol.CIRepairRecord, bool, error) {
	var record protocol.CIRepairRecord
	var checks string
	var completedAt int64
	err := tx.QueryRowContext(ctx, `
		SELECT r.attempt_id, a.job_id, a.attempt_number, r.round, r.branch,
		       r.head_before, r.head_after, r.failed_checks, r.completed_at
		FROM publish_ci_repairs r JOIN attempts a ON a.id = r.attempt_id
		WHERE r.attempt_id = ? AND r.round = ?
	`, attemptID, round).Scan(&record.AttemptID, &record.JobID, &record.AttemptNumber,
		&record.Round, &record.Branch, &record.HeadBefore, &record.HeadAfter, &checks, &completedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return record, false, nil
	}
	if err != nil {
		return record, false, unavailable(err)
	}
	if err := json.Unmarshal([]byte(checks), &record.FailedChecks); err != nil {
		return record, false, unavailable(err)
	}
	record.CompletedAt = fromMillis(completedAt)
	return record, true, nil
}

// AuthorizeCIRepair is the pre-push fence for one repair round. It reports
// the budget, and a recorded round as Completed so a re-entering worker
// skips the push instead of repeating it.
func (s *Store) AuthorizeCIRepair(
	ctx context.Context, attemptID string, input protocol.CIRepairAuthorizationRequest,
) (protocol.CIRepairAuthorization, error) {
	var authorization protocol.CIRepairAuthorization
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
	if err := validateCIRepairRequest(input.LeaseToken, input.Round, input.Branch,
		input.HeadBefore, value.expectedBranch); err != nil {
		return authorization, err
	}
	if err := verifyActiveLease(value.lease, input.LeaseToken, now); err != nil {
		return authorization, err
	}
	budget, err := ciRepairBudget(ctx, tx, attemptID)
	if err != nil {
		return authorization, err
	}
	record, exists, err := readCIRepair(ctx, tx, attemptID, input.Round)
	if err != nil {
		return authorization, err
	}
	if !exists {
		if err := requireNextCIRepairRound(ctx, tx, attemptID, input.Round, budget, input.HeadBefore); err != nil {
			return authorization, err
		}
	}
	if err := tx.Commit(); err != nil {
		return authorization, unavailable(err)
	}
	authorization = protocol.CIRepairAuthorization{
		AttemptID: attemptID, Round: input.Round, Budget: budget, Branch: value.expectedBranch,
	}
	if exists {
		authorization.Completed = &record
	}
	return authorization, nil
}

// RecordCIRepair records one pushed repair round under the lease token.
// Replaying identical values returns the stored record; replaying different
// values for a recorded round is a conflict.
func (s *Store) RecordCIRepair(
	ctx context.Context, attemptID string, input protocol.CIRepairRecordRequest,
) (protocol.CIRepairRecord, error) {
	var record protocol.CIRepairRecord
	if !publishRefPattern.MatchString(input.HeadAfter) {
		return record, invalid("invalid_publish_ref", "head_after must be a commit SHA")
	}
	if input.HeadAfter == input.HeadBefore {
		return record, invalid("invalid_ci_repair_head", "a round must push a new head")
	}
	if len(input.FailedChecks) > protocol.MaxCIRepairFailedChecks {
		return record, invalid("invalid_ci_repair_checks", fmt.Sprintf(
			"a round records at most %d failed check names", protocol.MaxCIRepairFailedChecks))
	}
	for _, name := range input.FailedChecks {
		if len(name) > protocol.MaxCIRepairCheckNameBytes {
			return record, invalid("invalid_ci_repair_checks", "a failed check name exceeds its storage limit")
		}
	}
	checks := input.FailedChecks
	if checks == nil {
		checks = []string{}
	}
	encodedChecks, err := json.Marshal(checks)
	if err != nil {
		return record, invalid("invalid_ci_repair_checks", err.Error())
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
	if err := validateCIRepairRequest(input.LeaseToken, input.Round, input.Branch,
		input.HeadBefore, value.expectedBranch); err != nil {
		return record, err
	}
	if err := verifyActiveLease(value.lease, input.LeaseToken, now); err != nil {
		return record, err
	}
	budget, err := ciRepairBudget(ctx, tx, attemptID)
	if err != nil {
		return record, err
	}
	existing, exists, err := readCIRepair(ctx, tx, attemptID, input.Round)
	if err != nil {
		return record, err
	}
	if exists {
		existingChecks, _ := json.Marshal(existing.FailedChecks)
		if existing.HeadBefore != input.HeadBefore || existing.HeadAfter != input.HeadAfter ||
			string(existingChecks) != string(encodedChecks) {
			return record, conflict("ci_repair_conflict", fmt.Sprintf(
				"CI repair round %d is already recorded with a different result", input.Round))
		}
		if err := tx.Commit(); err != nil {
			return record, unavailable(err)
		}
		return existing, nil
	}
	if err := requireNextCIRepairRound(ctx, tx, attemptID, input.Round, budget, input.HeadBefore); err != nil {
		return record, err
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO publish_ci_repairs(attempt_id, round, branch, head_before, head_after, failed_checks, completed_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
	`, attemptID, input.Round, value.expectedBranch, input.HeadBefore, input.HeadAfter,
		string(encodedChecks), now); err != nil {
		if isConstraintViolation(err) {
			return record, conflict("ci_repair_conflict", "a concurrent publish already recorded this round")
		}
		return record, unavailable(err)
	}
	written, _, err := readCIRepair(ctx, tx, attemptID, input.Round)
	if err != nil {
		return record, err
	}
	if err := tx.Commit(); err != nil {
		return record, unavailable(err)
	}
	return written, nil
}

// AttemptCIRepairs lists one attempt's recorded repair rounds in order.
func (s *Store) AttemptCIRepairs(ctx context.Context, attemptID string) ([]protocol.CIRepairRecord, error) {
	var exists int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM attempts WHERE id = ?`, attemptID).Scan(&exists); err != nil {
		return nil, unavailable(err)
	}
	if exists == 0 {
		return nil, ErrNotFound
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT r.attempt_id, a.job_id, a.attempt_number, r.round, r.branch,
		       r.head_before, r.head_after, r.failed_checks, r.completed_at
		FROM publish_ci_repairs r JOIN attempts a ON a.id = r.attempt_id
		WHERE r.attempt_id = ? ORDER BY r.round
	`, attemptID)
	if err != nil {
		return nil, unavailable(err)
	}
	defer rows.Close()
	records := []protocol.CIRepairRecord{}
	for rows.Next() {
		var record protocol.CIRepairRecord
		var checks string
		var completedAt int64
		if err := rows.Scan(&record.AttemptID, &record.JobID, &record.AttemptNumber, &record.Round,
			&record.Branch, &record.HeadBefore, &record.HeadAfter, &checks, &completedAt); err != nil {
			return nil, unavailable(err)
		}
		if err := json.Unmarshal([]byte(checks), &record.FailedChecks); err != nil {
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

// requireCIGreenPastRepairs is CompleteAttempt's rule for attempts that ran
// repair rounds (R6): the green ci record must not name a head any round
// found red. It does not require the last pushed head, because a person may
// push a fix after the rounds run out and the publish-only retry must still
// be able to accept it. Ordering needs no check here: the ledger refuses a
// round once ci is recorded, so a ci record always postdates every round.
func requireCIGreenPastRepairs(ctx context.Context, tx *sql.Tx, attemptID string) error {
	var repaired int
	if err := tx.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM publish_ci_repairs r
		JOIN publish_records p ON p.attempt_id = r.attempt_id AND p.step = ?
		WHERE r.attempt_id = ? AND p.remote_ref = r.head_before
	`, protocol.PublishStepCI, attemptID).Scan(&repaired); err != nil {
		return unavailable(err)
	}
	if repaired > 0 {
		return conflict("publish_ci_on_repaired_head",
			"the recorded green CI names a head a repair round found red; "+
				"complete as accepted_unpublished and re-judge CI on the current head")
	}
	return nil
}

// ---- HTTP surface ----------------------------------------------------------

func (a *API) registerCIRepairRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/attempts/{attempt_id}/publish/ci-repair/authorize", a.authorizeCIRepair)
	mux.HandleFunc("POST /api/attempts/{attempt_id}/publish/ci-repair/record", a.recordCIRepair)
	mux.HandleFunc("GET /api/attempts/{attempt_id}/publish/ci-repairs", a.attemptCIRepairs)
}

func (a *API) authorizeCIRepair(w http.ResponseWriter, r *http.Request) {
	if !a.prepareMutation(w, r) {
		return
	}
	var input protocol.CIRepairAuthorizationRequest
	if !decodeJSON(w, r, &input) {
		return
	}
	authorization, err := a.store.AuthorizeCIRepair(r.Context(), r.PathValue("attempt_id"), input)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, authorization)
}

func (a *API) recordCIRepair(w http.ResponseWriter, r *http.Request) {
	if !a.prepareMutation(w, r) {
		return
	}
	var input protocol.CIRepairRecordRequest
	if !decodeJSON(w, r, &input) {
		return
	}
	record, err := a.store.RecordCIRepair(r.Context(), r.PathValue("attempt_id"), input)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, record)
}

func (a *API) attemptCIRepairs(w http.ResponseWriter, r *http.Request) {
	records, err := a.store.AttemptCIRepairs(r.Context(), r.PathValue("attempt_id"))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, records)
}
