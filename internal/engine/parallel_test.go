// parallel_test.go — the parallel read-only reviewer group (plan U5, R8–R11)
// on the scripted runtime. Members run concurrently in private views and
// merge in declared order; repair edges resolve after the join; any
// worktree change during the group is a breach.
package engine_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/StructuPath/jig/internal/engine"
	"github.com/StructuPath/jig/internal/engine/enginetest"
	"github.com/StructuPath/jig/internal/protocol"
	"github.com/StructuPath/jig/internal/worker"
)

// panelRoster: a builder, three read-only reviewers with distinct system
// prompts (the fake routes on them), and a read-only phase after the panel.
const panelRoster = `
roster:
  builder:
    model: test-model
    system_prompt: Build.
    user_prompt: "Build the app."
    writes: ["src/"]
  correctness:
    model: test-model
    system_prompt: Review correctness.
    user_prompt: "Review the work."
    writes: []
  security:
    model: test-model
    system_prompt: Review security.
    user_prompt: "Review the work."
    writes: []
  maintainability:
    model: test-model
    system_prompt: Review maintainability.
    user_prompt: "Review the work."
    writes: []
  closer:
    model: test-model
    system_prompt: Close.
    user_prompt: "Wrap up."
    writes: []
`

// panelSnapshot is the panel chain with the given edges and group line.
// Every reviewer carries a verdict gate and a rejection edge back to build.
func panelSnapshot(edges map[string]string, group string) string {
	edge := func(name string) string {
		if custom, found := edges[name]; found {
			return custom
		}
		return `{when: "approved == false", run: build, then: rerun-self, budget: 1, exhausted: fail-job}`
	}
	return "name: panel\n" + panelRoster + `
phases:
  - {name: build, kind: agent, owner: builder}
  - name: review-correctness
    kind: agent
    owner: correctness
    gates: [{name: verdict_consistent}]
    on_fail: ` + edge("review-correctness") + `
  - name: review-security
    kind: agent
    owner: security
    gates: [{name: verdict_consistent}]
    on_fail: ` + edge("review-security") + `
  - name: review-maintainability
    kind: agent
    owner: maintainability
    gates: [{name: verdict_consistent}]
    on_fail: ` + edge("review-maintainability") + `
  - {name: wrap-up, kind: agent, owner: closer}
` + group + `
acceptance: [all_phases_passed, verdict_consistent]
publish:
  ci:
    wait: true
    on_fail: {run: build, budget: 1}
`
}

const panelGroup = "parallel: [review-correctness, review-security, review-maintainability]"

var (
	buildRole           = enginetest.ForSystemPrompt("Build.")
	correctnessRole     = enginetest.ForSystemPrompt("Review correctness.")
	securityRole        = enginetest.ForSystemPrompt("Review security.")
	maintainabilityRole = enginetest.ForSystemPrompt("Review maintainability.")
	closerRole          = enginetest.ForSystemPrompt("Close.")
)

func approve(summary string, extra map[string]any) enginetest.Step {
	fields := map[string]any{"approved": true, "blocking": []any{}}
	for key, value := range extra {
		fields[key] = value
	}
	return success(summary, fields)
}

func reject(summary string) enginetest.Step {
	return success(summary, map[string]any{"approved": false, "blocking": []any{summary}})
}

// normalizedSummary is the outcome's result with the wall-clock stamps
// removed, so a grouped and an ungrouped run can be compared exactly.
func normalizedSummary(t *testing.T, result string) map[string]any {
	t.Helper()
	var summary map[string]any
	if err := json.Unmarshal([]byte(result), &summary); err != nil {
		t.Fatalf("result does not parse: %v\n%s", err, result)
	}
	phases, _ := summary["phases"].([]any)
	for _, phase := range phases {
		entry := phase.(map[string]any)
		delete(entry, "started_at")
		delete(entry, "ended_at")
	}
	return summary
}

func callsFor(fake *enginetest.Runtime, match func(enginetest.Call) bool) []enginetest.Call {
	var matched []enginetest.Call
	for _, call := range fake.Calls() {
		if match(call) {
			matched = append(matched, call)
		}
	}
	return matched
}

// scriptPanel scripts the panel identically for a grouped and an ungrouped
// run: prompt-independent reviewers, so the only thing that differs is how
// the engine runs them. Security's first emission omits its verdict, so its
// own gate correction goes to its own session inside the group.
func scriptPanel(fake *enginetest.Runtime) {
	fake.Route(buildRole, writes(map[string]string{"src/app.txt": "app"}, "built the app"))
	fake.Route(correctnessRole, approve("correct", map[string]any{"score": 1.0, "correctness": "ok"}))
	fake.Route(securityRole,
		success("forgot the verdict", map[string]any{"score": 2.0}),
		approve("secure", map[string]any{"score": 2.0, "security": "ok"}))
	fake.Route(maintainabilityRole, approve("maintainable", map[string]any{"score": 3.0}))
	fake.Route(closerRole, success("wrapped up", nil))
}

// R9: grouped and ungrouped runs of prompt-independent reviewers merge
// identically — results, gate reports, acceptance, and the field view — and
// every member's prompt carries the pre-group envelope, while the phase
// after the group gets the last member's envelope.
func TestAGroupMergesExactlyAsTheSameReviewersWouldSequentially(t *testing.T) {
	run := func(group string) (*repairFixture, map[string]any, map[string]any) {
		f := newRepairFixture(t, panelSnapshot(nil, group), nil)
		scriptPanel(f.fake)
		outcome := f.execute(t)
		if outcome.State != protocol.AttemptAcceptedUnpublished || outcome.Continuation == nil {
			t.Fatalf("%q: outcome = %s (%s), want accepted with a continuation", group, outcome.State, outcome.Error)
		}
		t.Cleanup(outcome.Continuation.Release)
		if f.fake.Remaining() != 0 {
			t.Fatalf("%q: unconsumed scripted steps: %d", group, f.fake.Remaining())
		}
		return f, normalizedSummary(t, outcome.Result), engine.FieldViewOf(outcome.Continuation)
	}
	_, sequential, sequentialView := run("")
	grouped, parallel, parallelView := run(panelGroup)

	if !reflect.DeepEqual(parallel, sequential) {
		a, _ := json.MarshalIndent(parallel, "", "  ")
		b, _ := json.MarshalIndent(sequential, "", "  ")
		t.Fatalf("grouped summary differs from sequential\ngrouped:\n%s\nsequential:\n%s", a, b)
	}
	if !reflect.DeepEqual(parallelView, sequentialView) {
		t.Fatalf("grouped field view %v differs from sequential %v", parallelView, sequentialView)
	}
	if parallelView["score"] != 3.0 {
		t.Fatalf("score = %v, want the last member's 3 (declared-order merge)", parallelView["score"])
	}

	// Every member saw the builder's envelope, and no sibling's.
	for _, member := range []func(enginetest.Call) bool{correctnessRole, securityRole, maintainabilityRole} {
		first := callsFor(grouped.fake, member)[0]
		if !strings.Contains(first.Prompt, "built the app") {
			t.Fatalf("member prompt lacks the pre-group envelope: %.400q", first.Prompt)
		}
		for _, sibling := range []string{`"correct"`, `"secure"`, `"maintainable"`} {
			if strings.Contains(first.Prompt, sibling) {
				t.Fatalf("member prompt carries a sibling's envelope %s: %.400q", sibling, first.Prompt)
			}
		}
	}
	closer := callsFor(grouped.fake, closerRole)[0]
	if !strings.Contains(closer.Prompt, `"maintainable"`) || strings.Contains(closer.Prompt, `"secure"`) {
		t.Fatalf("the phase after the group must get the last member's envelope: %.400q", closer.Prompt)
	}
	// Security's gate correction stayed in its own session.
	security := callsFor(grouped.fake, securityRole)
	if len(security) != 2 || security[0].SessionKey != security[1].SessionKey {
		t.Fatalf("security calls = %+v, want a correction in the same session", security)
	}
}

// executeWithin runs the fixture's attempt and fails the test if it does not
// return in time — the shape of a deadlock.
func executeWithin(t *testing.T, f *repairFixture, limit time.Duration) worker.Outcome {
	t.Helper()
	done := make(chan worker.Outcome, 1)
	go func() { done <- f.runner.Execute(context.Background(), f.attempt) }()
	select {
	case outcome := <-done:
		if outcome.Continuation != nil {
			t.Cleanup(outcome.Continuation.Release)
		}
		return outcome
	case <-time.After(limit):
		t.Fatalf("the attempt did not finish within %s", limit)
		return worker.Outcome{}
	}
}

// Members run concurrently: two reviewers that each hold their result until
// the other has started both finish, where one-at-a-time would deadlock.
// Session keys stay unique across members and the chain, and the trace's
// seq stays strictly increasing and gapless while members interleave.
func TestGroupMembersRunConcurrentlyUnderUniqueSessionsAndOneSequence(t *testing.T) {
	f := newRepairFixture(t, panelSnapshot(nil, panelGroup), nil)
	correctnessStarted, securityStarted := make(chan struct{}), make(chan struct{})
	correctness := approve("correct", nil)
	correctness.Started, correctness.WaitFor = correctnessStarted, securityStarted
	security := approve("secure", nil)
	security.Started, security.WaitFor = securityStarted, correctnessStarted
	f.fake.Route(buildRole, writes(map[string]string{"src/app.txt": "app"}, "built the app"))
	f.fake.Route(correctnessRole, correctness)
	f.fake.Route(securityRole, security)
	f.fake.Route(maintainabilityRole, approve("maintainable", nil))
	f.fake.Route(closerRole, success("wrapped up", nil))

	outcome := executeWithin(t, f, 20*time.Second)
	if outcome.State != protocol.AttemptAcceptedUnpublished {
		t.Fatalf("outcome = %s (%s), want accepted", outcome.State, outcome.Error)
	}

	keys := map[string]string{}
	for _, call := range f.fake.Calls() {
		if owner, taken := keys[call.SessionKey]; taken && owner != call.Options.SystemPrompt {
			t.Fatalf("session key %q is shared by %q and %q", call.SessionKey, owner, call.Options.SystemPrompt)
		}
		keys[call.SessionKey] = call.Options.SystemPrompt
	}
	if len(keys) != 5 {
		t.Fatalf("session keys = %v, want one per role (5)", keys)
	}
	for i, event := range f.sink.all() {
		if event.Seq != int64(i+1) {
			t.Fatalf("event %d has seq %d: seq must be strictly increasing without gaps", i, event.Seq)
		}
	}
}

func repairEdges(sink *recordingSink) []string {
	var phases []string
	for _, event := range sink.all() {
		if event.Type == protocol.EventLog && event.Name == "repair_edge" {
			phases = append(phases, event.Phase)
		}
	}
	return phases
}

// R10: when two members reject, only the first in declared order dispatches
// the builder, with its own envelope, and the whole group runs again. Only
// that member's budget is charged: the second member still has its one use
// when it rejects on the next run.
func TestOnlyTheFirstRejectingMemberDispatchesAndOnlyItsBudgetIsCharged(t *testing.T) {
	f := newRepairFixture(t, panelSnapshot(nil, panelGroup), nil)
	f.fake.Route(buildRole,
		writes(map[string]string{"src/app.txt": "v1"}, "built the app"),
		writes(map[string]string{"src/app.txt": "v2"}, "rebuilt once"),
		writes(map[string]string{"src/app.txt": "v3"}, "rebuilt twice"))
	f.fake.Route(correctnessRole, reject("correctness-blocker"), approve("correct", nil), approve("correct", nil))
	f.fake.Route(securityRole, reject("security-blocker-1"), reject("security-blocker-2"), approve("secure", nil))
	f.fake.Route(maintainabilityRole,
		approve("maintainable", nil), approve("maintainable", nil), approve("maintainable", nil))
	f.fake.Route(closerRole, success("wrapped up", nil))

	outcome := executeWithin(t, f, 20*time.Second)
	if outcome.State != protocol.AttemptAcceptedUnpublished {
		t.Fatalf("outcome = %s (%s), want accepted", outcome.State, outcome.Error)
	}
	if f.fake.Remaining() != 0 {
		t.Fatalf("unconsumed scripted steps: %d", f.fake.Remaining())
	}
	if got := repairEdges(f.sink); !reflect.DeepEqual(got, []string{"review-correctness", "review-security"}) {
		t.Fatalf("repair edges = %v, want correctness's then (next run) security's", got)
	}
	if got := f.sink.count(protocol.EventLog, "parallel_group_start"); got != 3 {
		t.Fatalf("group runs = %d, want 3", got)
	}
	builds := callsFor(f.fake, buildRole)
	if !strings.Contains(builds[1].Prompt, "correctness-blocker") || strings.Contains(builds[1].Prompt, "security-blocker") {
		t.Fatalf("the first dispatch must carry only the first rejecting member's envelope: %.500q", builds[1].Prompt)
	}
	if !strings.Contains(builds[2].Prompt, "security-blocker-2") {
		t.Fatalf("the second dispatch must carry security's second rejection: %.500q", builds[2].Prompt)
	}
	// The rerun group sees the repaired envelope as its previous.
	if second := callsFor(f.fake, correctnessRole)[1]; !strings.Contains(second.Prompt, "rebuilt once") {
		t.Fatalf("the rerun group's members must see the repair's envelope: %.500q", second.Prompt)
	}
}

const (
	proceedEdge = `{when: "approved == false", run: build, then: rerun-self, budget: 1, exhausted: proceed}`
)

// R10: the first member triggers with its budget spent under proceed, so the
// walk moves on and the second member dispatches.
func TestAnExhaustedProceedMemberLetsTheNextMemberDispatch(t *testing.T) {
	f := newRepairFixture(t, panelSnapshot(map[string]string{"review-correctness": proceedEdge}, panelGroup), nil)
	f.fake.Route(buildRole,
		writes(map[string]string{"src/app.txt": "v1"}, "built the app"),
		writes(map[string]string{"src/app.txt": "v2"}, "rebuilt once"),
		writes(map[string]string{"src/app.txt": "v3"}, "rebuilt twice"))
	f.fake.Route(correctnessRole, reject("c-1"), reject("c-2"), reject("c-3"))
	f.fake.Route(securityRole, approve("secure", nil), reject("security-blocker"), approve("secure", nil))
	f.fake.Route(maintainabilityRole,
		approve("maintainable", nil), approve("maintainable", nil), approve("maintainable", nil))
	f.fake.Route(closerRole, success("wrapped up", nil))

	outcome := executeWithin(t, f, 20*time.Second)
	if outcome.State != protocol.AttemptAcceptedUnpublished {
		t.Fatalf("outcome = %s (%s), want accepted", outcome.State, outcome.Error)
	}
	if got := repairEdges(f.sink); !reflect.DeepEqual(got, []string{"review-correctness", "review-security"}) {
		t.Fatalf("repair edges = %v, want correctness's, then security's past correctness's exhaustion", got)
	}
	if got := f.sink.count(protocol.EventLog, "repair_exhausted"); got != 2 {
		t.Fatalf("repair_exhausted events = %d, want 2 (correctness on runs 2 and 3)", got)
	}
	if builds := callsFor(f.fake, buildRole); !strings.Contains(builds[2].Prompt, "security-blocker") {
		t.Fatalf("the second dispatch must carry security's rejection: %.500q", builds[2].Prompt)
	}
}

// R10: the first member triggers with its budget spent under fail-job: the
// attempt ends there, and no later member dispatches.
func TestAnExhaustedFailJobMemberEndsTheAttempt(t *testing.T) {
	f := newRepairFixture(t, panelSnapshot(nil, panelGroup), nil)
	f.fake.Route(buildRole,
		writes(map[string]string{"src/app.txt": "v1"}, "built the app"),
		writes(map[string]string{"src/app.txt": "v2"}, "rebuilt once"))
	f.fake.Route(correctnessRole, reject("c-1"), reject("c-2"))
	f.fake.Route(securityRole, approve("secure", nil), reject("security-blocker"))
	f.fake.Route(maintainabilityRole, approve("maintainable", nil), approve("maintainable", nil))

	outcome := executeWithin(t, f, 20*time.Second)
	if outcome.State != protocol.AttemptFailed || !strings.Contains(outcome.Error, `"review-correctness": repair budget (1) exhausted`) {
		t.Fatalf("outcome = %s (%s), want failed on correctness's exhausted budget", outcome.State, outcome.Error)
	}
	if got := len(callsFor(f.fake, buildRole)); got != 2 {
		t.Fatalf("build ran %d time(s), want 2: nothing may dispatch past a fail-job exhaustion", got)
	}
}

// R10's bound: total group runs are at most one plus the sum of member
// budgets. Reviewers that never approve, under proceed, reach it exactly.
func TestGroupRunsAreBoundedByOnePlusTheSumOfMemberBudgets(t *testing.T) {
	proceed := func(budget int) string {
		return `{when: "approved == false", run: build, then: rerun-self, budget: ` +
			strconv.Itoa(budget) + `, exhausted: proceed}`
	}
	f := newRepairFixture(t, panelSnapshot(map[string]string{
		"review-correctness": proceed(1), "review-security": proceed(2), "review-maintainability": proceed(1),
	}, panelGroup), nil)
	const runs = 1 + 1 + 2 + 1
	for i := 0; i < runs; i++ {
		f.fake.Route(buildRole, writes(map[string]string{"src/app.txt": strconv.Itoa(i)}, "built"))
		f.fake.Route(correctnessRole, reject("c"))
		f.fake.Route(securityRole, reject("s"))
		f.fake.Route(maintainabilityRole, reject("m"))
	}
	f.fake.Route(closerRole, success("wrapped up", nil))

	outcome := executeWithin(t, f, 30*time.Second)
	if outcome.State != protocol.AttemptAcceptedUnpublished {
		t.Fatalf("outcome = %s (%s), want accepted (every exhaustion proceeds)", outcome.State, outcome.Error)
	}
	if got := f.sink.count(protocol.EventLog, "parallel_group_start"); got != runs {
		t.Fatalf("group runs = %d, want exactly %d", got, runs)
	}
	if f.fake.Remaining() != 0 {
		t.Fatalf("unconsumed scripted steps: %d", f.fake.Remaining())
	}
}

func phaseStatuses(t *testing.T, result string) map[string][]string {
	t.Helper()
	var summary struct {
		Phases []protocol.PhaseResult `json:"phases"`
	}
	if err := json.Unmarshal([]byte(result), &summary); err != nil {
		t.Fatalf("result does not parse: %v", err)
	}
	statuses := map[string][]string{}
	for _, phase := range summary.Phases {
		statuses[phase.Phase] = append(statuses[phase.Phase], phase.Status)
	}
	return statuses
}

// R11: a member that writes breaches: the attempt aborts, the write is
// rolled back to the group snapshot, and no member's view is merged.
func TestAMemberWriteIsABreachThatAbortsWithNoPartialMerge(t *testing.T) {
	f := newRepairFixture(t, panelSnapshot(nil, panelGroup), nil)
	sneaky := approve("correct", nil)
	sneaky.Files = map[string]string{"notes.txt": "a reviewer's scratch"}
	f.fake.Route(buildRole, writes(map[string]string{"src/app.txt": "app"}, "built the app"))
	f.fake.Route(correctnessRole, sneaky)
	f.fake.Route(securityRole, approve("secure", nil))
	f.fake.Route(maintainabilityRole, approve("maintainable", nil))

	outcome := executeWithin(t, f, 20*time.Second)
	if outcome.State != protocol.AttemptFailed || !strings.Contains(outcome.Error, "notes.txt") ||
		!strings.Contains(outcome.Error, "parallel group") {
		t.Fatalf("outcome = %s (%s), want failed on a group breach naming notes.txt", outcome.State, outcome.Error)
	}
	if _, err := os.Stat(filepath.Join(f.repo, "notes.txt")); !os.IsNotExist(err) {
		t.Fatalf("the breaching write survived: %v", err)
	}
	for _, member := range []string{"review-correctness", "review-security", "review-maintainability"} {
		if got := phaseStatuses(t, outcome.Result)[member]; !reflect.DeepEqual(got, []string{protocol.EnvelopeFail}) {
			t.Fatalf("%s results = %v, want one failed breach result and nothing merged", member, got)
		}
	}
	if !f.sink.has(protocol.EventError, "write_boundary_breach") {
		t.Fatal("no write_boundary_breach event")
	}
}

// R11: a member that dies leaving a write is a breach too — the member's
// death does not roll the tree back; the group's one enforcement finds it.
func TestADeadMembersLeftoverWriteIsABreach(t *testing.T) {
	f := newRepairFixture(t, panelSnapshot(nil, panelGroup), nil)
	f.fake.Route(buildRole, writes(map[string]string{"src/app.txt": "app"}, "built the app"))
	f.fake.Route(correctnessRole,
		enginetest.Step{Crash: true, Files: map[string]string{"left-behind.txt": "half a thought"}},
		approve("correct", nil))
	f.fake.Route(securityRole, approve("secure", nil))
	f.fake.Route(maintainabilityRole, approve("maintainable", nil))

	outcome := executeWithin(t, f, 20*time.Second)
	if outcome.State != protocol.AttemptFailed || !strings.Contains(outcome.Error, "left-behind.txt") {
		t.Fatalf("outcome = %s (%s), want failed on the dead member's leftover write", outcome.State, outcome.Error)
	}
	if _, err := os.Stat(filepath.Join(f.repo, "left-behind.txt")); !os.IsNotExist(err) {
		t.Fatalf("the dead member's write survived: %v", err)
	}
}

func eventSeq(t *testing.T, sink *recordingSink, eventType, phase string, match func(protocol.Event) bool) int64 {
	t.Helper()
	for _, event := range sink.all() {
		if event.Type == eventType && event.Phase == phase && (match == nil || match(event)) {
			return event.Seq
		}
	}
	t.Fatalf("no %s event for %s", eventType, phase)
	return 0
}

// A member death re-enters only that member, after its siblings finish, and
// the siblings' results are kept rather than re-run.
func TestAMemberDeathReentersOnlyThatMemberAfterSiblingsFinish(t *testing.T) {
	f := newRepairFixture(t, panelSnapshot(nil, panelGroup), nil)
	correctnessStarted := make(chan struct{})
	slowSecurity := approve("secure", nil)
	slowSecurity.WaitFor, slowSecurity.Delay = correctnessStarted, 300*time.Millisecond
	f.fake.Route(buildRole, writes(map[string]string{"src/app.txt": "app"}, "built the app"))
	f.fake.Route(correctnessRole,
		enginetest.Step{Crash: true, Started: correctnessStarted},
		approve("correct", nil))
	f.fake.Route(securityRole, slowSecurity)
	f.fake.Route(maintainabilityRole, approve("maintainable", nil))
	f.fake.Route(closerRole, success("wrapped up", nil))

	outcome := executeWithin(t, f, 20*time.Second)
	if outcome.State != protocol.AttemptAcceptedUnpublished {
		t.Fatalf("outcome = %s (%s), want accepted", outcome.State, outcome.Error)
	}
	reentry := eventSeq(t, f.sink, protocol.EventPhaseStart, "review-correctness", func(event protocol.Event) bool {
		return strings.Contains(string(event.Payload), `"phase_attempt":2`)
	})
	for _, sibling := range []string{"review-security", "review-maintainability"} {
		if end := eventSeq(t, f.sink, protocol.EventPhaseEnd, sibling, nil); end > reentry {
			t.Fatalf("correctness re-entered (seq %d) before %s finished (seq %d)", reentry, sibling, end)
		}
	}
	statuses := phaseStatuses(t, outcome.Result)
	if got := statuses["review-correctness"]; !reflect.DeepEqual(got, []string{protocol.EnvelopeFail, protocol.EnvelopeSuccess}) {
		t.Fatalf("correctness results = %v, want its death then its re-entry", got)
	}
	if got := statuses["review-security"]; !reflect.DeepEqual(got, []string{protocol.EnvelopeSuccess}) {
		t.Fatalf("security results = %v, want its one kept result", got)
	}
	if calls := callsFor(f.fake, correctnessRole); calls[0].SessionKey == calls[1].SessionKey {
		t.Fatalf("the re-entry must use a fresh session, got %q twice", calls[0].SessionKey)
	}
}

const twoMemberGroup = "parallel: [review-correctness, review-security]"

// A repair that flips the field a member's guard reads must not skip the
// member that rejected it: guards are judged on the group's first run only.
// Sequentially, rerun-self never re-checks a guard, and a skipped phase
// counts as passed — so a re-judged guard would let a repair silence its own
// reviewer. A member skipped on the first run stays skipped.
func TestARepairCannotFlipAGuardToSkipTheReviewerThatRejectedIt(t *testing.T) {
	snapshot := strings.Replace(panelSnapshot(nil, panelGroup),
		"    owner: security\n", "    owner: security\n    if: \"touches_auth == true\"\n", 1)
	snapshot = strings.Replace(snapshot,
		"    owner: maintainability\n", "    owner: maintainability\n    if: \"touches_db == true\"\n", 1)
	f := newRepairFixture(t, snapshot, nil)
	built := writes(map[string]string{"src/app.txt": "v1"}, "built the app")
	built.Text = envelope(map[string]any{"status": "success", "summary": "built the app",
		"touches_auth": true, "touches_db": false})
	rebuilt := writes(map[string]string{"src/app.txt": "v2"}, "rebuilt")
	rebuilt.Text = envelope(map[string]any{"status": "success", "summary": "moved the auth check away",
		"touches_auth": false, "touches_db": true})
	f.fake.Route(buildRole, built, rebuilt)
	f.fake.Route(correctnessRole, approve("correct", nil), approve("correct", nil))
	f.fake.Route(securityRole, reject("auth bypass"), approve("secure now", nil))
	f.fake.Route(closerRole, success("wrapped up", nil))

	outcome := executeWithin(t, f, 20*time.Second)
	if outcome.State != protocol.AttemptAcceptedUnpublished {
		t.Fatalf("outcome = %s (%s), want accepted", outcome.State, outcome.Error)
	}
	if got := len(callsFor(f.fake, securityRole)); got != 2 {
		t.Fatalf("security ran %d time(s), want 2: the repair flipped its guard, and it must re-judge anyway", got)
	}
	if got := len(callsFor(f.fake, maintainabilityRole)); got != 0 {
		t.Fatalf("maintainability ran %d time(s): skipped on the first run, it must stay skipped", got)
	}
	statuses := phaseStatuses(t, outcome.Result)["review-security"]
	if statuses[len(statuses)-1] != protocol.EnvelopeSuccess {
		t.Fatalf("security's last result = %v, want its approval, never a skip", statuses)
	}
}

var handoffLine = regexp.MustCompile(`Share working notes for later phases in: (\S+)`)

func handoffDir(t *testing.T, prompt string) string {
	t.Helper()
	match := handoffLine.FindStringSubmatch(prompt)
	if match == nil {
		t.Errorf("prompt names no handoff directory: %.300q", prompt)
		return ""
	}
	return match[1]
}

// Members never share a handoff directory: a sibling cannot read another's
// notes mid-run, and two notes with the same name both survive, merged into
// the chain's handoff directory in declared order under per-member folders.
func TestMembersKeepPrivateHandoffNotesThatMergeAtTheJoin(t *testing.T) {
	f := newRepairFixture(t, panelSnapshot(nil, twoMemberGroup), nil)
	built := writes(map[string]string{"src/app.txt": "app"}, "built the app")
	built.Do = func(call enginetest.Call) {
		// Notes a phase before the group left, and a stand-in for a
		// sibling's notes merged on an earlier group run.
		dir := handoffDir(t, call.Prompt)
		for name, body := range map[string]string{
			"plan.md": "the plan", filepath.Join("parallel", "review-security", "notes.md"): "sibling's",
		} {
			if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, name)), 0o700); err != nil {
				t.Errorf("mkdir: %v", err)
			}
			if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
				t.Errorf("write %s: %v", name, err)
			}
		}
	}
	seesPreGroupNotesOnly := func(dir string) {
		if body, err := os.ReadFile(filepath.Join(dir, "plan.md")); err != nil || string(body) != "the plan" {
			t.Errorf("a member cannot read the notes earlier phases left: %q, %v", body, err)
		}
		if _, err := os.Stat(filepath.Join(dir, "parallel")); !os.IsNotExist(err) {
			t.Errorf("a member was seeded with members' merged notes: %v", err)
		}
	}
	wrote := make(chan struct{})
	correctness := approve("correct", nil)
	correctness.Do = func(call enginetest.Call) {
		defer close(wrote)
		dir := handoffDir(t, call.Prompt)
		seesPreGroupNotesOnly(dir)
		if err := os.WriteFile(filepath.Join(dir, "notes.md"), []byte("from correctness"), 0o600); err != nil {
			t.Errorf("write correctness notes: %v", err)
		}
	}
	security := approve("secure", nil)
	security.Do = func(call enginetest.Call) {
		<-wrote
		dir := handoffDir(t, call.Prompt)
		seesPreGroupNotesOnly(dir)
		if _, err := os.Stat(filepath.Join(dir, "notes.md")); !os.IsNotExist(err) {
			t.Errorf("security can see a sibling's notes mid-run in %s: %v", dir, err)
		}
		if err := os.WriteFile(filepath.Join(dir, "notes.md"), []byte("from security"), 0o600); err != nil {
			t.Errorf("write security notes: %v", err)
		}
		// An edit to a seeded note is the member's own work, and is kept.
		if err := os.WriteFile(filepath.Join(dir, "plan.md"), []byte("the plan, annotated"), 0o600); err != nil {
			t.Errorf("annotate the plan: %v", err)
		}
	}
	f.fake.Route(buildRole, built)
	f.fake.Route(correctnessRole, correctness)
	f.fake.Route(securityRole, security)
	f.fake.Route(maintainabilityRole, approve("maintainable", nil))
	f.fake.Route(closerRole, success("wrapped up", nil))

	outcome := executeWithin(t, f, 20*time.Second)
	if outcome.State != protocol.AttemptAcceptedUnpublished {
		t.Fatalf("outcome = %s (%s), want accepted", outcome.State, outcome.Error)
	}
	shared := filepath.Join(f.scratch, "attempt-1", "handoff")
	for member, want := range map[string]string{
		"review-correctness": "from correctness", "review-security": "from security",
	} {
		body, err := os.ReadFile(filepath.Join(shared, "parallel", member, "notes.md"))
		if err != nil || string(body) != want {
			t.Fatalf("%s's merged notes = %q, %v; want %q", member, body, err, want)
		}
	}
	// Only what a member wrote or changed is published, not what it was
	// seeded with: correctness left the plan alone, security annotated it.
	if _, err := os.Stat(filepath.Join(shared, "parallel", "review-correctness", "plan.md")); !os.IsNotExist(err) {
		t.Fatalf("correctness republished the pre-group notes it was seeded with: %v", err)
	}
	annotated, err := os.ReadFile(filepath.Join(shared, "parallel", "review-security", "plan.md"))
	if err != nil || string(annotated) != "the plan, annotated" {
		t.Fatalf("security's edit to a seeded note = %q, %v; want it published", annotated, err)
	}
	if body, err := os.ReadFile(filepath.Join(shared, "plan.md")); err != nil || string(body) != "the plan" {
		t.Fatalf("the pre-group notes changed: %q, %v", body, err)
	}
	if got := handoffDir(t, callsFor(f.fake, closerRole)[0].Prompt); got != shared {
		t.Fatalf("the phase after the group was pointed at %s, want the chain's handoff %s", got, shared)
	}
}

// A member that panics is a panic in the chain, but only after the group's
// one enforcement: a sibling's worktree write is rolled back first.
func TestAPanickingMemberStillHasItsSiblingsWritesRolledBack(t *testing.T) {
	f := newRepairFixture(t, panelSnapshot(nil, twoMemberGroup), nil)
	correctnessStarted := make(chan struct{})
	f.fake.Route(buildRole, writes(map[string]string{"src/app.txt": "app"}, "built the app"))
	f.fake.Route(correctnessRole, enginetest.Step{Hang: true, Started: correctnessStarted,
		Files: map[string]string{"planted.txt": "written before the crash"}})
	f.fake.Route(securityRole, enginetest.Step{Do: func(enginetest.Call) {
		<-correctnessStarted
		panic("runtime blew up")
	}})

	recovered := func() (value any) {
		defer func() { value = recover() }()
		f.runner.Execute(context.Background(), f.attempt)
		return nil
	}()
	if recovered != "runtime blew up" {
		t.Fatalf("Execute recovered %v, want the member's panic re-raised", recovered)
	}
	if _, err := os.Stat(filepath.Join(f.repo, "planted.txt")); !os.IsNotExist(err) {
		t.Fatalf("a sibling's write survived the panic: %v", err)
	}
	if !f.sink.has(protocol.EventError, "parallel_group_panic") {
		t.Fatal("no parallel_group_panic event recording what enforcement found")
	}
}

// A member that fails without firing its edge ends the attempt at the join,
// as it would have sequentially: nothing after the group runs.
func TestAnUntriggeredFailingMemberEndsTheAttempt(t *testing.T) {
	f := newRepairFixture(t, panelSnapshot(nil, panelGroup), nil)
	failing := enginetest.Step{Text: envelope(map[string]any{
		"status": "fail", "summary": "could not finish the review", "approved": true, "blocking": []any{}})}
	f.fake.Route(buildRole, writes(map[string]string{"src/app.txt": "app"}, "built the app"))
	f.fake.Route(correctnessRole, approve("correct", nil))
	f.fake.Route(securityRole, failing)
	f.fake.Route(maintainabilityRole, approve("maintainable", nil))
	f.fake.Route(closerRole, success("wrapped up", nil))

	outcome := executeWithin(t, f, 20*time.Second)
	if outcome.State != protocol.AttemptFailed || !strings.Contains(outcome.Error, `"review-security"`) ||
		strings.Contains(outcome.Error, "acceptance") {
		t.Fatalf("outcome = %s (%s), want failed on review-security itself", outcome.State, outcome.Error)
	}
	if calls := callsFor(f.fake, closerRole); len(calls) != 0 {
		t.Fatal("the phase after the group ran after a member failed")
	}
}

// A member that fails on a runtime error takes the fail exit, which inside a
// group must not run a boundary check of its own: the member never took a
// snapshot, so it would read the builder's authorized work as the member's
// breach and roll it back. The group's one enforcement is the only check.
func TestAMemberRuntimeErrorFailsWithoutItsOwnBoundaryCheck(t *testing.T) {
	f := newRepairFixture(t, panelSnapshot(nil, twoMemberGroup), nil)
	f.fake.Route(buildRole, writes(map[string]string{"src/app.txt": "app"}, "built the app"))
	f.fake.Route(correctnessRole, approve("correct", nil))
	f.fake.Route(securityRole, enginetest.Step{IsError: true, ExitCode: 1, Text: "rate limit exceeded"})

	outcome := executeWithin(t, f, 20*time.Second)
	if outcome.State != protocol.AttemptFailed || !strings.Contains(outcome.Error, "rate limit exceeded") ||
		strings.Contains(outcome.Error, "allowlist") {
		t.Fatalf("outcome = %s (%s), want failed on the runtime error alone", outcome.State, outcome.Error)
	}
	if body, err := os.ReadFile(filepath.Join(f.repo, "src", "app.txt")); err != nil || string(body) != "app" {
		t.Fatalf("the builder's authorized work was rolled back: %q, %v", body, err)
	}
}

// A member's role may also own a phase outside the group. The two run in
// different HOMEs, so they must never share a session key — a runtime would
// try to resume a conversation that lives in the other HOME.
func TestAMemberRoleUsedOutsideTheGroupKeepsSeparateSessions(t *testing.T) {
	snapshot := strings.Replace(panelSnapshot(nil, panelGroup),
		"{name: wrap-up, kind: agent, owner: closer}", "{name: wrap-up, kind: agent, owner: maintainability}", 1)
	f := newRepairFixture(t, snapshot, nil)
	f.fake.Route(buildRole, writes(map[string]string{"src/app.txt": "app"}, "built the app"))
	f.fake.Route(correctnessRole, approve("correct", nil))
	f.fake.Route(securityRole, approve("secure", nil))
	f.fake.Route(maintainabilityRole, approve("maintainable", nil), success("wrapped up", nil))

	outcome := executeWithin(t, f, 20*time.Second)
	if outcome.State != protocol.AttemptAcceptedUnpublished {
		t.Fatalf("outcome = %s (%s), want accepted", outcome.State, outcome.Error)
	}
	calls := callsFor(f.fake, maintainabilityRole)
	if len(calls) != 2 || calls[0].SessionKey == calls[1].SessionKey {
		t.Fatalf("maintainability calls = %d, keys %q — the member and the chain phase must not share a session",
			len(calls), []string{calls[0].SessionKey, calls[len(calls)-1].SessionKey})
	}
}

// A member hitting the send budget stops a sibling that is mid-send; the
// runner waits for it, enforces, and ends the attempt on the send budget —
// never one send past it.
func TestAMemberOnTheSendBudgetStopsItsSiblingAndEndsTheAttempt(t *testing.T) {
	f := newRepairFixture(t, panelSnapshot(nil, twoMemberGroup),
		func(config *engine.Config) { config.MaxAttemptSends = 3 })
	correctnessStarted := make(chan struct{})
	f.fake.Route(buildRole, writes(map[string]string{"src/app.txt": "app"}, "built the app"))
	f.fake.Route(correctnessRole, enginetest.Step{Hang: true, Started: correctnessStarted})
	f.fake.Route(securityRole, enginetest.Step{Text: "not an envelope", WaitFor: correctnessStarted})

	outcome := executeWithin(t, f, 20*time.Second)
	if outcome.State != protocol.AttemptFailed || !strings.Contains(outcome.Error, "send budget") {
		t.Fatalf("outcome = %s (%s), want failed on the send budget", outcome.State, outcome.Error)
	}
	if got := len(f.fake.Calls()); got != 3 {
		t.Fatalf("sends = %d, want exactly the budget (3)", got)
	}
	if got := f.fake.Kills(); got != 1 {
		t.Fatalf("killed sends = %d when Execute returned, want the hanging sibling's 1", got)
	}
	if !f.sink.has(protocol.EventError, "attempt_send_budget_exhausted") {
		t.Fatal("no attempt_send_budget_exhausted event")
	}
	// The stopped sibling still closes its agent_start, with its killed send
	// counted as unmetered rather than free (R1).
	var stopped struct {
		Outcome   string `json:"outcome"`
		Unmetered int    `json:"unmetered_sends"`
	}
	for _, event := range f.sink.all() {
		if event.Type == protocol.EventAgentEnd && event.Phase == "review-correctness" {
			if err := json.Unmarshal(event.Payload, &stopped); err != nil {
				t.Fatal(err)
			}
		}
	}
	if stopped.Outcome != protocol.AgentStopped || stopped.Unmetered != 1 {
		t.Fatalf("stopped sibling's agent_end = %+v, want outcome %q with 1 unmetered send",
			stopped, protocol.AgentStopped)
	}
}

// Precedence: a breach outranks the send budget, which proves the one
// enforcement runs on a terminal exit too.
func TestABreachOutranksTheSendBudgetThatEndedTheGroup(t *testing.T) {
	f := newRepairFixture(t, panelSnapshot(nil, twoMemberGroup),
		func(config *engine.Config) { config.MaxAttemptSends = 3 })
	correctnessStarted := make(chan struct{})
	f.fake.Route(buildRole, writes(map[string]string{"src/app.txt": "app"}, "built the app"))
	f.fake.Route(correctnessRole, enginetest.Step{Hang: true, Started: correctnessStarted,
		Files: map[string]string{"planted.txt": "while hanging"}})
	f.fake.Route(securityRole, enginetest.Step{Text: "not an envelope", WaitFor: correctnessStarted})

	outcome := executeWithin(t, f, 20*time.Second)
	if outcome.State != protocol.AttemptFailed || !strings.Contains(outcome.Error, "planted.txt") {
		t.Fatalf("outcome = %s (%s), want failed on the breach, not the send budget", outcome.State, outcome.Error)
	}
	if f.sink.has(protocol.EventError, "attempt_send_budget_exhausted") {
		t.Fatal("the send budget was reported over a breach")
	}
	if _, err := os.Stat(filepath.Join(f.repo, "planted.txt")); !os.IsNotExist(err) {
		t.Fatalf("the breaching write survived: %v", err)
	}
}

// Every member runs in its own seeded HOME — not the chain's, not a
// sibling's — and every member HOME is wiped when the chain ends. A CI
// repair round runs the group through the same runner in fresh HOMEs.
func TestEachMemberRunsInItsOwnSeededHomeAndEveryHomeIsWiped(t *testing.T) {
	var mutex sync.Mutex
	seeded := map[string][]string{}
	var inFlight, overlapped atomic.Int32
	f := newRepairFixture(t, panelSnapshot(nil, panelGroup), func(config *engine.Config) {
		config.SeedHome = func(home, role string, _ protocol.RoleSpec) error {
			// Seeding is serial, before any member starts: a second seed
			// arriving while this one sleeps is a concurrent seed.
			if inFlight.Add(1) > 1 {
				overlapped.Add(1)
			}
			defer inFlight.Add(-1)
			time.Sleep(30 * time.Millisecond)
			mutex.Lock()
			seeded[role] = append(seeded[role], home)
			mutex.Unlock()
			return os.WriteFile(filepath.Join(home, "auth-"+role), []byte("token"), 0o600)
		}
	})
	scriptPanel(f.fake)
	outcome := executeWithin(t, f, 20*time.Second)
	if outcome.State != protocol.AttemptAcceptedUnpublished || outcome.Continuation == nil {
		t.Fatalf("outcome = %s (%s), want accepted with a continuation", outcome.State, outcome.Error)
	}

	if overlapped.Load() != 0 {
		t.Fatalf("%d HOME seed(s) overlapped another: members must be seeded serially before fan-out",
			overlapped.Load())
	}
	chainHome := filepath.Join(f.scratch, "attempt-1", "home")
	homes := map[string]string{}
	for _, role := range []string{"correctness", "security", "maintainability"} {
		if len(seeded[role]) != 1 {
			t.Fatalf("%s seeded %d time(s), want once", role, len(seeded[role]))
		}
		home := seeded[role][0]
		if home == chainHome {
			t.Fatalf("member role %s was seeded into the chain's HOME", role)
		}
		if other, shared := homes[home]; shared {
			t.Fatalf("members %s and %s share HOME %s", other, role, home)
		}
		homes[home] = role
		for _, call := range callsFor(f.fake, enginetest.ForSystemPrompt(
			map[string]string{"correctness": "Review correctness.", "security": "Review security.",
				"maintainability": "Review maintainability."}[role])) {
			if !contains(call.Options.Env, "HOME="+home) {
				t.Fatalf("%s's send ran outside its seeded HOME %s: %v", role, home, call.Options.Env)
			}
		}
	}
	if seeded["builder"][0] != chainHome {
		t.Fatalf("the builder was seeded into %s, want the chain's HOME", seeded["builder"][0])
	}
	for home := range homes {
		if _, err := os.Stat(home); !os.IsNotExist(err) {
			t.Fatalf("member HOME %s survived the chain: %v", home, err)
		}
	}

	// A round re-runs the group, seeding every member again.
	f.fake.Route(buildRole, writes(map[string]string{"src/fix.txt": "fixed"}, "fixed"))
	f.fake.Route(correctnessRole, approve("correct", nil))
	f.fake.Route(securityRole, approve("secure", nil))
	f.fake.Route(maintainabilityRole, approve("maintainable", nil))
	f.fake.Route(closerRole, success("wrapped up", nil))
	if round := outcome.Continuation.RepairCI(context.Background(), redLint); round.State != protocol.AttemptAcceptedUnpublished {
		t.Fatalf("round = %s (%s), want accepted", round.State, round.Error)
	}
	if got := f.sink.count(protocol.EventLog, "parallel_group_start"); got != 2 {
		t.Fatalf("group runs = %d, want the chain's and the round's", got)
	}
	for _, role := range []string{"correctness", "security", "maintainability"} {
		if len(seeded[role]) != 2 {
			t.Fatalf("%s seeded %d time(s), want again for the round", role, len(seeded[role]))
		}
	}
}

func contains(items []string, want string) bool {
	for _, item := range items {
		if item == want {
			return true
		}
	}
	return false
}

// groupEventPayload decodes the payload of the first group-level (phaseless)
// event of the given type and name.
func groupEventPayload(t *testing.T, sink *recordingSink, eventType, name string) map[string]any {
	t.Helper()
	for _, event := range sink.all() {
		if event.Type == eventType && event.Name == name && event.Phase == "" {
			var payload map[string]any
			if err := json.Unmarshal(event.Payload, &payload); err != nil {
				t.Fatalf("%s payload does not parse: %v", name, err)
			}
			return payload
		}
	}
	t.Fatalf("no group-level %s event", name)
	return nil
}

func payloadStrings(payload map[string]any, key string) []string {
	items, _ := payload[key].([]any)
	values := make([]string, 0, len(items))
	for _, item := range items {
		if value, ok := item.(string); ok {
			values = append(values, value)
		}
	}
	return values
}

// Members are read-only apart from the definition's declared build outputs.
// The group takes one snapshot, so the outputs it reports carry the group,
// never a role.
func TestAMemberMayWriteDeclaredBuildOutputsAndNothingElse(t *testing.T) {
	t.Run("only an output", func(t *testing.T) {
		f := newRepairFixture(t, panelSnapshot(nil, panelGroup)+buildOutputsLine, nil)
		ignoreBin(t, f.repo)
		reviewer := approve("correct", nil)
		reviewer.Files = map[string]string{"bin/jig": "a reviewer ran the build"}
		f.fake.Route(buildRole, writes(map[string]string{"src/app.txt": "app"}, "built the app"))
		f.fake.Route(correctnessRole, reviewer)
		f.fake.Route(securityRole, approve("secure", nil))
		f.fake.Route(maintainabilityRole, approve("maintainable", nil))
		f.fake.Route(closerRole, success("wrapped up", nil))

		outcome := executeWithin(t, f, 20*time.Second)
		if outcome.State != protocol.AttemptAcceptedUnpublished {
			t.Fatalf("outcome = %s (%s), want accepted", outcome.State, outcome.Error)
		}
		payload := groupEventPayload(t, f.sink, protocol.EventLog, "build_outputs_touched")
		if _, hasGroup := payload["parallel_group"]; !hasGroup {
			t.Fatalf("payload = %v, want parallel_group", payload)
		}
		if _, hasRole := payload["role"]; hasRole {
			t.Fatalf("payload = %v, want no role: one snapshot cannot attribute a write", payload)
		}
		if paths := payloadStrings(payload, "paths"); !contains(paths, "bin/") {
			t.Fatalf("paths = %v, want the collapsed bin/", paths)
		}
		if _, err := os.Stat(filepath.Join(f.repo, "bin/jig")); err != nil {
			t.Fatalf("the build output did not survive the group: %v", err)
		}
	})
	t.Run("an output and a stray write", func(t *testing.T) {
		f := newRepairFixture(t, panelSnapshot(nil, panelGroup)+buildOutputsLine, nil)
		ignoreBin(t, f.repo)
		sneaky := approve("correct", nil)
		sneaky.Files = map[string]string{"bin/jig": "a reviewer ran the build", "notes.txt": "scratch"}
		f.fake.Route(buildRole, writes(map[string]string{"src/app.txt": "app"}, "built the app"))
		f.fake.Route(correctnessRole, sneaky)
		f.fake.Route(securityRole, approve("secure", nil))
		f.fake.Route(maintainabilityRole, approve("maintainable", nil))

		outcome := executeWithin(t, f, 20*time.Second)
		if outcome.State != protocol.AttemptFailed || !strings.Contains(outcome.Error, "notes.txt") {
			t.Fatalf("outcome = %s (%s), want failed on a breach naming notes.txt", outcome.State, outcome.Error)
		}
		if strings.Contains(outcome.Error, "bin/") {
			t.Fatalf("error %q names the declared build output as a breach", outcome.Error)
		}
		payload := groupEventPayload(t, f.sink, protocol.EventError, "write_boundary_breach")
		if outputs := payloadStrings(payload, "build_outputs"); !contains(outputs, "bin/") {
			t.Fatalf("breach payload build_outputs = %v, want the collapsed bin/", outputs)
		}
	})
}

// Outputs a dead member wrote survive the group: a member's death rolls
// nothing back, and the group's one enforcement passes declared outputs.
func TestADeadMembersBuildOutputsSurviveTheGroup(t *testing.T) {
	f := newRepairFixture(t, panelSnapshot(nil, panelGroup)+buildOutputsLine, nil)
	ignoreBin(t, f.repo)
	f.fake.Route(buildRole, writes(map[string]string{"src/app.txt": "app"}, "built the app"))
	f.fake.Route(correctnessRole,
		enginetest.Step{Crash: true, Files: map[string]string{"bin/x": "half a build"}},
		approve("correct", nil))
	f.fake.Route(securityRole, approve("secure", nil))
	f.fake.Route(maintainabilityRole, approve("maintainable", nil))
	f.fake.Route(closerRole, success("wrapped up", nil))

	outcome := executeWithin(t, f, 20*time.Second)
	if outcome.State != protocol.AttemptAcceptedUnpublished {
		t.Fatalf("outcome = %s (%s), want accepted", outcome.State, outcome.Error)
	}
	if _, err := os.Stat(filepath.Join(f.repo, "bin/x")); err != nil {
		t.Fatalf("the dead member's build output did not survive: %v", err)
	}
	if f.sink.has(protocol.EventError, "write_boundary_breach") {
		t.Fatal("a dead member's declared build output raised a breach")
	}
}

func TestAPanickingGroupReportsBuildOutputsBesideBreaches(t *testing.T) {
	f := newRepairFixture(t, panelSnapshot(nil, twoMemberGroup)+buildOutputsLine, nil)
	ignoreBin(t, f.repo)
	correctnessStarted := make(chan struct{})
	f.fake.Route(buildRole, writes(map[string]string{"src/app.txt": "app"}, "built the app"))
	f.fake.Route(correctnessRole, enginetest.Step{Hang: true, Started: correctnessStarted,
		Files: map[string]string{"planted.txt": "written before the crash", "bin/jig": "binary"}})
	f.fake.Route(securityRole, enginetest.Step{Do: func(enginetest.Call) {
		<-correctnessStarted
		panic("runtime blew up")
	}})

	recovered := func() (value any) {
		defer func() { value = recover() }()
		f.runner.Execute(context.Background(), f.attempt)
		return nil
	}()
	if recovered != "runtime blew up" {
		t.Fatalf("Execute recovered %v, want the member's panic re-raised", recovered)
	}
	payload := groupEventPayload(t, f.sink, protocol.EventError, "parallel_group_panic")
	if outputs := payloadStrings(payload, "build_outputs"); !contains(outputs, "bin/") {
		t.Fatalf("panic payload build_outputs = %v, want the collapsed bin/", outputs)
	}
	breaches, _ := payload["breaches"].([]any)
	found := false
	for _, item := range breaches {
		if entry, ok := item.(map[string]any); ok && entry["path"] == "planted.txt" {
			found = true
		}
	}
	if !found {
		t.Fatalf("panic payload breaches = %v, want planted.txt", breaches)
	}
}

// A member HOME that cannot be wiped keeps the chain from being handed to
// a round, exactly as the chain's own HOME does.
func TestAnUnwipeableMemberHomeIsNeverHandedToARound(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions, so the wipe cannot be made to fail")
	}
	f := newRepairFixture(t, panelSnapshot(nil, panelGroup), func(config *engine.Config) {
		config.SeedHome = func(home, role string, _ protocol.RoleSpec) error {
			if role == "security" {
				return lockHome(t, home)
			}
			return nil
		}
	})
	scriptPanel(f.fake)
	outcome := executeWithin(t, f, 20*time.Second)
	if outcome.State != protocol.AttemptAcceptedUnpublished || outcome.Continuation != nil {
		t.Fatalf("outcome = %s (%s) continuation=%v, want accepted with no continuation",
			outcome.State, outcome.Error, outcome.Continuation != nil)
	}
}

// Both members' process groups are live at once and both are recorded; each
// is cleared when its send ends.
func TestEveryMembersProcessGroupIsRecordedWhileLive(t *testing.T) {
	var mutex sync.Mutex
	live := map[int64]bool{}
	bothLive := make(chan struct{})
	var once sync.Once
	f := newRepairFixture(t, panelSnapshot(nil, twoMemberGroup), func(config *engine.Config) {
		config.RecordProcess = func(_ string, group int64, active bool) error {
			mutex.Lock()
			defer mutex.Unlock()
			if active {
				live[group] = true
			} else {
				delete(live, group)
			}
			if live[101] && live[102] {
				once.Do(func() { close(bothLive) })
			}
			return nil
		}
	})
	correctness := approve("correct", nil)
	correctness.ProcessGroup, correctness.WaitFor = 101, bothLive
	security := approve("secure", nil)
	security.ProcessGroup, security.WaitFor = 102, bothLive
	f.fake.Route(buildRole, writes(map[string]string{"src/app.txt": "app"}, "built the app"))
	f.fake.Route(correctnessRole, correctness)
	f.fake.Route(securityRole, security)
	f.fake.Route(maintainabilityRole, approve("maintainable", nil))
	f.fake.Route(closerRole, success("wrapped up", nil))

	outcome := executeWithin(t, f, 20*time.Second)
	if outcome.State != protocol.AttemptAcceptedUnpublished {
		t.Fatalf("outcome = %s (%s), want accepted", outcome.State, outcome.Error)
	}
	mutex.Lock()
	defer mutex.Unlock()
	if len(live) != 0 {
		t.Fatalf("process groups still recorded live after the chain: %v", live)
	}
}
