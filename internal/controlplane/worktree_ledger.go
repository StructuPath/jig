// worktree_ledger.go — the control-plane side of the retained-worktree
// ledger (U3, R16). Rows enter from worker registration payloads and
// reconciliation reports, feed the claim transaction's per-repository
// skip-over cap (R4), and leave `retained` only through the operator release
// action or a reconciliation that proves the disk copy gone. The ledger
// never deletes anything itself: it is bookkeeping for fail-closed workers.
package controlplane

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/StructuPath/jig/internal/protocol"
)

// applyRetainedWorktrees upserts ledger rows for a worker's reported
// retained worktrees. Rows are scoped to attempts the control plane knows
// and this worker owns — a report for a foreign or unknown attempt is
// skipped and named, never written. An operator-released row is never
// flipped back to retained by a worker report; a lost row that reappears on
// disk becomes retained again.
func (s *Store) applyRetainedWorktrees(ctx context.Context, tx *sql.Tx, workerID string, retained []protocol.RetainedWorktree) ([]string, error) {
	var skipped []string
	now := s.now().UnixMilli()
	for _, entry := range retained {
		if strings.TrimSpace(entry.AttemptID) == "" || strings.TrimSpace(entry.Path) == "" ||
			strings.TrimSpace(entry.Repository) == "" {
			return nil, invalid("invalid_retained_worktree",
				"retained worktrees require attempt_id, repository, and path")
		}
		result, err := tx.ExecContext(ctx, `
			INSERT INTO retained_worktrees(attempt_id, worker_id, repository, path, reason, state, created_at, updated_at)
			SELECT a.id, ?, ?, ?, ?, 'retained', ?, ?
			FROM attempts a WHERE a.id = ? AND a.worker_id = ?
			ON CONFLICT(attempt_id) DO UPDATE SET
				repository = excluded.repository,
				path = excluded.path,
				reason = excluded.reason,
				state = CASE WHEN retained_worktrees.state = 'released'
					THEN 'released' ELSE 'retained' END,
				updated_at = excluded.updated_at
			WHERE retained_worktrees.worker_id = excluded.worker_id
		`, workerID, entry.Repository, entry.Path, boundedReason(entry.Reason), now, now,
			entry.AttemptID, workerID)
		if err != nil {
			return nil, unavailable(err)
		}
		if changed, _ := result.RowsAffected(); changed == 0 {
			skipped = append(skipped, entry.AttemptID)
		}
	}
	return skipped, nil
}

// ReconcileWorktrees applies one worker reconciliation report (R16) in one
// transaction: retained rows upsert, missing attempt IDs mark their retained
// rows lost, orphan paths are acknowledged untouched. The answer is the
// worker's complete ledger view after the report.
func (s *Store) ReconcileWorktrees(ctx context.Context, workerID string, report protocol.WorktreeReconciliationReport) (protocol.WorktreeReconciliationResult, error) {
	var result protocol.WorktreeReconciliationResult
	now := s.now().UnixMilli()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return result, unavailable(err)
	}
	defer tx.Rollback()
	var exists int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM workers WHERE id = ?`, workerID).Scan(&exists); err != nil {
		return result, unavailable(err)
	}
	if exists == 0 {
		return result, ErrNotFound
	}
	if _, err := s.applyRetainedWorktrees(ctx, tx, workerID, report.Retained); err != nil {
		return result, err
	}
	for _, attemptID := range report.MissingAttemptIDs {
		if strings.TrimSpace(attemptID) == "" {
			return result, invalid("invalid_missing_attempt", "missing attempt IDs must be non-empty")
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE retained_worktrees SET state = 'lost', updated_at = ?
			WHERE attempt_id = ? AND worker_id = ? AND state = 'retained'
		`, now, attemptID, workerID); err != nil {
			return result, unavailable(err)
		}
	}
	if err := tx.Commit(); err != nil {
		return result, unavailable(err)
	}
	ledger, err := s.WorkerWorktrees(ctx, workerID)
	if err != nil {
		return result, err
	}
	result.Ledger = ledger
	result.OrphanPaths = append([]string(nil), report.OrphanPaths...)
	return result, nil
}

// WorkerWorktrees lists one worker's ledger rows, oldest first.
func (s *Store) WorkerWorktrees(ctx context.Context, workerID string) ([]protocol.WorktreeLedgerEntry, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT attempt_id, worker_id, repository, path, reason, state, created_at, updated_at
		FROM retained_worktrees WHERE worker_id = ?
		ORDER BY created_at, attempt_id
	`, workerID)
	if err != nil {
		return nil, unavailable(err)
	}
	defer rows.Close()
	entries := []protocol.WorktreeLedgerEntry{}
	for rows.Next() {
		entry, err := scanWorktreeLedgerEntry(rows)
		if err != nil {
			return nil, unavailable(err)
		}
		entries = append(entries, entry)
	}
	if err := rows.Err(); err != nil {
		return nil, unavailable(err)
	}
	return entries, nil
}

// ReleaseWorktree is the operator release action (R16): it moves one
// retained ledger row to `released`, and only with an explicit inspection
// confirmation. Releasing an already-released row replays the stored state;
// a lost row has nothing on disk to release and conflicts.
func (s *Store) ReleaseWorktree(ctx context.Context, attemptID string, input protocol.WorktreeReleaseRequest) (protocol.WorktreeLedgerEntry, error) {
	if !input.Confirm {
		return protocol.WorktreeLedgerEntry{}, invalid("release_requires_confirmation",
			"releasing a retained worktree requires {\"confirm\": true} after inspection")
	}
	now := s.now().UnixMilli()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return protocol.WorktreeLedgerEntry{}, unavailable(err)
	}
	defer tx.Rollback()
	var state string
	err = tx.QueryRowContext(ctx, `
		SELECT state FROM retained_worktrees WHERE attempt_id = ?
	`, attemptID).Scan(&state)
	if errors.Is(err, sql.ErrNoRows) {
		return protocol.WorktreeLedgerEntry{}, ErrNotFound
	}
	if err != nil {
		return protocol.WorktreeLedgerEntry{}, unavailable(err)
	}
	switch state {
	case protocol.WorktreeRetained:
		if _, err := tx.ExecContext(ctx, `
			UPDATE retained_worktrees SET state = 'released', updated_at = ?
			WHERE attempt_id = ? AND state = 'retained'
		`, now, attemptID); err != nil {
			return protocol.WorktreeLedgerEntry{}, unavailable(err)
		}
	case protocol.WorktreeReleased:
		// Replayed release: the stored state wins.
	default:
		return protocol.WorktreeLedgerEntry{}, conflict("worktree_not_retained",
			fmt.Sprintf("worktree is %q; only a retained worktree can be released", state))
	}
	if err := tx.Commit(); err != nil {
		return protocol.WorktreeLedgerEntry{}, unavailable(err)
	}
	return s.worktreeLedgerEntry(ctx, attemptID)
}

func (s *Store) worktreeLedgerEntry(ctx context.Context, attemptID string) (protocol.WorktreeLedgerEntry, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT attempt_id, worker_id, repository, path, reason, state, created_at, updated_at
		FROM retained_worktrees WHERE attempt_id = ?
	`, attemptID)
	entry, err := scanWorktreeLedgerEntry(row)
	if errors.Is(err, sql.ErrNoRows) {
		return entry, ErrNotFound
	}
	if err != nil {
		return entry, unavailable(err)
	}
	return entry, nil
}

func scanWorktreeLedgerEntry(row rowScanner) (protocol.WorktreeLedgerEntry, error) {
	var entry protocol.WorktreeLedgerEntry
	var createdAt, updatedAt int64
	err := row.Scan(&entry.AttemptID, &entry.WorkerID, &entry.Repository, &entry.Path,
		&entry.Reason, &entry.State, &createdAt, &updatedAt)
	if err != nil {
		return entry, err
	}
	entry.CreatedAt = fromMillis(createdAt)
	entry.UpdatedAt = fromMillis(updatedAt)
	return entry, nil
}

func boundedReason(value string) string {
	if len(value) > protocol.MaxRetentionReasonBytes {
		return value[:protocol.MaxRetentionReasonBytes]
	}
	return value
}
