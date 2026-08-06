// runs.go — invocation, freezing, fan-out, and run aggregation (U5; R2, R3,
// R12, KTD9).
//
// Invoking a definition freezes it BY VALUE. The run row carries the whole
// definition source, the parameters, and one pinned base commit per target;
// the worker executes the snapshot and never reads the definitions table.
// That is what makes the snapshot-isolation contract true rather than
// intended: an edit to a definition cannot reach an admitted run because
// nothing in the run path ever looks at the definition again.
//
// Base SHAs are resolved HERE, at admission (KTD9) — a snapshot that
// resolves refs lazily is not frozen: a retry days later would silently run
// against a different tree than its siblings did. "Re-admit at head" is the
// explicit, separate escape hatch (ReadmitRunAtHead), never an implicit
// re-resolution.
//
// Fan-out creates N independent jobs in the admitting transaction: either
// every target got a job or none did. After admission the run is only an
// aggregate — failure, retry, and cancellation live on jobs (R3), and the
// run's state is a pure function of its jobs' states (R12).
package controlplane

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"strings"

	"github.com/StructuPath/jig/internal/protocol"
)

// InvocationTarget is one repository a run is invoked against. Repository is
// normalized at admission (U3's identity note). Ref names what to pin —
// defaulting to the repository's HEAD — and BaseSHA pins explicitly, for
// callers that already resolved it (or that admit a repository this machine
// cannot reach).
type InvocationTarget struct {
	Repository string `json:"repository"`
	Ref        string `json:"ref,omitempty"`
	BaseSHA    string `json:"base_sha,omitempty"`
}

// RunInvocation is one manual invocation of a definition (U6's triggers
// build the same value; see the unit notes). Parameters are frozen verbatim.
// Instructions and Context are the two halves of the prompt: Instructions is
// operator-authored and trusted, Context is repository/issue text and is
// not — they are joined only by protocol.ComposeInvocationPrompt, whose
// result is frozen as the {{prompt}} parameter.
type RunInvocation struct {
	DefinitionID string                      `json:"definition_id"`
	Parameters   map[string]string           `json:"parameters,omitempty"`
	Instructions string                      `json:"instructions,omitempty"`
	Context      []protocol.UntrustedSection `json:"context,omitempty"`
	Targets      []InvocationTarget          `json:"targets"`
}

// RunView is a run with the jobs it fanned out into, in target order — the
// shape every run route answers with.
type RunView struct {
	Run  protocol.Run   `json:"run"`
	Jobs []protocol.Job `json:"jobs"`
}

// InvokeDefinition admits one run: it freezes the definition's current
// source, composes and freezes the prompt, pins a base SHA per target, and
// fans out one job per target (R2, R3, KTD9).
func (s *Store) InvokeDefinition(ctx context.Context, input RunInvocation) (RunView, error) {
	var zero RunView
	definition, err := s.Definition(ctx, strings.TrimSpace(input.DefinitionID))
	if err != nil {
		return zero, err
	}
	parameters, err := freezeParameters(input)
	if err != nil {
		return zero, err
	}
	targets, err := s.resolveTargets(ctx, input.Targets)
	if err != nil {
		return zero, err
	}
	return s.admitRun(ctx, definition.ID, definition.Generation, definition.Source, parameters, targets)
}

// ReadmitRunAtHead is the KTD9 escape hatch: a new run carrying the ORIGINAL
// run's frozen snapshot and parameters — so a definition edited in the
// meantime cannot leak in — against base SHAs resolved fresh at each
// target's head. It is the answer to "the pinned commit is no longer
// fetchable" and to "rerun this against today's tree", and it is deliberately
// a separate action: no retry, and no invocation, ever re-resolves a pin.
func (s *Store) ReadmitRunAtHead(ctx context.Context, runID string) (RunView, error) {
	var zero RunView
	previous, err := s.Run(ctx, runID)
	if err != nil {
		return zero, err
	}
	if len(previous.Targets) == 0 {
		return zero, invalid("no_targets", "run "+quote(runID)+" has no targets to re-admit")
	}
	targets := make([]InvocationTarget, 0, len(previous.Targets))
	for _, target := range previous.Targets {
		targets = append(targets, InvocationTarget{Repository: target.Repository})
	}
	resolved, err := s.resolveTargets(ctx, targets)
	if err != nil {
		return zero, err
	}
	return s.admitRun(ctx, previous.DefinitionID, previous.DefinitionGeneration,
		previous.Snapshot, previous.Parameters, resolved)
}

// freezeParameters builds the run's frozen parameter map. A caller may pass
// {{prompt}} directly OR pass instructions and untrusted context to be
// composed — never both, because then two different strings would claim to
// be the prompt and only one would execute.
func freezeParameters(input RunInvocation) (map[string]string, error) {
	parameters := make(map[string]string, len(input.Parameters)+1)
	for name, value := range input.Parameters {
		parameters[name] = value
	}
	if strings.TrimSpace(input.Instructions) == "" && len(input.Context) == 0 {
		return parameters, nil
	}
	if _, exists := parameters[protocol.PromptParameter]; exists {
		return nil, invalid("ambiguous_prompt",
			"pass either a "+quote(protocol.PromptParameter)+
				" parameter or instructions/context to compose, not both")
	}
	composed, err := protocol.ComposeInvocationPrompt(input.Instructions, input.Context)
	if err != nil {
		return nil, invalid("invalid_prompt", err.Error())
	}
	parameters[protocol.PromptParameter] = composed
	return parameters, nil
}

// resolveTargets normalizes every target's repository identity and pins its
// base commit (U3's identity note, KTD9). Normalization happens before the
// duplicate check, so the same repository named two different ways is
// rejected here rather than surfacing later as a UNIQUE violation.
func (s *Store) resolveTargets(ctx context.Context, targets []InvocationTarget) ([]protocol.RunTarget, error) {
	if len(targets) == 0 {
		return nil, invalid("no_targets", "a run requires at least one target repository")
	}
	resolved := make([]protocol.RunTarget, 0, len(targets))
	seen := make(map[string]bool, len(targets))
	for _, target := range targets {
		identity, err := protocol.NormalizeRepositoryIdentity(target.Repository)
		if err != nil {
			return nil, invalid("invalid_target",
				"target repository "+quote(target.Repository)+": "+err.Error())
		}
		if seen[identity] {
			return nil, invalid("duplicate_target",
				"repository "+quote(identity)+" is listed twice; one run has one job per repository")
		}
		seen[identity] = true
		baseSHA := strings.TrimSpace(target.BaseSHA)
		if baseSHA == "" {
			baseSHA, err = s.resolveBaseSHA(ctx, identity, strings.TrimSpace(target.Ref))
			if err != nil {
				return nil, err
			}
		} else if !isCommitSHA(baseSHA) {
			return nil, invalid("invalid_base_sha",
				"base_sha "+quote(baseSHA)+" for "+quote(identity)+" is not a full 40-character commit SHA")
		}
		resolved = append(resolved, protocol.RunTarget{Repository: identity, BaseSHA: baseSHA})
	}
	return resolved, nil
}

// resolveBaseSHA asks the repository itself what the ref points at right
// now, through the only source derivable from the identity (R17: never a URL
// from a ticket). This is the single moment a run touches a live ref.
func (s *Store) resolveBaseSHA(ctx context.Context, identity, ref string) (string, error) {
	source, err := protocol.CloneSourceForIdentity(identity)
	if err != nil {
		return "", invalid("invalid_target", err.Error())
	}
	if ref == "" {
		ref = "HEAD"
	}
	ctx, cancel := context.WithTimeout(ctx, protocol.GitCommandTimeout)
	defer cancel()
	command := exec.CommandContext(ctx, "git", "ls-remote", "--exit-code", source, ref)
	// The control plane's own environment, minus any chance of git stopping
	// to ask a human for credentials: an admission must fail loudly, never
	// block on a prompt nobody is watching.
	command.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	output, err := command.Output()
	if err != nil {
		detail := ""
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			detail = ": " + strings.TrimSpace(tail(string(exit.Stderr), 400))
		}
		return "", invalid("unresolvable_ref",
			"cannot resolve "+quote(ref)+" in "+quote(identity)+detail)
	}
	fields := strings.Fields(firstLine(string(output)))
	if len(fields) == 0 || !isCommitSHA(fields[0]) {
		return "", invalid("unresolvable_ref",
			"resolving "+quote(ref)+" in "+quote(identity)+" produced no commit")
	}
	return fields[0], nil
}

// admitRun writes the frozen run and its fan-out in ONE transaction: a crash
// mid-admission can never leave a run whose target set and job set disagree
// (R3). Each job is created with its first attempt in `queued`, exactly as
// EnqueueJob does — claim fills the worker and lease.
func (s *Store) admitRun(
	ctx context.Context, definitionID string, generation int, snapshot string,
	parameters map[string]string, targets []protocol.RunTarget,
) (RunView, error) {
	var zero RunView
	if parameters == nil {
		parameters = map[string]string{}
	}
	parametersJSON, err := json.Marshal(parameters)
	if err != nil {
		return zero, unavailable(err)
	}
	targetsJSON, err := json.Marshal(targets)
	if err != nil {
		return zero, unavailable(err)
	}
	runID, err := newID()
	if err != nil {
		return zero, unavailable(err)
	}
	now := s.now().UnixMilli()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return zero, unavailable(err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO runs(id, definition_id, definition_generation, snapshot, parameters, targets,
			state, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, 'active', ?, ?)
	`, runID, definitionID, generation, snapshot, string(parametersJSON), string(targetsJSON),
		now, now); err != nil {
		return zero, unavailable(err)
	}
	jobIDs := make([]string, 0, len(targets))
	for _, target := range targets {
		jobID, err := insertJob(ctx, tx, runID, target, now)
		if err != nil {
			return zero, err
		}
		jobIDs = append(jobIDs, jobID)
	}
	if err := tx.Commit(); err != nil {
		return zero, unavailable(err)
	}
	view := RunView{Jobs: make([]protocol.Job, 0, len(jobIDs))}
	if view.Run, err = s.Run(ctx, runID); err != nil {
		return zero, err
	}
	for _, jobID := range jobIDs {
		job, err := s.Job(ctx, jobID)
		if err != nil {
			return zero, err
		}
		view.Jobs = append(view.Jobs, job)
	}
	return view, nil
}

// insertJob creates one job and its first attempt inside the admitting
// transaction.
func insertJob(ctx context.Context, tx *sql.Tx, runID string, target protocol.RunTarget, now int64) (string, error) {
	jobID, err := newID()
	if err != nil {
		return "", unavailable(err)
	}
	attemptID, err := newID()
	if err != nil {
		return "", unavailable(err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO jobs(id, run_id, repository, base_sha, state, created_at, updated_at)
		VALUES (?, ?, ?, ?, 'queued', ?, ?)
	`, jobID, runID, target.Repository, target.BaseSHA, now, now); err != nil {
		if isConstraintViolation(err) {
			return "", conflict("job_exists", "the run already has a job for this repository")
		}
		return "", unavailable(err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO attempts(id, job_id, attempt_number, state, created_at)
		VALUES (?, ?, 1, 'queued', ?)
	`, attemptID, jobID, now); err != nil {
		if isConstraintViolation(err) {
			return "", conflict("attempt_conflict", "the job already has a live attempt")
		}
		return "", unavailable(err)
	}
	return jobID, nil
}

// ---- run aggregation (R12) -------------------------------------------------

// applyRunAggregationForJob recomputes the state of the run owning jobID. It
// is called from inside every transaction that moves a job's state — the
// aggregate is never allowed to drift from the jobs it summarizes, so it is
// recomputed rather than incremented.
func applyRunAggregationForJob(ctx context.Context, tx *sql.Tx, jobID string, nowMillis int64) error {
	var runID string
	err := tx.QueryRowContext(ctx, `SELECT run_id FROM jobs WHERE id = ?`, jobID).Scan(&runID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return unavailable(err)
	}
	return applyRunAggregation(ctx, tx, runID, nowMillis)
}

// applyRunAggregation writes the run state implied by its jobs (R12):
// `accepted` iff every job is accepted, `failed` iff every job failed,
// `cancelled` iff every job was cancelled, `active` while any job can still
// move, and `mixed` for every other terminal combination.
//
// One rule, no exceptions — including the single-job direct run (U11), whose
// `accepted_unpublished` job now aggregates to `mixed` rather than to a
// second, kinder definition of accepted. A job that reached acceptance but
// could not publish is exactly R12's "neither accepted nor failed"; the
// no-publish marker in the attempt result is what says why, and `jig run`
// reports the ATTEMPT state, which is unaffected.
func applyRunAggregation(ctx context.Context, tx *sql.Tx, runID string, nowMillis int64) error {
	rows, err := tx.QueryContext(ctx, `
		SELECT state, COUNT(*) FROM jobs WHERE run_id = ? GROUP BY state
	`, runID)
	if err != nil {
		return unavailable(err)
	}
	counts := map[string]int{}
	total := 0
	for rows.Next() {
		var state string
		var count int
		if err := rows.Scan(&state, &count); err != nil {
			rows.Close()
			return unavailable(err)
		}
		counts[state] = count
		total += count
	}
	if err := rows.Close(); err != nil {
		return unavailable(err)
	}
	if err := rows.Err(); err != nil {
		return unavailable(err)
	}
	if total == 0 {
		return nil
	}
	state := aggregateRunState(counts, total)
	if _, err := tx.ExecContext(ctx, `
		UPDATE runs SET state = ?, updated_at = ? WHERE id = ? AND state != ?
	`, state, nowMillis, runID, state); err != nil {
		return unavailable(err)
	}
	return nil
}

func aggregateRunState(counts map[string]int, total int) string {
	switch {
	case counts[protocol.JobQueued]+counts[protocol.JobActive] > 0:
		// A run whose jobs can still move is not an outcome yet. Retrying a
		// failed job therefore returns its run to `active`, which is why this
		// function is called on retry as well as on completion.
		return protocol.RunActive
	case counts[protocol.JobAccepted] == total:
		return protocol.RunAccepted
	case counts[protocol.JobFailed] == total:
		return protocol.RunFailed
	case counts[protocol.JobCancelled] == total:
		return protocol.RunCancelled
	default:
		return protocol.RunMixed
	}
}

// ---- reads -----------------------------------------------------------------

// Run reads one frozen run, snapshot and pins included.
func (s *Store) Run(ctx context.Context, runID string) (protocol.Run, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, definition_id, definition_generation, snapshot, parameters, targets,
		       state, created_at, updated_at
		FROM runs WHERE id = ?
	`, runID)
	value, err := scanRun(row)
	if errors.Is(err, sql.ErrNoRows) {
		return value, ErrNotFound
	}
	if err != nil {
		return value, unavailable(err)
	}
	return value, nil
}

// Runs lists every run, newest first.
func (s *Store) Runs(ctx context.Context) ([]protocol.Run, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, definition_id, definition_generation, snapshot, parameters, targets,
		       state, created_at, updated_at
		FROM runs ORDER BY created_at DESC, id
	`)
	if err != nil {
		return nil, unavailable(err)
	}
	defer rows.Close()
	values := []protocol.Run{}
	for rows.Next() {
		value, err := scanRun(rows)
		if err != nil {
			return nil, unavailable(err)
		}
		values = append(values, value)
	}
	if err := rows.Err(); err != nil {
		return nil, unavailable(err)
	}
	return values, nil
}

// RunJobs lists one run's jobs in creation order.
func (s *Store) RunJobs(ctx context.Context, runID string) ([]protocol.Job, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, run_id, repository, base_sha, state, cancellation_requested, created_at, updated_at
		FROM jobs WHERE run_id = ? ORDER BY created_at, id
	`, runID)
	if err != nil {
		return nil, unavailable(err)
	}
	defer rows.Close()
	values := []protocol.Job{}
	for rows.Next() {
		var value protocol.Job
		var cancel int
		var createdAt, updatedAt int64
		if err := rows.Scan(&value.ID, &value.RunID, &value.Repository, &value.BaseSHA,
			&value.State, &cancel, &createdAt, &updatedAt); err != nil {
			return nil, unavailable(err)
		}
		value.CancellationRequested = cancel != 0
		value.CreatedAt = fromMillis(createdAt)
		value.UpdatedAt = fromMillis(updatedAt)
		values = append(values, value)
	}
	if err := rows.Err(); err != nil {
		return nil, unavailable(err)
	}
	return values, nil
}

// RunDetail reads a run with its jobs.
func (s *Store) RunDetail(ctx context.Context, runID string) (RunView, error) {
	var view RunView
	run, err := s.Run(ctx, runID)
	if err != nil {
		return view, err
	}
	jobs, err := s.RunJobs(ctx, runID)
	if err != nil {
		return view, err
	}
	return RunView{Run: run, Jobs: jobs}, nil
}

func scanRun(row rowScanner) (protocol.Run, error) {
	var value protocol.Run
	var parametersJSON, targetsJSON string
	var createdAt, updatedAt int64
	if err := row.Scan(&value.ID, &value.DefinitionID, &value.DefinitionGeneration, &value.Snapshot,
		&parametersJSON, &targetsJSON, &value.State, &createdAt, &updatedAt); err != nil {
		return value, err
	}
	if err := json.Unmarshal([]byte(parametersJSON), &value.Parameters); err != nil {
		return value, err
	}
	if err := json.Unmarshal([]byte(targetsJSON), &value.Targets); err != nil {
		return value, err
	}
	value.CreatedAt = fromMillis(createdAt)
	value.UpdatedAt = fromMillis(updatedAt)
	return value, nil
}

// ---- small helpers ---------------------------------------------------------

func isCommitSHA(value string) bool {
	if len(value) != 40 {
		return false
	}
	for _, r := range value {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

func firstLine(value string) string {
	if index := strings.IndexByte(value, '\n'); index >= 0 {
		return value[:index]
	}
	return value
}

func tail(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	return value[len(value)-limit:]
}
