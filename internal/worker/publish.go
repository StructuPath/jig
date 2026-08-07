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
// Three idempotent steps, each authorized before its side effect and recorded
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
	"sort"
	"strconv"
	"strings"
	"sync"

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
)

// PublishSummary is what publish writes into the attempt's result: the
// verdict, the proof, and which steps this run performed versus adopted from
// the ledger. Reused steps are the visible half of idempotency — a retry that
// says it reused push and performed pull_request is a retry that provably did
// not push twice.
type PublishSummary struct {
	State          string   `json:"state"`
	Code           string   `json:"code,omitempty"`
	Detail         string   `json:"detail,omitempty"`
	Branch         string   `json:"branch,omitempty"`
	RemoteRef      string   `json:"remote_ref,omitempty"`
	PullRequestURL string   `json:"pr_url,omitempty"`
	Performed      []string `json:"performed_steps,omitempty"`
	Reused         []string `json:"reused_steps,omitempty"`
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
	worker := r.boundWorker()
	if worker == nil {
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
	summary := worker.publish(context.WithoutCancel(ctx), r.gateway, r.options,
		target, changedPathsFromResult(outcome.Result))
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
	entry, err := w.cache.entry(ctx, target.repository)
	if err != nil {
		return publishFailure("publish_repository_unavailable",
			"repository cache entry unavailable: %s", err)
	}
	entry.mutex.Lock()
	defer entry.mutex.Unlock()
	stdout, err := runGit(ctx, entry.dir, "ls-remote", "origin", "refs/heads/"+branch)
	if err != nil {
		return publishFailure("publish_proof_unavailable", "%s", err.Error())
	}
	observed := ""
	for _, line := range strings.Split(stdout, "\n") {
		sha, ref, found := strings.Cut(strings.TrimSpace(line), "\t")
		if !found || strings.TrimSpace(ref) != "refs/heads/"+branch {
			continue
		}
		observed = strings.TrimSpace(sha)
		break
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

// withPublishSummary replaces the engine's "publish": "not_attempted" marker
// with what publish actually did. The size discipline is the engine's: cut
// inputs, never serialized bytes, so the document always parses.
func withPublishSummary(result string, summary PublishSummary) string {
	document := map[string]any{}
	if strings.TrimSpace(result) != "" {
		if err := json.Unmarshal([]byte(result), &document); err != nil {
			document = map[string]any{
				"result_unparseable": true,
			}
		}
	}
	document["publish"] = summary
	if body, err := json.Marshal(document); err == nil && len(body) <= protocol.MaxResultBytes {
		return string(body)
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
		changedPathsFromResult(retry.Attempt.Result))
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
}

// NewGitHubCLIGateway builds the production gateway.
func NewGitHubCLIGateway() *GitHubCLIGateway {
	return &GitHubCLIGateway{LookPath: exec.LookPath, Run: runBoundedCommand}
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

// RetryPublish requests the publish-only retry of one accepted_unpublished
// job (R14).
func (c *Client) RetryPublish(
	ctx context.Context, jobID string, request protocol.PublishRetryRequest,
) (protocol.PublishRetry, error) {
	var retry protocol.PublishRetry
	err := c.call(ctx, http.MethodPost, "/api/jobs/"+url.PathEscape(jobID)+"/publish-retry", request, &retry)
	return retry, err
}
