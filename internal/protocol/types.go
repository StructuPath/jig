// Package protocol holds the shared domain types, the limits table, and the
// definition schema. It is the one vocabulary both sides of the
// worker/control-plane boundary speak; it must never import either side.
package protocol

import (
	"encoding/json"
	"time"
)

// Phase kinds (KTD2). A phase is either an agent invocation or a code step;
// there is no third kind.
const (
	PhaseKindAgent = "agent"
	PhaseKindCode  = "code"
)

// Attempt states, exactly the lifecycle diagram. `queued`, `preparing`, and
// `running` are the active states covered by the one-active-attempt-per-job
// partial unique index; everything else is terminal for the attempt.
const (
	AttemptQueued    = "queued"
	AttemptPreparing = "preparing"
	AttemptRunning   = "running"
	// AttemptAccepted: all phases passed, the acceptance predicate held, and
	// publish completed with remote proof (R12).
	AttemptAccepted = "accepted"
	// AttemptAcceptedUnpublished: phases and predicate passed but publish
	// failed. Distinct state because it has a publish-only retry action (R14).
	AttemptAcceptedUnpublished = "accepted_unpublished"
	AttemptFailed              = "failed"
	AttemptCancelled           = "cancelled"
	// AttemptLost: swept after MissedHeartbeatsBeforeSweep consecutive missed
	// heartbeats measured after server uptime resumed (R5).
	AttemptLost = "lost"
)

// ActiveAttemptStates are the states the partial unique index treats as "one
// at a time per job". Order matters nowhere; membership matters everywhere.
var ActiveAttemptStates = []string{AttemptQueued, AttemptPreparing, AttemptRunning}

// Job states. A job is the per-target unit of failure, retry, and
// cancellation (R3); `active` means an attempt currently holds it.
const (
	JobQueued              = "queued"
	JobActive              = "active"
	JobAccepted            = "accepted"
	JobAcceptedUnpublished = "accepted_unpublished"
	JobFailed              = "failed"
	JobCancelled           = "cancelled"
)

// Run states. A run only aggregates: `accepted` iff all jobs accepted,
// `failed` iff all jobs failed, otherwise `mixed` once terminal (R12). A run
// is never replayed as a whole (R3).
const (
	RunActive    = "active"
	RunAccepted  = "accepted"
	RunFailed    = "failed"
	RunMixed     = "mixed"
	RunCancelled = "cancelled"
)

// Event types, the SSSF vocabulary plus one jig addition.
const (
	EventPhaseStart = "phase_start"
	EventAgentStart = "agent_start"
	EventToolCall   = "tool_call"
	EventHandoff    = "handoff"
	EventGatePass   = "gate_pass"
	EventGateFail   = "gate_fail"
	EventLog        = "log"
	EventAgentEnd   = "agent_end"
	EventPhaseEnd   = "phase_end"
	EventError      = "error"
	// EventPhaseDeath is the envelope-less terminal event for a phase whose
	// agent subprocess died without emitting anything parseable — kill,
	// crash, or watchdog fire. It exists so a dead phase is distinguishable
	// in the trace from a phase that failed with an envelope.
	EventPhaseDeath = "phase_death"
)

// Envelope statuses. Success must be earned: everything that constructs a
// phase or envelope result starts from fail.
const (
	EnvelopeSuccess = "success"
	EnvelopeFail    = "fail"
)

// Definition is the persisted definition record. Source is the authoritative
// YAML; edits bump Generation in place (no revision library). A Run freezes
// Source by value into its snapshot at admission (R2), so this record can
// change freely without touching any admitted run.
type Definition struct {
	ID         string    `json:"id"`
	Name       string    `json:"name"`
	Generation int       `json:"generation"`
	Source     string    `json:"source"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// RunTarget is one repository a run was admitted against, with the base
// commit pinned at admission (KTD9). Retries days later still run against
// BaseSHA; "re-admit at head" is the explicit escape hatch.
type RunTarget struct {
	Repository string `json:"repository"`
	BaseSHA    string `json:"base_sha"`
}

// Run is one invocation of a definition, frozen by value: Snapshot carries
// the whole definition YAML, Parameters the invocation inputs, Targets the
// pinned base SHAs (R2). Nothing in a run resolves lazily.
type Run struct {
	ID                   string            `json:"id"`
	DefinitionID         string            `json:"definition_id"`
	DefinitionGeneration int               `json:"definition_generation"`
	Snapshot             string            `json:"snapshot"`
	Parameters           map[string]string `json:"parameters,omitempty"`
	Targets              []RunTarget       `json:"targets"`
	State                string            `json:"state"`
	CreatedAt            time.Time         `json:"created_at"`
	UpdatedAt            time.Time         `json:"updated_at"`
}

// Job is the fan-out unit: one target repository of one run (R3). Failure,
// retry, and cancellation happen here, never on the run.
// CancellationRequested is the durable cancel flag for an active job: the
// operator sets it, the worker observes it on its next heartbeat, and only
// the worker performs the transition (R5).
type Job struct {
	ID                    string    `json:"id"`
	RunID                 string    `json:"run_id"`
	Repository            string    `json:"repository"`
	BaseSHA               string    `json:"base_sha"`
	State                 string    `json:"state"`
	CancellationRequested bool      `json:"cancellation_requested"`
	CreatedAt             time.Time `json:"created_at"`
	UpdatedAt             time.Time `json:"updated_at"`
}

// Attempt is one worker execution of a job. Retry always creates a new
// attempt — cold, from phase 1, in a fresh worktree with fresh sessions
// (KTD5). RuntimeName/RuntimeVersion record the probed agent CLI for this
// attempt (KTD4).
type Attempt struct {
	ID             string     `json:"id"`
	JobID          string     `json:"job_id"`
	WorkerID       string     `json:"worker_id,omitempty"`
	AttemptNumber  int        `json:"attempt_number"`
	State          string     `json:"state"`
	LeaseExpiresAt *time.Time `json:"lease_expires_at,omitempty"`
	RuntimeName    string     `json:"runtime_name,omitempty"`
	RuntimeVersion string     `json:"runtime_version,omitempty"`
	Result         string     `json:"result,omitempty"`
	Error          string     `json:"error,omitempty"`
	StartedAt      *time.Time `json:"started_at,omitempty"`
	CompletedAt    *time.Time `json:"completed_at,omitempty"`
	CreatedAt      time.Time  `json:"created_at"`
}

// PhaseResult is the outcome of one phase execution inside an attempt.
// Status defaults to fail everywhere it is constructed; success is earned by
// a parsed envelope that cleared every gate (R9). PhaseAttempt counts
// re-entries of the same phase (repair edges, crash restarts).
type PhaseResult struct {
	Phase        string          `json:"phase"`
	Kind         string          `json:"kind"`
	Status       string          `json:"status"`
	PhaseAttempt int             `json:"phase_attempt"`
	Envelope     json.RawMessage `json:"envelope,omitempty"`
	Gates        GateReport      `json:"gates"`
	Error        string          `json:"error,omitempty"`
	StartedAt    *time.Time      `json:"started_at,omitempty"`
	EndedAt      *time.Time      `json:"ended_at,omitempty"`
}

// Envelope is the base shape of every inter-phase contract (R8). Concrete
// envelope types extend it with extra fields that ride in the raw JSON; the
// base is what every phase can rely on being present.
type Envelope struct {
	Status            string   `json:"status"`
	Summary           string   `json:"summary,omitempty"`
	Artifacts         []string `json:"artifacts,omitempty"`
	NotesForNextAgent string   `json:"notes_for_next_agent,omitempty"`
}

// GateCheck is one thing a gate looked at and what it found. Note is the
// evidence — "exists, 2.1KB", "exit 0", "not in the diff" — and on a failed
// check it doubles as the correction the agent is told (R9).
type GateCheck struct {
	Item string `json:"item"`
	Ok   bool   `json:"ok"`
	Note string `json:"note,omitempty"`
}

// GateReport is what every gate returns: the checks it ran. Violations are
// derived, never stored separately — a green gate says what it checked (R9).
type GateReport struct {
	Checks []GateCheck `json:"checks"`
}

// Check appends one result and returns the report, so authoring a gate stays
// a loop and a return.
func (r *GateReport) Check(item string, ok bool, note string) *GateReport {
	r.Checks = append(r.Checks, GateCheck{Item: item, Ok: ok, Note: note})
	return r
}

// Violations lists every failed check as "item: note" correction lines.
func (r *GateReport) Violations() []string {
	var violations []string
	for _, check := range r.Checks {
		if check.Ok {
			continue
		}
		note := check.Note
		if note == "" {
			note = "failed"
		}
		violations = append(violations, check.Item+": "+note)
	}
	return violations
}

// Passed reports whether every check succeeded.
func (r *GateReport) Passed() bool {
	return len(r.Violations()) == 0
}

// RuntimeCapability describes one agent CLI a worker probed at start (KTD4):
// its version and the capability flags the engine adapts to. CanResume=false
// degrades corrections to transcript-digest replay in fresh sessions (R7).
type RuntimeCapability struct {
	Name        string `json:"name"`
	Version     string `json:"version"`
	CanResume   bool   `json:"can_resume"`
	ReportsCost bool   `json:"reports_cost"`
}

// RetainedWorktree is one worktree the worker kept because it could not
// prove the work published (R16). Retention is fail-closed: uncertainty
// retains, only remote-ref proof deletes.
type RetainedWorktree struct {
	AttemptID  string `json:"attempt_id"`
	Repository string `json:"repository"`
	Path       string `json:"path"`
	Reason     string `json:"reason"`
}

// WorkerRegistration is what the single implicit worker sends at start
// (KTD12). EnvNames advertises available env-var *names only* — never
// values — so claim eligibility can fail a job missing a required name
// before any phase runs (R17). Runtimes carries the probe results per
// installed agent CLI.
type WorkerRegistration struct {
	Name              string              `json:"name"`
	WorkerVersion     string              `json:"worker_version"`
	Capacity          int                 `json:"capacity"`
	ActiveCount       int                 `json:"active_count"`
	EnvNames          []string            `json:"env_names"`
	Runtimes          []RuntimeCapability `json:"runtimes"`
	RetainedWorktrees []RetainedWorktree  `json:"retained_worktrees"`
}

// Worker is the control plane's record of a registered worker. ActiveCount
// is computed (attempts currently leased by this worker), never stored.
type Worker struct {
	ID            string              `json:"id"`
	Name          string              `json:"name"`
	WorkerVersion string              `json:"worker_version"`
	Capacity      int                 `json:"capacity"`
	ActiveCount   int                 `json:"active_count"`
	EnvNames      []string            `json:"env_names"`
	Runtimes      []RuntimeCapability `json:"runtimes"`
	RegisteredAt  time.Time           `json:"registered_at"`
	LastHeartbeat time.Time           `json:"last_heartbeat"`
}

// ClaimRequest is one idempotent claim (R4). RequestID dedupes the request;
// LeaseToken is the fencing token, stored server-side only as its SHA-256
// digest. Replaying the same pair returns the identical answer; the same
// RequestID with a different token is a conflict.
type ClaimRequest struct {
	RequestID  string `json:"request_id"`
	LeaseToken string `json:"lease_token"`
}

// Claim is the answer to a successful claim: the leased attempt, its job,
// and the run's frozen snapshot and parameters the worker executes against
// (R2). An empty claim — nothing eligible — is the absence of a Claim, not a
// zero value.
type Claim struct {
	Attempt    Attempt           `json:"attempt"`
	Job        Job               `json:"job"`
	Snapshot   string            `json:"snapshot"`
	Parameters map[string]string `json:"parameters,omitempty"`
}

// StartAttemptRequest moves a claimed attempt preparing -> running, fenced by
// the lease token (R6). RuntimeName/RuntimeVersion record the probed agent
// CLI for the attempt's trace (KTD4).
type StartAttemptRequest struct {
	LeaseToken     string `json:"lease_token"`
	RuntimeName    string `json:"runtime_name,omitempty"`
	RuntimeVersion string `json:"runtime_version,omitempty"`
}

// HeartbeatRequest renews an attempt's lease (R5).
type HeartbeatRequest struct {
	LeaseToken string `json:"lease_token"`
}

// HeartbeatResponse is the renewal answer. Cancellation rides here: the
// server never dials a worker (R5).
type HeartbeatResponse struct {
	LeaseExpiresAt        time.Time `json:"lease_expires_at"`
	CancellationRequested bool      `json:"cancellation_requested"`
}

// CompleteAttemptRequest records an attempt's terminal outcome, fenced by the
// lease token (R6). State must be a terminal attempt state the worker may
// declare: accepted, accepted_unpublished, failed, or cancelled — never lost,
// which only sweep assigns. A replay with the original token returns the
// stored outcome unchanged.
type CompleteAttemptRequest struct {
	LeaseToken string `json:"lease_token"`
	State      string `json:"state"`
	Result     string `json:"result,omitempty"`
	Error      string `json:"error,omitempty"`
}

// Event is one trace event, dual-written to attempt-local JSONL and streamed
// to the control plane. Seq is the per-attempt monotonic sequence number that
// makes replay idempotent and UI cursors stable (KTD8); it is assigned by the
// worker, never by the database.
type Event struct {
	Seq       int64           `json:"seq"`
	Type      string          `json:"type"`
	Phase     string          `json:"phase,omitempty"`
	Name      string          `json:"name,omitempty"`
	Payload   json.RawMessage `json:"payload,omitempty"`
	StartedAt *time.Time      `json:"started_at,omitempty"`
	EndedAt   *time.Time      `json:"ended_at,omitempty"`
}
