package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/StructuPath/JIG-Software-Factory/internal/controlplane"
	"github.com/StructuPath/JIG-Software-Factory/internal/protocol"
)

// releaseServer is a scripted control plane for `jig job release`: it
// answers the request POST, then serves the job's states in order, one per
// GET, repeating the last. It records what was asked of it.
type releaseServer struct {
	*httptest.Server
	mutex       sync.Mutex
	posts, gets int
	onGet       func(gets int)
}

func newReleaseServer(t *testing.T, postStatus int, states ...controlplane.JobDetail) *releaseServer {
	t.Helper()
	s := &releaseServer{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mutex.Lock()
		defer s.mutex.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/jobs/job-1/publish-request":
			s.posts++
			if postStatus != http.StatusOK {
				w.WriteHeader(postStatus)
				_, _ = w.Write([]byte(`{"error":{"code":"publish_request_not_allowed",` +
					`"message":"only an accepted_unpublished job has a publish to finish"}}`))
				return
			}
			now := time.Now().UTC()
			_ = json.NewEncoder(w).Encode(protocol.Job{ID: "job-1",
				State: protocol.JobAcceptedUnpublished, PublishRequestedAt: &now})
		case r.Method == http.MethodGet && r.URL.Path == "/api/jobs/job-1":
			index := min(s.gets, len(states)-1)
			s.gets++
			if s.onGet != nil {
				s.onGet(s.gets)
			}
			_ = json.NewEncoder(w).Encode(states[index])
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *releaseServer) counts() (posts, gets int) {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	return s.posts, s.gets
}

func jobState(state string, requested bool, attemptError string) controlplane.JobDetail {
	detail := controlplane.JobDetail{Job: protocol.Job{ID: "job-1", State: state}}
	if requested {
		now := time.Now().UTC()
		detail.Job.PublishRequestedAt = &now
	}
	detail.Attempts = []protocol.Attempt{{ID: "attempt-1", State: state, Error: attemptError}}
	if state == protocol.JobAccepted {
		detail.Publish = []protocol.PublishRecord{
			{Step: protocol.PublishStepPush},
			{Step: protocol.PublishStepPullRequest, PullRequestURL: "https://github.com/example/repo/pull/7"},
		}
	}
	return detail
}

func jigJob(t *testing.T, ctx context.Context, args ...string) (int, string, string) {
	t.Helper()
	previous := jobReleasePollInterval
	jobReleasePollInterval = time.Millisecond
	t.Cleanup(func() { jobReleasePollInterval = previous })
	var stdout, stderr bytes.Buffer
	code := jobCommand(ctx, args, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

// The whole wait: the request is pending, the worker takes it (the job goes
// active), and the retry publishes. Exit 0, with the pull request named.
func TestJobReleaseWaitsThroughThePendingRequestAndTheRetryToAccepted(t *testing.T) {
	server := newReleaseServer(t, http.StatusOK,
		jobState(protocol.JobAcceptedUnpublished, true, ""),
		jobState(protocol.JobActive, false, ""),
		jobState(protocol.JobAccepted, false, ""))

	code, stdout, stderr := jigJob(t, context.Background(), "release", "--server", server.URL, "job-1")
	if code != exitAccepted {
		t.Fatalf("exit = %d, want %d (stderr: %s)", code, exitAccepted, stderr)
	}
	if !strings.Contains(stdout, "state: accepted") ||
		!strings.Contains(stdout, "pull request: https://github.com/example/repo/pull/7") {
		t.Fatalf("stdout does not report the accepted job and its pull request:\n%s", stdout)
	}
	if posts, gets := server.counts(); posts != 1 || gets != 3 {
		t.Fatalf("server saw %d POSTs and %d GETs, want 1 and 3", posts, gets)
	}
}

// A retry that ran and still did not publish leaves the job
// accepted_unpublished with no request: exit 3, as the stopped-worker
// command exits, with the attempt's diagnostic.
func TestJobReleaseExitsThreeWhenTheRetryDidNotFinishThePublish(t *testing.T) {
	server := newReleaseServer(t, http.StatusOK,
		jobState(protocol.JobActive, false, ""),
		jobState(protocol.JobAcceptedUnpublished, false, "publish did not complete (ci_failed): test (failure)"))

	code, stdout, stderr := jigJob(t, context.Background(), "release", "--server", server.URL, "job-1")
	if code != exitRejected {
		t.Fatalf("exit = %d, want %d (stderr: %s)", code, exitRejected, stderr)
	}
	if !strings.Contains(stdout, "state: accepted_unpublished") || !strings.Contains(stdout, "ci_failed") {
		t.Fatalf("stdout does not carry the state and the diagnostic:\n%s", stdout)
	}
}

func TestJobReleaseReportsARefusedRequestAndNeverWaits(t *testing.T) {
	server := newReleaseServer(t, http.StatusConflict, jobState(protocol.JobAccepted, false, ""))

	code, _, stderr := jigJob(t, context.Background(), "release", "--server", server.URL, "job-1")
	if code != exitInfraFailed || !strings.Contains(stderr, "publish_request_not_allowed") {
		t.Fatalf("exit = %d stderr = %q, want %d naming the refusal", code, stderr, exitInfraFailed)
	}
	if _, gets := server.counts(); gets != 0 {
		t.Fatalf("a refused request was still waited on (%d GETs)", gets)
	}
}

func TestJobReleaseNoWaitRecordsTheRequestAndReturns(t *testing.T) {
	server := newReleaseServer(t, http.StatusOK, jobState(protocol.JobAcceptedUnpublished, true, ""))

	code, stdout, stderr := jigJob(t, context.Background(), "release", "--server", server.URL, "--no-wait", "job-1")
	if code != exitAccepted || !strings.Contains(stdout, "release requested for job job-1") {
		t.Fatalf("exit = %d stdout = %q (stderr: %s)", code, stdout, stderr)
	}
	if posts, gets := server.counts(); posts != 1 || gets != 0 {
		t.Fatalf("server saw %d POSTs and %d GETs, want 1 and 0", posts, gets)
	}
}

// An interrupted wait is not a failed release: the request is on the ledger
// and the worker will still take it. The command says so.
func TestJobReleaseInterruptedWaitLeavesTheRequestRecorded(t *testing.T) {
	server := newReleaseServer(t, http.StatusOK, jobState(protocol.JobAcceptedUnpublished, true, ""))
	ctx, cancel := context.WithCancel(context.Background())
	server.onGet = func(gets int) {
		if gets == 3 {
			cancel()
		}
	}

	code, _, stderr := jigJob(t, ctx, "release", "--server", server.URL, "job-1")
	if code != exitInfraFailed || !strings.Contains(stderr, "the request stays recorded") {
		t.Fatalf("exit = %d stderr = %q, want %d and the note that the request stays", code, stderr, exitInfraFailed)
	}
}

// A job that left the publish lane entirely — failed, cancelled — has
// nothing to wait for.
func TestJobReleaseStopsWaitingOnAJobWithNoPublishToFinish(t *testing.T) {
	server := newReleaseServer(t, http.StatusOK, jobState(protocol.JobFailed, false, ""))

	code, _, stderr := jigJob(t, context.Background(), "release", "--server", server.URL, "job-1")
	if code != exitInfraFailed || !strings.Contains(stderr, "is failed") {
		t.Fatalf("exit = %d stderr = %q, want %d naming the state", code, stderr, exitInfraFailed)
	}
}

func TestJobCommandUsageErrors(t *testing.T) {
	for _, args := range [][]string{{}, {"publish"}, {"release"}, {"release", "a", "b"}, {"release", "--bogus", "a"}} {
		if code, _, _ := jigJob(t, context.Background(), args...); code != exitUsage {
			t.Errorf("jig job %v: exit = %d, want %d", args, code, exitUsage)
		}
	}
}
