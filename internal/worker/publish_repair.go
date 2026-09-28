// publish_repair.go — the CI repair round loop (publish.ci.on_fail, plan
// U5). When publish ends red (`ci_failed`) and the engine kept its chain
// alive, the publishing runner repairs inside the attempt, one round at a
// time, until CI is green, the declared budget runs out, or a round ends any
// other way:
//
//	red CI → head still the one jig pushed? → authorize round N (fenced)
//	      → RepairCI: the on_fail phase and every phase after it, judged
//	        again by acceptance and the publish hold
//	      → commit and push the round's paths, non-force (critical section)
//	      → record round N → prove the remote head → wait for CI again
//
// Every way a round can stop short of green leaves the attempt exactly where
// red CI leaves it today: accepted_unpublished, the pull request at the last
// head jig pushed, and the reason in the publish summary. The publish-only
// retry never enters this loop (R11): it judges CI and nothing more.
package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/StructuPath/jig/internal/protocol"
)

// CIRepairSummary is one repair round as the publish summary reports it.
type CIRepairSummary struct {
	Round        int      `json:"round"`
	HeadBefore   string   `json:"head_before"`
	HeadAfter    string   `json:"head_after,omitempty"`
	FailedChecks []string `json:"failed_checks,omitempty"`
	// ChangedPaths and Acceptance are the round's own: what it changed and
	// the acceptance evidence it was pushed on. The attempt's top-level
	// result describes the chain; a pushed round is described here.
	ChangedPaths []string        `json:"changed_paths,omitempty"`
	Acceptance   json.RawMessage `json:"acceptance,omitempty"`
	// Outcome is pushed, or the code the round stopped on.
	Outcome string `json:"outcome"`
	Detail  string `json:"detail,omitempty"`
}

// ciRepairCodes are the publish summary codes a repair can end on, beyond
// the CI wait's own (ci_failed, ci_timeout, ci_unavailable, ...).
const (
	ciRepairPushed    = "pushed"
	ciRepairExhausted = "ci_repair_exhausted"
	ciRepairHeadMoved = "ci_repair_head_moved"
	ciRepairFailed    = "ci_repair_round_failed"
	ciRepairCancelled = "ci_repair_cancelled"
	ciRepairHeld      = "ci_repair_held"
	ciRepairNoChange  = "ci_repair_no_change"
)

// repairCI runs rounds while CI is red and budget remains. summary is the
// publish summary that ended on ci_failed; the returned summary is final.
// ctx must not be cancelled out from under git: cancellation reaches the
// engine through the attempt's cancel channel, exactly as it reaches the
// chain.
func (w *Worker) repairCI(
	ctx context.Context,
	gateway PullRequestGateway,
	options PublishOptions,
	target publishTarget,
	continuation Continuation,
	summary PublishSummary,
	ci ciPolicy,
) PublishSummary {
	for summary.Code == "ci_failed" {
		rounds, err := w.client.AttemptCIRepairs(ctx, target.attemptID)
		if err != nil {
			return failSummary(summary, publishCode(err), "CI repair ledger unavailable: "+err.Error())
		}
		// The head jig last pushed comes from the ledger, not from memory:
		// the last round's head_after, else the proof ref (U2 review).
		pushed := summary.RemoteRef
		if len(rounds) > 0 {
			pushed = rounds[len(rounds)-1].HeadAfter
		}
		round := len(rounds) + 1
		if round > ci.repairBudget {
			return failSummary(summary, ciRepairExhausted, fmt.Sprintf(
				"CI is still red on %s after %d repair round(s), the definition's budget; %s",
				shortSHA(summary.CIRef), ci.repairBudget, summary.Detail))
		}
		red := summary.CIRef
		if red != pushed {
			// KTD7: someone else pushed to the branch. jig does not stack an
			// agent's commits on a person's; the publish-only retry judges
			// whatever they pushed.
			return failSummary(summary, ciRepairHeadMoved, fmt.Sprintf(
				"CI is red on %s, but jig last pushed %s: someone else pushed to the branch, "+
					"so no repair round runs", shortSHA(red), shortSHA(pushed)))
		}
		names := checkNames(summary.CIFailures)
		entry := CIRepairSummary{Round: round, HeadBefore: red, FailedChecks: names}

		if err := w.authorizeRound(ctx, target, round, red); err != nil {
			return failRound(summary, entry, publishCode(err), err.Error())
		}

		failure := CIFailure{Head: red, Checks: gateway.FailedCheckLogs(ctx, target.repository, summary.CIFailures)}
		repaired := continuation.RepairCI(ctx, failure)
		entry.ChangedPaths = changedPathsFromResult(repaired.Result)
		entry.Acceptance = acceptanceFromResult(repaired.Result)
		switch {
		case repaired.State == protocol.AttemptCancelled:
			return failRound(summary, entry, ciRepairCancelled, fmt.Sprintf(
				"the job was cancelled during round %d; nothing was pushed", round))
		case repaired.State != protocol.AttemptAcceptedUnpublished:
			return failRound(summary, entry, ciRepairFailed, fmt.Sprintf(
				"round %d ended %s: %s", round, repaired.State, repaired.Error))
		case repaired.PublishHold != "":
			return failRound(summary, entry, ciRepairHeld, fmt.Sprintf(
				"round %d's fix tripped the publish hold (%s); nothing was pushed", round, repaired.PublishHold))
		case repaired.Error != "":
			return failRound(summary, entry, ciRepairNoChange, repaired.Error)
		}

		// The round may have run for a long time: fence again right before
		// the irreversible push, so an attempt whose lease was lost (and whose
		// job may have a successor by now) never pushes (R6, KTD6).
		if err := w.authorizeRound(ctx, target, round, red); err != nil {
			return failRound(summary, entry, publishCode(err), err.Error())
		}
		if err := w.requireRoundCommitsDeclared(ctx, target, red, entry.ChangedPaths); err != nil {
			return failRound(summary, entry, publishCode(err), err.Error())
		}
		head, err := w.pushAttemptBranch(ctx, options, target, entry.ChangedPaths)
		if err == nil && head == red {
			err = publishFailure(ciRepairNoChange, "round %d produced no new commit", round)
		}
		if err != nil {
			return failRound(summary, entry, publishCode(err), err.Error())
		}
		entry.HeadAfter = head
		if err := w.verifyRemoteRef(ctx, target, target.branch, head); err != nil {
			return failRound(summary, entry, publishCode(err), err.Error())
		}
		if _, err := w.client.RecordCIRepair(ctx, target.attemptID, protocol.CIRepairRecordRequest{
			LeaseToken: target.lease.token, Round: round, Branch: target.branch,
			HeadBefore: red, HeadAfter: head, FailedChecks: names,
		}); err != nil {
			return failRound(summary, entry, publishCode(err), err.Error())
		}
		entry.Outcome = ciRepairPushed
		summary.CIRepairs = append(summary.CIRepairs, entry)
		// The remote now holds the round's head: that is what was pushed.
		summary.RemoteRef = head

		// A fresh ci step on the new head: green is recorded and publishes;
		// red loops; anything else ends the attempt as the CI wait does.
		summary.CIFailures = nil
		green, err := w.publishStep(ctx, target, protocol.PublishStepCI, map[string]protocol.PublishRecord{}, &summary,
			func(authorization protocol.PublishAuthorization) (protocol.PublishStepRequest, error) {
				ciHead, failures, err := w.awaitCI(ctx, gateway, options, target, authorization.Branch, ci.timeout)
				summary.CIRef = ciHead
				summary.CIFailures = failures
				if err != nil {
					return protocol.PublishStepRequest{}, err
				}
				return protocol.PublishStepRequest{Step: protocol.PublishStepCI,
					Branch: authorization.Branch, RemoteRef: ciHead}, nil
			})
		if err == nil {
			summary.CIRef = green.RemoteRef
			summary.State, summary.Code, summary.Detail = PublishStatePublished, "", ""
			return summary
		}
		// Red on the round's own head: whatever re-run budget the earlier
		// heads left applies here before the next round is spent (R5).
		summary = w.rerunFlakyCI(ctx, gateway, options, target, summary, ci)
	}
	return summary
}

// authorizeRound fences one round under a freshened lease: the heartbeat
// first, because only it can revive an expired-but-unswept lease (R5), then
// the ledger's authorization. It runs before the round and again right
// before the push. Either way the round must not be on the ledger yet: the
// loop listed no round N, so one appearing is another publisher's, never
// ours to adopt blind.
func (w *Worker) authorizeRound(ctx context.Context, target publishTarget, round int, red string) error {
	if err := target.lease.freshen(ctx); err != nil {
		return fmt.Errorf("lease could not be freshened: %w", err)
	}
	authorization, err := w.client.AuthorizeCIRepair(ctx, target.attemptID, protocol.CIRepairAuthorizationRequest{
		LeaseToken: target.lease.token, Round: round, Branch: target.branch, HeadBefore: red,
	})
	if err != nil {
		return err
	}
	if authorization.Completed != nil {
		return publishFailure("ci_repair_conflict", "round %d was recorded concurrently", round)
	}
	return nil
}

// requireRoundCommitsDeclared refuses a round whose own commits (a
// definition's commit phase runs `git add -A`) carry a path the round never
// changed — a test artifact the chain left untracked, say. Uncommitted
// paths get the same check from commitChangedPaths; this is its twin for
// what the round committed itself.
func (w *Worker) requireRoundCommitsDeclared(ctx context.Context, target publishTarget, red string, declared []string) error {
	committed, err := runGit(ctx, target.worktreePath, "diff", "--name-only", "-z", red, "HEAD")
	if err != nil {
		return publishFailure("publish_staging_unreadable", "%s", err.Error())
	}
	allowed := make(map[string]bool, len(declared))
	for _, path := range declared {
		allowed[path] = true
	}
	var escaped []string
	for _, path := range strings.Split(committed, "\x00") {
		if path != "" && !allowed[path] {
			escaped = append(escaped, path)
		}
	}
	if len(escaped) > 0 {
		return publishFailure("publish_staging_escape",
			"the repair round committed %d path(s) it never changed (%s); publish refuses to push them",
			len(escaped), strings.Join(escaped, ", "))
	}
	return nil
}

// acceptanceFromResult is the acceptance evidence in an engine result, or
// nil.
func acceptanceFromResult(result string) json.RawMessage {
	var parsed struct {
		Acceptance json.RawMessage `json:"acceptance"`
	}
	if json.Unmarshal([]byte(result), &parsed) != nil {
		return nil
	}
	return parsed.Acceptance
}

func failSummary(summary PublishSummary, code, detail string) PublishSummary {
	summary.State = PublishStateFailed
	summary.Code = code
	summary.Detail = boundedText(detail, protocol.MaxPublishDiagnosticBytes)
	return summary
}

// failRound records the round that stopped and ends the summary on it.
func failRound(summary PublishSummary, entry CIRepairSummary, code, detail string) PublishSummary {
	entry.Outcome = code
	entry.Detail = boundedText(detail, protocol.MaxPublishDiagnosticBytes)
	summary.CIRepairs = append(summary.CIRepairs, entry)
	return failSummary(summary, code, detail)
}

func checkNames(checks []CICheck) []string {
	names := make([]string, 0, len(checks))
	for _, check := range checks {
		if len(names) == protocol.MaxCIRepairFailedChecks {
			break
		}
		names = append(names, boundedText(check.Name, protocol.MaxCIRepairCheckNameBytes))
	}
	return names
}
