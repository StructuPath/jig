package controlplane

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	"github.com/StructuPath/jig/migrations"
	_ "modernc.org/sqlite"
)

func openMigratedDatabase(t *testing.T) *sql.DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "jig.db")
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if err := Migrate(context.Background(), db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return db
}

func embeddedMigrationCount(t *testing.T) int {
	t.Helper()
	entries, err := migrations.Files.ReadDir(".")
	if err != nil {
		t.Fatalf("list embedded migrations: %v", err)
	}
	count := 0
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".sql") {
			count++
		}
	}
	return count
}

// seedJob inserts the foreign-key chain an attempt row needs:
// definition -> run -> job, plus one worker.
func seedJob(t *testing.T, db *sql.DB) (jobID string) {
	t.Helper()
	statements := []string{
		`INSERT INTO definitions(id, name, generation, source, created_at, updated_at)
		 VALUES ('def-1', 'smoke', 1, 'name: smoke', 0, 0)`,
		`INSERT INTO runs(id, definition_id, definition_generation, snapshot, state, created_at, updated_at)
		 VALUES ('run-1', 'def-1', 1, 'name: smoke', 'active', 0, 0)`,
		`INSERT INTO jobs(id, run_id, repository, base_sha, state, created_at, updated_at)
		 VALUES ('job-1', 'run-1', 'github.com/StructuPath/jig', 'abc123', 'queued', 0, 0)`,
		`INSERT INTO workers(id, name, worker_version, capacity, registered_at, last_heartbeat)
		 VALUES ('worker-1', 'local', 'dev', 2, 0, 0)`,
	}
	for _, statement := range statements {
		if _, err := db.Exec(statement); err != nil {
			t.Fatalf("seed: %v\n%s", err, statement)
		}
	}
	return "job-1"
}

func insertAttempt(db *sql.DB, id, jobID string, number int, state string) error {
	_, err := db.Exec(`
		INSERT INTO attempts(id, job_id, worker_id, attempt_number, state, created_at)
		VALUES (?, ?, 'worker-1', ?, ?, 0)`,
		id, jobID, number, state)
	return err
}

func TestMigratingAnEmptyDatabaseAppliesAllMigrationsAndRecordsTheLedger(t *testing.T) {
	db := openMigratedDatabase(t)
	var recorded int
	if err := db.QueryRow(`SELECT COUNT(*) FROM schema_migrations`).Scan(&recorded); err != nil {
		t.Fatalf("count ledger rows: %v", err)
	}
	if want := embeddedMigrationCount(t); recorded != want {
		t.Fatalf("ledger records %d migrations, embedded %d", recorded, want)
	}
	for _, table := range []string{
		"definitions", "runs", "jobs", "attempts", "claim_requests", "events",
		"envelopes", "gate_results", "workers", "retained_worktrees", "publish_records",
	} {
		var name string
		err := db.QueryRow(
			`SELECT name FROM sqlite_master WHERE type = 'table' AND name = ?`, table,
		).Scan(&name)
		if err != nil {
			t.Fatalf("expected table %q after migration: %v", table, err)
		}
	}
}

func TestRerunningMigrationsAgainstACurrentDatabaseIsANoOp(t *testing.T) {
	db := openMigratedDatabase(t)
	var before int
	if err := db.QueryRow(`SELECT COUNT(*) FROM schema_migrations`).Scan(&before); err != nil {
		t.Fatalf("count ledger rows: %v", err)
	}
	if err := Migrate(context.Background(), db); err != nil {
		t.Fatalf("re-running migrations should be a no-op, got: %v", err)
	}
	var after int
	if err := db.QueryRow(`SELECT COUNT(*) FROM schema_migrations`).Scan(&after); err != nil {
		t.Fatalf("count ledger rows: %v", err)
	}
	if before != after {
		t.Fatalf("re-run changed the ledger: %d rows before, %d after", before, after)
	}
}

func TestSecondActiveAttemptForOneJobViolatesThePartialUniqueIndex(t *testing.T) {
	db := openMigratedDatabase(t)
	jobID := seedJob(t, db)
	if err := insertAttempt(db, "attempt-1", jobID, 1, "running"); err != nil {
		t.Fatalf("first active attempt should insert: %v", err)
	}
	err := insertAttempt(db, "attempt-2", jobID, 2, "queued")
	if err == nil {
		t.Fatal("second active attempt for the same job inserted; the partial unique index must reject it")
	}
	if !strings.Contains(err.Error(), "one_active_attempt_per_job") &&
		!strings.Contains(strings.ToLower(err.Error()), "unique") {
		t.Fatalf("expected a unique-constraint violation, got: %v", err)
	}
	if _, err := db.Exec(`UPDATE attempts SET state = 'failed' WHERE id = 'attempt-1'`); err != nil {
		t.Fatalf("terminate first attempt: %v", err)
	}
	if err := insertAttempt(db, "attempt-2", jobID, 2, "queued"); err != nil {
		t.Fatalf("a new attempt after the first terminated should insert: %v", err)
	}
}

func TestDuplicateAttemptEventSequenceInsertIsIgnored(t *testing.T) {
	db := openMigratedDatabase(t)
	jobID := seedJob(t, db)
	if err := insertAttempt(db, "attempt-1", jobID, 1, "running"); err != nil {
		t.Fatalf("insert attempt: %v", err)
	}
	insertEvent := func(payload string) (int64, error) {
		result, err := db.Exec(`
			INSERT OR IGNORE INTO events(attempt_id, seq, type, phase, payload, payload_bytes, server_time)
			VALUES ('attempt-1', 7, 'log', 'build', ?, ?, 0)`,
			payload, len(payload))
		if err != nil {
			return 0, err
		}
		return result.RowsAffected()
	}
	if affected, err := insertEvent(`{"line":"original"}`); err != nil || affected != 1 {
		t.Fatalf("first insert: affected=%d err=%v", affected, err)
	}
	affected, err := insertEvent(`{"line":"replayed"}`)
	if err != nil {
		t.Fatalf("replayed insert must be ignored, not fail: %v", err)
	}
	if affected != 0 {
		t.Fatalf("replayed (attempt_id, seq) affected %d rows, want 0", affected)
	}
	var payload string
	if err := db.QueryRow(
		`SELECT payload FROM events WHERE attempt_id = 'attempt-1' AND seq = 7`,
	).Scan(&payload); err != nil {
		t.Fatalf("read event back: %v", err)
	}
	if payload != `{"line":"original"}` {
		t.Fatalf("replay overwrote the original payload: %q", payload)
	}
}

// 004 rebuilds publish_records to widen its step vocabulary with `ci`. A
// rebuild is only safe if it carries every existing row across, so this
// applies 001–003 by hand, records a proven step under the old schema, then
// applies 004 and checks the row survived and the new vocabulary holds.
func TestPublishCIStepMigrationPreservesRecordsAndWidensTheStepVocabulary(t *testing.T) {
	path := filepath.Join(t.TempDir(), "jig.db")
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	apply := func(name string) {
		t.Helper()
		body, err := migrations.Files.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if _, err := db.Exec(string(body)); err != nil {
			t.Fatalf("apply %s: %v", name, err)
		}
	}
	for _, name := range []string{"001_core.sql", "002_job_cancellation.sql", "003_admission_triggers.sql"} {
		apply(name)
	}
	jobID := seedJob(t, db)
	if err := insertAttempt(db, "attempt-1", jobID, 1, "running"); err != nil {
		t.Fatalf("insert attempt: %v", err)
	}
	insertStep := func(step string) error {
		_, err := db.Exec(`INSERT INTO publish_records(attempt_id, step, branch, remote_ref, completed_at)
			VALUES ('attempt-1', ?, 'jig/job-1/1', '1111111111111111111111111111111111111111', 7)`, step)
		return err
	}
	if err := insertStep("proof"); err != nil {
		t.Fatalf("record proof under the 003 schema: %v", err)
	}
	if err := insertStep("ci"); err == nil {
		t.Fatal("the 003 schema accepted a ci step; the migration under test would be a no-op")
	}

	apply("004_publish_ci_step.sql")

	var branch, ref string
	var completedAt int64
	if err := db.QueryRow(`SELECT branch, remote_ref, completed_at FROM publish_records
		WHERE attempt_id = 'attempt-1' AND step = 'proof'`).Scan(&branch, &ref, &completedAt); err != nil {
		t.Fatalf("the proof record did not survive the rebuild: %v", err)
	}
	if branch != "jig/job-1/1" || completedAt != 7 {
		t.Fatalf("proof record = %s %s %d after the rebuild, want it unchanged", branch, ref, completedAt)
	}
	if err := insertStep("ci"); err != nil {
		t.Fatalf("record ci after 004: %v", err)
	}
	if err := insertStep("deploy"); err == nil {
		t.Fatal("the rebuilt table accepted a step outside its vocabulary")
	}
	if err := insertStep("ci"); err == nil {
		t.Fatal("the rebuilt table lost its (attempt_id, step) primary key")
	}
}
