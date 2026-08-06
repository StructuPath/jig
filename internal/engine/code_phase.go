// code_phase.go — code phases and the adapter envelope (R8). A code phase
// runs one command from the FROZEN definition (trusted operator content,
// never agent output) and wraps the deterministic result in an envelope
// shaped like sssf's VerifyOutput — so a failing test suite enters the
// repair loop through exactly the same door as a failing agent report. The
// chain executor is the only thing that knows the difference.
package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"syscall"
	"time"

	"github.com/StructuPath/jig/internal/protocol"
)

// commandResult is one deterministic command execution: exit status and a
// bounded output tail (the full output belongs to the trace, the tail to
// the envelope an agent can act on).
type commandResult struct {
	ExitCode   int
	TimedOut   bool
	StartError string
	OutputTail string
}

// Passed reports a clean exit.
func (r commandResult) Passed() bool {
	return !r.TimedOut && r.StartError == "" && r.ExitCode == 0
}

// runShellCommand executes one definition command via `sh -c` in dir with
// the given environment and timeout. The command gets its own process group
// so a timeout kills everything it spawned, not just the shell.
func runShellCommand(
	ctx context.Context, dir, command string, env []string, timeout time.Duration,
) commandResult {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	shell := exec.Command("/bin/sh", "-c", command)
	shell.Dir = dir
	if env != nil {
		shell.Env = env
	}
	shell.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	tail := &tailWriter{limit: protocol.MaxCommandOutputTailBytes}
	shell.Stdout = tail
	shell.Stderr = tail
	if err := shell.Start(); err != nil {
		return commandResult{ExitCode: -1, StartError: err.Error()}
	}
	done := make(chan error, 1)
	go func() { done <- shell.Wait() }()
	select {
	case err := <-done:
		result := commandResult{OutputTail: tail.String()}
		if exitError, ok := err.(*exec.ExitError); ok {
			result.ExitCode = exitError.ExitCode()
		} else if err != nil {
			result.ExitCode = -1
			result.StartError = err.Error()
		}
		return result
	case <-ctx.Done():
		_ = syscall.Kill(-shell.Process.Pid, syscall.SIGKILL)
		<-done
		return commandResult{ExitCode: -1, TimedOut: true, OutputTail: tail.String()}
	}
}

// adapterEnvelope wraps a command result as an envelope (R8): the
// sssf:VerifyOutput shape — status, passed, failures — plus the exit status
// and output tail as the evidence a repair-target agent works from.
func adapterEnvelope(command string, result commandResult) parsedEnvelope {
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
	fields := map[string]any{
		"status":      status,
		"summary":     summary,
		"passed":      result.Passed(),
		"failures":    failures,
		"exit_code":   result.ExitCode,
		"output_tail": result.OutputTail,
	}
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
