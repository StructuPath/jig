package controlplane

import (
	"context"
	"testing"
	"time"

	"github.com/StructuPath/JIG-Software-Factory/internal/protocol"
)

const mergeFixtureSnapshot = ciFixtureSnapshot + "  merge: {method: squash}\n"

func mergeClaim(t *testing.T, store *Store) *protocol.Claim {
	t.Helper()
	registerTestWorker(t, store, 1)
	if _, err := store.db.Exec("UPDATE workers SET allow_merge = 1"); err != nil {
		t.Fatal(err)
	}
	seedRunSnapshot(t, store, "run-1", mergeFixtureSnapshot, protocol.RunTarget{Repository: repoA, BaseSHA: shaA})
	return claimAndStart(t, store, "run-1", repoA)
}

func mergeProof(t *testing.T, store *Store, claim *protocol.Claim, ciHead string) {
	t.Helper()
	for _, step := range []string{"push", "pull_request", "proof", "ci"} {
		head := shaA
		if step == "ci" {
			head = ciHead
		}
		if _, err := recordStep(store, claim, tokenA, step, head, "https://github.com/example/repo/pull/1"); err != nil {
			t.Fatal(err)
		}
	}
}

func TestMergeFencesRejectMissingConsentHoldForeignHeadAndLease(t *testing.T) {
	for _, scenario := range []struct{ name, sql, code string }{
		{"no definition consent", `UPDATE runs SET snapshot = ?`, "merge_not_declared"},
		{"held", `UPDATE attempts SET publish_hold = 'risk'`, "merge_held"},
		{"worker opted out", `UPDATE workers SET allow_merge = 0`, "merge_not_allowed"},
		{"foreign head", `UPDATE publish_records SET remote_ref = '` + shaB + `' WHERE step = 'ci'`, "merge_head_moved"},
		{"missing CI", `DELETE FROM publish_records WHERE step = 'ci'`, "publish_step_out_of_order"},
		{"expired lease", `UPDATE attempts SET lease_expires_at = 0`, "lease_not_owner"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			store, _ := newTestStore(t)
			claim := mergeClaim(t, store)
			mergeProof(t, store, claim, shaA)
			args := []any{}
			if scenario.name == "no definition consent" {
				args = append(args, ciFixtureSnapshot)
			}
			if _, err := store.db.Exec(scenario.sql, args...); err != nil {
				t.Fatal(err)
			}
			_, authErr := authorizeStep(store, claim, tokenA, "merge")
			_, recordErr := recordStep(store, claim, tokenA, "merge", shaB, "")
			for _, err := range []error{authErr, recordErr} {
				if err == nil || serviceCode(t, err) != scenario.code {
					t.Fatalf("got %v, want %s", err, scenario.code)
				}
			}
		})
	}
}

func TestMergeLedgerRequiresMergeForAcceptanceAndReplays(t *testing.T) {
	store, _ := newTestStore(t)
	claim := mergeClaim(t, store)
	mergeProof(t, store, claim, shaA)
	ctx := context.Background()
	if _, err := store.CompleteAttempt(ctx, claim.Attempt.ID, protocol.CompleteAttemptRequest{LeaseToken: tokenA, State: protocol.AttemptAccepted}); err == nil || serviceCode(t, err) != "publish_merge_required" {
		t.Fatalf("accepted without merge: %v", err)
	}
	auth, err := authorizeStep(store, claim, tokenA, "merge")
	if err != nil || auth.MergeHead != shaA {
		t.Fatalf("authorization: %+v %v", auth, err)
	}
	for range 2 {
		if _, err := recordStep(store, claim, tokenA, "merge", shaB, ""); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := recordStep(store, claim, tokenA, "merge", shaA, ""); err == nil || serviceCode(t, err) != "publish_step_conflict" {
		t.Fatalf("conflict: %v", err)
	}
	auth, err = authorizeStep(store, claim, tokenA, "merge")
	if err != nil || auth.Completed == nil {
		t.Fatalf("replay: %+v %v", auth, err)
	}
	if _, err := store.CompleteAttempt(ctx, claim.Attempt.ID, protocol.CompleteAttemptRequest{LeaseToken: tokenA, State: protocol.AttemptAccepted}); err != nil {
		t.Fatal(err)
	}
	report := mustReport(t, store, store.now().Add(-time.Hour), store.now().Add(time.Hour))
	if report.Accepts.Merged != 1 || report.Accepts.MergedRate == nil || *report.Accepts.MergedRate != 1 {
		t.Fatalf("report: %+v", report.Accepts)
	}
}

func TestMergeClaimRequiresWorkerOptInAndQueueExplainsIt(t *testing.T) {
	store, _ := newTestStore(t)
	registerTestWorker(t, store, 1)
	seedRunSnapshot(t, store, "run-1", mergeFixtureSnapshot, protocol.RunTarget{Repository: repoA, BaseSHA: shaA})
	if _, err := store.EnqueueJob(context.Background(), "run-1", repoA); err != nil {
		t.Fatal(err)
	}
	if got := claimAs(t, store, workerA, "no-merge", tokenA); got != nil {
		t.Fatal("worker without consent claimed merge")
	}
	queue, err := store.Queue(context.Background())
	if err != nil || len(queue.Entries) != 1 || !queue.Entries[0].RequiresMerge {
		t.Fatalf("queue: %+v %v", queue, err)
	}
	if _, err := store.RegisterWorker(context.Background(), workerA, protocol.WorkerRegistration{Name: "local", Capacity: 1, AllowMerge: true, EnvNames: []string{"GITHUB_TOKEN", "HOME", "PATH"}}); err != nil {
		t.Fatal(err)
	}
	if got := claimAs(t, store, workerA, "merge", tokenB); got == nil {
		t.Fatal("merge worker did not claim")
	}
}

func TestHeldMergeDefinitionAcceptsWithoutMerge(t *testing.T) {
	store, _ := newTestStore(t)
	claim := mergeClaim(t, store)
	mergeProof(t, store, claim, shaA)
	ctx := context.Background()
	if _, err := store.CompleteAttempt(ctx, claim.Attempt.ID, protocol.CompleteAttemptRequest{LeaseToken: tokenA, State: protocol.AttemptAcceptedUnpublished, PublishHold: "risk"}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.RetryPublish(ctx, claim.Job.ID, protocol.PublishRetryRequest{WorkerID: workerA, LeaseToken: tokenB}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CompleteAttempt(ctx, claim.Attempt.ID, protocol.CompleteAttemptRequest{LeaseToken: tokenB, State: protocol.AttemptAccepted}); err != nil {
		t.Fatal(err)
	}
}
