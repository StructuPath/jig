// publish_ledger_test.go — the control-plane half of U7: the fence on every
// publish step (R6), the attempt-scoped branch invariant and step ordering
// (R14), and the publish-only retry that re-leases the SAME attempt so a
// re-entry can never produce a second branch.
package controlplane

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/StructuPath/JIG-Software-Factory/internal/protocol"
)

const (
	shaA = "1111111111111111111111111111111111111111"
	shaB = "2222222222222222222222222222222222222222"
)

func publishBranchOf(claim *protocol.Claim) string {
	return protocol.PublishBranch(claim.Job.ID, claim.Attempt.AttemptNumber)
}

// recordStep is the standard fenced record call.
func recordStep(store *Store, claim *protocol.Claim, token, step, ref, url string) (protocol.PublishRecord, error) {
	return store.RecordPublishStep(context.Background(), claim.Attempt.ID, protocol.PublishStepRequest{
		LeaseToken: token, Step: step, Branch: publishBranchOf(claim),
		RemoteRef: ref, PullRequestURL: url,
	})
}

func authorizeStep(store *Store, claim *protocol.Claim, token, step string) (protocol.PublishAuthorization, error) {
	return store.AuthorizePublishStep(context.Background(), claim.Attempt.ID, protocol.PublishAuthorizationRequest{
		LeaseToken: token, Step: step, Branch: publishBranchOf(claim),
	})
}

// A publish step is a fenced write like any other attempt-state write (R6):
// the token must own an attempt that is still leased and whose lease has not
// expired. This is the pre-effect half of the fence — the whole point of
// authorizing before pushing is that a zombie is refused before it touches
// the remote, not after.
func TestEveryPublishStepValidatesTheLeaseToken(t *testing.T) {
	store, clock := newTestStore(t)
	registerTestWorker(t, store, 1)
	seedRun(t, store, "run-1", protocol.RunTarget{Repository: repoA, BaseSHA: shaA})
	claim := claimAndStart(t, store, "run-1", repoA)

	if _, err := authorizeStep(store, claim, tokenB, protocol.PublishStepPush); err == nil ||
		serviceCode(t, err) != "lease_not_owner" {
		t.Fatalf("authorize with a foreign token: err=%v, want lease_not_owner", err)
	}
	if _, err := recordStep(store, claim, tokenB, protocol.PublishStepPush, shaA, ""); err == nil ||
		serviceCode(t, err) != "lease_not_owner" {
		t.Fatalf("record with a foreign token: err=%v, want lease_not_owner", err)
	}

	// The lease expires: this attempt is now a zombie, and both halves of the
	// fence must refuse it.
	clock.Advance(protocol.LeaseDuration + time.Second)
	if _, err := authorizeStep(store, claim, tokenA, protocol.PublishStepPush); err == nil ||
		serviceCode(t, err) != "lease_not_owner" {
		t.Fatalf("authorize on an expired lease: err=%v, want lease_not_owner", err)
	}
	if _, err := recordStep(store, claim, tokenA, protocol.PublishStepPush, shaA, ""); err == nil ||
		serviceCode(t, err) != "lease_not_owner" {
		t.Fatalf("record on an expired lease: err=%v, want lease_not_owner", err)
	}
	records, err := store.AttemptPublishRecords(context.Background(), claim.Attempt.ID)
	if err != nil {
		t.Fatalf("read publish records: %v", err)
	}
	if len(records) != 0 {
		t.Fatalf("a fenced-out attempt recorded %d publish steps: %+v", len(records), records)
	}
}

// The branch is computed control-plane-side from the job id and attempt
// number, so an attempt can only ever publish its own branch (R14). A worker
// asking to record someone else's branch — a successor's, a hand-crafted
// one — is refused rather than believed.
func TestAPublishStepMayOnlyNameItsOwnAttemptScopedBranch(t *testing.T) {
	store, _ := newTestStore(t)
	registerTestWorker(t, store, 1)
	seedRun(t, store, "run-1", protocol.RunTarget{Repository: repoA, BaseSHA: shaA})
	claim := claimAndStart(t, store, "run-1", repoA)

	foreign := protocol.PublishBranch(claim.Job.ID, claim.Attempt.AttemptNumber+1)
	_, err := store.AuthorizePublishStep(context.Background(), claim.Attempt.ID,
		protocol.PublishAuthorizationRequest{
			LeaseToken: tokenA, Step: protocol.PublishStepPush, Branch: foreign,
		})
	if err == nil || serviceCode(t, err) != "publish_branch_mismatch" {
		t.Fatalf("authorize for a successor's branch: err=%v, want publish_branch_mismatch", err)
	}
	_, err = store.RecordPublishStep(context.Background(), claim.Attempt.ID,
		protocol.PublishStepRequest{
			LeaseToken: tokenA, Step: protocol.PublishStepPush, Branch: "main", RemoteRef: shaA,
		})
	if err == nil || serviceCode(t, err) != "publish_branch_mismatch" {
		t.Fatalf("record against main: err=%v, want publish_branch_mismatch", err)
	}

	// An authorization that names no branch is answered with the one branch
	// this attempt is allowed to write.
	authorization, err := store.AuthorizePublishStep(context.Background(), claim.Attempt.ID,
		protocol.PublishAuthorizationRequest{LeaseToken: tokenA, Step: protocol.PublishStepPush})
	if err != nil {
		t.Fatalf("authorize: %v", err)
	}
	if authorization.Branch != publishBranchOf(claim) {
		t.Fatalf("authorization branch = %q, want %q", authorization.Branch, publishBranchOf(claim))
	}
	if authorization.Completed != nil {
		t.Fatalf("an unrecorded step reported itself completed: %+v", authorization.Completed)
	}
	if authorization.Repository != repoA || authorization.BaseSHA != shaA {
		t.Fatalf("authorization = %+v, want the job's repository and pinned base", authorization)
	}
}

// Order is a control-plane invariant, not a worker convention: proof cannot
// precede the push it proves, and a pull request cannot precede its branch.
// Recording is idempotent by primary key, and a second, DIFFERENT answer for
// one step is a conflict rather than an overwrite — one step with two results
// is evidence of a race worth surfacing.
func TestPublishStepsAreOrderedAndIdempotent(t *testing.T) {
	store, _ := newTestStore(t)
	registerTestWorker(t, store, 1)
	seedRun(t, store, "run-1", protocol.RunTarget{Repository: repoA, BaseSHA: shaA})
	claim := claimAndStart(t, store, "run-1", repoA)

	if _, err := recordStep(store, claim, tokenA, protocol.PublishStepPullRequest, "",
		"https://github.com/example/repo-a/pull/1"); err == nil ||
		serviceCode(t, err) != "publish_step_out_of_order" {
		t.Fatalf("pull request before push: err=%v, want publish_step_out_of_order", err)
	}
	if _, err := recordStep(store, claim, tokenA, protocol.PublishStepProof, shaB, ""); err == nil ||
		serviceCode(t, err) != "publish_step_out_of_order" {
		t.Fatalf("proof before push: err=%v, want publish_step_out_of_order", err)
	}

	first, err := recordStep(store, claim, tokenA, protocol.PublishStepPush, shaB, "")
	if err != nil {
		t.Fatalf("record push: %v", err)
	}
	if first.Branch != publishBranchOf(claim) || first.RemoteRef != shaB ||
		first.JobID != claim.Job.ID || first.AttemptNumber != claim.Attempt.AttemptNumber {
		t.Fatalf("push record = %+v, want the attempt's branch at %s", first, shaB)
	}
	replay, err := recordStep(store, claim, tokenA, protocol.PublishStepPush, shaB, "")
	if err != nil {
		t.Fatalf("replay push: %v", err)
	}
	if replay.CompletedAt != first.CompletedAt || replay.RemoteRef != first.RemoteRef {
		t.Fatalf("replayed push record = %+v, want the stored %+v", replay, first)
	}
	if _, err := recordStep(store, claim, tokenA, protocol.PublishStepPush, shaA, ""); err == nil ||
		serviceCode(t, err) != "publish_step_conflict" {
		t.Fatalf("push recorded twice with different SHAs: err=%v, want publish_step_conflict", err)
	}

	// With push recorded, the authorization for it reports it completed — the
	// signal that makes a re-entering worker skip the side effect entirely.
	authorization, err := authorizeStep(store, claim, tokenA, protocol.PublishStepPush)
	if err != nil {
		t.Fatalf("authorize a recorded step: %v", err)
	}
	if authorization.Completed == nil || authorization.Completed.RemoteRef != shaB {
		t.Fatalf("authorization = %+v, want the recorded push", authorization)
	}

	// A push record does not by itself validate a malformed proof.
	if _, err := recordStep(store, claim, tokenA, protocol.PublishStepProof, "not-a-sha", ""); err == nil ||
		serviceCode(t, err) != "invalid_publish_ref" {
		t.Fatalf("proof with a non-SHA ref: err=%v, want invalid_publish_ref", err)
	}
	if _, err := recordStep(store, claim, tokenA, protocol.PublishStepPullRequest, "", "notaurl"); err == nil ||
		serviceCode(t, err) != "invalid_publish_url" {
		t.Fatalf("pull request with a non-URL: err=%v, want invalid_publish_url", err)
	}

	if _, err := recordStep(store, claim, tokenA, protocol.PublishStepPullRequest, "",
		"https://github.com/example/repo-a/pull/1"); err != nil {
		t.Fatalf("record pull request: %v", err)
	}
	if _, err := recordStep(store, claim, tokenA, protocol.PublishStepProof, shaB, ""); err != nil {
		t.Fatalf("record proof: %v", err)
	}
	records, err := store.JobPublishRecords(context.Background(), claim.Job.ID)
	if err != nil {
		t.Fatalf("job publish records: %v", err)
	}
	if len(records) != 3 {
		t.Fatalf("job holds %d publish records, want the three steps: %+v", len(records), records)
	}
}

// The publish-only retry re-leases the SAME attempt (R14). A new attempt
// would mean a new attempt-scoped branch and a second pull request for one
// piece of work, which is exactly what this action exists to avoid — so the
// attempt id must not change, and the steps already proven must come back
// with it so publish re-enters where it stopped.
func TestPublishOnlyRetryReLeasesTheSameAttemptWithItsProvenSteps(t *testing.T) {
	store, _ := newTestStore(t)
	registerTestWorker(t, store, 1)
	seedRun(t, store, "run-1", protocol.RunTarget{Repository: repoA, BaseSHA: shaA})
	claim := claimAndStart(t, store, "run-1", repoA)

	// Push landed; the pull request did not. The attempt completes
	// accepted_unpublished, which is the state with a publish-only retry.
	if _, err := recordStep(store, claim, tokenA, protocol.PublishStepPush, shaB, ""); err != nil {
		t.Fatalf("record push: %v", err)
	}
	if _, err := store.CompleteAttempt(context.Background(), claim.Attempt.ID,
		protocol.CompleteAttemptRequest{LeaseToken: tokenA,
			State: protocol.AttemptAcceptedUnpublished, Result: `{"changed_paths":["a.txt"]}`}); err != nil {
		t.Fatalf("complete accepted_unpublished: %v", err)
	}
	job, err := store.Job(context.Background(), claim.Job.ID)
	if err != nil {
		t.Fatalf("read job: %v", err)
	}
	if job.State != protocol.JobAcceptedUnpublished {
		t.Fatalf("job state = %q, want accepted_unpublished", job.State)
	}

	// The retry belongs to the worker that holds the attempt's retained state.
	if _, err := store.RetryPublish(context.Background(), claim.Job.ID,
		protocol.PublishRetryRequest{WorkerID: "worker-other", LeaseToken: tokenB}); err == nil ||
		serviceCode(t, err) != "publish_retry_foreign_worker" {
		t.Fatalf("retry from a foreign worker: err=%v, want publish_retry_foreign_worker", err)
	}

	retry, err := store.RetryPublish(context.Background(), claim.Job.ID,
		protocol.PublishRetryRequest{WorkerID: workerA, LeaseToken: tokenB})
	if err != nil {
		t.Fatalf("publish retry: %v", err)
	}
	if retry.Attempt.ID != claim.Attempt.ID {
		t.Fatalf("retry produced attempt %s, want the same attempt %s — a new attempt means a new branch",
			retry.Attempt.ID, claim.Attempt.ID)
	}
	if retry.Attempt.State != protocol.AttemptRunning || retry.Job.State != protocol.JobActive {
		t.Fatalf("retry left attempt=%q job=%q, want running/active", retry.Attempt.State, retry.Job.State)
	}
	if len(retry.Records) != 1 || retry.Records[0].Step != protocol.PublishStepPush ||
		retry.Records[0].RemoteRef != shaB {
		t.Fatalf("retry records = %+v, want the proven push", retry.Records)
	}
	var attempts int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM attempts WHERE job_id = ?`,
		claim.Job.ID).Scan(&attempts); err != nil {
		t.Fatalf("count attempts: %v", err)
	}
	if attempts != 1 {
		t.Fatalf("the job holds %d attempts after a publish retry, want 1", attempts)
	}

	// The old token is fenced out by the re-lease; the new one owns the
	// attempt and finishes the publish.
	if _, err := recordStep(store, claim, tokenA, protocol.PublishStepPullRequest, "",
		"https://github.com/example/repo-a/pull/7"); err == nil ||
		serviceCode(t, err) != "lease_not_owner" {
		t.Fatalf("superseded token recorded a step: err=%v, want lease_not_owner", err)
	}
	if _, err := recordStep(store, claim, tokenB, protocol.PublishStepPullRequest, "",
		"https://github.com/example/repo-a/pull/7"); err != nil {
		t.Fatalf("record pull request under the retry lease: %v", err)
	}
	if _, err := recordStep(store, claim, tokenB, protocol.PublishStepProof, shaB, ""); err != nil {
		t.Fatalf("record proof under the retry lease: %v", err)
	}
	attempt, err := store.CompleteAttempt(context.Background(), claim.Attempt.ID,
		protocol.CompleteAttemptRequest{LeaseToken: tokenB, State: protocol.AttemptAccepted})
	if err != nil {
		t.Fatalf("complete accepted after retry: %v", err)
	}
	if attempt.State != protocol.AttemptAccepted {
		t.Fatalf("attempt state = %q, want accepted", attempt.State)
	}
	job, err = store.Job(context.Background(), claim.Job.ID)
	if err != nil {
		t.Fatalf("read job: %v", err)
	}
	if job.State != protocol.JobAccepted {
		t.Fatalf("job state = %q, want accepted", job.State)
	}
}

// Only an accepted_unpublished job has a publish-only retry: a failed job
// retries cold through RetryJob (KTD5), and an accepted one has nothing left
// to publish.
func TestPublishRetryIsRefusedForJobsThatAreNotAcceptedUnpublished(t *testing.T) {
	store, _ := newTestStore(t)
	registerTestWorker(t, store, 1)
	seedRun(t, store, "run-1", protocol.RunTarget{Repository: repoA, BaseSHA: shaA})
	claim := claimAndStart(t, store, "run-1", repoA)

	if _, err := store.RetryPublish(context.Background(), claim.Job.ID,
		protocol.PublishRetryRequest{WorkerID: workerA, LeaseToken: tokenB}); err == nil ||
		serviceCode(t, err) != "publish_retry_not_allowed" {
		t.Fatalf("retry of an active job: err=%v, want publish_retry_not_allowed", err)
	}
	if _, err := store.CompleteAttempt(context.Background(), claim.Attempt.ID,
		protocol.CompleteAttemptRequest{LeaseToken: tokenA, State: protocol.AttemptFailed,
			Error: "phase failed"}); err != nil {
		t.Fatalf("complete failed: %v", err)
	}
	if _, err := store.RetryPublish(context.Background(), claim.Job.ID,
		protocol.PublishRetryRequest{WorkerID: workerA, LeaseToken: tokenB}); err == nil ||
		serviceCode(t, err) != "publish_retry_not_allowed" {
		t.Fatalf("retry of a failed job: err=%v, want publish_retry_not_allowed", err)
	}
}

// ciFixtureSnapshot is fixtureSnapshot with publish.ci opted in.
const ciFixtureSnapshot = fixtureSnapshot + `publish:
  ci:
    wait: true
`

// optInToCI swaps the run's frozen definition for one whose publish waits
// for CI — the fixture every CI-gate scenario starts from.
func optInToCI(t *testing.T, store *Store, runID string) {
	t.Helper()
	if _, err := store.db.Exec(`UPDATE runs SET snapshot = ? WHERE id = ?`, ciFixtureSnapshot, runID); err != nil {
		t.Fatalf("opt run %s into CI: %v", runID, err)
	}
}

// A definition that waits for CI extends R12's "accepted means published"
// to "accepted means published AND green". Like proof, that is checked by
// the store against its own ledger and the run's frozen definition — never
// taken from the worker's word — and the ci step sits after proof in the
// fenced step order.
func TestAcceptedRequiresARecordedCIStepWhenTheDefinitionWaitsForCI(t *testing.T) {
	store, _ := newTestStore(t)
	registerTestWorker(t, store, 1)
	seedRun(t, store, "run-1", protocol.RunTarget{Repository: repoA, BaseSHA: shaA})
	optInToCI(t, store, "run-1")
	claim := claimAndStart(t, store, "run-1", repoA)
	ctx := context.Background()

	if _, err := recordStep(store, claim, tokenA, protocol.PublishStepPush, shaB, ""); err != nil {
		t.Fatalf("record push: %v", err)
	}
	if _, err := recordStep(store, claim, tokenA, protocol.PublishStepCI, shaB, ""); err == nil ||
		serviceCode(t, err) != "publish_step_out_of_order" {
		t.Fatalf("ci before proof: err=%v, want publish_step_out_of_order", err)
	}
	if _, err := recordStep(store, claim, tokenA, protocol.PublishStepPullRequest, "",
		"https://github.com/example/repo-a/pull/3"); err != nil {
		t.Fatalf("record pull request: %v", err)
	}
	if _, err := recordStep(store, claim, tokenA, protocol.PublishStepProof, shaB, ""); err != nil {
		t.Fatalf("record proof: %v", err)
	}

	_, err := store.CompleteAttempt(ctx, claim.Attempt.ID, protocol.CompleteAttemptRequest{
		LeaseToken: tokenA, State: protocol.AttemptAccepted,
	})
	if err == nil || serviceCode(t, err) != "publish_ci_required" {
		t.Fatalf("accepted with proof but no ci record: err=%v, want publish_ci_required", err)
	}

	if _, err := recordStep(store, claim, tokenA, protocol.PublishStepCI, "green", ""); err == nil ||
		serviceCode(t, err) != "invalid_publish_ref" {
		t.Fatalf("ci with a non-SHA ref: err=%v, want invalid_publish_ref", err)
	}
	// CI may be judged on a later head than jig pushed (a person's fix on the
	// branch), so the ci ref need not equal the push ref.
	if _, err := recordStep(store, claim, tokenA, protocol.PublishStepCI, shaA, ""); err != nil {
		t.Fatalf("record ci: %v", err)
	}
	accepted, err := store.CompleteAttempt(ctx, claim.Attempt.ID, protocol.CompleteAttemptRequest{
		LeaseToken: tokenA, State: protocol.AttemptAccepted,
	})
	if err != nil {
		t.Fatalf("complete accepted with a ci record: %v", err)
	}
	if accepted.State != protocol.AttemptAccepted {
		t.Fatalf("attempt state = %q, want accepted", accepted.State)
	}
}

// A definition that does not opt in is unchanged: proof alone is enough.
func TestAcceptedNeedsNoCIStepWhenTheDefinitionDoesNotWait(t *testing.T) {
	store, _ := newTestStore(t)
	registerTestWorker(t, store, 1)
	seedRun(t, store, "run-1", protocol.RunTarget{Repository: repoA, BaseSHA: shaA})
	claim := claimAndStart(t, store, "run-1", repoA)
	for _, step := range []struct{ name, ref, url string }{
		{protocol.PublishStepPush, shaB, ""},
		{protocol.PublishStepPullRequest, "", "https://github.com/example/repo-a/pull/4"},
		{protocol.PublishStepProof, shaB, ""},
	} {
		if _, err := recordStep(store, claim, tokenA, step.name, step.ref, step.url); err != nil {
			t.Fatalf("record %s: %v", step.name, err)
		}
	}
	if _, err := store.CompleteAttempt(context.Background(), claim.Attempt.ID, protocol.CompleteAttemptRequest{
		LeaseToken: tokenA, State: protocol.AttemptAccepted,
	}); err != nil {
		t.Fatalf("complete accepted without CI opt-in: %v", err)
	}
}

// The publish-only retry hands the worker the run's frozen definition, so a
// retry re-applies the same CI policy the first publish ran under.
func TestPublishRetryCarriesTheRunSnapshot(t *testing.T) {
	store, _ := newTestStore(t)
	registerTestWorker(t, store, 1)
	seedRun(t, store, "run-1", protocol.RunTarget{Repository: repoA, BaseSHA: shaA})
	optInToCI(t, store, "run-1")
	claim := claimAndStart(t, store, "run-1", repoA)
	if _, err := store.CompleteAttempt(context.Background(), claim.Attempt.ID,
		protocol.CompleteAttemptRequest{LeaseToken: tokenA, State: protocol.AttemptAcceptedUnpublished}); err != nil {
		t.Fatalf("complete accepted_unpublished: %v", err)
	}
	retry, err := store.RetryPublish(context.Background(), claim.Job.ID,
		protocol.PublishRetryRequest{WorkerID: workerA, LeaseToken: tokenB})
	if err != nil {
		t.Fatalf("publish retry: %v", err)
	}
	if retry.Snapshot != ciFixtureSnapshot {
		t.Fatalf("retry snapshot = %q, want the run's frozen definition", retry.Snapshot)
	}
}

// A publish hold is a fact on the attempt, not a string inside a result the
// control plane never reads: written once when the attempt completes held,
// and kept through the publish-only retry that releases it. What jig may
// later do with the work is decided from this column, so a released hold
// must still read as held.
func TestAPublishHoldIsRecordedOnceAndSurvivesItsRelease(t *testing.T) {
	store, _ := newTestStore(t)
	registerTestWorker(t, store, 1)
	seedRun(t, store, "run-1", protocol.RunTarget{Repository: repoA, BaseSHA: shaA})
	claim := claimAndStart(t, store, "run-1", repoA)
	ctx := context.Background()
	const reason = `publish.hold_when "risk != low" held (risk = high)`

	if started, err := store.Attempt(ctx, claim.Attempt.ID); err != nil || started.PublishHold != "" {
		t.Fatalf("a running attempt reads publish_hold %q (err=%v), want never held", started.PublishHold, err)
	}
	held, err := store.CompleteAttempt(ctx, claim.Attempt.ID, protocol.CompleteAttemptRequest{
		LeaseToken: tokenA, State: protocol.AttemptAcceptedUnpublished, PublishHold: reason,
	})
	if err != nil {
		t.Fatalf("complete held: %v", err)
	}
	if held.PublishHold != reason {
		t.Fatalf("publish_hold = %q, want the reason", held.PublishHold)
	}

	// The release: the same attempt is re-leased, publishes, and completes
	// accepted. Its completion carries no hold, and the first reason stands.
	retry, err := store.RetryPublish(ctx, claim.Job.ID,
		protocol.PublishRetryRequest{WorkerID: workerA, LeaseToken: tokenB})
	if err != nil {
		t.Fatalf("publish retry: %v", err)
	}
	if retry.Attempt.PublishHold != reason {
		t.Fatalf("the re-leased attempt reads publish_hold %q, want the reason", retry.Attempt.PublishHold)
	}
	proveThePublish(t, store, claim, tokenB)
	released, err := store.CompleteAttempt(ctx, claim.Attempt.ID, protocol.CompleteAttemptRequest{
		LeaseToken: tokenB, State: protocol.AttemptAccepted,
	})
	if err != nil {
		t.Fatalf("complete released: %v", err)
	}
	if released.State != protocol.AttemptAccepted || released.PublishHold != reason {
		t.Fatalf("released attempt = %s with publish_hold %q, want accepted and still held",
			released.State, released.PublishHold)
	}
}

// A second hold on the same attempt — a retry that somehow reported one —
// never rewrites the first: the column answers "was this ever held, and why
// first", and the first reason is the one a person released.
func TestTheFirstPublishHoldReasonStands(t *testing.T) {
	store, _ := newTestStore(t)
	registerTestWorker(t, store, 1)
	seedRun(t, store, "run-1", protocol.RunTarget{Repository: repoA, BaseSHA: shaA})
	claim := claimAndStart(t, store, "run-1", repoA)
	ctx := context.Background()

	if _, err := store.CompleteAttempt(ctx, claim.Attempt.ID, protocol.CompleteAttemptRequest{
		LeaseToken: tokenA, State: protocol.AttemptAcceptedUnpublished, PublishHold: "first",
	}); err != nil {
		t.Fatalf("complete held: %v", err)
	}
	if _, err := store.RetryPublish(ctx, claim.Job.ID,
		protocol.PublishRetryRequest{WorkerID: workerA, LeaseToken: tokenB}); err != nil {
		t.Fatalf("publish retry: %v", err)
	}
	again, err := store.CompleteAttempt(ctx, claim.Attempt.ID, protocol.CompleteAttemptRequest{
		LeaseToken: tokenB, State: protocol.AttemptAcceptedUnpublished, PublishHold: "second",
	})
	if err != nil {
		t.Fatalf("complete again: %v", err)
	}
	if again.PublishHold != "first" {
		t.Fatalf("publish_hold = %q, want the first reason", again.PublishHold)
	}
}

// A hold travels only with accepted_unpublished. Held work is accepted and
// not shipped; a hold on anything else is a worker contradicting itself, and
// the completion is refused whole rather than stored without the hold.
func TestAPublishHoldIsRefusedWithAnyOtherState(t *testing.T) {
	for _, state := range []string{protocol.AttemptAccepted, protocol.AttemptFailed, protocol.AttemptCancelled} {
		t.Run(state, func(t *testing.T) {
			store, _ := newTestStore(t)
			registerTestWorker(t, store, 1)
			seedRun(t, store, "run-1", protocol.RunTarget{Repository: repoA, BaseSHA: shaA})
			claim := claimAndStart(t, store, "run-1", repoA)
			ctx := context.Background()
			proveThePublish(t, store, claim, tokenA)

			_, err := store.CompleteAttempt(ctx, claim.Attempt.ID, protocol.CompleteAttemptRequest{
				LeaseToken: tokenA, State: state, PublishHold: "risk = high",
			})
			if err == nil || serviceCode(t, err) != "invalid_publish_hold" {
				t.Fatalf("hold with %s: err=%v, want invalid_publish_hold", state, err)
			}
			attempt, err := store.Attempt(ctx, claim.Attempt.ID)
			if err != nil {
				t.Fatalf("read attempt: %v", err)
			}
			if attempt.State != protocol.AttemptRunning || attempt.PublishHold != "" {
				t.Fatalf("the refused completion left state %q and publish_hold %q",
					attempt.State, attempt.PublishHold)
			}
		})
	}
}

func TestAnOversizedPublishHoldIsRefused(t *testing.T) {
	store, _ := newTestStore(t)
	registerTestWorker(t, store, 1)
	seedRun(t, store, "run-1", protocol.RunTarget{Repository: repoA, BaseSHA: shaA})
	claim := claimAndStart(t, store, "run-1", repoA)

	_, err := store.CompleteAttempt(context.Background(), claim.Attempt.ID, protocol.CompleteAttemptRequest{
		LeaseToken: tokenA, State: protocol.AttemptAcceptedUnpublished,
		PublishHold: strings.Repeat("x", protocol.MaxPublishDiagnosticBytes+1),
	})
	if err == nil || serviceCode(t, err) != "result_too_large" {
		t.Fatalf("oversized hold: err=%v, want result_too_large", err)
	}
}

// heldJob drives one job to accepted_unpublished as workerA, the state an
// operator's publish request applies to.
func heldJob(t *testing.T, store *Store, runID string) *protocol.Claim {
	t.Helper()
	seedRun(t, store, runID, protocol.RunTarget{Repository: repoA, BaseSHA: shaA})
	claim := claimAndStart(t, store, runID, repoA)
	if _, err := store.CompleteAttempt(context.Background(), claim.Attempt.ID, protocol.CompleteAttemptRequest{
		LeaseToken: tokenA, State: protocol.AttemptAcceptedUnpublished, PublishHold: "risk = high",
	}); err != nil {
		t.Fatalf("complete held: %v", err)
	}
	return claim
}

// The operator's request carries no lease and names no worker. It is
// recorded on the job, listed for the worker that ran the attempt and for no
// other, and consumed by the completion of the retry it asks for: one
// request, one retry.
func TestAPublishRequestIsListedForItsWorkerAndConsumedByTheRetry(t *testing.T) {
	store, clock := newTestStore(t)
	registerTestWorker(t, store, 1)
	claim := heldJob(t, store, "run-1")
	ctx := context.Background()

	if job, err := store.Job(ctx, claim.Job.ID); err != nil || job.PublishRequestedAt != nil {
		t.Fatalf("a job nobody asked about reads publish_requested_at %v (err=%v)", job.PublishRequestedAt, err)
	}
	requested, err := store.RequestPublish(ctx, claim.Job.ID)
	if err != nil {
		t.Fatalf("request publish: %v", err)
	}
	if requested.PublishRequestedAt == nil || !requested.PublishRequestedAt.Equal(clock.Now()) {
		t.Fatalf("publish_requested_at = %v, want now", requested.PublishRequestedAt)
	}
	first := *requested.PublishRequestedAt

	// Asking again is not asking for a second retry: the first request and
	// its time stand.
	clock.Advance(time.Minute)
	again, err := store.RequestPublish(ctx, claim.Job.ID)
	if err != nil {
		t.Fatalf("request publish again: %v", err)
	}
	if again.PublishRequestedAt == nil || !again.PublishRequestedAt.Equal(first) {
		t.Fatalf("a repeated request moved its time to %v, want %v", again.PublishRequestedAt, first)
	}

	mine, err := store.WorkerPublishRequests(ctx, workerA)
	if err != nil {
		t.Fatalf("list requests: %v", err)
	}
	if len(mine) != 1 || mine[0].JobID != claim.Job.ID || mine[0].AttemptID != claim.Attempt.ID ||
		!mine[0].RequestedAt.Equal(first) {
		t.Fatalf("requests for the owning worker = %+v, want the one request", mine)
	}
	if theirs, err := store.WorkerPublishRequests(ctx, "worker-other"); err != nil || len(theirs) != 0 {
		t.Fatalf("requests for a foreign worker = %+v (err=%v), want none", theirs, err)
	}

	retry, err := store.RetryPublish(ctx, claim.Job.ID,
		protocol.PublishRetryRequest{WorkerID: workerA, LeaseToken: tokenB})
	if err != nil {
		t.Fatalf("publish retry: %v", err)
	}
	// The grant does not consume the request — an interrupted retry must be
	// served again — but the running job no longer lists it.
	if retry.Job.PublishRequestedAt == nil || !retry.Job.PublishRequestedAt.Equal(first) {
		t.Fatalf("the granted retry moved its request to %v, want %v", retry.Job.PublishRequestedAt, first)
	}
	if left, err := store.WorkerPublishRequests(ctx, workerA); err != nil || len(left) != 0 {
		t.Fatalf("requests after the retry was granted = %+v (err=%v), want none", left, err)
	}

	// The retry did not finish the publish. The job is accepted_unpublished
	// again with no request pending: trying again is the operator's call.
	if _, err := store.CompleteAttempt(ctx, claim.Attempt.ID, protocol.CompleteAttemptRequest{
		LeaseToken: tokenB, State: protocol.AttemptAcceptedUnpublished,
	}); err != nil {
		t.Fatalf("complete the failed retry: %v", err)
	}
	if job, err := store.Job(ctx, claim.Job.ID); err != nil || job.State != protocol.JobAcceptedUnpublished ||
		job.PublishRequestedAt != nil {
		t.Fatalf("job after a failed retry = %+v (err=%v), want accepted_unpublished with no request", job, err)
	}
}

// The retry an operator's Release started is interrupted: its worker died
// mid-publish and the sweeper returns the job to accepted_unpublished. The
// request was never served, so it must still be pending and listed for the
// worker, not silently dropped with the lost lease.
func TestAPublishRequestSurvivesAnInterruptedRetry(t *testing.T) {
	store, clock := newTestStore(t)
	registerTestWorker(t, store, 1)
	claim := heldJob(t, store, "run-1")
	ctx := context.Background()

	requested, err := store.RequestPublish(ctx, claim.Job.ID)
	if err != nil {
		t.Fatalf("request publish: %v", err)
	}
	if _, err := store.RetryPublish(ctx, claim.Job.ID,
		protocol.PublishRetryRequest{WorkerID: workerA, LeaseToken: tokenB}); err != nil {
		t.Fatalf("publish retry: %v", err)
	}
	sweepUntilSettled(t, store, clock, claim.Attempt.ID)

	job, err := store.Job(ctx, claim.Job.ID)
	if err != nil {
		t.Fatalf("read job: %v", err)
	}
	if job.State != protocol.JobAcceptedUnpublished || job.PublishRequestedAt == nil ||
		!job.PublishRequestedAt.Equal(*requested.PublishRequestedAt) {
		t.Fatalf("job after an interrupted retry = %+v, want accepted_unpublished with the request pending", job)
	}
	mine, err := store.WorkerPublishRequests(ctx, workerA)
	if err != nil || len(mine) != 1 || mine[0].JobID != claim.Job.ID {
		t.Fatalf("requests after an interrupted retry = %+v (err=%v), want the request again", mine, err)
	}
}

func TestAPublishRequestCanBeWithdrawnBeforeAWorkerTakesIt(t *testing.T) {
	store, _ := newTestStore(t)
	registerTestWorker(t, store, 1)
	claim := heldJob(t, store, "run-1")
	ctx := context.Background()

	if _, err := store.RequestPublish(ctx, claim.Job.ID); err != nil {
		t.Fatalf("request publish: %v", err)
	}
	withdrawn, err := store.WithdrawPublishRequest(ctx, claim.Job.ID)
	if err != nil {
		t.Fatalf("withdraw: %v", err)
	}
	if withdrawn.PublishRequestedAt != nil || withdrawn.State != protocol.JobAcceptedUnpublished {
		t.Fatalf("withdrawn job = %+v, want accepted_unpublished with no request", withdrawn)
	}
	if left, err := store.WorkerPublishRequests(ctx, workerA); err != nil || len(left) != 0 {
		t.Fatalf("requests after withdrawal = %+v (err=%v), want none", left, err)
	}
	// Withdrawing nothing is not an error; withdrawing from no job is.
	if _, err := store.WithdrawPublishRequest(ctx, claim.Job.ID); err != nil {
		t.Fatalf("withdraw with nothing pending: %v", err)
	}
	if _, err := store.WithdrawPublishRequest(ctx, "no-such-job"); err != ErrNotFound {
		t.Fatalf("withdraw from an unknown job: err=%v, want ErrNotFound", err)
	}
}

// Only an accepted_unpublished job has a publish to finish. A request for
// work still running, or already published, is refused rather than parked.
func TestAPublishRequestIsRefusedForAJobThatIsNotAcceptedUnpublished(t *testing.T) {
	store, _ := newTestStore(t)
	registerTestWorker(t, store, 1)
	seedRun(t, store, "run-1", protocol.RunTarget{Repository: repoA, BaseSHA: shaA})
	claim := claimAndStart(t, store, "run-1", repoA)
	ctx := context.Background()

	if _, err := store.RequestPublish(ctx, claim.Job.ID); err == nil ||
		serviceCode(t, err) != "publish_request_not_allowed" {
		t.Fatalf("request for a running job: err=%v, want publish_request_not_allowed", err)
	}
	proveThePublish(t, store, claim, tokenA)
	if _, err := store.CompleteAttempt(ctx, claim.Attempt.ID, protocol.CompleteAttemptRequest{
		LeaseToken: tokenA, State: protocol.AttemptAccepted,
	}); err != nil {
		t.Fatalf("complete accepted: %v", err)
	}
	if _, err := store.RequestPublish(ctx, claim.Job.ID); err == nil ||
		serviceCode(t, err) != "publish_request_not_allowed" {
		t.Fatalf("request for an accepted job: err=%v, want publish_request_not_allowed", err)
	}
	if _, err := store.RequestPublish(ctx, "no-such-job"); err != ErrNotFound {
		t.Fatalf("request for an unknown job: err=%v, want ErrNotFound", err)
	}
	if job, err := store.Job(ctx, claim.Job.ID); err != nil || job.PublishRequestedAt != nil {
		t.Fatalf("a refused request was recorded: %+v (err=%v)", job, err)
	}
}
