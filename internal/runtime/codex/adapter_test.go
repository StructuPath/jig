// adapter_test.go — the Codex adapter against a scripted stub CLI: probed
// capability flags, create-or-continue argument shape, stdin prompt delivery,
// exact environment pass-through, stream normalization, per-send usage delta,
// and process-group kill. The claudecode adapter's hard-won cases are ported
// here in full, because the process shape is the same shape and every one of
// those bugs is reachable from this adapter too. The real CLI is exercised by
// live_test.go (JIG_LIVE_SMOKE_CODEX=1).
package codex

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

// The adapter is one of the engine's runtimes or it is nothing.
var _ runtime.Runtime = (*Adapter)(nil)

// stubScript stands in for the CLI. It answers `--version` and
// `exec resume --help` the way codex-cli 0.146.0 does, so the probe is
// exercised against the real shapes, and it echoes back the thread id it was
// told to resume so session continuity is observable; STUB_MODE=coldstart
// makes it answer under a different thread instead.
const stubScript = `#!/bin/sh
case "$1" in
  --version) echo "codex-cli ${STUB_VERSION:-9.9.9}"; exit 0 ;;
esac
if [ "$1" = "exec" ] && [ "$2" = "resume" ] && [ "$3" = "--help" ]; then
  if [ -n "$STUB_NO_RESUME" ]; then
    echo "error: unrecognized subcommand 'resume'" >&2
    exit 2
  fi
  echo "Usage: codex exec resume [OPTIONS] [SESSION_ID] [PROMPT]"
  exit 0
fi
printf '%s\n' "$@" > "$STUB_DIR/args"
RESUMING=0
for ARGUMENT in "$@"; do
  if [ "$ARGUMENT" = "resume" ]; then RESUMING=1; fi
done
THREAD="${STUB_THREAD:-thread-created}"
if [ "$RESUMING" = "1" ]; then
  PREVIOUS=""
  for ARGUMENT in "$@"; do
    if [ "$ARGUMENT" = "-" ]; then THREAD="$PREVIOUS"; fi
    PREVIOUS="$ARGUMENT"
  done
fi
cat > "$STUB_DIR/prompt"
env > "$STUB_DIR/environment"
IN="${STUB_IN:-100}"
OUT="${STUB_OUT:-20}"
started() { printf '{"type":"thread.started","thread_id":"%s"}\n{"type":"turn.started"}\n' "$1"; }
completed() {
  printf '{"type":"turn.completed","usage":{"input_tokens":%s,"cached_input_tokens":40,"cache_write_input_tokens":0,"output_tokens":%s,"reasoning_output_tokens":5}}\n' "$IN" "$OUT"
}
case "$STUB_MODE" in
  hang)
    sleep 60 ;;
  failedturn)
    started "$THREAD"
    printf '%s\n' '{"type":"turn.failed","error":{"message":"boom"}}' ;;
  noresult)
    started "$THREAD" ;;
  nonzero)
    started "$THREAD"
    printf '%s\n' '{"type":"item.completed","item":{"id":"item_0","type":"agent_message","text":"partial"}}'
    completed
    echo "codex fell over" >&2
    exit 3 ;;
  orphan)
    # A descendant that inherits stdout and outlives the CLI — an MCP server,
    # a backgrounded tool process. The write end of the stream stays open
    # after the CLI itself has exited and delivered its terminal turn.
    sleep 60 &
    started "$THREAD"
    printf '%s\n' '{"type":"item.completed","item":{"id":"item_0","type":"agent_message","text":"the CLI exited first"}}'
    completed ;;
  coldstart)
    started "a-different-thread"
    printf '%s\n' '{"type":"item.completed","item":{"id":"item_0","type":"agent_message","text":"cold start"}}'
    completed ;;
  flood)
    PAD=xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx
    PAD="$PAD$PAD$PAD$PAD"
    started "$THREAD"
    COUNT=0
    while [ "$COUNT" -lt 399 ]; do
      printf '{"type":"item.completed","item":{"id":"item_%s","type":"agent_message","text":"%s"}}\n' "$COUNT" "$PAD"
      COUNT=$((COUNT+1))
    done
    printf '%s\n' '{"type":"item.completed","item":{"id":"item_last","type":"agent_message","text":"survived the flood"}}'
    completed ;;
  *)
    started "$THREAD"
    printf '%s\n' '{"type":"item.started","item":{"id":"item_0","type":"command_execution"}}'
    printf '%s\n' '{"type":"item.completed","item":{"id":"item_0","type":"command_execution","command":"ls","exit_code":0,"status":"completed"}}'
    printf '%s\n' '{"type":"item.completed","item":{"id":"item_1","type":"error","message":"a notice, not a verdict"}}'
    printf '%s\n' '{"type":"item.completed","item":{"id":"item_2","type":"agent_message","text":"hello from stub"}}'
    completed ;;
esac
`

func writeStub(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "codex-stub")
	if err := os.WriteFile(path, []byte(stubScript), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func stubEnv(stubDir, mode string, extra ...string) []string {
	return append([]string{
		"PATH=/usr/bin:/bin",
		"STUB_DIR=" + stubDir,
		"STUB_MODE=" + mode,
		"MARKER=present",
	}, extra...)
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

func drainQuietly(handle runtime.Handle) {
	for range handle.Events() {
	}
}

func TestProbeReportsVersionAndProbedCapabilityFlags(t *testing.T) {
	adapter := NewWithExecutable(writeStub(t))
	capability, err := adapter.Probe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if capability.Name != RuntimeName || capability.Version != "9.9.9" {
		t.Fatalf("capability = %+v", capability)
	}
	if !capability.CanResume {
		t.Error("a CLI whose `exec resume --help` names SESSION_ID can resume")
	}
	// Codex reports tokens and no dollars anywhere in the exec stream, so
	// advertising cost would be advertising a fabrication.
	if capability.ReportsCost {
		t.Error("codex reports no cost; reports_cost must be false")
	}
}

// The flag is measured, not declared: a build without non-interactive resume
// must say so, because can_resume=false is what routes the engine onto the
// transcript-digest replay path instead of losing every correction (R7).
func TestProbeReportsCanResumeFalseWhenTheCLILacksTheResumeSubcommand(t *testing.T) {
	t.Setenv("STUB_NO_RESUME", "1")
	adapter := NewWithExecutable(writeStub(t))
	capability, err := adapter.Probe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if capability.CanResume {
		t.Fatal("a CLI without `exec resume` must report can_resume=false, not a hopeful true")
	}
}

func TestFirstSendCreatesTheThreadAndTheSecondResumesIt(t *testing.T) {
	adapter := NewWithExecutable(writeStub(t))
	stubDir := t.TempDir()
	workDir := t.TempDir()
	session := &runtime.Session{Key: "attempt-writer"}
	opts := runtime.Options{
		SystemPrompt: "be careful",
		Model:        "test-model",
		WorkDir:      workDir,
		Env:          stubEnv(stubDir, "ok", "STUB_IN=100", "STUB_OUT=20"),
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

	// Codex mints the id; the session learns it from the stream.
	if session.NativeID != "thread-created" || session.Sends != 1 {
		t.Fatalf("session identity not established from the stream: %+v", session)
	}
	argText := readStubFile(t, stubDir, "args")
	for _, want := range []string{"exec", "--json", "--skip-git-repo-check",
		"--dangerously-bypass-approvals-and-sandbox", "--model", "test-model"} {
		if !strings.Contains(argText, want) {
			t.Errorf("first-send args missing %q:\n%s", want, argText)
		}
	}
	if strings.Contains(argText, "resume") {
		t.Error("first send must create, not resume")
	}
	if !strings.HasSuffix(strings.TrimRight(argText, "\n"), "\n-") {
		t.Errorf("the prompt must be read from stdin (trailing `-`):\n%s", argText)
	}
	// No --system-prompt flag exists, so it rides in front of the prompt.
	prompt := readStubFile(t, stubDir, "prompt")
	if !strings.HasPrefix(prompt, "be careful") || !strings.HasSuffix(prompt, "first prompt") {
		t.Errorf("prompt arrived as %q, want the system prompt ahead of the text", prompt)
	}
	environment := readStubFile(t, stubDir, "environment")
	if !strings.Contains(environment, "MARKER=present") {
		t.Error("opts.Env was not passed through")
	}
	if strings.Contains(environment, "GOPATH=") {
		t.Error("the worker's own environment leaked into the subprocess")
	}

	if result.Text != "hello from stub" || result.SessionID != "thread-created" || result.IsError {
		t.Fatalf("result = %+v", result)
	}
	if result.Usage.InputTokens != 100 || result.Usage.OutputTokens != 20 ||
		result.Usage.TotalTokens != 120 {
		t.Fatalf("usage = %+v", result.Usage)
	}
	if result.Usage.CostUSD != 0 || result.Usage.ContextTokens != 0 {
		t.Fatalf("usage = %+v, want zeros where codex reports nothing", result.Usage)
	}
	var kinds []string
	for _, event := range events {
		kinds = append(kinds, event.Kind+":"+event.Name)
	}
	joined := strings.Join(kinds, ",")
	for _, want := range []string{
		runtime.EventLog + ":thread_started",
		runtime.EventLog + ":started:command_execution",
		runtime.EventToolCall + ":command_execution",
		runtime.EventLog + ":item_error",
		runtime.EventText + ":",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("normalized stream missing %q: %s", want, joined)
		}
	}

	// Second send: the same thread continues via `exec resume <id>`, and the
	// cumulative counts the CLI reports come back as this send's delta.
	opts.Env = stubEnv(stubDir, "ok", "STUB_IN=250", "STUB_OUT=45")
	handle, err = adapter.StartOrContinue(context.Background(), session, "second prompt", opts)
	if err != nil {
		t.Fatal(err)
	}
	drain(t, handle)
	result, err = handle.Result()
	if err != nil {
		t.Fatal(err)
	}
	if session.Sends != 2 {
		t.Fatalf("session sends = %d, want 2", session.Sends)
	}
	argText = readStubFile(t, stubDir, "args")
	if !strings.Contains(argText, "exec\nresume\n") {
		t.Errorf("second-send args missing `exec resume`:\n%s", argText)
	}
	if !strings.Contains(argText, "\nthread-created\n-") {
		t.Errorf("second-send args missing the thread id before the stdin marker:\n%s", argText)
	}
	// `codex exec resume` rejects --color outright; sending it fails the send.
	if strings.Contains(argText, "--color") {
		t.Errorf("resume must not carry --color, which the subcommand rejects:\n%s", argText)
	}
	if strings.HasPrefix(readStubFile(t, stubDir, "prompt"), "be careful") {
		t.Error("the system prompt is already in the live thread; resending it fights the context")
	}
	if result.Usage.InputTokens != 150 || result.Usage.OutputTokens != 25 ||
		result.Usage.TotalTokens != 175 {
		t.Fatalf("usage = %+v, want the per-send delta of codex's cumulative totals",
			result.Usage)
	}
}

func readStubFile(t *testing.T, stubDir, name string) string {
	t.Helper()
	content, err := os.ReadFile(filepath.Join(stubDir, name))
	if err != nil {
		t.Fatal(err)
	}
	return string(content)
}

// A role that declared a tool allowlist against a runtime with no way to
// enforce one must fail loudly. Dropping it silently would run a bypassed
// agent with a wider reach than the definition asked for.
func TestADeclaredToolAllowlistFailsTheSendRatherThanBeingDropped(t *testing.T) {
	adapter := NewWithExecutable(writeStub(t))
	_, err := adapter.StartOrContinue(context.Background(), &runtime.Session{Key: "attempt-tools"}, "p",
		runtime.Options{WorkDir: t.TempDir(), Env: stubEnv(t.TempDir(), "ok"), Tools: []string{"Write"}})
	if !errors.Is(err, ErrToolAllowlistUnsupported) {
		t.Fatalf("error = %v, want ErrToolAllowlistUnsupported", err)
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
	groupID := handle.ProcessGroupID()
	if groupID <= 0 {
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
	// The anchor is a member of the group it leads; kill takes it too.
	waitForGroupExit(t, groupID, 5*time.Second)
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
// SIGINT and nothing else is left to stop an approvals-bypassed agent that is
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

// A resume that silently starts a new thread loses the whole conversation. It
// gets its own diagnostic instead of N context-free corrections downstream.
func TestAResumeAnsweredUnderADifferentThreadIsSurfaced(t *testing.T) {
	adapter := NewWithExecutable(writeStub(t))
	stubDir := t.TempDir()
	session := &runtime.Session{Key: "attempt-resume", NativeID: "thread-we-asked-to-resume", Sends: 1}
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
	for _, want := range []string{"thread-we-asked-to-resume", "a-different-thread"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("diagnostic %q does not name %q", err, want)
		}
	}
	if result.SessionID != "a-different-thread" {
		t.Fatalf("result = %+v, want the CLI's own thread id recorded", result)
	}
}

// The same check must stay quiet on the healthy path: a resume answered under
// the thread it was given is continuity, not a diagnostic.
func TestAResumeAnsweredUnderTheSameThreadIsNotADiagnostic(t *testing.T) {
	adapter := NewWithExecutable(writeStub(t))
	stubDir := t.TempDir()
	session := &runtime.Session{Key: "attempt-resume-ok", NativeID: "live-thread", Sends: 1}
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
	if result.SessionID != "live-thread" {
		t.Fatalf("result = %+v, want the resumed thread id", result)
	}
}

func TestTurnFailedIsSurfacedAsTheRuntimesOwnErrorVerdict(t *testing.T) {
	adapter := NewWithExecutable(writeStub(t))
	stubDir := t.TempDir()
	handle, err := adapter.StartOrContinue(context.Background(),
		&runtime.Session{Key: "attempt-err"}, "p",
		runtime.Options{WorkDir: t.TempDir(), Env: stubEnv(stubDir, "failedturn")})
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

func TestCleanExitWithoutATerminalTurnEventIsAnError(t *testing.T) {
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
		!strings.Contains(err.Error(), "no terminal turn event") {
		t.Fatalf("error = %v, want the missing-turn diagnostic", err)
	}
}

// A nonzero exit is a phase death with a cause, and the cause is on stderr —
// the CLI's own diagnostic must reach the trace, not just "exit status 3".
func TestANonzeroExitSurfacesWithItsStderrTail(t *testing.T) {
	adapter := NewWithExecutable(writeStub(t))
	stubDir := t.TempDir()
	handle, err := adapter.StartOrContinue(context.Background(),
		&runtime.Session{Key: "attempt-exit"}, "p",
		runtime.Options{WorkDir: t.TempDir(), Env: stubEnv(stubDir, "nonzero")})
	if err != nil {
		t.Fatal(err)
	}
	drainQuietly(handle)
	result, err := handle.Result()
	if err == nil {
		t.Fatal("a nonzero exit must not be reported as a successful send")
	}
	for _, want := range []string{"exit status 3", "codex fell over"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("diagnostic %q does not carry %q", err, want)
		}
	}
	if result.ExitCode != 3 {
		t.Fatalf("result = %+v, want the process exit status recorded", result)
	}
}

// Result's own contract is "wait for the subprocess to exit and the stream to
// drain", so a caller is entitled to call it while it is still reading the
// stream. Cmd.Wait closes the read end of an exec-managed stdout pipe the
// moment the process exits, so waiting before the reader is finished cuts it
// off mid-stream and loses whatever is still in the pipe — including the
// terminal `turn.completed` line, which then surfaces as "codex returned no
// terminal turn event": a phase failure whose cause appears nowhere in the
// transcript.
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
	texts := 0
	for event := range handle.Events() {
		if event.Kind == runtime.EventText {
			texts++
		}
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
	if texts != 400 {
		t.Errorf("the stream delivered %d text events, want all 400 the CLI wrote", texts)
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

// Interface conformance: the sequence the engine actually drives a runtime
// through (U4's scripted suite in miniature) — probe at worker start, a
// creating send, a correction that must land in the SAME live session, and a
// watchdog kill — run end to end against the adapter with a stub CLI. If the
// engine's contract and this adapter ever disagree, it fails here rather than
// in a live run.
func TestTheAdapterHonoursTheEngineRuntimeContract(t *testing.T) {
	adapter := NewWithExecutable(writeStub(t))
	stubDir := t.TempDir()
	workDir := t.TempDir()

	capability, err := adapter.Probe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if capability.Name != RuntimeName || capability.Version == "" {
		t.Fatalf("registration would carry %+v", capability)
	}

	// The engine holds one Session per role for the attempt's life and passes
	// the same pointer back on every send (KTD5).
	session := &runtime.Session{Key: "attempt-1/writer"}
	opts := runtime.Options{WorkDir: workDir, Env: stubEnv(stubDir, "ok")}
	for send := 1; send <= 3; send++ {
		handle, startErr := adapter.StartOrContinue(
			context.Background(), session, "prompt", opts)
		if startErr != nil {
			t.Fatalf("send %d: %v", send, startErr)
		}
		if handle.ProcessGroupID() <= 0 {
			t.Fatalf("send %d: no process group for the attempt manifest", send)
		}
		drain(t, handle)
		result, resultErr := handle.Result()
		if resultErr != nil {
			t.Fatalf("send %d: %v", send, resultErr)
		}
		if result.SessionID != "thread-created" {
			t.Fatalf("send %d: correction left the live session: %+v", send, result)
		}
		if session.Sends != send {
			t.Fatalf("send %d: session sends = %d", send, session.Sends)
		}
		// Repeat calls are idempotent — the engine's error path may call
		// Result again after a drain.
		if _, again := handle.Result(); again != nil {
			t.Fatalf("send %d: second Result call reported %v", send, again)
		}
	}
	// Only the first send created; the rest resumed the same thread.
	if !strings.Contains(readStubFile(t, stubDir, "args"), "exec\nresume\n") {
		t.Error("corrections must resume the live thread, not cold start")
	}

	// And the watchdog path: a hung send is killed and reports ErrKilled.
	hung, err := adapter.StartOrContinue(context.Background(),
		&runtime.Session{Key: "attempt-1/hung"}, "prompt",
		runtime.Options{WorkDir: workDir, Env: stubEnv(t.TempDir(), "hang")})
	if err != nil {
		t.Fatal(err)
	}
	_ = hung.Kill()
	drainQuietly(hung)
	if _, err := hung.Result(); !errors.Is(err, runtime.ErrKilled) {
		t.Fatalf("killed send reported %v, want ErrKilled", err)
	}
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
// with no cause at all — "exit status 3:" and nothing after it.
func TestDrainStreamWaitsForTheStderrCaptureBeforeClosingIt(t *testing.T) {
	stdoutReader, stdoutWriter, err := os.Pipe()
	if err != nil {
		t.Fatalf("open stdout pipe: %v", err)
	}
	stderrReader, stderrWriter, err := os.Pipe()
	if err != nil {
		t.Fatalf("open stderr pipe: %v", err)
	}
	handle := &codexHandle{
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
	if _, err := io.WriteString(stderrWriter, "codex fell over"); err != nil {
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

	if got := handle.stderrTail.String(); got != "codex fell over" {
		t.Fatalf("stderr tail = %q, want the CLI's diagnostic intact", got)
	}
}

func TestEffortBecomesARootReasoningOverrideOnBothShapes(t *testing.T) {
	adapter := NewWithExecutable("codex")
	for _, test := range []struct{ effort, want string }{
		{"low", `model_reasoning_effort="low"`},
		{"xhigh", `model_reasoning_effort="xhigh"`},
		{"max", `model_reasoning_effort="xhigh"`},
	} {
		for _, nativeID := range []string{"", "thread-1"} {
			arguments, _ := adapter.arguments(
				&runtime.Session{NativeID: nativeID}, runtime.Options{Effort: test.effort})
			if len(arguments) < 3 || arguments[0] != "-c" || arguments[1] != test.want ||
				arguments[2] != "exec" {
				t.Errorf("effort %q (resume=%v): args = %q, want -c %s ahead of exec",
					test.effort, nativeID != "", arguments, test.want)
			}
		}
	}
	arguments, _ := adapter.arguments(&runtime.Session{}, runtime.Options{})
	if arguments[0] != "exec" {
		t.Errorf("an unset effort must add no override, got %q", arguments)
	}
}
