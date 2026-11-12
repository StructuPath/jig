// job.go — `jig job release`: the operator's request to finish publishing an
// accepted_unpublished job, served by the worker that ran it while that
// worker keeps running.
//
// The command does not publish anything itself and holds no lock: it records
// the request on the control plane and, unless told not to, watches the job
// until the owning worker has taken the request and the retry has ended. Its
// exit codes are `jig worker publish-retry`'s, so a script written against
// one works against the other.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/StructuPath/JIG-Software-Factory/internal/controlplane"
	"github.com/StructuPath/JIG-Software-Factory/internal/protocol"
)

const jobUsage = `Usage: jig job release [flags] <job-id>

Finish publishing an accepted_unpublished job — release a publish hold, or
retry a publish that did not complete — without stopping the worker. The
request is recorded on the control plane; the worker that ran the job takes
it on its next poll and runs the publish-only retry itself: the same attempt,
the same branch, the same pull request, no phase re-run and no inference
spent. A held job that is released is published as a pull request; jig never
merges it.

By default the command waits for the retry to end. If the owning worker is
not running, the request stays pending until it is; interrupt the wait with
Ctrl-C and the request stays recorded.

For a worker that is stopped, use jig worker publish-retry instead.

Flags:
  --server <url>  control plane base URL (default http://127.0.0.1:8383)
  --no-wait       record the request and return
  --json          emit the job as JSON

Exit codes:
  0  published: the job is accepted (or, with --no-wait, the request is
     recorded)
  1  the request could not be made or watched: the control plane is
     unreachable, it refused the request (the job is not
     accepted_unpublished), or the wait was interrupted
  2  usage error
  3  the retry ran but publish still did not finish; the job stays
     accepted_unpublished and can be released again
`

// jobReleasePollInterval is how often the wait re-reads the job. A variable
// so the command tests do not wait on a wall clock.
var jobReleasePollInterval = 2 * time.Second

func jobCommand(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		fmt.Fprint(stderr, jobUsage)
		if len(args) == 0 {
			return exitUsage
		}
		return exitAccepted
	}
	if args[0] != "release" {
		fmt.Fprintf(stderr, "jig job: unknown command %q\n\n%s", args[0], jobUsage)
		return exitUsage
	}
	return jobRelease(ctx, args[1:], stdout, stderr)
}

func jobRelease(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("jig job release", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.Usage = func() { fmt.Fprint(stderr, jobUsage) }
	server := flags.String("server", "http://"+defaultServerAuthority, "control plane URL")
	noWait := flags.Bool("no-wait", false, "record the request and return")
	asJSON := flags.Bool("json", false, "emit the job as JSON")
	if err := flags.Parse(args); err != nil {
		return exitUsage
	}
	if flags.NArg() != 1 {
		fmt.Fprint(stderr, jobUsage)
		return exitUsage
	}
	jobID := flags.Arg(0)
	jobPath := "/api/jobs/" + url.PathEscape(jobID)

	var requested protocol.Job
	if err := apiCall(ctx, *server, http.MethodPost, jobPath+"/publish-request", nil, &requested); err != nil {
		fmt.Fprintf(stderr, "jig job release: %v\n", err)
		return exitInfraFailed
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
	ticker := time.NewTicker(jobReleasePollInterval)
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
