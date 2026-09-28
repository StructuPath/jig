// parallel.go — the one opt-in parallel construct (plan U5, R8–R11): a
// definition's `parallel:` group of read-only agent phases runs as ONE step
// of the chain, its members concurrently.
//
// Each member runs in a private view of the execution (KTD6): its own
// session, transcript, results, deferred field merges, gate reports, and an
// ephemeral HOME of its own, seeded serially before any member starts, so
// concurrent agent CLIs never share credentials or `~/.claude.json`. What
// the members share with the chain is safe under concurrency by
// construction: the locked emitter, the read-only deadline, and the
// attempt-wide counters (sends, session keys, phase entries).
//
// The worktree is the group's, not any member's. One snapshot is taken
// before the members start and the boundary is enforced once after all of
// them stop, with an empty allowlist: every member is read-only, so ANY
// change — a member's write, a dead member's leftover — is a breach that
// rolls the tree back to the group snapshot and aborts the attempt.
//
// Members see the envelope that preceded the group, never a sibling's.
// Their results merge into the chain in declared order after the join, and
// the phase after the group receives the last member's envelope. Repair
// edges resolve after the join too, walking members in declared order: the
// first member whose edge fired with budget left dispatches its repair
// target with its own envelope, charges only its own budget, and the whole
// group runs again. Total group runs are at most 1 + the sum of member
// budgets.
package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/StructuPath/jig/internal/protocol"
	"github.com/StructuPath/jig/internal/runtime"
)

// memberRun is one member's part in one group run.
type memberRun struct {
	skipped bool
	run     phaseRun
	view    *execution
}

// memberResult is what one member goroutine hands back to the runner.
type memberResult struct {
	run        phaseRun
	died       bool
	panicked   bool
	panicValue any
}

// memberTerminal is a member result that ends the attempt.
type memberTerminal struct {
	index int
	run   phaseRun
}

// runGroup runs the group as one chain step with R10's edge walk, and
// returns the envelope the next phase receives.
func (e *execution) runGroup(
	ctx context.Context, members []protocol.PhaseSpec, previous *parsedEnvelope, edgeUses map[string]int,
) (*chainEnd, *parsedEnvelope) {
	// Guards are judged once, on the group's first run. A repair between
	// runs can change the field a guard reads, and a re-judged guard would
	// let the repair skip the very reviewer that rejected it — recorded as
	// skipped, which acceptance counts as passed. Sequential rerun-self never
	// re-checks a guard either: a member that ran runs again, one that was
	// skipped stays skipped.
	var skipped []bool
	for groupRun := 1; ; groupRun++ {
		runs, end := e.runGroupOnce(ctx, members, previous, groupRun, skipped)
		if end != nil {
			return end, nil
		}
		if skipped == nil {
			skipped = make([]bool, len(members))
			for i := range runs {
				skipped[i] = runs[i].skipped
			}
		}
		last := previous
		rerun := false
		for i, member := range members {
			if runs[i].skipped {
				continue
			}
			run := runs[i].run
			if run.hasEnvelope {
				last = run.envelopeRef()
			}
			if !edgeTriggered(member, run) {
				// Not triggered: a member that passed is done; anything else
				// ends the attempt here, as a failure would have run
				// sequentially. Success is earned, never defaulted into.
				if run.outcome != phasePassed {
					return &chainEnd{endFailed, run.failure}, nil
				}
				continue
			}
			end, repaired, dispatched := e.followEdge(ctx, member, run, edgeUses)
			if end != nil {
				return end, nil
			}
			if dispatched {
				previous = repaired
				rerun = true
				break
			}
			// Exhausted under proceed: on to the next member.
		}
		if !rerun {
			return nil, last
		}
	}
}

// runGroupOnce runs every member that is not skipped, concurrently, then
// enforces the boundary and merges. On the first run (firstSkipped nil)
// each member's guard decides; on every later run firstSkipped does. A
// non-nil chainEnd means nothing was merged.
func (e *execution) runGroupOnce(
	ctx context.Context, members []protocol.PhaseSpec, previous *parsedEnvelope, groupRun int,
	firstSkipped []bool,
) ([]memberRun, *chainEnd) {
	if e.cancelled() {
		return nil, &chainEnd{endCancelled, "cancelled before the parallel group"}
	}
	if e.ceilingExceeded() {
		return nil, &chainEnd{endCeiling, ""}
	}
	names := make([]string, len(members))
	for i, member := range members {
		names[i] = member.Name
	}
	e.emit.emit(protocol.EventLog, "", "parallel_group_start",
		map[string]any{"members": names, "run": groupRun})

	// Every guard is judged against the view that preceded the group's
	// first run — validation guarantees no guard reads a field a sibling
	// reports, and runGroup keeps a repair from re-judging one.
	runs := make([]memberRun, len(members))
	var active []int
	for i, member := range members {
		skip := member.If != "" && !guardHolds(member.If, e.fieldView)
		if firstSkipped != nil {
			skip = firstSkipped[i]
		}
		if skip {
			runs[i].skipped = true
			e.emit.emit(protocol.EventLog, member.Name, "phase_skipped",
				map[string]string{"guard": member.If})
			continue
		}
		active = append(active, i)
	}

	if len(active) > 0 {
		before, err := snapshotTree(ctx, e.attempt.WorktreePath)
		if err != nil {
			return nil, e.groupInfra(names, err.Error())
		}
		// Seed serially, before any member starts.
		for _, i := range active {
			view, err := e.memberView(i, members[i])
			if err != nil {
				return nil, e.groupInfra(names, err.Error())
			}
			if err := view.beginGroupRun(e.scratch.handoff); err != nil {
				return nil, e.groupInfra(names, err.Error())
			}
			// Whatever a member left in its private handoff directory is
			// published at the merge below or discarded here, never both.
			defer os.RemoveAll(view.scratch.handoff)
			owner := members[i].Owner
			if err := view.seedRole(owner, e.spec.Roster[owner]); err != nil {
				return nil, e.groupInfra(names, fmt.Sprintf(
					"seed ephemeral HOME for member %q: %s", members[i].Name, err))
			}
			runs[i].view = view
		}
		if end := e.runMembers(ctx, members, active, runs, previous, names, before); end != nil {
			return nil, end
		}
	}

	for i, member := range members {
		if runs[i].skipped {
			e.recordResult(protocol.PhaseResult{
				Phase: member.Name, Kind: member.Kind, Status: phaseStatusSkipped,
			})
			continue
		}
		view := runs[i].view
		e.results = append(e.results, view.results...)
		for _, merge := range view.pendingFields {
			e.mergeAgentFields(merge.phase, merge.role, merge.fields)
		}
		for name, report := range view.gateReports {
			e.gateReports[name] = report
		}
		// A member's handoff notes join the chain's handoff directory only
		// now, in declared order, each under its own folder: no sibling
		// could read them mid-run, and none can overwrite another's.
		if err := publishMemberHandoff(
			view.scratch.handoff, e.scratch.handoff, i, member.Name, view.handoffSeed); err != nil {
			e.emit.emit(protocol.EventError, member.Name, "handoff_merge_failed",
				map[string]string{"error": err.Error()})
		}
	}
	e.emit.emit(protocol.EventLog, "", "parallel_group_end",
		map[string]any{"members": names, "run": groupRun})
	return runs, nil
}

// runMembers fans the active members out in waves: every member runs
// concurrently; a member that died re-enters alone in the next wave, after
// its siblings finished, until its death budget is spent. The first result
// that ends the attempt stops every sibling — their sends are killed — and
// the runner waits for all of them before it enforces the boundary once.
func (e *execution) runMembers(
	ctx context.Context, members []protocol.PhaseSpec, active []int, runs []memberRun,
	previous *parsedEnvelope, names []string, before treeSnapshot,
) *chainEnd {
	stop := make(chan struct{})
	groupCtx, cancelGroup := context.WithCancel(ctx)
	defer cancelGroup()
	var stopOnce sync.Once
	stopMembers := func() {
		stopOnce.Do(func() {
			close(stop)
			cancelGroup()
		})
	}

	deaths := make([]int, len(members))
	var terminals []memberTerminal
	var panicked bool
	var panicValue any
	for pending := active; len(pending) > 0; {
		results := make([]memberResult, len(members))
		var wait sync.WaitGroup
		for _, i := range pending {
			view := runs[i].view
			view.stop = stop
			wait.Add(1)
			go func(i int, view *execution) {
				defer wait.Done()
				defer func() {
					if recovered := recover(); recovered != nil {
						results[i] = memberResult{panicked: true, panicValue: recovered}
						stopMembers()
					}
				}()
				run, died := view.runAgentPhaseAttempt(groupCtx, members[i], previous)
				results[i] = memberResult{run: run, died: died}
				if endsAttempt(run.outcome) {
					stopMembers()
				}
			}(i, view)
		}
		wait.Wait()

		var next []int
		for _, i := range pending {
			result := results[i]
			switch {
			case result.panicked:
				if !panicked {
					panicked, panicValue = true, result.panicValue
				}
			case endsAttempt(result.run.outcome):
				terminals = append(terminals, memberTerminal{index: i, run: result.run})
			case result.died:
				deaths[i]++
				runs[i].view.dropSession(members[i].Owner)
				if deaths[i] > e.phaseCorrectionBudget(members[i]) {
					runs[i].run = phaseRun{outcome: phaseFailed, failure: fmt.Sprintf(
						"phase %q: agent died %d time(s): %s", members[i].Name, deaths[i], result.run.failure)}
					continue
				}
				next = append(next, i)
			default:
				runs[i].run = result.run
			}
		}
		if panicked || len(terminals) > 0 {
			break
		}
		pending = next
	}

	if panicked {
		// A panic in a member is a panic in the chain, exactly as it would
		// be sequentially — re-raised on the chain's own goroutine, where
		// Execute's deferred cleanup still runs. Siblings kept running until
		// the stop, so the worktree is enforced first: a sibling's write
		// must not outlive the attempt just because another member crashed.
		_, breaches, err := enforceBoundary(
			context.WithoutCancel(ctx), e.attempt.WorktreePath, before, []string{})
		e.emit.emit(protocol.EventError, "", "parallel_group_panic", map[string]any{
			"parallel_group": names, "panic": fmt.Sprint(panicValue),
			"breaches": breaches, "enforcement_error": errorText(err),
		})
		panic(panicValue)
	}

	// The one enforcement, detached from the caller's context: cancellation
	// is one of the exits it guards, and on a dead context every git command
	// would fail.
	_, breaches, err := enforceBoundary(context.WithoutCancel(ctx), e.attempt.WorktreePath, before, []string{})
	if err != nil {
		return e.groupInfra(names, "write-boundary enforcement: "+err.Error())
	}
	if len(breaches) > 0 {
		return e.groupBreach(members, active, runs, names, breaches)
	}
	if len(terminals) > 0 {
		return groupTerminalEnd(terminals)
	}
	return nil
}

func errorText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// endsAttempt reports whether a member's outcome ends the attempt.
func endsAttempt(outcome phaseOutcome) bool {
	switch outcome {
	case phaseAborted, phaseCancelled, phaseCeiling, phaseSendBudget:
		return true
	}
	return false
}

// terminalRank orders the causes a group can end on: cancellation, then
// the ceiling, then the send budget. A breach outranks them all and is
// decided before this is consulted.
var terminalRank = map[phaseOutcome]int{
	phaseAborted:    0,
	phaseCancelled:  1,
	phaseCeiling:    2,
	phaseSendBudget: 3,
}

// groupTerminalEnd picks the attempt's end among the members' terminal
// results: by cause precedence, then by declared member order.
func groupTerminalEnd(terminals []memberTerminal) *chainEnd {
	best := terminals[0]
	for _, candidate := range terminals[1:] {
		rank, bestRank := terminalRank[candidate.run.outcome], terminalRank[best.run.outcome]
		if rank < bestRank || (rank == bestRank && candidate.index < best.index) {
			best = candidate
		}
	}
	return best.run.attemptEnd()
}

// groupBreach records the abort. enforceBoundary has already rolled every
// change back to the group snapshot. No member's partial view is merged:
// each member that ran records one failed result, since the breach cannot
// be pinned on one of them.
func (e *execution) groupBreach(
	members []protocol.PhaseSpec, active []int, runs []memberRun, names []string, breaches []breach,
) *chainEnd {
	e.emit.emit(protocol.EventError, "", "write_boundary_breach", map[string]any{
		"parallel_group": names, "writes": []string{}, "breaches": breaches,
	})
	for _, i := range active {
		entry := 0
		if results := runs[i].view.results; len(results) > 0 {
			entry = results[len(results)-1].PhaseAttempt
		}
		e.recordResult(protocol.PhaseResult{
			Phase: members[i].Name, Kind: members[i].Kind, Status: protocol.EnvelopeFail,
			PhaseAttempt: entry, Error: "write boundary breach during the parallel group",
		})
	}
	paths := make([]string, 0, len(breaches))
	for _, item := range breaches {
		paths = append(paths, item.Path+" — "+item.Outcome)
	}
	return &chainEnd{endAborted, fmt.Sprintf(
		"parallel group [%s]: members are read-only, but %d path(s) changed during the group: %s",
		strings.Join(names, ", "), len(breaches), strings.Join(paths, "; "))}
}

func (e *execution) groupInfra(names []string, detail string) *chainEnd {
	e.emit.emit(protocol.EventError, "", "engine_error", map[string]any{
		"parallel_group": names, "error": detail,
	})
	return &chainEnd{endFailed, fmt.Sprintf("parallel group [%s]: %s", strings.Join(names, ", "), detail)}
}

// memberView returns the member's private view, creating it — and its own
// ephemeral HOME under the attempt's scratch family — on first use. A view
// lives until the HOMEs are wiped, so a member keeps its session across
// group runs just as a sequential phase keeps its role's session across
// rerun-self.
func (e *execution) memberView(index int, member protocol.PhaseSpec) (*execution, error) {
	if view := e.members[member.Name]; view != nil {
		return view, nil
	}
	home := filepath.Join(e.scratch.members, strconv.Itoa(index))
	if err := os.MkdirAll(home, 0o700); err != nil {
		return nil, fmt.Errorf("create ephemeral HOME for member %q: %w", member.Name, err)
	}
	scratch := *e.scratch
	scratch.home = home
	scratch.handoff = filepath.Join(e.scratch.memberHandoff, strconv.Itoa(index))
	view := &execution{
		runner:      e.runner,
		attempt:     e.attempt,
		spec:        e.spec,
		capability:  e.capability,
		emit:        e.emit,
		scratch:     &scratch,
		timeouts:    e.timeouts,
		deadline:    e.deadline,
		sessions:    make(map[string]*runtime.Session),
		transcripts: make(map[string][]exchange),
		seededRoles: make(map[string]bool),
		sessionKey:  e.sessionKey,
		counters:    e.counters,
		grouped:     true,
	}
	if e.members == nil {
		e.members = make(map[string]*execution)
	}
	e.members[member.Name] = view
	return view, nil
}

// beginGroupRun clears what one group run's merge reads, keeping what the
// member carries across runs: its session, transcript, and seeded HOME.
//
// Its private handoff directory is rebuilt every run from the chain's
// handoff directory — the notes every phase before the group left, which a
// sequential reviewer could read too — minus handoff/parallel, where members'
// own merged notes live: no member reads a sibling's notes. What was seeded
// is fingerprinted, so the join publishes only what this member wrote.
func (e *execution) beginGroupRun(shared string) error {
	e.results = nil
	e.pendingFields = nil
	e.gateReports = make(map[string]protocol.GateReport)
	e.touchedPaths = make(map[string]bool)
	if err := os.RemoveAll(e.scratch.handoff); err != nil {
		return fmt.Errorf("reset member handoff directory: %w", err)
	}
	if err := os.MkdirAll(e.scratch.handoff, 0o700); err != nil {
		return fmt.Errorf("create member handoff directory: %w", err)
	}
	e.handoffSeed = make(map[string]string)
	err := copyNotes(shared, e.scratch.handoff, func(relative string, digest string) bool {
		if relative == memberNotesRoot || strings.HasPrefix(relative, memberNotesRoot+string(filepath.Separator)) {
			return false
		}
		e.handoffSeed[relative] = digest
		return true
	})
	if err != nil {
		return fmt.Errorf("seed member handoff directory: %w", err)
	}
	return nil
}

// memberNotesRoot is where members' merged notes live inside the chain's
// handoff directory.
const memberNotesRoot = "parallel"

// memberHandoffFolder is where a member's notes land inside the chain's
// handoff directory: handoff/parallel/<phase>, or the member's index when
// its phase name is not a plain path element.
func memberHandoffFolder(shared string, index int, phase string) string {
	name := phase
	if !filepath.IsLocal(name) || strings.ContainsAny(name, `/\`) {
		name = "member-" + strconv.Itoa(index)
	}
	return filepath.Join(shared, memberNotesRoot, name)
}

// publishMemberHandoff copies the notes a member wrote or changed — not the
// pre-group notes it was seeded with — into the chain's handoff directory,
// replacing what an earlier run of the same member put there.
func publishMemberHandoff(private, shared string, index int, phase string, seed map[string]string) error {
	target := memberHandoffFolder(shared, index, phase)
	if err := os.RemoveAll(target); err != nil {
		return err
	}
	return copyNotes(private, target, func(relative string, digest string) bool {
		seeded, found := seed[relative]
		return !found || seeded != digest
	})
}

// copyNotes copies the regular files under source into destination,
// creating directories as files need them, for every file keep accepts
// (given its path relative to source and a digest of its content). Nothing
// but regular files is copied: a symlink is how an agent would point the
// next phase at something outside the scratch family. A missing source
// copies nothing.
func copyNotes(source, destination string, keep func(relative, digest string) bool) error {
	if _, err := os.Lstat(source); os.IsNotExist(err) {
		return nil
	}
	return filepath.WalkDir(source, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if !entry.Type().IsRegular() {
			return nil
		}
		relative, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(body)
		if !keep(relative, hex.EncodeToString(sum[:])) {
			return nil
		}
		target := filepath.Join(destination, relative)
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			return err
		}
		return os.WriteFile(target, body, 0o600)
	})
}
