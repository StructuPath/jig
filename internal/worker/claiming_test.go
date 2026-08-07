// claiming_test.go — what happens to an attempt whose local manifest cannot
// be written while it is in flight. The manifest is the worker's durable
// memory, but it is not the record of truth for the operator: the control
// plane is. Losing the ability to write one must not lose the other.
package worker

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/StructuPath/jig/internal/controlplane"
	"github.com/StructuPath/jig/internal/protocol"
)

// newHookedWorker builds a worker whose control plane fires before(path) on
// every request — the seam for making a local manifest write fail at an exact
// point in the attempt's life, which no in-process API otherwise reaches.
func newHookedWorker(
	t *testing.T, h *harness, dataDir string, runner AttemptRunner, before func(path string),
) *Worker {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	inner := controlplane.NewHandler(h.store, "", logger)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		before(request.URL.Path)
		inner.ServeHTTP(writer, request)
	}))
	t.Cleanup(server.Close)
	w, err := New(Config{
		ServerURL: server.URL,
		DataDir:   dataDir,
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

// sealAttemptsDirectory makes every later manifest write fail, the way a full
// disk, a revoked permission, or a read-only mount would. Reads keep working,
// so this isolates the write path.
func sealAttemptsDirectory(t *testing.T, dataDir string) {
	t.Helper()
	attempts := filepath.Join(dataDir, "attempts")
	if err := os.Chmod(attempts, 0o500); err != nil {
		t.Fatalf("seal attempts directory: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(attempts, 0o700) })
}

func attemptStateInStore(t *testing.T, h *harness, attemptID string) string {
	t.Helper()
	var state string
	if err := h.db.QueryRow(`SELECT state FROM attempts WHERE id = ?`, attemptID).Scan(&state); err != nil {
		t.Fatalf("read attempt state: %v", err)
	}
	return state
}

func retainedLedgerEntry(t *testing.T, w *Worker, attemptID string) protocol.WorktreeLedgerEntry {
	t.Helper()
	ledger, err := w.client.Worktrees(context.Background())
	if err != nil {
		t.Fatalf("read ledger: %v", err)
	}
	for _, entry := range ledger {
		if entry.AttemptID == attemptID {
			return entry
		}
	}
	t.Fatalf("no ledger row for attempt %s; the worktree it left behind is invisible to the operator", attemptID)
	return protocol.WorktreeLedgerEntry{}
}

// The runner finished and produced an outcome. If the manifest write that
// follows it fails, the outcome is the one thing that must not be dropped:
// discarding it leaves the attempt running until the sweeper calls it lost,
// and the job then reports failed even though the runner did the work.
func TestAManifestWriteFailureAfterTheRunnerStillReportsTheOutcomeAndRetains(t *testing.T) {
	h := newHarness(t)
	dataDir := filepath.Join(t.TempDir(), "worker")
	_, head, identity := newOriginRepo(t)
	h.seedRun("run-1", protocol.RunTarget{Repository: identity, BaseSHA: head})
	h.enqueue("run-1", identity)

	var attemptID, worktreePath string
	runner := RunnerFunc(func(_ context.Context, attempt *PreparedAttempt) Outcome {
		attemptID = attempt.Claim.Attempt.ID
		worktreePath = attempt.WorktreePath
		// The disk goes read-only while the attempt is running.
		sealAttemptsDirectory(t, dataDir)
		// A bare runner has no publish pipeline wrapped around it, so the
		// strongest outcome it can honestly declare is accepted_unpublished —
		// the control plane refuses `accepted` without a proof record (R12).
		return Outcome{State: protocol.AttemptAcceptedUnpublished, Result: "the runner did the work"}
	})
	w := newHookedWorker(t, h, dataDir, runner, func(string) {})

	if _, err := w.ClaimOnce(context.Background()); err == nil {
		t.Fatal("a failed manifest write must still surface as an error")
	}
	if state := attemptStateInStore(t, h, attemptID); state != protocol.AttemptAcceptedUnpublished {
		t.Fatalf("attempt state is %q, want %q — the runner's outcome was dropped when the "+
			"manifest write failed", state, protocol.AttemptAcceptedUnpublished)
	}
	entry := retainedLedgerEntry(t, w, attemptID)
	if entry.State != protocol.WorktreeRetained || !strings.Contains(entry.Reason, "could not record completion") {
		t.Fatalf("ledger row = %+v, want a retained worktree naming the manifest failure", entry)
	}
	if _, err := os.Stat(worktreePath); err != nil {
		t.Fatalf("the surviving work was deleted under manifest doubt: %v", err)
	}
}

// The mirror case: the attempt has already started server-side when the
// manifest write fails, so it is already the control plane's problem. It must
// be given a terminal state rather than abandoned mid-flight, and its
// worktree must be retained rather than left with no ledger record.
func TestAManifestWriteFailureBeforeTheRunnerTerminatesTheAttemptAndRetains(t *testing.T) {
	h := newHarness(t)
	dataDir := filepath.Join(t.TempDir(), "worker")
	_, head, identity := newOriginRepo(t)
	h.seedRun("run-1", protocol.RunTarget{Repository: identity, BaseSHA: head})
	h.enqueue("run-1", identity)

	ranAttempts := 0
	runner := RunnerFunc(func(context.Context, *PreparedAttempt) Outcome {
		ranAttempts++
		return Outcome{State: protocol.AttemptAcceptedUnpublished}
	})
	// The seal lands between the start call and the running-lifecycle write
	// that follows it — the one window where the attempt is live server-side
	// and the worker cannot record it locally.
	w := newHookedWorker(t, h, dataDir, runner, func(path string) {
		if strings.HasSuffix(path, "/start") {
			sealAttemptsDirectory(t, dataDir)
		}
	})

	if _, err := w.ClaimOnce(context.Background()); err == nil {
		t.Fatal("a failed manifest write must still surface as an error")
	}
	if ranAttempts != 0 {
		t.Fatalf("the runner ran %d times after the worker lost its manifest", ranAttempts)
	}
	var attemptID string
	if err := h.db.QueryRow(`SELECT id FROM attempts LIMIT 1`).Scan(&attemptID); err != nil {
		t.Fatalf("read attempt: %v", err)
	}
	if state := attemptStateInStore(t, h, attemptID); state != protocol.AttemptFailed {
		t.Fatalf("attempt state is %q, want %q — an attempt the worker abandoned stays running "+
			"until the sweeper calls it lost", state, protocol.AttemptFailed)
	}
	entry := retainedLedgerEntry(t, w, attemptID)
	if entry.State != protocol.WorktreeRetained ||
		!strings.Contains(entry.Reason, "could not record the running lifecycle") {
		t.Fatalf("ledger row = %+v, want a retained worktree naming the manifest failure", entry)
	}
}
