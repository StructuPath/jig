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
	"errors"
	"fmt"
	"os/exec"
	"strings"
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
//
// env is REQUIRED. A nil env means Go hands the child jig's own os.Environ()
// — every API key the operator exported, and their real HOME — which is
// exactly the containment KTD10 and KTD11 exist to remove. Commands run in
// the worktree an agent just wrote (a `go test` gate executes agent-authored
// code), so inheritance must fail loudly rather than silently return.
func runShellCommand(
	ctx context.Context, dir, command string, env []string, timeout time.Duration,
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

// reportedFields reads a reports_fields code phase's report: its last
// non-empty output line, which must be a JSON object. Everything before it
// is ordinary output — a script may log freely and report once, at the end.
func reportedFields(outputTail string) (map[string]any, error) {
	lines := strings.Split(strings.TrimRight(outputTail, "\n"), "\n")
	last := ""
	for i := len(lines) - 1; i >= 0; i-- {
		if trimmed := strings.TrimSpace(lines[i]); trimmed != "" {
			last = trimmed
			break
		}
	}
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
