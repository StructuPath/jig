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
	"strings"
	"time"

	"github.com/StructuPath/jig/internal/protocol"
)

// directWorkerID is the single implicit worker a direct run registers as.
// It is a stable id so repeated `jig run` invocations against one data dir
// upsert the same worker row instead of accumulating ghosts (KTD12).
const directWorkerID = "embedded-direct"

// maxDirectClaimAttempts bounds the claim loop that skips over stale queued
// jobs a crashed earlier direct run may have left behind.
const maxDirectClaimAttempts = 8

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
	for _, dir := range []string{config.DataDir, tracesDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return zero, fmt.Errorf("create data directory: %w", err)
		}
	}
	store, err := Open(ctx, filepath.Join(config.DataDir, "jig.db"))
	if err != nil {
		return zero, err
	}
	defer store.Close()

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

	if _, err := store.RegisterWorker(ctx, directWorkerID, protocol.WorkerRegistration{
		Name:          "embedded direct runner",
		WorkerVersion: "jig-run",
		Capacity:      1,
		EnvNames:      config.BaseEnvNames,
		Runtimes:      []protocol.RuntimeCapability{config.Capability},
	}); err != nil {
		return zero, err
	}

	definitionID, generation, err := store.upsertDefinition(ctx, spec.Name, string(config.Source))
	if err != nil {
		return zero, err
	}
	runID, err := store.insertFrozenRun(ctx, definitionID, generation, string(config.Source),
		config.Parameters, protocol.RunTarget{Repository: config.RepoPath, BaseSHA: config.HeadSHA})
	if err != nil {
		return zero, err
	}
	job, err := store.EnqueueJob(ctx, runID, config.RepoPath)
	if err != nil {
		return zero, err
	}

	claim, token, err := store.claimDirectJob(ctx, job.ID)
	if err != nil {
		return zero, err
	}
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
	// a background heartbeat renews it — the same rhythm the worker keeps.
	beatCtx, stopBeats := context.WithCancel(ctx)
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
			return store.AppendEvents(ctx, attemptID, []protocol.Event{event})
		},
		FreshenLease: func(ctx context.Context) error {
			_, err := store.Heartbeat(ctx, attemptID, protocol.HeartbeatRequest{LeaseToken: token})
			return err
		},
	})
	stopBeats()

	attempt, err := store.CompleteAttempt(ctx, attemptID, protocol.CompleteAttemptRequest{
		LeaseToken: token,
		State:      outcome.State,
		Result:     outcome.Result,
		Error:      outcome.Error,
	})
	if err != nil {
		return zero, fmt.Errorf("record outcome (engine said %s): %w", outcome.State, err)
	}
	if err := store.finishDirectRun(ctx, runID, attempt.State); err != nil {
		return zero, err
	}
	job, err = store.Job(ctx, job.ID)
	if err != nil {
		return zero, err
	}
	return DirectRunResult{RunID: runID, Job: job, Attempt: attempt, TracePath: tracePath}, nil
}

// claimDirectJob claims until it holds THE job this run enqueued. A crashed
// earlier direct run can leave an older queued job in the store; FIFO would
// hand that one over first, so any foreign claim is completed as failed —
// explicitly, with its cause — and the loop claims again.
func (s *Store) claimDirectJob(ctx context.Context, jobID string) (*protocol.Claim, string, error) {
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
		if _, err := s.CompleteAttempt(ctx, claim.Attempt.ID, protocol.CompleteAttemptRequest{
			LeaseToken: token,
			State:      protocol.AttemptFailed,
			Error:      "orphaned queued job claimed and failed by a later direct run",
		}); err != nil {
			return nil, "", err
		}
	}
	return nil, "", fmt.Errorf("could not claim job %s within %d claims", jobID, maxDirectClaimAttempts)
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

// upsertDefinition saves the definition record behind a direct run:
// insert on first sight, generation bump in place after (R1 — no revision
// library; the run snapshot preserves what actually executed).
func (s *Store) upsertDefinition(ctx context.Context, name, source string) (string, int, error) {
	now := s.now().UnixMilli()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", 0, unavailable(err)
	}
	defer tx.Rollback()
	var id string
	var generation int
	err = tx.QueryRowContext(ctx,
		`SELECT id, generation FROM definitions WHERE name = ?`, name).Scan(&id, &generation)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		id, err = newID()
		if err != nil {
			return "", 0, unavailable(err)
		}
		generation = 1
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO definitions(id, name, generation, source, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?)
		`, id, name, generation, source, now, now); err != nil {
			return "", 0, unavailable(err)
		}
	case err != nil:
		return "", 0, unavailable(err)
	default:
		generation++
		if _, err := tx.ExecContext(ctx, `
			UPDATE definitions SET generation = ?, source = ?, updated_at = ? WHERE id = ?
		`, generation, source, now, id); err != nil {
			return "", 0, unavailable(err)
		}
	}
	if err := tx.Commit(); err != nil {
		return "", 0, unavailable(err)
	}
	return id, generation, nil
}

// insertFrozenRun freezes one run by value (R2, KTD9): the snapshot, the
// parameters, and the single pinned target. Nothing in it resolves lazily.
func (s *Store) insertFrozenRun(
	ctx context.Context, definitionID string, generation int, snapshot string,
	parameters map[string]string, target protocol.RunTarget,
) (string, error) {
	if parameters == nil {
		parameters = map[string]string{}
	}
	parametersJSON, err := json.Marshal(parameters)
	if err != nil {
		return "", unavailable(err)
	}
	targetsJSON, err := json.Marshal([]protocol.RunTarget{target})
	if err != nil {
		return "", unavailable(err)
	}
	runID, err := newID()
	if err != nil {
		return "", unavailable(err)
	}
	now := s.now().UnixMilli()
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO runs(id, definition_id, definition_generation, snapshot, parameters, targets, state, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, 'active', ?, ?)
	`, runID, definitionID, generation, snapshot, string(parametersJSON), string(targetsJSON),
		now, now); err != nil {
		return "", unavailable(err)
	}
	return runID, nil
}

// finishDirectRun records the single-job run's terminal state. Direct runs
// end at predicate evaluation with publish out of scope by design, so an
// accepted_unpublished attempt IS the direct-run notion of accepted — the
// no-publish marker in the attempt result keeps the record honest (U11;
// full multi-job aggregation is U5's).
func (s *Store) finishDirectRun(ctx context.Context, runID, attemptState string) error {
	var state string
	switch attemptState {
	case protocol.AttemptAcceptedUnpublished, protocol.AttemptAccepted:
		state = protocol.RunAccepted
	case protocol.AttemptCancelled:
		state = protocol.RunCancelled
	default:
		state = protocol.RunFailed
	}
	if _, err := s.db.ExecContext(ctx, `
		UPDATE runs SET state = ?, updated_at = ? WHERE id = ? AND state = 'active'
	`, state, s.now().UnixMilli(), runID); err != nil {
		return unavailable(err)
	}
	return nil
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
		if _, err := tx.ExecContext(ctx, `
			INSERT OR IGNORE INTO events(attempt_id, seq, type, phase, payload, payload_bytes, server_time)
			VALUES (?, ?, ?, ?, ?, ?, ?)
		`, attemptID, event.Seq, event.Type, event.Phase, body, len(body), now); err != nil {
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
