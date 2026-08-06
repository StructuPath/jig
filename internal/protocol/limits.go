package protocol

import "time"

// Every operational limit in jig is a named constant in this table. A limit
// that matters enough to enforce matters enough to name, document, and test
// against — magic numbers inside transaction or loop code are rejected in
// review.
const (
	// LeaseDuration is how long a claim's lease is valid before a heartbeat
	// must renew it. An attempt whose lease expires is not immediately lost:
	// sweep requires MissedHeartbeatsBeforeSweep consecutive misses measured
	// after server uptime resumes (R5), so whole-machine sleep is benign.
	LeaseDuration = 30 * time.Second

	// HeartbeatInterval is how often the worker renews its lease. It must be
	// comfortably shorter than LeaseDuration so one delayed heartbeat never
	// expires a healthy attempt.
	HeartbeatInterval = 10 * time.Second

	// MissedHeartbeatsBeforeSweep is the number of consecutive missed
	// heartbeats — counted only while the server is up — before sweep marks a
	// leased attempt `lost`. One missed beat is jitter; N missed beats after
	// uptime resumed is a dead worker (R5).
	MissedHeartbeatsBeforeSweep = 3

	// SweeperUptimeGap is the tick-to-tick gap beyond which the sweeper
	// concludes the server itself was down (sleep, suspend) and resets every
	// missed-heartbeat count. Misses are therefore only ever counted against
	// server uptime, so whole-machine sleep never sweeps intact work (R5).
	SweeperUptimeGap = 2 * HeartbeatInterval

	// WorkerLivenessWindow is how stale a worker's last observed heartbeat may
	// be while the worker still counts as live for claim eligibility (R4).
	// Registration and attempt heartbeats both refresh it (KTD12).
	WorkerLivenessWindow = 3 * HeartbeatInterval

	// RegistrationInterval is how often the worker re-registers. Registration
	// is also the idle liveness heartbeat (KTD12): claims never refresh
	// liveness, so this must stay comfortably inside WorkerLivenessWindow.
	RegistrationInterval = HeartbeatInterval

	// ClaimPollInterval is how often an idle worker with free capacity polls
	// for work after an empty claim answer.
	ClaimPollInterval = 2 * time.Second

	// WorkerRequestTimeout bounds every worker→server HTTP request.
	WorkerRequestTimeout = 15 * time.Second

	// GitCommandTimeout bounds every git invocation the worker runs. A git
	// command that cannot finish inside it is treated as failed, never waited
	// on indefinitely.
	GitCommandTimeout = 60 * time.Second

	// MaxCachedRepositories caps the worker's on-demand managed repository
	// cache (U3). At the cap, a claim for an uncached repository fails its
	// preparation rather than growing disk without bound.
	MaxCachedRepositories = 32

	// MaxRetentionReasonBytes caps a retained-worktree reason everywhere it
	// is persisted — manifest, registration payload, ledger row.
	MaxRetentionReasonBytes = 1000

	// EmptyClaimTTL is how long an empty claim answer stays replayable under
	// its request id. Past it the row is deleted (by sweep, or lazily by a
	// replay) and the same request id may claim afresh (R4).
	EmptyClaimTTL = time.Minute

	// ParseBudgetPerEmission is how many times a single envelope emission may
	// be re-prompted for a parse failure before the phase fails. Every
	// gate-corrected emission re-enters parsing with a fresh budget of this
	// size (R7).
	ParseBudgetPerEmission = 3

	// DefaultPhaseRetryBudget is the default gate-correction budget for a
	// phase whose definition does not declare one: how many corrected
	// emissions a gate violation may request before the phase fails (R7, R9).
	DefaultPhaseRetryBudget = 2

	// WorstCaseSendCount is the maximum number of prompt sends one phase can
	// issue at the default budgets: gate_budget × parse_budget, because every
	// gate correction opens a fresh per-emission parse budget. A definition
	// that raises either budget raises this product; the per-attempt
	// wall-clock ceiling bounds total cost regardless.
	WorstCaseSendCount = DefaultPhaseRetryBudget * ParseBudgetPerEmission

	// DefaultPhaseTimeout is the wall-clock ceiling for a single phase. A
	// phase that exceeds it is killed by process group, consumes one phase
	// retry, and restarts from the pre-phase snapshot with a fresh session
	// (R11).
	DefaultPhaseTimeout = 30 * time.Minute

	// NoOutputWatchdog is how long an agent subprocess may emit nothing
	// before it is presumed hung and killed. Distinct from DefaultPhaseTimeout:
	// a phase can be busy for a long time, but never silent this long (R11).
	NoOutputWatchdog = 5 * time.Minute

	// MaxAttemptDuration is the wall-clock ceiling for one whole attempt
	// across all phases, retries, and corrections. It is the outermost cost
	// bound: no combination of budgets can keep an attempt alive past it
	// (R11).
	MaxAttemptDuration = 4 * time.Hour

	// MaxRetainedWorktreesPerRepo caps how many unpublished worktrees the
	// ledger may retain per repository. Claiming skips over jobs whose
	// repository is at this cap rather than growing disk without bound (R4,
	// R16).
	MaxRetainedWorktreesPerRepo = 10

	// MaxEventsPerBatch caps how many trace events one worker→server
	// ingestion request may carry. Batches are bounded so an event storm
	// backpressures into the worker's local buffer instead of one giant
	// request (KTD8).
	MaxEventsPerBatch = 100

	// MaxInvalidEnvelopeBytes caps the size of an invalid-envelope row
	// persisted server-side. The full invalid emission always survives in
	// attempt-local JSONL; the SQLite copy is capped so a runaway emission
	// cannot bloat the control-plane database (R7, R15).
	MaxInvalidEnvelopeBytes = 64 << 10

	// MaxRequestBodyBytes bounds every HTTP request body the control plane
	// will read. Larger bodies fail with 413 before any handler logic (R20).
	MaxRequestBodyBytes = 1 << 20

	// MaxResultBytes caps an attempt completion's result payload.
	MaxResultBytes = 64 << 10

	// MaxErrorBytes caps an attempt completion's error text.
	MaxErrorBytes = 16 << 10
)
