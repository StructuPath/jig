// reconcile_test.go — the process-group half of startup reconciliation (U4's
// read side): a crashed worker's agent process groups are stopped, and a
// recorded group whose pid no longer matches what was recorded is never
// signalled.
package worker

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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

// A recorded group id of 1 becomes kill(-1, SIGTERM) — every process the
// operator's user may signal — and 0 becomes kill(0, ...), jig's own group.
// Neither can ever be an attempt's agent group, so no identity check can
// redeem them and nothing may reach the syscall with one. Three layers say so
// independently: the range predicate the signal path gates on, the identity
// check that never even inspects such a value, and manifest validation that
// refuses to hand one back off disk.
func TestAProcessGroupIdAtOrBelowOneIsNeverSignalled(t *testing.T) {
	for _, groupID := range []int64{-1, 0, 1} {
		if signallableProcessGroup(groupID) {
			t.Errorf("process group %d is treated as signallable; "+
				"stopProcessGroup would negate it into a machine-wide kill", groupID)
		}
		manifest := attemptManifest{ProcessGroupID: groupID, ProcessActive: true}
		ours, reason := processGroupIsOurs(context.Background(), manifest)
		if ours {
			t.Errorf("process group %d was claimed as ours", groupID)
		}
		if !strings.Contains(reason, "can never name an attempt's own group") {
			t.Errorf("process group %d was refused for %q, want the range reason "+
				"(it must be refused before any identity check runs)", groupID, reason)
		}
	}
	// The gate must not be so wide that a real group is skipped.
	if !signallableProcessGroup(2) {
		t.Error("process group 2 is a real, signallable group id")
	}
}

// The same three values must be unrepresentable in a manifest, so a zeroed or
// hand-edited field never becomes a signal target on a later restart.
func TestAManifestCannotCarryAnUnsignallableProcessGroup(t *testing.T) {
	store := newManifestStore(t.TempDir(), "11111111-1111-4111-8111-111111111111")
	base := attemptManifest{
		SchemaVersion: manifestSchemaVersion,
		WorkerID:      store.workerID,
		JobID:         "22222222-2222-4222-8222-222222222222",
		AttemptID:     "33333333-3333-4333-8333-333333333333",
		AttemptNumber: 1,
		Repository:    "github.com/example/repo",
		RepositoryDir: filepath.Join(store.dataDirectory, "repos", "entry"),
		BaseSHA:       strings.Repeat("a", 40),
		Lifecycle:     manifestRunning,
		CreatedAt:     time.Now().UTC(),
		UpdatedAt:     time.Now().UTC(),
	}
	base.WorktreePath = filepath.Join(store.dataDirectory, "worktrees", base.AttemptID)
	base.Branch = attemptBranch(base.JobID, base.AttemptNumber)
	if err := store.validate(base); err != nil {
		t.Fatalf("the fixture manifest is not otherwise valid: %v", err)
	}

	for _, groupID := range []int64{-1, 1} {
		manifest := base
		manifest.ProcessGroupID = groupID
		if err := store.validate(manifest); err == nil {
			t.Errorf("a manifest carrying process group %d validated", groupID)
		}
	}
	// Zero is the unrecorded state and stays legal — but not while the
	// manifest claims a process is live.
	unrecorded := base
	if err := store.validate(unrecorded); err != nil {
		t.Errorf("an unrecorded process group must stay valid: %v", err)
	}
	unrecorded.ProcessActive = true
	if err := store.validate(unrecorded); err == nil {
		t.Error("a manifest advertising a live process with group id 0 validated")
	}
	live := base
	live.ProcessGroupID = 4242
	live.ProcessActive = true
	if err := store.validate(live); err != nil {
		t.Errorf("a real live process group must stay valid: %v", err)
	}
}

// End to end: a manifest that reaches disk carrying process_group_id 1 —
// corruption, a bad merge, an operator's editor — must not make
// reconciliation signal anything, and must not make it delete anything
// either.
func TestReconcileSignalsNothingForACorruptedProcessGroupOnDisk(t *testing.T) {
	h := newHarness(t)
	dataDir := filepath.Join(t.TempDir(), "worker")
	_, attemptID := claimOneAttempt(t, h, dataDir)

	manifestPath := filepath.Join(dataDir, "attempts", attemptID+".json")
	body, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatal(err)
	}
	worktreePath, _ := raw["worktree_path"].(string)
	raw["process_group_id"] = 1
	raw["process_active"] = true
	corrupted, err := json.Marshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifestPath, corrupted, 0o600); err != nil {
		t.Fatal(err)
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
		t.Fatalf("reconcile stopped %v — group id 1 would have signalled every "+
			"process the operator may signal", report.StoppedProcessGroups)
	}
	if _, err := os.Stat(worktreePath); err != nil {
		t.Fatalf("reconcile deleted the worktree behind an unreadable manifest: %v", err)
	}
}
