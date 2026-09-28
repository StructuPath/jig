// reconcile_test.go — the process-group half of startup reconciliation (U4's
// read side): a crashed worker's agent process groups are stopped, and a
// recorded group whose pid no longer matches what was recorded is never
// signalled.
package worker

import (
	"context"
	"encoding/json"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
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
	if manifest.ProcessActive || len(manifest.ProcessGroups) != 0 {
		t.Fatal("the manifest still advertises a live process group after it was stopped")
	}
}

// A parallel reviewer group runs several agents at once. The manifest keeps
// the SET of live groups — recording one never overwrites another, and
// clearing one leaves the rest — and reconciliation stops every group a
// crashed worker left in it.
func TestReconcileStopsEveryLiveGroupOfAParallelGroup(t *testing.T) {
	h := newHarness(t)
	dataDir := filepath.Join(t.TempDir(), "worker")
	w, attemptID := claimOneAttempt(t, h, dataDir)

	first, second, finished := startGroupLeader(t), startGroupLeader(t), startGroupLeader(t)
	for _, groupID := range []int{first, finished, second} {
		if err := w.RecordProcessGroup(attemptID, int64(groupID), true); err != nil {
			t.Fatalf("record process group %d: %v", groupID, err)
		}
	}
	if err := w.RecordProcessGroup(attemptID, int64(finished), false); err != nil {
		t.Fatalf("clear process group: %v", err)
	}
	manifest, err := w.manifests.load(attemptID)
	if err != nil {
		t.Fatal(err)
	}
	want := []int64{int64(first), int64(second)}
	if want[0] > want[1] {
		want[0], want[1] = want[1], want[0]
	}
	if len(manifest.ProcessGroups) != 2 || manifest.ProcessGroups[0] != want[0] || manifest.ProcessGroups[1] != want[1] {
		t.Fatalf("recorded groups = %v, want exactly the two still live %v", manifest.ProcessGroups, want)
	}

	restarted := newTestWorker(t, h, dataDir, 1, RunnerFunc(
		func(context.Context, *PreparedAttempt) Outcome {
			return Outcome{State: protocol.AttemptFailed, Error: "unused"}
		}))
	report, err := restarted.Reconcile(context.Background())
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(report.StoppedProcessGroups) != 2 {
		t.Fatalf("reconcile stopped %v, want both live members' groups %v", report.StoppedProcessGroups, want)
	}
	waitForProcessExit(t, first, 15*time.Second)
	waitForProcessExit(t, second, 15*time.Second)
	if err := syscall.Kill(-finished, 0); err != nil {
		t.Fatalf("reconcile stopped a group the manifest had already cleared: %v", err)
	}
	after, err := restarted.manifests.load(attemptID)
	if err != nil {
		t.Fatal(err)
	}
	if len(after.ProcessGroups) != 0 {
		t.Fatalf("the manifest still records %v after reconciliation", after.ProcessGroups)
	}
}

// preSetManifest is the manifest shape jig wrote before the process-group
// set, decoded as that version did: strictly, unknown fields refused.
type preSetManifest struct {
	SchemaVersion   int       `json:"schema_version"`
	WorkerID        string    `json:"worker_id"`
	JobID           string    `json:"job_id"`
	AttemptID       string    `json:"attempt_id"`
	AttemptNumber   int       `json:"attempt_number"`
	Repository      string    `json:"repository"`
	RepositoryDir   string    `json:"repository_dir"`
	BaseSHA         string    `json:"base_sha"`
	WorktreePath    string    `json:"worktree_path"`
	Branch          string    `json:"branch"`
	ProcessGroupID  int64     `json:"process_group_id,omitempty"`
	ProcessActive   bool      `json:"process_active"`
	Lifecycle       string    `json:"lifecycle"`
	TerminalState   string    `json:"terminal_state,omitempty"`
	RetentionReason string    `json:"retention_reason,omitempty"`
	CleanupIntent   string    `json:"cleanup_intent,omitempty"`
	CleanupResult   string    `json:"cleanup_result,omitempty"`
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
}

// A worker rolled back to a jig that predates the set still sees a live
// group: the legacy fields always name the lowest live one. With one group
// live the manifest is exactly the old shape; with several, the legacy
// fields still name the lowest.
func TestAnOlderReaderStillSeesALiveProcessGroup(t *testing.T) {
	h := newHarness(t)
	dataDir := filepath.Join(t.TempDir(), "worker")
	w, attemptID := claimOneAttempt(t, h, dataDir)
	path := filepath.Join(dataDir, "attempts", attemptID+".json")
	readOld := func() (preSetManifest, error) {
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var old preSetManifest
		return old, decodeStrictJSON(body, &old)
	}

	if err := w.RecordProcessGroup(attemptID, 5100, true); err != nil {
		t.Fatal(err)
	}
	old, err := readOld()
	if err != nil {
		t.Fatalf("an older reader cannot read a one-group manifest: %v", err)
	}
	if !old.ProcessActive || old.ProcessGroupID != 5100 {
		t.Fatalf("older reader sees active=%v group=%d, want the live group 5100", old.ProcessActive, old.ProcessGroupID)
	}

	for _, groupID := range []int64{5300, 5200} {
		if err := w.RecordProcessGroup(attemptID, groupID, true); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.RecordProcessGroup(attemptID, 5100, false); err != nil {
		t.Fatal(err)
	}
	current, err := w.manifests.load(attemptID)
	if err != nil {
		t.Fatal(err)
	}
	if !current.ProcessActive || current.ProcessGroupID != 5200 {
		t.Fatalf("legacy fields = active %v group %d, want the lowest live group 5200",
			current.ProcessActive, current.ProcessGroupID)
	}

	if err := w.RecordProcessGroup(attemptID, 5200, false); err != nil {
		t.Fatal(err)
	}
	if old, err = readOld(); err != nil || !old.ProcessActive || old.ProcessGroupID != 5300 {
		t.Fatalf("older reader after the set shrank to one: %+v, %v; want active group 5300", old, err)
	}
	if err := w.RecordProcessGroup(attemptID, 5300, false); err != nil {
		t.Fatal(err)
	}
	if old, err = readOld(); err != nil || old.ProcessActive {
		t.Fatalf("older reader after every group ended: %+v, %v; want nothing live", old, err)
	}
}

// Concurrent records — members starting at once — all land.
func TestConcurrentProcessGroupRecordsAllLand(t *testing.T) {
	h := newHarness(t)
	dataDir := filepath.Join(t.TempDir(), "worker")
	w, attemptID := claimOneAttempt(t, h, dataDir)
	const members = 8
	errs := make(chan error, members)
	for i := 0; i < members; i++ {
		go func(groupID int64) { errs <- w.RecordProcessGroup(attemptID, groupID, true) }(int64(1000 + i))
	}
	for i := 0; i < members; i++ {
		if err := <-errs; err != nil {
			t.Fatalf("record: %v", err)
		}
	}
	manifest, err := w.manifests.load(attemptID)
	if err != nil {
		t.Fatal(err)
	}
	if len(manifest.ProcessGroups) != members {
		t.Fatalf("recorded %v, want all %d concurrent records", manifest.ProcessGroups, members)
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
		ours, reason := processGroupIsOurs(context.Background(), manifest, groupID)
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

// The signal path narrows the recorded int64 to an int before negating it. On
// a 32-bit build that narrowing truncates, and the truncated values land
// exactly on the ids the range gate exists to refuse — 2^32 becomes 0, 2^32+1
// becomes 1 — so a value that does not survive the round trip must be refused
// outright instead of silently reinterpreted as some other group.
func TestAProcessGroupIdThatCannotSurviveTheSignalPathIsRefused(t *testing.T) {
	// Every one of these is >= 2 (so the range gate alone lets it through) and
	// truncates to an id at or below 1 on a 32-bit build.
	for _, groupID := range []int64{
		1 << 32,           // truncates to 0 — jig's own process group
		1<<32 + 1,         // truncates to 1 — kill(-1, ...), the whole session
		math.MaxInt32 + 1, // truncates to a negative id that names nothing of ours
		math.MaxInt64,     // truncates to -1, which negates into pid 1
	} {
		signallable := signallableProcessGroup(groupID)
		// The invariant holds on both word sizes: a recorded id is signallable
		// only if the id that reaches kill(2) is the id that was recorded.
		if signallable && int64(int(groupID)) != groupID {
			t.Errorf("process group %d is treated as signallable but reaches "+
				"the syscall as %d", groupID, int(groupID))
		}
		// And on a 32-bit build none of them may pass at all.
		if want := strconv.IntSize >= 64; signallable != want {
			t.Errorf("signallableProcessGroup(%d) = %v on a %d-bit int, want %v",
				groupID, signallable, strconv.IntSize, want)
		}
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
	// The set is held to the same bar, and never lists a group twice.
	for _, groups := range [][]int64{{4242, 1}, {0}, {-1}, {4242, 4242}} {
		manifest := base
		manifest.ProcessGroups = groups
		if err := store.validate(manifest); err == nil {
			t.Errorf("a manifest recording process groups %v validated", groups)
		}
	}
	set := base
	set.ProcessGroups = []int64{4242, 4343}
	if err := store.validate(set); err != nil {
		t.Errorf("a set of real live process groups must stay valid: %v", err)
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
	// Without this the test proves nothing it claims to: an absent field leaves
	// worktreePath empty, os.Stat("") fails, and the failure is reported as
	// "reconcile deleted the worktree" — naming the wrong cause.
	worktreePath, ok := raw["worktree_path"].(string)
	if !ok || worktreePath == "" {
		t.Fatalf("the manifest carries no worktree_path to check: %v", raw["worktree_path"])
	}
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
