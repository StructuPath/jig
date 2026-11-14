// code_phase.go — code phases and the adapter envelope (R8). A code phase
// runs one command from the FROZEN definition (trusted operator content,
// never agent output) and wraps the deterministic result in an envelope
// shaped like sssf's VerifyOutput — so a failing test suite enters the
// repair loop through exactly the same door as a failing agent report. The
// chain executor is the only thing that knows the difference.
package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"github.com/StructuPath/JIG-Software-Factory/internal/protocol"
)

// commandResult is one deterministic command execution: exit status and a
// bounded output tail (the full output belongs to the trace, the tail to
// the envelope an agent can act on).
type commandResult struct {
	ExitCode   int
	TimedOut   bool
	StartError string
	OutputTail string
	// LastLine is the last non-empty output line, kept whole (up to
	// MaxReportLineBytes) independently of the bounded tail, so a
	// reports_fields report longer than the tail still parses.
	LastLine         string
	LastLineOverflow bool
	// RecordError is set when the command's process group could not be
	// recorded as live. The command was killed and reaped before it could
	// do anything worth judging, so this is an infrastructure failure, never
	// a verdict on the command.
	RecordError string
}

// Passed reports a clean exit.
func (r commandResult) Passed() bool {
	return !r.TimedOut && r.StartError == "" && r.ExitCode == 0
}

// runShellCommand executes one definition command via `sh -c` in dir with
// the given environment and timeout. The command gets its own process group
// so a timeout kills everything it spawned, not just the shell.
//
// record, when non-nil, is told the group is live right after the start and
// no longer live once the shell has exited or been killed and reaped — the
// same record agent sends keep (U4), so a worker that dies hard mid-command
// leaves a manifest its restart reconciliation can stop the group from. A
// command whose group cannot be recorded does not run on unrecorded: the
// group is killed, reaped, and the record cleared again in case the failed
// write half-landed. Only the group is tracked, through its leader. A
// background process the command leaves running in its group is stopped
// when the shell exits, before the record is cleared, so nothing the
// command started writes the worktree after the phase ends (R8); a process
// that left the group is beyond reach (see stopCommandGroup).
//
// env is REQUIRED. A nil env means Go hands the child jig's own os.Environ()
// — every API key the operator exported, and their real HOME — which is
// exactly the containment KTD10 and KTD11 exist to remove. Commands run in
// the worktree an agent just wrote (a `go test` gate executes agent-authored
// code), so inheritance must fail loudly rather than silently return.
func runShellCommand(
	ctx context.Context, dir, command string, env []string, timeout time.Duration,
	record func(groupID int64, active bool) error,
) commandResult {
	if env == nil {
		return commandResult{ExitCode: -1, StartError: "refusing to run a command with an " +
			"inherited environment: every jig subprocess environment is composed explicitly " +
			"from the role allowlist plus the ephemeral HOME (KTD10, KTD11)"}
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	shell := exec.Command("/bin/sh", "-c", command)
	shell.Dir = dir
	shell.Env = env
	shell.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	tail := &tailWriter{limit: protocol.MaxCommandOutputTailBytes}
	lines := &lastLineWriter{limit: protocol.MaxReportLineBytes}
	output := io.MultiWriter(tail, lines)
	// The output goes through a pipe jig owns, not a Go writer handed to
	// exec, so Wait returns the moment the shell exits instead of waiting for
	// every process holding the pipe. A process the command backgrounded
	// (`server & go test`) holds it after the shell exits; it must be stopped
	// at that moment, not after a WaitDelay during which it could still write
	// the worktree, and the gate must not hang on it either (R8).
	reader, writer, err := os.Pipe()
	if err != nil {
		return commandResult{ExitCode: -1, StartError: err.Error()}
	}
	shell.Stdout = writer
	shell.Stderr = writer
	startErr := shell.Start()
	// The child has its own copy of the write end; jig's must close, or the
	// pipe never reports end of output.
	_ = writer.Close()
	if startErr != nil {
		_ = reader.Close()
		return commandResult{ExitCode: -1, StartError: startErr.Error()}
	}
	copied := make(chan struct{})
	go func() {
		defer close(copied)
		_, _ = io.Copy(output, reader)
	}()
	// drain ends the output copy: at once when every holder of the pipe is
	// gone, else after commandWaitDelay — a process that left the group
	// (setsid) is not stopped and may hold it indefinitely. Closing the read
	// end unblocks the copy, and waiting for it keeps the tail race-free.
	drain := func() {
		select {
		case <-copied:
		case <-time.After(commandWaitDelay):
		}
		_ = reader.Close()
		<-copied
	}
	// Nothing reaps the shell until its group is recorded: an unreaped
	// leader keeps its pid, so the group id cannot be reused by a stranger
	// before the kill below — however early the command exits.
	groupID := int64(shell.Process.Pid)
	if record != nil {
		if err := record(groupID, true); err != nil {
			_ = syscall.Kill(-shell.Process.Pid, syscall.SIGKILL)
			_ = shell.Wait()
			drain()
			_ = record(groupID, false)
			return commandResult{ExitCode: -1, RecordError: err.Error()}
		}
		defer func() { _ = record(groupID, false) }()
	}
	done := make(chan error, 1)
	go func() { done <- shell.Wait() }()
	select {
	case err := <-done:
		// The shell has exited, but a process it backgrounded may still be
		// running in its group. Left alive it could write the worktree after
		// the phase boundary check (R8). Stop the group before the record is
		// cleared and before the result is judged; the shell's own exit
		// status is still the phase result.
		stopCommandGroup(shell.Process.Pid)
		drain()
		last, overflow := lines.Last()
		result := commandResult{OutputTail: tail.String(), LastLine: last, LastLineOverflow: overflow}
		if exitError, ok := err.(*exec.ExitError); ok {
			result.ExitCode = exitError.ExitCode()
		} else if err != nil {
			result.ExitCode = -1
			result.StartError = err.Error()
		}
		return result
	case <-ctx.Done():
		_ = syscall.Kill(-shell.Process.Pid, syscall.SIGKILL)
		// Wait returns once the shell is dead; this bounds the case where
		// the kill itself does not land, so a cancelled phase always returns.
		select {
		case <-done:
		case <-time.After(2 * commandWaitDelay):
		}
		drain()
		return commandResult{ExitCode: -1, TimedOut: true, OutputTail: tail.String()}
	}
}

// commandWaitDelay is how long output is still read after the shell has
// exited and its group been stopped, before the pipe is closed.
const commandWaitDelay = 5 * time.Second

// commandStopGrace is how long a command's leftover group gets to exit on
// SIGTERM before it is killed. It is short on purpose: the group is still
// running after its command has finished, so every moment of grace is a
// moment it can write the worktree, and SIGTERM is not reliable — a shell
// subshell that receives it between fork and exec can lose it (seen with
// macOS /bin/sh), leaving the process it then execs untouched.
const commandStopGrace = 500 * time.Millisecond

// commandKillSettle bounds the wait for a SIGKILLed group to be gone, so
// the command returns after its group has died rather than while it dies.
const commandKillSettle = time.Second

// stopCommandGroup TERMs what is left of a command's process group once the
// shell has exited, waits up to commandStopGrace, then KILLs the rest and
// waits up to commandKillSettle for it to be gone — the same TERM-then-KILL
// the runtime adapters and reconciliation use. ESRCH means the group is
// already empty, which is success.
//
// The shell has been reaped by now, but its pid cannot be reused while any
// process still carries it as a group id, so a non-empty group is still ours.
// An empty group answers ESRCH unless, in the instant since the reap, a new
// process took that pid and made itself a group leader — a window too narrow
// to be worth a second mechanism.
//
// A process that left the group (setsid, setpgid) is out of reach, here and
// on timeout alike: nothing in jig follows it, and it may outlive the phase.
// Containing those needs a session or cgroup-style boundary jig does not
// have.
func stopCommandGroup(groupID int) {
	if groupID <= 1 {
		return
	}
	if err := syscall.Kill(-groupID, syscall.SIGTERM); err != nil {
		return
	}
	if groupGoneWithin(groupID, commandStopGrace) {
		return
	}
	if err := syscall.Kill(-groupID, syscall.SIGKILL); err != nil {
		return
	}
	_ = groupGoneWithin(groupID, commandKillSettle)
}

// groupGoneWithin polls until the process group has no members or the wait
// runs out, and reports which.
func groupGoneWithin(groupID int, wait time.Duration) bool {
	deadline := time.Now().Add(wait)
	for time.Now().Before(deadline) {
		time.Sleep(25 * time.Millisecond)
		if err := syscall.Kill(-groupID, 0); err != nil {
			return true
		}
	}
	return false
}

// reportedFields reads a reports_fields code phase's report: its last
// non-empty output line, which must be a JSON object. Everything before it
// is ordinary output — a script may log freely and report once, at the end.
func reportedFields(lastLine string, overflow bool) (map[string]any, error) {
	if overflow {
		return nil, fmt.Errorf("the last output line exceeds %d bytes", protocol.MaxReportLineBytes)
	}
	last := strings.TrimSpace(lastLine)
	if last == "" {
		return nil, errors.New("the command printed nothing to report")
	}
	var fields map[string]any
	if err := json.Unmarshal([]byte(last), &fields); err != nil {
		return nil, fmt.Errorf("the last output line is not a JSON object: %w", err)
	}
	for key := range fields {
		if adapterReservedFields[key] {
			return nil, fmt.Errorf("reported field %q is reserved for the adapter envelope", key)
		}
	}
	return fields, nil
}

// adapterReservedFields are the envelope fields the adapter owns; a
// reports_fields script may not overwrite the record of its own exit.
var adapterReservedFields = map[string]bool{
	"status": true, "summary": true, "passed": true, "failures": true,
	"exit_code": true, "output_tail": true,
}

// adapterEnvelope wraps a command result as an envelope (R8): the
// sssf:VerifyOutput shape — status, passed, failures — plus the exit status
// and output tail as the evidence a repair-target agent works from, plus
// any fields a reports_fields phase declared.
func adapterEnvelope(command string, result commandResult, reported map[string]any) parsedEnvelope {
	status := protocol.EnvelopeFail
	summary := fmt.Sprintf("command %q exited %d", command, result.ExitCode)
	var failures []string
	switch {
	case result.Passed():
		status = protocol.EnvelopeSuccess
		summary = fmt.Sprintf("command %q exited 0", command)
	case result.TimedOut:
		summary = fmt.Sprintf("command %q timed out", command)
		failures = append(failures, "timed out")
	case result.StartError != "":
		summary = fmt.Sprintf("command %q could not run: %s", command, result.StartError)
		failures = append(failures, result.StartError)
	default:
		failures = append(failures, fmt.Sprintf("exit %d", result.ExitCode))
	}
	fields := make(map[string]any, len(reported)+6)
	for key, value := range reported {
		fields[key] = value
	}
	fields["status"] = status
	fields["summary"] = summary
	fields["passed"] = result.Passed()
	fields["failures"] = failures
	fields["exit_code"] = result.ExitCode
	fields["output_tail"] = result.OutputTail
	raw, err := json.Marshal(fields)
	if err != nil {
		raw = []byte(`{"status":"fail","summary":"adapter envelope encoding failed"}`)
	}
	// Re-parse through the one validation door so adapter envelopes obey the
	// same contract agents are held to.
	envelope, parseErr := validateEnvelope(fields, raw)
	if parseErr != nil {
		envelope = parsedEnvelope{Raw: raw, Fields: fields,
			Base: protocol.Envelope{Status: status, Summary: summary}}
	}
	return envelope
}

// tailWriter keeps the last limit bytes — bounded command output capture.
type tailWriter struct {
	bytes []byte
	limit int
}

func (w *tailWriter) Write(value []byte) (int, error) {
	w.bytes = append(w.bytes, value...)
	if len(w.bytes) > w.limit {
		w.bytes = append([]byte(nil), w.bytes[len(w.bytes)-w.limit:]...)
	}
	return len(value), nil
}

func (w *tailWriter) String() string { return string(w.bytes) }

// lastLineWriter keeps the last non-empty line written to it, whole, up to
// limit bytes. A line longer than limit is not truncated into something that
// might still parse: it is reported as overflowed.
type lastLineWriter struct {
	limit           int
	current         []byte
	currentOverflow bool
	last            []byte
	lastOverflow    bool
}

func (w *lastLineWriter) Write(value []byte) (int, error) {
	for _, b := range value {
		if b == '\n' {
			w.finishLine()
			continue
		}
		if len(w.current) < w.limit {
			w.current = append(w.current, b)
		} else {
			w.currentOverflow = true
		}
	}
	return len(value), nil
}

func (w *lastLineWriter) finishLine() {
	if w.currentOverflow || len(bytes.TrimSpace(w.current)) > 0 {
		w.last = append(w.last[:0], w.current...)
		w.lastOverflow = w.currentOverflow
	}
	w.current = w.current[:0]
	w.currentOverflow = false
}

// Last returns the last non-empty line, counting an unterminated final line.
func (w *lastLineWriter) Last() (string, bool) {
	if w.currentOverflow || len(bytes.TrimSpace(w.current)) > 0 {
		w.finishLine()
	}
	return string(w.last), w.lastOverflow
}
