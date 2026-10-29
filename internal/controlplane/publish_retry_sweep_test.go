// publish_retry_sweep_test.go — an interrupted publish-only retry stays
// recoverable (R14): the sweeper returns its attempt to accepted_unpublished,
// same attempt and records, instead of `lost`, and the interrupted publisher
// is fenced out. The publish_retry marker — not the stored result — is the
// discriminator, because an accepted_unpublished attempt may have no result.
package controlplane

import (
	"context"
	"database/sql"
	"sort"
	"strings"
	"testing"

	"github.com/StructuPath/jig/internal/protocol"
	"github.com/StructuPath/jig/migrations"
)

const tokenC = "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"

// sweepUntilSettled ticks the sweeper at heartbeat cadence until the attempt
// leaves `running`, and returns what the sweeper reported.
func sweepUntilSettled(t *testing.T, store *Store, clock *testClock, attemptID string) []LostAttempt {
	t.Helper()
	sweeper := NewSweeper(store)
	maxTicks := int(protocol.LeaseDuration/protocol.HeartbeatInterval) +
		protocol.MissedHeartbeatsBeforeSweep + 2
	var reported []LostAttempt
	for i := 0; i < maxTicks; i++ {
		clock.Advance(protocol.HeartbeatInterval)
		swept, err := sweeper.Tick(context.Background())
		if err != nil {
			t.Fatalf("sweep tick: %v", err)
		}
		reported = append(reported, swept...)
		attempt, err := store.Attempt(context.Background(), attemptID)
		if err != nil {
			t.Fatalf("read attempt: %v", err)
		}
		if attempt.State != protocol.AttemptRunning {
			return reported
		}
	}
	t.Fatal("the sweeper never settled the attempt")
	return nil
}

func publishRetryMarker(t *testing.T, store *Store, attemptID string) int {
	t.Helper()
	var marker int
	if err := store.db.QueryRow(`SELECT publish_retry FROM attempts WHERE id = ?`, attemptID).Scan(&marker); err != nil {
		t.Fatalf("read publish_retry: %v", err)
	}
	return marker
}

func storedResult(t *testing.T, store *Store, attemptID string) sql.NullString {
	t.Helper()
	var result sql.NullString
	if err := store.db.QueryRow(`SELECT result FROM attempts WHERE id = ?`, attemptID).Scan(&result); err != nil {
		t.Fatalf("read result: %v", err)
	}
	return result
}

// An interrupted retry — from a previous result or from none at all — is
// swept back to accepted_unpublished on the same attempt, with its publish
// records, its result, and the run aggregate exactly as before the retry. The
// interrupted publisher can no longer heartbeat, record, or complete, and a
// second retry re-enters the same attempt where the first stopped.
func TestAnInterruptedPublishRetryIsSweptBackToAcceptedUnpublished(t *testing.T) {
	for name, result := range map[string]string{
		"previous result":    `{"changed_paths":["a.txt"]}`,
		"no previous result": "",
	} {
		t.Run(name, func(t *testing.T) {
			store, clock := newTestStore(t)
			ctx := context.Background()
			registerTestWorker(t, store, 1)
			seedRun(t, store, "run-1", protocol.RunTarget{Repository: repoA, BaseSHA: shaA})
			claim := claimAndStart(t, store, "run-1", repoA)
			if _, err := recordStep(store, claim, tokenA, protocol.PublishStepPush, shaB, ""); err != nil {
				t.Fatalf("record push: %v", err)
			}
			if _, err := store.CompleteAttempt(ctx, claim.Attempt.ID, protocol.CompleteAttemptRequest{
				LeaseToken: tokenA, State: protocol.AttemptAcceptedUnpublished, Result: result,
			}); err != nil {
				t.Fatalf("complete accepted_unpublished: %v", err)
			}
			before := storedResult(t, store, claim.Attempt.ID)
			runBefore, err := store.Run(ctx, "run-1")
			if err != nil {
				t.Fatal(err)
			}

			if _, err := store.RetryPublish(ctx, claim.Job.ID,
				protocol.PublishRetryRequest{WorkerID: workerA, LeaseToken: tokenB}); err != nil {
				t.Fatalf("publish retry: %v", err)
			}
			if marker := publishRetryMarker(t, store, claim.Attempt.ID); marker != 1 {
				t.Fatalf("publish_retry = %d on the re-leased attempt, want 1", marker)
			}
			// While the interrupted retry still holds its lease, a second retry
			// is refused with a reason an operator can act on.
			if _, err := store.RetryPublish(ctx, claim.Job.ID,
				protocol.PublishRetryRequest{WorkerID: workerA, LeaseToken: tokenC}); err == nil ||
				serviceCode(t, err) != "publish_retry_in_progress" {
				t.Fatalf("retry over a live retry lease: err=%v, want publish_retry_in_progress", err)
			}

			// The retry's process dies: no heartbeat, no completion.
			swept := sweepUntilSettled(t, store, clock, claim.Attempt.ID)
			if len(swept) != 1 || swept[0].AttemptID != claim.Attempt.ID ||
				swept[0].State != protocol.AttemptAcceptedUnpublished {
				t.Fatalf("sweeper reported %+v, want this attempt returned to accepted_unpublished", swept)
			}
			attempt, err := store.Attempt(ctx, claim.Attempt.ID)
			if err != nil {
				t.Fatal(err)
			}
			job, err := store.Job(ctx, claim.Job.ID)
			if err != nil {
				t.Fatal(err)
			}
			if attempt.State != protocol.AttemptAcceptedUnpublished || job.State != protocol.JobAcceptedUnpublished {
				t.Fatalf("attempt=%q job=%q, want accepted_unpublished for both", attempt.State, job.State)
			}
			if !strings.Contains(attempt.Error, "publish retry interrupted") {
				t.Fatalf("attempt error = %q, want the interruption named", attempt.Error)
			}
			if after := storedResult(t, store, claim.Attempt.ID); after != before {
				t.Fatalf("result changed across the sweep: %+v -> %+v", before, after)
			}
			if marker := publishRetryMarker(t, store, claim.Attempt.ID); marker != 0 {
				t.Fatalf("publish_retry = %d after the sweep, want 0", marker)
			}
			if runAfter, err := store.Run(ctx, "run-1"); err != nil || runAfter.State != runBefore.State {
				t.Fatalf("run state %q -> %q (err %v), want it unchanged by the recovery",
					runBefore.State, runAfter.State, err)
			}
			records, err := store.AttemptPublishRecords(ctx, claim.Attempt.ID)
			if err != nil || len(records) != 1 || records[0].RemoteRef != shaB {
				t.Fatalf("publish records = %+v (err %v), want the proven push kept", records, err)
			}

			// The interrupted publisher wakes up: every fenced write refuses it.
			if _, err := store.Heartbeat(ctx, claim.Attempt.ID,
				protocol.HeartbeatRequest{LeaseToken: tokenB}); err == nil {
				t.Fatal("a swept retry's heartbeat renewed its lease")
			}
			if _, err := recordStep(store, claim, tokenB, protocol.PublishStepPullRequest, "",
				"https://github.com/example/repo-a/pull/7"); err == nil ||
				serviceCode(t, err) != "lease_not_owner" {
				t.Fatalf("a swept retry recorded a step: err=%v, want lease_not_owner", err)
			}
			if _, err := store.CompleteAttempt(ctx, claim.Attempt.ID, protocol.CompleteAttemptRequest{
				LeaseToken: tokenB, State: protocol.AttemptAcceptedUnpublished, Result: "zombie",
			}); err == nil || serviceCode(t, err) != "lease_not_owner" {
				t.Fatalf("a swept retry completed: err=%v, want lease_not_owner", err)
			}

			// The retry is available again, on the same attempt.
			again, err := store.RetryPublish(ctx, claim.Job.ID,
				protocol.PublishRetryRequest{WorkerID: workerA, LeaseToken: tokenC})
			if err != nil {
				t.Fatalf("publish retry after recovery: %v", err)
			}
			if again.Attempt.ID != claim.Attempt.ID || len(again.Records) != 1 {
				t.Fatalf("second retry = attempt %s records %+v, want the same attempt and its push",
					again.Attempt.ID, again.Records)
			}
			// The superseded first token stays fenced out of the new lease.
			if _, err := store.Heartbeat(ctx, claim.Attempt.ID,
				protocol.HeartbeatRequest{LeaseToken: tokenB}); err == nil ||
				serviceCode(t, err) != "lease_not_owner" {
				t.Fatalf("the first retry's token renewed the second's lease: err=%v", err)
			}
			if _, err := store.CompleteAttempt(ctx, claim.Attempt.ID, protocol.CompleteAttemptRequest{
				LeaseToken: tokenC, State: protocol.AttemptAcceptedUnpublished, Result: result,
			}); err != nil {
				t.Fatalf("complete the second retry: %v", err)
			}
			if marker := publishRetryMarker(t, store, claim.Attempt.ID); marker != 0 {
				t.Fatalf("publish_retry = %d after completion, want 0", marker)
			}
			var attempts int
			if err := store.db.QueryRow(`SELECT COUNT(*) FROM attempts WHERE job_id = ?`,
				claim.Job.ID).Scan(&attempts); err != nil || attempts != 1 {
				t.Fatalf("the job holds %d attempts (err %v), want 1", attempts, err)
			}
		})
	}
}

// Without the marker an expired running attempt is lost exactly as before —
// even one whose result column happens to be set, which is why the result is
// not the discriminator. An ordinary attempt that runs CI repair rounds is an
// ordinary running attempt and is covered by the same path.
func TestAnExpiredUnmarkedAttemptIsStillLost(t *testing.T) {
	store, clock := newTestStore(t)
	registerTestWorker(t, store, 1)
	seedRun(t, store, "run-1", protocol.RunTarget{Repository: repoA, BaseSHA: shaA})
	claim := claimAndStart(t, store, "run-1", repoA)
	if _, err := store.db.Exec(`UPDATE attempts SET result = '{"partial":true}' WHERE id = ?`,
		claim.Attempt.ID); err != nil {
		t.Fatal(err)
	}
	swept := sweepUntilSettled(t, store, clock, claim.Attempt.ID)
	if len(swept) != 1 || swept[0].State != protocol.AttemptLost {
		t.Fatalf("sweeper reported %+v, want the attempt lost", swept)
	}
	job, err := store.Job(context.Background(), claim.Job.ID)
	if err != nil || job.State != protocol.JobFailed {
		t.Fatalf("job = %q (err %v), want failed", job.State, err)
	}
	if _, err := store.RetryPublish(context.Background(), claim.Job.ID,
		protocol.PublishRetryRequest{WorkerID: workerA, LeaseToken: tokenB}); err == nil ||
		serviceCode(t, err) != "publish_retry_not_allowed" {
		t.Fatalf("publish retry of a lost attempt: err=%v, want publish_retry_not_allowed", err)
	}
}

// Migration 006 is additive: rows written under 005 keep every column, and
// read back unmarked.
func TestThePublishRetryMarkerMigrationKeepsExistingRows(t *testing.T) {
	path := t.TempDir() + "/jig.db"
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE schema_migrations (version INTEGER PRIMARY KEY, applied_at INTEGER NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	entries, err := migrations.Files.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	for _, entry := range entries {
		version, err := migrationVersion(entry.Name())
		if err != nil || version >= 6 {
			continue
		}
		body, err := migrations.Files.ReadFile(entry.Name())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(string(body)); err != nil {
			t.Fatalf("apply %s: %v", entry.Name(), err)
		}
		if _, err := db.Exec(`INSERT INTO schema_migrations(version, applied_at) VALUES (?, 0)`, version); err != nil {
			t.Fatal(err)
		}
	}
	jobID := seedJob(t, db)
	if _, err := db.Exec(`
		INSERT INTO attempts(id, job_id, worker_id, attempt_number, state, result, error, created_at)
		VALUES ('attempt-1', ?, 'worker-1', 1, 'accepted_unpublished', '{"kept":true}', 'gh_failed', 7)
	`, jobID); err != nil {
		t.Fatal(err)
	}

	if err := Migrate(context.Background(), db); err != nil {
		t.Fatalf("migrate to 006: %v", err)
	}
	var state, result, errorText string
	var createdAt int64
	var marker int
	if err := db.QueryRow(`SELECT state, result, error, created_at, publish_retry FROM attempts WHERE id = 'attempt-1'`).
		Scan(&state, &result, &errorText, &createdAt, &marker); err != nil {
		t.Fatalf("read migrated row: %v", err)
	}
	if state != "accepted_unpublished" || result != `{"kept":true}` || errorText != "gh_failed" ||
		createdAt != 7 || marker != 0 {
		t.Fatalf("migrated row = %s %s %s %d marker=%d, want it unchanged and unmarked",
			state, result, errorText, createdAt, marker)
	}
	if _, err := db.Exec(`UPDATE attempts SET publish_retry = 2 WHERE id = 'attempt-1'`); err == nil {
		t.Fatal("publish_retry accepted a value other than 0 or 1")
	}
}
