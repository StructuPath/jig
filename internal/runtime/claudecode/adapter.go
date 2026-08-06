// Package claudecode is the one real v1 runtime adapter (KTD4): it runs the
// Claude Code CLI as a supervised subprocess per send.
//
// Invocation is `claude --print --output-format stream-json --verbose` (the
// factory:supervisor.go shape); session identity is create-or-continue —
// `--session-id <uuid>` mints the conversation on the first send and
// `--resume <uuid>` continues it after, which is what makes live-session
// repair one message instead of a cold start (R7).
//
// Process ownership follows factory's anchor pattern: a trap-TERM shell
// becomes the group leader, the CLI joins its group, and every descendant
// the CLI spawns dies with one signal to -pgid. The anchor is our own
// unreaped child for the handle's whole life, so its pid — the group id —
// cannot be recycled and signalling the group needs no identity dance.
//
// The prompt arrives on stdin (never argv: prompts carry untrusted context
// and argv is world-readable). Output is captured as bounded JSON lines; the
// terminal `result` event yields the final text, session id, and cost.
//
// Unix-only, matching jig's supported platforms (macOS and Linux CI).
package claudecode

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
	RuntimeName = "claude-code"

	// maxLineBytes bounds one captured stream-json line; the remainder of an
	// overlong line is discarded with a truncation marker, never buffered.
	maxLineBytes = 4 << 20

	// eventChannelDepth bounds the ordered event stream between the reader
	// goroutine and the engine. The engine always drains, so sends block
	// (backpressure) rather than drop.
	eventChannelDepth = 256

	// terminationGrace is how long TERM gets before KILL when stopping the
	// process group.
	terminationGrace = 3 * time.Second

	// anchorSelfTerminationGrace is how long the anchor gives its own group
	// after the parent-death watchdog fires, before killing it outright. It
	// lives in the anchor script, so it is written in shell seconds.
	anchorSelfTerminationGrace = "3"
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
// EOF and the anchor tears the whole group down on its way out. Without that
// watchdog an interrupted jig left an immortal anchor behind per send, and a
// bypassPermissions CLI still editing the operator's repository.
var anchorScript = strings.Join([]string{
	"# " + anchorSignature,
	"trap '' TERM",
	"read -r _ 2>/dev/null",
	"kill -TERM 0 2>/dev/null",
	"sleep " + anchorSelfTerminationGrace,
	"kill -KILL 0 2>/dev/null",
}, "\n")

// ErrSessionDiscontinuity reports that a --resume send came back under a
// DIFFERENT session id than the one it was told to continue: the CLI cold
// started instead of resuming, so every correction in that send lost the
// conversation. Surfacing it as a send failure costs one phase retry with a
// fresh session; swallowing it costs the whole phase's context and gets
// misreported as "the agent never produced a valid envelope" (R7, R11).
var ErrSessionDiscontinuity = errors.New("claude resumed a different session than the one requested")

// Adapter runs the Claude Code CLI. The zero value is not usable; New
// resolves the executable once so agent subprocesses never depend on PATH
// from their own (allowlisted) environment to find the CLI.
type Adapter struct {
	executable string
}

// New builds an adapter over the installed `claude` executable, resolved
// against the worker's own environment at construction time.
func New() (*Adapter, error) {
	path, err := exec.LookPath("claude")
	if err != nil {
		return nil, fmt.Errorf("claude CLI is not installed: %w", err)
	}
	return &Adapter{executable: path}, nil
}

// NewWithExecutable builds an adapter over an explicit executable path —
// the test seam for scripted stub CLIs.
func NewWithExecutable(path string) *Adapter {
	return &Adapter{executable: path}
}

// Probe runs `claude --version` and reports the capability record (KTD4).
// Claude Code resumes sessions (--resume) and reports cost in its result
// event, so both flags are true by construction of this adapter.
func (a *Adapter) Probe(ctx context.Context) (protocol.RuntimeCapability, error) {
	ctx, cancel := context.WithTimeout(ctx, protocol.GitCommandTimeout)
	defer cancel()
	output, err := exec.CommandContext(ctx, a.executable, "--version").Output()
	if err != nil {
		return protocol.RuntimeCapability{}, fmt.Errorf("probe %s: %w", a.executable, err)
	}
	firstLine, _, _ := strings.Cut(strings.TrimSpace(string(output)), "\n")
	version, _, _ := strings.Cut(firstLine, " ")
	if version == "" {
		return protocol.RuntimeCapability{}, fmt.Errorf(
			"probe %s: no version in output %q", a.executable, firstLine)
	}
	return protocol.RuntimeCapability{
		Name:        RuntimeName,
		Version:     version,
		CanResume:   true,
		ReportsCost: true,
	}, nil
}

// StartOrContinue sends one prompt into the session. The CLI-native session
// id is minted here on first use; later sends resume it. The subprocess
// environment is exactly opts.Env (KTD10) — nothing from the worker's own
// environment leaks in.
func (a *Adapter) StartOrContinue(
	ctx context.Context, session *runtime.Session, prompt string, opts runtime.Options,
) (runtime.Handle, error) {
	if session == nil {
		return nil, errors.New("session is required")
	}
	if opts.WorkDir == "" {
		return nil, errors.New("work directory is required")
	}

	arguments := []string{
		"--print",
		"--output-format", "stream-json",
		"--verbose",
		"--permission-mode", "bypassPermissions",
	}
	resumedID := ""
	if session.NativeID == "" {
		id, err := newUUID()
		if err != nil {
			return nil, err
		}
		session.NativeID = id
		arguments = append(arguments, "--session-id", id)
	} else {
		resumedID = session.NativeID
		arguments = append(arguments, "--resume", session.NativeID)
	}
	if opts.Model != "" {
		arguments = append(arguments, "--model", opts.Model)
	}
	if opts.SystemPrompt != "" {
		arguments = append(arguments, "--system-prompt", opts.SystemPrompt)
	}
	if len(opts.Tools) > 0 {
		arguments = append(arguments, "--tools", strings.Join(opts.Tools, ","))
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
		return nil, fmt.Errorf("open claude stdin: %w", err)
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		abandon()
		return nil, fmt.Errorf("open claude stdout: %w", err)
	}
	stderrTail := &tailBuffer{limit: protocol.MaxErrorBytes}
	command.Stderr = stderrTail
	if err := command.Start(); err != nil {
		abandon()
		return nil, fmt.Errorf("start claude: %w", err)
	}
	session.Sends++

	handle := &claudeHandle{
		command:    command,
		anchor:     anchor,
		watchdog:   watchdog,
		groupID:    groupID,
		resumedID:  resumedID,
		events:     make(chan runtime.Event, eventChannelDepth),
		done:       make(chan struct{}),
		stopped:    make(chan struct{}),
		stderrTail: stderrTail,
	}
	go func() {
		_, writeErr := io.WriteString(stdin, prompt)
		handle.setPromptError(errors.Join(writeErr, stdin.Close()))
	}()
	go handle.consume(stdout)
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

// claudeHandle is one in-flight send: the CLI process, its anchor-led
// process group, and the bounded stream capture.
type claudeHandle struct {
	command *exec.Cmd
	anchor  *exec.Cmd
	// watchdog is the write end of the anchor's stdin pipe. It stays open for
	// the handle's life; closing it (or the process dying) is what tells the
	// anchor to take the group down with it.
	watchdog io.WriteCloser
	groupID  int
	// resumedID is the session this send was told to continue, empty on a
	// creating send. Result compares it against what the CLI reports back.
	resumedID  string
	events     chan runtime.Event
	done       chan struct{}
	stopped    chan struct{}
	stopOnce   sync.Once
	stderrTail *tailBuffer

	mutex       sync.Mutex
	killed      bool
	promptError error
	result      runtime.Result
	resultSeen  bool
	finished    bool
	finalErr    error
}

func (h *claudeHandle) Events() <-chan runtime.Event { return h.events }
func (h *claudeHandle) ProcessGroupID() int64        { return int64(h.groupID) }

// Kill stops the whole process group: TERM, a grace window, then KILL. The
// anchor holds the group id valid, so the signal cannot stray.
func (h *claudeHandle) Kill() error {
	h.mutex.Lock()
	h.killed = true
	h.mutex.Unlock()
	h.stopEverything(terminationGrace)
	return nil
}

// stopEverything is the single teardown path every exit takes: the whole
// process group goes, then the watchdog pipe closes so a group that somehow
// survived still sees its parent-death signal. It is idempotent.
func (h *claudeHandle) stopEverything(grace time.Duration) {
	h.stopOnce.Do(func() { close(h.stopped) })
	stopGroup(h.groupID, grace)
	if h.watchdog != nil {
		_ = h.watchdog.Close()
	}
}

func (h *claudeHandle) setPromptError(err error) {
	if err == nil || errors.Is(err, syscall.EPIPE) {
		return
	}
	h.mutex.Lock()
	defer h.mutex.Unlock()
	if h.promptError == nil {
		h.promptError = err
	}
}

// consume reads bounded stream-json lines, forwards trace-worthy events, and
// captures the terminal result event. It owns closing the event channel.
func (h *claudeHandle) consume(stdout io.Reader) {
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

// streamLine is the subset of the Claude Code stream-json vocabulary the
// adapter interprets. Everything else forwards as a log event.
type streamLine struct {
	Type         string          `json:"type"`
	Subtype      string          `json:"subtype"`
	SessionID    string          `json:"session_id"`
	Result       string          `json:"result"`
	IsError      bool            `json:"is_error"`
	TotalCostUSD float64         `json:"total_cost_usd"`
	Usage        streamUsage     `json:"usage"`
	Message      json.RawMessage `json:"message"`
}

type streamUsage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
}

type messageBody struct {
	Content []contentBlock `json:"content"`
}

type contentBlock struct {
	Type  string          `json:"type"`
	Name  string          `json:"name"`
	Text  string          `json:"text"`
	Input json.RawMessage `json:"input"`
}

func (h *claudeHandle) handleLine(line []byte, truncated bool) {
	if truncated {
		h.emit(runtime.Event{Kind: runtime.EventLog, Name: "line_truncated",
			Text: fmt.Sprintf("stream line exceeded %d bytes", maxLineBytes)})
		return
	}
	var event streamLine
	if err := json.Unmarshal(line, &event); err != nil {
		h.emit(runtime.Event{Kind: runtime.EventLog, Name: "unparsed_line",
			Text: boundedText(string(line), 512)})
		return
	}
	switch event.Type {
	case "result":
		occupancy := event.Usage.InputTokens + event.Usage.CacheReadInputTokens +
			event.Usage.CacheCreationInputTokens + event.Usage.OutputTokens
		h.mutex.Lock()
		h.result = runtime.Result{
			Text:      event.Result,
			SessionID: event.SessionID,
			IsError:   event.IsError,
			Usage: runtime.Usage{
				InputTokens:   event.Usage.InputTokens,
				OutputTokens:  event.Usage.OutputTokens,
				TotalTokens:   event.Usage.InputTokens + event.Usage.OutputTokens,
				CostUSD:       event.TotalCostUSD,
				ContextTokens: occupancy,
			},
		}
		h.resultSeen = true
		h.mutex.Unlock()
	case "assistant", "user":
		var body messageBody
		if json.Unmarshal(event.Message, &body) != nil {
			return
		}
		for _, block := range body.Content {
			switch block.Type {
			case "tool_use":
				h.emit(runtime.Event{Kind: runtime.EventToolCall, Name: block.Name,
					Payload: boundedRaw(block.Input, protocol.MaxEventPayloadBytes)})
			case "text":
				if block.Text != "" {
					h.emit(runtime.Event{Kind: runtime.EventText,
						Text: boundedText(block.Text, protocol.MaxEventPayloadBytes)})
				}
			}
		}
	case "system":
		h.emit(runtime.Event{Kind: runtime.EventLog, Name: "system:" + event.Subtype})
	}
}

func (h *claudeHandle) emit(event runtime.Event) {
	event.Time = time.Now().UTC()
	h.events <- event
}

// Result waits for the subprocess to exit and the stream to drain, stops the
// anchor, and returns the captured terminal result. After Kill it returns
// ErrKilled; a clean exit without a result event is an error, never an
// empty envelope.
func (h *claudeHandle) Result() (runtime.Result, error) {
	waitErr := h.command.Wait()
	<-h.done
	// The CLI is gone; release the group. The anchor ignores TERM by design,
	// so this is the one place it dies.
	h.stopEverything(0)
	_ = h.anchor.Wait()

	h.mutex.Lock()
	defer h.mutex.Unlock()
	if h.finished {
		return h.result, h.finalErr
	}
	h.finished = true
	switch {
	case h.killed:
		h.finalErr = runtime.ErrKilled
	case waitErr != nil:
		h.finalErr = fmt.Errorf("claude exited: %w: %s", waitErr,
			boundedText(h.stderrTail.String(), 1024))
		h.result.ExitCode = exitCode(waitErr)
	case h.promptError != nil:
		h.finalErr = fmt.Errorf("send prompt to claude: %w", h.promptError)
	case !h.resultSeen:
		h.finalErr = errors.New("claude returned no terminal result event")
	case h.resumedID != "" && h.result.SessionID != "" && h.result.SessionID != h.resumedID:
		// Asserted continuity, verified: a resume that silently cold started
		// gets its own diagnostic instead of a phase full of context-free
		// corrections misreported downstream.
		h.finalErr = fmt.Errorf("%w: asked to resume %s, the CLI answered under %s",
			ErrSessionDiscontinuity, h.resumedID, h.result.SessionID)
	}
	return h.result, h.finalErr
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

func newUUID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("mint session id: %w", err)
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}
