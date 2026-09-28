// publish_rerun.go — declared re-runs of flaky GitHub Actions checks
// (publish.ci.rerun, plan U4, KTD5). When CI is red on the head jig pushed
// and every red check is an Actions job, jig re-runs those jobs on the same
// head before it spends a repair round or ends the attempt:
//
//	red CI → every red check an Actions job, budget left?
//	      → wait until nothing on the head is pending (GitHub refuses to
//	        re-run a job whose workflow run is still going)
//	      → freshen the lease, re-run each failed job (an "in progress"
//	        refusal waits and asks again; it spends nothing)
//	      → a fresh ci step on the same head, the re-run check runs pending
//	        until newer runs of the same name replace them
//	      → green publishes, flagged flaky; red loops while budget remains
//
// A re-run never moves the branch, so it is fenced by the lease rather than
// the ledger, and every one is recorded in the summary's ci_reruns. The
// budget is per attempt: a repair round's new head gets only what is left.
package worker

import (
	"context"
	"fmt"
	"time"

	"github.com/StructuPath/jig/internal/protocol"
)

// CIRerunSummary is one re-run as the publish summary reports it. The field
// names are pinned: the report reads ci_reruns[].{attempt, jobs, outcome}.
type CIRerunSummary struct {
	// Attempt counts re-runs within the attempt, from 1.
	Attempt int `json:"attempt"`
	// Head is the commit the jobs were re-run on.
	Head string `json:"head"`
	// Jobs names the failed jobs re-run (or, when none were sent, the ones
	// that would have been).
	Jobs []string `json:"jobs"`
	// Outcome is passed, failed, or the code the re-run stopped on.
	Outcome string `json:"outcome"`
	Detail  string `json:"detail,omitempty"`
}

const (
	ciRerunPassed = "passed"
	ciRerunFailed = "failed"
	// ciRerunRefused: GitHub refused a re-run for a reason other than a
	// workflow run still in progress.
	ciRerunRefused = "ci_rerun_refused"
	// ciRerunHeadMoved: someone pushed to the branch while jig waited to
	// re-run; there is nothing of jig's left to re-run.
	ciRerunHeadMoved = "ci_rerun_head_moved"
	// ciRerunInProgress is the gateway's code for GitHub refusing a re-run
	// because the job's workflow run has not finished. It is waited out,
	// never recorded as a spent re-run.
	ciRerunInProgress = "ci_rerun_in_progress"
	// maxCIRerunInProgressRetries bounds how often one job's re-run is asked
	// for again after an "in progress" refusal; the CI timeout bounds how
	// long.
	maxCIRerunInProgressRetries = 5
)

// isActionsJob reports whether a check is a GitHub Actions job jig can
// re-run: only those have a job id, and it is their check-run id.
func isActionsJob(check CICheck) bool {
	return check.App == "github-actions" && check.CheckRunID > 0
}

// allActionsJobs reports whether every red check is a re-runnable Actions
// job. One red check of any other kind means a re-run cannot turn CI green,
// so none is attempted (R7).
func allActionsJobs(checks []CICheck) bool {
	if len(checks) == 0 {
		return false
	}
	for _, check := range checks {
		if check.Verdict == CIFail && !isActionsJob(check) {
			return false
		}
	}
	return true
}

// rerunView is how the CI wait reads a head after re-runs: a check run jig
// just re-ran still shows its old red result until GitHub creates the new
// run, so it counts as pending until a check run of the same name that was
// not on the head before the re-run appears (KTD5).
type rerunView struct {
	known map[int64]bool // every check-run id on the head before the re-run
	rerun map[int64]bool // the check-run ids jig asked GitHub to re-run
}

func newRerunView(checks []CICheck) *rerunView {
	view := &rerunView{known: make(map[int64]bool, len(checks)), rerun: map[int64]bool{}}
	for _, check := range checks {
		if check.CheckRunID > 0 {
			view.known[check.CheckRunID] = true
		}
	}
	return view
}

// apply rewrites one poll's checks. A re-run check is dropped once as many
// new runs of its name exist as were re-run under it, and is pending until
// then. A nil view changes nothing.
func (v *rerunView) apply(checks []CICheck) []CICheck {
	if v == nil || len(v.rerun) == 0 {
		return checks
	}
	fresh, stale := map[string]int{}, map[string]int{}
	for _, check := range checks {
		switch {
		case v.rerun[check.CheckRunID]:
			stale[check.Name]++
		case check.CheckRunID > 0 && !v.known[check.CheckRunID]:
			fresh[check.Name]++
		}
	}
	viewed := make([]CICheck, 0, len(checks))
	for _, check := range checks {
		if v.rerun[check.CheckRunID] {
			if fresh[check.Name] >= stale[check.Name] {
				continue
			}
			check.Verdict = CIPending
		}
		viewed = append(viewed, check)
	}
	return viewed
}

// rerunFlakyCI runs declared re-runs while CI is red on the head jig pushed,
// every red check is an Actions job, and budget remains. summary is a
// publish summary that ended on ci_failed; the returned one is either
// published (flaky), still ci_failed (for a repair round or the end), or
// ended on the code a re-run stopped on. Every pass through the loop records
// one ci_reruns entry or returns, so it runs at most the budget's times.
func (w *Worker) rerunFlakyCI(
	ctx context.Context,
	gateway PullRequestGateway,
	options PublishOptions,
	target publishTarget,
	summary PublishSummary,
	ci ciPolicy,
) PublishSummary {
	for summary.Code == "ci_failed" && len(summary.CIReruns) < ci.rerunBudget {
		head := summary.CIRef
		if head == "" || head != summary.RemoteRef || !allActionsJobs(summary.CIFailures) {
			// Red on a head jig did not push is a person's to fix; red on a
			// check that is not an Actions job cannot be re-run.
			return summary
		}
		entry := CIRerunSummary{Attempt: len(summary.CIReruns) + 1, Head: head,
			Jobs: checkNames(summary.CIFailures)}
		deadline := time.Now().Add(ci.timeout)

		// The CI wait stopped at the first red check; its siblings may still
		// be running, and GitHub will not re-run a job in a running workflow.
		checks, err := w.settleCI(ctx, gateway, options, target, head, deadline, nil)
		if err != nil {
			return rerunStopped(summary, entry, err)
		}
		failed := failedChecks(checks)
		if len(failed) == 0 {
			// Nothing is red any more (someone re-ran it). Judge the head
			// afresh; with no re-run of jig's, a pass here is not flaky.
			return w.judgeAfterRerun(ctx, gateway, options, target, summary, ci, nil)
		}
		summary.CIFailures = boundedChecks(failed)
		entry.Jobs = checkNames(failed)
		if !allActionsJobs(failed) {
			// A sibling that finished red is not an Actions job.
			return summary
		}

		view := newRerunView(checks)
		if err := w.requestReruns(ctx, gateway, options, target, head, deadline, summary.CIFailures, view); err != nil {
			return rerunStopped(summary, entry, err)
		}
		summary = w.judgeAfterRerun(ctx, gateway, options, target, summary, ci, view)
		switch {
		case summary.Published():
			summary.CIFlaky = true
			entry.Outcome = ciRerunPassed
		case summary.Code == "ci_failed":
			entry.Outcome = ciRerunFailed
			entry.Detail = summary.Detail
		default:
			entry.Outcome = summary.Code
			entry.Detail = summary.Detail
		}
		summary.CIReruns = append(summary.CIReruns, entry)
	}
	return summary
}

// rerunStopped records a re-run that stopped before its CI wait. A lost
// lease, a cancellation, and a moved head end the publish on that code; any
// other stop (a refusal, CI that never settled) leaves CI red, so a repair
// round or the attempt's end takes it from there. Either way the entry is
// recorded, and the loop does not go round again.
func rerunStopped(summary PublishSummary, entry CIRerunSummary, err error) PublishSummary {
	code := publishCode(err)
	entry.Outcome = code
	entry.Detail = boundedText(err.Error(), protocol.MaxPublishDiagnosticBytes)
	summary.CIReruns = append(summary.CIReruns, entry)
	switch code {
	case ciRerunRefused, "ci_timeout", "ci_unavailable":
		return summary
	}
	return failSummary(summary, code, err.Error())
}

// requestReruns asks GitHub to re-run each failed job, freshening the lease
// before every request so an attempt that lost its lease sends none. An "in
// progress" refusal is waited out and asked again, within the deadline and
// maxCIRerunInProgressRetries; any other refusal stops the re-run.
func (w *Worker) requestReruns(
	ctx context.Context, gateway PullRequestGateway, options PublishOptions,
	target publishTarget, head string, deadline time.Time, failed []CICheck, view *rerunView,
) error {
	for _, job := range failed {
		for retries := 0; ; retries++ {
			if err := target.lease.freshen(ctx); err != nil {
				return fmt.Errorf("lease could not be freshened: %w", err)
			}
			err := gateway.RerunActionsJob(ctx, target.repository, job.CheckRunID)
			if err == nil {
				view.rerun[job.CheckRunID] = true
				break
			}
			if publishCode(err) != ciRerunInProgress {
				return publishFailure(ciRerunRefused, "GitHub refused to re-run %s: %s", job.Name, err.Error())
			}
			if retries == maxCIRerunInProgressRetries {
				return publishFailure(ciRerunRefused,
					"GitHub still reported %s's workflow run in progress after %d waits: %s",
					job.Name, retries, err.Error())
			}
			if err := w.ciPause(ctx, options, target, head, deadline); err != nil {
				return err
			}
			if _, err := w.settleCI(ctx, gateway, options, target, head, deadline, view); err != nil {
				return err
			}
		}
	}
	return nil
}

// judgeAfterRerun is a fresh, fenced ci step on the branch: green is
// recorded and publishes, anything else ends the step as the CI wait does.
func (w *Worker) judgeAfterRerun(
	ctx context.Context, gateway PullRequestGateway, options PublishOptions,
	target publishTarget, summary PublishSummary, ci ciPolicy, view *rerunView,
) PublishSummary {
	summary.CIFailures = nil
	green, err := w.publishStep(ctx, target, protocol.PublishStepCI, map[string]protocol.PublishRecord{}, &summary,
		func(authorization protocol.PublishAuthorization) (protocol.PublishStepRequest, error) {
			ciHead, failures, err := w.awaitCIViewed(ctx, gateway, options, target, authorization.Branch,
				ci.timeout, view)
			summary.CIRef = ciHead
			summary.CIFailures = failures
			if err != nil {
				return protocol.PublishStepRequest{}, err
			}
			return protocol.PublishStepRequest{Step: protocol.PublishStepCI,
				Branch: authorization.Branch, RemoteRef: ciHead}, nil
		})
	if err != nil {
		return summary
	}
	summary.CIRef = green.RemoteRef
	summary.State, summary.Code, summary.Detail = PublishStatePublished, "", ""
	return summary
}

// settleCI polls one head until none of its checks is pending, and returns
// them. Unlike the CI wait it does not stop at a red check, and it does not
// follow the branch: a head that moves ends it with ci_rerun_head_moved.
func (w *Worker) settleCI(
	ctx context.Context, gateway PullRequestGateway, options PublishOptions,
	target publishTarget, head string, deadline time.Time, view *rerunView,
) ([]CICheck, error) {
	transient := 0
	for {
		current, err := w.remoteHead(ctx, target, target.branch)
		if err == nil && current != head {
			return nil, publishFailure(ciRerunHeadMoved,
				"the branch moved from %s to %s while jig waited to re-run its failed jobs",
				shortSHA(head), shortSHA(current))
		}
		var checks []CICheck
		if err == nil {
			checks, err = gateway.CommitChecks(ctx, target.repository, head)
		}
		var pending []CICheck
		if err != nil {
			transient++
			if transient >= protocol.MaxCITransientFailures {
				return nil, publishFailure("ci_unavailable",
					"CI state could not be read %d times in a row: %s",
					transient, boundedText(err.Error(), protocol.MaxPublishDiagnosticBytes))
			}
		} else {
			transient = 0
			checks = view.apply(checks)
			for _, check := range checks {
				if check.Verdict == CIPending {
					pending = append(pending, check)
				}
			}
			if len(pending) == 0 {
				return checks, nil
			}
		}
		if !time.Now().Before(deadline) {
			return nil, publishFailure("ci_timeout",
				"CI on %s did not finish before the re-run could be requested; still pending: %s",
				shortSHA(head), describeChecks(pending))
		}
		if err := w.ciPause(ctx, options, target, head, deadline); err != nil {
			return nil, err
		}
	}
}

func failedChecks(checks []CICheck) []CICheck {
	var failed []CICheck
	for _, check := range checks {
		if check.Verdict == CIFail {
			failed = append(failed, check)
		}
	}
	return failed
}
