// adapter_test.go — the Claude Code adapter against a scripted stub CLI:
// probe parsing, create-or-continue argument shape, stdin prompt delivery,
// exact environment pass-through, stream capture, and process-group kill.
// The real CLI is exercised by the engine's live smoke (JIG_LIVE_SMOKE=1).
package claudecode

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/StructuPath/jig/internal/runtime"
)

const stubScript = `#!/bin/sh
case "$1" in
  --version) echo "9.9.9 (Claude Code)"; exit 0 ;;
esac
printf '%s\n' "$@" > "$STUB_DIR/args"
cat > "$STUB_DIR/prompt"
env > "$STUB_DIR/environment"
case "$STUB_MODE" in
  hang)
    sleep 60 ;;
  error)
    printf '%s\n' '{"type":"result","result":"boom","is_error":true,"session_id":"sid-err"}' ;;
  noresult)
    printf '%s\n' '{"type":"system","subtype":"init"}' ;;
  *)
    printf '%s\n' '{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Write","input":{"path":"x.txt"}},{"type":"text","text":"working on it"}]}}'
    printf '%s\n' '{"type":"result","result":"hello from stub","is_error":false,"total_cost_usd":0.5,"session_id":"sid-123","usage":{"input_tokens":5,"output_tokens":7,"cache_read_input_tokens":2}}'
    ;;
esac
`

func writeStub(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "claude-stub")
	if err := os.WriteFile(path, []byte(stubScript), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func stubEnv(stubDir, mode string) []string {
	return []string{
		"PATH=/usr/bin:/bin",
		"STUB_DIR=" + stubDir,
		"STUB_MODE=" + mode,
		"MARKER=present",
	}
}

func drain(t *testing.T, handle runtime.Handle) []runtime.Event {
	t.Helper()
	var events []runtime.Event
	deadline := time.After(10 * time.Second)
	for {
		select {
		case event, open := <-handle.Events():
			if !open {
				return events
			}
			events = append(events, event)
		case <-deadline:
			t.Fatal("event stream did not close")
		}
	}
}

func TestProbeReportsVersionAndCapabilityFlags(t *testing.T) {
	adapter := NewWithExecutable(writeStub(t))
	capability, err := adapter.Probe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if capability.Name != RuntimeName || capability.Version != "9.9.9" {
		t.Fatalf("capability = %+v", capability)
	}
	if !capability.CanResume || !capability.ReportsCost {
		t.Fatalf("claude-code must advertise can-resume and reports-cost: %+v", capability)
	}
}

func TestFirstSendCreatesTheSessionAndTheSecondResumesIt(t *testing.T) {
	adapter := NewWithExecutable(writeStub(t))
	stubDir := t.TempDir()
	workDir := t.TempDir()
	session := &runtime.Session{Key: "attempt-writer"}
	opts := runtime.Options{
		SystemPrompt: "be careful",
		Model:        "test-model",
		WorkDir:      workDir,
		Env:          stubEnv(stubDir, "ok"),
	}

	handle, err := adapter.StartOrContinue(context.Background(), session, "first prompt", opts)
	if err != nil {
		t.Fatal(err)
	}
	events := drain(t, handle)
	result, err := handle.Result()
	if err != nil {
		t.Fatal(err)
	}

	if session.NativeID == "" || session.Sends != 1 {
		t.Fatalf("session identity not established: %+v", session)
	}
	arguments, err := os.ReadFile(filepath.Join(stubDir, "args"))
	if err != nil {
		t.Fatal(err)
	}
	argText := string(arguments)
	for _, want := range []string{"--print", "stream-json", "--permission-mode",
		"--session-id", session.NativeID, "--model", "test-model", "--system-prompt"} {
		if !strings.Contains(argText, want) {
			t.Errorf("first-send args missing %q:\n%s", want, argText)
		}
	}
	if strings.Contains(argText, "--resume") {
		t.Error("first send must create, not resume")
	}
	prompt, _ := os.ReadFile(filepath.Join(stubDir, "prompt"))
	if string(prompt) != "first prompt" {
		t.Errorf("prompt arrived as %q, want stdin delivery of the exact text", prompt)
	}
	environment, _ := os.ReadFile(filepath.Join(stubDir, "environment"))
	if !strings.Contains(string(environment), "MARKER=present") {
		t.Error("opts.Env was not passed through")
	}
	if strings.Contains(string(environment), "GOPATH=") {
		t.Error("the worker's own environment leaked into the subprocess")
	}

	if result.Text != "hello from stub" || result.SessionID != "sid-123" || result.IsError {
		t.Fatalf("result = %+v", result)
	}
	if result.Usage.CostUSD != 0.5 || result.Usage.TotalTokens != 12 || result.Usage.ContextTokens != 14 {
		t.Fatalf("usage = %+v", result.Usage)
	}
	var kinds []string
	for _, event := range events {
		kinds = append(kinds, event.Kind+":"+event.Name)
	}
	joined := strings.Join(kinds, ",")
	if !strings.Contains(joined, runtime.EventToolCall+":Write") {
		t.Errorf("tool_call event missing: %s", joined)
	}

	// Second send: same session continues via --resume.
	handle, err = adapter.StartOrContinue(context.Background(), session, "second prompt", opts)
	if err != nil {
		t.Fatal(err)
	}
	drain(t, handle)
	if _, err := handle.Result(); err != nil {
		t.Fatal(err)
	}
	if session.Sends != 2 {
		t.Fatalf("session sends = %d, want 2", session.Sends)
	}
	arguments, _ = os.ReadFile(filepath.Join(stubDir, "args"))
	if !strings.Contains(string(arguments), "--resume\n"+session.NativeID) &&
		!strings.Contains(string(arguments), "--resume") {
		t.Errorf("second-send args missing --resume:\n%s", arguments)
	}
	if strings.Contains(string(arguments), "--session-id") {
		t.Error("second send must resume, not create")
	}
}

func TestKillStopsTheWholeProcessGroupAndResultReportsKilled(t *testing.T) {
	adapter := NewWithExecutable(writeStub(t))
	stubDir := t.TempDir()
	session := &runtime.Session{Key: "attempt-hang"}
	handle, err := adapter.StartOrContinue(context.Background(), session, "hang please",
		runtime.Options{WorkDir: t.TempDir(), Env: stubEnv(stubDir, "hang")})
	if err != nil {
		t.Fatal(err)
	}
	if handle.ProcessGroupID() <= 0 {
		t.Fatal("no process group to record into the manifest")
	}
	// Give the stub a moment to start, then kill the group.
	time.Sleep(200 * time.Millisecond)
	finished := make(chan struct{})
	var resultErr error
	go func() {
		defer close(finished)
		_ = handle.Kill()
		drainQuietly(handle)
		_, resultErr = handle.Result()
	}()
	select {
	case <-finished:
	case <-time.After(15 * time.Second):
		t.Fatal("kill did not stop the process group in time")
	}
	if !errors.Is(resultErr, runtime.ErrKilled) {
		t.Fatalf("result error = %v, want ErrKilled", resultErr)
	}
}

func drainQuietly(handle runtime.Handle) {
	for range handle.Events() {
	}
}

func TestTerminalErrorResultIsSurfaced(t *testing.T) {
	adapter := NewWithExecutable(writeStub(t))
	stubDir := t.TempDir()
	handle, err := adapter.StartOrContinue(context.Background(),
		&runtime.Session{Key: "attempt-err"}, "p",
		runtime.Options{WorkDir: t.TempDir(), Env: stubEnv(stubDir, "error")})
	if err != nil {
		t.Fatal(err)
	}
	drainQuietly(handle)
	result, err := handle.Result()
	if err != nil {
		t.Fatal(err)
	}
	if !result.IsError || result.Text != "boom" {
		t.Fatalf("result = %+v, want the runtime's own error verdict surfaced", result)
	}
}

func TestCleanExitWithoutAResultEventIsAnError(t *testing.T) {
	adapter := NewWithExecutable(writeStub(t))
	stubDir := t.TempDir()
	handle, err := adapter.StartOrContinue(context.Background(),
		&runtime.Session{Key: "attempt-nores"}, "p",
		runtime.Options{WorkDir: t.TempDir(), Env: stubEnv(stubDir, "noresult")})
	if err != nil {
		t.Fatal(err)
	}
	drainQuietly(handle)
	if _, err := handle.Result(); err == nil ||
		!strings.Contains(err.Error(), "no terminal result event") {
		t.Fatalf("error = %v, want the missing-result diagnostic", err)
	}
}
