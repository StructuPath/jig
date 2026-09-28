// publish_rerun.go — declared re-runs of flaky GitHub Actions checks
// (publish.ci.rerun, plan U4, KTD5). When CI is red on the head jig pushed
// and every red check is an Actions job, jig re-runs those jobs on the same
// head before it spends a repair round or ends the attempt:
//
//	red CI → every red check an Actions job, budget left?
//	      → wait until nothing on the head is pending (GitHub refuses to
//	        re-run a job whose workflow run is still going)
//	      → group the failed jobs by workflow run; per run, fence on the
//	        lease and ask GitHub to re-run its failed jobs (an "in progress"
//	        refusal waits and asks again; it spends nothing)
//	      → judge the SAME head again, the re-run check runs pending until
//	        newer runs of the same name replace them
//	      → green publishes, flagged flaky; red loops while budget remains
//
// One re-run — settle, requests, and judgement — fits inside one CI
// timeout. A re-run never moves the branch, so it is fenced by the lease
// rather than the ledger, and every one is recorded in the summary's
// ci_reruns. It never turns a red CI that a repair round could take into a
// terminal stop: a re-run that GitHub accepted but that never finished is
// recorded, and CI is left red as it was. The budget is per attempt: a
// repair round's new head gets only what is left.
//
// Re-runs are per workflow run (`rerun-failed-jobs`), not per job as the
// plan's KTD5 first said: re-running one job puts its run in progress, so
// GitHub refuses every sibling in the same run until it finishes, and N
// failed matrix shards would cost N workflow durations.
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
	// because the workflow run has not finished. It is waited out, never
	// recorded as a spent re-run.
	ciRerunInProgress = "ci_rerun_in_progress"
	// ciRerunCancelled: the job was cancelled before a re-run request.
	ciRerunCancelled = "ci_rerun_cancelled"
	// maxCIRerunInProgressRetries bounds how often one run's re-run is asked
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
// ended on the code a re-run stopped on (a moved head, a lost lease, a
// cancellation). Every pass through the loop records one ci_reruns entry or
// returns, so it runs at most the budget's times, and each pass is bounded
// by one CI timeout.
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
		// One deadline for the whole re-run: settle, requests, judgement.
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
			// afresh; with no re-run of jig's, a pass here is not flaky, and
			// a judgement that cannot finish leaves CI red as it was.
			judged := w.judgeAfterRerun(ctx, gateway, options, target, summary, head, deadline, nil)
			if unjudged(judged.Code) {
				return summary
			}
			return judged
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
		judged := w.judgeAfterRerun(ctx, gateway, options, target, summary, head, deadline, view)
		switch {
		case judged.Published():
			judged.CIFlaky = true
			entry.Outcome = ciRerunPassed
		case judged.Code == "ci_failed":
			entry.Outcome = ciRerunFailed
			entry.Detail = judged.Detail
		case unjudged(judged.Code):
			// GitHub took the re-run but it never finished (an outage, a
			// queued concurrency group) or CI could not be read: that is
			// no verdict on the code, so CI stays red exactly as it was and
			// a repair round, or the plain red stop, takes it from here.
			entry.Outcome = judged.Code
			entry.Detail = judged.Detail
			summary.CIReruns = append(summary.CIReruns, entry)
			return summary
		default:
			entry.Outcome = judged.Code
			entry.Detail = judged.Detail
		}
		summary = judged
		summary.CIReruns = append(summary.CIReruns, entry)
	}
	return summary
}

// unjudged reports whether a re-run's judgement ended without a verdict on
// the code: CI did not finish in time, or could not be read.
func unjudged(code string) bool {
	return code == "ci_timeout" || code == "ci_unavailable"
}

// rerunStopped records a re-run that stopped before its judgement. A lost
// lease, a cancellation, and a moved head end the publish on that code; any
// other stop (a refusal, CI that never settled) leaves CI red, so a repair
// round or the attempt's end takes it from there. Either way the entry is
// recorded, and the loop does not go round again.
func rerunStopped(summary PublishSummary, entry CIRerunSummary, err error) PublishSummary {
	code := publishCode(err)
	entry.Outcome = code
	entry.Detail = boundedText(err.Error(), protocol.MaxPublishDiagnosticBytes)
	summary.CIReruns = append(summary.CIReruns, entry)
	if code == ciRerunRefused || unjudged(code) {
		return summary
	}
	return failSummary(summary, code, err.Error())
}

// requestReruns asks GitHub to re-run the failed jobs of each workflow run
// they belong to, once per run. Every request is fenced: the lease is
// freshened and checked for cancellation or loss immediately before it, so
// an attempt that lost its lease or was cancelled sends none. An "in
// progress" refusal is waited out and asked again, while the deadline
// allows and at most maxCIRerunInProgressRetries times; any other refusal
// stops the re-run.
func (w *Worker) requestReruns(
	ctx context.Context, gateway PullRequestGateway, options PublishOptions,
	target publishTarget, head string, deadline time.Time, failed []CICheck, view *rerunView,
) error {
	runs, order := map[int64][]CICheck{}, []int64{}
	for _, job := range failed {
		run, err := gateway.ActionsJobRun(ctx, target.repository, job.CheckRunID)
		if err != nil {
			return publishFailure(ciRerunRefused, "the workflow run of %s could not be read: %s", job.Name, err.Error())
		}
		if _, seen := runs[run]; !seen {
			order = append(order, run)
		}
		runs[run] = append(runs[run], job)
	}
	for _, run := range order {
		jobs := runs[run]
		for retries := 0; ; retries++ {
			if err := fenceRerun(ctx, target); err != nil {
				return err
			}
			err := gateway.RerunFailedJobs(ctx, target.repository, run)
			if err == nil {
				for _, job := range jobs {
					view.rerun[job.CheckRunID] = true
				}
				break
			}
			if publishCode(err) != ciRerunInProgress {
				return publishFailure(ciRerunRefused, "GitHub refused to re-run %s: %s",
					describeChecks(jobs), err.Error())
			}
			if retries == maxCIRerunInProgressRetries {
				return publishFailure(ciRerunRefused,
					"GitHub still reported the workflow run of %s in progress after %d waits: %s",
					describeChecks(jobs), retries, err.Error())
			}
			if !time.Now().Before(deadline) {
				return publishFailure(ciRerunRefused,
					"GitHub still reported the workflow run of %s in progress at the CI timeout: %s",
					describeChecks(jobs), err.Error())
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

// fenceRerun is the lease fence before one re-run request: heartbeat if the
// lease may be stale, then refuse if the lease is known lost or the job was
// cancelled — a heartbeat reports cancellation without failing, and a fresh
// lease is not heartbeated at all, so both are checked here.
func fenceRerun(ctx context.Context, target publishTarget) error {
	if err := target.lease.freshen(ctx); err != nil {
		return fmt.Errorf("lease could not be freshened: %w", err)
	}
	if err := target.lease.lostVerdict(); err != nil {
		return fmt.Errorf("the lease was lost: %w", err)
	}
	select {
	case <-target.lease.cancelled:
		return publishFailure(ciRerunCancelled, "the job was cancelled before its failed jobs were re-run")
	default:
		return nil
	}
}

// judgeAfterRerun judges the head the re-run ran on — never whatever the
// branch has moved to — within the re-run's deadline, and records a green
// verdict through the fenced ci step. A branch that moves after the re-run
// ends it with ci_rerun_head_moved: a person's push is theirs to judge, not
// jig's flaky pass.
func (w *Worker) judgeAfterRerun(
	ctx context.Context, gateway PullRequestGateway, options PublishOptions,
	target publishTarget, summary PublishSummary, head string, deadline time.Time, view *rerunView,
) PublishSummary {
	summary.CIRef = head
	checks, err := w.settleCI(ctx, gateway, options, target, head, deadline, view)
	if err == nil && len(checks) == 0 {
		err = publishFailure("ci_unavailable", "no CI checks were reported on %s after the re-run", shortSHA(head))
	}
	if err == nil {
		if failed := failedChecks(checks); len(failed) > 0 {
			summary.CIFailures = boundedChecks(failed)
			err = publishFailure("ci_failed", "%d CI check(s) failed on %s after the re-run: %s",
				len(failed), shortSHA(head), describeChecks(failed))
		}
	}
	if err != nil {
		return failSummary(summary, publishCode(err),
			fmt.Sprintf("publish step %q: %s", protocol.PublishStepCI, err.Error()))
	}
	summary.CIFailures = nil
	green, err := w.publishStep(ctx, target, protocol.PublishStepCI, map[string]protocol.PublishRecord{}, &summary,
		func(authorization protocol.PublishAuthorization) (protocol.PublishStepRequest, error) {
			return protocol.PublishStepRequest{Step: protocol.PublishStepCI,
				Branch: authorization.Branch, RemoteRef: head}, nil
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
				"CI on %s did not finish within the re-run's CI timeout; still pending: %s",
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
