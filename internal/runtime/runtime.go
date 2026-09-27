// Package runtime is the agent-CLI seam (KTD4): one interface with per-CLI
// capability flags rather than a lowest-common-denominator abstraction.
// Claude Code and Codex differ in session resume semantics, stream shape,
// and cost reporting; the engine reads the probed RuntimeCapability and
// adapts (a can-resume=false runtime degrades corrections to
// transcript-digest replay, R7). The interface is deliberately small so a
// scripted fake can implement it fully — the engine's loop is tested
// against the fake first, the real CLI last.
package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/StructuPath/jig/internal/protocol"
)

// ErrKilled is what Handle.Result returns after Kill stopped the process
// group. The engine treats it as a phase death (R11), never as an envelope.
var ErrKilled = errors.New("runtime process group was killed")

// Event kinds a runtime may stream while a send is in flight. They map onto
// the protocol trace vocabulary; the engine assigns sequence numbers.
const (
	EventToolCall = "tool_call"
	EventText     = "text"
	EventLog      = "log"
)

// Event is one bounded, ordered output event from a running send. Payload is
// runtime-shaped and size-capped by the adapter; the engine forwards it into
// the trace without interpreting it.
type Event struct {
	Kind    string
	Name    string
	Text    string
	Payload json.RawMessage
	Time    time.Time
}

// Usage is one send's cost accounting. Spend accumulates across retries at
// the engine (every send costs); ContextTokens is occupancy — only the last
// send's value is current. Runtimes that do not report cost leave zeros
// (capability flag ReportsCost=false).
type Usage struct {
	InputTokens   int
	OutputTokens  int
	TotalTokens   int
	CostUSD       float64
	ContextTokens int
	ContextWindow int
}

// Options configures one send. Env is the COMPLETE subprocess environment —
// the adapter must pass it through exactly and never mix in the operator's
// environment (KTD10, KTD11): the engine composed it from the role's
// allowlist plus the ephemeral HOME.
type Options struct {
	SystemPrompt string
	Model        string
	Effort       string
	Tools        []string
	WorkDir      string
	Env          []string
}

// Session is the engine's handle on one agent conversation. Key is the
// engine-chosen stable name (attempt-scoped by declaration, KTD5); NativeID
// is the per-CLI identity the adapter mints on the first send and reuses to
// continue. Sends counts completed sends so create-or-continue is a property
// of the session, not a separate method.
type Session struct {
	Key      string
	NativeID string
	Sends    int
}

// Result is one send's terminal outcome: the final response text plus
// accounting. IsError is the runtime's own terminal-error flag (distinct
// from a nonzero exit).
type Result struct {
	Text      string
	SessionID string
	ExitCode  int
	IsError   bool
	Truncated bool
	Usage     Usage
}

// Handle is one in-flight send. Events yields bounded ordered output until
// the send finishes (the channel closes); Result then returns the terminal
// outcome. Kill stops the entire process group — after it, Result returns
// ErrKilled. The engine must drain Events before or while waiting on Result.
type Handle interface {
	Events() <-chan Event
	Result() (Result, error)
	Kill() error
	// ProcessGroupID is the subprocess group the send runs in, recorded into
	// the attempt manifest so reconcile can stop orphans; zero when no real
	// process exists (fakes).
	ProcessGroupID() int64
}

// Runtime is one agent CLI. Probe records version and capability flags at
// worker start (KTD4); StartOrContinue sends one prompt into the session,
// creating the CLI-native conversation on first use and continuing it after.
type Runtime interface {
	Probe(ctx context.Context) (protocol.RuntimeCapability, error)
	StartOrContinue(ctx context.Context, session *Session, prompt string, opts Options) (Handle, error)
}
