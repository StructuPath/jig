// Package enginetest is the scripted fake Runtime the engine's loop is
// built against, test-first, before the real CLI (the U4 execution note).
// Each call consumes one scripted step: "write these files, then emit this
// output" — so write-boundary scenarios are expressible — or hang until
// killed, or crash without a result. The fake records every call (prompt,
// session identity, options) so tests can assert that corrections re-enter
// the SAME live session, that fresh sessions appear after deaths, and that
// a can-resume=false engine replays transcript digests into new sessions.
package enginetest

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/StructuPath/jig/internal/protocol"
	"github.com/StructuPath/jig/internal/runtime"
)

// Step scripts one StartOrContinue call, consumed in order.
type Step struct {
	// Files are written before the output is emitted. Keys beginning with
	// "~/" resolve against the call's HOME (from opts.Env); everything else
	// is relative to the call's work directory.
	Files map[string]string
	// Events are streamed before the result.
	Events []runtime.Event
	// Text is the final response text.
	Text string
	// Hang emits nothing and waits for Kill — the watchdog scenario.
	Hang bool
	// Crash ends the send with a process error and no result.
	Crash bool
	// IsError sets the runtime's own terminal-error flag on the result — the
	// CLI that exits 0 and returns error prose (auth rejected, rate limited)
	// rather than an envelope. Text carries the CLI's message.
	IsError bool
	// ExitCode is the reported process exit status.
	ExitCode int
	// Usage is the send's reported accounting.
	Usage runtime.Usage
}

// Call records one StartOrContinue invocation for assertions.
type Call struct {
	Prompt     string
	SessionKey string
	NativeID   string
	Send       int // session.Sends after this call started (1 = created it)
	Options    runtime.Options
}

// Runtime is the scripted fake.
type Runtime struct {
	// CanResume is the advertised capability flag (KTD4). False exercises
	// the transcript-digest degraded path.
	CanResume bool

	mutex sync.Mutex
	steps []Step
	next  int
	calls []Call
}

// New builds a resume-capable scripted runtime.
func New(steps ...Step) *Runtime {
	return &Runtime{CanResume: true, steps: steps}
}

// Append adds more scripted steps.
func (r *Runtime) Append(steps ...Step) {
	r.mutex.Lock()
	defer r.mutex.Unlock()
	r.steps = append(r.steps, steps...)
}

// Calls returns a copy of every recorded call.
func (r *Runtime) Calls() []Call {
	r.mutex.Lock()
	defer r.mutex.Unlock()
	return append([]Call(nil), r.calls...)
}

// Remaining reports unconsumed steps — a finished test should have zero.
func (r *Runtime) Remaining() int {
	r.mutex.Lock()
	defer r.mutex.Unlock()
	return len(r.steps) - r.next
}

// Probe reports the scripted capability record.
func (r *Runtime) Probe(context.Context) (protocol.RuntimeCapability, error) {
	return protocol.RuntimeCapability{
		Name: "scripted", Version: "0.0.0", CanResume: r.CanResume, ReportsCost: true,
	}, nil
}

// StartOrContinue consumes the next step: writes its files, then hands back
// a handle that streams its events and result.
func (r *Runtime) StartOrContinue(
	_ context.Context, session *runtime.Session, prompt string, opts runtime.Options,
) (runtime.Handle, error) {
	r.mutex.Lock()
	if r.next >= len(r.steps) {
		r.mutex.Unlock()
		return nil, fmt.Errorf("scripted runtime: no step for call %d (prompt %.80q)", r.next+1, prompt)
	}
	step := r.steps[r.next]
	r.next++
	if session.NativeID == "" {
		session.NativeID = "fake-" + session.Key
	}
	session.Sends++
	r.calls = append(r.calls, Call{
		Prompt:     prompt,
		SessionKey: session.Key,
		NativeID:   session.NativeID,
		Send:       session.Sends,
		Options:    opts,
	})
	r.mutex.Unlock()

	for path, content := range step.Files {
		target, err := resolvePath(path, opts)
		if err != nil {
			return nil, err
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return nil, err
		}
		if err := os.WriteFile(target, []byte(content), 0o644); err != nil {
			return nil, err
		}
	}

	handle := &handle{
		step:   step,
		events: make(chan runtime.Event, len(step.Events)+1),
		killed: make(chan struct{}),
	}
	go handle.run()
	return handle, nil
}

func resolvePath(path string, opts runtime.Options) (string, error) {
	if home, found := strings.CutPrefix(path, "~/"); found {
		for _, entry := range opts.Env {
			if value, ok := strings.CutPrefix(entry, "HOME="); ok {
				return filepath.Join(value, home), nil
			}
		}
		return "", fmt.Errorf("scripted step writes %q but the call env has no HOME", path)
	}
	if opts.WorkDir == "" {
		return "", fmt.Errorf("scripted step writes %q but the call has no work directory", path)
	}
	return filepath.Join(opts.WorkDir, path), nil
}

type handle struct {
	step     Step
	events   chan runtime.Event
	killed   chan struct{}
	killOnce sync.Once
}

func (h *handle) run() {
	if h.step.Hang {
		// Emit nothing; the channel closes only when the group is "killed".
		<-h.killed
		close(h.events)
		return
	}
	for _, event := range h.step.Events {
		if event.Time.IsZero() {
			event.Time = time.Now().UTC()
		}
		select {
		case h.events <- event:
		case <-h.killed:
			close(h.events)
			return
		}
	}
	close(h.events)
}

func (h *handle) Events() <-chan runtime.Event { return h.events }
func (h *handle) ProcessGroupID() int64        { return 0 }

func (h *handle) Kill() error {
	h.killOnce.Do(func() { close(h.killed) })
	return nil
}

func (h *handle) Result() (runtime.Result, error) {
	select {
	case <-h.killed:
		return runtime.Result{}, runtime.ErrKilled
	default:
	}
	if h.step.Crash {
		return runtime.Result{}, fmt.Errorf("scripted crash: agent subprocess exited without a result")
	}
	return runtime.Result{
		Text:     h.step.Text,
		IsError:  h.step.IsError,
		ExitCode: h.step.ExitCode,
		Usage:    h.step.Usage,
	}, nil
}

// EnvelopeText renders a bare JSON envelope — a convenience for scripts.
func EnvelopeText(fields map[string]any) string {
	body, err := json.Marshal(fields)
	if err != nil {
		panic(err)
	}
	return string(body)
}
