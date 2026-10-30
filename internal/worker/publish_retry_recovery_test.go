// publish_retry_recovery_test.go — a publish-only retry that stalls (CI that
// never answers, a process that stops heartbeating) loses its lease to the
// sweeper, which returns the attempt to accepted_unpublished rather than
// `lost`. The stalled retry can no longer write anything when it wakes, and
// the next retry re-enters the SAME attempt: same branch and pull request, CI
// judged on the branch's current head, and still not one phase re-run.
package worker

import (
	"context"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/StructuPath/jig/internal/controlplane"
	"github.com/StructuPath/jig/internal/protocol"
)

func TestAStalledPublishRetryIsSweptBackAndTheNextRetryFinishesTheSameAttempt(t *testing.T) {
	h := newHarness(t)
	originDir, head, identity := newOriginRepo(t)
	h.seedRunWithSnapshot("run-1", ciSnapshot, protocol.RunTarget{Repository: identity, BaseSHA: head})
	job := h.enqueue("run-1", identity)
	branch := protocol.PublishBranch(job.ID, 1)

	var phaseRuns int
	inner := RunnerFunc(func(ctx context.Context, attempt *PreparedAttempt) Outcome {
		phaseRuns++
		return writeAndDeclare(t, map[string]string{"work.txt": "work\n"}).Run(ctx, attempt)
	})
	// CI is red for the attempt, never answers for the first retry, and is
	// green for the second.
	var mode string
	entered, release := make(chan struct{}), make(chan struct{})
	var enterOnce sync.Once
	gateway := newFakeGateway()
	gateway.checks = func(string, int) ([]CICheck, error) {
		switch mode {
		case "stall":
			enterOnce.Do(func() { close(entered) })
			<-release
			return []CICheck{check("test", CIPass)}, nil
		case "green":
			return []CICheck{check("test", CIPass)}, nil
		}
		return []CICheck{check("test", CIFail)}, nil
	}
	w := newCIWorker(t, h, inner, gateway)
	attempt, err := w.ClaimOnce(context.Background())
	if err != nil || attempt == nil || attempt.State != protocol.AttemptAcceptedUnpublished {
		t.Fatalf("claim = %+v (%v), want accepted_unpublished on red CI", attempt, err)
	}

	mode = "stall"
	stalled := make(chan error, 1)
	go func() {
		_, err := w.RetryPublish(context.Background(), job.ID, gateway, fastCIOptions())
		stalled <- err
	}()
	select {
	case <-entered:
	case <-time.After(30 * time.Second):
		t.Fatal("the first retry never reached CI")
	}
	// Its lease runs out while CI hangs: three missed heartbeats at the
	// sweeper's cadence, compressed by expiring the lease outright.
	if _, err := h.db.Exec(`UPDATE attempts SET lease_expires_at = ? WHERE id = ?`,
		time.Now().Add(-time.Minute).UnixMilli(), attempt.ID); err != nil {
		t.Fatal(err)
	}
	sweeper := controlplane.NewSweeper(h.store)
	var swept []controlplane.LostAttempt
	for i := 0; i < protocol.MissedHeartbeatsBeforeSweep; i++ {
		ticked, err := sweeper.Tick(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		swept = append(swept, ticked...)
	}
	if len(swept) != 1 || swept[0].State != protocol.AttemptAcceptedUnpublished {
		t.Fatalf("sweep = %+v, want the retry's attempt returned to accepted_unpublished", swept)
	}

	// A person pushes a fix while the stalled retry is still asleep; then it
	// wakes, judges green, and is fenced out of every write.
	fixed := pushToBranch(t, originDir, branch)
	close(release)
	select {
	case err := <-stalled:
		if err == nil {
			t.Fatal("the swept retry completed its attempt")
		}
	case <-time.After(60 * time.Second):
		t.Fatal("the stalled retry never returned")
	}
	if steps := recordedSteps(t, w, attempt.ID); slices.Contains(steps, protocol.PublishStepCI) {
		t.Fatalf("ledger steps = %v: the swept retry recorded ci", steps)
	}
	if after, err := h.store.Attempt(context.Background(), attempt.ID); err != nil ||
		after.State != protocol.AttemptAcceptedUnpublished {
		t.Fatalf("attempt after the zombie woke = %+v (%v), want accepted_unpublished", after.State, err)
	}

	mode = "green"
	retried, err := w.RetryPublish(context.Background(), job.ID, gateway, fastCIOptions())
	if err != nil {
		t.Fatalf("second retry: %v", err)
	}
	summary := publishSummaryOf(t, retried.Result)
	if retried.ID != attempt.ID || retried.State != protocol.AttemptAccepted || summary.CIRef != fixed {
		t.Fatalf("second retry = %s %s judged %s, want attempt %s accepted on the current head %s",
			retried.ID, retried.State, summary.CIRef, attempt.ID, fixed)
	}
	if created, _ := gateway.counts(); created != 1 {
		t.Fatalf("pull requests created = %d, want 1", created)
	}
	if phaseRuns != 1 || len(gateway.reruns) != 0 || len(summary.CIRepairs) != 0 {
		t.Fatalf("phase runs %d, CI re-runs %v, repairs %v: a publish retry re-runs and repairs nothing",
			phaseRuns, gateway.reruns, summary.CIRepairs)
	}
	if remoteBranches(t, originDir)[branch] != fixed {
		t.Fatal("the branch moved off the person's fix")
	}
}
