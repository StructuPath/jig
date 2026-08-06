// admission_test.go — unattended admission end to end (U6, R13, KTD7):
// overlap-skip, the single overdue instant, GitHub dedup and crash recovery,
// the actionable `gh` diagnostics, and the Origin fence on the trigger
// surface.
//
// No test here reaches GitHub. The `gh` seam is an interface, and the
// diagnostic tests drive the REAL gateway with an injected command runner, so
// the argument construction, the strict JSON decoding, and the failure
// classification are all exercised without a network or a gh binary.
package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/StructuPath/jig/internal/protocol"
)

const triggerRepository = "github.com/example/triggered"

// pinnedSHA is what the injected resolver pins for repositories this machine
// cannot reach.
const pinnedSHA = "3333333333333333333333333333333333333333"

// newTriggerStore is newTestStore with the base-SHA resolver stubbed, so a
// GitHub trigger can admit runs for github.com/example/... without the test
// touching the network. Schedule tests that use real local fixture
// repositories do not need it and use newTestStore directly.
func newTriggerStore(t *testing.T) (*Store, *testClock) {
	t.Helper()
	store, clock := newTestStore(t)
	store.resolveRef = func(context.Context, string, string) (string, error) { return pinnedSHA, nil }
	return store, clock
}

// discardLogger keeps admission's operational logging out of test output.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// ---- fakes ------------------------------------------------------------------

// fakeGitHubGateway is the seam's test double: it answers with whatever the
// test says GitHub currently holds, and counts calls so "two consecutive
// polls" is provably two polls.
type fakeGitHubGateway struct {
	issues       []GitHubIssueMatch
	pullRequests []GitHubPullRequestMatch
	err          error
	calls        int
}

func (g *fakeGitHubGateway) ListIssues(context.Context, TriggerConfig) ([]GitHubIssueMatch, error) {
	g.calls++
	if g.err != nil {
		return nil, g.err
	}
	return g.issues, nil
}

func (g *fakeGitHubGateway) ListPullRequests(context.Context, TriggerConfig) ([]GitHubPullRequestMatch, error) {
	g.calls++
	if g.err != nil {
		return nil, g.err
	}
	return g.pullRequests, nil
}

func issueMatch(number int, title string, labels ...string) GitHubIssueMatch {
	if labels == nil {
		labels = []string{}
	}
	return GitHubIssueMatch{
		Number: number, Title: title, State: "open",
		URL:  fmt.Sprintf("https://github.com/example/triggered/issues/%d", number),
		Body: "please do the thing", Labels: labels,
	}
}

// ---- helpers ----------------------------------------------------------------

func mustCreateTrigger(t *testing.T, store *Store, input TriggerInput) Trigger {
	t.Helper()
	trigger, err := store.CreateTrigger(context.Background(), input)
	if err != nil {
		t.Fatalf("create trigger: %v", err)
	}
	if trigger.Enabled {
		t.Fatal("a trigger must be created disabled")
	}
	return trigger
}

func mustEnableTrigger(t *testing.T, store *Store, triggerID string) Trigger {
	t.Helper()
	trigger, err := store.SetTriggerEnabled(context.Background(), triggerID, true)
	if err != nil {
		t.Fatalf("enable trigger: %v", err)
	}
	return trigger
}

func triggerOccurrences(t *testing.T, store *Store, triggerID string) []Occurrence {
	t.Helper()
	occurrences, err := store.TriggerOccurrences(context.Background(), triggerID)
	if err != nil {
		t.Fatalf("read occurrences: %v", err)
	}
	return occurrences
}

func mustReadTrigger(t *testing.T, store *Store, triggerID string) Trigger {
	t.Helper()
	trigger, err := store.Trigger(context.Background(), triggerID)
	if err != nil {
		t.Fatalf("read trigger: %v", err)
	}
	return trigger
}

func runCount(t *testing.T, store *Store) int {
	t.Helper()
	var count int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM runs`).Scan(&count); err != nil {
		t.Fatalf("count runs: %v", err)
	}
	return count
}

func tick(t *testing.T, store *Store, gateway GitHubGateway) {
	t.Helper()
	if err := NewAdmissionRunner(store, gateway, discardLogger()).Tick(context.Background()); err != nil {
		t.Fatalf("admission tick: %v", err)
	}
}

func decodeSource(t *testing.T, occurrence Occurrence) occurrenceSource {
	t.Helper()
	var source occurrenceSource
	if err := json.Unmarshal(occurrence.Source, &source); err != nil {
		t.Fatalf("decode occurrence source: %v", err)
	}
	return source
}

// ---- schedules (R13) ---------------------------------------------------------

// R13, literally: a schedule firing while its prior run is still active
// creates no run and increments the skip counter. The firing is still
// recorded — the history has to be honest about what the clock did — but it
// is recorded as `skipped`.
func TestAScheduleFiringWhileItsPriorRunIsActiveCreatesNoRunAndIncrementsTheSkipCounter(t *testing.T) {
	store, clock := newTestStore(t)
	definition := mustCreateDefinition(t, store, u5Definition)
	repositoryPath, _, _ := newFixtureRepository(t, "scheduled")
	trigger := mustCreateTrigger(t, store, TriggerInput{
		Name: "hourly", DefinitionID: definition.ID, Kind: TriggerSchedule,
		Config: TriggerConfig{
			Cron: "0 * * * *", Timezone: "UTC", Instructions: "sweep the repository",
			Targets: []InvocationTarget{{Repository: repositoryPath}},
		},
	})
	mustEnableTrigger(t, store, trigger.ID)

	// The first firing admits a run, which stays `active` because its job is
	// queued and nothing has claimed it.
	clock.Advance(time.Hour)
	tick(t, store, nil)
	if runs := runCount(t, store); runs != 1 {
		t.Fatalf("the first firing produced %d runs, want 1", runs)
	}
	occurrences := triggerOccurrences(t, store, trigger.ID)
	if len(occurrences) != 1 || occurrences[0].State != OccurrenceDispatched || occurrences[0].RunID == "" {
		t.Fatalf("first firing = %+v, want one dispatched occurrence carrying a run", occurrences)
	}
	priorRun, err := store.Run(context.Background(), occurrences[0].RunID)
	if err != nil {
		t.Fatalf("read prior run: %v", err)
	}
	if priorRun.State != protocol.RunActive {
		t.Fatalf("prior run state = %q, want active — the overlap scenario needs a live run", priorRun.State)
	}

	// The second firing lands while that run is still active.
	clock.Advance(time.Hour)
	tick(t, store, nil)
	if runs := runCount(t, store); runs != 1 {
		t.Fatalf("the overlapping firing produced %d runs, want the original 1", runs)
	}
	occurrences = triggerOccurrences(t, store, trigger.ID)
	if len(occurrences) != 2 {
		t.Fatalf("trigger holds %d occurrences, want 2", len(occurrences))
	}
	skipped := occurrences[0]
	if skipped.State != OccurrenceSkipped || skipped.RunID != "" {
		t.Fatalf("overlapping firing = %+v, want a skipped occurrence with no run", skipped)
	}
	if !strings.Contains(skipped.Diagnostic, "still active") {
		t.Fatalf("skip diagnostic = %q, want it to name the still-active run", skipped.Diagnostic)
	}
	if counted := mustReadTrigger(t, store, trigger.ID); counted.SkippedCount != 1 {
		t.Fatalf("skipped_count = %d, want 1", counted.SkippedCount)
	}
}

// R13's other half: on wake after downtime, the ONE stored overdue instant is
// admitted and the cursor jumps to the next FUTURE match. The seven hourly
// instants the sleep passed over are never admitted and never stored, because
// a schedule's whole memory of the future is a single row value.
func TestASimulatedEightHourClockJumpAdmitsExactlyOneOverdueInstantThenTheNextFutureMatch(t *testing.T) {
	store, clock := newTestStore(t)
	registerTestWorker(t, store, 4)
	definition := mustCreateDefinition(t, store, u5Definition)
	repositoryPath, _, _ := newFixtureRepository(t, "hourly")
	trigger := mustCreateTrigger(t, store, TriggerInput{
		Name: "hourly", DefinitionID: definition.ID, Kind: TriggerSchedule,
		Config: TriggerConfig{
			Cron: "0 * * * *", Timezone: "UTC", Instructions: "sweep the repository",
			Targets: []InvocationTarget{{Repository: repositoryPath}},
		},
	})
	// The clock starts at 08:00 UTC, so the stored instant is 09:00.
	enabled := mustEnableTrigger(t, store, trigger.ID)
	wantStored := time.Date(2026, 8, 5, 9, 0, 0, 0, time.UTC)
	if enabled.NextDueAt == nil || !enabled.NextDueAt.Equal(wantStored) {
		t.Fatalf("stored instant at enable = %v, want %v", enabled.NextDueAt, wantStored)
	}

	// Server and machine sleep for eight hours: 09:00 through 16:00 all pass
	// unobserved, and the server wakes at 17:00.
	clock.Advance(9 * time.Hour)
	tick(t, store, nil)

	occurrences := triggerOccurrences(t, store, trigger.ID)
	if len(occurrences) != 1 {
		t.Fatalf("wake admitted %d occurrences, want exactly the one stored overdue instant", len(occurrences))
	}
	if occurrences[0].ScheduledAt == nil || !occurrences[0].ScheduledAt.Equal(wantStored) {
		t.Fatalf("admitted instant = %v, want the stored %v", occurrences[0].ScheduledAt, wantStored)
	}
	if !strings.Contains(occurrences[0].Diagnostic, "not caught up") {
		t.Fatalf("catch-up diagnostic = %q, want it to say the missed instants were not caught up",
			occurrences[0].Diagnostic)
	}
	after := mustReadTrigger(t, store, trigger.ID)
	wantNext := time.Date(2026, 8, 5, 18, 0, 0, 0, time.UTC)
	if after.NextDueAt == nil || !after.NextDueAt.Equal(wantNext) {
		t.Fatalf("cursor after wake = %v, want the next FUTURE match %v", after.NextDueAt, wantNext)
	}

	// Finish the admitted run so the next firing is not merely overlap-skipped,
	// then let the next future match arrive: it admits normally. (The worker
	// re-registers because the simulated sleep took it past its liveness
	// window — R4 — exactly as a real worker would on wake.)
	registerTestWorker(t, store, 4)
	finishJobs(t, store, protocol.AttemptFailed)
	clock.Advance(time.Hour)
	tick(t, store, nil)

	occurrences = triggerOccurrences(t, store, trigger.ID)
	if len(occurrences) != 2 {
		t.Fatalf("trigger holds %d occurrences, want 2 (one overdue, one future)", len(occurrences))
	}
	instants := []time.Time{*occurrences[1].ScheduledAt, *occurrences[0].ScheduledAt}
	for index, want := range []time.Time{wantStored, wantNext} {
		if !instants[index].Equal(want) {
			t.Fatalf("occurrence %d is for %v, want %v — no missed instant may be caught up",
				index+1, instants[index], want)
		}
	}
	if occurrences[0].State != OccurrenceDispatched {
		t.Fatalf("the next future match = %q, want dispatched", occurrences[0].State)
	}
}

// ---- GitHub polling (KTD7) ---------------------------------------------------

func githubIssueTrigger(t *testing.T, store *Store, definitionID string) Trigger {
	t.Helper()
	trigger := mustCreateTrigger(t, store, TriggerInput{
		Name: "issues", DefinitionID: definitionID, Kind: TriggerGitHubIssue,
		Config: TriggerConfig{
			Repository: triggerRepository, State: "open", Instructions: "resolve the issue",
		},
	})
	return mustEnableTrigger(t, store, trigger.ID)
}

// The dedup key is derived from event content, so an issue that is still open
// on the next poll is the same event, not a new one: one occurrence, one run,
// however many times it is observed.
func TestTheSameGitHubIssueObservedByTwoConsecutivePollsCreatesOneOccurrenceAndOneRun(t *testing.T) {
	store, clock := newTriggerStore(t)
	definition := mustCreateDefinition(t, store, u5Definition)
	trigger := githubIssueTrigger(t, store, definition.ID)
	gateway := &fakeGitHubGateway{issues: []GitHubIssueMatch{issueMatch(1481, "the build is red")}}

	tick(t, store, gateway)
	clock.Advance(protocol.DefaultTriggerPollInterval + time.Second)
	tick(t, store, gateway)

	if gateway.calls != 2 {
		t.Fatalf("the gateway was polled %d times, want 2 — the scenario is two consecutive polls", gateway.calls)
	}
	occurrences := triggerOccurrences(t, store, trigger.ID)
	if len(occurrences) != 1 {
		t.Fatalf("two polls of one issue produced %d occurrences, want 1", len(occurrences))
	}
	if occurrences[0].State != OccurrenceDispatched || occurrences[0].RunID == "" {
		t.Fatalf("occurrence = %+v, want dispatched with a run", occurrences[0])
	}
	if runs := runCount(t, store); runs != 1 {
		t.Fatalf("two polls of one issue produced %d runs, want 1", runs)
	}

	// The issue text is untrusted context, never instructions and never a raw
	// prompt parameter (KTD11): the frozen prompt must fence it.
	run, err := store.Run(context.Background(), occurrences[0].RunID)
	if err != nil {
		t.Fatalf("read run: %v", err)
	}
	prompt := run.Parameters[protocol.PromptParameter]
	if !strings.Contains(prompt, "resolve the issue") {
		t.Fatalf("frozen prompt lost the trusted instructions: %q", prompt)
	}
	if !strings.Contains(prompt, "JIG-UNTRUSTED-BEGIN issue-1481") ||
		!strings.Contains(prompt, "please do the thing") {
		t.Fatalf("issue text is not fenced as untrusted context: %q", prompt)
	}
	if index := strings.Index(prompt, "JIG-UNTRUSTED-BEGIN"); index >= 0 &&
		strings.Index(prompt, "resolve the issue") > index {
		t.Fatal("the trusted instructions must precede the untrusted block")
	}
}

// Commit-occurrence-then-dispatch, at the crash it exists for: the occurrence
// is durable, the run is not yet, and recovery produces exactly one run —
// never zero (the work would be silently dropped) and never two.
func TestACrashBetweenOccurrenceCommitAndDispatchRecoversToExactlyOneRun(t *testing.T) {
	store, _ := newTriggerStore(t)
	definition := mustCreateDefinition(t, store, u5Definition)
	trigger := githubIssueTrigger(t, store, definition.ID)
	gateway := &fakeGitHubGateway{issues: []GitHubIssueMatch{issueMatch(1481, "the build is red")}}

	// The poll commits the occurrence. The dispatcher reserves it and the
	// process dies there — exactly the window the two-phase protocol exists
	// for.
	if err := store.PollGitHubTrigger(context.Background(), trigger.ID, gateway); err != nil {
		t.Fatalf("poll: %v", err)
	}
	occurrences := triggerOccurrences(t, store, trigger.ID)
	if len(occurrences) != 1 || occurrences[0].State != OccurrencePending {
		t.Fatalf("after the poll: %+v, want one pending occurrence", occurrences)
	}
	if runs := runCount(t, store); runs != 0 {
		t.Fatalf("the poll created %d runs; polling must only commit occurrences", runs)
	}
	if _, err := store.db.Exec(
		`UPDATE occurrences SET state = 'dispatching' WHERE id = ?`, occurrences[0].ID); err != nil {
		t.Fatalf("simulate the crash: %v", err)
	}

	// Restart: recovery re-drives what was interrupted.
	runner := NewAdmissionRunner(store, gateway, discardLogger())
	if err := runner.Recover(context.Background()); err != nil {
		t.Fatalf("recover: %v", err)
	}
	if runs := runCount(t, store); runs != 1 {
		t.Fatalf("recovery produced %d runs, want exactly 1", runs)
	}

	// And the poll that follows the restart still sees the same open issue.
	if err := runner.Tick(context.Background()); err != nil {
		t.Fatalf("tick after recovery: %v", err)
	}
	if runs := runCount(t, store); runs != 1 {
		t.Fatalf("the post-recovery poll produced %d runs, want the original 1", runs)
	}
	occurrences = triggerOccurrences(t, store, trigger.ID)
	if len(occurrences) != 1 || occurrences[0].State != OccurrenceDispatched {
		t.Fatalf("after recovery: %+v, want one dispatched occurrence", occurrences)
	}
}

// A pull request is content-addressed by its head SHA, so a new push is a new
// event — and the head SHA is what the run pins, so the run examines the
// commit the trigger matched rather than whatever the default branch holds.
func TestANewPushToAPullRequestIsANewEventAndPinsItsHeadCommit(t *testing.T) {
	store, clock := newTriggerStore(t)
	definition := mustCreateDefinition(t, store, u5Definition)
	trigger := mustCreateTrigger(t, store, TriggerInput{
		Name: "pulls", DefinitionID: definition.ID, Kind: TriggerGitHubPullRequest,
		Config: TriggerConfig{
			Repository: triggerRepository, State: "open", Instructions: "review the pull request",
		},
	})
	mustEnableTrigger(t, store, trigger.ID)
	pullRequest := GitHubPullRequestMatch{
		Number: 42, Title: "add the widget", State: "open",
		URL:  "https://github.com/example/triggered/pull/42",
		Body: "please review", Labels: []string{},
		BaseBranch: "main", HeadCommit: shaA,
	}
	gateway := &fakeGitHubGateway{pullRequests: []GitHubPullRequestMatch{pullRequest}}

	tick(t, store, gateway)
	clock.Advance(protocol.DefaultTriggerPollInterval + time.Second)
	tick(t, store, gateway)
	if occurrences := triggerOccurrences(t, store, trigger.ID); len(occurrences) != 1 {
		t.Fatalf("the same head observed twice produced %d occurrences, want 1", len(occurrences))
	}

	// A new push: same pull request, new head, new event.
	pullRequest.HeadCommit = shaB
	gateway.pullRequests = []GitHubPullRequestMatch{pullRequest}
	clock.Advance(protocol.DefaultTriggerPollInterval + time.Second)
	tick(t, store, gateway)

	occurrences := triggerOccurrences(t, store, trigger.ID)
	if len(occurrences) != 2 {
		t.Fatalf("a new head produced %d occurrences, want 2", len(occurrences))
	}
	newest := occurrences[0]
	if decodeSource(t, newest).HeadSHA != shaB {
		t.Fatalf("newest occurrence head = %q, want %q", decodeSource(t, newest).HeadSHA, shaB)
	}
	run, err := store.Run(context.Background(), newest.RunID)
	if err != nil {
		t.Fatalf("read run: %v", err)
	}
	if len(run.Targets) != 1 || run.Targets[0].BaseSHA != shaB {
		t.Fatalf("run pinned %+v, want the pull request's head %s", run.Targets, shaB)
	}
}

// ---- gh diagnostics (KTD7) ---------------------------------------------------

// Each failure mode produces its own actionable diagnostic, and NONE of them
// creates an occurrence: a failed check admits nothing. These drive the real
// gateway through an injected command runner, so the argument vector, the
// strict decoding, and the classification are all under test.
func TestGhFailuresProduceDistinctActionableDiagnosticsAndCreateNoOccurrences(t *testing.T) {
	overLimit, err := json.Marshal(func() []map[string]any {
		values := make([]map[string]any, 0, protocol.MaxTriggerMatches+1)
		for number := 1; number <= protocol.MaxTriggerMatches+1; number++ {
			values = append(values, map[string]any{
				"number": number, "title": "issue", "state": "open", "body": "",
				"url":    fmt.Sprintf("https://github.com/example/triggered/issues/%d", number),
				"labels": []any{},
			})
		}
		return values
	}())
	if err != nil {
		t.Fatalf("build the over-limit response: %v", err)
	}

	for _, testCase := range []struct {
		name     string
		run      func(context.Context, string, ...string) ([]byte, []byte, bool, bool, error)
		wantCode string
	}{
		{
			name: "timeout",
			run: func(context.Context, string, ...string) ([]byte, []byte, bool, bool, error) {
				return nil, nil, false, false, context.DeadlineExceeded
			},
			wantCode: "gh_timed_out",
		},
		{
			name: "unauthenticated",
			run: func(context.Context, string, ...string) ([]byte, []byte, bool, bool, error) {
				return nil, []byte("error: not logged into any GitHub hosts. Run gh auth login"),
					false, false, &exec.ExitError{}
			},
			wantCode: "gh_unauthenticated",
		},
		{
			name: "over the match limit",
			run: func(context.Context, string, ...string) ([]byte, []byte, bool, bool, error) {
				return overLimit, nil, false, false, nil
			},
			wantCode: "gh_match_limit",
		},
		{
			name: "stdout past its bound",
			run: func(context.Context, string, ...string) ([]byte, []byte, bool, bool, error) {
				return []byte("[{"), nil, true, false, nil
			},
			wantCode: "gh_output_too_large",
		},
		{
			name: "unreadable json",
			run: func(context.Context, string, ...string) ([]byte, []byte, bool, bool, error) {
				return []byte(`[{"number":1,"surprise":true}]`), nil, false, false, nil
			},
			wantCode: "gh_malformed_output",
		},
		{
			name: "a result naming another repository",
			run: func(context.Context, string, ...string) ([]byte, []byte, bool, bool, error) {
				return []byte(`[{"number":7,"title":"elsewhere","state":"open","body":"",` +
						`"url":"https://github.com/attacker/elsewhere/issues/7","labels":[]}]`),
					nil, false, false, nil
			},
			wantCode: "gh_invalid_output",
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			store, _ := newTriggerStore(t)
			definition := mustCreateDefinition(t, store, u5Definition)
			trigger := githubIssueTrigger(t, store, definition.ID)
			gateway := &GitHubCLIGateway{
				LookPath: func(string) (string, error) { return "/usr/bin/gh", nil },
				Run:      testCase.run,
			}

			err := store.PollGitHubTrigger(context.Background(), trigger.ID, gateway)
			if err == nil {
				t.Fatal("a failed check must surface as an error")
			}
			if code := serviceCode(t, err); code != testCase.wantCode {
				t.Fatalf("diagnostic code = %q, want %q", code, testCase.wantCode)
			}
			var service *ServiceError
			if errors.As(err, &service) && strings.TrimSpace(service.Message) == "" {
				t.Fatal("a diagnostic must carry an actionable message, not just a code")
			}
			if occurrences := triggerOccurrences(t, store, trigger.ID); len(occurrences) != 0 {
				t.Fatalf("a failed check created %d occurrences, want none: %+v",
					len(occurrences), occurrences)
			}
			if runs := runCount(t, store); runs != 0 {
				t.Fatalf("a failed check created %d runs, want none", runs)
			}
			// The diagnostic is stored on the trigger, and the trigger keeps
			// polling: a `gh` failure is a fact about the network, not a
			// reason to disable an operator's trigger.
			stored := mustReadTrigger(t, store, trigger.ID)
			if stored.DiagnosticCode != testCase.wantCode || stored.Diagnostic == "" {
				t.Fatalf("stored diagnostic = %q/%q, want %q with a message",
					stored.DiagnosticCode, stored.Diagnostic, testCase.wantCode)
			}
			if !stored.Enabled || stored.NextPollAt == nil {
				t.Fatalf("a failed check left the trigger enabled=%v next_poll=%v, want it still polling",
					stored.Enabled, stored.NextPollAt)
			}
		})
	}
}

// The `gh` argument vector is fixed: every value is a separate argument, so
// nothing observed on GitHub is ever parsed as syntax.
func TestTheGhArgumentVectorIsFixedAndCarriesTheConfiguredNarrowing(t *testing.T) {
	var captured []string
	gateway := &GitHubCLIGateway{
		LookPath: func(string) (string, error) { return "/usr/bin/gh", nil },
		Run: func(_ context.Context, _ string, arguments ...string) ([]byte, []byte, bool, bool, error) {
			captured = arguments
			return []byte("[]"), nil, false, false, nil
		},
	}
	if _, err := gateway.ListIssues(context.Background(), TriggerConfig{
		Repository: triggerRepository, State: "open",
		RequiredLabels: []string{"needs triage", "jig"},
	}); err != nil {
		t.Fatalf("list issues: %v", err)
	}
	want := []string{
		"issue", "list", "--repo", "example/triggered", "--state", "open",
		"--limit", fmt.Sprint(protocol.MaxTriggerMatches + 1),
		"--json", "number,title,url,state,body,labels",
		"--label", "needs triage", "--label", "jig",
	}
	if len(captured) != len(want) {
		t.Fatalf("argument vector = %q, want %q", captured, want)
	}
	for index := range want {
		if captured[index] != want[index] {
			t.Fatalf("argument %d = %q, want %q (full vector %q)", index, captured[index], want[index], captured)
		}
	}
}

// ---- authoring and the HTTP surface -----------------------------------------

func TestTriggerValidationRejectsWhatCannotFireNamingTheFault(t *testing.T) {
	store, _ := newTestStore(t)
	definition := mustCreateDefinition(t, store, u5Definition)
	repositoryPath, _, _ := newFixtureRepository(t, "validated")
	for _, testCase := range []struct {
		name     string
		input    TriggerInput
		wantCode string
	}{
		{"unknown definition", TriggerInput{
			Name: "t", DefinitionID: "nope", Kind: TriggerSchedule,
			Config: TriggerConfig{Cron: "0 * * * *", Timezone: "UTC", Instructions: "go",
				Targets: []InvocationTarget{{Repository: repositoryPath}}},
		}, "unknown_definition"},
		{"unknown kind", TriggerInput{
			Name: "t", DefinitionID: definition.ID, Kind: "webhook",
			Config: TriggerConfig{Instructions: "go"},
		}, "invalid_trigger_kind"},
		{"no instructions", TriggerInput{
			Name: "t", DefinitionID: definition.ID, Kind: TriggerSchedule,
			Config: TriggerConfig{Cron: "0 * * * *", Timezone: "UTC",
				Targets: []InvocationTarget{{Repository: repositoryPath}}},
		}, "invalid_instructions"},
		{"a prompt parameter alongside instructions", TriggerInput{
			Name: "t", DefinitionID: definition.ID, Kind: TriggerSchedule,
			Config: TriggerConfig{Cron: "0 * * * *", Timezone: "UTC", Instructions: "go",
				Parameters: map[string]string{protocol.PromptParameter: "smuggled"},
				Targets:    []InvocationTarget{{Repository: repositoryPath}}},
		}, "ambiguous_prompt"},
		{"unparseable cron", TriggerInput{
			Name: "t", DefinitionID: definition.ID, Kind: TriggerSchedule,
			Config: TriggerConfig{Cron: "@hourly", Timezone: "UTC", Instructions: "go",
				Targets: []InvocationTarget{{Repository: repositoryPath}}},
		}, "invalid_cron"},
		{"schedule without targets", TriggerInput{
			Name: "t", DefinitionID: definition.ID, Kind: TriggerSchedule,
			Config: TriggerConfig{Cron: "0 * * * *", Timezone: "UTC", Instructions: "go"},
		}, "no_targets"},
		{"a non-github repository on a github trigger", TriggerInput{
			Name: "t", DefinitionID: definition.ID, Kind: TriggerGitHubIssue,
			Config: TriggerConfig{Repository: repositoryPath, Instructions: "go"},
		}, "invalid_target"},
		{"a poll interval under the floor", TriggerInput{
			Name: "t", DefinitionID: definition.ID, Kind: TriggerGitHubIssue,
			Config: TriggerConfig{Repository: triggerRepository, Instructions: "go",
				PollIntervalSeconds: 1},
		}, "invalid_poll_interval"},
		{"a state the kind does not have", TriggerInput{
			Name: "t", DefinitionID: definition.ID, Kind: TriggerGitHubIssue,
			Config: TriggerConfig{Repository: triggerRepository, Instructions: "go", State: "merged"},
		}, "invalid_trigger_state"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			_, err := store.CreateTrigger(context.Background(), testCase.input)
			if err == nil {
				t.Fatal("invalid trigger was accepted")
			}
			if code := serviceCode(t, err); code != testCase.wantCode {
				t.Fatalf("error code = %q, want %q (%v)", code, testCase.wantCode, err)
			}
		})
	}
}

// A disabled trigger has no future: it never accumulates an overdue instant
// while it is off, so enabling starts from now rather than from whenever it
// was last on (R13).
func TestADisabledTriggerHasNoStoredInstantAndReEnablingStartsFromNow(t *testing.T) {
	store, clock := newTestStore(t)
	definition := mustCreateDefinition(t, store, u5Definition)
	repositoryPath, _, _ := newFixtureRepository(t, "toggled")
	trigger := mustCreateTrigger(t, store, TriggerInput{
		Name: "hourly", DefinitionID: definition.ID, Kind: TriggerSchedule,
		Config: TriggerConfig{Cron: "0 * * * *", Timezone: "UTC", Instructions: "go",
			Targets: []InvocationTarget{{Repository: repositoryPath}}},
	})
	if trigger.NextDueAt != nil {
		t.Fatalf("a trigger created disabled has a stored instant %v", trigger.NextDueAt)
	}
	mustEnableTrigger(t, store, trigger.ID)
	disabled, err := store.SetTriggerEnabled(context.Background(), trigger.ID, false)
	if err != nil {
		t.Fatalf("disable: %v", err)
	}
	if disabled.NextDueAt != nil {
		t.Fatalf("a disabled trigger kept the stored instant %v", disabled.NextDueAt)
	}
	clock.Advance(5 * time.Hour)
	tick(t, store, nil)
	if occurrences := triggerOccurrences(t, store, trigger.ID); len(occurrences) != 0 {
		t.Fatalf("a disabled trigger admitted %d occurrences", len(occurrences))
	}
	reEnabled := mustEnableTrigger(t, store, trigger.ID)
	want := time.Date(2026, 8, 5, 14, 0, 0, 0, time.UTC)
	if reEnabled.NextDueAt == nil || !reEnabled.NextDueAt.Equal(want) {
		t.Fatalf("re-enabled instant = %v, want the next match after now, %v", reEnabled.NextDueAt, want)
	}
}

// The trigger surface takes the same Origin fence every other mutation takes
// (R20), and creation is disabled whatever the caller sends.
func TestTriggerRoutesAreOriginFencedAndCreateTriggersDisabled(t *testing.T) {
	store, _ := newTestStore(t)
	definition := mustCreateDefinition(t, store, u5Definition)
	repositoryPath, _, _ := newFixtureRepository(t, "fenced")
	handler := NewHandler(store, "", discardLogger())
	server := httptest.NewServer(handler)
	defer server.Close()

	body := fmt.Sprintf(`{"name":"hourly","definition_id":%q,"kind":"schedule","config":{`+
		`"cron":"0 * * * *","timezone":"UTC","instructions":"go","targets":[{"repository":%q}]}}`,
		definition.ID, repositoryPath)

	post := func(t *testing.T, path, payload, origin string) *http.Response {
		t.Helper()
		request, err := http.NewRequest("POST", server.URL+path, strings.NewReader(payload))
		if err != nil {
			t.Fatalf("build request: %v", err)
		}
		request.Header.Set("Content-Type", "application/json")
		if origin != "" {
			request.Header.Set("Origin", origin)
		}
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatalf("request: %v", err)
		}
		return response
	}

	response := post(t, "/api/triggers", body, "http://evil.test")
	defer response.Body.Close()
	if response.StatusCode != http.StatusForbidden {
		t.Fatalf("foreign-origin trigger creation = %d, want 403", response.StatusCode)
	}
	if triggers, err := store.Triggers(context.Background()); err != nil || len(triggers) != 0 {
		t.Fatalf("a fenced-out request created triggers: %v, %v", triggers, err)
	}

	created := post(t, "/api/triggers", body, server.URL)
	defer created.Body.Close()
	if created.StatusCode != http.StatusCreated {
		t.Fatalf("same-origin trigger creation = %d, want 201", created.StatusCode)
	}
	var trigger Trigger
	if err := json.NewDecoder(created.Body).Decode(&trigger); err != nil {
		t.Fatalf("decode trigger: %v", err)
	}
	if trigger.Enabled {
		t.Fatal("a trigger created over HTTP must start disabled")
	}

	enabled := post(t, "/api/triggers/"+trigger.ID+"/enable", "", server.URL)
	defer enabled.Body.Close()
	if enabled.StatusCode != http.StatusOK {
		t.Fatalf("enable = %d, want 200", enabled.StatusCode)
	}

	// Occurrence listing is a read: no fence, and it answers with a JSON array.
	listed, err := http.Get(server.URL + "/api/triggers/" + trigger.ID + "/occurrences")
	if err != nil {
		t.Fatalf("list occurrences: %v", err)
	}
	defer listed.Body.Close()
	if listed.StatusCode != http.StatusOK {
		t.Fatalf("occurrence listing = %d, want 200", listed.StatusCode)
	}
	var occurrences []Occurrence
	if err := json.NewDecoder(listed.Body).Decode(&occurrences); err != nil {
		t.Fatalf("decode occurrences: %v", err)
	}
	if len(occurrences) != 0 {
		t.Fatalf("a freshly enabled trigger already has %d occurrences", len(occurrences))
	}
}
