// crash_reconcile_test.go — a code phase's command outlives a hard-killed
// worker only until the worker restarts. The worker runs as a real child
// process (this test binary re-executed, TestHelperProcess), its code phase
// is a shell loop writing a heartbeat file, and the worker is SIGKILLed while
// that shell is alive: no drain, no cleanup, nothing but the manifest left
// behind. A worker restarted on the SAME data directory must find the group
// in the manifest, stop it, and the heartbeat must stop growing.
package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/StructuPath/jig/internal/controlplane"
	"github.com/StructuPath/jig/internal/protocol"
)

func TestARestartedWorkerStopsTheCodePhaseGroupAHardKilledWorkerLeftRunning(t *testing.T) {
	repo := initRepo(t)
	serverData, workerData, scratch := t.TempDir(), t.TempDir(), t.TempDir()
	scriptRuntime(t)
	base, stopServe := startServe(t, serverData, "--no-github-poll")
	defer stopServe()

	pidFile, heartbeat := filepath.Join(scratch, "pid"), filepath.Join(scratch, "heartbeat")
	release := filepath.Join(scratch, "release")
	source := "name: crash-fixture\nphases:\n  - name: loop\n    kind: code\n    command: |\n" +
		"      echo $$ > '" + pidFile + "'; while [ -d '" + scratch + "' ] && [ ! -e '" + release + "' ]; do " +
		"echo beat >> '" + heartbeat + "'; sleep 0.05; done\n" +
		"acceptance: [all_phases_passed]\n"
	var definition protocol.Definition
	if err := apiCall(context.Background(), base, "POST", "/api/definitions",
		controlplane.DefinitionInput{Source: source}, &definition); err != nil {
		t.Fatal(err)
	}
	if err := apiCall(context.Background(), base, "POST", "/api/runs", controlplane.RunInvocation{
		DefinitionID: definition.ID, Targets: []controlplane.InvocationTarget{{Repository: repo}},
	}, &controlplane.RunView{}); err != nil {
		t.Fatal(err)
	}

	// Defensive cleanup releases the loop through its own file rather than
	// signalling a cached group id, which the OS may have reused by then.
	// Registered before the worker starts so an early failure releases it too.
	t.Cleanup(func() { _ = os.WriteFile(release, nil, 0o600) })
	child, _ := startHelper(t, "jig", "JIG_TEST_HELPER_ARGS="+strings.Join(
		[]string{"worker", "--server", base, "--data", workerData}, "\x1f"))
	group := awaitCommandGroup(t, pidFile)
	awaitManifestGroup(t, workerData, group)

	if err := child.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	_ = child.Wait()
	// The worker is gone; its command is not — that is the orphan.
	if !growing(t, heartbeat) {
		t.Fatal("the code phase's shell died with the worker; the fixture proves nothing")
	}

	ctx, interrupt := context.WithCancel(context.Background())
	stdout, stderr := &syncBuffer{}, &syncBuffer{}
	exited := make(chan int, 1)
	go func() {
		exited <- workerCommand(ctx, []string{"--server", base, "--data", workerData}, stdout, stderr)
	}()
	defer func() {
		interrupt()
		select {
		case <-exited:
		case <-time.After(60 * time.Second):
			t.Error("the restarted worker did not stop")
		}
	}()
	want := "stopped orphaned agent process group: " + strconv.FormatInt(group, 10)
	deadline := time.Now().Add(30 * time.Second)
	for !strings.Contains(stdout.String(), want) {
		if time.Now().After(deadline) {
			t.Fatalf("the restarted worker never reported %q\nstdout:\n%s\nstderr:\n%s", want, stdout, stderr)
		}
		select {
		case code := <-exited:
			t.Fatalf("the restarted worker exited %d\nstderr:\n%s", code, stderr)
		case <-time.After(20 * time.Millisecond):
		}
	}
	if err := syscall.Kill(-int(group), 0); err != syscall.ESRCH {
		t.Fatalf("process group %d still exists after reconciliation (kill 0: %v)", group, err)
	}
	if growing(t, heartbeat) {
		t.Fatal("the heartbeat is still growing after reconciliation")
	}
}

func awaitCommandGroup(t *testing.T, pidFile string) int64 {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		if body, err := os.ReadFile(pidFile); err == nil && strings.HasSuffix(string(body), "\n") {
			group, err := strconv.ParseInt(strings.TrimSpace(string(body)), 10, 64)
			if err != nil {
				t.Fatal(err)
			}
			return group
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("the code phase never started")
	return 0
}

// awaitManifestGroup waits until an attempt manifest records the group live.
func awaitManifestGroup(t *testing.T, workerData string, group int64) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		paths, _ := filepath.Glob(filepath.Join(workerData, "attempts", "*.json"))
		for _, path := range paths {
			var manifest struct {
				ProcessGroups  []int64 `json:"process_groups"`
				ProcessGroupID int64   `json:"process_group_id"`
				ProcessActive  bool    `json:"process_active"`
			}
			body, err := os.ReadFile(path)
			if err != nil || json.Unmarshal(body, &manifest) != nil {
				continue
			}
			if (manifest.ProcessActive && manifest.ProcessGroupID == group) ||
				slices.Contains(manifest.ProcessGroups, group) {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("no attempt manifest recorded process group %d", group)
}

// growing reports whether the file grows over a window several times the
// loop's period.
func growing(t *testing.T, path string) bool {
	t.Helper()
	size := func() int64 {
		info, err := os.Stat(path)
		if err != nil {
			return 0
		}
		return info.Size()
	}
	before := size()
	time.Sleep(500 * time.Millisecond)
	return size() > before
}
