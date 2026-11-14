// def.go — `jig def` and `jig trigger` (U9): the operator's surface over the
// control plane's definition and admission-trigger routes, which U6 left as
// HTTP-only.
//
// Two conventions run through everything here:
//
//   - Every read command takes --json and prints the API's own object
//     verbatim. jig's operator is very often an agent, and an agent that has
//     to scrape a prose table is an agent that will misread it. The prose
//     form is the convenience; the JSON form is the contract.
//   - Validation is local and comes first. `jig def validate` never touches
//     the network, and create/update parse the file before sending it, so a
//     malformed definition is named by the same message whether or not a
//     server is running.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/StructuPath/JIG-Software-Factory/internal/controlplane"
	"github.com/StructuPath/JIG-Software-Factory/internal/protocol"
)

const defUsage = `Usage: jig def <command> [flags]

Author and invoke job definitions against a running control plane.

Commands:
  validate <file>              parse and validate a definition file locally
  create <file>                save a new definition
  update <def> <file>          replace a definition's source in place
  list                         list saved definitions
  show <def>                   print one definition, source included
  invoke <def> --repo <r> ...  admit a run of a definition

<def> is a definition's id or its name. Flags may come before or after the
positional arguments; everything after a bare -- is positional.

Flags:
  --server <url>   control plane URL (default $JIG_SERVER or http://127.0.0.1:8383)
  --json           emit the API object instead of the prose report
  --instructions   (invoke) the trusted, operator-authored prompt half
  --repo <r>       (invoke) a target repository, repeatable: a local path
                   (relative paths resolve here), file:///path, or
                   github.com/owner/repo
  --param k=v      (invoke) an extra frozen run parameter; repeatable

Exit codes:
  0  success
  1  the control plane could not be reached, or rejected the request
  2  usage or definition-validation error
`

const triggerUsage = `Usage: jig trigger <command> [flags]

Manage standing admission triggers: cron schedules and GitHub polls.

Commands:
  create        save a trigger
  list          list triggers with their counters and diagnostics
  show <id>     print one trigger
  enable <id>
  disable <id>

Flags may come before or after the positional arguments.

Create flags:
  --def <def>           definition the trigger admits, by id or name (required)
  --name <name>         human name (required)
  --kind <kind>         schedule | github_issue | github_pull_request
  --instructions <text> trusted prompt half, frozen at save (required)
  --cron "<expr>"       (schedule) five-field expression
  --timezone <IANA>     (schedule) e.g. America/Denver
  --repo <r>            (schedule) target repository, repeatable;
                        (github) the single repository watched and run against
  --state <state>       (github) open | closed | all | merged
  --label <label>       (github) required label, repeatable
  --base-branch <b>     (github_pull_request) base branch, repeatable
  --include-drafts      (github_pull_request) include draft pull requests
  --poll-seconds <n>    (github) how often ` + "`gh`" + ` may be spent

Common flags:
  --server <url>   control plane URL (default $JIG_SERVER or http://127.0.0.1:8383)
  --json           emit the API object instead of the prose report

Exit codes:
  0  success
  1  the control plane could not be reached, or rejected the request
  2  usage or validation error
`

// Per-subcommand usage: what a usage error prints after naming the problem,
// so the operator reads one command's synopsis rather than the whole family.
const (
	defValidateUsage = "Usage: jig def validate [--json] <file>...\n"
	defCreateUsage   = "Usage: jig def create [--server <url>] [--json] <file>\n"
	defUpdateUsage   = "Usage: jig def update [--server <url>] [--json] <def> <file>\n"
	defListUsage     = "Usage: jig def list [--server <url>] [--json]\n"
	defShowUsage     = "Usage: jig def show [--server <url>] [--json] <def>\n"
	defInvokeUsage   = `Usage: jig def invoke <def> --instructions <text> --repo <r> [--repo <r>]...
                      [--param k=v]... [--server <url>] [--json]

<def> is the definition's id or name. Run jig def --help for every flag.
`
	triggerCreateUsage = `Usage: jig trigger create --def <def> --name <name> --instructions <text> --repo <r>
                       [--kind <kind>] [--cron <expr>] [--timezone <IANA>] [...]

Run jig trigger --help for every flag.
`
	triggerListUsage = "Usage: jig trigger list [--server <url>] [--json]\n"
)

func triggerIDUsage(action string) string {
	return "Usage: jig trigger " + action + " [--server <url>] [--json] <id>\n"
}

// apiTimeout bounds one CLI request. Admission resolves refs (KTD9), which
// touches the network, so it is generous rather than snappy.
const apiTimeout = 60 * time.Second

// ---- jig def ---------------------------------------------------------------

func defCommand(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, defUsage)
		return exitUsage
	}
	switch args[0] {
	case "validate":
		return defValidate(args[1:], stdout, stderr)
	case "create":
		return defCreate(ctx, args[1:], stdout, stderr)
	case "update":
		return defUpdate(ctx, args[1:], stdout, stderr)
	case "list":
		return defList(ctx, args[1:], stdout, stderr)
	case "show":
		return defShow(ctx, args[1:], stdout, stderr)
	case "invoke":
		return defInvoke(ctx, args[1:], stdout, stderr)
	case "help", "-h", "--help":
		fmt.Fprint(stdout, defUsage)
		return exitAccepted
	default:
		fmt.Fprintf(stderr, "jig def: unknown command %q\n\n%s", args[0], defUsage)
		return exitUsage
	}
}

// validationReport is the --json shape of one validated file: an agent
// checking a directory of definitions reads verdicts, not prose.
type validationReport struct {
	Path   string `json:"path"`
	Ok     bool   `json:"ok"`
	Name   string `json:"name,omitempty"`
	Phases int    `json:"phases,omitempty"`
	Roles  int    `json:"roles,omitempty"`
	Error  string `json:"error,omitempty"`
}

// defValidate is the offline door: the same ParseDefinition the store calls
// at save time, with no server involved, so a definition can be checked in a
// pre-commit hook or a CI job.
func defValidate(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("jig def validate", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.Usage = func() { fmt.Fprint(stderr, defValidateUsage) }
	asJSON := flags.Bool("json", false, "emit the verdicts as JSON")
	if err := parseFlags(flags, args); err != nil {
		return flagExit(err)
	}
	if flags.NArg() == 0 {
		return usageError(stderr, "jig def validate", "a definition file is required", defValidateUsage)
	}
	reports := make([]validationReport, 0, flags.NArg())
	failed := false
	for _, path := range flags.Args() {
		report := validationReport{Path: path}
		source, err := os.ReadFile(path)
		if err == nil {
			var spec *protocol.DefinitionSpec
			spec, err = protocol.ParseDefinition(source)
			if err == nil {
				report.Ok = true
				report.Name = spec.Name
				report.Phases = len(spec.Phases)
				report.Roles = len(spec.Roster)
			}
		}
		if err != nil {
			report.Error = err.Error()
			failed = true
			if !*asJSON {
				fmt.Fprintf(stderr, "jig def validate: %s: %v\n", path, err)
			}
		} else if !*asJSON {
			fmt.Fprintf(stdout, "ok %s — definition %q, %d phase(s), %d role(s)\n",
				path, report.Name, report.Phases, report.Roles)
		}
		reports = append(reports, report)
	}
	if *asJSON {
		if code := emitJSON(stdout, stderr, reports); code != exitAccepted {
			return code
		}
	}
	if failed {
		return exitUsage
	}
	return exitAccepted
}

func defCreate(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags, server, asJSON := defFlags("jig def create", stderr, defCreateUsage)
	if err := parseFlags(flags, args); err != nil {
		return flagExit(err)
	}
	if flags.NArg() != 1 {
		return usageError(stderr, "jig def create", "exactly one definition file is required", defCreateUsage)
	}
	source, code := readDefinitionSource(flags.Arg(0), stderr)
	if code != exitAccepted {
		return code
	}
	var definition protocol.Definition
	if err := apiCall(ctx, *server, http.MethodPost, "/api/definitions",
		controlplane.DefinitionInput{Source: source}, &definition); err != nil {
		fmt.Fprintf(stderr, "jig def create: %v\n", err)
		return apiExit(err)
	}
	return reportDefinition(stdout, stderr, definition, *asJSON)
}

func defUpdate(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags, server, asJSON := defFlags("jig def update", stderr, defUpdateUsage)
	if err := parseFlags(flags, args); err != nil {
		return flagExit(err)
	}
	if flags.NArg() != 2 {
		return usageError(stderr, "jig def update", "a definition and a file are required", defUpdateUsage)
	}
	source, code := readDefinitionSource(flags.Arg(1), stderr)
	if code != exitAccepted {
		return code
	}
	definitionID, err := resolveDefinition(ctx, *server, flags.Arg(0))
	if err != nil {
		fmt.Fprintf(stderr, "jig def update: %v\n", err)
		return apiExit(err)
	}
	var definition protocol.Definition
	if err := apiCall(ctx, *server, http.MethodPut,
		"/api/definitions/"+url.PathEscape(definitionID),
		controlplane.DefinitionInput{Source: source}, &definition); err != nil {
		if isNotFound(err) {
			err = definitionNotFound(flags.Arg(0))
		}
		fmt.Fprintf(stderr, "jig def update: %v\n", err)
		return apiExit(err)
	}
	return reportDefinition(stdout, stderr, definition, *asJSON)
}

func defList(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags, server, asJSON := defFlags("jig def list", stderr, defListUsage)
	if err := parseFlags(flags, args); err != nil {
		return flagExit(err)
	}
	if flags.NArg() != 0 {
		return usageError(stderr, "jig def list",
			fmt.Sprintf("unexpected argument %q", flags.Arg(0)), defListUsage)
	}
	var definitions []protocol.Definition
	if err := apiCall(ctx, *server, http.MethodGet, "/api/definitions", nil, &definitions); err != nil {
		fmt.Fprintf(stderr, "jig def list: %v\n", err)
		return apiExit(err)
	}
	if *asJSON {
		return emitJSON(stdout, stderr, definitions)
	}
	if len(definitions) == 0 {
		fmt.Fprintln(stdout, "no definitions saved")
		return exitAccepted
	}
	for _, definition := range definitions {
		fmt.Fprintf(stdout, "%s  %-24s  generation %d  updated %s\n",
			definition.ID, definition.Name, definition.Generation,
			definition.UpdatedAt.Format(time.RFC3339))
	}
	return exitAccepted
}

func defShow(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags, server, asJSON := defFlags("jig def show", stderr, defShowUsage)
	if err := parseFlags(flags, args); err != nil {
		return flagExit(err)
	}
	if flags.NArg() != 1 {
		return usageError(stderr, "jig def show", "exactly one definition id or name is required", defShowUsage)
	}
	definitionID, err := resolveDefinition(ctx, *server, flags.Arg(0))
	if err != nil {
		fmt.Fprintf(stderr, "jig def show: %v\n", err)
		return apiExit(err)
	}
	var definition protocol.Definition
	if err := apiCall(ctx, *server, http.MethodGet,
		"/api/definitions/"+url.PathEscape(definitionID), nil, &definition); err != nil {
		if isNotFound(err) {
			err = definitionNotFound(flags.Arg(0))
		}
		fmt.Fprintf(stderr, "jig def show: %v\n", err)
		return apiExit(err)
	}
	if *asJSON {
		return emitJSON(stdout, stderr, definition)
	}
	fmt.Fprintf(stdout, "id: %s\nname: %s\ngeneration: %d\nupdated: %s\n\n%s\n",
		definition.ID, definition.Name, definition.Generation,
		definition.UpdatedAt.Format(time.RFC3339), definition.Source)
	return exitAccepted
}

// defInvoke admits a manual run. Instructions and repositories are kept as
// separate flags rather than one free-form prompt because the control plane
// keeps the trusted and untrusted prompt halves apart (KTD11): what the CLI
// sends as instructions is operator-authored by definition.
func defInvoke(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags, server, asJSON := defFlags("jig def invoke", stderr, defInvokeUsage)
	instructions := flags.String("instructions", "", "trusted prompt half")
	var repositories repeatedFlag
	flags.Var(&repositories, "repo", "target repository (repeatable)")
	var parameters repeatedFlag
	flags.Var(&parameters, "param", "extra run parameter k=v (repeatable)")
	if err := parseFlags(flags, args); err != nil {
		return flagExit(err)
	}
	switch {
	case flags.NArg() != 1:
		return usageError(stderr, "jig def invoke", "exactly one definition id or name is required", defInvokeUsage)
	case strings.TrimSpace(*instructions) == "":
		return usageError(stderr, "jig def invoke", "--instructions is required", defInvokeUsage)
	case len(repositories) == 0:
		return usageError(stderr, "jig def invoke", "at least one --repo is required", defInvokeUsage)
	}
	frozen := make(map[string]string, len(parameters))
	for _, entry := range parameters {
		name, value, found := strings.Cut(entry, "=")
		if !found || strings.TrimSpace(name) == "" {
			fmt.Fprintf(stderr, "jig def invoke: --param %q is not name=value\n", entry)
			return exitUsage
		}
		frozen[name] = value
	}
	targets := make([]controlplane.InvocationTarget, 0, len(repositories))
	for _, repository := range repositories {
		targets = append(targets, controlplane.InvocationTarget{Repository: cliRepository(repository)})
	}
	definitionID, err := resolveDefinition(ctx, *server, flags.Arg(0))
	if err != nil {
		fmt.Fprintf(stderr, "jig def invoke: %v\n", err)
		return apiExit(err)
	}
	var view controlplane.RunView
	if err := apiCall(ctx, *server, http.MethodPost, "/api/runs", controlplane.RunInvocation{
		DefinitionID: definitionID,
		Instructions: *instructions,
		Parameters:   frozen,
		Targets:      targets,
	}, &view); err != nil {
		if isNotFound(err) {
			err = definitionNotFound(flags.Arg(0))
		}
		fmt.Fprintf(stderr, "jig def invoke: %v\n", err)
		return apiExit(err)
	}
	if *asJSON {
		return emitJSON(stdout, stderr, view)
	}
	fmt.Fprintf(stdout, "run: %s (%s)\n", view.Run.ID, view.Run.State)
	for _, job := range view.Jobs {
		fmt.Fprintf(stdout, "  job %s  %s  base %s  %s\n",
			job.ID, job.Repository, job.BaseSHA, job.State)
	}
	fmt.Fprintf(stdout, "view: %s\n", runViewURL(*server, view.Run.ID))
	// Queued work waits for a worker; saying so here saves the operator
	// watching a run that nothing will claim.
	if liveWorkers(ctx, *server) == 0 {
		fmt.Fprintln(stdout, "note: no worker is online; start one with: jig worker")
	}
	return exitAccepted
}

func reportDefinition(stdout, stderr io.Writer, definition protocol.Definition, asJSON bool) int {
	if asJSON {
		return emitJSON(stdout, stderr, definition)
	}
	fmt.Fprintf(stdout, "%s  %s  generation %d\n",
		definition.ID, definition.Name, definition.Generation)
	return exitAccepted
}

// readDefinitionSource reads and validates a definition file before it is
// ever sent. The server validates too; failing here means the operator gets
// the same message with no round trip and no half-saved state.
func readDefinitionSource(path string, stderr io.Writer) (string, int) {
	source, err := os.ReadFile(path)
	if err != nil {
		fmt.Fprintf(stderr, "jig def: %v\n", err)
		return "", exitUsage
	}
	if _, err := protocol.ParseDefinition(source); err != nil {
		fmt.Fprintf(stderr, "jig def: invalid definition: %v\n", err)
		return "", exitUsage
	}
	return string(source), exitAccepted
}

// ---- jig trigger -----------------------------------------------------------

func triggerCommand(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, triggerUsage)
		return exitUsage
	}
	switch args[0] {
	case "create":
		return triggerCreate(ctx, args[1:], stdout, stderr)
	case "list":
		return triggerList(ctx, args[1:], stdout, stderr)
	case "show":
		return triggerShow(ctx, args[1:], stdout, stderr)
	case "enable":
		return triggerSetEnabled(ctx, args[1:], stdout, stderr, true)
	case "disable":
		return triggerSetEnabled(ctx, args[1:], stdout, stderr, false)
	case "help", "-h", "--help":
		fmt.Fprint(stdout, triggerUsage)
		return exitAccepted
	default:
		fmt.Fprintf(stderr, "jig trigger: unknown command %q\n\n%s", args[0], triggerUsage)
		return exitUsage
	}
}

func triggerCreate(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags, server, asJSON := defFlags("jig trigger create", stderr, triggerCreateUsage)
	definitionRef := flags.String("def", "", "definition id or name")
	name := flags.String("name", "", "trigger name")
	kind := flags.String("kind", controlplane.TriggerSchedule, "trigger kind")
	instructions := flags.String("instructions", "", "trusted prompt half")
	cron := flags.String("cron", "", "five-field cron expression")
	timezone := flags.String("timezone", "", "IANA timezone")
	state := flags.String("state", "", "github issue/pull-request state")
	includeDrafts := flags.Bool("include-drafts", false, "include draft pull requests")
	pollSeconds := flags.Int("poll-seconds", 0, "github poll interval in seconds")
	var repositories, labels, baseBranches repeatedFlag
	flags.Var(&repositories, "repo", "repository (repeatable)")
	flags.Var(&labels, "label", "required label (repeatable)")
	flags.Var(&baseBranches, "base-branch", "pull-request base branch (repeatable)")
	if err := parseFlags(flags, args); err != nil {
		return flagExit(err)
	}
	const command = "jig trigger create"
	switch {
	case flags.NArg() != 0:
		return usageError(stderr, command, fmt.Sprintf("unexpected argument %q", flags.Arg(0)), triggerCreateUsage)
	case *definitionRef == "":
		return usageError(stderr, command, "--def is required", triggerCreateUsage)
	case *name == "":
		return usageError(stderr, command, "--name is required", triggerCreateUsage)
	case len(repositories) == 0:
		return usageError(stderr, command, "at least one --repo is required", triggerCreateUsage)
	}
	for i := range repositories {
		repositories[i] = cliRepository(repositories[i])
	}

	config := controlplane.TriggerConfig{
		Instructions:        *instructions,
		Cron:                *cron,
		Timezone:            *timezone,
		State:               *state,
		RequiredLabels:      labels,
		BaseBranches:        baseBranches,
		IncludeDrafts:       *includeDrafts,
		PollIntervalSeconds: *pollSeconds,
	}
	if *kind == controlplane.TriggerSchedule {
		for _, repository := range repositories {
			config.Targets = append(config.Targets,
				controlplane.InvocationTarget{Repository: repository})
		}
	} else {
		// A GitHub trigger watches and runs against ONE repository — never a
		// URL read out of a ticket (R17). More than one is a usage error
		// here rather than a silently dropped argument.
		if len(repositories) != 1 {
			fmt.Fprintf(stderr,
				"jig trigger create: a %s trigger takes exactly one --repo, got %d\n",
				*kind, len(repositories))
			return exitUsage
		}
		config.Repository = repositories[0]
	}

	definitionID, err := resolveDefinition(ctx, *server, *definitionRef)
	if err != nil {
		fmt.Fprintf(stderr, "jig trigger create: %v\n", err)
		return apiExit(err)
	}
	var trigger controlplane.Trigger
	if err := apiCall(ctx, *server, http.MethodPost, "/api/triggers", controlplane.TriggerInput{
		Name:         *name,
		DefinitionID: definitionID,
		Kind:         *kind,
		Config:       config,
	}, &trigger); err != nil {
		if isNotFound(err) {
			err = definitionNotFound(*definitionRef)
		}
		fmt.Fprintf(stderr, "jig trigger create: %v\n", err)
		return apiExit(err)
	}
	if *asJSON {
		return emitJSON(stdout, stderr, trigger)
	}
	printTrigger(stdout, trigger)
	return exitAccepted
}

func triggerList(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags, server, asJSON := defFlags("jig trigger list", stderr, triggerListUsage)
	if err := parseFlags(flags, args); err != nil {
		return flagExit(err)
	}
	if flags.NArg() != 0 {
		return usageError(stderr, "jig trigger list",
			fmt.Sprintf("unexpected argument %q", flags.Arg(0)), triggerListUsage)
	}
	var triggers []controlplane.Trigger
	if err := apiCall(ctx, *server, http.MethodGet, "/api/triggers", nil, &triggers); err != nil {
		fmt.Fprintf(stderr, "jig trigger list: %v\n", err)
		return apiExit(err)
	}
	if *asJSON {
		return emitJSON(stdout, stderr, triggers)
	}
	if len(triggers) == 0 {
		fmt.Fprintln(stdout, "no triggers saved")
		return exitAccepted
	}
	for _, trigger := range triggers {
		printTrigger(stdout, trigger)
	}
	return exitAccepted
}

func triggerShow(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags, server, asJSON := defFlags("jig trigger show", stderr, triggerIDUsage("show"))
	if err := parseFlags(flags, args); err != nil {
		return flagExit(err)
	}
	if flags.NArg() != 1 {
		return usageError(stderr, "jig trigger show", "exactly one trigger id is required", triggerIDUsage("show"))
	}
	var trigger controlplane.Trigger
	if err := apiCall(ctx, *server, http.MethodGet,
		"/api/triggers/"+url.PathEscape(flags.Arg(0)), nil, &trigger); err != nil {
		fmt.Fprintf(stderr, "jig trigger show: %v\n", err)
		return apiExit(err)
	}
	if *asJSON {
		return emitJSON(stdout, stderr, trigger)
	}
	printTrigger(stdout, trigger)
	return exitAccepted
}

func triggerSetEnabled(
	ctx context.Context, args []string, stdout, stderr io.Writer, enabled bool,
) int {
	action := "disable"
	if enabled {
		action = "enable"
	}
	flags, server, asJSON := defFlags("jig trigger "+action, stderr, triggerIDUsage(action))
	if err := parseFlags(flags, args); err != nil {
		return flagExit(err)
	}
	if flags.NArg() != 1 {
		return usageError(stderr, "jig trigger "+action, "exactly one trigger id is required", triggerIDUsage(action))
	}
	var trigger controlplane.Trigger
	if err := apiCall(ctx, *server, http.MethodPost,
		"/api/triggers/"+url.PathEscape(flags.Arg(0))+"/"+action, nil, &trigger); err != nil {
		fmt.Fprintf(stderr, "jig trigger %s: %v\n", action, err)
		return apiExit(err)
	}
	if *asJSON {
		return emitJSON(stdout, stderr, trigger)
	}
	printTrigger(stdout, trigger)
	return exitAccepted
}

// printTrigger reports the counters and the diagnostic alongside the
// configuration: a schedule firing into a run that never finishes is visible
// only as a rising skip count (R13), and a GitHub trigger that cannot reach
// `gh` is visible only as its diagnostic code.
func printTrigger(stdout io.Writer, trigger controlplane.Trigger) {
	status := "disabled"
	if trigger.Enabled {
		status = "enabled"
	}
	fmt.Fprintf(stdout, "%s  %-20s  %-20s  %s\n",
		trigger.ID, trigger.Name, trigger.Kind, status)
	if trigger.Kind == controlplane.TriggerSchedule {
		fmt.Fprintf(stdout, "  cron: %s (%s)\n", trigger.Config.Cron, trigger.Config.Timezone)
		for _, target := range trigger.Config.Targets {
			fmt.Fprintf(stdout, "  target: %s\n", target.Repository)
		}
	} else {
		fmt.Fprintf(stdout, "  repository: %s  state: %s  poll: %ds\n",
			trigger.Config.Repository, trigger.Config.State, trigger.Config.PollIntervalSeconds)
	}
	fmt.Fprintf(stdout, "  admitted: %d  skipped: %d\n",
		trigger.AdmittedCount, trigger.SkippedCount)
	if trigger.NextDueAt != nil {
		fmt.Fprintf(stdout, "  next due: %s\n", trigger.NextDueAt.Format(time.RFC3339))
	}
	if trigger.NextPollAt != nil {
		fmt.Fprintf(stdout, "  next poll: %s\n", trigger.NextPollAt.Format(time.RFC3339))
	}
	if trigger.Diagnostic != "" {
		fmt.Fprintf(stdout, "  diagnostic: %s: %s\n", trigger.DiagnosticCode, trigger.Diagnostic)
	}
}

// ---- shared plumbing -------------------------------------------------------

// repeatedFlag collects a repeatable string flag in argument order.
type repeatedFlag []string

func (f *repeatedFlag) String() string { return strings.Join(*f, ",") }

func (f *repeatedFlag) Set(value string) error {
	*f = append(*f, value)
	return nil
}

// defFlags builds the flag set every def/trigger subcommand shares.
func defFlags(name string, stderr io.Writer, usage string) (*flag.FlagSet, *string, *bool) {
	flags := flag.NewFlagSet(name, flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.Usage = func() { fmt.Fprint(stderr, usage) }
	server := serverFlag(flags)
	asJSON := flags.Bool("json", false, "emit the API object as JSON")
	return flags, server, asJSON
}

func emitJSON(stdout, stderr io.Writer, value any) int {
	body, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		fmt.Fprintf(stderr, "jig: encode response: %v\n", err)
		return exitInfraFailed
	}
	fmt.Fprintf(stdout, "%s\n", body)
	return exitAccepted
}

// apiCall performs one control-plane request. It sends NO Origin header: the
// browser-origin fence exists to stop foreign web pages from driving a
// loopback control plane through the operator's browser, and a CLI is not a
// browser (R20).
func apiCall(ctx context.Context, server, method, path string, body, into any) error {
	base, err := url.Parse(strings.TrimSpace(server))
	if err != nil || base.Scheme == "" || base.Host == "" {
		return fmt.Errorf("server URL %q is not an http(s) address", server)
	}
	var payload io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encode request: %w", err)
		}
		payload = bytes.NewReader(encoded)
	}
	requestCtx, cancel := context.WithTimeout(ctx, apiTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(
		requestCtx, method, strings.TrimRight(base.String(), "/")+path, payload)
	if err != nil {
		return err
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer response.Body.Close()
	answer, err := io.ReadAll(io.LimitReader(response.Body, 8<<20))
	if err != nil {
		return fmt.Errorf("read response: %w", err)
	}
	if response.StatusCode >= 400 {
		return apiRejection(response.StatusCode, answer)
	}
	if into == nil {
		return nil
	}
	if err := json.Unmarshal(answer, into); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	return nil
}

// apiRejection turns the control plane's error document back into its
// actionable form: the store's own diagnostic code and message, which are
// already the name of the fault.
func apiRejection(status int, body []byte) error {
	var document struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &document); err == nil && document.Error.Message != "" {
		return &apiError{Status: status, Code: document.Error.Code, Message: document.Error.Message}
	}
	return &apiError{Status: status, Message: fmt.Sprintf("control plane returned %d: %s",
		status, strings.TrimSpace(string(body)))}
}
