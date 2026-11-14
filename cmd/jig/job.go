// job.go — `jig job`: the operator's verbs over one job. show reads it (and
// with --follow waits for its verdict), cancel and retry are the store's
// own transitions, and release asks the worker that ran an
// accepted_unpublished job to finish publishing it while that worker keeps
// running.
//
// release does not publish anything itself and holds no lock: it records the
// request on the control plane and, unless told not to, watches the job
// until the owning worker has taken the request and the retry has ended. Its
// exit codes are `jig worker publish-retry`'s, so a script written against
// one works against the other.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/StructuPath/JIG-Software-Factory/internal/controlplane"
	"github.com/StructuPath/JIG-Software-Factory/internal/protocol"
)

const jobUsage = `Usage: jig job <command> <job-id> [flags]

Act on one job — one target repository of one run. Job ids are printed by
jig def invoke and jig runs show.

Commands:
  show <job-id>     print the job, its attempts, and its pull request
                    (--follow waits until it settles)
  cancel <job-id>   cancel a queued job, or ask an active one's worker to
                    stop it
  retry <job-id>    re-queue a failed job as a new attempt from phase 1
  release <job-id>  finish publishing an accepted_unpublished job

Flags:
  --server <url>  control plane base URL
                  (default $JIG_SERVER or http://127.0.0.1:8383)
  --json          emit the API object as JSON
  --follow        (show) poll until the job is accepted, accepted_unpublished,
                  failed, or cancelled, and exit with its outcome
  --no-wait       (release) record the request and return

Exit codes:
  0  success; with show --follow, the job is accepted
  1  the control plane could not be reached, or refused the request
  2  usage error
  3  show --follow: the job settled without being accepted (failed,
     cancelled, or accepted_unpublished — held or not yet published)

release: the request is recorded on the control plane; the worker that ran
the job takes it on its next poll and runs the publish-only retry itself:
the same attempt, the same branch, the same pull request, no phase re-run
and no inference spent. A held job that is released is published as a pull
request; jig never merges it. By default release waits for the retry to
end. If the owning worker is not running, the request stays pending until
it is; interrupt the wait with Ctrl-C and the request stays recorded. For a
worker that is stopped, use jig worker publish-retry instead. release exits
0 when the job is accepted (or, with --no-wait, the request is recorded), 1
when the request could not be made or watched (the control plane refused it
because the job is not accepted_unpublished, or the wait was interrupted),
and 3 when the retry ran but publish still did not finish; the job stays
accepted_unpublished and can be released again.
`

// jobPollInterval is how often release and show --follow re-read the job. A
// variable so the command tests do not wait on a wall clock.
var jobPollInterval = 2 * time.Second

// jobVerbs is the `jig job` vocabulary (see dispatchSubcommand).
var jobVerbs = []subcommand{
	{name: "show", run: jobShow},
	{name: "cancel", run: func(ctx context.Context, args []string, stdout, stderr io.Writer) int {
		return jobTransition(ctx, "cancel", args, stdout, stderr)
	}},
	{name: "retry", run: func(ctx context.Context, args []string, stdout, stderr io.Writer) int {
		return jobTransition(ctx, "retry", args, stdout, stderr)
	}},
	{name: "release", run: jobRelease},
}

func jobCommand(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	return dispatchSubcommand(ctx, "jig job", jobUsage, jobVerbs, args, stdout, stderr)
}

func jobIDUsage(action string, extra string) string {
	return "Usage: jig job " + action + " <job-id> [--server <url>] [--json]" + extra + "\n"
}

// jobSettled reports whether a job has reached a state nothing will move it
// out of without an operator: the verdict show --follow waits for. An
// accepted_unpublished job with a publish request pending is still moving —
// its worker is about to retry the publish.
func jobSettled(job protocol.Job) bool {
	switch job.State {
	case protocol.JobAccepted, protocol.JobFailed, protocol.JobCancelled:
		return true
	case protocol.JobAcceptedUnpublished:
		return job.PublishRequestedAt == nil
	}
	return false
}

func jobShow(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	usage := jobIDUsage("show", " [--follow]")
	flags, server, asJSON := defFlags("jig job show", stderr, usage)
	follow := flags.Bool("follow", false, "poll until the job settles")
	if err := parseFlags(flags, args); err != nil {
		return flagExit(err)
	}
	if flags.NArg() != 1 {
		return usageError(stderr, "jig job show", "exactly one job id is required", usage)
	}
	jobID := flags.Arg(0)
	jobPath := "/api/jobs/" + url.PathEscape(jobID)
	var detail controlplane.JobDetail
	if err := apiCall(ctx, *server, http.MethodGet, jobPath, nil, &detail); err != nil {
		err = notFoundAs(err, "job", jobID)
		fmt.Fprintf(stderr, "jig job show: %v\n", err)
		return apiExit(err)
	}
	if *follow {
		var err error
		if detail, err = followJob(ctx, *server, jobPath, detail, stderr); err != nil {
			fmt.Fprintf(stderr, "jig job show: %v\n", err)
			return exitInfraFailed
		}
	}
	if *asJSON {
		if code := emitJSON(stdout, stderr, detail); code != exitAccepted {
			return code
		}
	} else {
		printJob(stdout, detail)
	}
	if !*follow || detail.Job.State == protocol.JobAccepted {
		return exitAccepted
	}
	return exitRejected
}

// followJob polls until the job settles, narrating each state change on
// stderr so stdout stays the one final answer.
func followJob(
	ctx context.Context, server, jobPath string, detail controlplane.JobDetail, stderr io.Writer,
) (controlplane.JobDetail, error) {
	ticker := time.NewTicker(jobPollInterval)
	defer ticker.Stop()
	last := ""
	for {
		if state := detail.Job.State; state != last {
			fmt.Fprintf(stderr, "job %s: %s\n", detail.Job.ID, state)
			last = state
		}
		if jobSettled(detail.Job) {
			return detail, nil
		}
		select {
		case <-ctx.Done():
			return detail, context.Canceled
		case <-ticker.C:
		}
		if err := apiCall(ctx, server, http.MethodGet, jobPath, nil, &detail); err != nil {
			if ctx.Err() != nil {
				return detail, context.Canceled
			}
			return detail, err
		}
	}
}

// printJob is the job as a compact report: identity, state, its pull
// request, and one row per attempt.
func printJob(stdout io.Writer, detail controlplane.JobDetail) {
	job := detail.Job
	state := job.State
	if job.CancellationRequested && job.State == protocol.JobActive {
		state += " (cancellation requested)"
	}
	if job.PublishRequestedAt != nil {
		state += " (publish requested)"
	}
	fmt.Fprintf(stdout, "job:        %s\nrun:        %s\n", job.ID, job.RunID)
	if detail.Definition != "" {
		fmt.Fprintf(stdout, "definition: %s\n", detail.Definition)
	}
	fmt.Fprintf(stdout, "repository: %s\nbase:       %s\nstate:      %s\n",
		job.Repository, job.BaseSHA, state)
	for _, record := range detail.Publish {
		if record.PullRequestURL != "" {
			fmt.Fprintf(stdout, "pull:       %s\n", record.PullRequestURL)
			break
		}
	}
	if len(detail.Attempts) == 0 {
		return
	}
	fmt.Fprintln(stdout)
	table := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(table, "ATTEMPT\tSTATE\tRUNTIME\tNOTE")
	for _, attempt := range detail.Attempts {
		runtime := strings.TrimSpace(attempt.RuntimeName + " " + attempt.RuntimeVersion)
		note := attempt.Error
		if attempt.PublishHold != "" {
			note = "held: " + attempt.PublishHold
		}
		fmt.Fprintf(table, "%d\t%s\t%s\t%s\n",
			attempt.AttemptNumber, attempt.State, orDash(runtime), orDash(truncate(note, 80)))
	}
	table.Flush()
	switch {
	case job.State == protocol.JobFailed:
		fmt.Fprintf(stdout, "\nretry it with: jig job retry %s\n", job.ID)
	case job.State == protocol.JobAcceptedUnpublished && job.PublishRequestedAt == nil:
		fmt.Fprintf(stdout, "\nfinish publishing with: jig job release %s\n", job.ID)
	case job.State == protocol.JobCancelled:
		fmt.Fprintf(stdout, "\nre-admit its run with: jig runs readmit %s\n", job.RunID)
	}
}

// jobTransition is cancel and retry: one bodyless POST each, answered with
// the job as the store left it.
func jobTransition(ctx context.Context, action string, args []string, stdout, stderr io.Writer) int {
	command := "jig job " + action
	usage := jobIDUsage(action, "")
	flags, server, asJSON := defFlags(command, stderr, usage)
	if err := parseFlags(flags, args); err != nil {
		return flagExit(err)
	}
	if flags.NArg() != 1 {
		return usageError(stderr, command, "exactly one job id is required", usage)
	}
	jobID := flags.Arg(0)
	var job protocol.Job
	if err := apiCall(ctx, *server, http.MethodPost,
		"/api/jobs/"+url.PathEscape(jobID)+"/"+action, nil, &job); err != nil {
		err = notFoundAs(err, "job", jobID)
		fmt.Fprintf(stderr, "%s: %v\n", command, err)
		return apiExit(err)
	}
	if *asJSON {
		return emitJSON(stdout, stderr, job)
	}
	switch {
	case action == "retry":
		fmt.Fprintf(stdout, "job %s: %s — a worker runs it as a new attempt from phase 1\n", job.ID, job.State)
	case job.State == protocol.JobActive && job.CancellationRequested:
		// R5: only the worker performs the transition, on its next heartbeat.
		fmt.Fprintf(stdout, "job %s: cancellation requested — its worker stops it at the next heartbeat\n", job.ID)
	case job.State == protocol.JobCancelled:
		fmt.Fprintf(stdout, "job %s: cancelled\n", job.ID)
	default:
		fmt.Fprintf(stdout, "job %s is already %s; nothing to cancel\n", job.ID, job.State)
	}
	return exitAccepted
}

func orDash(value string) string {
	if value == "" {
		return "-"
	}
	return value
}

// truncate shortens one table cell to a line's worth, by rune.
func truncate(value string, limit int) string {
	value = strings.Join(strings.Fields(value), " ")
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return string(runes[:limit-1]) + "…"
}

func jobRelease(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	usage := jobIDUsage("release", " [--no-wait]") + "\nRun jig job --help for what release waits for and its exit codes.\n"
	flags, server, asJSON := defFlags("jig job release", stderr, usage)
	noWait := flags.Bool("no-wait", false, "record the request and return")
	if err := parseFlags(flags, args); err != nil {
		return flagExit(err)
	}
	if flags.NArg() != 1 {
		return usageError(stderr, "jig job release", "exactly one job id is required", usage)
	}
	jobID := flags.Arg(0)
	jobPath := "/api/jobs/" + url.PathEscape(jobID)

	var requested protocol.Job
	if err := apiCall(ctx, *server, http.MethodPost, jobPath+"/publish-request", nil, &requested); err != nil {
		err = notFoundAs(err, "job", jobID)
		fmt.Fprintf(stderr, "jig job release: %v\n", err)
		return apiExit(err)
	}
	if *noWait {
		if *asJSON {
			return emitJSON(stdout, stderr, requested)
		}
		fmt.Fprintf(stdout, "release requested for job %s; the worker that ran it will publish it\n", requested.ID)
		return exitAccepted
	}

	fmt.Fprintf(stderr, "release requested for job %s; waiting for the worker that ran it\n", requested.ID)
	detail, err := awaitRelease(ctx, *server, jobPath)
	if err != nil {
		fmt.Fprintf(stderr, "jig job release: %v\n", err)
		if errors.Is(err, context.Canceled) {
			fmt.Fprintln(stderr, "the request stays recorded; the worker will still take it")
		}
		return exitInfraFailed
	}
	if *asJSON {
		if code := emitJSON(stdout, stderr, detail.Job); code != exitAccepted {
			return code
		}
	} else {
		printRelease(stdout, detail)
	}
	if detail.Job.State == protocol.JobAccepted {
		return exitAccepted
	}
	return exitRejected
}

// awaitRelease polls the job until the request has been taken and the retry
// it started has ended. While the request is pending, or the retry holds its
// lease (the job is `active`), there is nothing to report yet. The wait ends
// on `accepted`, or on accepted_unpublished with no request left — the retry
// ran and publish still did not finish, or someone withdrew the request.
func awaitRelease(ctx context.Context, server, jobPath string) (controlplane.JobDetail, error) {
	ticker := time.NewTicker(jobPollInterval)
	defer ticker.Stop()
	for {
		var detail controlplane.JobDetail
		if err := apiCall(ctx, server, http.MethodGet, jobPath, nil, &detail); err != nil {
			if ctx.Err() != nil {
				return detail, context.Canceled
			}
			return detail, err
		}
		switch detail.Job.State {
		case protocol.JobAccepted:
			return detail, nil
		case protocol.JobAcceptedUnpublished:
			if detail.Job.PublishRequestedAt == nil {
				return detail, nil
			}
		case protocol.JobActive:
		default:
			return detail, fmt.Errorf("job %s is %s; it has no publish to finish",
				detail.Job.ID, detail.Job.State)
		}
		select {
		case <-ctx.Done():
			return detail, context.Canceled
		case <-ticker.C:
		}
	}
}

// printRelease is the release's answer: the job, its state, and either the
// pull request or why publish stopped again.
func printRelease(stdout io.Writer, detail controlplane.JobDetail) {
	fmt.Fprintf(stdout, "job: %s\nstate: %s\n", detail.Job.ID, detail.Job.State)
	for _, record := range detail.Publish {
		if record.PullRequestURL != "" {
			fmt.Fprintf(stdout, "pull request: %s\n", record.PullRequestURL)
			break
		}
	}
	if detail.Job.State == protocol.JobAccepted || len(detail.Attempts) == 0 {
		return
	}
	latest := detail.Attempts[len(detail.Attempts)-1]
	if latest.Error != "" {
		fmt.Fprintf(stdout, "publish did not finish: %s\n", latest.Error)
		return
	}
	fmt.Fprintln(stdout, "publish did not finish, or the request was withdrawn before a worker took it")
}
