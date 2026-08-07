// github_poll.go — GitHub admission by `gh` polling (U6, R13, KTD7).
//
// Webhooks cannot reach a loopback-only server, so admission asks GitHub
// rather than being told. That makes `gh` a subprocess on the control plane's
// critical path, and every rule in this file follows from treating it as one:
//
//   - FIXED ARGUMENT VECTORS, never a shell string. Nothing observed on
//     GitHub — a label, a branch name, a title — is ever interpolated into a
//     command; arguments come from validated configuration and are passed as
//     a slice, so there is no parsing layer for a crafted branch name to
//     escape from.
//   - BOUNDED OUTPUT. stdout and stderr are read into fixed-size buffers.
//     A `gh` that decides to print a million issues fails with a diagnostic
//     instead of taking the server's memory with it.
//   - STRICT JSON. Unknown fields are refused, trailing data is refused, and
//     every match is validated against the trigger that asked for it —
//     including that its URL names THIS repository and THIS number. A
//     response cannot smuggle in an issue from a repository the operator
//     never configured.
//   - A MATCH LIMIT. `gh` is asked for one more than the cap so an
//     over-limit answer is detectable; over the cap the poll admits NOTHING
//     and says so. A trigger that would fan out five hundred runs is a
//     configuration error, and admitting the first hundred of them would
//     bury the evidence.
//   - ONE ACTIONABLE DIAGNOSTIC PER FAILURE MODE. `gh_timed_out`,
//     `gh_unauthenticated`, `gh_match_limit`, `gh_not_found` — each names
//     what to do next, and none of them creates an occurrence. A failed check
//     admits nothing, ever.
//
// The seam is an interface: no unit test in this package reaches GitHub.
package controlplane

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os/exec"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/StructuPath/jig/internal/protocol"
)

// GitHubIssueMatch is one observed issue, already validated against the
// trigger that asked for it.
type GitHubIssueMatch struct {
	Number int      `json:"number"`
	Title  string   `json:"title"`
	URL    string   `json:"url"`
	State  string   `json:"state"`
	Body   string   `json:"body"`
	Labels []string `json:"labels"`
}

// GitHubPullRequestMatch is one observed pull request. HeadCommit is what
// makes a pull-request occurrence content-addressed: a new push is a new
// event, and the same push observed twice is not.
type GitHubPullRequestMatch struct {
	Number     int      `json:"number"`
	Title      string   `json:"title"`
	URL        string   `json:"url"`
	State      string   `json:"state"`
	Body       string   `json:"body"`
	Labels     []string `json:"labels"`
	BaseBranch string   `json:"base_branch"`
	HeadCommit string   `json:"head_commit"`
	IsDraft    bool     `json:"is_draft"`
}

// GitHubGateway is the seam over `gh`. Tests substitute a fake; the
// production implementation is GitHubCLIGateway below.
type GitHubGateway interface {
	ListIssues(ctx context.Context, config TriggerConfig) ([]GitHubIssueMatch, error)
	ListPullRequests(ctx context.Context, config TriggerConfig) ([]GitHubPullRequestMatch, error)
}

// checkFailure builds the typed diagnostic a failed check carries. 502 is the
// honest status: the fault is upstream, and the operator's next action is
// named in the message.
func checkFailure(code, message string, arguments ...any) error {
	if len(arguments) > 0 {
		message = fmt.Sprintf(message, arguments...)
	}
	return &ServiceError{Code: code, Message: boundedDiagnostic(message), Status: 502}
}

// ---- polling ----------------------------------------------------------------

// pollDueGitHubTriggers polls every enabled GitHub trigger whose interval has
// elapsed.
func (s *Store) pollDueGitHubTriggers(ctx context.Context, gateway GitHubGateway) error {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id FROM triggers
		WHERE kind IN ('github_issue', 'github_pull_request') AND enabled = 1
		  AND next_poll_at IS NOT NULL AND next_poll_at <= ?
		ORDER BY next_poll_at, id
	`, s.now().UnixMilli())
	if err != nil {
		return unavailable(err)
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return unavailable(err)
		}
		ids = append(ids, id)
	}
	if err := rows.Close(); err != nil {
		return unavailable(err)
	}
	var failures error
	for _, id := range ids {
		if ctx.Err() != nil {
			break
		}
		if err := s.PollGitHubTrigger(ctx, id, gateway); err != nil {
			failures = errors.Join(failures, err)
		}
	}
	return failures
}

// PollGitHubTrigger performs one check and commits an occurrence per NEW
// event. It is the commit half of commit-then-dispatch: nothing here creates
// a run, so a crash immediately after this returns loses nothing — the
// occurrences are durable and the next startup drives them.
//
// A failed check records its diagnostic on the trigger, schedules the next
// poll, and creates no occurrences whatsoever.
func (s *Store) PollGitHubTrigger(ctx context.Context, triggerID string, gateway GitHubGateway) error {
	trigger, err := s.Trigger(ctx, triggerID)
	if err != nil {
		return err
	}
	if trigger.Kind != TriggerGitHubIssue && trigger.Kind != TriggerGitHubPullRequest {
		return invalid("invalid_trigger_kind", "only a GitHub trigger can be polled")
	}
	if !trigger.Enabled {
		return conflict("trigger_disabled", "enable the trigger before polling it")
	}
	sources, err := s.observe(ctx, trigger, gateway)
	if err != nil {
		if recordErr := s.recordCheckFailure(ctx, trigger, err); recordErr != nil {
			return errors.Join(err, recordErr)
		}
		return err
	}
	now := s.now()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return unavailable(err)
	}
	defer tx.Rollback()
	for _, source := range sources {
		// A key collision means an earlier poll already committed this exact
		// event. That is the dedup working, not an anomaly, so it is silent —
		// counting it would turn "this issue is still open" into a rising
		// skip counter.
		if _, err := insertOccurrenceTx(ctx, tx,
			trigger.ID, githubRequestKey(trigger, source), OccurrencePending,
			source, nil, "", now); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE triggers SET last_checked_at = ?, next_poll_at = ?,
		    diagnostic_code = '', diagnostic = '', updated_at = ?
		WHERE id = ? AND enabled = 1
	`, now.UnixMilli(), s.nextPollAt(trigger, now), now.UnixMilli(), trigger.ID); err != nil {
		return unavailable(err)
	}
	if err := tx.Commit(); err != nil {
		return unavailable(err)
	}
	return nil
}

// observe runs the check and freezes each match as an occurrence source.
func (s *Store) observe(ctx context.Context, trigger Trigger, gateway GitHubGateway) ([]occurrenceSource, error) {
	if trigger.Kind == TriggerGitHubIssue {
		matches, err := gateway.ListIssues(ctx, trigger.Config)
		if err != nil {
			return nil, err
		}
		sources := make([]occurrenceSource, 0, len(matches))
		for _, match := range matches {
			sources = append(sources, occurrenceSource{
				Kind: TriggerGitHubIssue, Repository: trigger.Config.Repository,
				Number: match.Number, Title: match.Title, URL: match.URL,
				Body: boundedBody(match.Body), Labels: sortedLabels(match.Labels),
			})
		}
		return sources, nil
	}
	matches, err := gateway.ListPullRequests(ctx, trigger.Config)
	if err != nil {
		return nil, err
	}
	sources := make([]occurrenceSource, 0, len(matches))
	for _, match := range matches {
		if match.IsDraft && !trigger.Config.IncludeDrafts {
			continue
		}
		sources = append(sources, occurrenceSource{
			Kind: TriggerGitHubPullRequest, Repository: trigger.Config.Repository,
			Number: match.Number, Title: match.Title, URL: match.URL,
			Body: boundedBody(match.Body), Labels: sortedLabels(match.Labels),
			BaseBranch: match.BaseBranch, HeadSHA: match.HeadCommit,
		})
	}
	return sources, nil
}

// githubRequestKey derives the dedup key from EVENT CONTENT, never from the
// order polls happened in: the trigger, its kind, the repository, the number,
// and — for pull requests — the head SHA, so a new push is a new event and a
// re-observation of the same push is not (KTD7).
func githubRequestKey(trigger Trigger, source occurrenceSource) string {
	key := "trigger:" + trigger.ID + ":" + trigger.Kind + ":" +
		source.Repository + "#" + strconv.Itoa(source.Number)
	if source.HeadSHA != "" {
		key += "@" + source.HeadSHA
	}
	return key
}

func (s *Store) nextPollAt(trigger Trigger, now time.Time) int64 {
	interval := time.Duration(trigger.Config.PollIntervalSeconds) * time.Second
	if interval < protocol.MinTriggerPollInterval {
		interval = protocol.DefaultTriggerPollInterval
	}
	return now.Add(interval).UnixMilli()
}

// recordCheckFailure stores the diagnostic and schedules the next attempt.
// The trigger stays enabled: a `gh` timeout is a transient fact about the
// network, and disabling on it would turn a blip into an outage the operator
// has to notice and undo.
func (s *Store) recordCheckFailure(ctx context.Context, trigger Trigger, cause error) error {
	code, message := "gh_failed", cause.Error()
	var service *ServiceError
	if errors.As(cause, &service) {
		code, message = service.Code, service.Message
	}
	now := s.now()
	if _, err := s.db.ExecContext(ctx, `
		UPDATE triggers SET last_checked_at = ?, next_poll_at = ?,
		    diagnostic_code = ?, diagnostic = ?, updated_at = ?
		WHERE id = ?
	`, now.UnixMilli(), s.nextPollAt(trigger, now), code, boundedDiagnostic(message),
		now.UnixMilli(), trigger.ID); err != nil {
		return unavailable(err)
	}
	return nil
}

func boundedBody(body string) string {
	if len(body) <= protocol.MaxUntrustedSectionBytes {
		return body
	}
	return body[:protocol.MaxUntrustedSectionBytes]
}

// ---- the gh gateway ---------------------------------------------------------

// GitHubCLIGateway is the production GitHubGateway.
type GitHubCLIGateway struct {
	// LookPath and Run are injectable so the argument construction can be
	// exercised without a gh binary on PATH.
	LookPath func(string) (string, error)
	Run      func(ctx context.Context, name string, arguments ...string) (stdout, stderr []byte, stdoutTooLarge, stderrTooLarge bool, err error)
}

// NewGitHubCLIGateway builds the production gateway.
func NewGitHubCLIGateway() *GitHubCLIGateway {
	return &GitHubCLIGateway{LookPath: exec.LookPath, Run: runTriggerCommand}
}

func (g *GitHubCLIGateway) lookPath() func(string) (string, error) {
	if g.LookPath != nil {
		return g.LookPath
	}
	return exec.LookPath
}

func (g *GitHubCLIGateway) run() func(context.Context, string, ...string) ([]byte, []byte, bool, bool, error) {
	if g.Run != nil {
		return g.Run
	}
	return runTriggerCommand
}

// ListIssues runs `gh issue list` with a fixed argument vector.
func (g *GitHubCLIGateway) ListIssues(ctx context.Context, config TriggerConfig) ([]GitHubIssueMatch, error) {
	project, err := githubProjectPath(config.Repository)
	if err != nil {
		return nil, err
	}
	if _, err := g.lookPath()("gh"); err != nil {
		return nil, checkFailure("gh_not_found",
			"the GitHub CLI (gh) was not found on PATH. Install gh, then run `gh auth login`.")
	}
	arguments := []string{
		"issue", "list", "--repo", project, "--state", config.State,
		"--limit", strconv.Itoa(protocol.MaxTriggerMatches + 1),
		"--json", "number,title,url,state,body,labels",
	}
	for _, label := range config.RequiredLabels {
		arguments = append(arguments, "--label", label)
	}
	stdout, stderr, stdoutTooLarge, stderrTooLarge, runErr := g.run()(ctx, "gh", arguments...)
	if err := ghRunDiagnostic("gh issue list", runErr, stderr, stdoutTooLarge, stderrTooLarge); err != nil {
		return nil, err
	}
	var values []struct {
		Number int    `json:"number"`
		Title  string `json:"title"`
		URL    string `json:"url"`
		State  string `json:"state"`
		Body   string `json:"body"`
		Labels []struct {
			ID          string `json:"id"`
			Name        string `json:"name"`
			Description string `json:"description"`
			Color       string `json:"color"`
		} `json:"labels"`
	}
	if err := decodeStrictJSON(stdout, &values); err != nil {
		return nil, checkFailure("gh_malformed_output",
			"gh issue list returned malformed or unexpected JSON: %s", err)
	}
	if values == nil {
		return nil, checkFailure("gh_malformed_output", "gh issue list returned null instead of a JSON array")
	}
	if len(values) > protocol.MaxTriggerMatches {
		return nil, checkFailure("gh_match_limit",
			"gh issue list returned more than %d issues. Add required labels or narrow the issue state; "+
				"nothing was admitted.", protocol.MaxTriggerMatches)
	}
	matches := make([]GitHubIssueMatch, 0, len(values))
	seen := make(map[int]bool, len(values))
	for index, value := range values {
		labels := make([]string, 0, len(value.Labels))
		for _, label := range value.Labels {
			labels = append(labels, label.Name)
		}
		match := GitHubIssueMatch{
			Number: value.Number, Title: strings.TrimSpace(value.Title),
			URL: strings.TrimSpace(value.URL), State: strings.ToLower(strings.TrimSpace(value.State)),
			Body: value.Body, Labels: labels,
		}
		if err := validateIssueMatch(config, match); err != nil {
			return nil, checkFailure("gh_invalid_output", "gh issue list result %d is invalid: %s", index+1, err)
		}
		if seen[match.Number] {
			return nil, checkFailure("gh_conflicting_duplicate",
				"gh issue list returned issue #%d twice", match.Number)
		}
		seen[match.Number] = true
		matches = append(matches, match)
	}
	return matches, nil
}

// ListPullRequests runs `gh pr list` with a fixed argument vector, once per
// configured base branch (gh takes one --base at a time).
func (g *GitHubCLIGateway) ListPullRequests(ctx context.Context, config TriggerConfig) ([]GitHubPullRequestMatch, error) {
	project, err := githubProjectPath(config.Repository)
	if err != nil {
		return nil, err
	}
	if _, err := g.lookPath()("gh"); err != nil {
		return nil, checkFailure("gh_not_found",
			"the GitHub CLI (gh) was not found on PATH. Install gh, then run `gh auth login`.")
	}
	branches := config.BaseBranches
	if len(branches) == 0 {
		branches = []string{""}
	}
	matches := make([]GitHubPullRequestMatch, 0)
	seen := make(map[int]bool)
	for _, branch := range branches {
		values, err := g.listPullRequestsForBase(ctx, project, config, branch)
		if err != nil {
			return nil, err
		}
		for _, match := range values {
			if seen[match.Number] {
				continue
			}
			seen[match.Number] = true
			matches = append(matches, match)
			if len(matches) > protocol.MaxTriggerMatches {
				return nil, checkFailure("gh_match_limit",
					"gh pr list returned more than %d pull requests. Add required labels or base "+
						"branches; nothing was admitted.", protocol.MaxTriggerMatches)
			}
		}
	}
	return matches, nil
}

func (g *GitHubCLIGateway) listPullRequestsForBase(
	ctx context.Context, project string, config TriggerConfig, baseBranch string,
) ([]GitHubPullRequestMatch, error) {
	arguments := []string{
		"pr", "list", "--repo", project, "--state", config.State,
		"--limit", strconv.Itoa(protocol.MaxTriggerMatches + 1),
		"--json", "number,title,url,state,body,labels,isDraft,baseRefName,headRefOid",
	}
	if baseBranch != "" {
		arguments = append(arguments, "--base", baseBranch)
	}
	for _, label := range config.RequiredLabels {
		arguments = append(arguments, "--label", label)
	}
	stdout, stderr, stdoutTooLarge, stderrTooLarge, runErr := g.run()(ctx, "gh", arguments...)
	if err := ghRunDiagnostic("gh pr list", runErr, stderr, stdoutTooLarge, stderrTooLarge); err != nil {
		return nil, err
	}
	var values []struct {
		Number      int    `json:"number"`
		Title       string `json:"title"`
		URL         string `json:"url"`
		State       string `json:"state"`
		Body        string `json:"body"`
		IsDraft     *bool  `json:"isDraft"`
		BaseRefName string `json:"baseRefName"`
		HeadRefOID  string `json:"headRefOid"`
		Labels      []struct {
			ID          string `json:"id"`
			Name        string `json:"name"`
			Description string `json:"description"`
			Color       string `json:"color"`
		} `json:"labels"`
	}
	if err := decodeStrictJSON(stdout, &values); err != nil {
		return nil, checkFailure("gh_malformed_output",
			"gh pr list returned malformed or unexpected JSON: %s", err)
	}
	if values == nil {
		return nil, checkFailure("gh_malformed_output", "gh pr list returned null instead of a JSON array")
	}
	if len(values) > protocol.MaxTriggerMatches {
		return nil, checkFailure("gh_match_limit",
			"gh pr list returned more than %d pull requests for one base branch. Add required "+
				"labels or narrow the state; nothing was admitted.", protocol.MaxTriggerMatches)
	}
	matches := make([]GitHubPullRequestMatch, 0, len(values))
	for index, value := range values {
		if value.IsDraft == nil {
			return nil, checkFailure("gh_malformed_output",
				"gh pr list result %d is missing isDraft", index+1)
		}
		labels := make([]string, 0, len(value.Labels))
		for _, label := range value.Labels {
			labels = append(labels, label.Name)
		}
		match := GitHubPullRequestMatch{
			Number: value.Number, Title: strings.TrimSpace(value.Title),
			URL: strings.TrimSpace(value.URL), State: strings.ToLower(strings.TrimSpace(value.State)),
			Body: value.Body, Labels: labels, IsDraft: *value.IsDraft,
			BaseBranch: strings.TrimSpace(value.BaseRefName),
			HeadCommit: strings.TrimSpace(value.HeadRefOID),
		}
		if err := validatePullRequestMatch(config, match); err != nil {
			return nil, checkFailure("gh_invalid_output", "gh pr list result %d is invalid: %s", index+1, err)
		}
		matches = append(matches, match)
	}
	return matches, nil
}

// ghRunDiagnostic turns one failed invocation into the one diagnostic that
// names the operator's next action. Ordering matters: the bounded-output
// checks come first because a truncated stream makes every later inference
// unreliable.
func ghRunDiagnostic(command string, runErr error, stderr []byte, stdoutTooLarge, stderrTooLarge bool) error {
	if stdoutTooLarge {
		return checkFailure("gh_output_too_large",
			"%s printed more than %d bytes. Narrow the state or required labels.",
			command, protocol.MaxTriggerStdoutBytes)
	}
	if stderrTooLarge {
		return checkFailure("gh_error_output_too_large",
			"%s wrote more than %d bytes to stderr. Run `gh auth status` and retry.",
			command, protocol.MaxTriggerStderrBytes)
	}
	if runErr == nil {
		return nil
	}
	if errors.Is(runErr, context.DeadlineExceeded) {
		return checkFailure("gh_timed_out",
			"%s did not finish within %s. Check GitHub connectivity and narrow the match.",
			command, protocol.TriggerPollTimeout)
	}
	if errors.Is(runErr, context.Canceled) {
		return checkFailure("gh_cancelled", "%s was cancelled before it finished.", command)
	}
	message := strings.TrimSpace(string(stderr))
	lower := strings.ToLower(message)
	switch {
	case containsAny(lower, "auth", "login", "not logged", "authentication"):
		return checkFailure("gh_unauthenticated",
			"gh is not authenticated for github.com. Run `gh auth login`, then verify with `gh auth status`.")
	case containsAny(lower, "permission", "forbidden", "resource not accessible",
		"could not resolve to a repository", "not found"):
		return checkFailure("gh_permission_denied",
			"gh cannot access this repository. Verify repository access and token scopes with `gh auth status`.")
	}
	if message == "" {
		message = runErr.Error()
	}
	return checkFailure("gh_failed",
		"%s failed: %s. Run `gh auth status` and verify repository access.", command, message)
}

func containsAny(value string, needles ...string) bool {
	for _, needle := range needles {
		if strings.Contains(value, needle) {
			return true
		}
	}
	return false
}

// ---- match validation -------------------------------------------------------

// validateIssueMatch checks that a returned issue is one THIS trigger asked
// for. The URL check is the important one: it is what stops a response from
// naming a repository the operator never configured, which would otherwise
// become the run's target.
func validateIssueMatch(config TriggerConfig, match GitHubIssueMatch) error {
	if err := validateMatchIdentity(config, match.Number, match.Title, match.URL, "issues"); err != nil {
		return err
	}
	if config.State != "all" && match.State != config.State {
		return fmt.Errorf("state %q does not match the configured state %q", match.State, config.State)
	}
	return validateMatchLabels(config, match.Labels)
}

func validatePullRequestMatch(config TriggerConfig, match GitHubPullRequestMatch) error {
	if err := validateMatchIdentity(config, match.Number, match.Title, match.URL, "pull"); err != nil {
		return err
	}
	if config.State != "all" && match.State != config.State {
		return fmt.Errorf("state %q does not match the configured state %q", match.State, config.State)
	}
	if match.BaseBranch == "" || len(match.BaseBranch) > 255 {
		return errors.New("base branch must be nonblank and at most 255 bytes")
	}
	if len(config.BaseBranches) > 0 {
		found := false
		for _, configured := range config.BaseBranches {
			if configured == match.BaseBranch {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("base branch %q is not one of the configured base branches", match.BaseBranch)
		}
	}
	// A 40-character lowercase hex SHA is required, not merely preferred: it
	// becomes the run's pinned base commit, and admission refuses anything
	// else. Failing here names the real cause instead of surfacing later as a
	// confusing invalid_base_sha at dispatch.
	if !isCommitSHA(match.HeadCommit) {
		return errors.New("head commit must be a full 40-character lowercase hexadecimal SHA")
	}
	return validateMatchLabels(config, match.Labels)
}

func validateMatchIdentity(config TriggerConfig, number int, title, rawURL, segment string) error {
	if number < 1 || int64(number) > int64(1<<31-1) {
		return errors.New("number must be a positive 32-bit integer")
	}
	if title == "" || utf8.RuneCountInString(title) > 500 || len(title) > 2<<10 {
		return errors.New("title must be nonblank, at most 500 characters, and at most 2 KiB")
	}
	if len(rawURL) > 2048 {
		return errors.New("URL exceeds 2 KiB")
	}
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.Scheme != "https" || !strings.EqualFold(parsed.Host, "github.com") ||
		parsed.RawQuery != "" || parsed.Fragment != "" {
		return errors.New("URL must be an HTTPS github.com URL with no query or fragment")
	}
	expected := "/" + strings.TrimPrefix(config.Repository, "github.com/") + "/" + segment + "/" + strconv.Itoa(number)
	if !strings.EqualFold(strings.TrimSuffix(parsed.Path, "/"), expected) {
		return fmt.Errorf("URL %q does not identify %s #%d", rawURL, config.Repository, number)
	}
	return nil
}

func validateMatchLabels(config TriggerConfig, labels []string) error {
	if len(labels) > 100 {
		return errors.New("more than 100 labels")
	}
	total := 0
	for _, label := range labels {
		total += len(label)
		if label == "" || label != strings.TrimSpace(label) || len(label) > 200 {
			return errors.New("contains an invalid label")
		}
	}
	if total > 8<<10 {
		return errors.New("labels exceed 8 KiB")
	}
	for _, required := range config.RequiredLabels {
		found := false
		for _, label := range labels {
			if strings.EqualFold(required, label) {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("missing required label %q", required)
		}
	}
	return nil
}

// githubProjectPath derives the "owner/repo" argument `gh --repo` takes from
// the stored canonical identity — never from anything observed on GitHub.
func githubProjectPath(identity string) (string, error) {
	project, found := strings.CutPrefix(identity, "github.com/")
	if !found || project == "" || strings.Count(project, "/") != 1 {
		return "", checkFailure("invalid_target",
			"repository %q is not a github.com owner/repo identity", identity)
	}
	return project, nil
}

// ---- bounded subprocess execution -------------------------------------------

// runTriggerCommand runs one command with a wall-clock timeout and fixed-size
// output buffers, reporting truncation of either stream separately so the
// caller can say which bound was hit.
func runTriggerCommand(
	ctx context.Context, executable string, arguments ...string,
) ([]byte, []byte, bool, bool, error) {
	commandContext, cancel := context.WithTimeout(ctx, protocol.TriggerPollTimeout)
	defer cancel()
	command := exec.CommandContext(commandContext, executable, arguments...)
	stdout := &boundedBuffer{limit: protocol.MaxTriggerStdoutBytes}
	stderr := &boundedBuffer{limit: protocol.MaxTriggerStderrBytes}
	command.Stdout = stdout
	command.Stderr = stderr
	err := command.Run()
	if commandContext.Err() != nil {
		// The context's own error is the true cause: a killed process reports
		// only "signal: killed", which names the symptom, not the timeout.
		err = commandContext.Err()
	}
	return stdout.Bytes(), stderr.Bytes(), stdout.truncated, stderr.truncated, err
}

// boundedBuffer accepts writes up to its limit, records that it truncated,
// and keeps claiming full writes so the writer never sees a short-write error
// and starts retrying.
type boundedBuffer struct {
	buffer    bytes.Buffer
	limit     int
	truncated bool
}

func (b *boundedBuffer) Write(value []byte) (int, error) {
	original := len(value)
	remaining := b.limit - b.buffer.Len()
	if remaining > 0 {
		if len(value) > remaining {
			value = value[:remaining]
		}
		_, _ = b.buffer.Write(value)
	}
	if original > remaining {
		b.truncated = true
	}
	return original, nil
}

func (b *boundedBuffer) Bytes() []byte { return b.buffer.Bytes() }

// decodeStrictJSON refuses unknown fields and trailing data. `gh` growing a
// field is a contract change worth noticing, and a second JSON value in one
// response is not something to read past.
func decodeStrictJSON(body []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); err == nil {
		return errors.New("response contained more than one JSON value")
	} else if !errors.Is(err, io.EOF) {
		return fmt.Errorf("trailing data after the JSON array: %w", err)
	}
	return nil
}
