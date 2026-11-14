// claiming_retry_test.go — what the worker does when the control plane's
// answer to a claim or a completion is lost in transit. The server may have
// acted on a request whose response never arrived, so the worker resends the
// same request rather than giving up (completion) or minting a new identity
// (claim).
package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/StructuPath/JIG-Software-Factory/internal/controlplane"
	"github.com/StructuPath/JIG-Software-Factory/internal/protocol"
)

// fault decides what happens to one request: pass it through, let the server
// act and then drop the connection before the answer, or answer with a fixed
// status without reaching the server.
type fault int

const (
	passThrough fault = iota
	actThenDrop
	answer503
	answer409LeaseLost
)

// newFaultyWorker builds a worker whose control plane consults decide(path,
// body) on every request.
func newFaultyWorker(t *testing.T, h *harness, runner AttemptRunner, decide func(path string, body []byte) fault) *Worker {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	inner := controlplane.NewHandler(h.store, "", logger)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		body, _ := io.ReadAll(request.Body)
		request.Body = io.NopCloser(bytes.NewReader(body))
		switch decide(request.URL.Path, body) {
		case actThenDrop:
			inner.ServeHTTP(httptest.NewRecorder(), request)
			connection, _, err := writer.(http.Hijacker).Hijack()
			if err != nil {
				t.Errorf("hijack: %v", err)
				return
			}
			_ = connection.Close()
		case answer503:
			writer.Header().Set("Content-Type", "application/json")
			writer.WriteHeader(http.StatusServiceUnavailable)
			_, _ = writer.Write([]byte(`{"error":{"code":"unavailable","message":"database is locked"}}`))
		case answer409LeaseLost:
			writer.Header().Set("Content-Type", "application/json")
			writer.WriteHeader(http.StatusConflict)
			_, _ = writer.Write([]byte(`{"error":{"code":"lease_not_owner","message":"swept"}}`))
		default:
			inner.ServeHTTP(writer, request)
		}
	}))
	t.Cleanup(server.Close)
	w, err := New(Config{
		ServerURL: server.URL,
		DataDir:   filepath.Join(t.TempDir(), "worker"),
		Capacity:  1,
		Environ:   []string{"PATH=" + os.Getenv("PATH")},
		Probes:    []RuntimeProbe{},
		Runner:    runner,
		Logger:    logger,
	})
	if err != nil {
		t.Fatalf("build worker: %v", err)
	}
	if _, err := w.RegisterOnce(context.Background()); err != nil {
		t.Fatalf("register worker: %v", err)
	}
	return w
}

// fastRetries shrinks the resend delays so the tests do not wait on them.
func fastRetries(t *testing.T) {
	t.Helper()
	claimDelays, completionDelays := claimResendDelays, completionRetryDelays
	claimResendDelays = []time.Duration{time.Millisecond, time.Millisecond}
	completionRetryDelays = []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond}
	t.Cleanup(func() {
		claimResendDelays, completionRetryDelays = claimDelays, completionDelays
	})
}

func acceptingRunner() AttemptRunner {
	return RunnerFunc(func(context.Context, *PreparedAttempt) Outcome {
		return Outcome{State: protocol.AttemptAcceptedUnpublished, Result: "the runner did the work"}
	})
}

// The first completion commits but its answer is lost, the second meets a
// busy database: the worker must keep going until the replayed completion
// answers, rather than return and let the sweeper call finished work lost.
func TestALostCompletionAnswerIsRetriedUntilTheReplayAnswers(t *testing.T) {
	fastRetries(t)
	h := newHarness(t)
	_, head, identity := newOriginRepo(t)
	h.seedRun("run-1", protocol.RunTarget{Repository: identity, BaseSHA: head})
	h.enqueue("run-1", identity)

	var mutex sync.Mutex
	completions := 0
	w := newFaultyWorker(t, h, acceptingRunner(), func(path string, _ []byte) fault {
		if !strings.HasSuffix(path, "/complete") {
			return passThrough
		}
		mutex.Lock()
		defer mutex.Unlock()
		completions++
		switch completions {
		case 1:
			return actThenDrop
		case 2:
			return answer503
		}
		return passThrough
	})

	attempt, err := w.ClaimOnce(context.Background())
	if err != nil {
		t.Fatalf("a lost completion answer must be retried, got %v", err)
	}
	if attempt == nil || attempt.State != protocol.AttemptAcceptedUnpublished {
		t.Fatalf("attempt = %+v, want the replayed accepted_unpublished outcome", attempt)
	}
	if completions != 3 {
		t.Fatalf("completion was sent %d times, want 3", completions)
	}
	if state := attemptStateInStore(t, h, attempt.ID); state != protocol.AttemptAcceptedUnpublished {
		t.Fatalf("attempt state is %q, want %q", state, protocol.AttemptAcceptedUnpublished)
	}
}

// A lease verdict is final: repeating the completion cannot change it, so
// the worker stops at once and retains the worktree.
func TestALeaseVerdictOnCompletionIsNotRetried(t *testing.T) {
	fastRetries(t)
	h := newHarness(t)
	_, head, identity := newOriginRepo(t)
	h.seedRun("run-1", protocol.RunTarget{Repository: identity, BaseSHA: head})
	h.enqueue("run-1", identity)

	completions := 0
	w := newFaultyWorker(t, h, acceptingRunner(), func(path string, _ []byte) fault {
		if strings.HasSuffix(path, "/complete") {
			completions++
			return answer409LeaseLost
		}
		return passThrough
	})

	if _, err := w.ClaimOnce(context.Background()); errorCode(err) != "lease_not_owner" {
		t.Fatalf("ClaimOnce error = %v, want the lease_not_owner verdict", err)
	}
	if completions != 1 {
		t.Fatalf("completion was sent %d times after a lease verdict, want 1", completions)
	}
}

// The first claim commits but its answer is lost. The resend must carry the
// same request id and token so the server replays the committed claim; a new
// pair would claim nothing new and leave the first attempt preparing under a
// token no worker holds.
func TestALostClaimAnswerIsResentWithTheSameRequest(t *testing.T) {
	fastRetries(t)
	h := newHarness(t)
	_, head, identity := newOriginRepo(t)
	h.seedRun("run-1", protocol.RunTarget{Repository: identity, BaseSHA: head})
	h.enqueue("run-1", identity)

	var claims []protocol.ClaimRequest
	w := newFaultyWorker(t, h, acceptingRunner(), func(path string, body []byte) fault {
		if !strings.HasSuffix(path, "/claims") {
			return passThrough
		}
		var request protocol.ClaimRequest
		if err := json.Unmarshal(body, &request); err != nil {
			t.Errorf("decode claim request: %v", err)
		}
		claims = append(claims, request)
		if len(claims) == 1 {
			return actThenDrop
		}
		return passThrough
	})

	attempt, err := w.ClaimOnce(context.Background())
	if err != nil {
		t.Fatalf("a lost claim answer must be resent, got %v", err)
	}
	if attempt == nil || attempt.State != protocol.AttemptAcceptedUnpublished {
		t.Fatalf("attempt = %+v, want the replayed claim run to accepted_unpublished", attempt)
	}
	if len(claims) != 2 {
		t.Fatalf("claim was sent %d times, want 2", len(claims))
	}
	if claims[0] != claims[1] {
		t.Fatalf("resent claim %+v differs from the original %+v; the server cannot replay it", claims[1], claims[0])
	}
	var attempts int
	if err := h.db.QueryRow(`SELECT COUNT(*) FROM attempts`).Scan(&attempts); err != nil {
		t.Fatalf("count attempts: %v", err)
	}
	if attempts != 1 {
		t.Fatalf("store holds %d attempts, want 1 — the resend claimed afresh", attempts)
	}
}

// The first completion commits and its answer is lost. Before the retry, the
// heartbeat — still running, as it must during retries — meets the now
// terminal attempt and is told attempt_transitioned. That verdict is the
// commit the worker is retrying, not a reason to stop: the retry must still
// be sent and reach the server's replay of the stored outcome.
func TestACompletionRetryOutlivesAHeartbeatThatSeesTheCommit(t *testing.T) {
	fastRetries(t)
	interval := heartbeatInterval
	heartbeatInterval = 10 * time.Millisecond
	completionRetryDelays = []time.Duration{100 * time.Millisecond, 100 * time.Millisecond}
	t.Cleanup(func() { heartbeatInterval = interval })

	h := newHarness(t)
	_, head, identity := newOriginRepo(t)
	h.seedRun("run-1", protocol.RunTarget{Repository: identity, BaseSHA: head})
	h.enqueue("run-1", identity)

	var mutex sync.Mutex
	completions, heartbeatsBetween := 0, 0
	w := newFaultyWorker(t, h, acceptingRunner(), func(path string, _ []byte) fault {
		mutex.Lock()
		defer mutex.Unlock()
		switch {
		case strings.HasSuffix(path, "/heartbeat"):
			if completions == 1 {
				heartbeatsBetween++
			}
		case strings.HasSuffix(path, "/complete"):
			completions++
			if completions == 1 {
				return actThenDrop
			}
		}
		return passThrough
	})

	attempt, err := w.ClaimOnce(context.Background())
	if err != nil {
		t.Fatalf("a completion retry must survive attempt_transitioned, got %v", err)
	}
	if attempt == nil || attempt.State != protocol.AttemptAcceptedUnpublished {
		t.Fatalf("attempt = %+v, want the replayed accepted_unpublished outcome", attempt)
	}
	mutex.Lock()
	defer mutex.Unlock()
	if heartbeatsBetween == 0 {
		t.Fatalf("no heartbeat landed between the dropped answer and the retry; the test proves nothing")
	}
	if completions != 2 {
		t.Fatalf("completion was sent %d times, want 2", completions)
	}
}
