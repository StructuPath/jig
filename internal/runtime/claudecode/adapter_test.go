// adapter_test.go — the Claude Code adapter against a scripted stub CLI:
// probe parsing, create-or-continue argument shape, stdin prompt delivery,
// exact environment pass-through, stream capture, and process-group kill.
// The real CLI is exercised by the engine's live smoke (JIG_LIVE_SMOKE=1).
package claudecode

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/StructuPath/jig/internal/protocol"
	"github.com/StructuPath/jig/internal/runtime"
)

// stubScript stands in for the CLI. It echoes back the session id it was
// given (--session-id or --resume), the way the real CLI does, so session
// continuity is observable; STUB_MODE=coldstart makes it answer under a
// different session instead. STUB_TOTAL_COST is the session's running cost
// total the result reports, the way the real CLI reports total_cost_usd.
const stubScript = `#!/bin/sh
case "$1" in
  --version) echo "9.9.9 (Claude Code)"; exit 0 ;;
esac
printf '%s\n' "$@" > "$STUB_DIR/args"
SESSION=""
PREVIOUS=""
for ARGUMENT in "$@"; do
  case "$PREVIOUS" in
    --session-id|--resume) SESSION="$ARGUMENT" ;;
  esac
  PREVIOUS="$ARGUMENT"
done
cat > "$STUB_DIR/prompt"
env > "$STUB_DIR/environment"
case "$STUB_MODE" in
  hang)
    sleep 60 ;;
  error)
    printf '%s\n' '{"type":"result","result":"boom","is_error":true,"session_id":"sid-err"}' ;;
  noresult)
    printf '%s\n' '{"type":"system","subtype":"init"}' ;;
  orphan)
    # A descendant that inherits stdout and outlives the CLI — an MCP server,
    # a backgrounded tool process. The write end of the stream stays open
    # after the CLI itself has exited and delivered its terminal result.
    sleep 60 &
    printf '{"type":"result","result":"the CLI exited first","is_error":false,"session_id":"%s"}\n' "$SESSION" ;;
  coldstart)
    printf '{"type":"result","result":"cold start","is_error":false,"session_id":"a-different-session"}\n' ;;
  flood)
    PAD=xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx
    PAD="$PAD$PAD$PAD$PAD"
    COUNT=0
    while [ "$COUNT" -lt 400 ]; do
      printf '{"type":"assistant","message":{"content":[{"type":"text","text":"%s"}]}}\n' "$PAD"
      COUNT=$((COUNT+1))
    done
    printf '{"type":"result","result":"survived the flood","is_error":false,"session_id":"%s"}\n' "$SESSION" ;;
  *)
    printf '%s\n' '{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Write","input":{"path":"x.txt"}},{"type":"text","text":"working on it"}]}}'
    printf '{"type":"result","result":"hello from stub","is_error":false,"total_cost_usd":%s,"session_id":"%s","usage":{"input_tokens":5,"output_tokens":7,"cache_read_input_tokens":2}}\n' "${STUB_TOTAL_COST:-0.5}" "$SESSION"
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

	if result.Text != "hello from stub" || result.SessionID != session.NativeID || result.IsError {
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

	// Second send: same session continues via --resume. The CLI reports the
	// session's running total, so the send's own cost is the difference.
	opts.Env = append(opts.Env, "STUB_TOTAL_COST=1.25")
	handle, err = adapter.StartOrContinue(context.Background(), session, "second prompt", opts)
	if err != nil {
		t.Fatal(err)
	}
	drain(t, handle)
	result, err = handle.Result()
	if err != nil {
		t.Fatal(err)
	}
	if result.Usage.CostUSD != 0.75 || session.ReportedCostUSD != 1.25 {
		t.Fatalf("resumed send cost = %v (session total %v), want 0.75 of 1.25",
			result.Usage.CostUSD, session.ReportedCostUSD)
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

// waitForGroupExit polls a process group until nothing in it remains.
func waitForGroupExit(t *testing.T, groupID int64, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(int(-groupID), 0); err != nil {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("process group %d still has live members", groupID)
}

// waitForStub blocks until the stub CLI has actually started.
func waitForStub(t *testing.T, stubDir string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(filepath.Join(stubDir, "prompt")); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("the stub CLI never started")
}

// Cancelling the send's context is the signal path: `jig run` cancels on
// SIGINT and nothing else is left to stop a bypassPermissions agent that is
// editing the operator's repository in place.
func TestCancellingTheContextStopsTheWholeProcessGroupIncludingTheAnchor(t *testing.T) {
	adapter := NewWithExecutable(writeStub(t))
	stubDir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	handle, err := adapter.StartOrContinue(ctx, &runtime.Session{Key: "attempt-ctx"}, "hang please",
		runtime.Options{WorkDir: t.TempDir(), Env: stubEnv(stubDir, "hang")})
	if err != nil {
		t.Fatal(err)
	}
	groupID := handle.ProcessGroupID()
	if groupID <= 0 {
		t.Fatal("no process group to stop")
	}
	waitForStub(t, stubDir)

	cancel()
	finished := make(chan error, 1)
	go func() {
		drainQuietly(handle)
		_, resultErr := handle.Result()
		finished <- resultErr
	}()
	select {
	case resultErr := <-finished:
		if !errors.Is(resultErr, runtime.ErrKilled) {
			t.Fatalf("result error = %v, want ErrKilled", resultErr)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("cancelling the context left the send (and its process group) running")
	}
	waitForGroupExit(t, groupID, 5*time.Second)
}

// The anchor is the group's dead-man's switch: if jig dies without
// unwinding, the closed watchdog pipe must take the anchor AND its group
// down. Otherwise every interrupted send leaks an immortal anchor.
func TestTheProcessGroupAnchorDiesWithItsParentAndTakesTheGroupWithIt(t *testing.T) {
	anchor := exec.Command("/bin/sh", "-c", anchorScript)
	anchor.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	watchdog, err := anchor.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := anchor.Start(); err != nil {
		t.Fatal(err)
	}
	groupID := int64(anchor.Process.Pid)

	// A long-running member of the same group stands in for the agent CLI.
	member := exec.Command("/bin/sh", "-c", "sleep 60")
	member.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pgid: int(groupID)}
	if err := member.Start(); err != nil {
		t.Fatal(err)
	}
	memberExit := make(chan error, 1)
	go func() { memberExit <- member.Wait() }()

	// Closing the watchdog is exactly what jig's death does to it.
	if err := watchdog.Close(); err != nil {
		t.Fatal(err)
	}
	anchorExit := make(chan error, 1)
	go func() { anchorExit <- anchor.Wait() }()
	select {
	case <-anchorExit:
	case <-time.After(20 * time.Second):
		_ = syscall.Kill(int(-groupID), syscall.SIGKILL)
		t.Fatal("the anchor survived its parent — one leaked process per interrupted send")
	}
	select {
	case <-memberExit:
	case <-time.After(20 * time.Second):
		_ = syscall.Kill(int(-groupID), syscall.SIGKILL)
		t.Fatal("the anchor died without stopping the rest of its group")
	}
	waitForGroupExit(t, groupID, 5*time.Second)
}

// A --resume that silently cold starts loses the whole conversation. It gets
// its own diagnostic instead of N context-free corrections downstream.
func TestAResumeAnsweredUnderADifferentSessionIsSurfaced(t *testing.T) {
	adapter := NewWithExecutable(writeStub(t))
	stubDir := t.TempDir()
	session := &runtime.Session{Key: "attempt-resume", NativeID: "session-we-asked-to-resume", Sends: 1}
	handle, err := adapter.StartOrContinue(context.Background(), session, "correct this",
		runtime.Options{WorkDir: t.TempDir(), Env: stubEnv(stubDir, "coldstart")})
	if err != nil {
		t.Fatal(err)
	}
	drainQuietly(handle)
	result, err := handle.Result()
	if !errors.Is(err, ErrSessionDiscontinuity) {
		t.Fatalf("error = %v, want ErrSessionDiscontinuity", err)
	}
	for _, want := range []string{"session-we-asked-to-resume", "a-different-session"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("diagnostic %q does not name %q", err, want)
		}
	}
	if result.SessionID != "a-different-session" {
		t.Fatalf("result = %+v, want the CLI's own session id recorded", result)
	}
}

// The same check must stay quiet on the healthy path: a resume answered
// under the session it was given is continuity, not a diagnostic.
func TestAResumeAnsweredUnderTheSameSessionIsNotADiagnostic(t *testing.T) {
	adapter := NewWithExecutable(writeStub(t))
	stubDir := t.TempDir()
	session := &runtime.Session{Key: "attempt-resume-ok", NativeID: "live-session", Sends: 1}
	handle, err := adapter.StartOrContinue(context.Background(), session, "correct this",
		runtime.Options{WorkDir: t.TempDir(), Env: stubEnv(stubDir, "ok")})
	if err != nil {
		t.Fatal(err)
	}
	drainQuietly(handle)
	result, err := handle.Result()
	if err != nil {
		t.Fatalf("a faithful resume reported %v", err)
	}
	if result.SessionID != "live-session" {
		t.Fatalf("result = %+v, want the resumed session id", result)
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

// Result's own contract is "wait for the subprocess to exit and the stream to
// drain", so a caller is entitled to call it while it is still reading the
// stream. Cmd.Wait closes the read end of the stdout pipe the moment the
// process exits, so waiting before the reader is finished cuts it off
// mid-stream and loses whatever is still in the pipe — including the terminal
// `result` line, which then surfaces as "claude returned no terminal result
// event": a phase failure whose cause appears nowhere in the transcript.
func TestTheTerminalResultSurvivesAResultCallThatOverlapsTheStream(t *testing.T) {
	adapter := NewWithExecutable(writeStub(t))
	stubDir := t.TempDir()
	handle, err := adapter.StartOrContinue(context.Background(),
		&runtime.Session{Key: "attempt-flood"}, "flood please",
		runtime.Options{WorkDir: t.TempDir(), Env: stubEnv(stubDir, "flood")})
	if err != nil {
		t.Fatal(err)
	}

	type answer struct {
		result runtime.Result
		err    error
	}
	answers := make(chan answer, 1)
	go func() {
		result, resultErr := handle.Result()
		answers <- answer{result, resultErr}
	}()

	// The consumer arrives late: by now the CLI has written everything it will
	// write and exited, and more output is outstanding than any single buffer
	// between it and the reader holds.
	time.Sleep(500 * time.Millisecond)
	events := 0
	for range handle.Events() {
		events++
	}

	select {
	case got := <-answers:
		if got.err != nil {
			t.Fatalf("result error = %v, want the terminal result the CLI actually sent", got.err)
		}
		if got.result.Text != "survived the flood" {
			t.Fatalf("result = %+v, want the terminal result the CLI actually sent", got.result)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("Result never returned")
	}
	if events != 400 {
		t.Errorf("the stream delivered %d events, want all 400 the CLI wrote", events)
	}
}

// The other half of that contract: waiting for the stream must not be the way
// the send learns the CLI is gone. A descendant that inherits stdout — an MCP
// server, a backgrounded tool process — holds the write end open after the
// CLI exits, so EOF never arrives on its own. Waiting on it left Result
// blocked on a CLI that had already delivered its terminal result, with the
// process group still alive and nothing left to stop it.
func TestResultReturnsWhenADescendantOutlivesTheCLIHoldingTheStream(t *testing.T) {
	adapter := NewWithExecutable(writeStub(t))
	stubDir := t.TempDir()
	handle, err := adapter.StartOrContinue(context.Background(),
		&runtime.Session{Key: "attempt-orphan"}, "p",
		runtime.Options{WorkDir: t.TempDir(), Env: stubEnv(stubDir, "orphan")})
	if err != nil {
		t.Fatal(err)
	}
	groupID := handle.ProcessGroupID()
	if groupID <= 0 {
		t.Fatal("no process group to stop")
	}

	type answer struct {
		result runtime.Result
		err    error
	}
	answers := make(chan answer, 1)
	go func() {
		result, resultErr := handle.Result()
		answers <- answer{result, resultErr}
	}()
	go drainQuietly(handle)

	select {
	case got := <-answers:
		if got.err != nil {
			t.Fatalf("result error = %v, want the terminal result the CLI sent before exiting", got.err)
		}
		if got.result.Text != "the CLI exited first" {
			t.Fatalf("result = %+v, want the terminal result the CLI sent before exiting", got.result)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("Result never returned: it is still waiting for a stdout EOF " +
			"that a descendant of the exited CLI will not deliver")
	}
	// And the group goes with it — the descendant does not outlive the send.
	waitForGroupExit(t, groupID, 5*time.Second)
}

// gatedReader withholds its first read until released. It makes the
// scheduling window deterministic: the stderr capture provably has not
// consumed the pipe at the moment drainStream runs, which is the ordering
// that only shows up on a loaded machine.
type gatedReader struct {
	reader io.Reader
	gate   chan struct{}
	opened bool
}

func (g *gatedReader) Read(p []byte) (int, error) {
	if !g.opened {
		<-g.gate
		g.opened = true
	}
	return g.reader.Read(p)
}

// The CLI's diagnostic survives even when stdout's capture finishes first.
// Closing both read ends on stdout alone cuts the stderr copy off with the
// bytes still sitting in the pipe, and a nonzero exit then reaches the trace
// with no cause at all.
func TestDrainStreamWaitsForTheStderrCaptureBeforeClosingIt(t *testing.T) {
	stdoutReader, stdoutWriter, err := os.Pipe()
	if err != nil {
		t.Fatalf("open stdout pipe: %v", err)
	}
	stderrReader, stderrWriter, err := os.Pipe()
	if err != nil {
		t.Fatalf("open stderr pipe: %v", err)
	}
	handle := &claudeHandle{
		done:         make(chan struct{}),
		stderrDone:   make(chan struct{}),
		stdoutReader: stdoutReader,
		stderrReader: stderrReader,
		stderrTail:   &tailBuffer{limit: protocol.MaxErrorBytes},
	}
	gate := make(chan struct{})
	go handle.captureStderr(&gatedReader{reader: stderrReader, gate: gate})

	// The shape a dying CLI leaves behind: its diagnostic is unread in the
	// pipe and the write end went with the process.
	if _, err := io.WriteString(stderrWriter, "claude fell over"); err != nil {
		t.Fatalf("write stderr: %v", err)
	}
	_ = stderrWriter.Close()
	// stdout's capture finishes first — the ordering that loses the tail.
	_ = stdoutWriter.Close()
	close(handle.done)

	drained := make(chan struct{})
	go func() { handle.drainStream(); close(drained) }()

	// Long enough that a drainStream which closes on stdout alone has already
	// done so; the release then finds a dead descriptor instead of the tail.
	time.Sleep(50 * time.Millisecond)
	close(gate)
	<-drained

	if got := handle.stderrTail.String(); got != "claude fell over" {
		t.Fatalf("stderr tail = %q, want the CLI's diagnostic intact", got)
	}
}

func TestEffortIsPassedOnlyWhenTheRoleSetsIt(t *testing.T) {
	adapter := NewWithExecutable(writeStub(t))
	for _, effort := range []string{"high", ""} {
		stubDir := t.TempDir()
		opts := runtime.Options{
			Model: "opus", Effort: effort, WorkDir: t.TempDir(), Env: stubEnv(stubDir, "ok"),
		}
		handle, err := adapter.StartOrContinue(
			context.Background(), &runtime.Session{Key: "effort"}, "prompt", opts)
		if err != nil {
			t.Fatal(err)
		}
		drain(t, handle)
		if _, err := handle.Result(); err != nil {
			t.Fatal(err)
		}
		arguments, err := os.ReadFile(filepath.Join(stubDir, "args"))
		if err != nil {
			t.Fatal(err)
		}
		argText := string(arguments)
		if effort != "" && !strings.Contains(argText, "--effort\n"+effort+"\n") {
			t.Errorf("args missing --effort %s:\n%s", effort, argText)
		}
		if effort == "" && strings.Contains(argText, "--effort") {
			t.Errorf("an unset effort must leave the CLI default, got:\n%s", argText)
		}
	}
}

func TestVersionWarningFlagsOnlyReleasesOlderThanRecommended(t *testing.T) {
	for version, wantWarning := range map[string]bool{
		RecommendedVersion: false,
		"2.1.283":          false,
		"2.2.0":            false,
		"3.0.0":            false,
		"2.1.279":          true,
		"2.0.999":          true,
		"1.9.0":            true,
		"2.1.279-beta":     true,
		"not-a-version":    false,
		"":                 false,
	} {
		warning := VersionWarning(version)
		if (warning != "") != wantWarning {
			t.Errorf("VersionWarning(%q) = %q, want warning: %v", version, warning, wantWarning)
		}
	}
}

func TestBudgetIsPassedAsMaxBudgetUSDOnlyWhenSet(t *testing.T) {
	adapter := NewWithExecutable(writeStub(t))
	for budget, want := range map[float64]string{2.5: "--max-budget-usd\n2.5\n", 0: ""} {
		stubDir := t.TempDir()
		handle, err := adapter.StartOrContinue(context.Background(), &runtime.Session{Key: "budget"},
			"prompt", runtime.Options{Model: "opus", BudgetUSD: budget, WorkDir: t.TempDir(),
				Env: stubEnv(stubDir, "ok")})
		if err != nil {
			t.Fatal(err)
		}
		drain(t, handle)
		if _, err := handle.Result(); err != nil {
			t.Fatal(err)
		}
		arguments, err := os.ReadFile(filepath.Join(stubDir, "args"))
		if err != nil {
			t.Fatal(err)
		}
		argText := string(arguments)
		if want != "" && !strings.Contains(argText, want) {
			t.Errorf("budget %v: args missing %q:\n%s", budget, want, argText)
		}
		if want == "" && strings.Contains(argText, "--max-budget-usd") {
			t.Errorf("an unset budget must add no flag, got:\n%s", argText)
		}
	}
}
