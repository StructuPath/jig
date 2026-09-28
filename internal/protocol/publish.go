// publish.go — the publish vocabulary both sides of the worker/control-plane
// boundary speak (U7, R14). Publish is a three-step pipeline whose every step
// is authorized and recorded through the control plane under the attempt's
// lease token (R6): push the attempt-scoped branch, find-or-create the pull
// request by head ref, then prove the remote ref. The records in
// publish_records ARE the proof, and they are what a publish-only retry reads
// to re-enter at the failed step without re-running a single phase.
package protocol

import (
	"fmt"
	"time"
)

// Publish steps, exactly the vocabulary of the publish_records CHECK
// constraint. Order is meaningful: a pull request cannot precede its branch,
// and proof cannot precede either.
const (
	PublishStepPush        = "push"
	PublishStepPullRequest = "pull_request"
	PublishStepProof       = "proof"
	// PublishStepCI records a green CI run on the pull request's head. It
	// runs only for definitions whose publish.ci waits, and its remote_ref is
	// the head CI passed on — which may be a later commit than proof's when a
	// person pushed a fix before the publish retry.
	PublishStepCI = "ci"
)

// PublishSteps is the pipeline in execution order.
var PublishSteps = []string{PublishStepPush, PublishStepPullRequest, PublishStepProof, PublishStepCI}

// PublishStepPrerequisites names the steps that must already be recorded
// before a step may be authorized or recorded. It is enforced
// control-plane-side, so a worker cannot record proof for a branch it never
// pushed.
var PublishStepPrerequisites = map[string][]string{
	PublishStepPush:        nil,
	PublishStepPullRequest: {PublishStepPush},
	PublishStepProof:       {PublishStepPush, PublishStepPullRequest},
	PublishStepCI:          {PublishStepPush, PublishStepPullRequest, PublishStepProof},
}

// ValidPublishStep reports whether value names a publish step.
func ValidPublishStep(value string) bool {
	_, exists := PublishStepPrerequisites[value]
	return exists
}

// PublishBranch is the attempt-scoped branch name (R14) — the same name U3's
// worktree is created on. It is computed control-plane-side as well as
// worker-side so the naming is an enforced invariant rather than a worker
// convention: an attempt may only ever publish its own branch.
func PublishBranch(jobID string, attemptNumber int) string {
	return fmt.Sprintf("jig/%s/%d", jobID, attemptNumber)
}

// Publish limits. Every one of them bounds either a stored field or an
// external command, in the style of the limits table.
const (
	// MaxPublishBranchBytes caps a branch name everywhere it is stored or
	// passed to git.
	MaxPublishBranchBytes = 300

	// MaxPublishRefBytes caps a recorded remote ref. A ref is a commit SHA,
	// so this is generous by design and strict in shape (hex only).
	MaxPublishRefBytes = 64

	// MaxPublishURLBytes caps a recorded pull-request URL.
	MaxPublishURLBytes = 500

	// PublishCommandTimeout bounds every `gh` invocation the publish pipeline
	// runs. Matches factory's automation posture: a command that cannot finish
	// inside it is failed with a diagnostic, never waited on.
	PublishCommandTimeout = 60 * time.Second

	// MaxPublishStdoutBytes and MaxPublishStderrBytes bound what one `gh`
	// invocation may return. Over-limit output is a distinct diagnostic, not a
	// truncated parse.
	MaxPublishStdoutBytes = 4 << 20
	MaxPublishStderrBytes = 64 << 10

	// CIPollInterval is how often the ci step re-reads check state while it
	// waits. GitHub's own UI refreshes on a similar cadence.
	CIPollInterval = 30 * time.Second

	// CIRegistrationGrace is how long a head with no checks at all counts as
	// "CI has not registered yet" before it counts as "this repository runs
	// no CI". Workflows usually attach within seconds of a push.
	CIRegistrationGrace = 2 * time.Minute

	// MaxCITransientFailures is how many consecutive failed check reads the
	// ci step tolerates before it gives up with a diagnostic.
	MaxCITransientFailures = 3

	// MaxPublishDiagnosticBytes caps the diagnostic text carried out of a
	// failed publish step into the attempt result.
	MaxPublishDiagnosticBytes = 4 << 10

	// MaxPublishPathsPerBatch caps how many pathspecs one `git add` carries.
	// A large changeset is staged in several bounded invocations rather than
	// one argv the OS may refuse.
	MaxPublishPathsPerBatch = 200

	// MaxPublishPaths caps how many changed paths one publish will stage. A
	// changeset larger than this is refused with a diagnostic rather than
	// committed blind.
	MaxPublishPaths = 5000

	// MaxPublishBodyPaths caps how many changed paths the pull-request body
	// lists before it summarizes the remainder.
	MaxPublishBodyPaths = 50

	// MaxStrayBranchesPerRepository caps how many stray attempt-scoped
	// branches one reconciliation reports per repository. The count is always
	// reported even when the list is capped.
	MaxStrayBranchesPerRepository = 200
)

// PublishRecord is one recorded, fenced publish step — the control plane's
// proof that this attempt performed it (R14). AttemptNumber and JobID are
// joined from the attempt so a reader can tell which branch the record is
// about without a second query.
type PublishRecord struct {
	AttemptID      string    `json:"attempt_id"`
	JobID          string    `json:"job_id"`
	AttemptNumber  int       `json:"attempt_number"`
	Step           string    `json:"step"`
	Branch         string    `json:"branch"`
	RemoteRef      string    `json:"remote_ref,omitempty"`
	PullRequestURL string    `json:"pr_url,omitempty"`
	CompletedAt    time.Time `json:"completed_at"`
}

// PublishAuthorizationRequest asks the control plane whether this lease may
// perform one publish step right now. It is the fence that runs BEFORE the
// irreversible side effect: a zombie attempt is rejected here rather than
// after it has already pushed (KTD6).
type PublishAuthorizationRequest struct {
	LeaseToken string `json:"lease_token"`
	Step       string `json:"step"`
	// Branch is the branch the worker intends to write. It must equal the
	// attempt-scoped name the control plane computes; anything else is
	// refused.
	Branch string `json:"branch"`
}

// PublishAuthorization is a granted step authorization. Completed is non-nil
// when the step is already recorded — the worker must then skip the side
// effect entirely, which is what makes re-entry idempotent.
type PublishAuthorization struct {
	AttemptID     string         `json:"attempt_id"`
	JobID         string         `json:"job_id"`
	AttemptNumber int            `json:"attempt_number"`
	Repository    string         `json:"repository"`
	BaseSHA       string         `json:"base_sha"`
	Step          string         `json:"step"`
	Branch        string         `json:"branch"`
	Completed     *PublishRecord `json:"completed,omitempty"`
}

// PublishStepRequest records one completed publish step, fenced by the lease
// token (R6). Recording is idempotent: replaying identical values returns the
// stored record; replaying different values for a recorded step is a
// conflict, because two different answers for one step is evidence, not a
// retry.
type PublishStepRequest struct {
	LeaseToken     string `json:"lease_token"`
	Step           string `json:"step"`
	Branch         string `json:"branch"`
	RemoteRef      string `json:"remote_ref,omitempty"`
	PullRequestURL string `json:"pr_url,omitempty"`
}

// PublishRetryRequest is the publish-only retry action (R14). It re-leases
// the job's accepted_unpublished attempt to the worker that owns it, so
// publish re-enters at the failed step against the SAME attempt-scoped
// branch. No phase is re-run, and no new attempt is created — a new attempt
// would mean a new branch, which is exactly the duplicate this action exists
// to avoid.
type PublishRetryRequest struct {
	WorkerID   string `json:"worker_id"`
	LeaseToken string `json:"lease_token"`
}

// PublishRetry is the granted retry: the re-leased attempt, its job, and the
// steps already proven, so the worker knows where to re-enter.
type PublishRetry struct {
	Attempt Attempt         `json:"attempt"`
	Job     Job             `json:"job"`
	Records []PublishRecord `json:"records"`
	// Snapshot is the run's frozen definition, so the retrying worker applies
	// the same publish policy (a CI wait, say) the original attempt ran under.
	Snapshot string `json:"snapshot"`
}

// MaxCIRepairFailedChecks caps how many failed check names one CI repair
// round records, and MaxCIRepairCheckNameBytes caps each name. The round
// record is evidence of what was red, not the log itself.
const (
	MaxCIRepairFailedChecks   = 20
	MaxCIRepairCheckNameBytes = 200
)

// CI repair log bounds. A failed GitHub Actions job's log reaches the repair
// agent as its last MaxCIRepairLogBytesPerCheck bytes (the failure is almost
// always at the end), and all logs together stay within MaxCIRepairLogBytes,
// so at most MaxCIRepairLoggedChecks checks carry one. Logs are untrusted
// text headed for an agent with write access: bounded here, framed as data
// in the prompt.
const (
	MaxCIRepairLogBytesPerCheck = 16 << 10
	MaxCIRepairLogBytes         = 64 << 10
	MaxCIRepairLoggedChecks     = MaxCIRepairLogBytes / MaxCIRepairLogBytesPerCheck
)

// CIRepairRecord is one recorded CI repair round (publish.ci.on_fail): CI
// was red on HeadBefore, a repair ran the chain again, and HeadAfter is the
// fix it pushed to the same branch. Rounds chain: round 1's HeadBefore is
// the proof step's remote_ref, and every later round's HeadBefore is the
// previous round's HeadAfter. It is a record of its own rather than another
// publish step because a second push is a new fact, not a replay of the
// first.
type CIRepairRecord struct {
	AttemptID     string    `json:"attempt_id"`
	JobID         string    `json:"job_id"`
	AttemptNumber int       `json:"attempt_number"`
	Round         int       `json:"round"`
	Branch        string    `json:"branch"`
	HeadBefore    string    `json:"head_before"`
	HeadAfter     string    `json:"head_after"`
	FailedChecks  []string  `json:"failed_checks"`
	CompletedAt   time.Time `json:"completed_at"`
}

// CIRepairAuthorizationRequest asks whether this lease may push repair
// round Round over HeadBefore right now. Like a publish step, it is asked
// BEFORE the push, so a zombie or an over-budget round is refused before
// it touches the remote.
type CIRepairAuthorizationRequest struct {
	LeaseToken string `json:"lease_token"`
	Round      int    `json:"round"`
	Branch     string `json:"branch"`
	HeadBefore string `json:"head_before"`
}

// CIRepairAuthorization is a granted round. Budget is the frozen
// definition's on_fail budget. Completed is non-nil when the round is
// already recorded, and the worker must then skip the push.
type CIRepairAuthorization struct {
	AttemptID string          `json:"attempt_id"`
	Round     int             `json:"round"`
	Budget    int             `json:"budget"`
	Branch    string          `json:"branch"`
	Completed *CIRepairRecord `json:"completed,omitempty"`
}

// CIRepairRecordRequest records one pushed repair round under the lease
// token. Replaying identical values returns the stored record; different
// values for a recorded round are a conflict.
type CIRepairRecordRequest struct {
	LeaseToken   string   `json:"lease_token"`
	Round        int      `json:"round"`
	Branch       string   `json:"branch"`
	HeadBefore   string   `json:"head_before"`
	HeadAfter    string   `json:"head_after"`
	FailedChecks []string `json:"failed_checks"`
}
