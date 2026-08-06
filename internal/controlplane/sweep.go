// sweep.go — lease expiry sweeping and heartbeat renewal (U2, R5).
//
// The design premise: an expired lease is evidence, not a verdict. Sweep
// marks an attempt `lost` only after MissedHeartbeatsBeforeSweep consecutive
// missed heartbeats counted against server uptime — the sweeper tracks its
// own tick-to-tick deltas and resets every count when a gap says the server
// itself was down, so an 8-hour laptop sleep of server and worker together
// wakes to an intact attempt whose first heartbeat renews the expired lease.
// All sweep decisions ride time deltas between observations, never absolute
// wall-clock comparisons against pre-sleep timestamps.
package controlplane

import (
	"context"
	"database/sql"
	"log/slog"
	"sync"
	"time"

	"github.com/StructuPath/jig/internal/protocol"
)

// LostAttempt names one attempt the sweeper marked lost and its job.
type LostAttempt struct {
	AttemptID string
	JobID     string
}

// Sweeper owns the missed-heartbeat bookkeeping. It is not persistent by
// design: a server restart resets every count, which is exactly R5's
// "counted after server uptime resumes".
type Sweeper struct {
	store *Store

	mu       sync.Mutex
	lastTick time.Time
	misses   map[string]int
}

func NewSweeper(store *Store) *Sweeper {
	return &Sweeper{store: store, misses: make(map[string]int)}
}

// Run ticks the sweeper at HeartbeatInterval — the cadence that makes one
// tick-with-expired-lease equal one missed heartbeat — until ctx ends.
func (sw *Sweeper) Run(ctx context.Context, logger *slog.Logger) {
	if logger == nil {
		logger = slog.Default()
	}
	ticker := time.NewTicker(protocol.HeartbeatInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			lost, err := sw.Tick(ctx)
			if err != nil {
				logger.Error("lease_sweep_failed", "error", err)
				continue
			}
			for _, value := range lost {
				logger.Info("state_change",
					"resource_type", "attempt", "resource_id", value.AttemptID,
					"job_id", value.JobID, "new_state", "lost")
			}
		}
	}
}

// Tick performs one sweep pass. Callers outside Run (tests, an operator
// action) must respect that a tick represents one HeartbeatInterval of
// observed server uptime.
func (sw *Sweeper) Tick(ctx context.Context) ([]LostAttempt, error) {
	sw.mu.Lock()
	defer sw.mu.Unlock()
	now := sw.store.now()
	// A gap between our own ticks means the server was not observing —
	// asleep, suspended, or stopped. Counting restarts (R5). The delta uses
	// the monotonic clock when the process stayed alive across a wall-clock
	// jump, so a jump alone never fabricates uptime.
	if !sw.lastTick.IsZero() && now.Sub(sw.lastTick) > protocol.SweeperUptimeGap {
		sw.misses = make(map[string]int)
	}
	sw.lastTick = now
	nowMillis := now.UnixMilli()

	// Expired empty claim answers age out here (R4).
	if _, err := sw.store.db.ExecContext(ctx, `
		DELETE FROM claim_requests WHERE attempt_id IS NULL AND created_at < ?
	`, nowMillis-protocol.EmptyClaimTTL.Milliseconds()); err != nil {
		return nil, unavailable(err)
	}

	rows, err := sw.store.db.QueryContext(ctx, `
		SELECT id, lease_expires_at FROM attempts
		WHERE state IN ('preparing', 'running')
	`)
	if err != nil {
		return nil, unavailable(err)
	}
	leased := make(map[string]bool)
	var overdue []string
	for rows.Next() {
		var id string
		var expiry sql.NullInt64
		if err := rows.Scan(&id, &expiry); err != nil {
			rows.Close()
			return nil, unavailable(err)
		}
		leased[id] = true
		if expiry.Valid && expiry.Int64 <= nowMillis {
			overdue = append(overdue, id)
		} else {
			// A renewal happened since the last tick: the run of misses ends.
			delete(sw.misses, id)
		}
	}
	if err := rows.Close(); err != nil {
		return nil, unavailable(err)
	}
	for id := range sw.misses {
		if !leased[id] {
			delete(sw.misses, id)
		}
	}
	var lost []LostAttempt
	for _, id := range overdue {
		sw.misses[id]++
		if sw.misses[id] < protocol.MissedHeartbeatsBeforeSweep {
			continue
		}
		value, swept, err := sw.store.sweepAttempt(ctx, id, nowMillis)
		if err != nil {
			return lost, err
		}
		delete(sw.misses, id)
		if swept {
			lost = append(lost, value)
		}
	}
	return lost, nil
}

// sweepAttempt marks one attempt lost and its job failed in one transaction.
// The guarded UPDATE re-checks state and expiry inside the transaction, so a
// heartbeat that renewed the lease between our read and this write wins and
// the sweep is a no-op (KTD3).
func (s *Store) sweepAttempt(ctx context.Context, attemptID string, nowMillis int64) (LostAttempt, bool, error) {
	value := LostAttempt{AttemptID: attemptID}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return value, false, unavailable(err)
	}
	defer tx.Rollback()
	if err := tx.QueryRowContext(ctx, `
		SELECT job_id FROM attempts WHERE id = ?
	`, attemptID).Scan(&value.JobID); err != nil {
		return value, false, unavailable(err)
	}
	result, err := tx.ExecContext(ctx, `
		UPDATE attempts SET state = 'lost', error = 'lease expired: consecutive heartbeats missed', completed_at = ?
		WHERE id = ? AND state IN ('preparing', 'running') AND lease_expires_at <= ?
	`, nowMillis, attemptID, nowMillis)
	if err != nil {
		return value, false, unavailable(err)
	}
	changed, _ := result.RowsAffected()
	if changed != 1 {
		// A heartbeat renewed the lease (or a completion landed) after our
		// read: the attempt survives and this sweep is a no-op.
		if err := tx.Commit(); err != nil {
			return value, false, unavailable(err)
		}
		return value, false, nil
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE jobs SET state = 'failed', updated_at = ? WHERE id = ? AND state = 'active'
	`, nowMillis, value.JobID); err != nil {
		return value, false, unavailable(err)
	}
	if err := tx.Commit(); err != nil {
		return value, false, unavailable(err)
	}
	return value, true, nil
}

// Heartbeat renews an attempt's lease in one fenced transaction (R5, R6) and
// carries cancellation back to the worker. The token digest must own the
// attempt, and renewal is permitted — expired or not — iff the attempt has
// not transitioned out of its leased state and no successor attempt exists.
// That is what makes whole-machine sleep benign: the worker's first
// heartbeat after wake revives an expired-but-unswept lease, while a zombie
// whose job was already swept and retried is fenced out.
func (s *Store) Heartbeat(ctx context.Context, attemptID string, input protocol.HeartbeatRequest) (protocol.HeartbeatResponse, error) {
	if err := validateLeaseToken(input.LeaseToken); err != nil {
		return protocol.HeartbeatResponse{}, err
	}
	now := s.now()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return protocol.HeartbeatResponse{}, unavailable(err)
	}
	defer tx.Rollback()
	lease, err := loadLease(ctx, tx, attemptID)
	if err != nil {
		return protocol.HeartbeatResponse{}, err
	}
	if !equalDigest(lease.digest, digestToken(input.LeaseToken)) {
		return protocol.HeartbeatResponse{}, conflict("lease_not_owner", "the lease token does not own this attempt")
	}
	var successors int
	if err := tx.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM attempts WHERE job_id = ? AND attempt_number > ?
	`, lease.jobID, lease.attemptNumber).Scan(&successors); err != nil {
		return protocol.HeartbeatResponse{}, unavailable(err)
	}
	if successors > 0 {
		return protocol.HeartbeatResponse{}, conflict("lease_superseded",
			"a successor attempt exists; this lease cannot renew")
	}
	if !isLeasedState(lease.attemptState) {
		return protocol.HeartbeatResponse{}, conflict("attempt_transitioned",
			"the attempt already left its leased state")
	}
	expiry := now.Add(protocol.LeaseDuration)
	if _, err := tx.ExecContext(ctx, `
		UPDATE attempts SET lease_expires_at = ? WHERE id = ?
	`, expiry.UnixMilli(), attemptID); err != nil {
		return protocol.HeartbeatResponse{}, unavailable(err)
	}
	// Worker liveness rides attempt heartbeats (KTD12).
	if lease.workerID.Valid {
		if _, err := tx.ExecContext(ctx, `
			UPDATE workers SET last_heartbeat = ? WHERE id = ?
		`, now.UnixMilli(), lease.workerID.String); err != nil {
			return protocol.HeartbeatResponse{}, unavailable(err)
		}
	}
	if err := tx.Commit(); err != nil {
		return protocol.HeartbeatResponse{}, unavailable(err)
	}
	return protocol.HeartbeatResponse{
		LeaseExpiresAt:        expiry.UTC(),
		CancellationRequested: lease.cancel,
	}, nil
}
