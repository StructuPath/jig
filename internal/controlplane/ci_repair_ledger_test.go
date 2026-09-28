// ci_repair_ledger_test.go — the control-plane half of CI repair (U2): the
// round fence, the chain from proof through every pushed fix, the frozen
// budget, and the rule that `accepted` never rests on a head a round found
// red.
package controlplane

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/StructuPath/jig/internal/protocol"
)

const (
	shaC = "3333333333333333333333333333333333333333"
	shaD = "4444444444444444444444444444444444444444"
	shaE = "5555555555555555555555555555555555555555"
)

// repairFixtureSnapshot waits for CI and repairs it: two rounds, the builder
// fixing and the reviewer after it judging.
const repairFixtureSnapshot = `name: fixture
roster:
  builder:
    model: claude-sonnet
    system_prompt: build
    user_prompt: build it
  reviewer:
    model: claude-sonnet
    system_prompt: review
    user_prompt: review it
phases:
  - name: build
    kind: agent
    owner: builder
  - name: review
    kind: agent
    owner: reviewer
publish:
  ci:
    wait: true
    on_fail: {run: build, budget: 2}
`

func useSnapshot(t *testing.T, store *Store, runID, snapshot string) {
	t.Helper()
	if _, err := store.db.Exec(`UPDATE runs SET snapshot = ? WHERE id = ?`, snapshot, runID); err != nil {
		t.Fatalf("set run %s snapshot: %v", runID, err)
	}
}

// claimRepairable seeds a run under the given snapshot and claims it.
func claimRepairable(t *testing.T, snapshot string) (*Store, *testClock, *protocol.Claim) {
	t.Helper()
	store, clock := newTestStore(t)
	registerTestWorker(t, store, 1)
	seedRun(t, store, "run-1", protocol.RunTarget{Repository: repoA, BaseSHA: shaA})
	useSnapshot(t, store, "run-1", snapshot)
	return store, clock, claimAndStart(t, store, "run-1", repoA)
}

// publishThroughProof records push, pull request, and proof on proofRef.
func publishThroughProof(t *testing.T, store *Store, claim *protocol.Claim, proofRef string) {
	t.Helper()
	for _, step := range []struct{ name, ref, url string }{
		{protocol.PublishStepPush, proofRef, ""},
		{protocol.PublishStepPullRequest, "", "https://github.com/example/repo-a/pull/7"},
		{protocol.PublishStepProof, proofRef, ""},
	} {
		if _, err := recordStep(store, claim, tokenA, step.name, step.ref, step.url); err != nil {
			t.Fatalf("record %s: %v", step.name, err)
		}
	}
}

func authorizeRound(store *Store, claim *protocol.Claim, token string, round int, before string) (protocol.CIRepairAuthorization, error) {
	return store.AuthorizeCIRepair(context.Background(), claim.Attempt.ID, protocol.CIRepairAuthorizationRequest{
		LeaseToken: token, Round: round, Branch: publishBranchOf(claim), HeadBefore: before,
	})
}

func recordRound(store *Store, claim *protocol.Claim, token string, round int, before, after string, checks ...string) (protocol.CIRepairRecord, error) {
	return store.RecordCIRepair(context.Background(), claim.Attempt.ID, protocol.CIRepairRecordRequest{
		LeaseToken: token, Round: round, Branch: publishBranchOf(claim),
		HeadBefore: before, HeadAfter: after, FailedChecks: checks,
	})
}

func wantCode(t *testing.T, err error, code, what string) {
	t.Helper()
	if err == nil || serviceCode(t, err) != code {
		t.Fatalf("%s: err=%v, want %s", what, err, code)
	}
}

// Rounds chain from the proof ref through every pushed fix, in order, within
// the budget the run froze — and each is authorized before its push and
// idempotent after it, exactly like a publish step.
func TestCIRepairRoundsChainFromProofWithinTheFrozenBudget(t *testing.T) {
	store, _, claim := claimRepairable(t, repairFixtureSnapshot)

	_, err := authorizeRound(store, claim, tokenA, 1, shaB)
	wantCode(t, err, "publish_step_out_of_order", "round before proof")

	publishThroughProof(t, store, claim, shaB)

	_, err = authorizeRound(store, claim, tokenA, 2, shaB)
	wantCode(t, err, "ci_repair_out_of_order", "round 2 before round 1")
	_, err = authorizeRound(store, claim, tokenA, 1, shaA)
	wantCode(t, err, "ci_repair_head_mismatch", "round 1 over a head jig never pushed")

	granted, err := authorizeRound(store, claim, tokenA, 1, shaB)
	if err != nil {
		t.Fatalf("authorize round 1: %v", err)
	}
	if granted.Budget != 2 || granted.Completed != nil || granted.Branch != publishBranchOf(claim) {
		t.Fatalf("authorization = %+v, want budget 2, not completed, the attempt's branch", granted)
	}
	first, err := recordRound(store, claim, tokenA, 1, shaB, shaC, "lint", "test (ubuntu)")
	if err != nil {
		t.Fatalf("record round 1: %v", err)
	}
	if first.Round != 1 || first.HeadBefore != shaB || first.HeadAfter != shaC ||
		len(first.FailedChecks) != 2 || first.FailedChecks[1] != "test (ubuntu)" {
		t.Fatalf("round 1 = %+v", first)
	}

	if replay, err := recordRound(store, claim, tokenA, 1, shaB, shaC, "lint", "test (ubuntu)"); err != nil ||
		replay.HeadAfter != shaC {
		t.Fatalf("identical replay: record=%+v err=%v, want the stored round", replay, err)
	}
	if again, err := authorizeRound(store, claim, tokenA, 1, shaB); err != nil ||
		again.Completed == nil || again.Completed.HeadAfter != shaC {
		t.Fatalf("re-authorize a recorded round: %+v err=%v, want Completed", again, err)
	}
	_, err = recordRound(store, claim, tokenA, 1, shaB, shaD, "lint", "test (ubuntu)")
	wantCode(t, err, "ci_repair_conflict", "round 1 replayed with a different head")
	_, err = recordRound(store, claim, tokenA, 1, shaB, shaC, "lint")
	wantCode(t, err, "ci_repair_conflict", "round 1 replayed with different checks")

	_, err = authorizeRound(store, claim, tokenA, 2, shaB)
	wantCode(t, err, "ci_repair_head_mismatch", "round 2 over round 1's red head")
	if _, err := authorizeRound(store, claim, tokenA, 2, shaC); err != nil {
		t.Fatalf("authorize round 2: %v", err)
	}
	if _, err := recordRound(store, claim, tokenA, 2, shaC, shaD, "lint"); err != nil {
		t.Fatalf("record round 2: %v", err)
	}

	_, err = authorizeRound(store, claim, tokenA, 3, shaD)
	wantCode(t, err, "ci_repair_budget_exhausted", "round 3 on a budget of 2")
	_, err = recordRound(store, claim, tokenA, 3, shaD, shaE)
	wantCode(t, err, "ci_repair_budget_exhausted", "recording round 3 without authorization")

	rounds, err := store.AttemptCIRepairs(context.Background(), claim.Attempt.ID)
	if err != nil {
		t.Fatalf("list rounds: %v", err)
	}
	if len(rounds) != 2 || rounds[0].HeadAfter != shaC || rounds[1].HeadBefore != shaC ||
		rounds[1].HeadAfter != shaD || len(rounds[1].FailedChecks) != 1 {
		t.Fatalf("rounds = %+v, want the two chained rounds", rounds)
	}
}

// Only a definition that declares on_fail has rounds; one that merely waits
// for CI, or does not wait at all, is refused before anything is pushed.
func TestCIRepairIsRefusedWhenTheDefinitionDeclaresNoRepair(t *testing.T) {
	for name, snapshot := range map[string]string{
		"waits without on_fail": ciFixtureSnapshot,
		"does not wait":         fixtureSnapshot,
	} {
		t.Run(name, func(t *testing.T) {
			store, _, claim := claimRepairable(t, snapshot)
			publishThroughProof(t, store, claim, shaB)
			_, err := authorizeRound(store, claim, tokenA, 1, shaB)
			wantCode(t, err, "ci_repair_not_declared", "authorize")
			_, err = recordRound(store, claim, tokenA, 1, shaB, shaC)
			wantCode(t, err, "ci_repair_not_declared", "record")
		})
	}
}

// A round is a fenced write: a foreign token or an expired lease is refused
// at both halves, and nothing is recorded.
func TestCIRepairRoundsValidateTheLeaseToken(t *testing.T) {
	store, clock, claim := claimRepairable(t, repairFixtureSnapshot)
	publishThroughProof(t, store, claim, shaB)

	_, err := authorizeRound(store, claim, tokenB, 1, shaB)
	wantCode(t, err, "lease_not_owner", "authorize with a foreign token")
	_, err = recordRound(store, claim, tokenB, 1, shaB, shaC)
	wantCode(t, err, "lease_not_owner", "record with a foreign token")

	clock.Advance(protocol.LeaseDuration + time.Second)
	_, err = authorizeRound(store, claim, tokenA, 1, shaB)
	wantCode(t, err, "lease_not_owner", "authorize on an expired lease")
	_, err = recordRound(store, claim, tokenA, 1, shaB, shaC)
	wantCode(t, err, "lease_not_owner", "record on an expired lease")

	rounds, err := store.AttemptCIRepairs(context.Background(), claim.Attempt.ID)
	if err != nil || len(rounds) != 0 {
		t.Fatalf("rounds = %+v err=%v, want none from a fenced-out attempt", rounds, err)
	}
}

// Once CI is recorded green there is nothing to repair, which is also what
// guarantees a ci record always postdates every round.
func TestCIRepairIsRefusedOnceCIIsGreen(t *testing.T) {
	store, _, claim := claimRepairable(t, repairFixtureSnapshot)
	publishThroughProof(t, store, claim, shaB)
	if _, err := recordStep(store, claim, tokenA, protocol.PublishStepCI, shaB, ""); err != nil {
		t.Fatalf("record ci: %v", err)
	}
	_, err := authorizeRound(store, claim, tokenA, 1, shaB)
	wantCode(t, err, "ci_repair_after_green", "authorize after green")
	_, err = recordRound(store, claim, tokenA, 1, shaB, shaC)
	wantCode(t, err, "ci_repair_after_green", "record after green")
}

// Malformed rounds are refused before any transaction.
func TestCIRepairRecordsAreValidated(t *testing.T) {
	store, _, claim := claimRepairable(t, repairFixtureSnapshot)
	publishThroughProof(t, store, claim, shaB)

	_, err := recordRound(store, claim, tokenA, 1, shaB, shaB)
	wantCode(t, err, "invalid_ci_repair_head", "a round that pushes no new head")
	_, err = recordRound(store, claim, tokenA, 1, shaB, "fixed")
	wantCode(t, err, "invalid_publish_ref", "a non-SHA head_after")
	_, err = authorizeRound(store, claim, tokenA, 1, "main")
	wantCode(t, err, "invalid_publish_ref", "a non-SHA head_before")
	_, err = authorizeRound(store, claim, tokenA, 0, shaB)
	wantCode(t, err, "invalid_ci_repair_round", "round 0")

	tooMany := make([]string, protocol.MaxCIRepairFailedChecks+1)
	for i := range tooMany {
		tooMany[i] = fmt.Sprintf("check-%d", i)
	}
	_, err = recordRound(store, claim, tokenA, 1, shaB, shaC, tooMany...)
	wantCode(t, err, "invalid_ci_repair_checks", "too many failed checks")
	_, err = recordRound(store, claim, tokenA, 1, shaB, shaC, string(make([]byte, protocol.MaxCIRepairCheckNameBytes+1)))
	wantCode(t, err, "invalid_ci_repair_checks", "an oversized check name")

	_, err = store.AuthorizeCIRepair(context.Background(), claim.Attempt.ID, protocol.CIRepairAuthorizationRequest{
		LeaseToken: tokenA, Round: 1, Branch: "jig/someone-else/1", HeadBefore: shaB,
	})
	wantCode(t, err, "publish_branch_mismatch", "another attempt's branch")
}

// `accepted` after rounds needs a green head no round found red (R6). It
// need not be the last pushed head: a person's later fix is acceptable.
func TestAcceptedAfterRepairRoundsRefusesAGreenRunOnARepairedHead(t *testing.T) {
	for _, tc := range []struct {
		name, greenHead, wantCode string
	}{
		{"green on the head round 1 repaired", shaB, "publish_ci_on_repaired_head"},
		{"green on the pushed fix", shaC, ""},
		{"green on a person's later fix", shaE, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store, _, claim := claimRepairable(t, repairFixtureSnapshot)
			publishThroughProof(t, store, claim, shaB)
			if _, err := recordRound(store, claim, tokenA, 1, shaB, shaC, "lint"); err != nil {
				t.Fatalf("record round 1: %v", err)
			}
			if _, err := recordStep(store, claim, tokenA, protocol.PublishStepCI, tc.greenHead, ""); err != nil {
				t.Fatalf("record ci: %v", err)
			}
			attempt, err := store.CompleteAttempt(context.Background(), claim.Attempt.ID,
				protocol.CompleteAttemptRequest{LeaseToken: tokenA, State: protocol.AttemptAccepted})
			if tc.wantCode != "" {
				wantCode(t, err, tc.wantCode, "complete accepted")
				return
			}
			if err != nil || attempt.State != protocol.AttemptAccepted {
				t.Fatalf("complete accepted: attempt=%+v err=%v", attempt, err)
			}
		})
	}
}

// Without rounds the CI gate is unchanged: green on the proof head accepts.
func TestAcceptedWithoutRepairRoundsIsUnchanged(t *testing.T) {
	store, _, claim := claimRepairable(t, repairFixtureSnapshot)
	publishThroughProof(t, store, claim, shaB)
	if _, err := recordStep(store, claim, tokenA, protocol.PublishStepCI, shaB, ""); err != nil {
		t.Fatalf("record ci: %v", err)
	}
	if _, err := store.CompleteAttempt(context.Background(), claim.Attempt.ID,
		protocol.CompleteAttemptRequest{LeaseToken: tokenA, State: protocol.AttemptAccepted}); err != nil {
		t.Fatalf("complete accepted with no rounds: %v", err)
	}
}
