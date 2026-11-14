package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"net/url"
	"strconv"
	"strings"

	"github.com/StructuPath/JIG-Software-Factory/internal/protocol"
)

// Merge follows the final CI verdict, including repair and rerun (merge plan U6).
func (w *Worker) mergePublished(ctx context.Context, gateway PullRequestGateway, target publishTarget, policy ciPolicy, summary PublishSummary) PublishSummary {
	if !summary.Published() || policy.mergeMethod == "" || target.wasHeld {
		return summary
	}
	summary.State = PublishStateFailed
	if !w.config.AllowMerge {
		summary.Code, summary.Detail = "merge_not_allowed", "start this worker with --allow-merge to merge never-held work"
		return summary
	}
	merged, err := w.publishStep(ctx, target, protocol.PublishStepMerge, map[string]protocol.PublishRecord{}, &summary,
		func(auth protocol.PublishAuthorization) (protocol.PublishStepRequest, error) {
			if auth.MergeHead == "" || auth.MergeHead != summary.CIRef {
				return protocol.PublishStepRequest{}, publishFailure("merge_head_moved", "the merge authorization does not match the CI head")
			}
			sha, err := gateway.MergePullRequest(ctx, target.repository, summary.PullRequestURL, policy.mergeMethod, auth.MergeHead)
			return protocol.PublishStepRequest{Step: protocol.PublishStepMerge, Branch: auth.Branch, RemoteRef: sha}, err
		})
	if err == nil {
		summary.State, summary.Code, summary.Detail = PublishStatePublished, "", ""
		summary.MergeCommit = merged.RemoteRef
	}
	return summary
}

// Use the immediate REST merge: gh pr merge can enqueue a PR even without
// --auto. A deferred merge cannot preserve Jig's exact-head fence (KTD2/6).
func (g *GitHubCLIGateway) MergePullRequest(ctx context.Context, repository, prURL, method, head string) (string, error) {
	project, err := githubProject(repository)
	if err != nil {
		return "", err
	}
	u, err := url.Parse(prURL)
	if err != nil || u.Scheme != "https" || u.Host != "github.com" || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return "", publishFailure("merge_failed", "invalid pull request URL")
	}
	prefix := "/" + project + "/pull/"
	if !strings.HasPrefix(u.Path, prefix) {
		return "", publishFailure("merge_failed", "pull request is outside the registered repository")
	}
	number := strings.TrimPrefix(u.Path, prefix)
	if number == "" || strings.Trim(number, "0123456789") != "" || !ciSHAPattern.MatchString(head) {
		return "", publishFailure("merge_failed", "invalid pull request number or expected head")
	}
	switch method {
	case "squash", "merge", "rebase":
	default:
		return "", publishFailure("merge_failed", "invalid merge method")
	}
	ctx, cancel := context.WithTimeout(ctx, protocol.PublishCommandTimeout)
	defer cancel()
	if _, err := g.lookPath()("gh"); err != nil {
		return "", publishFailure("gh_not_found", "GitHub CLI is not on PATH")
	}
	path := "repos/" + project + "/pulls/" + number
	read := func() (string, bool, error) {
		stdout, stderr, outLarge, errLarge, err := g.run()(ctx, "gh", "api", path)
		if err != nil || outLarge || errLarge {
			return "", false, ghDiagnostic("read merge state", err, stderr, outLarge, errLarge)
		}
		var pr struct {
			Merged      bool   `json:"merged"`
			MergeCommit string `json:"merge_commit_sha"`
			Head        struct {
				SHA string `json:"sha"`
			} `json:"head"`
		}
		if err := json.Unmarshal(stdout, &pr); err != nil {
			return "", false, publishFailure("merge_failed", "unreadable pull request state")
		}
		if pr.Head.SHA != head {
			return "", false, publishFailure("merge_head_moved", "pull request head differs from the head CI approved")
		}
		if pr.Merged && !ciSHAPattern.MatchString(pr.MergeCommit) {
			return "", false, publishFailure("merge_failed", "merged pull request has no valid merge commit")
		}
		return pr.MergeCommit, pr.Merged, nil
	}
	if sha, merged, err := read(); err != nil || merged {
		return sha, err
	}
	// -i puts the HTTP status line ahead of the body, so the failure is
	// classified by GitHub's status rather than by words in gh's stderr,
	// which change between gh releases and can quote the PR's own title.
	stdout, stderr, outLarge, errLarge, err := g.run()(ctx, "gh", "api", "-i", "--method", "PUT", path+"/merge", "-f", "sha="+head, "-f", "merge_method="+method)
	status, body := splitGHResponse(stdout)
	if err != nil || outLarge || errLarge {
		return "", publishFailure(mergeFailureCode(status, body), "%s",
			ghDiagnostic("merge pull request", err, stderr, outLarge, errLarge))
	}
	var result struct {
		Merged  bool   `json:"merged"`
		SHA     string `json:"sha"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return "", publishFailure("merge_failed", "unreadable merge response: %s", err)
	}
	if !result.Merged {
		return "", publishFailure("merge_blocked", "%s", boundedText(result.Message, protocol.MaxPublishDiagnosticBytes))
	}
	if !ciSHAPattern.MatchString(result.SHA) {
		return "", publishFailure("merge_failed", "merge response has no valid merge commit")
	}
	return result.SHA, nil
}

// mergeFailureCode maps the status of GitHub's merge endpoint onto the
// publish codes an operator acts on: 409 is GitHub refusing the sha fence
// (the head moved), 405 is "not mergeable" — a conflict when GitHub's
// message says so, otherwise a branch rule — and 403/422 are rules or
// validation that block the merge. No status (gh never got an answer) is
// merge_failed, which a publish retry resolves by re-reading the PR.
func mergeFailureCode(status int, body []byte) string {
	switch status {
	case 409:
		return "merge_head_moved"
	case 405:
		var answer struct {
			Message string `json:"message"`
		}
		if json.Unmarshal(body, &answer) == nil && strings.Contains(strings.ToLower(answer.Message), "conflict") {
			return "merge_conflict"
		}
		return "merge_blocked"
	case 403, 422:
		return "merge_blocked"
	}
	return "merge_failed"
}

// splitGHResponse splits `gh api -i` output into the HTTP status and the
// body. gh writes the status line ("HTTP/2.0 409 Conflict"), the headers,
// and a blank line before the body; status is 0 when the output does not
// start with a status line (gh failed before GitHub answered).
func splitGHResponse(output []byte) (int, []byte) {
	if !bytes.HasPrefix(output, []byte("HTTP/")) {
		return 0, output
	}
	line, rest, _ := bytes.Cut(output, []byte("\n"))
	fields := strings.Fields(string(line))
	if len(fields) < 2 {
		return 0, output
	}
	status, err := strconv.Atoi(fields[1])
	if err != nil {
		return 0, output
	}
	for len(rest) > 0 {
		var header []byte
		header, rest, _ = bytes.Cut(rest, []byte("\n"))
		if len(bytes.TrimSpace(header)) == 0 {
			break
		}
	}
	return status, rest
}
