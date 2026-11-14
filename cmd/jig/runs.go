// runs.go — `jig runs`: the operator's read of what was admitted, and the
// KTD9 escape hatch. A run is one invocation frozen by value (R2); its jobs
// are where failure, retry, and cancellation happen (`jig job`), so the run
// verbs only list, show, and re-admit.
package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/StructuPath/JIG-Software-Factory/internal/controlplane"
	"github.com/StructuPath/JIG-Software-Factory/internal/protocol"
)

const runsUsage = `Usage: jig runs <command> [flags]

List and inspect runs — one invocation of a definition, fanned out into one
job per target repository — and re-admit a run at current HEAD.

Commands:
  list               list runs, newest first
  show <run-id>      print one run and its jobs
  readmit <run-id>   admit a new run with the same frozen definition and
                     parameters, re-pinned at each target's current HEAD

Flags may come before or after the positional arguments.

Flags:
  --server <url>   control plane URL (default $JIG_SERVER or http://127.0.0.1:8383)
  --json           emit the API object instead of the table
  --limit <n>      (list) print at most n runs; 0 is all (default 20)

Exit codes:
  0  success
  1  the control plane could not be reached, or rejected the request
  2  usage error
`

const (
	runsListUsage    = "Usage: jig runs list [--limit <n>] [--server <url>] [--json]\n"
	runsShowUsage    = "Usage: jig runs show <run-id> [--server <url>] [--json]\n"
	runsReadmitUsage = "Usage: jig runs readmit <run-id> [--server <url>] [--json]\n"
)

// runsVerbs is the `jig runs` vocabulary (see dispatchSubcommand).
var runsVerbs = []subcommand{
	{name: "list", run: runsList},
	{name: "show", run: runsShow},
	{name: "readmit", run: runsReadmit},
}

func runsCommand(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	return dispatchSubcommand(ctx, "jig runs", runsUsage, runsVerbs, args, stdout, stderr)
}

func runsList(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags, server, asJSON := defFlags("jig runs list", stderr, runsListUsage)
	limit := flags.Int("limit", 20, "print at most n runs; 0 is all")
	if err := parseFlags(flags, args); err != nil {
		return flagExit(err)
	}
	switch {
	case flags.NArg() != 0:
		return usageError(stderr, "jig runs list", fmt.Sprintf("unexpected argument %q", flags.Arg(0)), runsListUsage)
	case *limit < 0:
		return usageError(stderr, "jig runs list", "--limit must not be negative", runsListUsage)
	}
	var runs []protocol.Run
	if err := apiCall(ctx, *server, http.MethodGet, "/api/runs", nil, &runs); err != nil {
		fmt.Fprintf(stderr, "jig runs list: %v\n", err)
		return apiExit(err)
	}
	if *limit > 0 && len(runs) > *limit {
		runs = runs[:*limit]
	}
	if *asJSON {
		return emitJSON(stdout, stderr, runs)
	}
	if len(runs) == 0 {
		fmt.Fprintln(stdout, "no runs yet; admit one with: jig def invoke <def> --instructions <text> --repo <r>")
		return exitAccepted
	}
	names := definitionNames(ctx, *server)
	table := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(table, "RUN\tSTATE\tDEFINITION\tTARGETS\tCREATED")
	for _, run := range runs {
		fmt.Fprintf(table, "%s\t%s\t%s\t%s\t%s\n", run.ID, run.State,
			definitionLabel(names, run), targetSummary(run.Targets),
			run.CreatedAt.Local().Format(time.DateTime))
	}
	table.Flush()
	return exitAccepted
}

func runsShow(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags, server, asJSON := defFlags("jig runs show", stderr, runsShowUsage)
	if err := parseFlags(flags, args); err != nil {
		return flagExit(err)
	}
	if flags.NArg() != 1 {
		return usageError(stderr, "jig runs show", "exactly one run id is required", runsShowUsage)
	}
	runID := flags.Arg(0)
	var view controlplane.RunView
	if err := apiCall(ctx, *server, http.MethodGet, "/api/runs/"+url.PathEscape(runID), nil, &view); err != nil {
		err = notFoundAs(err, "run", runID)
		fmt.Fprintf(stderr, "jig runs show: %v\n", err)
		return apiExit(err)
	}
	if *asJSON {
		return emitJSON(stdout, stderr, view)
	}
	run := view.Run
	fmt.Fprintf(stdout, "run:        %s\nstate:      %s\ndefinition: %s (generation %d)\ncreated:    %s\n",
		run.ID, run.State, definitionLabel(definitionNames(ctx, *server), run), run.DefinitionGeneration,
		run.CreatedAt.Local().Format(time.DateTime))
	if names := parameterNames(run.Parameters); names != "" {
		fmt.Fprintf(stdout, "params:     %s\n", names)
	}
	printRunJobs(stdout, view.Jobs)
	fmt.Fprintf(stdout, "view: %s\n", runViewURL(*server, run.ID))
	return exitAccepted
}

// runsReadmit is KTD9's escape hatch: a run whose pins went stale, or whose
// jobs were cancelled, is admitted again by value — the same frozen
// definition snapshot and parameters — at each target's current HEAD.
func runsReadmit(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags, server, asJSON := defFlags("jig runs readmit", stderr, runsReadmitUsage)
	if err := parseFlags(flags, args); err != nil {
		return flagExit(err)
	}
	if flags.NArg() != 1 {
		return usageError(stderr, "jig runs readmit", "exactly one run id is required", runsReadmitUsage)
	}
	runID := flags.Arg(0)
	var view controlplane.RunView
	if err := apiCall(ctx, *server, http.MethodPost,
		"/api/runs/"+url.PathEscape(runID)+"/readmit", nil, &view); err != nil {
		err = notFoundAs(err, "run", runID)
		fmt.Fprintf(stderr, "jig runs readmit: %v\n", err)
		return apiExit(err)
	}
	if *asJSON {
		return emitJSON(stdout, stderr, view)
	}
	fmt.Fprintf(stdout, "run: %s (%s), re-admitted from %s\n", view.Run.ID, view.Run.State, runID)
	printRunJobs(stdout, view.Jobs)
	fmt.Fprintf(stdout, "view: %s\n", runViewURL(*server, view.Run.ID))
	if liveWorkers(ctx, *server) == 0 {
		fmt.Fprintln(stdout, "note: no worker is online; start one with: jig worker")
	}
	return exitAccepted
}

func printRunJobs(stdout io.Writer, jobs []protocol.Job) {
	if len(jobs) == 0 {
		return
	}
	fmt.Fprintln(stdout)
	table := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(table, "JOB\tSTATE\tREPOSITORY\tBASE")
	for _, job := range jobs {
		state := job.State
		if job.CancellationRequested && job.State == protocol.JobActive {
			state += " (cancelling)"
		}
		fmt.Fprintf(table, "%s\t%s\t%s\t%s\n", job.ID, state, job.Repository, shortSHA(job.BaseSHA))
	}
	table.Flush()
	fmt.Fprintln(stdout)
}

// definitionNames maps definition ids to names for the tables. It is a
// label only: when the list cannot be read, rows fall back to the id.
func definitionNames(ctx context.Context, server string) map[string]string {
	var definitions []protocol.Definition
	if err := apiCall(ctx, server, http.MethodGet, "/api/definitions", nil, &definitions); err != nil {
		return nil
	}
	names := make(map[string]string, len(definitions))
	for _, definition := range definitions {
		names[definition.ID] = definition.Name
	}
	return names
}

func definitionLabel(names map[string]string, run protocol.Run) string {
	if name := names[run.DefinitionID]; name != "" {
		return name
	}
	return run.DefinitionID
}

func targetSummary(targets []protocol.RunTarget) string {
	switch len(targets) {
	case 0:
		return "-"
	case 1:
		return targets[0].Repository
	}
	return fmt.Sprintf("%s +%d", targets[0].Repository, len(targets)-1)
}

// parameterNames lists a run's frozen parameters by name only: the composed
// prompt alone is pages long, and `--json` carries the values.
func parameterNames(parameters map[string]string) string {
	names := make([]string, 0, len(parameters))
	for name := range parameters {
		names = append(names, name)
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

func shortSHA(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}
