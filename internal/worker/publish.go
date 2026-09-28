// publish.go — the publish pipeline (U7, R6/R12/R14, KTD6): getting accepted
// work out, fenced, idempotent, and provable.
//
// Publish is a critical section. Once an attempt's phases and acceptance
// predicate have passed, the only way its work reaches anyone is this
// pipeline, and abandoning it half-done — a branch with no pull request, a
// pull request with no proof — costs more than finishing it. So publish runs
// on a context that cannot be cancelled and never consults the attempt's
// cancellation signal: cancel is a no-op inside publish.
//
// Three idempotent steps (four with CI), each authorized before its side effect and recorded
// after it, both under the lease token (R6):
//
//	push          the attempt-scoped branch jig/<job-id>/<attempt-n>, staging
//	              exactly the engine's computed changed_paths — never `git add
//	              -A`, which is how SSSF once committed fifteen stray .pyc
//	              files
//	pull_request  found-or-created by head ref, so a re-entry after a crash
//	              adopts the existing pull request instead of opening a second
//	proof         the REMOTE ref is read back and must equal the pushed
//	              commit; a local ref or reflog entry proves nothing, and it is
//	              this proof that later unblocks worktree cleanup (R16)
//	ci            only when the definition sets publish.ci.wait: the branch's
//	              current remote head must go green before the job may be
//	              `accepted`. This step has no side effect, so it is the one
//	              place publish honours cancellation — a cancel while waiting
//	              ends it as ci_wait_cancelled, with the branch and pull
//	              request left in place
//
// A push that succeeds and a pull request that fails leaves the job
// `accepted_unpublished` with every proven step recorded, and the publish-only
// retry re-enters at the failed step against the same attempt and the same
// branch — no phase re-runs, no second branch, no second pull request.
//
// The honest limit (KTD6): the fence cannot fence GitHub. A zombie attempt
// whose lease expires between its authorization and its push still completes
// that push, and the record it then tries to write is refused. Nothing here
// prevents that. What contains it is the attempt-scoped branch name — the
// zombie can only ever touch its own attempt's branch, never the successor's —
// and what surfaces it is StrayPublishBranches, which reads the remote and the
// ledger and reports every attempt-scoped branch that no fenced push record
// accounts for.
package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/StructuPath/jig/internal/protocol"
)

// ---- diagnostics -----------------------------------------------------------

// publishError carries an actionable diagnostic code alongside its message,
// matching the posture of factory's automation runtime: an operator reading a
// failed publish gets a code they can act on, not a stack trace.
type publishError struct {
	code    string
	message string
}

func (e *publishError) Error() string { return e.message }

// Code reports the diagnostic code.
func (e *publishError) Code() string { return e.code }

func publishFailure(code, format string, arguments ...any) error {
	return &publishError{code: code, message: fmt.Sprintf(format, arguments...)}
}

// publishCode extracts the actionable code from any error the pipeline
// produces: publish diagnostics keep their own, control-plane rejections keep
// theirs (lease_not_owner, publish_step_out_of_order), everything else is
// generically failed.
func publishCode(err error) string {
	var diagnostic *publishError
	if errors.As(err, &diagnostic) {
		return diagnostic.code
	}
	if code := errorCode(err); code != "" {
		return code
	}
	return "publish_failed"
}

// ---- publish summary -------------------------------------------------------

// Publish summary states, written into the attempt result in place of the
// engine's "publish": "not_attempted" marker.
const (
	PublishStatePublished = "published"
	PublishStateFailed    = "failed"
	// PublishStateHeld: the definition's publish.hold_when held, so nothing
	// was pushed. Not a failure — the retry action is the release.
	PublishStateHeld = "held"
)

// PublishSummary is what publish writes into the attempt's result: the
// verdict, the proof, and which steps this run performed versus adopted from
// the ledger. Reused steps are the visible half of idempotency — a retry that
// says it reused push and performed pull_request is a retry that provably did
// not push twice.
type PublishSummary struct {
	State          string `json:"state"`
	Code           string `json:"code,omitempty"`
	Detail         string `json:"detail,omitempty"`
	Branch         string `json:"branch,omitempty"`
	RemoteRef      string `json:"remote_ref,omitempty"`
	PullRequestURL string `json:"pr_url,omitempty"`
	// CIRef is the head commit CI was judged on; CIFailures names the checks
	// that were red (or still pending at the timeout), bounded.
	CIRef      string    `json:"ci_ref,omitempty"`
	CIFailures []CICheck `json:"ci_failures,omitempty"`
	// CIRepairs lists the CI repair rounds this publish ran, in order.
	CIRepairs []CIRepairSummary `json:"ci_repairs,omitempty"`
	// CIReruns lists the flaky-check re-runs this publish ran, in order;
	// CIFlaky says CI went green only after one (R6), never a clean pass.
	CIReruns  []CIRerunSummary `json:"ci_reruns,omitempty"`
	CIFlaky   bool             `json:"ci_flaky,omitempty"`
	Performed []string         `json:"performed_steps,omitempty"`
	Reused    []string         `json:"reused_steps,omitempty"`
}

// Published reports whether the pipeline completed with remote proof — the
// publish half of R12's acceptance conjunct.
func (s PublishSummary) Published() bool { return s.State == PublishStatePublished }

// ---- the gh seam -----------------------------------------------------------

// PullRequest is one pull request as the gateway reports it.
type PullRequest struct {
	Number int    `json:"number"`
	URL    string `json:"url"`
	State  string `json:"state"`
}

// PullRequestRequest is one find-or-create by head ref.
type PullRequestRequest struct {
	// Repository is the job's registered repository identity, never a URL
	// from any other source.
	Repository string
	// Head is the attempt-scoped branch, already pushed.
	Head string
	// Base is the target branch; empty means the repository's default.
	Base  string
	Title string
	Body  string
}

// PullRequestGateway is the seam over `gh`. Find-or-create is one operation
// rather than two so idempotency is the gateway's contract: whatever the
// implementation, calling it twice for one head ref must yield one pull
// request. Tests substitute a fake; no unit test reaches GitHub.
type PullRequestGateway interface {
	FindOrCreatePullRequest(ctx context.Context, request PullRequestRequest) (PullRequest, error)
	// CommitChecks reports every CI signal attached to one commit: check runs
	// and legacy commit statuses alike, each normalized to pending, pass, or
	// fail.
	CommitChecks(ctx context.Context, repository, sha string) ([]CICheck, error)
	// FailedCheckLogs returns checks with the tail of each GitHub Actions
	// job's log attached, within the CI repair log bounds. It never fails:
	// a log that cannot be read leaves a LogNote saying why.
	FailedCheckLogs(ctx context.Context, repository string, checks []CICheck) []CICheck
	// ActionsJobRun reports the workflow run one Actions job belongs to.
	ActionsJobRun(ctx context.Context, repository string, jobID int64) (int64, error)
	// RerunFailedJobs asks GitHub to re-run every failed job of one
	// workflow run on the same commit. A refusal because the run is still
	// in progress carries the code ci_rerun_in_progress, so it can be
	// waited out rather than counted.
	RerunFailedJobs(ctx context.Context, repository string, runID int64) error
}

// CI check verdicts, normalized across check runs and commit statuses.
const (
	CIPending = "pending"
	CIPass    = "pass"
	CIFail    = "fail"
)

// CICheck is one CI signal on a commit.
type CICheck struct {
	Name string `json:"name"`
	// Verdict is CIPending, CIPass, or CIFail.
	Verdict string `json:"verdict"`
	// Conclusion is GitHub's own word (failure, timed_out, error, …), kept
	// for the diagnostic.
	Conclusion string `json:"conclusion,omitempty"`
	URL        string `json:"url,omitempty"`
	// CheckRunID and App identify a check run; for App "github-actions" the
	// check run id is the Actions job id, which is how its log is found.
	CheckRunID int64  `json:"check_run_id,omitempty"`
	App        string `json:"app,omitempty"`
	// LogTail is the end of the job's log, attached for a CI repair round;
	// LogNote says why there is none.
	LogTail string `json:"log_tail,omitempty"`
	LogNote string `json:"log_note,omitempty"`
}

// ---- publish options -------------------------------------------------------

// PublishOptions configures the publish pipeline. Every field is operator
// content: nothing an agent produced is ever interpolated into a command.
type PublishOptions struct {
	// CommitAuthorName and CommitAuthorEmail identify jig's publish commit.
	// Agent subprocesses run under an ephemeral HOME with no git identity
	// (R21), so publish carries its own rather than inheriting one.
	CommitAuthorName  string
	CommitAuthorEmail string
	// BaseBranch is the pull request's target branch; empty resolves the
	// repository's default branch through the gateway.
	BaseBranch string
	// CIPollInterval and CIRegistrationGrace override the protocol defaults;
	// zero means the default. Tests shrink them; operators need not.
	CIPollInterval      time.Duration
	CIRegistrationGrace time.Duration
}

func (o PublishOptions) ciPollInterval() time.Duration {
	if o.CIPollInterval > 0 {
		return o.CIPollInterval
	}
	return protocol.CIPollInterval
}

func (o PublishOptions) ciRegistrationGrace() time.Duration {
	if o.CIRegistrationGrace > 0 {
		return o.CIRegistrationGrace
	}
	return protocol.CIRegistrationGrace
}

// ciPolicy is what the frozen definition says about CI: whether publish
// waits for it, and for how long.
type ciPolicy struct {
	wait    bool
	timeout time.Duration
	// repairBudget is publish.ci.on_fail's budget; zero means red CI ends
	// the attempt, as it always has.
	repairBudget int
	// rerunBudget is publish.ci.rerun's budget: flaky-check re-runs per
	// attempt; zero means none.
	rerunBudget int
}

// ciPolicyFor reads the policy out of a run snapshot. A snapshot that does
// not parse could not have run; publish does not wait on it, and the control
// plane — which parses the same snapshot — refuses to accept the attempt, so
// a parse failure can never turn into an unchecked `accepted`.
func ciPolicyFor(snapshot string) ciPolicy {
	spec, err := protocol.ParseDefinition([]byte(snapshot))
	if err != nil {
		return ciPolicy{}
	}
	policy := ciPolicy{wait: spec.WaitsForCI(), timeout: spec.CITimeout()}
	if policy.wait && spec.Publish.CI.OnFail != nil {
		policy.repairBudget = spec.Publish.CI.OnFail.Budget
	}
	if policy.wait && spec.Publish.CI.Rerun != nil {
		policy.rerunBudget = spec.Publish.CI.Rerun.Budget
	}
	return policy
}

func (o PublishOptions) authorName() string {
	if strings.TrimSpace(o.CommitAuthorName) == "" {
		return "jig"
	}
	return o.CommitAuthorName
}

func (o PublishOptions) authorEmail() string {
	if strings.TrimSpace(o.CommitAuthorEmail) == "" {
		return "jig@localhost"
	}
	return o.CommitAuthorEmail
}

// ---- the publishing runner (the worker-path seam) --------------------------

// PublishingRunner wraps the phase engine with publish. The engine ends an
// accepted chain at `accepted_unpublished` and marks its result
// "publish": "not_attempted" (R12); this runner is what turns that into
// `accepted` with remote proof, and what leaves it `accepted_unpublished` with
// a diagnostic when publish cannot finish.
//
// It sits at the AttemptRunner seam rather than inside the attempt lifecycle
// so publish happens before the attempt's terminal state is recorded: the
// state the control plane stores is always the state publish actually
// reached.
type PublishingRunner struct {
	inner   AttemptRunner
	gateway PullRequestGateway
	options PublishOptions

	mutex  sync.Mutex
	worker *Worker
}

// NewPublishingRunner wraps inner with the publish pipeline. Bind must be
// called with the worker before the first attempt runs — the worker is built
// from a Config that already names its runner, so the two are wired in that
// order.
func NewPublishingRunner(inner AttemptRunner, gateway PullRequestGateway, options PublishOptions) *PublishingRunner {
	return &PublishingRunner{inner: inner, gateway: gateway, options: options}
}

// Bind attaches the worker whose attempts this runner publishes.
func (r *PublishingRunner) Bind(w *Worker) {
	r.mutex.Lock()
	defer r.mutex.Unlock()
	r.worker = w
}

func (r *PublishingRunner) boundWorker() *Worker {
	r.mutex.Lock()
	defer r.mutex.Unlock()
	return r.worker
}

// Run executes the wrapped runner and, when it produced accepted work,
// publishes it.
func (r *PublishingRunner) Run(ctx context.Context, prepared *PreparedAttempt) Outcome {
	outcome := r.inner.Run(ctx, prepared)
	switch outcome.State {
	case protocol.AttemptAccepted, protocol.AttemptAcceptedUnpublished:
	default:
		// Failed, cancelled, or aborted work is not published. Nothing about
		// publish applies to it.
		return outcome
	}
	if outcome.PublishHold != "" {
		// Accepted, and deliberately not shipped: the definition asked for a
		// person here. The worktree is retained like any unpublished accept
		// (R16), so the publish-only retry can release it later.
		outcome.State = protocol.AttemptAcceptedUnpublished
		outcome.Result = withPublishSummary(outcome.Result, PublishSummary{
			State: PublishStateHeld, Code: "publish_held", Detail: outcome.PublishHold})
		return outcome
	}
	worker := r.boundWorker()
	if worker == nil {
		// This outcome is rebuilt, so the continuation would be dropped
		// rather than released downstream.
		releaseContinuation(outcome)
		return Outcome{State: protocol.AttemptAcceptedUnpublished,
			Result: withPublishSummary(outcome.Result, PublishSummary{
				State: PublishStateFailed, Code: "publisher_unbound",
				Detail: "the publishing runner was never bound to a worker"}),
			Error: outcome.Error}
	}
	target := publishTarget{
		attemptID:     prepared.Claim.Attempt.ID,
		jobID:         prepared.Claim.Job.ID,
		attemptNumber: prepared.Claim.Attempt.AttemptNumber,
		repository:    prepared.Claim.Job.Repository,
		branch:        prepared.Branch,
		worktreePath:  prepared.WorktreePath,
		baseSHA:       prepared.BaseSHA,
		lease:         prepared.lease,
	}
	// The critical section: an uncancellable context, and the attempt's
	// cancellation signal deliberately unread. Every command inside still
	// carries its own timeout, so "uncancellable" is not "unbounded".
	ci := ciPolicyFor(prepared.Claim.Snapshot)
	summary := worker.publish(context.WithoutCancel(ctx), r.gateway, r.options,
		target, changedPathsFromResult(outcome.Result), ci)
	// A flaky Actions job gets its declared re-runs before red CI costs a
	// repair round or ends the attempt (R5). Re-runs never move the branch.
	summary = worker.rerunFlakyCI(context.WithoutCancel(ctx), r.gateway, r.options, target, summary, ci)
	if summary.Code == "ci_failed" && outcome.Continuation != nil && ci.repairBudget > 0 {
		// Rounds run on the same uncancelled context: cancellation reaches
		// the engine through the attempt's cancel channel and the CI wait
		// through the lease, while git and the ledger always finish.
		summary = worker.repairCI(context.WithoutCancel(ctx), r.gateway, r.options,
			target, outcome.Continuation, summary, ci)
	}
	return publishedOutcome(outcome, summary)
}

// publishedOutcome maps a publish summary onto the attempt's terminal state:
// accepted only with remote proof (R12), accepted_unpublished otherwise —
// with the diagnostic preserved in the result, because a job an operator has
// to retry deserves to say why.
func publishedOutcome(outcome Outcome, summary PublishSummary) Outcome {
	outcome.Result = withPublishSummary(outcome.Result, summary)
	if summary.Published() {
		outcome.State = protocol.AttemptAccepted
		return outcome
	}
	outcome.State = protocol.AttemptAcceptedUnpublished
	if outcome.Error == "" {
		outcome.Error = boundedText("publish did not complete ("+summary.Code+"): "+summary.Detail,
			protocol.MaxErrorBytes)
	}
	return outcome
}

// ---- the pipeline ----------------------------------------------------------

// publishTarget is one attempt's publish identity: what to publish, where,
// and under which lease.
type publishTarget struct {
	attemptID     string
	jobID         string
	attemptNumber int
	repository    string
	branch        string
	// worktreePath may be absent on a publish-only retry whose worktree was
	// already disposed under remote proof. Only the push step needs it.
	worktreePath string
	baseSHA      string
	lease        *attemptLease
}

// publish runs the three steps, skipping every one the ledger already proves.
// The returned summary is always meaningful: a failure names the step it
// stopped at and the steps it had already banked.
func (w *Worker) publish(
	ctx context.Context,
	gateway PullRequestGateway,
	options PublishOptions,
	target publishTarget,
	changedPaths []string,
	ci ciPolicy,
) PublishSummary {
	summary := PublishSummary{State: PublishStateFailed, Branch: target.branch}
	if target.branch != protocol.PublishBranch(target.jobID, target.attemptNumber) {
		// The branch the worktree was created on must be the attempt-scoped
		// one; if it is not, the containment story is already broken and
		// nothing should be pushed under it.
		summary.Code = "publish_branch_mismatch"
		summary.Detail = fmt.Sprintf("worktree branch %q is not this attempt's branch %q",
			target.branch, protocol.PublishBranch(target.jobID, target.attemptNumber))
		return summary
	}
	records, err := w.client.AttemptPublishRecords(ctx, target.attemptID)
	if err != nil {
		summary.Code = publishCode(err)
		summary.Detail = boundedText("publish ledger unavailable: "+err.Error(),
			protocol.MaxPublishDiagnosticBytes)
		return summary
	}
	proven := make(map[string]protocol.PublishRecord, len(records))
	for _, record := range records {
		proven[record.Step] = record
	}

	pushed, err := w.publishStep(ctx, target, protocol.PublishStepPush, proven, &summary,
		func(authorization protocol.PublishAuthorization) (protocol.PublishStepRequest, error) {
			sha, err := w.pushAttemptBranch(ctx, options, target, changedPaths)
			if err != nil {
				return protocol.PublishStepRequest{}, err
			}
			return protocol.PublishStepRequest{Step: protocol.PublishStepPush,
				Branch: authorization.Branch, RemoteRef: sha}, nil
		})
	if err != nil {
		return summary
	}
	summary.RemoteRef = pushed.RemoteRef

	pullRequest, err := w.publishStep(ctx, target, protocol.PublishStepPullRequest, proven, &summary,
		func(authorization protocol.PublishAuthorization) (protocol.PublishStepRequest, error) {
			created, err := gateway.FindOrCreatePullRequest(ctx, PullRequestRequest{
				Repository: target.repository,
				Head:       authorization.Branch,
				Base:       options.BaseBranch,
				Title:      publishTitle(target),
				Body:       publishBody(target, pushed.RemoteRef, changedPaths),
			})
			if err != nil {
				return protocol.PublishStepRequest{}, err
			}
			return protocol.PublishStepRequest{Step: protocol.PublishStepPullRequest,
				Branch: authorization.Branch, PullRequestURL: created.URL}, nil
		})
	if err != nil {
		return summary
	}
	summary.PullRequestURL = pullRequest.PullRequestURL

	if _, err := w.publishStep(ctx, target, protocol.PublishStepProof, proven, &summary,
		func(authorization protocol.PublishAuthorization) (protocol.PublishStepRequest, error) {
			if err := w.verifyRemoteRef(ctx, target, authorization.Branch, pushed.RemoteRef); err != nil {
				return protocol.PublishStepRequest{}, err
			}
			return protocol.PublishStepRequest{Step: protocol.PublishStepProof,
				Branch: authorization.Branch, RemoteRef: pushed.RemoteRef}, nil
		}); err != nil {
		return summary
	}
	if ci.wait {
		green, err := w.publishStep(ctx, target, protocol.PublishStepCI, proven, &summary,
			func(authorization protocol.PublishAuthorization) (protocol.PublishStepRequest, error) {
				head, failures, err := w.awaitCI(ctx, gateway, options, target, authorization.Branch, ci.timeout)
				summary.CIRef = head
				summary.CIFailures = failures
				if err != nil {
					return protocol.PublishStepRequest{}, err
				}
				return protocol.PublishStepRequest{Step: protocol.PublishStepCI,
					Branch: authorization.Branch, RemoteRef: head}, nil
			})
		if err != nil {
			return summary
		}
		summary.CIRef = green.RemoteRef
	}
	summary.State = PublishStatePublished
	summary.Code = ""
	summary.Detail = ""
	return summary
}

// publishStep is the fenced shape every step shares: authorize (which is also
// where an already-recorded step is discovered and skipped), perform, record.
// Both fence calls validate the lease token, so an attempt that loses its
// lease mid-step cannot bank the result of that step.
func (w *Worker) publishStep(
	ctx context.Context,
	target publishTarget,
	step string,
	proven map[string]protocol.PublishRecord,
	summary *PublishSummary,
	perform func(protocol.PublishAuthorization) (protocol.PublishStepRequest, error),
) (protocol.PublishRecord, error) {
	if record, exists := proven[step]; exists {
		summary.Reused = append(summary.Reused, step)
		return record, nil
	}
	fail := func(err error) (protocol.PublishRecord, error) {
		summary.Code = publishCode(err)
		summary.Detail = boundedText(fmt.Sprintf("publish step %q: %s", step, err.Error()),
			protocol.MaxPublishDiagnosticBytes)
		return protocol.PublishRecord{}, err
	}
	// Wake-safe ordering (R5): heartbeat first when the lease may have gone
	// stale, because only a heartbeat can revive an expired-but-unswept lease
	// — and only when no successor exists, which is what keeps a genuine
	// zombie out.
	if err := target.lease.freshen(ctx); err != nil {
		return fail(fmt.Errorf("lease could not be freshened: %w", err))
	}
	authorization, err := w.client.AuthorizePublishStep(ctx, target.attemptID,
		protocol.PublishAuthorizationRequest{
			LeaseToken: target.lease.token, Step: step, Branch: target.branch,
		})
	if err != nil {
		return fail(err)
	}
	if authorization.Completed != nil {
		// Recorded between our ledger read and now — another publisher for the
		// same attempt, or our own interrupted run. Adopt it, never repeat it.
		proven[step] = *authorization.Completed
		summary.Reused = append(summary.Reused, step)
		return *authorization.Completed, nil
	}
	request, err := perform(authorization)
	if err != nil {
		return fail(err)
	}
	request.LeaseToken = target.lease.token
	record, err := w.client.RecordPublishStep(ctx, target.attemptID, request)
	if err != nil {
		return fail(err)
	}
	proven[step] = record
	summary.Performed = append(summary.Performed, step)
	return record, nil
}

// ---- git side of publish ---------------------------------------------------

// pushAttemptBranch commits the engine's computed changed paths (never
// `git add -A`) and pushes the attempt-scoped branch. Both halves are
// idempotent: a worktree with nothing left to stage keeps its existing head,
// and a branch already on the remote at that head is up-to-date rather than an
// error. The push is never forced — the attempt-scoped name means nobody else
// should be writing this ref, and if someone is, that is a fact to surface,
// not to overwrite.
func (w *Worker) pushAttemptBranch(
	ctx context.Context, options PublishOptions, target publishTarget, changedPaths []string,
) (string, error) {
	if target.worktreePath == "" {
		return "", publishFailure("publish_worktree_missing",
			"the attempt worktree is gone, so its commits cannot be pushed")
	}
	if _, err := os.Stat(target.worktreePath); err != nil {
		return "", publishFailure("publish_worktree_missing",
			"the attempt worktree at %s is unavailable: %s", target.worktreePath, err)
	}
	head, err := w.commitChangedPaths(ctx, options, target, changedPaths)
	if err != nil {
		return "", err
	}
	entry, err := w.cache.entry(ctx, target.repository)
	if err != nil {
		return "", publishFailure("publish_repository_unavailable",
			"repository cache entry unavailable: %s", err)
	}
	entry.mutex.Lock()
	defer entry.mutex.Unlock()
	if _, err := runGit(ctx, target.worktreePath,
		"-c", "core.hooksPath="+w.disabledHooksPath(),
		"push", "origin", target.branch+":refs/heads/"+target.branch); err != nil {
		return "", publishFailure("publish_push_failed", "%s", err.Error())
	}
	return head, nil
}

// commitChangedPaths stages exactly the paths the phase engine computed and
// commits them. The staged set is verified against that list afterwards: if
// anything else reached the index — an ignored artifact swept in by a
// directory pathspec, a path the list did not name — publish stops rather
// than committing it. This is the SSSF `.pyc` lesson encoded as a check
// instead of a convention.
func (w *Worker) commitChangedPaths(
	ctx context.Context, options PublishOptions, target publishTarget, changedPaths []string,
) (string, error) {
	status, err := runGit(ctx, target.worktreePath, "--no-optional-locks", "status", "--porcelain=v1")
	if err != nil {
		return "", publishFailure("publish_worktree_unreadable", "%s", err.Error())
	}
	if strings.TrimSpace(status) != "" {
		if len(changedPaths) == 0 {
			return "", publishFailure("publish_no_changed_paths",
				"the worktree has uncommitted changes but the attempt result declared no changed paths; "+
					"publish stages the engine's computed paths and never the whole tree")
		}
		if len(changedPaths) > protocol.MaxPublishPaths {
			return "", publishFailure("publish_changeset_too_large",
				"the attempt declared %d changed paths, above the limit of %d",
				len(changedPaths), protocol.MaxPublishPaths)
		}
		if err := validateChangedPaths(changedPaths); err != nil {
			return "", err
		}
		for _, batch := range pathBatches(changedPaths, protocol.MaxPublishPathsPerBatch) {
			arguments := append([]string{"add", "--"}, batch...)
			if _, err := runGit(ctx, target.worktreePath, arguments...); err != nil {
				return "", publishFailure("publish_staging_failed", "%s", err.Error())
			}
		}
		staged, err := runGit(ctx, target.worktreePath, "diff", "--cached", "--name-only", "-z")
		if err != nil {
			return "", publishFailure("publish_staging_unreadable", "%s", err.Error())
		}
		if escaped := stagingEscapes(staged, changedPaths); len(escaped) > 0 {
			return "", publishFailure("publish_staging_escape",
				"the index holds %d path(s) the attempt never declared changed (%s); "+
					"publish refuses to commit them", len(escaped), strings.Join(escaped, ", "))
		}
		if strings.TrimSpace(staged) != "" {
			if _, err := runGit(ctx, target.worktreePath,
				"-c", "user.name="+options.authorName(),
				"-c", "user.email="+options.authorEmail(),
				"-c", "core.hooksPath="+w.disabledHooksPath(),
				"commit", "-m", publishCommitMessage(target, changedPaths)); err != nil {
				return "", publishFailure("publish_commit_failed", "%s", err.Error())
			}
		}
	}
	head, err := runGit(ctx, target.worktreePath, "rev-parse", "HEAD")
	if err != nil {
		return "", publishFailure("publish_head_unreadable", "%s", err.Error())
	}
	head = strings.TrimSpace(head)
	if head == target.baseSHA {
		return "", publishFailure("publish_empty_changeset",
			"the attempt worktree is still at its pinned base %s; there is nothing to publish",
			target.baseSHA)
	}
	return head, nil
}

// verifyRemoteRef is the proof of publish (R14): it asks the REMOTE what the
// branch points at. A local ref, a local reflog entry, or a remote-tracking
// ref this worker wrote itself all prove nothing — only the origin's answer
// does, and it is this answer that later lets cleanup delete the worktree.
func (w *Worker) verifyRemoteRef(ctx context.Context, target publishTarget, branch, expected string) error {
	observed, err := w.remoteHead(ctx, target, branch)
	if err != nil {
		return err
	}
	if observed == "" {
		return publishFailure("publish_proof_missing",
			"the remote has no ref refs/heads/%s; the push did not reach it", branch)
	}
	if observed != expected {
		return publishFailure("publish_proof_mismatch",
			"the remote's refs/heads/%s is %s, not the pushed commit %s", branch, observed, expected)
	}
	return nil
}

// remoteHead reads the branch's current commit on the remote itself (never a
// local ref), or "" when the remote has no such branch.
func (w *Worker) remoteHead(ctx context.Context, target publishTarget, branch string) (string, error) {
	entry, err := w.cache.entry(ctx, target.repository)
	if err != nil {
		return "", publishFailure("publish_repository_unavailable",
			"repository cache entry unavailable: %s", err)
	}
	entry.mutex.Lock()
	defer entry.mutex.Unlock()
	stdout, err := runGit(ctx, entry.dir, "ls-remote", "origin", "refs/heads/"+branch)
	if err != nil {
		return "", publishFailure("publish_proof_unavailable", "%s", err.Error())
	}
	for _, line := range strings.Split(stdout, "\n") {
		sha, ref, found := strings.Cut(strings.TrimSpace(line), "\t")
		if found && strings.TrimSpace(ref) == "refs/heads/"+branch {
			return strings.TrimSpace(sha), nil
		}
	}
	return "", nil
}

// ---- CI wait ---------------------------------------------------------------

// awaitCI waits for CI on the branch's CURRENT remote head — not only the
// commit jig pushed, so a fix a person pushed before a publish retry is the
// one judged. It returns the head CI passed on, or a diagnostic:
//
//   - ci_failed: a check concluded red. It fails fast on the first red check
//     rather than waiting for the rest — red is red.
//   - ci_timeout: checks were still pending when the definition's timeout ran
//     out.
//   - ci_wait_cancelled: the operator cancelled the job while it waited.
//   - ci_unavailable: check state could not be read MaxCITransientFailures
//     times in a row.
//
// A head with no checks at all passes once CIRegistrationGrace has elapsed:
// that repository runs no CI on this branch, and waiting the full timeout
// would only park a worker slot. A head that moves mid-wait restarts the
// grace clock but not the timeout.
func (w *Worker) awaitCI(
	ctx context.Context, gateway PullRequestGateway, options PublishOptions,
	target publishTarget, branch string, timeout time.Duration,
) (string, []CICheck, error) {
	return w.awaitCIViewed(ctx, gateway, options, target, branch, timeout, nil)
}

// awaitCIViewed is awaitCI reading each poll through a re-run view, so a
// check run jig just re-ran counts as pending until its new run replaces it
// rather than being read back red (KTD5). A nil view is the plain wait.
func (w *Worker) awaitCIViewed(
	ctx context.Context, gateway PullRequestGateway, options PublishOptions,
	target publishTarget, branch string, timeout time.Duration, view *rerunView,
) (string, []CICheck, error) {
	start := time.Now()
	deadline := start.Add(timeout)
	head, headSeen := "", start
	transient := 0
	var lastErr error
	var pending []CICheck
	for {
		current, err := w.remoteHead(ctx, target, branch)
		if err == nil && current == "" {
			err = publishFailure("publish_proof_missing",
				"the remote has no ref refs/heads/%s to run CI on", branch)
		}
		var checks []CICheck
		if err == nil {
			if current != head {
				head, headSeen = current, time.Now()
			}
			checks, err = gateway.CommitChecks(ctx, target.repository, head)
		}
		if err != nil {
			transient++
			lastErr = err
			if transient >= protocol.MaxCITransientFailures {
				return head, nil, publishFailure("ci_unavailable",
					"CI state could not be read %d times in a row: %s",
					transient, boundedText(lastErr.Error(), protocol.MaxPublishDiagnosticBytes))
			}
		} else {
			transient = 0
			checks = view.apply(checks)
			var failed []CICheck
			pending = pending[:0]
			for _, check := range checks {
				switch check.Verdict {
				case CIFail:
					failed = append(failed, check)
				case CIPending:
					pending = append(pending, check)
				}
			}
			if len(failed) > 0 {
				return head, boundedChecks(failed), publishFailure("ci_failed",
					"%d CI check(s) failed on %s: %s", len(failed), shortSHA(head), describeChecks(failed))
			}
			if len(checks) > 0 && len(pending) == 0 {
				return head, nil, nil
			}
			if len(checks) == 0 && time.Since(headSeen) >= options.ciRegistrationGrace() {
				return head, nil, nil
			}
		}
		if !time.Now().Before(deadline) {
			waiting := "no checks had registered"
			if len(pending) > 0 {
				waiting = "still pending: " + describeChecks(pending)
			}
			return head, boundedChecks(pending), publishFailure("ci_timeout",
				"CI did not finish on %s within %s; %s", shortSHA(head), timeout, waiting)
		}
		if err := w.ciPause(ctx, options, target, head, deadline); err != nil {
			return head, nil, err
		}
	}
}

// ciPause sleeps one poll interval, or until the deadline if that is
// sooner, and ends early with ci_wait_cancelled when the job is cancelled.
func (w *Worker) ciPause(
	ctx context.Context, options PublishOptions, target publishTarget, head string, deadline time.Time,
) error {
	wait := options.ciPollInterval()
	if remaining := time.Until(deadline); remaining < wait {
		wait = remaining
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-target.lease.cancelled:
		return publishFailure("ci_wait_cancelled",
			"the job was cancelled while waiting for CI on %s", shortSHA(head))
	case <-ctx.Done():
		return publishFailure("ci_wait_cancelled", "the CI wait was interrupted: %s", ctx.Err())
	case <-timer.C:
		return nil
	}
}

// maxReportedCIChecks bounds how many checks a summary names.
const maxReportedCIChecks = 20

func boundedChecks(checks []CICheck) []CICheck {
	if len(checks) > maxReportedCIChecks {
		checks = checks[:maxReportedCIChecks]
	}
	return append([]CICheck(nil), checks...)
}

func describeChecks(checks []CICheck) string {
	parts := make([]string, 0, len(checks))
	for i, check := range checks {
		if i == maxReportedCIChecks {
			parts = append(parts, fmt.Sprintf("and %d more", len(checks)-i))
			break
		}
		label := check.Name
		if check.Conclusion != "" && check.Verdict == CIFail {
			label += " (" + check.Conclusion + ")"
		}
		parts = append(parts, label)
	}
	return strings.Join(parts, ", ")
}

func shortSHA(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

// disabledHooksPath is an empty directory jig points git at for its own
// publish commands. The worktree's hooks are agent-writable repository
// content; jig's publish is not the operator's shell, and it does not execute
// them.
func (w *Worker) disabledHooksPath() string {
	path := filepath.Join(w.config.DataDir, "no-hooks")
	// A failure here is not fatal: git tolerates a hooksPath that does not
	// exist, which is the same outcome as an empty one.
	_ = os.MkdirAll(path, 0o700)
	return path
}

// ---- changed-path handling -------------------------------------------------

// changedPathsFromResult reads the engine's computed changed_paths out of the
// attempt result (R12's result contract). A result that does not parse yields
// no paths, and publish then refuses to guess.
func changedPathsFromResult(result string) []string {
	if strings.TrimSpace(result) == "" {
		return nil
	}
	var document struct {
		ChangedPaths []string `json:"changed_paths"`
	}
	if err := json.Unmarshal([]byte(result), &document); err != nil {
		return nil
	}
	return document.ChangedPaths
}

// validateChangedPaths refuses anything that could not name a file inside the
// worktree, or that git would read as something other than a literal path:
// absolute paths, parent-directory escapes, pathspec magic (`:`), leading
// dashes, and embedded NULs or newlines.
func validateChangedPaths(paths []string) error {
	for _, path := range paths {
		if path == "" {
			return publishFailure("publish_invalid_path", "the changed path list holds an empty path")
		}
		if strings.ContainsAny(path, "\x00\n") {
			return publishFailure("publish_invalid_path",
				"changed path %q contains a NUL or newline", path)
		}
		if filepath.IsAbs(path) || strings.HasPrefix(path, "-") || strings.HasPrefix(path, ":") {
			return publishFailure("publish_invalid_path",
				"changed path %q is absolute, a flag, or a pathspec expression", path)
		}
		cleaned := filepath.Clean(path)
		if cleaned == ".." || strings.HasPrefix(cleaned, ".."+string(filepath.Separator)) {
			return publishFailure("publish_invalid_path",
				"changed path %q escapes the worktree", path)
		}
	}
	return nil
}

// stagingEscapes lists staged paths that no declared changed path accounts
// for. A declared path may be a directory (the engine collapses a wholly
// ignored tree into one entry), so a staged path is covered when it equals a
// declared path or lies under one.
func stagingEscapes(stagedNUL string, changedPaths []string) []string {
	declared := make(map[string]bool, len(changedPaths))
	prefixes := make([]string, 0, len(changedPaths))
	for _, path := range changedPaths {
		cleaned := filepath.Clean(path)
		declared[cleaned] = true
		prefixes = append(prefixes, cleaned+string(filepath.Separator))
	}
	var escaped []string
	for _, staged := range strings.Split(stagedNUL, "\x00") {
		staged = strings.TrimSpace(staged)
		if staged == "" {
			continue
		}
		cleaned := filepath.Clean(staged)
		if declared[cleaned] {
			continue
		}
		covered := false
		for _, prefix := range prefixes {
			if strings.HasPrefix(cleaned, prefix) {
				covered = true
				break
			}
		}
		if !covered {
			escaped = append(escaped, staged)
		}
	}
	sort.Strings(escaped)
	return escaped
}

func pathBatches(paths []string, size int) [][]string {
	var batches [][]string
	for start := 0; start < len(paths); start += size {
		end := start + size
		if end > len(paths) {
			end = len(paths)
		}
		batches = append(batches, paths[start:end])
	}
	return batches
}

// ---- commit and pull-request text ------------------------------------------
//
// Every line below is built from control-plane-known values — job id, attempt
// number, base SHA, and the validated changed-path list. No agent-authored
// prose reaches a command line or a pull request title.

func publishCommitMessage(target publishTarget, changedPaths []string) string {
	return fmt.Sprintf("jig: job %s attempt %d\n\nAttempt: %s\nBase: %s\nChanged paths: %d\n",
		target.jobID, target.attemptNumber, target.attemptID, target.baseSHA, len(changedPaths))
}

func publishTitle(target publishTarget) string {
	return fmt.Sprintf("jig: job %s attempt %d", target.jobID, target.attemptNumber)
}

func publishBody(target publishTarget, head string, changedPaths []string) string {
	var body strings.Builder
	body.WriteString("Published by jig.\n\n")
	fmt.Fprintf(&body, "- job: `%s`\n", target.jobID)
	fmt.Fprintf(&body, "- attempt: `%s` (#%d)\n", target.attemptID, target.attemptNumber)
	fmt.Fprintf(&body, "- branch: `%s`\n", target.branch)
	fmt.Fprintf(&body, "- base: `%s`\n", target.baseSHA)
	fmt.Fprintf(&body, "- head: `%s`\n", head)
	fmt.Fprintf(&body, "\nChanged paths (%d):\n", len(changedPaths))
	shown := changedPaths
	if len(shown) > protocol.MaxPublishBodyPaths {
		shown = shown[:protocol.MaxPublishBodyPaths]
	}
	for _, path := range shown {
		fmt.Fprintf(&body, "- `%s`\n", path)
	}
	if len(shown) < len(changedPaths) {
		fmt.Fprintf(&body, "- …and %d more\n", len(changedPaths)-len(shown))
	}
	return body.String()
}

// maxPublishHistory bounds publish_history: the summaries publish-only
// retries replaced, oldest first.
const maxPublishHistory = 5

// withPublishSummary replaces the engine's "publish": "not_attempted" marker
// with what publish actually did. A summary it replaces (a retry's
// predecessor) moves to publish_history first, so the stop codes and rounds
// an attempt went through outlive the retry that followed them (R15). The
// size discipline is the engine's: cut inputs, never serialized bytes, so the
// document always parses.
func withPublishSummary(result string, summary PublishSummary) string {
	document := map[string]any{}
	if strings.TrimSpace(result) != "" {
		if err := json.Unmarshal([]byte(result), &document); err != nil {
			document = map[string]any{
				"result_unparseable": true,
			}
		}
	}
	history, _ := document["publish_history"].([]any)
	if previous, ok := document["publish"].(map[string]any); ok {
		// Only a summary object is history: the engine's "not_attempted"
		// string is the absence of one.
		history = append(history, previous)
	}
	if len(history) > maxPublishHistory {
		history = history[len(history)-maxPublishHistory:]
	}
	document["publish"] = summary
	for {
		if len(history) == 0 {
			delete(document, "publish_history")
		} else {
			document["publish_history"] = history
		}
		if body, err := json.Marshal(document); err == nil && len(body) <= protocol.MaxResultBytes {
			return string(body)
		}
		if len(history) == 0 {
			break
		}
		// History gives first, oldest entry first: the phase detail and the
		// current verdict stay as long as they can.
		history = history[1:]
	}
	// The phase envelopes are the heavy part; the verdict and the proof are
	// what a retry and an operator need.
	delete(document, "phases")
	document["truncated"] = true
	document["truncation_reason"] = "publish summary did not fit; phase detail dropped"
	if body, err := json.Marshal(document); err == nil && len(body) <= protocol.MaxResultBytes {
		return string(body)
	}
	minimal := map[string]any{
		"truncated":         true,
		"truncation_reason": "publish summary did not fit; only the publish verdict kept",
		"publish":           summary,
	}
	if body, err := json.Marshal(minimal); err == nil && len(body) <= protocol.MaxResultBytes {
		return string(body)
	}
	return `{"truncated":true,"publish":{"state":"` + summary.State + `"}}`
}

// ---- publish-only retry ----------------------------------------------------

// RetryPublish performs the publish-only retry for one accepted_unpublished
// job (R14). It re-leases the SAME attempt through the control plane, re-runs
// only the publish steps the ledger does not already prove, and records the
// attempt's terminal state. No phase runs, no agent starts, and no second
// branch or pull request is created.
//
// The attempt worktree may or may not still exist: after a push that
// succeeded, remote-ref proof lets cleanup delete it, and the remaining steps
// need only the branch that is already on the remote.
func (w *Worker) RetryPublish(
	ctx context.Context, jobID string, gateway PullRequestGateway, options PublishOptions,
) (protocol.Attempt, error) {
	token, err := mintLeaseToken()
	if err != nil {
		return protocol.Attempt{}, err
	}
	retry, err := w.client.RetryPublish(ctx, jobID, protocol.PublishRetryRequest{
		WorkerID: w.id, LeaseToken: token,
	})
	if err != nil {
		return protocol.Attempt{}, fmt.Errorf("request publish retry: %w", err)
	}
	lease := newAttemptLease(w.client, retry.Attempt.ID, token)
	heartbeatCtx, stopHeartbeat := context.WithCancel(ctx)
	defer stopHeartbeat()
	go lease.keepAlive(heartbeatCtx, w.logger)

	w.trackActive(retry.Attempt.ID)
	defer w.untrackActive(retry.Attempt.ID)

	target := publishTarget{
		attemptID:     retry.Attempt.ID,
		jobID:         retry.Job.ID,
		attemptNumber: retry.Attempt.AttemptNumber,
		repository:    retry.Job.Repository,
		branch:        protocol.PublishBranch(retry.Job.ID, retry.Attempt.AttemptNumber),
		baseSHA:       retry.Job.BaseSHA,
		lease:         lease,
	}
	worktreePath := filepath.Join(w.worktreeRoot(), retry.Attempt.ID)
	if _, err := os.Stat(worktreePath); err == nil {
		target.worktreePath = worktreePath
	}
	// The critical section again: a retry that has just pushed must not be
	// abandoned before its pull request exists.
	summary := w.publish(context.WithoutCancel(ctx), gateway, options, target,
		changedPathsFromResult(retry.Attempt.Result), ciPolicyFor(retry.Snapshot))
	outcome := publishedOutcome(Outcome{
		State: protocol.AttemptAcceptedUnpublished, Result: retry.Attempt.Result,
	}, summary)
	attempt, err := w.completeAttempt(ctx, lease, outcome)
	if err != nil {
		w.retainAfterAttempt(ctx, retry.Attempt.ID,
			"publish retry outcome could not be recorded: "+err.Error())
		return protocol.Attempt{}, err
	}
	stopHeartbeat()
	if summary.Published() {
		if err := w.disposeAttemptWorktree(ctx, retry.Attempt.ID); err != nil {
			w.logger.Warn("publish_retry_cleanup_failed",
				"attempt_id", retry.Attempt.ID, "error", err)
		}
	}
	return attempt, nil
}

// ---- stray-branch reporting ------------------------------------------------

// StrayBranch is one attempt-scoped branch on a remote that no fenced push
// record accounts for — the visible residue of the race KTD6 cannot prevent:
// a zombie attempt whose lease expired after it pushed and before it could
// record. It is reported, never deleted: a branch nobody can explain is
// evidence, and deleting evidence is worse than keeping it.
type StrayBranch struct {
	Repository    string `json:"repository"`
	Branch        string `json:"branch"`
	JobID         string `json:"job_id"`
	AttemptNumber int    `json:"attempt_number"`
	RemoteRef     string `json:"remote_ref"`
	Reason        string `json:"reason"`
}

// PublishReconcileReport is the startup reconciliation (U3) plus the
// stray-branch half publish owns. Callers that want both call
// ReconcileIncludingPublish; the plain Reconcile is unchanged for callers that
// do not.
type PublishReconcileReport struct {
	ReconcileReport
	StrayBranches []StrayBranch `json:"stray_branches"`
}

// ReconcileIncludingPublish runs the startup reconciliation and then the
// stray-branch scan.
func (w *Worker) ReconcileIncludingPublish(ctx context.Context) (PublishReconcileReport, error) {
	var report PublishReconcileReport
	base, err := w.Reconcile(ctx)
	report.ReconcileReport = base
	if err != nil {
		return report, err
	}
	stray, strayErr := w.StrayPublishBranches(ctx)
	report.StrayBranches = stray
	if strayErr != nil {
		// A scan that could not finish is surfaced, not fatal: reconciliation's
		// own results are already valid.
		w.logger.Warn("stray_branch_scan_incomplete", "error", strayErr)
	}
	for _, branch := range stray {
		w.logger.Warn("stray_publish_branch_found",
			"repository", branch.Repository, "branch", branch.Branch,
			"job_id", branch.JobID, "remote_ref", branch.RemoteRef)
	}
	return report, nil
}

// StrayPublishBranches asks every repository this worker has touched what
// jig/* branches it holds, and reports the ones the control plane has no
// fenced push record for.
func (w *Worker) StrayPublishBranches(ctx context.Context) ([]StrayBranch, error) {
	manifests, loadErr := w.manifests.loadAll()
	repositories := make([]string, 0, len(manifests))
	seen := make(map[string]bool, len(manifests))
	for _, manifest := range manifests {
		if seen[manifest.Repository] {
			continue
		}
		seen[manifest.Repository] = true
		repositories = append(repositories, manifest.Repository)
	}
	sort.Strings(repositories)

	var stray []StrayBranch
	var scanErrors []error
	if loadErr != nil {
		scanErrors = append(scanErrors, loadErr)
	}
	pushedByJob := make(map[string]map[string]bool)
	for _, repository := range repositories {
		branches, err := w.remoteAttemptBranches(ctx, repository)
		if err != nil {
			if remoteRetired(err) {
				// A remote that no longer exists can neither grow new branches
				// nor answer for its old ones, and its manifests live on here
				// forever. Reporting it as an incomplete scan on every startup
				// would bury the strays an operator can still act on, so the
				// skip is recorded and the scan stays whole for the rest.
				w.logger.Info("stray_branch_scan_skipped_retired_remote",
					"repository", repository, "error", err)
				continue
			}
			scanErrors = append(scanErrors, fmt.Errorf("%s: %w", repository, err))
			continue
		}
		reported := 0
		for _, branch := range branches {
			pushed, exists := pushedByJob[branch.JobID]
			if !exists {
				pushed = w.pushedBranchesForJob(ctx, branch.JobID, &scanErrors)
				pushedByJob[branch.JobID] = pushed
			}
			if pushed[branch.Branch] {
				continue
			}
			if reported >= protocol.MaxStrayBranchesPerRepository {
				scanErrors = append(scanErrors, fmt.Errorf(
					"%s: more than %d stray branches; the report is capped",
					repository, protocol.MaxStrayBranchesPerRepository))
				break
			}
			reported++
			branch.Repository = repository
			branch.Reason = "attempt-scoped branch on the remote with no fenced push record"
			stray = append(stray, branch)
		}
	}
	sort.Slice(stray, func(i, j int) bool {
		if stray[i].Repository != stray[j].Repository {
			return stray[i].Repository < stray[j].Repository
		}
		return stray[i].Branch < stray[j].Branch
	})
	return stray, errors.Join(scanErrors...)
}

// remoteRetired reports whether a repository-level git failure means the
// remote is gone rather than momentarily out of reach. Git has no exit code
// for "this repository no longer exists" — the distinction lives only in the
// message — so this matches the not-found signatures the hosts emit.
//
// It is deliberately narrow. A timeout, a DNS failure, or a refused
// credential prompt must not match: those mean "ask again later", and
// treating them as retired would quietly stop scanning a repository that is
// still live. The one ambiguity it cannot resolve is a private repository
// the worker's credentials no longer reach — GitHub answers not-found rather
// than forbidden so as not to leak existence — which is why the caller logs
// the skip instead of swallowing it.
func remoteRetired(err error) bool {
	if err == nil {
		return false
	}
	// A file:// remote retires by disappearing from the filesystem, and that
	// arrives typed, before git is ever run — the local analogue of the
	// not-found answers below. A missing git binary does not match: exec
	// reports that as exec.ErrNotFound, not a filesystem ENOENT.
	if errors.Is(err, os.ErrNotExist) {
		return true
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "repository not found") ||
		strings.Contains(message, "does not appear to be a git repository")
}

// pushedBranchesForJob is the set of branches the control plane holds a push
// record for. A job the control plane does not know is answered with an empty
// set — its branches are stray by definition — while any other failure is
// reported and treated as "cannot tell", which reports nothing rather than
// accusing a legitimate branch.
func (w *Worker) pushedBranchesForJob(ctx context.Context, jobID string, scanErrors *[]error) map[string]bool {
	records, err := w.client.JobPublishRecords(ctx, jobID)
	if err != nil {
		if errorCode(err) == "not_found" {
			return map[string]bool{}
		}
		*scanErrors = append(*scanErrors, fmt.Errorf("publish records for job %s: %w", jobID, err))
		// Unknown is not stray: without an answer, nothing is accused.
		return nil
	}
	pushed := make(map[string]bool, len(records))
	for _, record := range records {
		if record.Step == protocol.PublishStepPush {
			pushed[record.Branch] = true
		}
	}
	return pushed
}

// remoteAttemptBranches lists the attempt-scoped branches one repository's
// remote currently holds.
func (w *Worker) remoteAttemptBranches(ctx context.Context, repository string) ([]StrayBranch, error) {
	entry, err := w.cache.entry(ctx, repository)
	if err != nil {
		return nil, fmt.Errorf("repository cache entry unavailable: %w", err)
	}
	entry.mutex.Lock()
	defer entry.mutex.Unlock()
	stdout, err := runGit(ctx, entry.dir, "ls-remote", "--heads", "origin", "refs/heads/jig/*")
	if err != nil {
		return nil, err
	}
	var branches []StrayBranch
	for _, line := range strings.Split(stdout, "\n") {
		sha, ref, found := strings.Cut(strings.TrimSpace(line), "\t")
		if !found {
			continue
		}
		name := strings.TrimPrefix(strings.TrimSpace(ref), "refs/heads/")
		jobID, attemptNumber, ok := parseAttemptBranch(name)
		if !ok {
			continue
		}
		branches = append(branches, StrayBranch{
			Branch: name, JobID: jobID, AttemptNumber: attemptNumber,
			RemoteRef: strings.TrimSpace(sha),
		})
	}
	return branches, nil
}

// parseAttemptBranch splits jig/<job-id>/<attempt-n> back into its parts. A
// name that is not exactly that shape is not an attempt branch and is left
// alone.
func parseAttemptBranch(name string) (jobID string, attemptNumber int, ok bool) {
	parts := strings.Split(name, "/")
	if len(parts) != 3 || parts[0] != "jig" {
		return "", 0, false
	}
	if !uuidPattern.MatchString(parts[1]) {
		return "", 0, false
	}
	number, err := strconv.Atoi(parts[2])
	if err != nil || number < 1 {
		return "", 0, false
	}
	return parts[1], number, true
}

// ---- the gh gateway --------------------------------------------------------

// GitHubCLIGateway is the production PullRequestGateway: `gh` invoked with
// fixed arguments, bounded output, and one actionable diagnostic per failure
// mode (KTD7's posture, applied to publish). It never builds a shell command
// and never passes agent-authored content as an argument — titles and bodies
// come from the control plane's own values.
type GitHubCLIGateway struct {
	// LookPath and Run are injectable for tests that exercise the argument
	// construction without a gh binary.
	LookPath func(string) (string, error)
	Run      func(ctx context.Context, name string, arguments ...string) (stdout, stderr []byte, stdoutTooLarge, stderrTooLarge bool, err error)
	// RunTail is Run for output whose END matters (a CI job log): it keeps
	// the last tailBytes of stdout rather than the first.
	RunTail func(ctx context.Context, tailBytes int, name string, arguments ...string) (tail, stderr []byte, err error)
}

// NewGitHubCLIGateway builds the production gateway.
func NewGitHubCLIGateway() *GitHubCLIGateway {
	return &GitHubCLIGateway{LookPath: exec.LookPath, Run: runBoundedCommand, RunTail: runTailCommand}
}

func (g *GitHubCLIGateway) lookPath() func(string) (string, error) {
	if g.LookPath != nil {
		return g.LookPath
	}
	return exec.LookPath
}

func (g *GitHubCLIGateway) run() func(context.Context, string, ...string) ([]byte, []byte, bool, bool, error) {
	if g.Run != nil {
		return g.Run
	}
	return runBoundedCommand
}

// FindOrCreatePullRequest finds the pull request whose head is the attempt
// branch, or creates it. Creation losing a race with another publisher is
// resolved by looking again rather than by failing: one head ref, one pull
// request (R14).
func (g *GitHubCLIGateway) FindOrCreatePullRequest(ctx context.Context, request PullRequestRequest) (PullRequest, error) {
	project, err := githubProject(request.Repository)
	if err != nil {
		return PullRequest{}, err
	}
	if _, err := g.lookPath()("gh"); err != nil {
		return PullRequest{}, publishFailure("gh_not_found",
			"the GitHub CLI (gh) was not found on PATH. Install gh, then run `gh auth login`.")
	}
	existing, found, err := g.findPullRequest(ctx, project, request.Head)
	if err != nil {
		return PullRequest{}, err
	}
	if found {
		return existing, nil
	}
	base := request.Base
	if strings.TrimSpace(base) == "" {
		base, err = g.defaultBranch(ctx, project)
		if err != nil {
			return PullRequest{}, err
		}
	}
	arguments := []string{"pr", "create", "--repo", project,
		"--head", request.Head, "--base", base,
		"--title", request.Title, "--body", request.Body}
	stdout, stderr, stdoutTooLarge, stderrTooLarge, runErr := g.run()(ctx, "gh", arguments...)
	if runErr != nil {
		if strings.Contains(strings.ToLower(string(stderr)), "already exists") {
			// Another publisher created it between our lookup and our create.
			existing, found, findErr := g.findPullRequest(ctx, project, request.Head)
			if findErr == nil && found {
				return existing, nil
			}
		}
		return PullRequest{}, ghDiagnostic("gh pr create", runErr, stderr, stdoutTooLarge, stderrTooLarge)
	}
	url := firstPullRequestURL(string(stdout))
	if url == "" {
		return PullRequest{}, publishFailure("gh_malformed_output",
			"gh pr create printed no pull request URL")
	}
	return PullRequest{URL: url, State: "open"}, nil
}

func (g *GitHubCLIGateway) findPullRequest(ctx context.Context, project, head string) (PullRequest, bool, error) {
	arguments := []string{"pr", "list", "--repo", project, "--head", head,
		"--state", "all", "--limit", "2", "--json", "number,url,state,headRefName"}
	stdout, stderr, stdoutTooLarge, stderrTooLarge, err := g.run()(ctx, "gh", arguments...)
	if err != nil {
		return PullRequest{}, false, ghDiagnostic("gh pr list", err, stderr, stdoutTooLarge, stderrTooLarge)
	}
	var values []struct {
		Number      int    `json:"number"`
		URL         string `json:"url"`
		State       string `json:"state"`
		HeadRefName string `json:"headRefName"`
	}
	if err := json.Unmarshal(stdout, &values); err != nil {
		return PullRequest{}, false, publishFailure("gh_malformed_output",
			"gh pr list returned unreadable JSON: %s", boundedText(err.Error(), protocol.MaxPublishDiagnosticBytes))
	}
	for _, value := range values {
		if value.HeadRefName != head {
			continue
		}
		if strings.TrimSpace(value.URL) == "" {
			return PullRequest{}, false, publishFailure("gh_malformed_output",
				"gh pr list returned a pull request with no URL")
		}
		return PullRequest{Number: value.Number, URL: value.URL,
			State: strings.ToLower(value.State)}, true, nil
	}
	return PullRequest{}, false, nil
}

// ciSHAPattern is a full commit SHA; nothing else goes into an API path.
var ciSHAPattern = regexp.MustCompile(`^[0-9a-f]{40}([0-9a-f]{24})?$`)

// CommitChecks reads both GitHub CI surfaces for one commit: check runs
// (Actions and most apps) and the combined commit status (older integrations).
// Each is read with --jq down to one compact JSON object per line, so the
// parse never depends on how gh concatenates paginated pages.
func (g *GitHubCLIGateway) CommitChecks(ctx context.Context, repository, sha string) ([]CICheck, error) {
	project, err := githubProject(repository)
	if err != nil {
		return nil, err
	}
	if !ciSHAPattern.MatchString(sha) {
		return nil, publishFailure("ci_invalid_ref", "%q is not a commit SHA", sha)
	}
	if _, err := g.lookPath()("gh"); err != nil {
		return nil, publishFailure("gh_not_found",
			"the GitHub CLI (gh) was not found on PATH. Install gh, then run `gh auth login`.")
	}
	runs, err := g.apiLines(ctx, "gh api check-runs",
		"repos/"+project+"/commits/"+sha+"/check-runs?per_page=100",
		`.check_runs[] | {name: .name, status: .status, conclusion: (.conclusion // ""), url: (.html_url // ""), id: .id, app: (.app.slug // "")}`)
	if err != nil {
		return nil, err
	}
	statuses, err := g.apiLines(ctx, "gh api status",
		"repos/"+project+"/commits/"+sha+"/status?per_page=100",
		`.statuses[] | {name: .context, state: .state, url: (.target_url // "")}`)
	if err != nil {
		return nil, err
	}
	checks := make([]CICheck, 0, len(runs)+len(statuses))
	for _, line := range runs {
		var run struct {
			Name, Status, Conclusion, URL, App string
			ID                                 int64
		}
		if err := json.Unmarshal(line, &run); err != nil {
			return nil, publishFailure("gh_malformed_output", "gh api check-runs returned unreadable JSON")
		}
		checks = append(checks, CICheck{Name: run.Name, Verdict: checkRunVerdict(run.Status, run.Conclusion),
			Conclusion: run.Conclusion, URL: run.URL, CheckRunID: run.ID, App: run.App})
	}
	for _, line := range statuses {
		var status struct{ Name, State, URL string }
		if err := json.Unmarshal(line, &status); err != nil {
			return nil, publishFailure("gh_malformed_output", "gh api status returned unreadable JSON")
		}
		checks = append(checks, CICheck{Name: status.Name, Verdict: commitStatusVerdict(status.State),
			Conclusion: status.State, URL: status.URL})
	}
	return checks, nil
}

// FailedCheckLogs attaches the tail of each failed GitHub Actions job's log,
// read with `gh api repos/{project}/actions/jobs/{id}/logs`. Only Actions
// check runs have a log this way (their check run id is the job id); other
// checks keep name, conclusion, and URL. Only failed checks are read, logs
// are kept for at most MaxCIRepairLoggedChecks of them, and at most
// maxCILogReads reads are attempted, each under the publish command
// timeout — failed reads count — so a hanging gh costs a bounded time.
// Nothing here fails the caller: an unreadable log becomes a LogNote.
func (g *GitHubCLIGateway) FailedCheckLogs(ctx context.Context, repository string, checks []CICheck) []CICheck {
	annotated := append([]CICheck(nil), checks...)
	project, projectErr := githubProject(repository)
	_, ghErr := g.lookPath()("gh")
	logged, reads := 0, 0
	for i := range annotated {
		check := &annotated[i]
		switch {
		case check.Verdict != CIFail:
			continue
		case check.App != "github-actions" || check.CheckRunID <= 0:
			check.LogNote = "no log: not a GitHub Actions job"
		case projectErr != nil:
			check.LogNote = "no log: " + projectErr.Error()
		case ghErr != nil:
			check.LogNote = "no log: the GitHub CLI (gh) was not found on PATH"
		case logged == protocol.MaxCIRepairLoggedChecks:
			check.LogNote = fmt.Sprintf("no log: the log budget went to the first %d failed jobs",
				protocol.MaxCIRepairLoggedChecks)
		case reads == maxCILogReads:
			check.LogNote = fmt.Sprintf("no log: %d log reads were already attempted", maxCILogReads)
		default:
			reads++
			// Read twice the kept size: cleaning (timestamps, ANSI codes)
			// shrinks it, and the cut lands on a line boundary.
			read := 2 * protocol.MaxCIRepairLogBytesPerCheck
			tail, stderr, err := g.readJobLog(ctx, read, project, check.CheckRunID)
			if err != nil {
				check.LogNote = boundedText("log unavailable: "+ghDiagnostic("gh api job logs", err, stderr,
					false, false).Error(), protocol.MaxCIRepairCheckNameBytes)
				continue
			}
			check.LogTail = cleanLogTail(tail, protocol.MaxCIRepairLogBytesPerCheck, len(tail) >= read)
			logged++
		}
	}
	return annotated
}

// ActionsJobRun reads the workflow run an Actions job belongs to with
// `gh api repos/{project}/actions/jobs/{id} --jq .run_id`.
func (g *GitHubCLIGateway) ActionsJobRun(ctx context.Context, repository string, jobID int64) (int64, error) {
	project, err := g.rerunPreflight(repository, jobID)
	if err != nil {
		return 0, err
	}
	stdout, stderr, stdoutTooLarge, stderrTooLarge, err := g.run()(ctx, "gh",
		"api", "-H", "Accept: application/vnd.github+json",
		fmt.Sprintf("repos/%s/actions/jobs/%d", project, jobID), "--jq", ".run_id")
	if err != nil {
		return 0, ghDiagnostic("gh api actions job", err, stderr, stdoutTooLarge, stderrTooLarge)
	}
	run, err := strconv.ParseInt(strings.TrimSpace(string(stdout)), 10, 64)
	if err != nil || run <= 0 {
		return 0, publishFailure("gh_malformed_output", "gh api actions job %d reported no workflow run id", jobID)
	}
	return run, nil
}

// RerunFailedJobs re-runs every failed job of one workflow run with
// `gh api -X POST repos/{project}/actions/runs/{id}/rerun-failed-jobs`: on
// the same commit, never moving the branch. It is per run rather than per
// job (the plan's KTD5) because re-running one job puts its run in
// progress, and GitHub then refuses every sibling in it. GitHub refuses
// while the run is still going; that refusal is ci_rerun_in_progress.
func (g *GitHubCLIGateway) RerunFailedJobs(ctx context.Context, repository string, runID int64) error {
	project, err := g.rerunPreflight(repository, runID)
	if err != nil {
		return err
	}
	stdout, stderr, stdoutTooLarge, stderrTooLarge, err := g.run()(ctx, "gh",
		"api", "-X", "POST", "-H", "Accept: application/vnd.github+json",
		fmt.Sprintf("repos/%s/actions/runs/%d/rerun-failed-jobs", project, runID))
	if err == nil {
		return nil
	}
	if rerunInProgress(stdout) || rerunInProgress(stderr) {
		return publishFailure(ciRerunInProgress,
			"GitHub will not re-run workflow run %d while it is in progress: %s", runID,
			boundedText(strings.TrimSpace(string(stderr)), protocol.MaxPublishDiagnosticBytes))
	}
	return ghDiagnostic("gh api rerun-failed-jobs", err, stderr, stdoutTooLarge, stderrTooLarge)
}

// rerunPreflight resolves the project and refuses an id that is not one,
// before gh is run.
func (g *GitHubCLIGateway) rerunPreflight(repository string, id int64) (string, error) {
	project, err := githubProject(repository)
	if err != nil {
		return "", err
	}
	if id <= 0 {
		return "", publishFailure("ci_rerun_invalid_id", "%d is not an Actions job or run id", id)
	}
	if _, err := g.lookPath()("gh"); err != nil {
		return "", publishFailure("gh_not_found",
			"the GitHub CLI (gh) was not found on PATH. Install gh, then run `gh auth login`.")
	}
	return project, nil
}

// rerunInProgress recognizes GitHub's refusal to re-run a job whose
// workflow run has not completed. gh prints the API message on stderr and
// the response body on stdout; either may carry it.
func rerunInProgress(output []byte) bool {
	lower := strings.ToLower(string(output))
	for _, phrase := range []string{"already running", "in progress", "is running", "not complete"} {
		if strings.Contains(lower, phrase) {
			return true
		}
	}
	return false
}

// maxCILogReads bounds the log reads one FailedCheckLogs call attempts,
// failures included: twice the kept logs leaves room for expired or
// unreadable ones without letting a hanging gh cost more than
// maxCILogReads × PublishCommandTimeout.
const maxCILogReads = 2 * protocol.MaxCIRepairLoggedChecks

var (
	logTimestamp = regexp.MustCompile(`(?m)^\d{4}-\d{2}-\d{2}T[0-9:.]+Z ?`)
	logANSI      = regexp.MustCompile(`\x1b\[[0-9;?]*[A-Za-z]`)
)

// cleanLogTail keeps the last limit bytes of a job log, starting on a whole
// line, with Actions' per-line timestamps and terminal escape codes
// stripped and the result valid UTF-8. cut says raw was itself cut from a
// longer log, so its first line is a fragment and is dropped too.
func cleanLogTail(raw []byte, limit int, cut bool) string {
	text := string(raw)
	if cut {
		text = dropFirstLine(text)
	}
	text = logANSI.ReplaceAllString(logTimestamp.ReplaceAllString(text, ""), "")
	text = strings.ToValidUTF8(text, "")
	if len(text) > limit {
		text = dropFirstLine(text[len(text)-limit:])
	}
	return strings.ToValidUTF8(text, "")
}

func dropFirstLine(text string) string {
	if newline := strings.IndexByte(text, '\n'); newline >= 0 {
		return text[newline+1:]
	}
	return ""
}

// readJobLog reads one Actions job log through gh. Job logs carry terminal
// escape codes, and gh refuses to print those unless told it may; cleanLogTail
// strips them afterwards. A gh too old to know the flag says so, and is asked
// again without it (it printed escape codes unconditionally).
func (g *GitHubCLIGateway) readJobLog(ctx context.Context, read int, project string, jobID int64) ([]byte, []byte, error) {
	path := fmt.Sprintf("repos/%s/actions/jobs/%d/logs", project, jobID)
	tail, stderr, err := g.runTail()(ctx, read, "gh",
		"api", "--allow-escape-sequences", "-H", "Accept: application/vnd.github+json", path)
	if err != nil && bytes.Contains(stderr, []byte("unknown flag: --allow-escape-sequences")) {
		return g.runTail()(ctx, read, "gh", "api", "-H", "Accept: application/vnd.github+json", path)
	}
	return tail, stderr, err
}

func (g *GitHubCLIGateway) runTail() func(context.Context, int, string, ...string) ([]byte, []byte, error) {
	if g.RunTail != nil {
		return g.RunTail
	}
	return runTailCommand
}

func (g *GitHubCLIGateway) apiLines(ctx context.Context, label, path, filter string) ([][]byte, error) {
	stdout, stderr, stdoutTooLarge, stderrTooLarge, err := g.run()(ctx, "gh",
		"api", "--paginate", "-H", "Accept: application/vnd.github+json", path, "--jq", filter)
	if err != nil {
		return nil, ghDiagnostic(label, err, stderr, stdoutTooLarge, stderrTooLarge)
	}
	var lines [][]byte
	for _, line := range bytes.Split(stdout, []byte("\n")) {
		if line = bytes.TrimSpace(line); len(line) > 0 {
			lines = append(lines, line)
		}
	}
	return lines, nil
}

// checkRunVerdict maps a GitHub check run. Neutral and skipped conclusions
// are not failures; anything else that completed without success is.
func checkRunVerdict(status, conclusion string) string {
	if status != "completed" {
		return CIPending
	}
	switch conclusion {
	case "success", "neutral", "skipped":
		return CIPass
	case "":
		return CIPending
	}
	return CIFail
}

// commitStatusVerdict maps a legacy commit status state.
func commitStatusVerdict(state string) string {
	switch state {
	case "success":
		return CIPass
	case "failure", "error":
		return CIFail
	}
	return CIPending
}

func (g *GitHubCLIGateway) defaultBranch(ctx context.Context, project string) (string, error) {
	// `gh repo view` takes the project positionally — it has no --repo flag,
	// unlike every other gh subcommand publish uses. githubProject has already
	// refused anything that could be read as an option.
	stdout, stderr, stdoutTooLarge, stderrTooLarge, err := g.run()(ctx, "gh",
		"repo", "view", project, "--json", "defaultBranchRef")
	if err != nil {
		return "", ghDiagnostic("gh repo view", err, stderr, stdoutTooLarge, stderrTooLarge)
	}
	var value struct {
		DefaultBranchRef struct {
			Name string `json:"name"`
		} `json:"defaultBranchRef"`
	}
	if err := json.Unmarshal(stdout, &value); err != nil || strings.TrimSpace(value.DefaultBranchRef.Name) == "" {
		return "", publishFailure("gh_malformed_output",
			"gh repo view did not report a default branch for %s", project)
	}
	return value.DefaultBranchRef.Name, nil
}

// githubProject maps a registered repository identity onto gh's owner/repo
// argument. Anything that is not a github.com identity has no publish path in
// v1 and says so plainly rather than half-publishing.
func githubProject(repository string) (string, error) {
	identity, err := normalizeRepositoryIdentity(repository)
	if err != nil {
		return "", publishFailure("publish_unsupported_remote",
			"repository identity %q is not recognized", repository)
	}
	if !strings.HasPrefix(identity, "github.com/") {
		return "", publishFailure("publish_unsupported_remote",
			"jig v1 publishes to GitHub only; %q has no pull-request path", identity)
	}
	project := strings.TrimPrefix(identity, "github.com/")
	if strings.Count(project, "/") != 1 || strings.HasPrefix(project, "-") {
		return "", publishFailure("publish_unsupported_remote",
			"repository identity %q is not an owner/repo GitHub project", identity)
	}
	return project, nil
}

func firstPullRequestURL(stdout string) string {
	for _, line := range strings.Split(stdout, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "https://") && strings.Contains(line, "/pull/") {
			return line
		}
	}
	return ""
}

// ghDiagnostic turns one failed gh invocation into an actionable code.
func ghDiagnostic(command string, err error, stderr []byte, stdoutTooLarge, stderrTooLarge bool) error {
	if stdoutTooLarge {
		return publishFailure("gh_output_too_large",
			"%s produced more than %d bytes of output", command, protocol.MaxPublishStdoutBytes)
	}
	if stderrTooLarge {
		return publishFailure("gh_error_output_too_large",
			"%s produced more than %d bytes of error output", command, protocol.MaxPublishStderrBytes)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return publishFailure("gh_timed_out",
			"%s did not finish within %s. Check GitHub connectivity and retry the publish.",
			command, protocol.PublishCommandTimeout)
	}
	if errors.Is(err, context.Canceled) {
		return publishFailure("gh_cancelled", "%s was cancelled before completion", command)
	}
	message := strings.TrimSpace(string(stderr))
	lower := strings.ToLower(message)
	switch {
	case strings.Contains(lower, "auth") || strings.Contains(lower, "not logged") ||
		strings.Contains(lower, "login"):
		return publishFailure("gh_unauthenticated",
			"gh is not authenticated for github.com. Run `gh auth login` and verify with `gh auth status`.")
	case strings.Contains(lower, "could not resolve") || strings.Contains(lower, "not found"):
		return publishFailure("gh_repository_unavailable",
			"%s could not reach the repository: %s", command,
			boundedText(message, protocol.MaxPublishDiagnosticBytes))
	}
	if message == "" {
		message = err.Error()
	}
	return publishFailure("gh_failed", "%s failed: %s", command,
		boundedText(message, protocol.MaxPublishDiagnosticBytes))
}

// runBoundedCommand runs one external command with a bounded timeout and
// bounded capture on both streams, reporting truncation rather than silently
// parsing a cut-off answer.
func runBoundedCommand(ctx context.Context, name string, arguments ...string) ([]byte, []byte, bool, bool, error) {
	ctx, cancel := context.WithTimeout(ctx, protocol.PublishCommandTimeout)
	defer cancel()
	command := exec.CommandContext(ctx, name, arguments...)
	stdout := &limitBuffer{limit: protocol.MaxPublishStdoutBytes}
	stderr := &limitBuffer{limit: protocol.MaxPublishStderrBytes}
	command.Stdout = stdout
	command.Stderr = stderr
	err := command.Run()
	if ctx.Err() != nil {
		err = ctx.Err()
	}
	return stdout.Bytes(), stderr.Bytes(), stdout.truncated, stderr.truncated, err
}

// runTailCommand runs a command under the publish timeout and keeps only the
// last tailBytes of its stdout.
func runTailCommand(ctx context.Context, tailBytes int, name string, arguments ...string) ([]byte, []byte, error) {
	ctx, cancel := context.WithTimeout(ctx, protocol.PublishCommandTimeout)
	defer cancel()
	command := exec.CommandContext(ctx, name, arguments...)
	stdout := &tailBuffer{limit: tailBytes}
	stderr := &limitBuffer{limit: protocol.MaxPublishStderrBytes}
	command.Stdout = stdout
	command.Stderr = stderr
	err := command.Run()
	if ctx.Err() != nil {
		err = ctx.Err()
	}
	return stdout.Bytes(), stderr.Bytes(), err
}

// tailBuffer keeps the last limit bytes written to it.
type tailBuffer struct {
	data  []byte
	limit int
}

func (b *tailBuffer) Write(value []byte) (int, error) {
	b.data = append(b.data, value...)
	if len(b.data) > b.limit {
		b.data = append(b.data[:0], b.data[len(b.data)-b.limit:]...)
	}
	return len(value), nil
}

func (b *tailBuffer) Bytes() []byte { return b.data }

type limitBuffer struct {
	buffer    bytes.Buffer
	limit     int
	truncated bool
}

func (b *limitBuffer) Write(value []byte) (int, error) {
	original := len(value)
	remaining := b.limit - b.buffer.Len()
	if remaining > 0 {
		if len(value) > remaining {
			value = value[:remaining]
		}
		_, _ = b.buffer.Write(value)
	}
	if original > remaining {
		b.truncated = true
	}
	return original, nil
}

func (b *limitBuffer) Bytes() []byte { return b.buffer.Bytes() }

// ---- control-plane client (publish routes) ---------------------------------

// AuthorizePublishStep asks the control plane whether this lease may perform
// one publish step, and whether it is already recorded (R6, R14).
func (c *Client) AuthorizePublishStep(
	ctx context.Context, attemptID string, request protocol.PublishAuthorizationRequest,
) (protocol.PublishAuthorization, error) {
	var authorization protocol.PublishAuthorization
	err := c.call(ctx, http.MethodPost, "/api/attempts/"+url.PathEscape(attemptID)+"/publish/authorize",
		request, &authorization)
	return authorization, err
}

// RecordPublishStep records one completed publish step (fenced, R6).
func (c *Client) RecordPublishStep(
	ctx context.Context, attemptID string, request protocol.PublishStepRequest,
) (protocol.PublishRecord, error) {
	var record protocol.PublishRecord
	err := c.call(ctx, http.MethodPost, "/api/attempts/"+url.PathEscape(attemptID)+"/publish/record",
		request, &record)
	return record, err
}

// AttemptPublishRecords reads one attempt's proven publish steps.
func (c *Client) AttemptPublishRecords(ctx context.Context, attemptID string) ([]protocol.PublishRecord, error) {
	var records []protocol.PublishRecord
	err := c.call(ctx, http.MethodGet, "/api/attempts/"+url.PathEscape(attemptID)+"/publish", nil, &records)
	return records, err
}

// JobPublishRecords reads every proven publish step of one job's attempts.
func (c *Client) JobPublishRecords(ctx context.Context, jobID string) ([]protocol.PublishRecord, error) {
	var records []protocol.PublishRecord
	err := c.call(ctx, http.MethodGet, "/api/jobs/"+url.PathEscape(jobID)+"/publish", nil, &records)
	return records, err
}

// AuthorizeCIRepair asks whether this lease may push one CI repair round
// (fenced, before the push).
func (c *Client) AuthorizeCIRepair(
	ctx context.Context, attemptID string, request protocol.CIRepairAuthorizationRequest,
) (protocol.CIRepairAuthorization, error) {
	var authorization protocol.CIRepairAuthorization
	err := c.call(ctx, http.MethodPost,
		"/api/attempts/"+url.PathEscape(attemptID)+"/publish/ci-repair/authorize", request, &authorization)
	return authorization, err
}

// RecordCIRepair records one pushed CI repair round (fenced, after the push).
func (c *Client) RecordCIRepair(
	ctx context.Context, attemptID string, request protocol.CIRepairRecordRequest,
) (protocol.CIRepairRecord, error) {
	var record protocol.CIRepairRecord
	err := c.call(ctx, http.MethodPost,
		"/api/attempts/"+url.PathEscape(attemptID)+"/publish/ci-repair/record", request, &record)
	return record, err
}

// AttemptCIRepairs reads one attempt's recorded CI repair rounds.
func (c *Client) AttemptCIRepairs(ctx context.Context, attemptID string) ([]protocol.CIRepairRecord, error) {
	var records []protocol.CIRepairRecord
	err := c.call(ctx, http.MethodGet,
		"/api/attempts/"+url.PathEscape(attemptID)+"/publish/ci-repairs", nil, &records)
	return records, err
}

// RetryPublish requests the publish-only retry of one accepted_unpublished
// job (R14).
func (c *Client) RetryPublish(
	ctx context.Context, jobID string, request protocol.PublishRetryRequest,
) (protocol.PublishRetry, error) {
	var retry protocol.PublishRetry
	err := c.call(ctx, http.MethodPost, "/api/jobs/"+url.PathEscape(jobID)+"/publish-retry", request, &retry)
	return retry, err
}
