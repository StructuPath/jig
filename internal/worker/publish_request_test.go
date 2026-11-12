// publish_request_test.go — the operator's publish request, end to end: a
// request posted to the control plane over plain HTTP, the way the UI and
// `jig job release` post it, picked up by the RUNNING worker that owns the
// attempt and served as a publish-only retry. The point of the feature is
// that nobody stops the worker, so every scenario here uses one live worker.
package worker

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/StructuPath/JIG-Software-Factory/internal/protocol"
)

// publishRequest posts (or, with DELETE, withdraws) an operator's request.
func (h *harness) publishRequest(method, jobID string) (int, protocol.Job) {
	h.t.Helper()
	request, err := http.NewRequest(method,
		h.server.URL+"/api/jobs/"+url.PathEscape(jobID)+"/publish-request", nil)
	if err != nil {
		h.t.Fatalf("build publish request: %v", err)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		h.t.Fatalf("publish request: %v", err)
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	var job protocol.Job
	if response.StatusCode == http.StatusOK {
		if err := json.Unmarshal(body, &job); err != nil {
			h.t.Fatalf("decode job from %s: %v", body, err)
		}
	}
	return response.StatusCode, job
}

// heldScenario is one worker that has run one attempt to a publish hold.
type heldScenario struct {
	h         *harness
	w         *Worker
	gateway   *fakeGateway
	originDir string
	job       protocol.Job
	branch    string
}

func newHeldScenario(t *testing.T, capacity int) *heldScenario {
	t.Helper()
	h := newHarness(t)
	originDir, head, identity := newOriginRepo(t)
	h.seedRun("run-1", protocol.RunTarget{Repository: identity, BaseSHA: head})
	job := h.enqueue("run-1", identity)

	write := writeAndDeclare(t, map[string]string{"src/auth.txt": "held work\n"})
	inner := RunnerFunc(func(ctx context.Context, attempt *PreparedAttempt) Outcome {
		outcome := write.Run(ctx, attempt)
		outcome.PublishHold = `publish.hold_when "risk != low" held (risk = high)`
		return outcome
	})
	gateway := newFakeGateway()
	runner := NewPublishingRunner(inner, gateway, PublishOptions{
		CommitAuthorName: "jig-test", CommitAuthorEmail: "jig@test",
	})
	w := newTestWorker(t, h, filepath.Join(t.TempDir(), "worker"), capacity, runner)
	runner.Bind(w)

	attempt, err := w.ClaimOnce(context.Background())
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if attempt == nil || attempt.State != protocol.AttemptAcceptedUnpublished {
		t.Fatalf("attempt = %+v, want accepted_unpublished (held)", attempt)
	}
	return &heldScenario{h: h, w: w, gateway: gateway, originDir: originDir,
		job: job, branch: protocol.PublishBranch(job.ID, 1)}
}

// serve runs one request pass, as one claim tick does, and waits for the
// retries it started.
func (s *heldScenario) serve() {
	var running sync.WaitGroup
	s.w.servePublishRequests(context.Background(), &running)
	running.Wait()
}

func (s *heldScenario) jobNow(t *testing.T) protocol.Job {
	t.Helper()
	job, err := s.h.store.Job(context.Background(), s.job.ID)
	if err != nil {
		t.Fatalf("read job: %v", err)
	}
	return job
}

func (s *heldScenario) pushed(t *testing.T) bool {
	t.Helper()
	_, pushed := remoteBranches(t, s.originDir)[s.branch]
	return pushed
}

// The whole path: a held job, a request over HTTP, and the running worker
// releasing it — same attempt, one branch, one pull request, no phase run.
func TestARunningWorkerReleasesAHeldJobOnRequest(t *testing.T) {
	s := newHeldScenario(t, 1)

	// Nothing was asked for yet, so a pass does nothing.
	s.serve()
	if s.pushed(t) {
		t.Fatal("a held job was published with no request")
	}

	status, requested := s.h.publishRequest(http.MethodPost, s.job.ID)
	if status != http.StatusOK || requested.PublishRequestedAt == nil {
		t.Fatalf("POST publish-request = %d %+v, want 200 with the request recorded", status, requested)
	}
	s.serve()

	job := s.jobNow(t)
	if job.State != protocol.JobAccepted || job.PublishRequestedAt != nil {
		t.Fatalf("job = %+v, want accepted with the request consumed", job)
	}
	attempts, err := s.h.store.JobAttempts(context.Background(), s.job.ID)
	if err != nil {
		t.Fatalf("read attempts: %v", err)
	}
	if len(attempts) != 1 || attempts[0].State != protocol.AttemptAccepted || attempts[0].PublishHold == "" {
		t.Fatalf("attempts = %+v, want the one attempt, accepted, still recorded as held", attempts)
	}
	if !s.pushed(t) {
		t.Fatal("the release did not push the held branch")
	}
	if created, adopted := s.gateway.counts(); created != 1 || adopted != 0 {
		t.Fatalf("gateway created %d and adopted %d pull requests, want exactly one created", created, adopted)
	}
	if s.w.activeCount() != 0 {
		t.Fatalf("the served request left %d slots held", s.w.activeCount())
	}

	// The request is spent: later passes find nothing and publish nothing.
	s.serve()
	if created, _ := s.gateway.counts(); created != 1 {
		t.Fatalf("a later pass created a second pull request (%d)", created)
	}
}

// A withdrawn request is never served.
func TestAWithdrawnPublishRequestIsNotServed(t *testing.T) {
	s := newHeldScenario(t, 1)
	if status, _ := s.h.publishRequest(http.MethodPost, s.job.ID); status != http.StatusOK {
		t.Fatalf("POST publish-request = %d", status)
	}
	status, withdrawn := s.h.publishRequest(http.MethodDelete, s.job.ID)
	if status != http.StatusOK || withdrawn.PublishRequestedAt != nil {
		t.Fatalf("DELETE publish-request = %d %+v, want 200 with no request", status, withdrawn)
	}
	s.serve()
	if s.pushed(t) || s.jobNow(t).State != protocol.JobAcceptedUnpublished {
		t.Fatal("a withdrawn request was served")
	}
}

// A release takes a slot. A full worker leaves the request pending, and
// serves it on a later pass once a slot is free.
func TestAFullWorkerServesThePublishRequestWhenASlotFrees(t *testing.T) {
	s := newHeldScenario(t, 1)
	if status, _ := s.h.publishRequest(http.MethodPost, s.job.ID); status != http.StatusOK {
		t.Fatalf("POST publish-request = %d", status)
	}

	s.w.trackActive("some-running-attempt")
	s.serve()
	if job := s.jobNow(t); s.pushed(t) || job.State != protocol.JobAcceptedUnpublished || job.PublishRequestedAt == nil {
		t.Fatalf("a full worker served the request, or lost it: %+v", job)
	}

	s.w.untrackActive("some-running-attempt")
	s.serve()
	if job := s.jobNow(t); job.State != protocol.JobAccepted {
		t.Fatalf("job = %+v, want accepted once a slot was free", job)
	}
}

// A request stays listed until its retry is granted, so two passes can see
// the same one. The slot reservation makes that one retry, not two.
func TestOnePublishRequestStartsOneRetryAcrossOverlappingPasses(t *testing.T) {
	s := newHeldScenario(t, 2)
	if status, _ := s.h.publishRequest(http.MethodPost, s.job.ID); status != http.StatusOK {
		t.Fatalf("POST publish-request = %d", status)
	}
	var running sync.WaitGroup
	for range 4 {
		s.w.servePublishRequests(context.Background(), &running)
	}
	running.Wait()

	if job := s.jobNow(t); job.State != protocol.JobAccepted {
		t.Fatalf("job = %+v, want accepted", job)
	}
	if created, adopted := s.gateway.counts(); created != 1 || adopted != 0 {
		t.Fatalf("gateway created %d and adopted %d pull requests, want exactly one created", created, adopted)
	}
}

// A request for a job another worker ran is not this worker's to serve: the
// retained worktree is not here. It stays pending for its owner.
func TestAWorkerDoesNotServeAnotherWorkersPublishRequest(t *testing.T) {
	s := newHeldScenario(t, 1)
	if status, _ := s.h.publishRequest(http.MethodPost, s.job.ID); status != http.StatusOK {
		t.Fatalf("POST publish-request = %d", status)
	}
	gateway := newFakeGateway()
	runner := NewPublishingRunner(RunnerFunc(func(context.Context, *PreparedAttempt) Outcome {
		return Outcome{State: protocol.AttemptFailed}
	}), gateway, PublishOptions{})
	other := newTestWorker(t, s.h, filepath.Join(t.TempDir(), "other"), 1, runner)
	runner.Bind(other)

	var running sync.WaitGroup
	other.servePublishRequests(context.Background(), &running)
	running.Wait()
	if job := s.jobNow(t); job.State != protocol.JobAcceptedUnpublished || job.PublishRequestedAt == nil {
		t.Fatalf("job = %+v, want it still waiting for its own worker", job)
	}
	if created, adopted := gateway.counts(); created+adopted != 0 {
		t.Fatal("a foreign worker touched the pull request")
	}
}

// The request is refused for a job with nothing left to publish, over the
// same route the operator surfaces use.
func TestThePublishRequestRouteRefusesAJobThatIsNotAcceptedUnpublished(t *testing.T) {
	s := newHeldScenario(t, 1)
	if status, _ := s.h.publishRequest(http.MethodPost, s.job.ID); status != http.StatusOK {
		t.Fatalf("POST publish-request = %d", status)
	}
	s.serve()
	if status, _ := s.h.publishRequest(http.MethodPost, s.job.ID); status != http.StatusConflict {
		t.Fatalf("POST publish-request for an accepted job = %d, want 409", status)
	}
	if status, _ := s.h.publishRequest(http.MethodPost, "no-such-job"); status != http.StatusNotFound {
		t.Fatalf("POST publish-request for an unknown job = %d, want 404", status)
	}
}

// The claim loop is what calls the request pass in a real worker. This is
// the wiring test: no direct call, only the loop, a request, and time.
func TestTheClaimLoopServesPublishRequests(t *testing.T) {
	s := newHeldScenario(t, 1)
	if status, _ := s.h.publishRequest(http.MethodPost, s.job.ID); status != http.StatusOK {
		t.Fatalf("POST publish-request = %d", status)
	}
	ctx, stop := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); s.w.runClaimLoop(ctx) }()
	defer func() { stop(); <-done }()

	deadline := time.Now().Add(5 * protocol.ClaimPollInterval)
	for s.jobNow(t).State != protocol.JobAccepted {
		if time.Now().After(deadline) {
			t.Fatalf("the claim loop did not serve the request: %+v", s.jobNow(t))
		}
		time.Sleep(50 * time.Millisecond)
	}
}
