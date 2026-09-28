// publish_repair_test.go — the CI repair round loop (plan U5) through the
// real worker, control plane, and git remotes, with a scripted continuation
// standing in for the engine and a fake gh for CI.
package worker

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/StructuPath/jig/internal/protocol"
)

// repairLoopSnapshot waits for CI and repairs it with up to `budget` rounds.
func repairLoopSnapshot(budget int) string {
	return integrationSnapshot + fmt.Sprintf(`  - name: review
    kind: agent
    owner: builder
publish:
  ci:
    wait: true
    on_fail: {run: build, budget: %d}
`, budget)
}

// roundScript is what one scripted repair round does: files to write into
// the worktree, an outcome override, and an action to run first (a person
// pushing mid-round, say).
type roundScript struct {
	files  map[string]string
	before func()
	result func(paths []string) Outcome
}

type scriptedContinuation struct {
	t        *testing.T
	worktree string
	rounds   []roundScript
	failures []CIFailure
	released int
}

func (c *scriptedContinuation) RepairCI(_ context.Context, failure CIFailure) Outcome {
	c.failures = append(c.failures, failure)
	if len(c.failures) > len(c.rounds) {
		c.t.Errorf("repair round %d was not scripted", len(c.failures))
		return Outcome{State: protocol.AttemptFailed, Error: "unscripted round"}
	}
	script := c.rounds[len(c.failures)-1]
	if script.before != nil {
		script.before()
	}
	paths := make([]string, 0, len(script.files))
	for name, body := range script.files {
		if err := os.WriteFile(filepath.Join(c.worktree, name), []byte(body), 0o644); err != nil {
			c.t.Errorf("write %s: %v", name, err)
		}
		paths = append(paths, name)
	}
	if script.result != nil {
		return script.result(paths)
	}
	return Outcome{State: protocol.AttemptAcceptedUnpublished, Result: engineResult(c.t, paths)}
}

func (c *scriptedContinuation) Release() { c.released++ }

// repairRunner is the inner runner: it writes the chain's work, and hands
// back the scripted continuation bound to this attempt's worktree.
func repairRunner(t *testing.T, continuation *scriptedContinuation) AttemptRunner {
	return RunnerFunc(func(_ context.Context, attempt *PreparedAttempt) Outcome {
		continuation.worktree = attempt.WorktreePath
		if err := os.WriteFile(filepath.Join(attempt.WorktreePath, "work.txt"), []byte("work\n"), 0o644); err != nil {
			t.Errorf("write work: %v", err)
		}
		return Outcome{State: protocol.AttemptAcceptedUnpublished,
			Result: engineResult(t, []string{"work.txt"}), Continuation: continuation}
	})
}

// redUntil scripts CI: every head in red fails "test", anything else is
// green. The first head judged is added to red when redFirst is set, so the
// chain's own push is red.
type ciScript struct {
	red    map[string]bool
	judged []string
}

func (c *ciScript) gateway(redPushes int) *fakeGateway {
	gateway := newFakeGateway()
	gateway.checks = func(sha string, _ int) ([]CICheck, error) {
		if len(c.judged) == 0 || c.judged[len(c.judged)-1] != sha {
			c.judged = append(c.judged, sha)
			if len(c.judged) <= redPushes {
				c.red[sha] = true
			}
		}
		if c.red[sha] {
			return []CICheck{check("lint", CIPass), check("test", CIFail)}, nil
		}
		return []CICheck{check("lint", CIPass), check("test", CIPass)}, nil
	}
	return gateway
}

type repairScenario struct {
	h            *harness
	originDir    string
	job          protocol.Job
	branch       string
	w            *Worker
	gateway      *fakeGateway
	ci           *ciScript
	continuation *scriptedContinuation
}

func newRepairScenario(t *testing.T, budget, redPushes int, rounds ...roundScript) *repairScenario {
	t.Helper()
	s := &repairScenario{h: newHarness(t), ci: &ciScript{red: map[string]bool{}}}
	var head, identity string
	s.originDir, head, identity = newOriginRepo(t)
	s.h.seedRunWithSnapshot("run-1", repairLoopSnapshot(budget), protocol.RunTarget{Repository: identity, BaseSHA: head})
	s.job = s.h.enqueue("run-1", identity)
	s.branch = protocol.PublishBranch(s.job.ID, 1)
	s.continuation = &scriptedContinuation{t: t, rounds: rounds}
	s.gateway = s.ci.gateway(redPushes)
	s.w = newCIWorker(t, s.h, repairRunner(t, s.continuation), s.gateway)
	return s
}

func (s *repairScenario) run(t *testing.T) (*protocol.Attempt, PublishSummary) {
	t.Helper()
	attempt, err := s.w.ClaimOnce(context.Background())
	if err != nil || attempt == nil {
		t.Fatalf("claim: attempt=%v err=%v", attempt, err)
	}
	return attempt, publishSummaryOf(t, attempt.Result)
}

func (s *repairScenario) rounds(t *testing.T, attemptID string) []protocol.CIRepairRecord {
	t.Helper()
	rounds, err := s.w.client.AttemptCIRepairs(context.Background(), attemptID)
	if err != nil {
		t.Fatalf("read rounds: %v", err)
	}
	return rounds
}

func fixRound() roundScript { return roundScript{files: map[string]string{"fix.txt": "fixed\n"}} }

// Red, one round, green: the attempt is accepted, the round is on the ledger
// from the proof head to the fix, and CI was recorded green on the fix.
func TestRedCIIsRepairedInOneRoundAndTheAttemptIsAccepted(t *testing.T) {
	s := newRepairScenario(t, 2, 1, fixRound())
	attempt, summary := s.run(t)
	if attempt.State != protocol.AttemptAccepted || !summary.Published() {
		t.Fatalf("attempt = %s summary = %+v, want accepted and published", attempt.State, summary)
	}
	fixed := remoteBranches(t, s.originDir)[s.branch]
	proof := s.ci.judged[0]
	rounds := s.rounds(t, attempt.ID)
	if len(rounds) != 1 || rounds[0].HeadBefore != proof || rounds[0].HeadAfter != fixed ||
		len(rounds[0].FailedChecks) != 1 || rounds[0].FailedChecks[0] != "test" {
		t.Fatalf("rounds = %+v, want one from the proof head %s to the fix %s failing [test]", rounds, proof, fixed)
	}
	if summary.CIRef != fixed || summary.RemoteRef != fixed || len(summary.CIRepairs) != 1 ||
		summary.CIRepairs[0].Outcome != "pushed" {
		t.Fatalf("summary = %+v, want green on the fix, remote_ref at the fix, one pushed round", summary)
	}
	if paths := summary.CIRepairs[0].ChangedPaths; len(paths) != 1 || paths[0] != "fix.txt" ||
		!strings.Contains(string(summary.CIRepairs[0].Acceptance), `"passed":true`) {
		t.Fatalf("round evidence = %+v, want its changed paths and acceptance", summary.CIRepairs[0])
	}
	if len(s.continuation.failures) != 1 || s.continuation.failures[0].Head != proof ||
		s.continuation.failures[0].Checks[0].Name != "test" {
		t.Fatalf("the round was handed %+v, want the red head and the red check", s.continuation.failures)
	}
	if steps := recordedSteps(t, s.w, attempt.ID); !strings.Contains(strings.Join(steps, ","), "ci") {
		t.Fatalf("ledger steps = %v, want ci recorded", steps)
	}
	if created, _ := s.gateway.counts(); created != 1 {
		t.Fatalf("pull requests created = %d, want the one pull request, fixed in place", created)
	}
}

// Red after every round until the budget runs out: accepted_unpublished,
// every round on the ledger, the pull request at the last fix.
func TestRedCIPastTheBudgetEndsAcceptedUnpublished(t *testing.T) {
	s := newRepairScenario(t, 1, 2, fixRound())
	attempt, summary := s.run(t)
	if attempt.State != protocol.AttemptAcceptedUnpublished || summary.Code != "ci_repair_exhausted" {
		t.Fatalf("attempt = %s summary = %+v, want accepted_unpublished on ci_repair_exhausted", attempt.State, summary)
	}
	rounds := s.rounds(t, attempt.ID)
	fixed := remoteBranches(t, s.originDir)[s.branch]
	if len(rounds) != 1 || rounds[0].HeadAfter != fixed || summary.CIRef != fixed {
		t.Fatalf("rounds = %+v ci_ref = %s, want one round and red CI judged on its fix %s", rounds, summary.CIRef, fixed)
	}
	if !strings.Contains(summary.Detail, "1 repair round") {
		t.Fatalf("detail = %q, want the budget named", summary.Detail)
	}
}

// Someone else's push before the first round means no round at all (KTD7).
func TestAPersonsPushBeforeARoundMeansNoRound(t *testing.T) {
	s := newRepairScenario(t, 2, 2)
	var personal string
	checks := s.gateway.checks
	s.gateway.checks = func(sha string, poll int) ([]CICheck, error) {
		if poll == 1 {
			personal = pushToBranch(t, s.originDir, s.branch)
			return []CICheck{check("test", CIPending)}, nil
		}
		return checks(sha, poll)
	}
	attempt, summary := s.run(t)
	if attempt.State != protocol.AttemptAcceptedUnpublished || summary.Code != "ci_repair_head_moved" {
		t.Fatalf("attempt = %s summary = %+v, want ci_repair_head_moved", attempt.State, summary)
	}
	if len(s.continuation.failures) != 0 || len(s.rounds(t, attempt.ID)) != 0 {
		t.Fatal("a round ran on top of a person's push")
	}
	if remoteBranches(t, s.originDir)[s.branch] != personal {
		t.Fatal("the person's push was overwritten")
	}
}

// A person pushing while a round runs makes jig's non-force push fail: the
// round ends without a record, and the person's commit stands.
func TestAPushRejectedMidRoundEndsCleanly(t *testing.T) {
	var personal string
	s := newRepairScenario(t, 2, 1)
	s.continuation.rounds = []roundScript{{files: map[string]string{"fix.txt": "fixed\n"},
		before: func() { personal = pushToBranch(t, s.originDir, s.branch) }}}
	attempt, summary := s.run(t)
	if attempt.State != protocol.AttemptAcceptedUnpublished || summary.Code != "publish_push_failed" {
		t.Fatalf("attempt = %s summary = %+v, want publish_push_failed", attempt.State, summary)
	}
	if len(s.rounds(t, attempt.ID)) != 0 || remoteBranches(t, s.originDir)[s.branch] != personal {
		t.Fatal("a rejected push was recorded, or the person's commit was overwritten")
	}
	if len(summary.CIRepairs) != 1 || summary.CIRepairs[0].Outcome != "publish_push_failed" {
		t.Fatalf("ci_repairs = %+v, want the round that stopped on the push", summary.CIRepairs)
	}
}

// A round that ends held, failed, cancelled, or with no change pushes
// nothing: the pull request stays at the head CI found red.
func TestARoundThatDoesNotEndAcceptedPushesNothing(t *testing.T) {
	for _, tc := range []struct {
		name, code string
		result     func(paths []string) Outcome
	}{
		{"held", "ci_repair_held", func([]string) Outcome {
			return Outcome{State: protocol.AttemptAcceptedUnpublished, PublishHold: "risk = high"}
		}},
		{"failed", "ci_repair_round_failed", func([]string) Outcome {
			return Outcome{State: protocol.AttemptFailed, Error: "acceptance predicate failed"}
		}},
		{"cancelled", "ci_repair_cancelled", func([]string) Outcome {
			return Outcome{State: protocol.AttemptCancelled, Error: "cancelled between phases"}
		}},
		{"no change", "ci_repair_no_change", func([]string) Outcome {
			return Outcome{State: protocol.AttemptAcceptedUnpublished,
				Error: "ci_repair_no_change: the CI repair round changed no files"}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newRepairScenario(t, 2, 1, roundScript{files: map[string]string{"fix.txt": "x\n"}, result: tc.result})
			attempt, summary := s.run(t)
			if attempt.State != protocol.AttemptAcceptedUnpublished || summary.Code != tc.code {
				t.Fatalf("attempt = %s summary = %+v, want %s", attempt.State, summary, tc.code)
			}
			if remoteBranches(t, s.originDir)[s.branch] != summary.RemoteRef || len(s.rounds(t, attempt.ID)) != 0 {
				t.Fatal("a round that did not end accepted moved the branch or was recorded")
			}
		})
	}
}

// Without a continuation (the engine could not keep the chain alive), red
// CI ends the attempt exactly as it did before repair existed.
func TestRedCIWithoutAContinuationEndsAsItAlwaysHas(t *testing.T) {
	s := newRepairScenario(t, 2, 1)
	s.w.config.Runner.(*PublishingRunner).inner = writeAndDeclare(t, map[string]string{"work.txt": "work\n"})
	attempt, summary := s.run(t)
	if attempt.State != protocol.AttemptAcceptedUnpublished || summary.Code != "ci_failed" || len(summary.CIRepairs) != 0 {
		t.Fatalf("attempt = %s summary = %+v, want plain ci_failed", attempt.State, summary)
	}
}

// The publish-only retry judges CI and never repairs (R11), even after the
// rounds ran out.
func TestThePublishRetryNeverRepairs(t *testing.T) {
	s := newRepairScenario(t, 1, 3, fixRound())
	attempt, _ := s.run(t)
	if attempt.State != protocol.AttemptAcceptedUnpublished {
		t.Fatalf("attempt = %s, want accepted_unpublished", attempt.State)
	}
	retried, err := s.w.RetryPublish(context.Background(), s.job.ID, s.gateway, fastCIOptions())
	if err != nil {
		t.Fatalf("publish retry: %v", err)
	}
	summary := publishSummaryOf(t, retried.Result)
	if retried.State != protocol.AttemptAcceptedUnpublished || summary.Code != "ci_failed" ||
		len(summary.CIRepairs) != 0 || len(s.continuation.failures) != 1 {
		t.Fatalf("retry = %s summary = %+v rounds run = %d, want red judged and no new round",
			retried.State, summary, len(s.continuation.failures))
	}
}

// Two rounds chain: round 2 repairs round 1's fix, which the worker learns
// from the ledger, and the second fix goes green.
func TestTwoRoundsChainOnTheLedgerUntilGreen(t *testing.T) {
	s := newRepairScenario(t, 2, 2, fixRound(), roundScript{files: map[string]string{"fix2.txt": "better\n"}})
	attempt, summary := s.run(t)
	if attempt.State != protocol.AttemptAccepted {
		t.Fatalf("attempt = %s summary = %+v, want accepted after two rounds", attempt.State, summary)
	}
	rounds := s.rounds(t, attempt.ID)
	if len(rounds) != 2 || rounds[1].HeadBefore != rounds[0].HeadAfter ||
		rounds[1].HeadAfter != remoteBranches(t, s.originDir)[s.branch] {
		t.Fatalf("rounds = %+v, want round 2 to repair round 1's fix and end at the remote head", rounds)
	}
	if len(s.continuation.failures) != 2 || s.continuation.failures[1].Head != rounds[0].HeadAfter {
		t.Fatalf("round 2 was handed %+v, want round 1's red fix", s.continuation.failures)
	}
}

// A continuation under a definition that declares no repair is never used:
// red CI ends as it always has.
func TestAContinuationWithoutOnFailIsNeverUsed(t *testing.T) {
	s := newRepairScenario(t, 1, 1, fixRound())
	if _, err := s.h.db.Exec(`UPDATE runs SET snapshot = ? WHERE id = 'run-1'`, ciSnapshot); err != nil {
		t.Fatal(err)
	}
	attempt, summary := s.run(t)
	if attempt.State != protocol.AttemptAcceptedUnpublished || summary.Code != "ci_failed" || len(s.continuation.failures) != 0 {
		t.Fatalf("attempt = %s summary = %+v rounds = %d, want plain ci_failed and no round",
			attempt.State, summary, len(s.continuation.failures))
	}
}

// A round whose lease is lost while it runs (the job swept, a successor
// leased) never pushes: the fence runs again right before the push.
func TestARoundThatLosesItsLeaseNeverPushes(t *testing.T) {
	s := newRepairScenario(t, 2, 1)
	s.continuation.rounds = []roundScript{{files: map[string]string{"fix.txt": "fixed\n"}, before: func() {
		if _, err := s.h.db.Exec(`UPDATE attempts SET lease_digest = x'00'`); err != nil {
			t.Errorf("supersede the lease: %v", err)
		}
	}}}
	attempt, err := s.w.ClaimOnce(context.Background())
	if err == nil && attempt != nil && attempt.State == protocol.AttemptAccepted {
		t.Fatalf("attempt = %+v, a zombie was accepted", attempt)
	}
	if head := remoteBranches(t, s.originDir)[s.branch]; head != s.ci.judged[0] {
		t.Fatalf("remote head = %s, want the red head %s: a zombie round pushed", head, s.ci.judged[0])
	}
}

// A round that commits a path it never changed (a definition's commit
// phase sweeping in the chain's test output) is refused before the push.
func TestARoundCommittingUndeclaredPathsIsNotPushed(t *testing.T) {
	s := newRepairScenario(t, 2, 1)
	s.continuation.rounds = []roundScript{{files: map[string]string{"fix.txt": "fixed\n"}, before: func() {
		worktree := s.continuation.worktree
		if err := os.WriteFile(filepath.Join(worktree, "coverage.out"), []byte("mode: set\n"), 0o644); err != nil {
			t.Errorf("write artifact: %v", err)
		}
		gitRun(t, worktree, "add", "coverage.out")
		gitRun(t, worktree, "-c", "user.name=jig", "-c", "user.email=jig@test", "commit", "-q", "-m", "sweep")
	}}}
	attempt, summary := s.run(t)
	if attempt.State != protocol.AttemptAcceptedUnpublished || summary.Code != "publish_staging_escape" ||
		!strings.Contains(summary.Detail, "coverage.out") {
		t.Fatalf("attempt = %s summary = %+v, want publish_staging_escape naming coverage.out", attempt.State, summary)
	}
	if remoteBranches(t, s.originDir)[s.branch] != s.ci.judged[0] || len(s.rounds(t, attempt.ID)) != 0 {
		t.Fatal("a round with an undeclared committed path was pushed or recorded")
	}
}
