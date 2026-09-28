// publish_ci_test.go — the opt-in CI wait: a definition with publish.ci.wait
// reaches `accepted` only once the branch's head is green, ends
// `accepted_unpublished` with the red checks named when it is not, and a
// publish-only retry judges whatever the branch's head is by then. The remote
// is a real bare repository; CI itself is the scripted fake gateway.
package worker

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/StructuPath/jig/internal/protocol"
)

// ciSnapshot is integrationSnapshot opted into the CI wait.
const ciSnapshot = integrationSnapshot + `publish:
  ci:
    wait: true
`

// fastCIOptions shrinks the poll interval and registration grace so a CI
// scenario takes milliseconds.
func fastCIOptions() PublishOptions {
	return PublishOptions{
		CommitAuthorName: "jig-test", CommitAuthorEmail: "jig@test",
		CIPollInterval: 5 * time.Millisecond, CIRegistrationGrace: 20 * time.Millisecond,
	}
}

func newCIWorker(
	t *testing.T, h *harness, inner AttemptRunner, gateway PullRequestGateway,
) *Worker {
	t.Helper()
	runner := NewPublishingRunner(inner, gateway, fastCIOptions())
	w := newTestWorker(t, h, filepath.Join(t.TempDir(), "worker"), 1, runner)
	runner.Bind(w)
	return w
}

func check(name, verdict string) CICheck {
	conclusion := ""
	if verdict == CIFail {
		conclusion = "failure"
	}
	return CICheck{Name: name, Verdict: verdict, Conclusion: conclusion}
}

func recordedSteps(t *testing.T, w *Worker, attemptID string) []string {
	t.Helper()
	records, err := w.client.AttemptPublishRecords(context.Background(), attemptID)
	if err != nil {
		t.Fatalf("read publish records: %v", err)
	}
	steps := make([]string, 0, len(records))
	for _, record := range records {
		steps = append(steps, record.Step)
	}
	slices.Sort(steps)
	return steps
}

// pushToBranch commits a file onto the attempt branch on the remote the way
// a person fixing CI would, and returns the new head.
func pushToBranch(t *testing.T, originDir, branch string) string {
	t.Helper()
	clone := filepath.Join(t.TempDir(), "human")
	gitRun(t, "", "clone", "--branch", branch, originDir, clone)
	if err := os.WriteFile(filepath.Join(clone, "fix.txt"), []byte("the fix\n"), 0o644); err != nil {
		t.Fatalf("write fix: %v", err)
	}
	gitRun(t, clone, "add", "fix.txt")
	gitRun(t, clone, "-c", "user.name=human", "-c", "user.email=human@test", "commit", "-m", "fix CI")
	gitRun(t, clone, "push", "origin", branch)
	return gitRun(t, clone, "rev-parse", "HEAD")
}

// ---- scenario: green ---------------------------------------------------------

// CI that is pending and then green lets the attempt reach `accepted`, with
// the ci step on the ledger at the commit CI judged.
func TestAWaitingDefinitionIsAcceptedOnceCIOnTheHeadIsGreen(t *testing.T) {
	h := newHarness(t)
	originDir, head, identity := newOriginRepo(t)
	h.seedRunWithSnapshot("run-1", ciSnapshot, protocol.RunTarget{Repository: identity, BaseSHA: head})
	job := h.enqueue("run-1", identity)

	gateway := newFakeGateway()
	gateway.checks = func(_ string, poll int) ([]CICheck, error) {
		if poll < 3 {
			return []CICheck{check("test", CIPending), check("lint", CIPass)}, nil
		}
		return []CICheck{check("test", CIPass), check("lint", CIPass)}, nil
	}
	w := newCIWorker(t, h, writeAndDeclare(t, map[string]string{"work.txt": "work\n"}), gateway)

	attempt, err := w.ClaimOnce(context.Background())
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if attempt == nil || attempt.State != protocol.AttemptAccepted {
		t.Fatalf("attempt = %+v, want accepted", attempt)
	}
	pushed := remoteBranches(t, originDir)[protocol.PublishBranch(job.ID, 1)]
	summary := publishSummaryOf(t, attempt.Result)
	if !summary.Published() || summary.CIRef != pushed || len(summary.CIFailures) != 0 {
		t.Fatalf("publish summary = %+v, want published with ci_ref %s", summary, pushed)
	}
	if !slices.Contains(summary.Performed, protocol.PublishStepCI) {
		t.Fatalf("performed steps = %v, want ci among them", summary.Performed)
	}
	if steps := recordedSteps(t, w, attempt.ID); !slices.Equal(steps,
		[]string{"ci", "proof", "pull_request", "push"}) {
		t.Fatalf("ledger steps = %v, want all four", steps)
	}
	if gateway.checkPolls != 3 {
		t.Fatalf("CI was polled %d times, want 3 (pending, pending, green)", gateway.checkPolls)
	}
}

// ---- scenario: red, then a person's fix, then the retry ----------------------

// Red CI ends the attempt `accepted_unpublished` — branch and pull request in
// place, the red checks named — and the publish-only retry judges the
// branch's CURRENT head, so a fix a person pushed is what turns it green.
func TestRedCIEndsAcceptedUnpublishedAndTheRetryJudgesTheCurrentHead(t *testing.T) {
	h := newHarness(t)
	originDir, head, identity := newOriginRepo(t)
	h.seedRunWithSnapshot("run-1", ciSnapshot, protocol.RunTarget{Repository: identity, BaseSHA: head})
	job := h.enqueue("run-1", identity)
	branch := protocol.PublishBranch(job.ID, 1)

	var redSHA string
	gateway := newFakeGateway()
	gateway.checks = func(sha string, _ int) ([]CICheck, error) {
		if redSHA == "" || sha == redSHA {
			redSHA = sha
			return []CICheck{check("lint", CIPass), check("test", CIFail), check("e2e", CIPending)}, nil
		}
		return []CICheck{check("lint", CIPass), check("test", CIPass), check("e2e", CIPass)}, nil
	}
	w := newCIWorker(t, h, writeAndDeclare(t, map[string]string{"work.txt": "work\n"}), gateway)

	attempt, err := w.ClaimOnce(context.Background())
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if attempt == nil || attempt.State != protocol.AttemptAcceptedUnpublished {
		t.Fatalf("attempt = %+v, want accepted_unpublished", attempt)
	}
	summary := publishSummaryOf(t, attempt.Result)
	if summary.Published() || summary.Code != "ci_failed" {
		t.Fatalf("publish summary = %+v, want ci_failed", summary)
	}
	if len(summary.CIFailures) != 1 || summary.CIFailures[0].Name != "test" ||
		!strings.Contains(summary.Detail, "test (failure)") {
		t.Fatalf("ci failures = %+v detail %q, want exactly the red test check", summary.CIFailures, summary.Detail)
	}
	if gateway.checkPolls != 1 {
		t.Fatalf("CI was polled %d times; red is red, the wait fails fast", gateway.checkPolls)
	}
	if summary.PullRequestURL == "" || remoteBranches(t, originDir)[branch] != redSHA {
		t.Fatalf("summary = %+v, want the branch and pull request left in place", summary)
	}
	if steps := recordedSteps(t, w, attempt.ID); slices.Contains(steps, "ci") {
		t.Fatalf("ledger steps = %v, a red run must not record ci", steps)
	}
	jobAfter, err := h.store.Job(context.Background(), job.ID)
	if err != nil {
		t.Fatalf("read job: %v", err)
	}
	if jobAfter.State != protocol.JobAcceptedUnpublished {
		t.Fatalf("job state = %q, want accepted_unpublished", jobAfter.State)
	}

	fixed := pushToBranch(t, originDir, branch)
	retried, err := w.RetryPublish(context.Background(), job.ID, gateway, fastCIOptions())
	if err != nil {
		t.Fatalf("publish retry: %v", err)
	}
	if retried.State != protocol.AttemptAccepted {
		t.Fatalf("retried attempt state = %q (result %s), want accepted", retried.State, retried.Result)
	}
	retrySummary := publishSummaryOf(t, retried.Result)
	if retrySummary.CIRef != fixed {
		t.Fatalf("retry judged %s, want the person's fix %s", retrySummary.CIRef, fixed)
	}
	if !slices.Equal(retrySummary.Performed, []string{protocol.PublishStepCI}) {
		t.Fatalf("retry performed %v, want only ci — push, pull request, and proof were proven", retrySummary.Performed)
	}
	if created, _ := gateway.counts(); created != 1 {
		t.Fatalf("gateway created %d pull requests, want 1", created)
	}
}

// ---- scenario: no CI configured ---------------------------------------------

// A head with no checks at all passes after the registration grace instead
// of parking a worker slot for the full timeout.
func TestAHeadWithNoChecksPassesAfterTheRegistrationGrace(t *testing.T) {
	h := newHarness(t)
	_, head, identity := newOriginRepo(t)
	h.seedRunWithSnapshot("run-1", ciSnapshot, protocol.RunTarget{Repository: identity, BaseSHA: head})
	h.enqueue("run-1", identity)

	gateway := newFakeGateway() // checks == nil: the repository runs no CI
	w := newCIWorker(t, h, writeAndDeclare(t, map[string]string{"work.txt": "work\n"}), gateway)
	started := time.Now()
	attempt, err := w.ClaimOnce(context.Background())
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if attempt == nil || attempt.State != protocol.AttemptAccepted {
		t.Fatalf("attempt = %+v, want accepted", attempt)
	}
	if gateway.checkPolls < 2 || time.Since(started) < fastCIOptions().CIRegistrationGrace {
		t.Fatalf("passed after %d polls; an empty head must wait out the grace first", gateway.checkPolls)
	}
}

// ---- awaitCI edges -----------------------------------------------------------

// publishedWorker runs one ordinary (non-waiting) publish so the worker's
// repository cache and the remote branch exist, and returns what awaitCI
// needs to be driven directly.
func publishedWorker(t *testing.T) (*Worker, *fakeGateway, publishTarget, string) {
	t.Helper()
	h := newHarness(t)
	originDir, head, identity := newOriginRepo(t)
	h.seedRun("run-1", protocol.RunTarget{Repository: identity, BaseSHA: head})
	job := h.enqueue("run-1", identity)
	gateway := newFakeGateway()
	w := newCIWorker(t, h, writeAndDeclare(t, map[string]string{"work.txt": "work\n"}), gateway)
	attempt, err := w.ClaimOnce(context.Background())
	if err != nil || attempt == nil || attempt.State != protocol.AttemptAccepted {
		t.Fatalf("setup publish: attempt=%+v err=%v", attempt, err)
	}
	if gateway.checkPolls != 0 {
		t.Fatalf("a definition without publish.ci polled CI %d times", gateway.checkPolls)
	}
	branch := protocol.PublishBranch(job.ID, 1)
	target := publishTarget{
		attemptID: attempt.ID, jobID: job.ID, attemptNumber: 1,
		repository: identity, branch: branch, baseSHA: head,
		lease: newAttemptLease(w.client, attempt.ID, "unused"),
	}
	return w, gateway, target, remoteBranches(t, originDir)[branch]
}

func TestTheCIWaitTimesOutNamingThePendingChecks(t *testing.T) {
	w, gateway, target, pushed := publishedWorker(t)
	gateway.checks = func(string, int) ([]CICheck, error) {
		return []CICheck{check("lint", CIPass), check("slow-e2e", CIPending)}, nil
	}
	head, failures, err := w.awaitCI(context.Background(), gateway, fastCIOptions(), target, target.branch,
		40*time.Millisecond)
	if publishCode(err) != "ci_timeout" || !strings.Contains(err.Error(), "slow-e2e") {
		t.Fatalf("err = %v, want ci_timeout naming slow-e2e", err)
	}
	if head != pushed || len(failures) != 1 || failures[0].Name != "slow-e2e" {
		t.Fatalf("head=%s failures=%+v, want %s with the pending check", head, failures, pushed)
	}
}

func TestTheCIWaitStopsWhenTheJobIsCancelled(t *testing.T) {
	w, gateway, target, _ := publishedWorker(t)
	gateway.checks = func(string, int) ([]CICheck, error) {
		return []CICheck{check("test", CIPending)}, nil
	}
	target.lease.cancelOnce.Do(func() { close(target.lease.cancelled) })
	_, _, err := w.awaitCI(context.Background(), gateway, fastCIOptions(), target, target.branch, time.Hour)
	if publishCode(err) != "ci_wait_cancelled" {
		t.Fatalf("err = %v, want ci_wait_cancelled", err)
	}
}

func TestRepeatedCIReadFailuresEndTheWaitAsUnavailable(t *testing.T) {
	w, gateway, target, _ := publishedWorker(t)
	gateway.checks = func(string, int) ([]CICheck, error) {
		return nil, errors.New("HTTP 502 from api.github.com")
	}
	_, _, err := w.awaitCI(context.Background(), gateway, fastCIOptions(), target, target.branch, time.Hour)
	if publishCode(err) != "ci_unavailable" || !strings.Contains(err.Error(), "502") {
		t.Fatalf("err = %v, want ci_unavailable carrying the cause", err)
	}
	if gateway.checkPolls != protocol.MaxCITransientFailures {
		t.Fatalf("polled %d times, want %d", gateway.checkPolls, protocol.MaxCITransientFailures)
	}
}

// One transient read failure is not a verdict: the next poll decides.
func TestASingleCIReadFailureIsRetried(t *testing.T) {
	w, gateway, target, pushed := publishedWorker(t)
	gateway.checks = func(_ string, poll int) ([]CICheck, error) {
		if poll == 1 {
			return nil, errors.New("connection reset")
		}
		return []CICheck{check("test", CIPass)}, nil
	}
	head, _, err := w.awaitCI(context.Background(), gateway, fastCIOptions(), target, target.branch, time.Hour)
	if err != nil || head != pushed {
		t.Fatalf("head=%s err=%v, want green on %s", head, err, pushed)
	}
}

// ---- the gh gateway ----------------------------------------------------------

// CommitChecks reads check runs and commit statuses through `gh api`, and
// the normalization is what every verdict above rests on.
func TestGitHubGatewayNormalizesCheckRunsAndCommitStatuses(t *testing.T) {
	const sha = "0123456789abcdef0123456789abcdef01234567"
	var calls [][]string
	gateway := &GitHubCLIGateway{
		LookPath: func(string) (string, error) { return "/usr/bin/gh", nil },
		Run: func(_ context.Context, name string, arguments ...string) ([]byte, []byte, bool, bool, error) {
			calls = append(calls, append([]string{name}, arguments...))
			path := strings.Join(arguments, " ")
			switch {
			case strings.Contains(path, "/check-runs"):
				return []byte(strings.Join([]string{
					`{"name":"build","status":"completed","conclusion":"success","url":"https://ci/1"}`,
					`{"name":"docs","status":"completed","conclusion":"skipped","url":""}`,
					`{"name":"test","status":"completed","conclusion":"timed_out","url":"https://ci/3"}`,
					`{"name":"e2e","status":"in_progress","conclusion":"","url":""}`,
					"",
				}, "\n")), nil, false, false, nil
			case strings.Contains(path, "/status"):
				return []byte(`{"name":"legacy/ci","state":"error","url":"https://legacy"}` + "\n" +
					`{"name":"legacy/cov","state":"pending","url":""}` + "\n"), nil, false, false, nil
			}
			t.Fatalf("unexpected gh call %v", arguments)
			return nil, nil, false, false, nil
		},
	}
	checks, err := gateway.CommitChecks(context.Background(), "github.com/example/repo", sha)
	if err != nil {
		t.Fatalf("commit checks: %v", err)
	}
	got := map[string]string{}
	for _, c := range checks {
		got[c.Name] = c.Verdict
	}
	want := map[string]string{
		"build": CIPass, "docs": CIPass, "test": CIFail, "e2e": CIPending,
		"legacy/ci": CIFail, "legacy/cov": CIPending,
	}
	if len(got) != len(want) {
		t.Fatalf("checks = %+v, want %v", checks, want)
	}
	for name, verdict := range want {
		if got[name] != verdict {
			t.Fatalf("%s verdict = %q, want %q (all: %+v)", name, got[name], verdict, checks)
		}
	}
	if len(calls) != 2 {
		t.Fatalf("gh was called %d times, want check-runs and status", len(calls))
	}
	for _, call := range calls {
		joined := strings.Join(call, " ")
		if !strings.Contains(joined, "--paginate") || !strings.Contains(joined, "--jq") ||
			!strings.Contains(joined, "repos/example/repo/commits/"+sha+"/") {
			t.Fatalf("gh call %v does not read the commit's CI with pagination", call)
		}
	}

	if _, err := gateway.CommitChecks(context.Background(), "github.com/example/repo", "main;rm -rf"); publishCode(err) != "ci_invalid_ref" {
		t.Fatalf("a non-SHA ref: err = %v, want ci_invalid_ref", err)
	}
	if len(calls) != 2 {
		t.Fatal("an invalid ref reached gh")
	}
}

// Heads for the round wire test; the ledger checks their shape, not git.
const (
	headOne = "1111111111111111111111111111111111111111"
	headTwo = "2222222222222222222222222222222222222222"
)

// repairSnapshot declares a CI repair, so the round routes accept it.
const repairSnapshot = integrationSnapshot + `  - name: review
    kind: agent
    owner: builder
publish:
  ci:
    wait: true
    on_fail: {run: build, budget: 1}
`

// The worker reaches the round ledger through its client over the real
// control-plane handler: authorize, record, replay as Completed, and list.
// The ledger's rules are the control plane's tests; this proves the wire.
func TestCIRepairRoundsTravelTheClientAndHTTPSurface(t *testing.T) {
	h := newHarness(t)
	const identity = "github.com/example/repair"
	h.seedRunWithSnapshot("run-1", repairSnapshot, protocol.RunTarget{Repository: identity, BaseSHA: headOne})
	h.enqueue("run-1", identity)
	w := newTestWorker(t, h, filepath.Join(t.TempDir(), "worker"), 1, nil)
	ctx := context.Background()

	token, err := mintLeaseToken()
	if err != nil {
		t.Fatalf("mint lease token: %v", err)
	}
	claim, err := w.client.Claim(ctx, protocol.ClaimRequest{RequestID: "repair-wire", LeaseToken: token})
	if err != nil || claim == nil {
		t.Fatalf("claim: claim=%v err=%v", claim, err)
	}
	if _, err := w.client.StartAttempt(ctx, claim.Attempt.ID, protocol.StartAttemptRequest{LeaseToken: token}); err != nil {
		t.Fatalf("start attempt: %v", err)
	}
	branch := protocol.PublishBranch(claim.Job.ID, claim.Attempt.AttemptNumber)
	for _, step := range []protocol.PublishStepRequest{
		{Step: protocol.PublishStepPush, RemoteRef: headOne},
		{Step: protocol.PublishStepPullRequest, PullRequestURL: "https://github.com/example/repair/pull/1"},
		{Step: protocol.PublishStepProof, RemoteRef: headOne},
	} {
		step.LeaseToken, step.Branch = token, branch
		if _, err := w.client.RecordPublishStep(ctx, claim.Attempt.ID, step); err != nil {
			t.Fatalf("record %s: %v", step.Step, err)
		}
	}

	authorization, err := w.client.AuthorizeCIRepair(ctx, claim.Attempt.ID, protocol.CIRepairAuthorizationRequest{
		LeaseToken: token, Round: 1, Branch: branch, HeadBefore: headOne,
	})
	if err != nil || authorization.Budget != 1 || authorization.Completed != nil {
		t.Fatalf("authorize round 1: %+v err=%v", authorization, err)
	}
	record, err := w.client.RecordCIRepair(ctx, claim.Attempt.ID, protocol.CIRepairRecordRequest{
		LeaseToken: token, Round: 1, Branch: branch, HeadBefore: headOne, HeadAfter: headTwo,
		FailedChecks: []string{"lint"},
	})
	if err != nil || record.HeadAfter != headTwo || record.JobID != claim.Job.ID {
		t.Fatalf("record round 1: %+v err=%v", record, err)
	}
	again, err := w.client.AuthorizeCIRepair(ctx, claim.Attempt.ID, protocol.CIRepairAuthorizationRequest{
		LeaseToken: token, Round: 1, Branch: branch, HeadBefore: headOne,
	})
	if err != nil || again.Completed == nil || again.Completed.HeadAfter != headTwo {
		t.Fatalf("re-authorize a recorded round: %+v err=%v, want Completed", again, err)
	}
	_, err = w.client.AuthorizeCIRepair(ctx, claim.Attempt.ID, protocol.CIRepairAuthorizationRequest{
		LeaseToken: token, Round: 2, Branch: branch, HeadBefore: headTwo,
	})
	var rejected *APIError
	if !errors.As(err, &rejected) || rejected.Code != "ci_repair_budget_exhausted" {
		t.Fatalf("round 2 on a budget of 1: err=%v, want ci_repair_budget_exhausted over the wire", err)
	}
	rounds, err := w.client.AttemptCIRepairs(ctx, claim.Attempt.ID)
	if err != nil || len(rounds) != 1 || rounds[0].FailedChecks[0] != "lint" {
		t.Fatalf("list rounds: %+v err=%v", rounds, err)
	}
}

// ---- CI repair logs (U4) ------------------------------------------------------

// Check runs carry their id and app, which is how an Actions job's log is
// found later.
func TestCommitChecksCarriesTheCheckRunIDAndApp(t *testing.T) {
	gateway := &GitHubCLIGateway{
		LookPath: func(string) (string, error) { return "/usr/bin/gh", nil },
		Run: func(_ context.Context, _ string, arguments ...string) ([]byte, []byte, bool, bool, error) {
			if joined := strings.Join(arguments, " "); strings.Contains(joined, "/check-runs") {
				if !strings.Contains(joined, "id: .id") || !strings.Contains(joined, "app: (.app.slug") {
					t.Fatalf("the check-runs query does not ask for id and app: %s", joined)
				}
				return []byte(`{"name":"lint","status":"completed","conclusion":"failure","url":"https://ci/9","id":987,"app":"github-actions"}` + "\n"), nil, false, false, nil
			}
			return nil, nil, false, false, nil
		},
	}
	checks, err := gateway.CommitChecks(context.Background(), "github.com/example/repo",
		"0123456789abcdef0123456789abcdef01234567")
	if err != nil || len(checks) != 1 || checks[0].CheckRunID != 987 || checks[0].App != "github-actions" {
		t.Fatalf("checks = %+v err=%v, want id 987 from app github-actions", checks, err)
	}
}

// actionsLog is a job log the way Actions serves it: a timestamp on every
// line, colour codes, and the failure at the very end.
func actionsLog(lines int) []byte {
	var log strings.Builder
	for i := 0; i < lines; i++ {
		fmt.Fprintf(&log, "2026-09-28T03:04:05.1234567Z \x1b[36mstep output line %06d\x1b[0m\n", i)
	}
	log.WriteString("2026-09-28T03:04:06.0000000Z --- FAIL: TestCheckout (0.01s)\n")
	return []byte(log.String())
}

// FailedCheckLogs attaches a bounded, cleaned tail to Actions jobs only,
// within the log budget, and never fails: every check without a log says
// why.
func TestFailedCheckLogsAttachesBoundedTailsToActionsJobsOnly(t *testing.T) {
	var paths []string
	gateway := &GitHubCLIGateway{
		LookPath: func(string) (string, error) { return "/usr/bin/gh", nil },
		RunTail: func(_ context.Context, tailBytes int, _ string, arguments ...string) ([]byte, []byte, error) {
			path := arguments[len(arguments)-1]
			paths = append(paths, path)
			if strings.Contains(path, "/jobs/3/") {
				return nil, []byte("HTTP 410: logs expired"), errors.New("exit status 1")
			}
			log := actionsLog(2000)
			if len(log) > tailBytes {
				log = log[len(log)-tailBytes:]
			}
			return log, nil, nil
		},
	}
	checks := []CICheck{{Name: "legacy-status", Verdict: CIFail}}
	for id := int64(1); id <= 6; id++ {
		checks = append(checks, CICheck{Name: fmt.Sprintf("job-%d", id), Verdict: CIFail,
			App: "github-actions", CheckRunID: id})
	}
	annotated := gateway.FailedCheckLogs(context.Background(), "github.com/example/repo", checks)

	if checks[1].LogTail != "" {
		t.Fatal("FailedCheckLogs mutated its input")
	}
	if annotated[0].LogTail != "" || !strings.Contains(annotated[0].LogNote, "not a GitHub Actions job") {
		t.Fatalf("status check = %+v, want no log and a note", annotated[0])
	}
	if !strings.Contains(annotated[3].LogNote, "log unavailable") || !strings.Contains(annotated[3].LogNote, "410") {
		t.Fatalf("job 3 = %+v, want the gh failure as a note", annotated[3])
	}
	for _, i := range []int{1, 2, 4, 5} {
		tail := annotated[i].LogTail
		if len(tail) == 0 || len(tail) > protocol.MaxCIRepairLogBytesPerCheck {
			t.Fatalf("job %d tail is %d bytes, want 1..%d", i, len(tail), protocol.MaxCIRepairLogBytesPerCheck)
		}
		if !strings.HasSuffix(tail, "--- FAIL: TestCheckout (0.01s)\n") {
			t.Fatalf("job %d tail lost the failure at the end: %q", i, tail[len(tail)-80:])
		}
		if strings.Contains(tail, "2026-09-28T") || strings.Contains(tail, "\x1b[") {
			t.Fatalf("job %d tail kept timestamps or escape codes", i)
		}
		if !strings.HasPrefix(tail, "step output line ") {
			t.Fatalf("job %d tail does not start on a line boundary: %q", i, tail[:40])
		}
	}
	if annotated[6].LogTail != "" || !strings.Contains(annotated[6].LogNote, "budget") {
		t.Fatalf("job 6 = %+v, want no log: the budget went to the first %d jobs",
			annotated[6], protocol.MaxCIRepairLoggedChecks)
	}
	if len(paths) != 5 || paths[0] != "repos/example/repo/actions/jobs/1/logs" {
		t.Fatalf("log reads = %v, want jobs 1-5 (3 failed and did not use budget)", paths)
	}
}

// Without gh, or for a repository jig cannot publish to, every check gets
// a note and nothing is run.
func TestFailedCheckLogsDegradesToNotes(t *testing.T) {
	actions := []CICheck{{Name: "lint", Verdict: CIFail, App: "github-actions", CheckRunID: 1}}
	noGH := &GitHubCLIGateway{
		LookPath: func(string) (string, error) { return "", errors.New("not found") },
		RunTail: func(context.Context, int, string, ...string) ([]byte, []byte, error) {
			t.Fatal("ran gh without gh")
			return nil, nil, nil
		},
	}
	if got := noGH.FailedCheckLogs(context.Background(), "github.com/example/repo", actions); !strings.Contains(got[0].LogNote, "gh") {
		t.Fatalf("without gh = %+v, want a note naming gh", got[0])
	}
	withGH := &GitHubCLIGateway{
		LookPath: func(string) (string, error) { return "/usr/bin/gh", nil },
		RunTail: func(context.Context, int, string, ...string) ([]byte, []byte, error) {
			t.Fatal("read a log for a repository jig cannot publish to")
			return nil, nil, nil
		},
	}
	if got := withGH.FailedCheckLogs(context.Background(), "gitlab.com/example/repo", actions); !strings.Contains(got[0].LogNote, "publishes to GitHub only") {
		t.Fatalf("non-GitHub repository = %+v, want the unsupported-remote note", got[0])
	}
}

// A clean log longer than the cap keeps its last whole lines, and never
// more than the cap.
func TestCleanLogTailKeepsTheLastWholeLinesWithinTheCap(t *testing.T) {
	var log strings.Builder
	for i := 0; log.Len() < 3*protocol.MaxCIRepairLogBytesPerCheck; i++ {
		fmt.Fprintf(&log, "plain output line %06d\n", i)
	}
	log.WriteString("--- FAIL: TestLast\n")
	tail := cleanLogTail([]byte(log.String()), protocol.MaxCIRepairLogBytesPerCheck, false)
	if len(tail) > protocol.MaxCIRepairLogBytesPerCheck || len(tail) < protocol.MaxCIRepairLogBytesPerCheck-100 {
		t.Fatalf("tail is %d bytes, want just under %d", len(tail), protocol.MaxCIRepairLogBytesPerCheck)
	}
	if !strings.HasSuffix(tail, "--- FAIL: TestLast\n") || !strings.HasPrefix(tail, "plain output line ") {
		t.Fatalf("tail lost its end or starts mid-line: %q … %q", tail[:30], tail[len(tail)-30:])
	}
	if whole := cleanLogTail([]byte("one\ntwo\n"), protocol.MaxCIRepairLogBytesPerCheck, false); whole != "one\ntwo\n" {
		t.Fatalf("an uncut short log = %q, want it whole", whole)
	}
}

// The tail runner keeps the END of a command's output.
func TestRunTailCommandKeepsTheEndOfTheOutput(t *testing.T) {
	tail, _, err := runTailCommand(context.Background(), 12, "sh", "-c",
		`i=0; while [ $i -lt 5000 ]; do printf 'line %d\n' $i; i=$((i+1)); done; printf 'THE END\n'`)
	if err != nil || string(tail) != "999\nTHE END\n" {
		t.Fatalf("tail = %q err=%v, want the last 12 bytes", tail, err)
	}
}

// Only failed checks are read, and reads are capped whether or not they
// succeed, so a hanging gh costs a bounded time.
func TestFailedCheckLogsReadsOnlyFailuresAndCapsAttempts(t *testing.T) {
	reads := 0
	gateway := &GitHubCLIGateway{
		LookPath: func(string) (string, error) { return "/usr/bin/gh", nil },
		RunTail: func(context.Context, int, string, ...string) ([]byte, []byte, error) {
			reads++
			return nil, nil, context.DeadlineExceeded
		},
	}
	checks := []CICheck{{Name: "passing", Verdict: CIPass, App: "github-actions", CheckRunID: 99}}
	for id := int64(1); id <= 12; id++ {
		checks = append(checks, CICheck{Name: fmt.Sprintf("job-%d", id), Verdict: CIFail,
			App: "github-actions", CheckRunID: id})
	}
	annotated := gateway.FailedCheckLogs(context.Background(), "github.com/example/repo", checks)
	if reads != maxCILogReads {
		t.Fatalf("reads = %d, want the cap of %d even though every read failed", reads, maxCILogReads)
	}
	if annotated[0].LogNote != "" || annotated[0].LogTail != "" {
		t.Fatalf("passing check = %+v, want it untouched", annotated[0])
	}
	if last := annotated[len(annotated)-1]; !strings.Contains(last.LogNote, "reads were already attempted") {
		t.Fatalf("check past the read cap = %+v, want a note", last)
	}
}
