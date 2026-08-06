// Package engine executes a frozen definition's phase chain inside one
// attempt (U4): agent phases with live-session repair, code phases with
// adapter envelopes, claim-verifying gates, declared repair edges,
// write-boundary enforcement, ephemeral HOME, and the acceptance predicate.
//
// The control flow is sssf:agents.py's call pipeline in Go, under the
// nesting the plan's execution diagram makes normative: the per-emission
// parse budget nests INSIDE the gate-correction loop, so every corrected
// emission re-enters parsing with a fresh parse budget (R7). Phase status
// defaults to fail; success is earned by a parsed envelope that cleared
// every gate. Corrections go to the same live session — one message, not a
// cold start; a can-resume=false runtime degrades to transcript-digest
// replay in fresh sessions, marked in the trace as the elevated-cost path.
//
// The engine implements the worker's AttemptRunner seam and returns an
// Outcome; the worker owns heartbeats, start/complete, and disposal.
// Success here is accepted_unpublished — publish is U7's critical section.
package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/StructuPath/jig/internal/protocol"
	"github.com/StructuPath/jig/internal/runtime"
	"github.com/StructuPath/jig/internal/worker"
)

// phaseStatusSkipped marks a phase whose `if:` guard held false. Skipping
// is the definition behaving, so acceptance treats it as passed.
const phaseStatusSkipped = "skipped"

// defaultMaxAttemptSends is the enforced ceiling on prompt sends inside ONE
// attempt. Every send is checked against it before the subprocess starts, and
// exceeding it ends the attempt with its own terminal cause.
//
// TODO(protocol): WorstCaseSendCount understates the ladder. The constant in
// internal/protocol declares gate_budget × parse_budget = 6 and is referenced
// by no code; the ladder the engine actually walks is
// parse(budget+1 emissions) × gate(budget+1 emissions) × crash
// re-entries(budget+1) × repair edge(budget+1 self runs plus budget target
// dispatches) — roughly 252 sends for a single phase at budget 3, and more
// once several phases carry edges. Until that constant is corrected (it lives
// in a package this pass does not own) the real bound is the one enforced
// here.
const defaultMaxAttemptSends = 64

// maxPhaseEnvelopeBytes caps the envelope blob one phase result may embed in
// the attempt summary, so the summary is bounded by capping its INPUTS rather
// than by cutting the serialized JSON afterwards.
const maxPhaseEnvelopeBytes = 8 << 10

// timeoutConfig is the attempt's three clocks (R11): the per-phase wall
// clock, the no-output watchdog, and the per-attempt ceiling.
type timeoutConfig struct {
	phase   time.Duration
	silence time.Duration
	ceiling time.Duration
}

// Config wires one Runner. Runtime and ScratchRoot are required; everything
// else has protocol defaults.
type Config struct {
	// Runtime is the probed agent CLI adapter (KTD4).
	Runtime runtime.Runtime
	// Capability is the runtime's probe record. Zero means Execute probes.
	Capability protocol.RuntimeCapability
	// Sink receives the attempt's ordered trace events. Nil discards.
	Sink EventSink
	// ScratchRoot holds per-attempt ephemeral HOME and handoff directories,
	// created per attempt and destroyed with it (KTD11).
	ScratchRoot string
	// SeedHome provisions runtime auth material into a fresh ephemeral HOME,
	// per role (KTD11). Nil means an empty HOME.
	SeedHome HomeSeeder
	// BaseEnv is the worker environment role allowlists resolve against
	// (KTD10). Nil means os.Environ().
	BaseEnv []string
	// RecordProcess records a live subprocess group into the attempt
	// manifest so reconcile can stop orphans; nil skips recording.
	RecordProcess func(attemptID string, processGroupID int64, active bool) error
	Logger        *slog.Logger

	// Test seams over the protocol limits; zero means the limit table value.
	PhaseTimeout    time.Duration
	NoOutputTimeout time.Duration
	AttemptCeiling  time.Duration
	// MaxAttemptSends bounds prompt sends across the whole attempt. Zero
	// means defaultMaxAttemptSends.
	MaxAttemptSends int
}

// Runner is the phase engine: one per worker, stateless across attempts.
type Runner struct {
	config Config
}

// New validates and builds a Runner.
func New(config Config) (*Runner, error) {
	if config.Runtime == nil {
		return nil, errors.New("engine runtime is required")
	}
	if config.ScratchRoot == "" {
		return nil, errors.New("engine scratch root is required")
	}
	if config.Logger == nil {
		config.Logger = slog.Default()
	}
	if config.BaseEnv == nil {
		config.BaseEnv = os.Environ()
	}
	if config.PhaseTimeout == 0 {
		config.PhaseTimeout = protocol.DefaultPhaseTimeout
	}
	if config.NoOutputTimeout == 0 {
		config.NoOutputTimeout = protocol.NoOutputWatchdog
	}
	if config.AttemptCeiling == 0 {
		config.AttemptCeiling = protocol.MaxAttemptDuration
	}
	if config.MaxAttemptSends == 0 {
		config.MaxAttemptSends = defaultMaxAttemptSends
	}
	return &Runner{config: config}, nil
}

// Attempt is the engine-facing view of one prepared attempt — the U3 seam's
// payload without the worker's unexported lease machinery, so tests can
// construct it directly.
type Attempt struct {
	Claim        protocol.Claim
	WorktreePath string
	Branch       string
	BaseSHA      string
	// Cancelled is closed when a heartbeat carried cancellation (R5). Nil
	// means never cancelled.
	Cancelled <-chan struct{}
	// FreshenLease heartbeats before fenced writes after a possible gap
	// (R5). The engine calls it between phases; nil skips.
	FreshenLease func(ctx context.Context) error
}

// Run implements worker.AttemptRunner over a prepared attempt.
func (r *Runner) Run(ctx context.Context, prepared *worker.PreparedAttempt) worker.Outcome {
	return r.Execute(ctx, Attempt{
		Claim:        prepared.Claim,
		WorktreePath: prepared.WorktreePath,
		Branch:       prepared.Branch,
		BaseSHA:      prepared.BaseSHA,
		Cancelled:    prepared.Cancelled(),
		FreshenLease: prepared.FreshenLease,
	})
}

// Execute runs the frozen chain to the attempt's terminal outcome. Success
// is earned: the zero path through this function is failure.
func (r *Runner) Execute(ctx context.Context, attempt Attempt) worker.Outcome {
	spec, err := protocol.ParseDefinition([]byte(attempt.Claim.Snapshot))
	if err != nil {
		return worker.Outcome{State: protocol.AttemptFailed,
			Error: "frozen snapshot does not parse: " + err.Error()}
	}
	capability := r.config.Capability
	if capability.Name == "" {
		capability, err = r.config.Runtime.Probe(ctx)
		if err != nil {
			return worker.Outcome{State: protocol.AttemptFailed,
				Error: "runtime probe failed: " + err.Error()}
		}
	}
	scratch, err := createScratch(r.config.ScratchRoot, attempt.Claim.Attempt.ID)
	if err != nil {
		return worker.Outcome{State: protocol.AttemptFailed, Error: err.Error()}
	}
	// The whole scratch family — ephemeral HOME included — dies with the
	// attempt (KTD11). Anything an agent wrote "to its home" goes with it.
	defer func() {
		if err := scratch.destroy(); err != nil {
			r.config.Logger.Warn("attempt_scratch_destroy_failed", "error", err)
		}
	}()

	e := &execution{
		runner:       r,
		attempt:      attempt,
		spec:         spec,
		capability:   capability,
		emit:         newEmitter(r.config.Sink, nil),
		scratch:      scratch,
		timeouts:     timeoutConfig{phase: r.config.PhaseTimeout, silence: r.config.NoOutputTimeout, ceiling: r.config.AttemptCeiling},
		sessions:     make(map[string]*runtime.Session),
		transcripts:  make(map[string][]exchange),
		fieldView:    make(map[string]any),
		gateReports:  make(map[string]protocol.GateReport),
		seededRoles:  make(map[string]bool),
		phaseEntries: make(map[string]int),
		touchedPaths: make(map[string]bool),
	}
	return e.run(ctx)
}

// exchange is one prompt/response pair kept for transcript digests (R7).
type exchange struct {
	Prompt   string
	Response string
}

// execution is one attempt's run state.
type execution struct {
	runner       *Runner
	attempt      Attempt
	spec         *protocol.DefinitionSpec
	capability   protocol.RuntimeCapability
	emit         *emitter
	scratch      *attemptScratch
	timeouts     timeoutConfig
	deadline     time.Time
	sessions     map[string]*runtime.Session
	sessionSeq   int
	transcripts  map[string][]exchange
	fieldView    map[string]any
	results      []protocol.PhaseResult
	gateReports  map[string]protocol.GateReport
	seededRoles  map[string]bool
	phaseEntries map[string]int
	touchedPaths map[string]bool
	// sends counts every prompt send this attempt has issued, across phases,
	// parse corrections, gate corrections, crash re-entries, and repair
	// dispatches alike — the one number the whole ladder is bounded by.
	sends int
}

// ---- attempt-level control -------------------------------------------------

type endKind int

const (
	endCompleted endKind = iota
	endFailed
	endAborted
	endCancelled
	endCeiling
	endSendBudget
)

type chainEnd struct {
	kind       endKind
	diagnostic string
}

func (e *execution) run(ctx context.Context) worker.Outcome {
	e.deadline = time.Now().Add(e.timeouts.ceiling)
	e.emit.emit(protocol.EventLog, "", "attempt_start", map[string]any{
		"attempt_id":      e.attempt.Claim.Attempt.ID,
		"job_id":          e.attempt.Claim.Job.ID,
		"attempt_number":  e.attempt.Claim.Attempt.AttemptNumber,
		"definition":      e.spec.Name,
		"runtime":         e.capability.Name,
		"runtime_version": e.capability.Version,
		"can_resume":      e.capability.CanResume,
	})

	end := e.runChain(ctx)
	switch end.kind {
	case endCancelled:
		e.emit.emit(protocol.EventLog, "", "attempt_cancelled", nil)
		return worker.Outcome{State: protocol.AttemptCancelled, Error: end.diagnostic}
	case endCeiling:
		// The ceiling gets its own terminal event (R11): it is a cost bound
		// firing, not a phase misbehaving.
		e.emit.emit(protocol.EventError, "", "attempt_ceiling_exceeded",
			map[string]any{"ceiling": e.timeouts.ceiling.String()})
		return worker.Outcome{State: protocol.AttemptFailed,
			Error: "attempt wall-clock ceiling exceeded", Result: e.summaryJSON(nil)}
	case endSendBudget:
		// Its own terminal cause, like the ceiling: the correction ladder ran
		// out of budget, which is neither a phase that failed nor a clock.
		e.emit.emit(protocol.EventError, "", "attempt_send_budget_exhausted",
			map[string]any{"max_sends": e.runner.config.MaxAttemptSends, "sends": e.sends})
		return worker.Outcome{State: protocol.AttemptFailed,
			Error: end.diagnostic, Result: e.summaryJSON(nil)}
	case endAborted, endFailed:
		return worker.Outcome{State: protocol.AttemptFailed,
			Error: end.diagnostic, Result: e.summaryJSON(nil)}
	}

	acceptance := evaluateAcceptance(e.spec, e.results, e.gateReports)
	e.emit.emit(protocol.EventLog, "", "acceptance", acceptance)
	if !acceptance.Passed {
		// Distinct from a phase failure by construction: every phase may have
		// passed while the declared bar was not met (R12).
		return worker.Outcome{State: protocol.AttemptFailed,
			Error:  "acceptance predicate failed: " + acceptanceDiagnostic(acceptance),
			Result: e.summaryJSON(&acceptance)}
	}
	// Publish is U7; direct runs mark no-publish explicitly.
	return worker.Outcome{State: protocol.AttemptAcceptedUnpublished,
		Result: e.summaryJSON(&acceptance)}
}

func acceptanceDiagnostic(acceptance acceptanceResult) string {
	var failed []string
	for _, check := range acceptance.Checks {
		if !check.Ok {
			failed = append(failed, check.Item+": "+check.Note)
		}
	}
	return strings.Join(failed, "; ")
}

// summaryJSON is the Outcome.Result payload: phase results, acceptance
// evidence, computed changed paths (U7 stages exactly these, never -A), and
// the explicit no-publish marker.
//
// The size bound is applied to the INPUTS, never to the serialized bytes.
// Cutting a JSON document at a byte offset always produces a document that
// does not parse, which destroys the acceptance evidence and the
// changed_paths U7 consumes — the payload arrives, and nothing downstream can
// read any of it. So: cap each embedded envelope first, then marshal; and if
// the result is still over, degrade to a smaller VALID object that still
// carries the verdict and the changed paths.
func (e *execution) summaryJSON(acceptance *acceptanceResult) string {
	var paths []string
	for path := range e.touchedPaths {
		paths = append(paths, path)
	}
	sort.Strings(paths)

	summary := map[string]any{
		"phases":        boundedResults(e.results),
		"changed_paths": paths,
		"publish":       "not_attempted",
	}
	if acceptance != nil {
		summary["acceptance"] = acceptance
	}
	if body, err := json.Marshal(summary); err == nil && len(body) <= protocol.MaxResultBytes {
		return string(body)
	}
	return degradedSummaryJSON(e.results, paths, acceptance)
}

// boundedResults caps every phase result's variable-length content so the
// summary's size is a property of its inputs. The envelope is replaced by a
// valid JSON object carrying its prefix, never by a truncated fragment — a
// json.RawMessage cut mid-object fails to marshal at all.
func boundedResults(results []protocol.PhaseResult) []protocol.PhaseResult {
	bounded := make([]protocol.PhaseResult, len(results))
	for i, result := range results {
		result.Envelope = boundedEnvelope(result.Envelope)
		result.Error = truncateText(result.Error, maxPhaseEnvelopeBytes)
		bounded[i] = result
	}
	return bounded
}

func boundedEnvelope(raw json.RawMessage) json.RawMessage {
	if len(raw) <= maxPhaseEnvelopeBytes {
		return raw
	}
	marker, err := json.Marshal(map[string]any{
		"truncated": true,
		"bytes":     len(raw),
		"prefix":    string(raw[:maxPhaseEnvelopeBytes]),
	})
	if err != nil {
		return json.RawMessage(`{"truncated":true}`)
	}
	return marker
}

// degradedSummaryJSON is what a summary too large even with bounded envelopes
// degrades to: valid JSON carrying a truncation marker, the per-phase
// verdicts, the acceptance result, and changed_paths. If even that does not
// fit, changed_paths is halved until it does — the marker says so, and the
// document parses at every step.
func degradedSummaryJSON(
	results []protocol.PhaseResult, paths []string, acceptance *acceptanceResult,
) string {
	type verdict struct {
		Phase        string `json:"phase"`
		Kind         string `json:"kind"`
		Status       string `json:"status"`
		PhaseAttempt int    `json:"phase_attempt"`
		Error        string `json:"error,omitempty"`
	}
	verdicts := make([]verdict, len(results))
	for i, result := range results {
		verdicts[i] = verdict{
			Phase: result.Phase, Kind: result.Kind, Status: result.Status,
			PhaseAttempt: result.PhaseAttempt, Error: truncateText(result.Error, 512),
		}
	}
	degraded := map[string]any{
		"truncated":         true,
		"truncation_reason": "attempt summary exceeded MaxResultBytes; per-phase envelopes dropped",
		"phases":            verdicts,
		"changed_paths":     paths,
		"publish":           "not_attempted",
	}
	if acceptance != nil {
		degraded["acceptance"] = acceptance
	}
	if body, err := json.Marshal(degraded); err == nil && len(body) <= protocol.MaxResultBytes {
		return string(body)
	}

	kept := paths
	for {
		minimal := map[string]any{
			"truncated":               true,
			"truncation_reason":       "attempt summary exceeded MaxResultBytes; phase detail dropped",
			"changed_paths":           kept,
			"changed_paths_truncated": len(kept) < len(paths),
			"changed_paths_total":     len(paths),
			"publish":                 "not_attempted",
		}
		if acceptance != nil {
			minimal["acceptance"] = map[string]any{"passed": acceptance.Passed}
		}
		body, err := json.Marshal(minimal)
		if err != nil {
			return `{"truncated":true,"publish":"not_attempted"}`
		}
		if len(body) <= protocol.MaxResultBytes || len(kept) == 0 {
			return string(body)
		}
		kept = kept[:len(kept)/2]
	}
}

func (e *execution) cancelled() bool {
	select {
	case <-e.attempt.Cancelled:
		return true
	default:
		return false
	}
}

func (e *execution) ceilingExceeded() bool { return time.Now().After(e.deadline) }

func (e *execution) freshenLease(ctx context.Context) {
	if e.attempt.FreshenLease == nil {
		return
	}
	if err := e.attempt.FreshenLease(ctx); err != nil {
		e.emit.emit(protocol.EventLog, "", "lease_freshen_failed",
			map[string]string{"error": err.Error()})
	}
}

// ---- the chain -------------------------------------------------------------

func (e *execution) runChain(ctx context.Context) chainEnd {
	var previous *parsedEnvelope
	edgeUses := make(map[string]int)
	for _, phase := range e.spec.Phases {
		if e.cancelled() {
			return chainEnd{endCancelled, "cancelled between phases"}
		}
		if e.ceilingExceeded() {
			return chainEnd{endCeiling, ""}
		}
		e.freshenLease(ctx)
		if phase.If != "" && !truthy(e.fieldView[phase.If]) {
			e.recordResult(protocol.PhaseResult{
				Phase: phase.Name, Kind: phase.Kind, Status: phaseStatusSkipped,
			})
			e.emit.emit(protocol.EventLog, phase.Name, "phase_skipped",
				map[string]string{"guard": phase.If})
			continue
		}
		end, envelope := e.runPhaseWithEdge(ctx, phase, previous, edgeUses)
		if end != nil {
			return *end
		}
		if envelope != nil {
			previous = envelope
		}
	}
	return chainEnd{kind: endCompleted}
}

// runPhaseWithEdge runs one chain phase honoring its declared repair edge
// (KTD2): failure — nonzero exit for code, the declared envelope predicate
// for agent — dispatches the named repair phase with the failing envelope,
// then reruns self, edge-budget bounded; exhaustion honors the declared
// fail-job/proceed policy.
func (e *execution) runPhaseWithEdge(
	ctx context.Context, phase protocol.PhaseSpec, previous *parsedEnvelope, edgeUses map[string]int,
) (*chainEnd, *parsedEnvelope) {
	edge := phase.OnFail
	for {
		run := e.runPhaseOnce(ctx, phase, previous)
		if end := run.attemptEnd(); end != nil {
			return end, nil
		}

		triggered := false
		if edge != nil && run.hasEnvelope {
			switch phase.Kind {
			case protocol.PhaseKindCode:
				triggered = run.outcome == phaseFailed
			case protocol.PhaseKindAgent:
				predicate, err := protocol.ParsePredicate(edge.When)
				triggered = err == nil && predicateHolds(predicate, run.envelope.Fields)
			}
		}
		if !triggered {
			if run.outcome == phaseFailed {
				return &chainEnd{endFailed, run.failure}, nil
			}
			return nil, run.envelopeRef()
		}

		if edgeUses[phase.Name] >= edge.Budget {
			policy := protocol.RepairExhaustedProceed
			if edgeExhaustionFailsJob(edge) {
				policy = protocol.RepairExhaustedFailJob
			}
			e.emit.emit(protocol.EventLog, phase.Name, "repair_exhausted", map[string]any{
				"budget": edge.Budget, "policy": policy,
			})
			if edgeExhaustionFailsJob(edge) {
				return &chainEnd{endFailed, fmt.Sprintf(
					"phase %q: repair budget (%d) exhausted", phase.Name, edge.Budget)}, nil
			}
			return nil, run.envelopeRef()
		}
		edgeUses[phase.Name]++
		e.emit.emit(protocol.EventLog, phase.Name, "repair_edge", map[string]any{
			"run": edge.Run, "use": edgeUses[phase.Name], "budget": edge.Budget,
		})

		// Dispatch the repair target with the FAILING envelope as its
		// previous — a failing test suite and a rejecting review enter the
		// repair loop through the same door (R8). The target's own repair
		// edge does not fire on a dispatched run: budgets bound one edge,
		// not a chain of them.
		repairPhase, found := e.phaseByName(edge.Run)
		if !found {
			return &chainEnd{endFailed, fmt.Sprintf(
				"phase %q: repair target %q missing from frozen snapshot", phase.Name, edge.Run)}, nil
		}
		repairRun := e.runPhaseOnce(ctx, repairPhase, run.envelopeRef())
		if end := repairRun.attemptEnd(); end != nil {
			return end, nil
		}
		if repairRun.outcome == phaseFailed {
			return &chainEnd{endFailed, fmt.Sprintf(
				"repair phase %q: %s", repairPhase.Name, repairRun.failure)}, nil
		}
		previous = repairRun.envelopeRef()
		// then: rerun-self.
	}
}

func (e *execution) phaseByName(name string) (protocol.PhaseSpec, bool) {
	for _, phase := range e.spec.Phases {
		if phase.Name == name {
			return phase, true
		}
	}
	return protocol.PhaseSpec{}, false
}

// ---- one phase execution ---------------------------------------------------

type phaseOutcome int

const (
	phaseFailed phaseOutcome = iota // fail is the default; success is earned
	phasePassed
	phaseAborted // write-boundary breach: the attempt dies, never retried
	phaseCancelled
	phaseCeiling
	phaseSendBudget // the attempt's send ladder hit its enforced bound
)

type phaseRun struct {
	outcome     phaseOutcome
	envelope    parsedEnvelope
	hasEnvelope bool
	failure     string
}

func (run phaseRun) attemptEnd() *chainEnd {
	switch run.outcome {
	case phaseAborted:
		return &chainEnd{endAborted, run.failure}
	case phaseCancelled:
		return &chainEnd{endCancelled, run.failure}
	case phaseCeiling:
		return &chainEnd{endCeiling, run.failure}
	case phaseSendBudget:
		return &chainEnd{endSendBudget, run.failure}
	}
	return nil
}

// terminalSend maps a non-OK send outcome that ends the ATTEMPT onto the
// phase run it forces. sendDeath and sendRuntimeError are absent on purpose:
// they end the phase, not the attempt, and their handling needs the
// per-entry death/fail closures.
func terminalSend(kind sendEnd, detail string) *phaseRun {
	switch kind {
	case sendCancelled:
		return &phaseRun{outcome: phaseCancelled, failure: detail}
	case sendCeiling:
		return &phaseRun{outcome: phaseCeiling}
	case sendBudget:
		return &phaseRun{outcome: phaseSendBudget, failure: detail}
	}
	return nil
}

func (run phaseRun) envelopeRef() *parsedEnvelope {
	if !run.hasEnvelope {
		return nil
	}
	envelope := run.envelope
	return &envelope
}

func (e *execution) nextEntry(phase string) int {
	e.phaseEntries[phase]++
	return e.phaseEntries[phase]
}

func (e *execution) recordResult(result protocol.PhaseResult) {
	now := time.Now().UTC()
	if result.EndedAt == nil {
		result.EndedAt = &now
	}
	e.results = append(e.results, result)
}

func (e *execution) runPhaseOnce(
	ctx context.Context, phase protocol.PhaseSpec, previous *parsedEnvelope,
) phaseRun {
	if e.cancelled() {
		return phaseRun{outcome: phaseCancelled, failure: "cancelled before phase " + phase.Name}
	}
	if e.ceilingExceeded() {
		return phaseRun{outcome: phaseCeiling}
	}
	if phase.Kind == protocol.PhaseKindCode {
		return e.runCodePhase(ctx, phase)
	}
	return e.runAgentPhase(ctx, phase, previous)
}

// phaseCorrectionBudget is the phase's gate-correction budget — the largest
// declared gate budget, defaulting per the limits table. Crash re-entries
// draw against the same number (R11).
func (e *execution) phaseCorrectionBudget(phase protocol.PhaseSpec) int {
	budget := 0
	for _, gate := range phase.Gates {
		if gate.Budget > budget {
			budget = gate.Budget
		}
	}
	if budget == 0 {
		budget = protocol.DefaultPhaseRetryBudget
	}
	return budget
}

// runGates runs every configured gate against the envelope, records the
// evidence, and returns the merged report. Every corrected envelope re-runs
// ALL gates, never only the failed one (R9).
func (e *execution) runGates(
	ctx context.Context, phase protocol.PhaseSpec, envelope parsedEnvelope, attempt int,
) protocol.GateReport {
	var merged protocol.GateReport
	env := e.phaseEnv(phase)
	for _, gate := range phase.Gates {
		report := runGate(gateContext{
			ctx:      ctx,
			worktree: e.attempt.WorktreePath,
			envelope: envelope,
			env:      env,
			timeout:  e.timeouts,
		}, gate)
		e.gateReports[gate.Name] = report
		merged.Checks = append(merged.Checks, report.Checks...)
		eventType := protocol.EventGatePass
		if !report.Passed() {
			eventType = protocol.EventGateFail
		}
		e.emit.emit(eventType, phase.Name, gate.Name, map[string]any{
			"attempt":    attempt,
			"checks":     report.Checks,
			"violations": report.Violations(),
		})
	}
	return merged
}

// phaseEnv composes the environment for everything this phase runs jig-side:
// its code command and its gates alike. PATH is always allowed so a command
// can resolve; everything else comes from the owning role's allowlist, and
// the HOME/XDG family always points at the attempt's ephemeral home (KTD10,
// KTD11). Code phases run trusted frozen-definition commands but still get
// the ephemeral HOME so nothing they spawn reads operator dotfiles; gate
// commands run in the worktree the agent just wrote, which makes them
// agent-influenced code and gets them the same containment.
func (e *execution) phaseEnv(phase protocol.PhaseSpec) []string {
	allow := []string{"PATH"}
	if phase.Owner != "" {
		allow = append(allow, e.spec.Roster[phase.Owner].Env...)
	}
	return subprocessEnv(e.runner.config.BaseEnv, allow, e.scratch.home)
}

func (e *execution) mergeFields(fields map[string]any) {
	for key, value := range fields {
		e.fieldView[key] = value
	}
}

// ---- code phases -----------------------------------------------------------

func (e *execution) runCodePhase(ctx context.Context, phase protocol.PhaseSpec) phaseRun {
	entry := e.nextEntry(phase.Name)
	started := time.Now().UTC()
	e.emit.emit(protocol.EventPhaseStart, phase.Name, "", map[string]any{
		"kind": phase.Kind, "phase_attempt": entry, "command": phase.Command,
	})

	env := e.phaseEnv(phase)
	// Cancellation during a code phase kills the command's process group via
	// context cancellation, then reports cancelled rather than a phase fail.
	commandCtx, stopCommand := context.WithCancel(ctx)
	go func() {
		select {
		case <-e.attempt.Cancelled:
			stopCommand()
		case <-commandCtx.Done():
		}
	}()
	result := runShellCommand(commandCtx, e.attempt.WorktreePath, phase.Command, env, e.timeouts.phase)
	stopCommand()
	if e.cancelled() {
		return phaseRun{outcome: phaseCancelled, failure: "cancelled during phase " + phase.Name}
	}
	envelope := adapterEnvelope(phase.Command, result)
	e.emit.emit(protocol.EventLog, phase.Name, "command_result", map[string]any{
		"exit_code": result.ExitCode, "timed_out": result.TimedOut,
		"output_tail": result.OutputTail,
	})

	// Gates verify the adapter envelope's claims too; there is no session to
	// correct, so a violation is a phase failure that the repair edge (if
	// declared) routes like any other code-phase failure.
	report := e.runGates(ctx, phase, envelope, entry)
	passed := result.Passed() && report.Passed()
	status := protocol.EnvelopeFail
	if passed {
		status = protocol.EnvelopeSuccess
	}
	e.mergeFields(envelope.Fields)
	e.recordResult(protocol.PhaseResult{
		Phase: phase.Name, Kind: phase.Kind, Status: status, PhaseAttempt: entry,
		Envelope: envelope.Raw, Gates: report, StartedAt: &started,
	})
	e.emit.emit(protocol.EventPhaseEnd, phase.Name, "", map[string]any{"status": status})
	run := phaseRun{envelope: envelope, hasEnvelope: true}
	if passed {
		run.outcome = phasePassed
	} else {
		run.outcome = phaseFailed
		run.failure = fmt.Sprintf("phase %q: %s", phase.Name, envelope.Base.Summary)
	}
	return run
}

// ---- agent phases ----------------------------------------------------------

// runAgentPhase runs an agent phase with crash recovery (R11): a subprocess
// death — crash, hang past the watchdog, phase wall clock — consumes one
// phase retry, rolls the worktree back to the pre-phase snapshot, and
// restarts with a fresh session.
func (e *execution) runAgentPhase(
	ctx context.Context, phase protocol.PhaseSpec, previous *parsedEnvelope,
) phaseRun {
	budget := e.phaseCorrectionBudget(phase)
	deaths := 0
	for {
		run, died := e.runAgentPhaseAttempt(ctx, phase, previous)
		if !died {
			return run
		}
		deaths++
		e.dropSession(phase.Owner)
		if deaths > budget {
			return phaseRun{outcome: phaseFailed, failure: fmt.Sprintf(
				"phase %q: agent died %d time(s): %s", phase.Name, deaths, run.failure)}
		}
	}
}

func (e *execution) failInfra(phase, detail string) phaseRun {
	e.emit.emit(protocol.EventError, phase, "engine_error", map[string]string{"error": detail})
	return phaseRun{outcome: phaseFailed, failure: fmt.Sprintf("phase %q: %s", phase, detail)}
}

// enforceWriteBoundary compares the worktree against the pre-phase snapshot
// (R10). A non-nil terminal is the run the phase must return: an engine
// failure when the comparison itself broke, or the abort when the role
// overstepped — the phase result is recorded here in that case, so callers
// must not record their own. Every exit from an agent phase entry that had a
// live subprocess goes through this, success and failure alike.
func (e *execution) enforceWriteBoundary(
	ctx context.Context, phase protocol.PhaseSpec, entry int, started time.Time,
	before treeSnapshot, writes []string,
) (touched []string, terminal *phaseRun) {
	touched, breaches, err := enforceBoundary(ctx, e.attempt.WorktreePath, before, writes)
	if err != nil {
		run := e.failInfra(phase.Name, "write-boundary enforcement: "+err.Error())
		return nil, &run
	}
	if len(breaches) == 0 {
		return touched, nil
	}
	e.emit.emit(protocol.EventError, phase.Name, "write_boundary_breach", map[string]any{
		"role": phase.Owner, "writes": writes, "breaches": breaches,
	})
	e.recordResult(protocol.PhaseResult{
		Phase: phase.Name, Kind: phase.Kind, Status: protocol.EnvelopeFail,
		PhaseAttempt: entry, Error: "write boundary breach", StartedAt: &started,
	})
	var paths []string
	for _, item := range breaches {
		paths = append(paths, item.Path+" — "+item.Outcome)
	}
	run := phaseRun{outcome: phaseAborted, failure: fmt.Sprintf(
		"phase %q: role %q modified %d path(s) outside its write allowlist: %s",
		phase.Name, phase.Owner, len(breaches), strings.Join(paths, "; "))}
	return nil, &run
}

// runAgentPhaseAttempt is one entry of an agent phase: snapshot, prompt,
// the nested parse/gate correction loops, boundary enforcement, envelope
// persistence. died=true means the subprocess was killed or crashed and the
// worktree has been rolled back to the pre-phase snapshot.
func (e *execution) runAgentPhaseAttempt(
	ctx context.Context, phase protocol.PhaseSpec, previous *parsedEnvelope,
) (run phaseRun, died bool) {
	entry := e.nextEntry(phase.Name)
	started := time.Now().UTC()
	role := e.spec.Roster[phase.Owner]
	e.emit.emit(protocol.EventPhaseStart, phase.Name, "", map[string]any{
		"kind": phase.Kind, "phase_attempt": entry, "owner": phase.Owner,
	})

	before, err := snapshotTree(ctx, e.attempt.WorktreePath)
	if err != nil {
		return e.failInfra(phase.Name, err.Error()), false
	}
	if err := e.seedRole(phase.Owner, role); err != nil {
		return e.failInfra(phase.Name, "seed ephemeral HOME: "+err.Error()), false
	}
	systemPrompt, err := e.rolePrompt(role.SystemPrompt, role.SystemPromptPath)
	if err != nil {
		return e.failInfra(phase.Name, err.Error()), false
	}
	userTemplate, err := e.rolePrompt(role.UserPrompt, role.UserPromptPath)
	if err != nil {
		return e.failInfra(phase.Name, err.Error()), false
	}

	sender := &agentSender{
		execution:     e,
		phase:         phase.Name,
		role:          phase.Owner,
		phaseDeadline: time.Now().Add(e.timeouts.phase),
		options: runtime.Options{
			SystemPrompt: systemPrompt,
			Model:        role.Model,
			Tools:        append([]string(nil), role.Tools...),
			WorkDir:      e.attempt.WorktreePath,
			Env:          subprocessEnv(e.runner.config.BaseEnv, role.Env, e.scratch.home),
		},
	}
	session := e.sessionFor(phase.Owner)
	e.emit.emit(protocol.EventAgentStart, phase.Name, phase.Owner, map[string]any{
		"model": role.Model, "session": session.Key, "can_resume": e.capability.CanResume,
		"tools": role.Tools, "phase_attempt": entry,
	})

	// death wraps up one dead entry: roll the worktree back to the pre-phase
	// snapshot and emit the envelope-less terminal trace event (R11).
	death := func(detail string) (phaseRun, bool) {
		e.emit.emit(protocol.EventPhaseDeath, phase.Name, phase.Owner, map[string]any{
			"phase_attempt": entry, "error": detail,
		})
		if rollbackErr := restoreSnapshot(ctx, e.attempt.WorktreePath, before); rollbackErr != nil {
			e.emit.emit(protocol.EventError, phase.Name, "rollback_failed",
				map[string]string{"error": rollbackErr.Error()})
		}
		e.recordResult(protocol.PhaseResult{
			Phase: phase.Name, Kind: phase.Kind, Status: protocol.EnvelopeFail,
			PhaseAttempt: entry, Error: detail, StartedAt: &started,
		})
		return phaseRun{outcome: phaseFailed, failure: detail}, true
	}
	// A phase that fails on parse exhaustion, gate-budget exhaustion, or a
	// terminal runtime error has still had a live agent in the worktree. The
	// boundary is enforced here too, or breaching writes survive into the
	// retained worktree and the next phase — a failed phase is not a phase
	// that wrote nothing (R10).
	fail := func(detail string) (phaseRun, bool) {
		if _, terminal := e.enforceWriteBoundary(
			ctx, phase, entry, started, before, role.Writes); terminal != nil {
			terminal.failure = detail + " — and " + terminal.failure
			return *terminal, false
		}
		e.recordResult(protocol.PhaseResult{
			Phase: phase.Name, Kind: phase.Kind, Status: protocol.EnvelopeFail,
			PhaseAttempt: entry, Error: detail, StartedAt: &started,
		})
		e.emit.emit(protocol.EventPhaseEnd, phase.Name, "", map[string]any{
			"status": protocol.EnvelopeFail, "error": detail,
		})
		return phaseRun{outcome: phaseFailed, failure: fmt.Sprintf("phase %q: %s", phase.Name, detail)}, false
	}

	prompt := composePrompt(userTemplate, e.attempt.Claim.Parameters, previous, e.scratch.handoff)
	result, sendEndKind, sendDetail := sender.send(ctx, prompt)
	switch sendEndKind {
	case sendDeath:
		return death(sendDetail)
	case sendRuntimeError:
		return fail(sendDetail)
	default:
		if terminal := terminalSend(sendEndKind, sendDetail); terminal != nil {
			return *terminal, false
		}
	}

	// Gate-correction loop with the parse budget NESTED inside: every
	// corrected emission re-enters parsing with a fresh per-emission budget
	// (the plan's normative nesting, R7).
	budget := e.phaseCorrectionBudget(phase)
	var envelope parsedEnvelope
	for gateAttempt := 1; ; gateAttempt++ {
		var parseEnd sendEnd
		var parseDetail string
		var ok bool
		envelope, parseEnd, parseDetail, ok = e.parseWithBudget(ctx, phase, sender, result)
		switch parseEnd {
		case sendDeath:
			return death(parseDetail)
		default:
			if terminal := terminalSend(parseEnd, parseDetail); terminal != nil {
				return *terminal, false
			}
		}
		if !ok {
			return fail(parseDetail)
		}

		report := e.runGates(ctx, phase, envelope, gateAttempt)
		if report.Passed() {
			break
		}
		if gateAttempt > budget {
			return fail(fmt.Sprintf("failed gates after %d attempt(s): %s",
				gateAttempt, strings.Join(report.Violations(), "; ")))
		}
		result, sendEndKind, sendDetail = sender.send(ctx, gateCorrection(report.Violations()))
		switch sendEndKind {
		case sendDeath:
			return death(sendDetail)
		case sendRuntimeError:
			return fail(sendDetail)
		default:
			if terminal := terminalSend(sendEndKind, sendDetail); terminal != nil {
				return *terminal, false
			}
		}
	}

	// Permission is checked after every send is done and before the envelope
	// is accepted: an agent does not get to report success on a phase in
	// which it wrote somewhere it was not allowed to (R10).
	touched, terminal := e.enforceWriteBoundary(ctx, phase, entry, started, before, role.Writes)
	if terminal != nil {
		return *terminal, false
	}
	for _, path := range touched {
		e.touchedPaths[path] = true
	}
	if len(touched) > 0 {
		e.emit.emit(protocol.EventLog, phase.Name, "paths_touched",
			map[string]any{"role": phase.Owner, "paths": touched})
	}

	e.mergeFields(envelope.Fields)
	e.emit.emit(protocol.EventHandoff, phase.Name, phase.Owner, map[string]any{
		"artifacts": envelope.Base.Artifacts, "summary": envelope.Base.Summary,
	})
	// Spend accumulated across every send; occupancy is the LAST send's
	// number — the one whose context is current (sssf tracer discipline).
	e.emit.emit(protocol.EventAgentEnd, phase.Name, phase.Owner, map[string]any{
		"tokens": sender.spend.TotalTokens, "cost": sender.spend.CostUSD,
		"context_tokens": sender.last.Usage.ContextTokens,
		"sends":          sender.sendCount,
	})

	status := protocol.EnvelopeFail
	if envelope.Base.Status == protocol.EnvelopeSuccess {
		status = protocol.EnvelopeSuccess
	}
	e.recordResult(protocol.PhaseResult{
		Phase: phase.Name, Kind: phase.Kind, Status: status, PhaseAttempt: entry,
		Envelope: envelope.Raw, Gates: e.lastMergedReport(phase), StartedAt: &started,
	})
	e.emit.emit(protocol.EventPhaseEnd, phase.Name, "", map[string]any{"status": status})

	run = phaseRun{envelope: envelope, hasEnvelope: true}
	if status == protocol.EnvelopeSuccess {
		run.outcome = phasePassed
	} else {
		run.outcome = phaseFailed
		run.failure = fmt.Sprintf("phase %q: agent reported status=%q: %s",
			phase.Name, envelope.Base.Status, envelope.Base.Summary)
	}
	return run, false
}

func (e *execution) lastMergedReport(phase protocol.PhaseSpec) protocol.GateReport {
	var merged protocol.GateReport
	for _, gate := range phase.Gates {
		merged.Checks = append(merged.Checks, e.gateReports[gate.Name].Checks...)
	}
	return merged
}

// parseWithBudget parses one emission against the envelope contract,
// re-prompting the live session on failure under the per-emission budget
// (R7). Every invalid attempt persists to the trace, size-capped.
func (e *execution) parseWithBudget(
	ctx context.Context, phase protocol.PhaseSpec, sender *agentSender, result runtime.Result,
) (parsedEnvelope, sendEnd, string, bool) {
	for attempt := 1; ; attempt++ {
		envelope, err := parseEnvelopeText(result.Text)
		if err == nil {
			return envelope, sendOK, "", true
		}
		e.emit.emit(protocol.EventLog, phase.Name, "invalid_envelope",
			invalidEnvelopePayload(phase.Owner, attempt, result.Text, err))
		if attempt > protocol.ParseBudgetPerEmission {
			return parsedEnvelope{}, sendOK, fmt.Sprintf(
				"agent never produced a valid envelope in %d parse attempt(s): %v", attempt, err), false
		}
		var endKind sendEnd
		var detail string
		result, endKind, detail = sender.send(ctx, parseCorrection(err))
		if endKind != sendOK {
			return parsedEnvelope{}, endKind, detail, false
		}
	}
}

// ---- sending ---------------------------------------------------------------

type sendEnd int

const (
	sendOK sendEnd = iota
	sendDeath
	sendCancelled
	sendCeiling
	// sendBudget: the attempt's enforced send ceiling was reached before this
	// send started. Terminal for the attempt — the ladder is the cost.
	sendBudget
	// sendRuntimeError: the runtime reported its own terminal error (R7).
	// A CLI can exit 0 and still have failed — auth rejected, rate limited —
	// and the prose it returns is not an envelope. Re-prompting it 3× per
	// emission, gate-correcting, counting deaths, and rerunning the repair
	// edge against a rate limit is a retry storm with the wrong diagnosis, so
	// this ends the PHASE with no parse ladder at all.
	sendRuntimeError
)

// agentSender owns one phase attempt's sends: session continuity (or its
// declared degradation), the three clocks, spend accumulation, transcripts,
// and process-group recording.
type agentSender struct {
	*execution
	phase         string
	role          string
	phaseDeadline time.Time // per-phase wall clock
	options       runtime.Options
	spend         runtime.Usage
	last          runtime.Result
	sendCount     int
}

func (s *agentSender) send(ctx context.Context, prompt string) (runtime.Result, sendEnd, string) {
	// Checked BEFORE the subprocess starts: the send that would exceed the
	// bound is the one that must not happen.
	if s.execution.sends >= s.runner.config.MaxAttemptSends {
		return runtime.Result{}, sendBudget, fmt.Sprintf(
			"attempt send budget (%d) exhausted in phase %q",
			s.runner.config.MaxAttemptSends, s.phase)
	}
	s.execution.sends++

	session := s.sessionFor(s.role)
	if !s.capability.CanResume && session.Sends > 0 {
		// The runtime cannot resume: replay a bounded transcript digest into
		// a fresh session and mark the elevated-cost path in the trace (R7).
		session = s.freshSession(s.role)
		prompt = transcriptDigest(s.transcripts[s.role]) + prompt
		s.emit.emit(protocol.EventLog, s.phase, "degraded_correction", map[string]any{
			"role": s.role, "session": session.Key,
			"reason": "runtime cannot resume sessions; transcript digest replayed into a fresh session",
		})
	}

	handle, err := s.runner.config.Runtime.StartOrContinue(ctx, session, prompt, s.options)
	if err != nil {
		return runtime.Result{}, sendDeath, "start agent subprocess: " + err.Error()
	}
	s.sendCount++
	s.recordProcess(handle.ProcessGroupID(), true)
	defer s.recordProcess(handle.ProcessGroupID(), false)

	silence := time.NewTimer(s.timeouts.silence)
	defer silence.Stop()
	phaseClock := time.NewTimer(time.Until(s.phaseDeadline))
	defer phaseClock.Stop()
	ceilingClock := time.NewTimer(time.Until(s.execution.deadline))
	defer ceilingClock.Stop()
	events := handle.Events()
	for events != nil {
		select {
		case event, open := <-events:
			if !open {
				events = nil
				continue
			}
			resetTimer(silence, s.timeouts.silence)
			s.emitRuntimeEvent(event)
		case <-silence.C:
			killAndDrain(handle)
			return runtime.Result{}, sendDeath, fmt.Sprintf(
				"no output for %s (watchdog)", s.timeouts.silence)
		case <-phaseClock.C:
			killAndDrain(handle)
			return runtime.Result{}, sendDeath, fmt.Sprintf(
				"phase wall clock (%s) exceeded", s.timeouts.phase)
		case <-ceilingClock.C:
			killAndDrain(handle)
			return runtime.Result{}, sendCeiling, "attempt wall-clock ceiling exceeded"
		case <-s.attempt.Cancelled:
			killAndDrain(handle)
			return runtime.Result{}, sendCancelled, "cancelled during phase " + s.phase
		}
	}
	result, err := handle.Result()
	if err != nil {
		return runtime.Result{}, sendDeath, "agent subprocess: " + err.Error()
	}
	if result.IsError {
		// The runtime's own terminal-error flag, checked before anything tries
		// to read an envelope out of the text. What comes back here is the
		// CLI's diagnosis — "invalid API key", "rate limit exceeded" — and
		// re-prompting it cannot make it parse.
		return result, sendRuntimeError, fmt.Sprintf(
			"runtime reported a terminal error (exit %d): %s",
			result.ExitCode,
			truncateText(strings.TrimSpace(result.Text), protocol.MaxCommandOutputTailBytes))
	}
	s.spend.InputTokens += result.Usage.InputTokens
	s.spend.OutputTokens += result.Usage.OutputTokens
	s.spend.TotalTokens += result.Usage.TotalTokens
	s.spend.CostUSD += result.Usage.CostUSD
	s.last = result
	s.transcripts[s.role] = append(s.transcripts[s.role], exchange{Prompt: prompt, Response: result.Text})
	return result, sendOK, ""
}

func (s *agentSender) emitRuntimeEvent(event runtime.Event) {
	switch event.Kind {
	case runtime.EventToolCall:
		s.emit.emit(protocol.EventToolCall, s.phase, event.Name, event.Payload)
	case runtime.EventText:
		s.emit.emit(protocol.EventLog, s.phase, "agent_text",
			map[string]string{"text": truncateText(event.Text, protocol.MaxEventPayloadBytes/2)})
	default:
		s.emit.emit(protocol.EventLog, s.phase, event.Name,
			map[string]string{"text": event.Text})
	}
}

func (s *agentSender) recordProcess(groupID int64, active bool) {
	if s.runner.config.RecordProcess == nil || groupID == 0 {
		return
	}
	if err := s.runner.config.RecordProcess(s.attempt.Claim.Attempt.ID, groupID, active); err != nil {
		s.runner.config.Logger.Warn("process_group_record_failed",
			"attempt_id", s.attempt.Claim.Attempt.ID, "error", err)
	}
}

// killAndDrain stops the process group and drains the event stream so the
// adapter's reader goroutine can finish; the result is discarded — a killed
// send never yields an envelope.
func killAndDrain(handle runtime.Handle) {
	_ = handle.Kill()
	for range handle.Events() {
	}
	_, _ = handle.Result()
}

func resetTimer(timer *time.Timer, duration time.Duration) {
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
	timer.Reset(duration)
}

// ---- sessions and prompts --------------------------------------------------

func (e *execution) sessionFor(role string) *runtime.Session {
	if session := e.sessions[role]; session != nil {
		return session
	}
	session := &runtime.Session{Key: e.attempt.Claim.Attempt.ID + "-" + role}
	e.sessions[role] = session
	return session
}

func (e *execution) freshSession(role string) *runtime.Session {
	e.sessionSeq++
	session := &runtime.Session{
		Key: fmt.Sprintf("%s-%s-r%d", e.attempt.Claim.Attempt.ID, role, e.sessionSeq),
	}
	e.sessions[role] = session
	return session
}

// dropSession discards a role's conversation after a death: the transcript
// goes, and the next send gets a session under a NEW key so no runtime can
// silently resume the killed conversation (R11: fresh session).
func (e *execution) dropSession(role string) {
	delete(e.transcripts, role)
	e.freshSession(role)
}

func (e *execution) seedRole(role string, spec protocol.RoleSpec) error {
	if e.seededRoles[role] || e.runner.config.SeedHome == nil {
		e.seededRoles[role] = true
		return nil
	}
	e.seededRoles[role] = true
	return e.runner.config.SeedHome(e.scratch.home, role, spec)
}

// rolePrompt resolves prompt content or a worktree-relative path — the two
// are mutually exclusive at save time.
func (e *execution) rolePrompt(content, path string) (string, error) {
	if content != "" {
		return content, nil
	}
	if path == "" {
		return "", nil
	}
	if filepath.IsAbs(path) || !filepath.IsLocal(path) {
		return "", fmt.Errorf("prompt path %q is not worktree-relative", path)
	}
	body, err := os.ReadFile(filepath.Join(e.attempt.WorktreePath, path))
	if err != nil {
		return "", fmt.Errorf("read prompt %q: %w", path, err)
	}
	return string(body), nil
}

// composePrompt renders the role's user prompt: parameters substitute as
// {{name}}, the previous envelope and handoff directory substitute when
// referenced and append as sections when not, and the envelope contract is
// always stated last.
func composePrompt(
	template string, parameters map[string]string, previous *parsedEnvelope, handoffDir string,
) string {
	text := template
	for name, value := range parameters {
		text = strings.ReplaceAll(text, "{{"+name+"}}", value)
	}
	previousJSON := "(none)"
	if previous != nil {
		previousJSON = string(previous.Raw)
	}
	if strings.Contains(text, "{{previous_envelope}}") {
		text = strings.ReplaceAll(text, "{{previous_envelope}}", previousJSON)
	} else {
		text += "\n\n## Previous envelope\n" + previousJSON
	}
	if strings.Contains(text, "{{handoff_dir}}") {
		text = strings.ReplaceAll(text, "{{handoff_dir}}", handoffDir)
	} else {
		text += "\n\n## Handoff directory\nShare working notes for later phases in: " + handoffDir
	}
	text += "\n\nWhen you are done, respond with ONLY a JSON object with these fields: " +
		"status (\"success\" or \"fail\"), summary, artifacts (repo-relative paths you " +
		"created or changed), notes_for_next_agent. No prose around it."
	return text
}

// transcriptDigest is the bounded replay prefix for a can-resume=false
// runtime (R7): the most recent exchanges, capped, oldest dropped first.
func transcriptDigest(entries []exchange) string {
	var parts []string
	total := 0
	for i := len(entries) - 1; i >= 0; i-- {
		part := "### Prompt\n" + entries[i].Prompt + "\n### Your response\n" + entries[i].Response + "\n"
		if total+len(part) > protocol.MaxTranscriptDigestBytes {
			break
		}
		total += len(part)
		parts = append([]string{part}, parts...)
	}
	return "You are continuing earlier work; your previous session is not resumable. " +
		"Transcript digest of that session (oldest first):\n\n" +
		strings.Join(parts, "\n") + "\n---\n\n"
}
