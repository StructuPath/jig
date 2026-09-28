// publish_rerun_test.go — declared re-runs of flaky Actions checks (plan
// U4) through the real worker, control plane, and git remote, with CI and
// the re-run endpoint scripted by the fake gateway.
package worker

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/StructuPath/jig/internal/protocol"
)

// rerunSnapshot waits for CI with a re-run budget and, when repairBudget is
// positive, a repair loop too.
func rerunSnapshot(rerunBudget, repairBudget int) string {
	if repairBudget == 0 {
		return integrationSnapshot + fmt.Sprintf(`publish:
  ci:
    wait: true
    rerun: {budget: %d}
`, rerunBudget)
	}
	return integrationSnapshot + fmt.Sprintf(`  - name: review
    kind: agent
    owner: builder
publish:
  ci:
    wait: true
    rerun: {budget: %d}
    on_fail: {run: build, budget: %d}
`, rerunBudget, repairBudget)
}

func actionsJob(name, verdict string, id int64) CICheck {
	check := check(name, verdict)
	check.App = "github-actions"
	check.CheckRunID = id
	return check
}

// flakyCI scripts CI per head: script is called with the 0-based index of
// the head being judged (in the order heads are first seen), the number of
// re-run requests made so far, and the 1-based poll count. It runs under
// the gateway's lock, so it reads g.reruns directly.
type flakyCI struct {
	heads  []string
	script func(head, reruns, poll int) []CICheck
}

func (c *flakyCI) install(g *fakeGateway) {
	g.checks = func(sha string, poll int) ([]CICheck, error) {
		index := slices.Index(c.heads, sha)
		if index < 0 {
			c.heads = append(c.heads, sha)
			index = len(c.heads) - 1
		}
		return c.script(index, len(g.reruns), poll), nil
	}
}

func newRerunScenario(t *testing.T, rerunBudget, repairBudget int, script func(head, reruns, poll int) []CICheck,
	rounds ...roundScript) (*repairScenario, *flakyCI) {
	t.Helper()
	s := &repairScenario{h: newHarness(t), ci: &ciScript{red: map[string]bool{}}}
	var head, identity string
	s.originDir, head, identity = newOriginRepo(t)
	s.h.seedRunWithSnapshot("run-1", rerunSnapshot(rerunBudget, repairBudget),
		protocol.RunTarget{Repository: identity, BaseSHA: head})
	s.job = s.h.enqueue("run-1", identity)
	s.branch = protocol.PublishBranch(s.job.ID, 1)
	s.continuation = &scriptedContinuation{t: t, rounds: rounds}
	s.gateway = newFakeGateway()
	ci := &flakyCI{script: script}
	ci.install(s.gateway)
	s.w = newCIWorker(t, s.h, repairRunner(t, s.continuation), s.gateway)
	return s, ci
}

// flakyTest is red until the first re-run, then its new run (a new check-run
// id) passes. lint is a green Actions sibling throughout.
func flakyTest(_, reruns, _ int) []CICheck {
	if reruns == 0 {
		return []CICheck{actionsJob("lint", CIPass, 1), actionsJob("test", CIFail, 11)}
	}
	return []CICheck{actionsJob("lint", CIPass, 1), actionsJob("test", CIPass, 12)}
}

// brokenTest is red on every run, each re-run a new check-run id.
func brokenTest(head, reruns, _ int) []CICheck {
	return []CICheck{actionsJob("lint", CIPass, 1), actionsJob("test", CIFail, int64(100*head+11+reruns))}
}

func assertReruns(t *testing.T, summary PublishSummary, want ...CIRerunSummary) {
	t.Helper()
	if len(summary.CIReruns) != len(want) {
		t.Fatalf("ci_reruns = %+v, want %d entries", summary.CIReruns, len(want))
	}
	for i, entry := range want {
		got := summary.CIReruns[i]
		if got.Attempt != entry.Attempt || got.Outcome != entry.Outcome ||
			(entry.Head != "" && got.Head != entry.Head) ||
			(entry.Jobs != nil && !slices.Equal(got.Jobs, entry.Jobs)) ||
			(entry.Detail != "" && !strings.Contains(got.Detail, entry.Detail)) {
			t.Fatalf("ci_reruns[%d] = %+v, want %+v", i, got, entry)
		}
	}
}

// A single flaky Actions job that passes on re-run publishes green, flagged
// flaky, and no repair round runs even though one was declared (R5, R6).
func TestAFlakyActionsJobPassesOnRerunAndPublishes(t *testing.T) {
	s, ci := newRerunScenario(t, 1, 1, flakyTest, fixRound())
	attempt, summary := s.run(t)
	if attempt.State != protocol.AttemptAccepted || !summary.Published() || !summary.CIFlaky {
		t.Fatalf("attempt = %s summary = %+v, want accepted, published, and flagged flaky", attempt.State, summary)
	}
	pushed := remoteBranches(t, s.originDir)[s.branch]
	assertReruns(t, summary, CIRerunSummary{Attempt: 1, Head: pushed, Jobs: []string{"test"}, Outcome: "passed"})
	if requests := s.gateway.rerunRequests(); !slices.Equal(requests, []int64{11}) {
		t.Fatalf("re-run requests = %v, want only the failed job 11", requests)
	}
	if len(s.continuation.failures) != 0 || len(s.rounds(t, attempt.ID)) != 0 || len(summary.CIRepairs) != 0 {
		t.Fatal("a repair round ran although the re-run went green")
	}
	if len(ci.heads) != 1 || summary.CIRef != pushed || summary.RemoteRef != pushed {
		t.Fatalf("heads judged = %v ci_ref = %s, want CI judged green on the one pushed head %s",
			ci.heads, summary.CIRef, pushed)
	}
	if steps := recordedSteps(t, s.w, attempt.ID); !slices.Contains(steps, protocol.PublishStepCI) {
		t.Fatalf("ledger steps = %v, want ci recorded after the re-run", steps)
	}
	if !strings.Contains(attempt.Result, `"ci_reruns":[{"attempt":1,`) ||
		!strings.Contains(attempt.Result, `"jobs":["test"],"outcome":"passed"`) {
		t.Fatalf("result = %s, want the pinned ci_reruns shape", attempt.Result)
	}
}

// A sibling still running when the first check goes red delays the re-run
// until it finishes: GitHub refuses to re-run a job in a running workflow.
func TestARerunWaitsForAPendingSiblingToFinish(t *testing.T) {
	const siblingDoneAt = 4
	var polls int
	s, _ := newRerunScenario(t, 1, 0, func(_, reruns, poll int) []CICheck {
		polls = poll
		slow := actionsJob("slow-e2e", CIPending, 21)
		if poll >= siblingDoneAt {
			slow = actionsJob("slow-e2e", CIPass, 21)
		}
		if reruns == 0 {
			return []CICheck{actionsJob("test", CIFail, 11), slow}
		}
		return []CICheck{actionsJob("test", CIPass, 12), slow}
	})
	requestedAt := 0
	s.gateway.rerun = func(int64, int) error {
		requestedAt = polls
		return nil
	}
	attempt, summary := s.run(t)
	if attempt.State != protocol.AttemptAccepted || !summary.CIFlaky {
		t.Fatalf("attempt = %s summary = %+v, want accepted after the re-run", attempt.State, summary)
	}
	if requestedAt < siblingDoneAt {
		t.Fatalf("the re-run was requested at poll %d, before the sibling finished at poll %d",
			requestedAt, siblingDoneAt)
	}
}

// The first polls after a re-run still return the old red check run: it is
// pending until a newer run of the same name appears, never read back red.
func TestTheStaleRedCheckAfterARerunIsPending(t *testing.T) {
	var rerunAt int
	s, _ := newRerunScenario(t, 1, 0, func(_, reruns, poll int) []CICheck {
		lint := actionsJob("lint", CIPass, 1)
		switch {
		case reruns == 0:
			return []CICheck{lint, actionsJob("test", CIFail, 11)}
		case poll <= rerunAt+3:
			// GitHub has not created the new run yet.
			return []CICheck{lint, actionsJob("test", CIFail, 11)}
		case poll <= rerunAt+5:
			return []CICheck{lint, actionsJob("test", CIPending, 12)}
		}
		return []CICheck{lint, actionsJob("test", CIPass, 12)}
	})
	s.gateway.rerun = func(int64, int) error {
		rerunAt = s.gateway.checkPolls
		return nil
	}
	attempt, summary := s.run(t)
	if attempt.State != protocol.AttemptAccepted {
		t.Fatalf("attempt = %s summary = %+v, want the stale red run waited past", attempt.State, summary)
	}
	assertReruns(t, summary, CIRerunSummary{Attempt: 1, Outcome: "passed"})
	if requests := s.gateway.rerunRequests(); len(requests) != 1 {
		t.Fatalf("re-run requests = %v, want one", requests)
	}
}

// A same-named check from another workflow that was already on the head is
// not the re-run's replacement: the re-run job stays pending until its own
// new run appears.
func TestAnOlderSameNamedCheckDoesNotReplaceTheRerun(t *testing.T) {
	view := newRerunView([]CICheck{actionsJob("build", CIFail, 11), actionsJob("build", CIPass, 12)})
	view.rerun[11] = true
	viewed := view.apply([]CICheck{actionsJob("build", CIFail, 11), actionsJob("build", CIPass, 12)})
	if len(viewed) != 2 || viewed[0].Verdict != CIPending {
		t.Fatalf("viewed = %+v, want the re-run check pending beside the other workflow's", viewed)
	}
	viewed = view.apply([]CICheck{actionsJob("build", CIFail, 11), actionsJob("build", CIPass, 12),
		actionsJob("build", CIPass, 13)})
	if len(viewed) != 2 || viewed[0].CheckRunID != 12 || viewed[1].CheckRunID != 13 {
		t.Fatalf("viewed = %+v, want the stale run dropped once its new run exists", viewed)
	}
	if plain := (*rerunView)(nil).apply([]CICheck{actionsJob("build", CIFail, 11)}); plain[0].Verdict != CIFail {
		t.Fatal("a nil view rewrote a check")
	}
}

// GitHub refusing because the workflow run is still in progress is waited
// out and asked again, and does not spend the budget.
func TestAnInProgressRefusalIsRetriedWithoutSpendingTheBudget(t *testing.T) {
	s, _ := newRerunScenario(t, 1, 0, flakyTest)
	s.gateway.rerun = func(_ int64, call int) error {
		if call <= 2 {
			return publishFailure(ciRerunInProgress, "This workflow is already running")
		}
		return nil
	}
	attempt, summary := s.run(t)
	if attempt.State != protocol.AttemptAccepted || !summary.CIFlaky {
		t.Fatalf("attempt = %s summary = %+v, want accepted after the retried re-run", attempt.State, summary)
	}
	assertReruns(t, summary, CIRerunSummary{Attempt: 1, Outcome: "passed"})
	if requests := s.gateway.rerunRequests(); !slices.Equal(requests, []int64{11, 11, 11}) {
		t.Fatalf("re-run requests = %v, want job 11 asked three times", requests)
	}
}

// An "in progress" refusal that never clears is bounded: after the retries
// run out it is recorded as a refusal.
func TestAnEndlessInProgressRefusalIsBounded(t *testing.T) {
	s, _ := newRerunScenario(t, 1, 0, flakyTest)
	s.gateway.rerun = func(int64, int) error {
		return publishFailure(ciRerunInProgress, "This workflow is already running")
	}
	attempt, summary := s.run(t)
	if attempt.State != protocol.AttemptAcceptedUnpublished || summary.Code != "ci_failed" {
		t.Fatalf("attempt = %s summary = %+v, want ci_failed", attempt.State, summary)
	}
	assertReruns(t, summary, CIRerunSummary{Attempt: 1, Outcome: "ci_rerun_refused", Detail: "in progress"})
	if requests := s.gateway.rerunRequests(); len(requests) != maxCIRerunInProgressRetries+1 {
		t.Fatalf("re-run requests = %d, want %d", len(requests), maxCIRerunInProgressRetries+1)
	}
}

// Any other refusal is recorded with its diagnostic and falls through: to a
// repair round when one is declared, to ci_failed when not.
func TestAnyOtherRefusalIsRecordedAndFallsThrough(t *testing.T) {
	refuse := func(int64, int) error {
		return publishFailure("gh_failed", "gh api job rerun failed: Resource not accessible by integration")
	}
	t.Run("to a repair round", func(t *testing.T) {
		s, _ := newRerunScenario(t, 2, 1, func(head, reruns, poll int) []CICheck {
			if head == 0 {
				return brokenTest(head, reruns, poll)
			}
			return []CICheck{actionsJob("lint", CIPass, 1), actionsJob("test", CIPass, 111)}
		}, fixRound())
		s.gateway.rerun = refuse
		attempt, summary := s.run(t)
		if attempt.State != protocol.AttemptAccepted || summary.CIFlaky {
			t.Fatalf("attempt = %s summary = %+v, want accepted by the repair round, not flaky", attempt.State, summary)
		}
		assertReruns(t, summary, CIRerunSummary{Attempt: 1, Outcome: "ci_rerun_refused",
			Detail: "Resource not accessible"})
		if len(summary.CIRepairs) != 1 || len(s.gateway.rerunRequests()) != 1 {
			t.Fatalf("repairs = %+v requests = %v, want one round after one refused request",
				summary.CIRepairs, s.gateway.rerunRequests())
		}
	})
	t.Run("to ci_failed", func(t *testing.T) {
		s, _ := newRerunScenario(t, 2, 0, brokenTest)
		s.gateway.rerun = refuse
		attempt, summary := s.run(t)
		if attempt.State != protocol.AttemptAcceptedUnpublished || summary.Code != "ci_failed" {
			t.Fatalf("attempt = %s summary = %+v, want ci_failed", attempt.State, summary)
		}
		assertReruns(t, summary, CIRerunSummary{Attempt: 1, Outcome: "ci_rerun_refused"})
		if requests := s.gateway.rerunRequests(); len(requests) != 1 {
			t.Fatalf("re-run requests = %v, want one: a refusal stops re-running this head", requests)
		}
	})
}

// A check still red after the budget goes on to a repair round when one is
// declared, and ends ci_failed with every re-run recorded when not.
func TestACheckStillRedAfterTheBudgetFallsThrough(t *testing.T) {
	t.Run("to a repair round", func(t *testing.T) {
		s, _ := newRerunScenario(t, 1, 1, func(head, reruns, poll int) []CICheck {
			if head == 0 {
				return brokenTest(head, reruns, poll)
			}
			return []CICheck{actionsJob("lint", CIPass, 1), actionsJob("test", CIPass, 111)}
		}, fixRound())
		attempt, summary := s.run(t)
		if attempt.State != protocol.AttemptAccepted || summary.CIFlaky {
			t.Fatalf("attempt = %s summary = %+v, want accepted by the round, not flaky", attempt.State, summary)
		}
		assertReruns(t, summary, CIRerunSummary{Attempt: 1, Outcome: "failed"})
		if len(summary.CIRepairs) != 1 || summary.CIRepairs[0].Outcome != "pushed" {
			t.Fatalf("ci_repairs = %+v, want one pushed round", summary.CIRepairs)
		}
		if len(s.continuation.failures) != 1 || s.continuation.failures[0].Checks[0].CheckRunID != 12 {
			t.Fatalf("the round was handed %+v, want the re-run's own red run", s.continuation.failures)
		}
	})
	t.Run("to ci_failed", func(t *testing.T) {
		s, _ := newRerunScenario(t, 2, 0, brokenTest)
		attempt, summary := s.run(t)
		if attempt.State != protocol.AttemptAcceptedUnpublished || summary.Code != "ci_failed" || summary.CIFlaky {
			t.Fatalf("attempt = %s summary = %+v, want ci_failed", attempt.State, summary)
		}
		assertReruns(t, summary, CIRerunSummary{Attempt: 1, Outcome: "failed"},
			CIRerunSummary{Attempt: 2, Outcome: "failed"})
		if requests := s.gateway.rerunRequests(); !slices.Equal(requests, []int64{11, 12}) {
			t.Fatalf("re-run requests = %v, want each red run re-run once, within the budget", requests)
		}
	})
}

// Any red check that is not an Actions job skips re-runs entirely (R7).
func TestARedNonActionsCheckSkipsReruns(t *testing.T) {
	for name, red := range map[string][]CICheck{
		"commit status": {check("legacy/ci", CIFail)},
		"mixed":         {actionsJob("test", CIFail, 11), check("legacy/ci", CIFail)},
		"no job id":     {actionsJob("test", CIFail, 0)},
		"other app":     {{Name: "codecov", Verdict: CIFail, App: "codecov", CheckRunID: 31}},
	} {
		t.Run(name, func(t *testing.T) {
			s, _ := newRerunScenario(t, 3, 0, func(int, int, int) []CICheck { return red })
			attempt, summary := s.run(t)
			if attempt.State != protocol.AttemptAcceptedUnpublished || summary.Code != "ci_failed" {
				t.Fatalf("attempt = %s summary = %+v, want ci_failed", attempt.State, summary)
			}
			if len(summary.CIReruns) != 0 || len(s.gateway.rerunRequests()) != 0 {
				t.Fatalf("ci_reruns = %+v requests = %v, want none", summary.CIReruns, s.gateway.rerunRequests())
			}
		})
	}
}

// A red sibling that is not an Actions job, found only once CI settles,
// also stops the re-run before any request.
func TestARedNonActionsSiblingFoundWhileSettlingSkipsTheRerun(t *testing.T) {
	s, _ := newRerunScenario(t, 1, 0, func(_, _, poll int) []CICheck {
		legacy := check("legacy/ci", CIPending)
		if poll >= 3 {
			legacy = check("legacy/ci", CIFail)
		}
		return []CICheck{actionsJob("test", CIFail, 11), legacy}
	})
	attempt, summary := s.run(t)
	if attempt.State != protocol.AttemptAcceptedUnpublished || summary.Code != "ci_failed" {
		t.Fatalf("attempt = %s summary = %+v, want ci_failed", attempt.State, summary)
	}
	if len(s.gateway.rerunRequests()) != 0 || len(summary.CIFailures) != 2 {
		t.Fatalf("requests = %v failures = %+v, want no request and both red checks named",
			s.gateway.rerunRequests(), summary.CIFailures)
	}
}

// The budget is per attempt: a repair round's new red head gets what the
// earlier head left, and nothing once it is spent.
func TestTheRerunBudgetCarriesAcrossRepairRounds(t *testing.T) {
	t.Run("what is left applies to the new head", func(t *testing.T) {
		s, ci := newRerunScenario(t, 2, 1, func(head, reruns, poll int) []CICheck {
			if head == 0 {
				return brokenTest(head, reruns, poll)
			}
			if reruns <= 1 {
				return []CICheck{actionsJob("lint", CIPass, 1), actionsJob("test", CIFail, 111)}
			}
			return []CICheck{actionsJob("lint", CIPass, 1), actionsJob("test", CIPass, 112)}
		}, fixRound())
		s.gateway.rerun = func(_ int64, call int) error {
			if call == 1 {
				return publishFailure("gh_failed", "refused")
			}
			return nil
		}
		attempt, summary := s.run(t)
		if attempt.State != protocol.AttemptAccepted || !summary.CIFlaky {
			t.Fatalf("attempt = %s summary = %+v, want accepted, flaky on the fix", attempt.State, summary)
		}
		assertReruns(t, summary,
			CIRerunSummary{Attempt: 1, Head: ci.heads[0], Outcome: "ci_rerun_refused"},
			CIRerunSummary{Attempt: 2, Head: ci.heads[1], Outcome: "passed"})
		if requests := s.gateway.rerunRequests(); !slices.Equal(requests, []int64{11, 111}) {
			t.Fatalf("re-run requests = %v, want one per head", requests)
		}
	})
	t.Run("a spent budget does not", func(t *testing.T) {
		s, _ := newRerunScenario(t, 1, 1, brokenTest, fixRound())
		attempt, summary := s.run(t)
		if attempt.State != protocol.AttemptAcceptedUnpublished || summary.Code != "ci_repair_exhausted" {
			t.Fatalf("attempt = %s summary = %+v, want ci_repair_exhausted", attempt.State, summary)
		}
		assertReruns(t, summary, CIRerunSummary{Attempt: 1, Outcome: "failed"})
		if requests := s.gateway.rerunRequests(); len(requests) != 1 || len(summary.CIRepairs) != 1 {
			t.Fatalf("requests = %v repairs = %+v, want one re-run and one round", requests, summary.CIRepairs)
		}
	})
}

// A lease lost before a re-run means no request is sent — before each
// request, not only the first.
func TestALostLeaseSendsNoRerun(t *testing.T) {
	for _, tc := range []struct {
		name     string
		loseFrom int // the request count after which the lease is lost
		want     []int64
	}{
		{"before the first", 0, nil},
		{"between two jobs", 1, []int64{11}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w, gateway, target, pushed := publishedWorker(t)
			var lost bool
			// The attempt is already terminal, so a heartbeat is refused:
			// a stale local clock is all it takes to lose the lease.
			target.lease.now = func() time.Time {
				if lost {
					return time.Now().Add(time.Hour)
				}
				return time.Now()
			}
			lost = tc.loseFrom == 0
			gateway.checks = func(string, int) ([]CICheck, error) {
				return []CICheck{actionsJob("test", CIFail, 11), actionsJob("lint", CIFail, 12)}, nil
			}
			gateway.rerun = func(_ int64, call int) error {
				if call >= tc.loseFrom {
					lost = true
				}
				return nil
			}
			summary := PublishSummary{State: PublishStateFailed, Code: "ci_failed", RemoteRef: pushed, CIRef: pushed,
				CIFailures: []CICheck{actionsJob("test", CIFail, 11)}}
			summary = w.rerunFlakyCI(context.Background(), gateway, fastCIOptions(), target, summary,
				ciPolicy{wait: true, timeout: time.Minute, rerunBudget: 1})
			if requests := gateway.rerunRequests(); !slices.Equal(requests, tc.want) {
				t.Fatalf("re-run requests = %v, want %v", requests, tc.want)
			}
			if summary.Published() || summary.Code == "ci_failed" || len(summary.CIReruns) != 1 ||
				summary.CIReruns[0].Outcome != summary.Code {
				t.Fatalf("summary = %+v, want the publish ended on the lease verdict, recorded", summary)
			}
		})
	}
}

// A person pushing while jig waits to re-run ends the re-run: there is
// nothing of jig's left on the branch to re-run.
func TestAPersonsPushWhileSettlingEndsTheRerun(t *testing.T) {
	s, _ := newRerunScenario(t, 1, 1, nil, fixRound())
	var personal string
	s.gateway.checks = func(_ string, poll int) ([]CICheck, error) {
		if poll == 2 {
			personal = pushToBranch(t, s.originDir, s.branch)
		}
		return []CICheck{actionsJob("test", CIFail, 11), actionsJob("slow", CIPending, 21)}, nil
	}
	attempt, summary := s.run(t)
	if attempt.State != protocol.AttemptAcceptedUnpublished || summary.Code != "ci_rerun_head_moved" {
		t.Fatalf("attempt = %s summary = %+v, want ci_rerun_head_moved", attempt.State, summary)
	}
	if len(s.gateway.rerunRequests()) != 0 || len(s.continuation.failures) != 0 ||
		remoteBranches(t, s.originDir)[s.branch] != personal {
		t.Fatal("a re-run or a repair round ran on top of a person's push")
	}
}

// A publish-only retry never re-runs: like repair, it judges CI and nothing
// more.
func TestThePublishRetryNeverReruns(t *testing.T) {
	s, _ := newRerunScenario(t, 1, 0, brokenTest)
	if attempt, _ := s.run(t); attempt.State != protocol.AttemptAcceptedUnpublished {
		t.Fatalf("attempt = %s, want accepted_unpublished", attempt.State)
	}
	retried, err := s.w.RetryPublish(context.Background(), s.job.ID, s.gateway, fastCIOptions())
	if err != nil {
		t.Fatalf("publish retry: %v", err)
	}
	if summary := publishSummaryOf(t, retried.Result); summary.Code != "ci_failed" || len(summary.CIReruns) != 0 ||
		len(s.gateway.rerunRequests()) != 1 {
		t.Fatalf("retry summary = %+v requests = %v, want red judged with no new re-run",
			summary, s.gateway.rerunRequests())
	}
}

// ---- the gh gateway ----------------------------------------------------------

func TestGitHubGatewayRerunsOneActionsJob(t *testing.T) {
	var calls [][]string
	var stdout, stderr []byte
	var runErr error
	gateway := &GitHubCLIGateway{
		LookPath: func(string) (string, error) { return "/usr/bin/gh", nil },
		Run: func(_ context.Context, name string, arguments ...string) ([]byte, []byte, bool, bool, error) {
			calls = append(calls, append([]string{name}, arguments...))
			return stdout, stderr, false, false, runErr
		},
	}
	if err := gateway.RerunActionsJob(context.Background(), "github.com/acme/widgets", 4242); err != nil {
		t.Fatalf("rerun: %v", err)
	}
	want := []string{"gh", "api", "-X", "POST", "-H", "Accept: application/vnd.github+json",
		"repos/acme/widgets/actions/jobs/4242/rerun"}
	if len(calls) != 1 || !slices.Equal(calls[0], want) {
		t.Fatalf("gh calls = %v, want %v", calls, want)
	}

	for _, tc := range []struct {
		name           string
		stdout, stderr string
		code           string
	}{
		{"in progress on stderr", "", "gh: This workflow is already running (HTTP 403)", ciRerunInProgress},
		{"in progress in the body", `{"message":"Cannot rerun a job while the run is in progress"}`,
			"gh: HTTP 403", ciRerunInProgress},
		{"forbidden", `{"message":"Resource not accessible by integration"}`,
			"gh: Resource not accessible by integration (HTTP 403)", "gh_failed"},
		{"unauthenticated", "", "gh: To get started with GitHub CLI, please run:  gh auth login", "gh_unauthenticated"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stdout, stderr, runErr = []byte(tc.stdout), []byte(tc.stderr), errors.New("exit status 1")
			err := gateway.RerunActionsJob(context.Background(), "github.com/acme/widgets", 4242)
			if publishCode(err) != tc.code {
				t.Fatalf("err = %v (code %s), want %s", err, publishCode(err), tc.code)
			}
		})
	}

	stdout, stderr, runErr = nil, nil, nil
	calls = nil
	if err := gateway.RerunActionsJob(context.Background(), "github.com/acme/widgets", 0); publishCode(err) != "ci_rerun_invalid_job" {
		t.Fatalf("err = %v, want ci_rerun_invalid_job", err)
	}
	if err := gateway.RerunActionsJob(context.Background(), "gitlab.com/acme/widgets", 1); publishCode(err) != "publish_unsupported_remote" {
		t.Fatalf("err = %v, want publish_unsupported_remote", err)
	}
	if len(calls) != 0 {
		t.Fatalf("gh was called for an invalid request: %v", calls)
	}
}

// ---- the gated real-gh test --------------------------------------------------

// rerunGateWorkflow fails its flaky job on a workflow run's first attempt
// and passes on any re-run; its slow sibling keeps the run in progress well
// after the flaky job goes red, which is the case the fakes could not see:
// GitHub refuses to re-run a job while its workflow run is still going.
const rerunGateWorkflow = `name: jig-rerun-gate
on:
  push:
    branches: ['jig/**']
jobs:
  flaky:
    runs-on: ubuntu-latest
    steps:
      - run: test "${{ github.run_attempt }}" != "1"
  slow:
    runs-on: ubuntu-latest
    steps:
      - run: sleep 90
`

// TestCIRerunGate re-runs a real Actions job through real gh after its slow
// sibling finishes. It is skipped unless JIG_RERUN_GATE names a scratch
// repository (owner/repo) whose other workflows, if any, pass on a jig/**
// branch. The attempt's change is the gate workflow itself, so gh needs the
// workflow scope; the test opens a pull request and leaves it for the
// operator to close.
func TestCIRerunGate(t *testing.T) {
	project := strings.TrimSpace(os.Getenv("JIG_RERUN_GATE"))
	if project == "" {
		t.Skip("set JIG_RERUN_GATE=owner/repo to run the flaky re-run gate")
	}
	ctx := context.Background()
	identity := "github.com/" + project
	stdout, err := runGit(ctx, "", "ls-remote", "https://github.com/"+project+".git", "HEAD")
	if err != nil {
		t.Fatalf("read %s HEAD: %v", project, err)
	}
	baseSHA, _, found := strings.Cut(strings.TrimSpace(stdout), "\t")
	if !found || len(baseSHA) < 40 {
		t.Fatalf("unreadable HEAD for %s: %q", project, stdout)
	}

	h := newHarness(t)
	h.seedRunWithSnapshot("run-gate", integrationSnapshot+`publish:
  ci:
    wait: true
    timeout: 20m
    rerun: {budget: 1}
`, protocol.RunTarget{Repository: identity, BaseSHA: baseSHA})
	h.enqueue("run-gate", identity)
	options := PublishOptions{CommitAuthorName: "jig-test", CommitAuthorEmail: "jig@test",
		CIPollInterval: 10 * time.Second}
	runner := NewPublishingRunner(writeAndDeclare(t, map[string]string{
		".github/workflows/jig-rerun-gate.yml": rerunGateWorkflow,
	}), NewGitHubCLIGateway(), options)
	w := newTestWorker(t, h, filepath.Join(t.TempDir(), "worker"), 1, runner)
	runner.Bind(w)

	attempt, err := w.ClaimOnce(ctx)
	if err != nil || attempt == nil {
		t.Fatalf("claim: attempt=%v err=%v", attempt, err)
	}
	summary := publishSummaryOf(t, attempt.Result)
	t.Logf("rerun gate: state=%s pull request=%s summary=%+v", attempt.State, summary.PullRequestURL, summary)
	if attempt.State != protocol.AttemptAccepted || !summary.CIFlaky {
		t.Fatalf("attempt = %s summary = %+v, want accepted and flagged flaky", attempt.State, summary)
	}
	if len(summary.CIReruns) != 1 || summary.CIReruns[0].Outcome != "passed" ||
		!slices.Equal(summary.CIReruns[0].Jobs, []string{"flaky"}) {
		t.Fatalf("ci_reruns = %+v, want the flaky job re-run once and passed", summary.CIReruns)
	}
}
