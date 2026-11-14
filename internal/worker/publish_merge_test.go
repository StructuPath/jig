package worker

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/StructuPath/JIG-Software-Factory/internal/protocol"
)

const mergeSnapshot = ciSnapshot + "  merge: {method: squash}\n"
const mergedSHA = "3333333333333333333333333333333333333333"

type mergeGateway struct {
	*fakeGateway
	heads []string
	fail  error
}

func (g *mergeGateway) MergePullRequest(_ context.Context, _, _, _, head string) (string, error) {
	g.heads = append(g.heads, head)
	return mergedSHA, g.fail
}

func allowMerge(t *testing.T, w *Worker) {
	t.Helper()
	w.config.AllowMerge = true
	if _, err := w.RegisterOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestMergeAfterGreenAndRetryWithoutAnotherAgent(t *testing.T) {
	for _, code := range []string{"", "merge_conflict", "merge_blocked", "merge_head_moved", "merge_failed"} {
		name := code
		if name == "" {
			name = "green"
		}
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			_, head, identity := newOriginRepo(t)
			h.seedRunWithSnapshot("run-1", mergeSnapshot, protocol.RunTarget{Repository: identity, BaseSHA: head})
			job := h.enqueue("run-1", identity)
			gateway := &mergeGateway{fakeGateway: newFakeGateway()}
			gateway.checks = func(string, int) ([]CICheck, error) { return []CICheck{check("test", CIPass)}, nil }
			if code != "" {
				gateway.fail = publishFailure(code, "blocked")
			}
			calls := 0
			inner := writeAndDeclare(t, map[string]string{"work.txt": "work"})
			w := newCIWorker(t, h, RunnerFunc(func(ctx context.Context, a *PreparedAttempt) Outcome { calls++; return inner.Run(ctx, a) }), gateway)
			allowMerge(t, w)
			attempt, err := w.ClaimOnce(context.Background())
			if err != nil || attempt == nil {
				t.Fatalf("claim: %+v %v", attempt, err)
			}
			if code != "" {
				summary := publishSummaryOf(t, attempt.Result)
				if attempt.State != protocol.AttemptAcceptedUnpublished || summary.Code != code || summary.CIRef == "" || summary.PullRequestURL == "" {
					t.Fatalf("failed merge: %+v %+v", attempt, summary)
				}
				gateway.fail = nil
				retried, err := w.RetryPublish(context.Background(), job.ID, gateway, fastCIOptions())
				if err != nil {
					t.Fatal(err)
				}
				attempt = &retried
			}
			summary := publishSummaryOf(t, attempt.Result)
			if attempt.State != protocol.AttemptAccepted || summary.MergeCommit != mergedSHA || calls != 1 || gateway.heads[0] != summary.CIRef {
				t.Fatalf("merge: %+v %+v calls=%d heads=%v", attempt, summary, calls, gateway.heads)
			}
		})
	}
}

func TestMergeNeverRunsForReleasedHoldOrPersonsHead(t *testing.T) {
	for _, held := range []bool{false, true} {
		t.Run(map[bool]string{true: "held", false: "person fix"}[held], func(t *testing.T) {
			h := newHarness(t)
			origin, head, identity := newOriginRepo(t)
			h.seedRunWithSnapshot("run-1", mergeSnapshot, protocol.RunTarget{Repository: identity, BaseSHA: head})
			job := h.enqueue("run-1", identity)
			gateway := &mergeGateway{fakeGateway: newFakeGateway()}
			gateway.checks = func(string, int) ([]CICheck, error) { return []CICheck{check("test", CIFail)}, nil }
			inner := writeAndDeclare(t, map[string]string{"work.txt": "work"})
			w := newCIWorker(t, h, RunnerFunc(func(ctx context.Context, a *PreparedAttempt) Outcome {
				o := inner.Run(ctx, a)
				if held {
					o.PublishHold = "high risk"
				}
				return o
			}), gateway)
			allowMerge(t, w)
			attempt, err := w.ClaimOnce(context.Background())
			if err != nil || attempt == nil {
				t.Fatalf("claim: %v", err)
			}
			if !held {
				pushToBranch(t, origin, protocol.PublishBranch(job.ID, 1))
			}
			gateway.checks = func(string, int) ([]CICheck, error) { return []CICheck{check("test", CIPass)}, nil }
			retried, err := w.RetryPublish(context.Background(), job.ID, gateway, fastCIOptions())
			if err != nil {
				t.Fatal(err)
			}
			if len(gateway.heads) != 0 {
				t.Fatal("merged held or human-authored work")
			}
			if held && retried.State != protocol.AttemptAccepted {
				t.Fatalf("release: %+v", retried)
			}
			if !held && publishSummaryOf(t, retried.Result).Code != "merge_head_moved" {
				t.Fatalf("foreign head: %+v", retried)
			}
		})
	}
}

func TestMergeUsesRepairedHead(t *testing.T) {
	h := newHarness(t)
	_, head, identity := newOriginRepo(t)
	snapshot := repairLoopSnapshot(1) + "  merge: {method: squash}\n"
	h.seedRunWithSnapshot("run-1", snapshot, protocol.RunTarget{Repository: identity, BaseSHA: head})
	h.enqueue("run-1", identity)
	continuation := &scriptedContinuation{t: t, rounds: []roundScript{fixRound()}}
	script := &ciScript{red: map[string]bool{}}
	gateway := &mergeGateway{fakeGateway: script.gateway(1)}
	w := newCIWorker(t, h, repairRunner(t, continuation), gateway)
	allowMerge(t, w)
	attempt, err := w.ClaimOnce(context.Background())
	if err != nil || attempt == nil || attempt.State != protocol.AttemptAccepted {
		t.Fatalf("repair merge: %+v %v", attempt, err)
	}
	if len(script.judged) != 2 || len(gateway.heads) != 1 || gateway.heads[0] != script.judged[1] {
		t.Fatalf("judged=%v merged=%v", script.judged, gateway.heads)
	}
}

func TestGitHubMergeUsesImmediateExactHeadAndRecoversAfterLostResponse(t *testing.T) {
	head := strings.Repeat("1", 40)
	for _, scenario := range []string{"fresh", "already merged", "head moved", "queue required", "lost response"} {
		t.Run(scenario, func(t *testing.T) {
			puts := 0
			merged := scenario == "already merged"
			g := &GitHubCLIGateway{LookPath: func(string) (string, error) { return "gh", nil }}
			g.Run = func(_ context.Context, _ string, args ...string) ([]byte, []byte, bool, bool, error) {
				if len(args) == 2 && args[0] == "api" {
					remoteHead := head
					if scenario == "head moved" {
						remoteHead = strings.Repeat("2", 40)
					}
					b, _ := json.Marshal(map[string]any{"head": map[string]string{"sha": remoteHead}, "merged": merged, "merge_commit_sha": mergedSHA})
					return b, nil, false, false, nil
				}
				want := []string{"api", "-i", "--method", "PUT", "repos/example/repo/pulls/1/merge", "-f", "sha=" + head, "-f", "merge_method=squash"}
				if strings.Join(args, " ") != strings.Join(want, " ") {
					t.Fatalf("unsafe merge args: %v", args)
				}
				puts++
				if scenario == "queue required" {
					return []byte("HTTP/2.0 405 Method Not Allowed\nContent-Type: application/json\r\n\r\n" +
							`{"message":"Repository rule violations found\n\nChanges must be made through the merge queue"}`),
						[]byte("gh: Repository rule violations found (HTTP 405)"), false, false, errors.New("exit 1")
				}
				merged = true
				if scenario == "lost response" {
					return nil, nil, false, false, errors.New("connection lost")
				}
				return []byte("HTTP/2.0 200 OK\nContent-Type: application/json\r\n\r\n" +
					`{"merged":true,"sha":"` + mergedSHA + `"}`), nil, false, false, nil
			}
			sha, err := g.MergePullRequest(context.Background(), "github.com/example/repo", "https://github.com/example/repo/pull/1", "squash", head)
			if scenario == "lost response" {
				if err == nil {
					t.Fatal("lost response accepted")
				}
				sha, err = g.MergePullRequest(context.Background(), "github.com/example/repo", "https://github.com/example/repo/pull/1", "squash", head)
			}
			switch scenario {
			case "head moved":
				if publishCode(err) != "merge_head_moved" || puts != 0 {
					t.Fatalf("moved: %v puts=%d", err, puts)
				}
			case "queue required":
				if publishCode(err) != "merge_blocked" {
					t.Fatalf("queue: %v", err)
				}
			default:
				if err != nil || sha != mergedSHA || puts > 1 {
					t.Fatalf("merge sha=%s err=%v puts=%d", sha, err, puts)
				}
			}
		})
	}
}

// Failures are classified by the HTTP status `gh api -i` reports, never by
// words in stderr: a PR titled "Fix 403 conflict handling" must not turn a
// moved head into a conflict.
func TestGitHubMergeFailureIsClassifiedByHTTPStatus(t *testing.T) {
	head := strings.Repeat("1", 40)
	misleading := []byte("gh: Fix 403 conflict handling in 422 path (HTTP %d)")
	cases := []struct {
		name, response, want string
	}{
		{"head moved", "HTTP/2.0 409 Conflict\nContent-Type: application/json\r\n\r\n" +
			`{"message":"Head branch was modified. Review and try the merge again."}`, "merge_head_moved"},
		{"conflict", "HTTP/2.0 405 Method Not Allowed\n\r\n" +
			`{"message":"Pull Request has merge conflicts"}`, "merge_conflict"},
		{"not mergeable", "HTTP/2.0 405 Method Not Allowed\n\r\n" +
			`{"message":"Pull Request is not mergeable"}`, "merge_blocked"},
		{"forbidden", "HTTP/2.0 403 Forbidden\n\r\n" + `{"message":"Resource not accessible"}`, "merge_blocked"},
		{"invalid", "HTTP/2.0 422 Unprocessable Entity\n\r\n" + `{"message":"Validation Failed"}`, "merge_blocked"},
		{"server error", "HTTP/2.0 502 Bad Gateway\n\r\n" + `{"message":"conflict 409"}`, "merge_failed"},
		{"no answer", "", "merge_failed"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			g := &GitHubCLIGateway{LookPath: func(string) (string, error) { return "gh", nil }}
			g.Run = func(_ context.Context, _ string, args ...string) ([]byte, []byte, bool, bool, error) {
				if len(args) == 2 {
					b, _ := json.Marshal(map[string]any{"head": map[string]string{"sha": head}})
					return b, nil, false, false, nil
				}
				return []byte(c.response), misleading, false, false, errors.New("exit 1")
			}
			_, err := g.MergePullRequest(context.Background(), "github.com/example/repo",
				"https://github.com/example/repo/pull/1", "squash", head)
			if got := publishCode(err); got != c.want {
				t.Fatalf("code = %q, want %q (err %v)", got, c.want, err)
			}
		})
	}
}
