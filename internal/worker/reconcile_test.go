// reconcile_test.go — the process-group half of startup reconciliation (U4's
// read side): a crashed worker's agent process groups are stopped, and a
// recorded group whose pid no longer matches what was recorded is never
// signalled.
package worker

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/StructuPath/jig/internal/protocol"
)

// claimOneAttempt runs one attempt that leaves unpublished work behind, so
// its manifest and worktree survive for reconciliation to reason about.
func claimOneAttempt(t *testing.T, h *harness, dataDir string) (*Worker, string) {
	t.Helper()
	_, head, identity := newOriginRepo(t)
	h.seedRun("run-1", protocol.RunTarget{Repository: identity, BaseSHA: head})
	h.enqueue("run-1", identity)
	runner := RunnerFunc(func(_ context.Context, attempt *PreparedAttempt) Outcome {
		if err := os.WriteFile(
			filepath.Join(attempt.WorktreePath, "work.txt"), []byte("unpublished\n"), 0o644); err != nil {
			t.Errorf("write work file: %v", err)
		}
		gitRun(t, attempt.WorktreePath, "add", "work.txt")
		gitRun(t, attempt.WorktreePath, "-c", "user.name=jig-test", "-c", "user.email=jig@test",
			"commit", "-m", "unpublished work")
		return Outcome{State: protocol.AttemptFailed, Error: "left unpublished work"}
	})
	w := newTestWorker(t, h, dataDir, 1, runner)
	attempt, err := w.ClaimOnce(context.Background())
	if err != nil || attempt == nil {
		t.Fatalf("claim: attempt=%v err=%v", attempt, err)
	}
	return w, attempt.ID
}

// startGroupLeader starts a long-lived process in its own process group —
// the shape of the adapter's anchor — and returns its pid (the group id).
func startGroupLeader(t *testing.T) int {
	t.Helper()
	command := exec.Command("/bin/sh", "-c", "sleep 60")
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	pid := command.Process.Pid
	exited := make(chan struct{})
	go func() { _ = command.Wait(); close(exited) }()
	t.Cleanup(func() {
		_ = syscall.Kill(-pid, syscall.SIGKILL)
		<-exited
	})
	return pid
}

func waitForProcessExit(t *testing.T, pid int, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(-pid, 0); err != nil {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("process group %d is still running", pid)
}

func TestReconcileStopsTheProcessGroupsACrashedWorkerLeftRunning(t *testing.T) {
	h := newHarness(t)
	dataDir := filepath.Join(t.TempDir(), "worker")
	w, attemptID := claimOneAttempt(t, h, dataDir)

	// U4's write side: the engine records the agent's live process group.
	groupID := startGroupLeader(t)
	if err := w.RecordProcessGroup(attemptID, int64(groupID), true); err != nil {
		t.Fatalf("record process group: %v", err)
	}

	// Worker restart over the same data directory.
	restarted := newTestWorker(t, h, dataDir, 1, RunnerFunc(
		func(context.Context, *PreparedAttempt) Outcome {
			return Outcome{State: protocol.AttemptFailed, Error: "unused"}
		}))
	report, err := restarted.Reconcile(context.Background())
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(report.StoppedProcessGroups) != 1 || report.StoppedProcessGroups[0] != int64(groupID) {
		t.Fatalf("reconcile stopped %v, want [%d]", report.StoppedProcessGroups, groupID)
	}
	waitForProcessExit(t, groupID, 15*time.Second)

	manifest, err := restarted.manifests.load(attemptID)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.ProcessActive {
		t.Fatal("the manifest still advertises a live process group after it was stopped")
	}
}

func TestReconcileNeverSignalsARecordedGroupWhosePidNoLongerMatches(t *testing.T) {
	h := newHarness(t)
	dataDir := filepath.Join(t.TempDir(), "worker")
	w, attemptID := claimOneAttempt(t, h, dataDir)

	// A pid that exists but does not lead the group it was recorded as: the
	// signature of a recycled pid. Killing it would be killing a stranger.
	leader := startGroupLeader(t)
	member := exec.Command("/bin/sh", "-c", "sleep 60")
	member.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pgid: leader}
	if err := member.Start(); err != nil {
		t.Fatal(err)
	}
	memberPid := member.Process.Pid
	memberExit := make(chan struct{})
	go func() { _ = member.Wait(); close(memberExit) }()
	t.Cleanup(func() {
		_ = syscall.Kill(-leader, syscall.SIGKILL)
		<-memberExit
	})

	if err := w.RecordProcessGroup(attemptID, int64(memberPid), true); err != nil {
		t.Fatalf("record process group: %v", err)
	}
	restarted := newTestWorker(t, h, dataDir, 1, RunnerFunc(
		func(context.Context, *PreparedAttempt) Outcome {
			return Outcome{State: protocol.AttemptFailed, Error: "unused"}
		}))
	report, err := restarted.Reconcile(context.Background())
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(report.StoppedProcessGroups) != 0 {
		t.Fatalf("reconcile stopped %v — an unverified process group must never be signalled",
			report.StoppedProcessGroups)
	}
	select {
	case <-memberExit:
		t.Fatal("reconcile killed a process whose identity did not match the manifest")
	case <-time.After(time.Second):
	}
	manifest, err := restarted.manifests.load(attemptID)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.ProcessActive {
		t.Fatal("the manifest still advertises a process group that could not be verified")
	}
}
