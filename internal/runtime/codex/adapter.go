// Package codex is the second real runtime adapter (KTD4): it runs the Codex
// CLI as a supervised subprocess per send, behind the same Runtime interface
// as claudecode, with the differences confined to capability flags and the
// stream normalizer.
//
// Invocation is `codex exec --json` for a creating send and
// `codex exec resume <thread-id> --json` for a continuing one. That is the
// first real difference from Claude Code: Codex mints the session id itself
// and records it under $CODEX_HOME/sessions, so jig cannot pre-assign one.
// The id is learned from the `thread.started` event and stored on the
// Session at Result time; resume is id-keyed, not path-keyed, and works from
// any working directory (verified against codex-cli 0.146.0).
//
// The second difference is cost: `turn.completed` carries token counts and
// no dollar figure, so ReportsCost is false and Usage.CostUSD stays zero
// rather than being invented. The counts themselves are CUMULATIVE for the
// thread — turn 2 reports turn 1's tokens again — so the adapter keeps the
// previous total per thread and reports the per-send delta, because the
// engine sums Usage across sends. Codex reports no context-window occupancy
// at all in the exec stream, so ContextTokens and ContextWindow stay zero.
//
// Everything else is the claudecode adapter's hard-won process shape,
// deliberately duplicated rather than hoisted into shared code: a trap-TERM
// anchor that keeps the group id occupied by our own unreaped child, jig
// owning both stdout and stderr descriptors so Cmd.Wait observes process
// exit alone, wait -> release the group -> drain under a bound, and a
// session-discontinuity check on every resume.
//
// Unix-only, matching jig's supported platforms (macOS and Linux CI).
package codex

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/StructuPath/jig/internal/protocol"
	"github.com/StructuPath/jig/internal/runtime"
)

const (
	// RuntimeName is the probe-reported runtime identity (KTD4).
	RuntimeName = "codex"

	// maxLineBytes bounds one captured JSONL line; the remainder of an
	// overlong line is discarded with a truncation marker, never buffered.
	maxLineBytes = 4 << 20

	// eventChannelDepth bounds the ordered event stream between the reader
	// goroutine and the engine. The engine always drains, so sends block
	// (backpressure) rather than drop.
	eventChannelDepth = 256

	// terminationGrace is how long TERM gets before KILL when stopping the
	// process group.
	terminationGrace = 3 * time.Second

	// streamDrainGrace bounds how long the stream capture may keep running
	// after the CLI has exited and its process group has been stopped. Only a
	// descendant that escaped the group can still hold the write end by then,
	// and no wait on it could ever end on its own.
	streamDrainGrace = 5 * time.Second

	// anchorSelfTerminationGrace is how long the anchor gives its own group
	// after the parent-death watchdog fires, before killing it outright. It
	// lives in the anchor script, so it is written in shell seconds.
	anchorSelfTerminationGrace = "3"

	// maxTrackedThreads bounds the cumulative-usage ledger. Sessions are
	// attempt-scoped (KTD5) and never signal their own end, so the ledger
	// evicts oldest-first rather than growing for the worker's lifetime.
	maxTrackedThreads = 512
)

// anchorSignature marks the anchor's command line so any reconciliation that
// finds a recorded process group can confirm the leader is still OUR anchor
// before signalling it (pids recycle).
const anchorSignature = "jig-process-anchor"

// anchorScript is the process-group anchor: the group leader, and the
// group's dead-man's switch.
//
// It ignores TERM so the group-stop protocol (TERM the group, then KILL)
// reaches the agent's descendants without killing the leader that keeps the
// group id valid. It blocks on stdin — a pipe held open only by jig — so the
// moment jig exits, however it exits (SIGKILL included), the read returns
// EOF and the anchor tears the whole group down on its way out.
var anchorScript = strings.Join([]string{
	"# " + anchorSignature,
	"trap '' TERM",
	"read -r _ 2>/dev/null",
	"kill -TERM 0 2>/dev/null",
	"sleep " + anchorSelfTerminationGrace,
	"kill -KILL 0 2>/dev/null",
}, "\n")

// ErrSessionDiscontinuity reports that a resume send came back under a
// DIFFERENT thread id than the one it was told to continue: the CLI started a
// new thread instead of resuming, so every correction in that send lost the
// conversation. Surfacing it as a send failure costs one phase retry with a
// fresh session; swallowing it costs the whole phase's context and gets
// misreported as "the agent never produced a valid envelope" (R7, R11).
var ErrSessionDiscontinuity = errors.New("codex resumed a different thread than the one requested")

// ErrToolAllowlistUnsupported reports a role that declared a tool allowlist
// against a runtime that has no way to enforce one. `codex exec` has no
// tool-allowlist flag in any form, and the adapter runs the CLI with
// approvals and sandbox bypassed, so quietly dropping the allowlist would
// widen the agent's reach past what the definition asked for. It fails the
// send instead.
var ErrToolAllowlistUnsupported = errors.New(
	"codex exec has no tool allowlist flag; a role tools: list cannot be enforced")

// Adapter runs the Codex CLI. The zero value is not usable; New resolves the
// executable once so agent subprocesses never depend on PATH from their own
// (allowlisted) environment to find the CLI.
type Adapter struct {
	executable string
	usage      usageLedger
}

// New builds an adapter over the installed `codex` executable, resolved
// against the worker's own environment at construction time.
func New() (*Adapter, error) {
	path, err := exec.LookPath("codex")
	if err != nil {
		return nil, fmt.Errorf("codex CLI is not installed: %w", err)
	}
	return &Adapter{executable: path}, nil
}

// NewWithExecutable builds an adapter over an explicit executable path —
// the test seam for scripted stub CLIs.
func NewWithExecutable(path string) *Adapter {
	return &Adapter{executable: path}
}

// Probe records the CLI version and capability flags at worker start (KTD4).
//
// CanResume is measured, not declared: `codex exec resume --help` either
// describes a session-id argument on this build or it does not, and a build
// without it must report can_resume=false so the engine takes the
// transcript-digest replay path (R7) instead of silently losing corrections.
// ReportsCost is false by construction: the `turn.completed` event carries
// token counts only, with no dollar figure anywhere in the exec stream.
func (a *Adapter) Probe(ctx context.Context) (protocol.RuntimeCapability, error) {
	ctx, cancel := context.WithTimeout(ctx, protocol.GitCommandTimeout)
	defer cancel()
	output, err := exec.CommandContext(ctx, a.executable, "--version").Output()
	if err != nil {
		return protocol.RuntimeCapability{}, fmt.Errorf("probe %s: %w", a.executable, err)
	}
	firstLine, _, _ := strings.Cut(strings.TrimSpace(string(output)), "\n")
	version := versionFrom(firstLine)
	if version == "" {
		return protocol.RuntimeCapability{}, fmt.Errorf(
			"probe %s: no version in output %q", a.executable, firstLine)
	}
	return protocol.RuntimeCapability{
		Name:        RuntimeName,
		Version:     version,
		CanResume:   probeResume(ctx, a.executable),
		ReportsCost: false,
	}, nil
}

// versionFrom pulls the version out of `codex --version`, which prints
// "codex-cli 0.146.0" — the number is the last field, not the first.
func versionFrom(line string) string {
	fields := strings.Fields(line)
	if len(fields) == 0 {
		return ""
	}
	return fields[len(fields)-1]
}

// probeResume asks the installed CLI whether non-interactive resume exists on
// this build. A build that lacks the subcommand exits nonzero; a build that
// has it prints a usage line naming the session-id argument.
func probeResume(ctx context.Context, executable string) bool {
	output, err := exec.CommandContext(ctx, executable, "exec", "resume", "--help").CombinedOutput()
	if err != nil {
		return false
	}
	return strings.Contains(string(output), "SESSION_ID")
}

// StartOrContinue sends one prompt into the session. Unlike Claude Code, the
// CLI-native id cannot be minted here — Codex assigns it — so a creating send
// runs with no id and the Session learns it from the stream in Result. The
// subprocess environment is exactly opts.Env (KTD10); nothing from the
// worker's own environment leaks in.
func (a *Adapter) StartOrContinue(
	ctx context.Context, session *runtime.Session, prompt string, opts runtime.Options,
) (runtime.Handle, error) {
	if session == nil {
		return nil, errors.New("session is required")
	}
	if opts.WorkDir == "" {
		return nil, errors.New("work directory is required")
	}
	if len(opts.Tools) > 0 {
		return nil, fmt.Errorf("%w (role asked for %s)",
			ErrToolAllowlistUnsupported, strings.Join(opts.Tools, ","))
	}

	arguments, resumedID := a.arguments(session, opts)
	// The system prompt has no flag on `codex exec`; the whole prompt arrives
	// on stdin as one text. It is prepended on the creating send only, where
	// it becomes the first thing in the conversation — on a resume it is
	// already in the thread, and repeating it would fight the live context.
	if resumedID == "" && opts.SystemPrompt != "" {
		prompt = opts.SystemPrompt + "\n\n---\n\n" + prompt
	}

	// Anchor first: a trap-TERM shell that becomes the group leader. It exists
	// so the group id stays occupied by our own unreaped child for the
	// handle's whole life — signalling -pgid can never hit a recycled pid —
	// and it holds the parent-death watchdog that kills the group if jig dies
	// without unwinding.
	anchor := exec.Command("/bin/sh", "-c", anchorScript)
	anchor.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	watchdog, err := anchor.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("open process-group anchor watchdog: %w", err)
	}
	if err := anchor.Start(); err != nil {
		watchdog.Close()
		return nil, fmt.Errorf("start process-group anchor: %w", err)
	}
	groupID := anchor.Process.Pid
	// abandon tears down a partially built handle: the group dies and the
	// anchor is reaped, so a failed start never leaks either.
	abandon := func() {
		stopGroup(groupID, 0)
		_ = watchdog.Close()
		_ = anchor.Wait()
	}

	command := exec.Command(a.executable, arguments...)
	command.Dir = opts.WorkDir
	command.Env = append([]string(nil), opts.Env...)
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pgid: groupID}
	stdin, err := command.StdinPipe()
	if err != nil {
		abandon()
		return nil, fmt.Errorf("open codex stdin: %w", err)
	}
	// stdout and stderr are descriptors jig owns rather than pipes exec
	// manages, because it is exec's ownership of them that ties process exit
	// to stream EOF in both directions: Cmd.Wait closes the StdoutPipe read
	// end the moment the process exits (cutting the reader off mid-stream and
	// losing the terminal `turn.completed` line), and Cmd.Wait blocks on the
	// copier behind a non-*os.File Stderr, which cannot finish while any
	// descendant still holds the inherited descriptor. With both streams on
	// our own descriptors, Wait observes the CLI's exit and nothing else, and
	// the stream drain is ours to bound.
	stdoutReader, stdoutWriter, err := os.Pipe()
	if err != nil {
		abandon()
		return nil, fmt.Errorf("open codex stdout: %w", err)
	}
	command.Stdout = stdoutWriter
	stderrReader, stderrWriter, err := os.Pipe()
	if err != nil {
		_ = stdoutReader.Close()
		_ = stdoutWriter.Close()
		abandon()
		return nil, fmt.Errorf("open codex stderr: %w", err)
	}
	command.Stderr = stderrWriter
	startErr := command.Start()
	// The child owns the write ends now. The parent's copies must go, or
	// neither stream can ever reach EOF — exec closes only the descriptors it
	// created itself.
	_ = stdoutWriter.Close()
	_ = stderrWriter.Close()
	if startErr != nil {
		_ = stdoutReader.Close()
		_ = stderrReader.Close()
		abandon()
		return nil, fmt.Errorf("start codex: %w", startErr)
	}
	session.Sends++

	handle := &codexHandle{
		adapter:      a,
		command:      command,
		anchor:       anchor,
		watchdog:     watchdog,
		groupID:      groupID,
		session:      session,
		resumedID:    resumedID,
		events:       make(chan runtime.Event, eventChannelDepth),
		done:         make(chan struct{}),
		stderrDone:   make(chan struct{}),
		stopped:      make(chan struct{}),
		stdoutReader: stdoutReader,
		stderrReader: stderrReader,
		stderrTail:   &tailBuffer{limit: protocol.MaxErrorBytes},
	}
	go func() {
		_, writeErr := io.WriteString(stdin, prompt)
		handle.setPromptError(errors.Join(writeErr, stdin.Close()))
	}()
	go handle.consume(stdoutReader)
	go handle.captureStderr(stderrReader)
	// The send dies with its context. Without this, a cancelled context left
	// the CLI running with nobody waiting on it — an agent editing the
	// operator's repository past the end of the run that authorized it.
	go func() {
		select {
		case <-ctx.Done():
			_ = handle.Kill()
		case <-handle.done:
		case <-handle.stopped:
		}
	}()
	return handle, nil
}

// arguments builds the create-or-continue command line and reports which
// thread id (if any) this send asserts it is continuing.
//
// The two shapes are not symmetric and the asymmetry is the CLI's, not ours:
// `codex exec resume` accepts neither `--color` nor `--sandbox`, so the
// bypass posture has to be expressed with the one flag both subcommands take.
func (a *Adapter) arguments(session *runtime.Session, opts runtime.Options) ([]string, string) {
	arguments := []string{"exec"}
	resumedID := session.NativeID
	if resumedID != "" {
		arguments = append(arguments, "resume")
	}
	arguments = append(arguments,
		"--json",
		"--skip-git-repo-check",
		// jig enforces its own write boundary (fingerprint snapshot, diff,
		// rollback) around the whole phase, so the CLI's own approval and
		// sandbox layers only get in the way — the same posture as the Claude
		// Code adapter's --permission-mode bypassPermissions.
		"--dangerously-bypass-approvals-and-sandbox",
	)
	if resumedID == "" {
		// Only the create shape accepts it; resume rejects --color outright.
		arguments = append(arguments, "--color", "never")
	}
	if opts.Model != "" {
		arguments = append(arguments, "--model", opts.Model)
	}
	if resumedID != "" {
		arguments = append(arguments, resumedID)
	}
	// A bare "-" is what makes the CLI read the prompt from stdin. Prompts
	// carry untrusted context and argv is world-readable, so they never go on
	// the command line.
	return append(arguments, "-"), resumedID
}

// codexHandle is one in-flight send: the CLI process, its anchor-led process
// group, and the bounded stream capture.
type codexHandle struct {
	adapter *Adapter
	command *exec.Cmd
	anchor  *exec.Cmd
	// watchdog is the write end of the anchor's stdin pipe. It stays open for
	// the handle's life; closing it (or the process dying) is what tells the
	// anchor to take the group down with it.
	watchdog io.WriteCloser
	groupID  int
	// session is the engine's session record. Codex assigns the thread id, so
	// Result is where the session learns its native identity.
	session *runtime.Session
	// resumedID is the thread this send was told to continue, empty on a
	// creating send. Result compares it against what the CLI reports back.
	resumedID string
	events    chan runtime.Event
	done      chan struct{}
	// stderrDone closes when the stderr capture goroutine has finished.
	stderrDone chan struct{}
	stopped    chan struct{}
	stopOnce   sync.Once
	// stdoutReader and stderrReader are the read ends jig owns. Closing them
	// is what bounds the drain when a descendant outlives the CLI still
	// holding the write end.
	stdoutReader *os.File
	stderrReader *os.File
	stderrTail   *tailBuffer

	mutex       sync.Mutex
	killed      bool
	promptError error
	threadID    string
	text        string
	turnFailure string
	turnSeen    bool
	total       threadUsage
	usageSeen   bool
	finished    bool
	result      runtime.Result
	finalErr    error
}

func (h *codexHandle) Events() <-chan runtime.Event { return h.events }
func (h *codexHandle) ProcessGroupID() int64        { return int64(h.groupID) }

// Kill stops the whole process group: TERM, a grace window, then KILL. The
// anchor holds the group id valid, so the signal cannot stray.
func (h *codexHandle) Kill() error {
	h.mutex.Lock()
	h.killed = true
	h.mutex.Unlock()
	h.stopEverything(terminationGrace)
	return nil
}

// stopEverything is the single teardown path every exit takes: the whole
// process group goes, then the watchdog pipe closes so a group that somehow
// survived still sees its parent-death signal. It is idempotent.
func (h *codexHandle) stopEverything(grace time.Duration) {
	h.stopOnce.Do(func() { close(h.stopped) })
	stopGroup(h.groupID, grace)
	if h.watchdog != nil {
		_ = h.watchdog.Close()
	}
}

func (h *codexHandle) setPromptError(err error) {
	if err == nil || errors.Is(err, syscall.EPIPE) {
		return
	}
	h.mutex.Lock()
	defer h.mutex.Unlock()
	if h.promptError == nil {
		h.promptError = err
	}
}

// consume reads bounded JSONL lines, forwards trace-worthy events, and
// captures the terminal turn. It owns closing the event channel.
func (h *codexHandle) consume(stdout io.Reader) {
	defer close(h.done)
	defer close(h.events)
	reader := bufio.NewReaderSize(stdout, 64<<10)
	line := make([]byte, 0, 64<<10)
	truncated := false
	for {
		fragment, isPrefix, err := reader.ReadLine()
		if len(fragment) > 0 {
			remaining := maxLineBytes - len(line)
			if len(fragment) > remaining {
				fragment = fragment[:remaining]
				truncated = true
			}
			line = append(line, fragment...)
		}
		if !isPrefix || err != nil {
			if len(line) > 0 {
				h.handleLine(line, truncated)
			}
			line = line[:0]
			truncated = false
		}
		if err != nil {
			return
		}
	}
}

// streamEvent is the subset of the `codex exec --json` vocabulary the adapter
// interprets: thread.started, turn.started, turn.completed, turn.failed, and
// item.started / item.updated / item.completed. Everything else forwards as a
// log event rather than being dropped.
type streamEvent struct {
	Type     string          `json:"type"`
	ThreadID string          `json:"thread_id"`
	Usage    streamUsage     `json:"usage"`
	Error    streamError     `json:"error"`
	Item     json.RawMessage `json:"item"`
}

type streamError struct {
	Message string `json:"message"`
}

// streamUsage is the token accounting on turn.completed. The counts are
// cumulative for the thread, and there is no cost field of any kind.
type streamUsage struct {
	InputTokens           int `json:"input_tokens"`
	CachedInputTokens     int `json:"cached_input_tokens"`
	CacheWriteInputTokens int `json:"cache_write_input_tokens"`
	OutputTokens          int `json:"output_tokens"`
	ReasoningOutputTokens int `json:"reasoning_output_tokens"`
}

// streamItem is one thread item. Only the fields the normalizer names are
// pulled out; the whole item rides along as the event payload.
type streamItem struct {
	ID      string `json:"id"`
	Type    string `json:"type"`
	Text    string `json:"text"`
	Message string `json:"message"`
	Server  string `json:"server"`
	Tool    string `json:"tool"`
}

func (h *codexHandle) handleLine(line []byte, truncated bool) {
	if truncated {
		h.emit(runtime.Event{Kind: runtime.EventLog, Name: "line_truncated",
			Text: fmt.Sprintf("stream line exceeded %d bytes", maxLineBytes)})
		return
	}
	var event streamEvent
	if err := json.Unmarshal(line, &event); err != nil {
		h.emit(runtime.Event{Kind: runtime.EventLog, Name: "unparsed_line",
			Text: boundedText(string(line), 512)})
		return
	}
	switch event.Type {
	case "thread.started":
		h.mutex.Lock()
		h.threadID = event.ThreadID
		h.mutex.Unlock()
		h.emit(runtime.Event{Kind: runtime.EventLog, Name: "thread_started",
			Text: event.ThreadID})
	case "turn.started":
		h.emit(runtime.Event{Kind: runtime.EventLog, Name: "turn_started"})
	case "turn.completed":
		h.mutex.Lock()
		h.turnSeen = true
		h.usageSeen = true
		h.total = threadUsage{
			input:  event.Usage.InputTokens,
			output: event.Usage.OutputTokens,
		}
		h.mutex.Unlock()
	case "turn.failed":
		message := event.Error.Message
		if message == "" {
			message = "codex reported a failed turn with no message"
		}
		h.mutex.Lock()
		h.turnSeen = true
		h.turnFailure = message
		h.mutex.Unlock()
		h.emit(runtime.Event{Kind: runtime.EventLog, Name: "turn_failed",
			Text: boundedText(message, protocol.MaxEventPayloadBytes)})
	case "item.started", "item.updated", "item.completed":
		h.handleItem(event.Type, event.Item)
	default:
		h.emit(runtime.Event{Kind: runtime.EventLog, Name: "unhandled:" + event.Type})
	}
}

// handleItem normalizes one thread item onto the engine's event vocabulary.
//
// item.started is forwarded as a log event and not merely skipped: the engine
// runs a no-output silence watchdog, and a long shell command that only
// reports on completion would otherwise look like an agent that has stopped
// talking. item.updated is forwarded the same way for the same reason.
func (h *codexHandle) handleItem(eventType string, raw json.RawMessage) {
	var item streamItem
	if len(raw) == 0 || json.Unmarshal(raw, &item) != nil {
		return
	}
	if eventType != "item.completed" {
		h.emit(runtime.Event{Kind: runtime.EventLog,
			Name: strings.TrimPrefix(eventType, "item.") + ":" + item.Type})
		return
	}
	switch item.Type {
	case "agent_message":
		// The last agent message of the turn IS the final response: it is the
		// same text `--output-last-message` would write to a file, taken from
		// the ordered stream instead so the send needs no scratch file.
		if item.Text != "" {
			h.mutex.Lock()
			h.text = item.Text
			h.mutex.Unlock()
			h.emit(runtime.Event{Kind: runtime.EventText,
				Text: boundedText(item.Text, protocol.MaxEventPayloadBytes)})
		}
	case "reasoning":
		h.emit(runtime.Event{Kind: runtime.EventLog, Name: "reasoning",
			Text: boundedText(item.Text, protocol.MaxEventPayloadBytes)})
	case "command_execution", "file_change", "web_search":
		h.emit(runtime.Event{Kind: runtime.EventToolCall, Name: item.Type,
			Payload: boundedRaw(raw, protocol.MaxEventPayloadBytes)})
	case "mcp_tool_call":
		name := item.Type
		if item.Server != "" && item.Tool != "" {
			name = item.Server + "." + item.Tool
		}
		h.emit(runtime.Event{Kind: runtime.EventToolCall, Name: name,
			Payload: boundedRaw(raw, protocol.MaxEventPayloadBytes)})
	case "error":
		// A Codex `error` item is an in-thread notice, not a terminal verdict
		// — the turn continues. It goes into the trace and nowhere else.
		h.emit(runtime.Event{Kind: runtime.EventLog, Name: "item_error",
			Text: boundedText(item.Message, protocol.MaxEventPayloadBytes)})
	default:
		h.emit(runtime.Event{Kind: runtime.EventLog, Name: "item:" + item.Type,
			Payload: boundedRaw(raw, protocol.MaxEventPayloadBytes)})
	}
}

func (h *codexHandle) emit(event runtime.Event) {
	event.Time = time.Now().UTC()
	h.events <- event
}

// Result waits for the subprocess to exit and the stream to drain, stops the
// anchor, and returns the captured terminal result. After Kill it returns
// ErrKilled; a clean exit without a terminal turn event is an error, never an
// empty envelope.
func (h *codexHandle) Result() (runtime.Result, error) {
	// Wait first, then drain — safe here only because both streams are on
	// descriptors jig owns, so Wait closes nothing the reader is using and
	// cannot cut consume off mid-stream (which would lose the terminal
	// `turn.completed` line still sitting in the pipe and surface downstream
	// as "codex returned no terminal turn event": a phase failure with no
	// cause in the transcript).
	//
	// Waiting on the process rather than on EOF is what keeps shutdown
	// independent of the stream. A descendant that inherited stdout and
	// outlived the CLI holds the write end open, so EOF never arrives on its
	// own; blocking on it here left Result hanging for a CLI that had already
	// exited, with the group still alive and nobody stopping it.
	waitErr := h.command.Wait()
	// The CLI is gone; release the group. The anchor ignores TERM by design,
	// so this is the one place it dies. This is also what frees the inherited
	// descriptor: any descendant still holding stdout is in this group.
	h.stopEverything(0)
	h.drainStream()
	_ = h.anchor.Wait()

	h.mutex.Lock()
	defer h.mutex.Unlock()
	if h.finished {
		return h.result, h.finalErr
	}
	h.finished = true

	// The thread id is the session's native identity, learned from the stream
	// rather than assigned. It is recorded even on a failed send: the thread
	// exists on disk from `thread.started` onward, and resuming it is what
	// keeps a correction in the same conversation.
	if h.threadID != "" && h.session != nil && h.session.NativeID == "" {
		h.session.NativeID = h.threadID
	}
	h.result = runtime.Result{
		Text:      h.text,
		SessionID: h.threadID,
		IsError:   h.turnFailure != "",
	}
	if h.turnFailure != "" {
		h.result.Text = h.turnFailure
	}
	if h.usageSeen {
		// Cumulative totals in, per-send delta out: the engine sums Usage
		// across sends, so reporting the thread total every time would count
		// the first turn again on every correction.
		delta := h.adapter.usage.delta(h.threadID, h.total)
		h.result.Usage = runtime.Usage{
			InputTokens:  delta.input,
			OutputTokens: delta.output,
			TotalTokens:  delta.input + delta.output,
			// CostUSD, ContextTokens and ContextWindow stay zero: the exec
			// stream reports no dollar figure and no context occupancy, and
			// ReportsCost=false is how the engine is told so.
		}
	}

	switch {
	case h.killed:
		h.finalErr = runtime.ErrKilled
	case waitErr != nil:
		h.finalErr = fmt.Errorf("codex exited: %w: %s", waitErr,
			boundedText(h.stderrTail.String(), 1024))
		h.result.ExitCode = exitCode(waitErr)
	case h.promptError != nil:
		h.finalErr = fmt.Errorf("send prompt to codex: %w", h.promptError)
	case !h.turnSeen:
		h.finalErr = errors.New("codex returned no terminal turn event")
	case h.resumedID != "" && h.threadID != "" && h.threadID != h.resumedID:
		// Asserted continuity, verified: a resume that silently started a new
		// thread gets its own diagnostic instead of a phase full of
		// context-free corrections misreported downstream.
		h.finalErr = fmt.Errorf("%w: asked to resume %s, the CLI answered under %s",
			ErrSessionDiscontinuity, h.resumedID, h.threadID)
	}
	return h.result, h.finalErr
}

// drainStream finishes the stream capture after the CLI has exited and the
// group has been released, and bounds how long that can take.
//
// With the group gone the readers normally reach EOF immediately, and the
// grace window is never spent. It exists for the one case the group teardown
// cannot reach: a descendant that left the group (its own setpgid/setsid)
// while still holding the inherited write end. Closing the read end from
// under the reader ends the capture rather than waiting on a descriptor that
// may never be released.
//
// Both captures are waited on, never stdout alone. Closing a read end is what
// ends its capture, so closing the moment stdout finishes can cut the stderr
// copy off before it has read what the exiting CLI left in the pipe — and for
// a nonzero exit that tail is the entire cause of the phase death. One
// deadline spans both waits, because the grace bounds the whole drain rather
// than each stream separately.
//
// The final wait is unbounded on purpose: at that point consume can only be
// blocked publishing to the event channel, and every caller of Result must
// drain Events.
func (h *codexHandle) drainStream() {
	deadline := time.Now().Add(streamDrainGrace)
	waitClosedBy(h.done, deadline)
	waitClosedBy(h.stderrDone, deadline)
	_ = h.stdoutReader.Close()
	_ = h.stderrReader.Close()
	<-h.done
	<-h.stderrDone
}

// waitClosedBy waits for done to close, giving up at deadline. A deadline
// already past does not wait at all, which is what lets one budget cover
// several waits in sequence.
func waitClosedBy(done <-chan struct{}, deadline time.Time) {
	remaining := time.Until(deadline)
	if remaining <= 0 {
		return
	}
	timer := time.NewTimer(remaining)
	defer timer.Stop()
	select {
	case <-done:
	case <-timer.C:
	}
}

// captureStderr keeps the bounded stderr tail. It reads through a descriptor
// jig owns, so Cmd.Wait never blocks on it.
func (h *codexHandle) captureStderr(reader io.Reader) {
	defer close(h.stderrDone)
	_, _ = io.Copy(h.stderrTail, reader)
}

// threadUsage is one thread's cumulative token counts as Codex reports them.
type threadUsage struct {
	input  int
	output int
}

// usageLedger remembers the last cumulative total seen per thread so a send
// can report its own delta. Entries are evicted oldest-first because a
// session never announces its end.
type usageLedger struct {
	mutex sync.Mutex
	seen  map[string]threadUsage
	order []string
}

func (l *usageLedger) delta(threadID string, total threadUsage) threadUsage {
	if threadID == "" {
		return total
	}
	l.mutex.Lock()
	defer l.mutex.Unlock()
	if l.seen == nil {
		l.seen = make(map[string]threadUsage)
	}
	previous, known := l.seen[threadID]
	if !known {
		l.order = append(l.order, threadID)
		for len(l.order) > maxTrackedThreads {
			delete(l.seen, l.order[0])
			l.order = l.order[1:]
		}
	}
	l.seen[threadID] = total
	return threadUsage{
		input:  nonNegative(total.input - previous.input),
		output: nonNegative(total.output - previous.output),
	}
}

func nonNegative(value int) int {
	if value < 0 {
		return 0
	}
	return value
}

// stopGroup TERMs the process group, waits up to grace, then KILLs it. ESRCH
// means already gone, which is success.
func stopGroup(groupID int, grace time.Duration) {
	if err := syscall.Kill(-groupID, syscall.SIGTERM); err != nil {
		return
	}
	deadline := time.Now().Add(grace)
	for grace > 0 && time.Now().Before(deadline) {
		time.Sleep(25 * time.Millisecond)
		if err := syscall.Kill(-groupID, 0); err != nil {
			return
		}
	}
	_ = syscall.Kill(-groupID, syscall.SIGKILL)
}

func exitCode(err error) int {
	var exitError *exec.ExitError
	if errors.As(err, &exitError) {
		return exitError.ExitCode()
	}
	return -1
}

// tailBuffer keeps the last limit bytes written — bounded stderr capture.
type tailBuffer struct {
	mutex sync.Mutex
	bytes []byte
	limit int
}

func (b *tailBuffer) Write(value []byte) (int, error) {
	b.mutex.Lock()
	defer b.mutex.Unlock()
	b.bytes = append(b.bytes, value...)
	if len(b.bytes) > b.limit {
		b.bytes = append([]byte(nil), b.bytes[len(b.bytes)-b.limit:]...)
	}
	return len(value), nil
}

func (b *tailBuffer) String() string {
	b.mutex.Lock()
	defer b.mutex.Unlock()
	return string(b.bytes)
}

func boundedText(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	return value[:limit]
}

func boundedRaw(value json.RawMessage, limit int) json.RawMessage {
	if len(value) <= limit {
		return value
	}
	capped, err := json.Marshal(map[string]any{
		"truncated": true, "prefix": string(value[:limit/2]),
	})
	if err != nil {
		return json.RawMessage(`{"truncated":true}`)
	}
	return capped
}
