// publish_ci_test.go — the opt-in CI wait: a definition with publish.ci.wait
// reaches `accepted` only once the branch's head is green, ends
// `accepted_unpublished` with the red checks named when it is not, and a
// publish-only retry judges whatever the branch's head is by then. The remote
// is a real bare repository; CI itself is the scripted fake gateway.
package worker

import (
	"context"
	"errors"
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
