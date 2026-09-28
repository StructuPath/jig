// Package enginetest is the scripted fake Runtime the engine's loop is
// built against, test-first, before the real CLI (the U4 execution note).
// Each call consumes one scripted step: "write these files, then emit this
// output" — so write-boundary scenarios are expressible — or hang until
// killed, or crash without a result. The fake records every call (prompt,
// session identity, options) so tests can assert that corrections re-enter
// the SAME live session, that fresh sessions appear after deaths, and that
// a can-resume=false engine replays transcript digests into new sessions.
//
// The fake is safe for concurrent StartOrContinue calls, which a parallel
// group makes. Concurrent calls arrive in no fixed order, so steps can be
// routed: Route gives calls matching a predicate (a role's system prompt,
// say) their own queue, and Started/WaitFor let two calls rendezvous.
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
	// Started, when set, is closed as the call starts, so another step can
	// wait for this one to be running.
	Started chan struct{}
	// WaitFor, when set, holds the send (no events, no result) until it is
	// closed or the send is killed.
	WaitFor <-chan struct{}
	// Delay holds the result this long after WaitFor released, or until the
	// send is killed.
	Delay time.Duration
	// ProcessGroup is the process group the handle reports.
	ProcessGroup int64
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

	mutex  sync.Mutex
	steps  []Step
	next   int
	routes []*route
	calls  []Call
	kills  int
}

// route is a queue of steps reserved for the calls its predicate matches.
type route struct {
	match func(Call) bool
	steps []Step
	next  int
}

// ForSystemPrompt matches the calls made under one system prompt — in
// practice, one role's calls.
func ForSystemPrompt(prompt string) func(Call) bool {
	return func(call Call) bool { return call.Options.SystemPrompt == prompt }
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

// Route reserves steps for the calls match accepts. A matching call takes
// the next unconsumed step of the first matching route that has one, and
// never falls through to the unrouted queue.
func (r *Runtime) Route(match func(Call) bool, steps ...Step) {
	r.mutex.Lock()
	defer r.mutex.Unlock()
	r.routes = append(r.routes, &route{match: match, steps: steps})
}

// Kills reports how many sends were killed.
func (r *Runtime) Kills() int {
	r.mutex.Lock()
	defer r.mutex.Unlock()
	return r.kills
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
	remaining := len(r.steps) - r.next
	for _, route := range r.routes {
		remaining += len(route.steps) - route.next
	}
	return remaining
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
	step, found := r.nextStep(Call{Prompt: prompt, SessionKey: session.Key, Options: opts})
	if !found {
		r.mutex.Unlock()
		return nil, fmt.Errorf("scripted runtime: no step for call %d (prompt %.80q)", len(r.calls)+1, prompt)
	}
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

	if step.Started != nil {
		close(step.Started)
	}
	handle := &handle{
		runtime: r,
		step:    step,
		events:  make(chan runtime.Event, len(step.Events)+1),
		killed:  make(chan struct{}),
	}
	go handle.run()
	return handle, nil
}

// nextStep takes the next step for a call: its route's, when one matches,
// else the unrouted queue's. Callers hold the mutex.
func (r *Runtime) nextStep(call Call) (Step, bool) {
	routed := false
	for _, route := range r.routes {
		if !route.match(call) {
			continue
		}
		routed = true
		if route.next < len(route.steps) {
			route.next++
			return route.steps[route.next-1], true
		}
	}
	if routed || r.next >= len(r.steps) {
		return Step{}, false
	}
	r.next++
	return r.steps[r.next-1], true
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
	runtime  *Runtime
	step     Step
	events   chan runtime.Event
	killed   chan struct{}
	killOnce sync.Once
}

func (h *handle) run() {
	if h.step.WaitFor != nil {
		select {
		case <-h.step.WaitFor:
		case <-h.killed:
			close(h.events)
			return
		}
	}
	if h.step.Delay > 0 {
		select {
		case <-time.After(h.step.Delay):
		case <-h.killed:
			close(h.events)
			return
		}
	}
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
func (h *handle) ProcessGroupID() int64        { return h.step.ProcessGroup }

func (h *handle) Kill() error {
	h.killOnce.Do(func() {
		h.runtime.mutex.Lock()
		h.runtime.kills++
		h.runtime.mutex.Unlock()
		close(h.killed)
	})
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
