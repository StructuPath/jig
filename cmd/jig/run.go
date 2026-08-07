// run.go — the `jig run` direct harness (U11): the serverless day-one loop
// and the Milestone 1 exit-gate surface. It validates the definition BEFORE
// anything else can spawn a subprocess, freezes an in-process run against
// the local repository pinned at its current HEAD, executes the chain via
// the engine with the embedded store, and prints the trace path and
// outcome.
//
// Exit codes (see runUsage): 0 accepted, 1 infrastructure failure, 2 usage
// or validation error, 3 the attempt was rejected. A rejected attempt is a
// verdict the harness delivered correctly; conflating it with an
// infrastructure failure makes `jig run` unscriptable.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/StructuPath/jig/internal/controlplane"
	"github.com/StructuPath/jig/internal/engine"
	"github.com/StructuPath/jig/internal/engine/enginetest"
	"github.com/StructuPath/jig/internal/protocol"
	"github.com/StructuPath/jig/internal/runtime"
)

const runUsage = `Usage: jig run --def <file> [--data <dir>] [--runtime <name>] [--json] [--no-seed-auth] <repo-path> "<prompt>"

Run a definition directly against a local repository: validate it, freeze
an in-process run pinned at the repository's current HEAD, execute the
phase chain with the embedded store, and print the trace path and outcome.
No standing server, no publish.

Flags (before the positional arguments):
  --def <file>     definition YAML file (required)
  --data <dir>     data directory for the store, traces, and scratch
                   (default ~/.jig)
  --runtime <name> agent CLI to run the roster on: claude-code (default)
                   or codex
  --json           emit the structured result as one JSON object on stdout
                   instead of the human report
  --no-seed-auth   do not seed runtime auth material into the ephemeral HOME

Exit codes:
  0  the attempt was accepted
  1  infrastructure failure — the harness could not deliver a verdict
  2  usage or definition-validation error
  3  the attempt was rejected (failed, or cancelled by SIGINT/SIGTERM)
`

// scriptedRuntimeEnv is the hidden test hook (U11): when set, it names a
// JSON script file and `jig run` substitutes the enginetest scripted fake
// for the real Claude Code adapter. Tests only; never documented in usage.
const scriptedRuntimeEnv = "JIG_SCRIPTED_RUNTIME"

// Exit codes. Rejected and infrastructure failure are deliberately distinct:
// a caller must be able to tell "jig worked and the answer was no" from "jig
// did not work".
const (
	exitAccepted    = 0
	exitInfraFailed = 1
	exitUsage       = 2
	exitRejected    = 3
)

func runCommand(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("jig run", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.Usage = func() { fmt.Fprint(stderr, runUsage) }
	defPath := flags.String("def", "", "definition YAML file (required)")
	defaultData := ""
	if home, err := os.UserHomeDir(); err == nil {
		defaultData = filepath.Join(home, ".jig")
	}
	dataDir := flags.String("data", defaultData, "data directory")
	runtimeName := flags.String("runtime", runtimeClaudeCode, "agent CLI runtime")
	asJSON := flags.Bool("json", false, "emit the structured result as JSON")
	noSeedAuth := flags.Bool("no-seed-auth", false, "do not seed runtime auth material")
	if err := flags.Parse(args); err != nil {
		return exitUsage
	}
	if *defPath == "" || *dataDir == "" || flags.NArg() != 2 {
		flags.Usage()
		return exitUsage
	}

	source, err := os.ReadFile(*defPath)
	if err != nil {
		fmt.Fprintf(stderr, "jig run: %v\n", err)
		return exitUsage
	}
	// Validation comes FIRST — before the repository is touched, before the
	// runtime is constructed, before any subprocess can spawn (U11). The
	// error names the offending element; the operator fixes the file from
	// the message alone.
	if _, err := protocol.ParseDefinition(source); err != nil {
		fmt.Fprintf(stderr, "jig run: invalid definition: %v\n", err)
		return exitUsage
	}

	repoPath, err := filepath.Abs(flags.Arg(0))
	if err != nil {
		fmt.Fprintf(stderr, "jig run: %v\n", err)
		return exitUsage
	}
	prompt := flags.Arg(1)
	headSHA, err := repoHead(repoPath)
	if err != nil {
		fmt.Fprintf(stderr, "jig run: %v\n", err)
		return exitUsage
	}

	if ctx == nil {
		ctx = context.Background()
	}
	// Auth seeding is a real-CLI concern: the scripted fake needs nothing,
	// and --no-seed-auth is the escape hatch for a CLI already configured
	// through its environment (e.g. ANTHROPIC_API_KEY on the role's env
	// allowlist). Both runtimes seed (KTD11) — see runtime.go.
	selected, err := selectRuntime(ctx, *runtimeName, !*noSeedAuth)
	if err != nil {
		fmt.Fprintf(stderr, "jig run: %v\n", err)
		return exitInfraFailed
	}

	result, err := controlplane.DirectRun(ctx, controlplane.DirectRunConfig{
		DataDir:    *dataDir,
		Source:     source,
		RepoPath:   repoPath,
		HeadSHA:    headSHA,
		Parameters: map[string]string{"prompt": prompt},
		Capability: selected.Capability,
		Execute: engineExecutor(ctx, *dataDir, repoPath, headSHA,
			selected.Runtime, selected.Capability, selected.Seeder),
	})
	if err != nil {
		fmt.Fprintf(stderr, "jig run: %v\n", err)
		return exitInfraFailed
	}

	if *asJSON {
		if err := emitJSONOutcome(stdout, result); err != nil {
			fmt.Fprintf(stderr, "jig run: %v\n", err)
			return exitInfraFailed
		}
	} else {
		printOutcome(stdout, result)
	}
	if result.Attempt.State == protocol.AttemptAcceptedUnpublished {
		return exitAccepted
	}
	return exitRejected
}

// engineExecutor wires the phase engine into DirectRun's executor seam:
// events dual-write to the attempt-local JSONL trace and the store's
// events table (KTD8), and lease freshening rides the engine's
// between-phase hook. The control plane stays engine-free (KTD1).
//
// signalCtx is the interrupt-aware context from main. It becomes the
// attempt's cancellation channel, which is what turns Ctrl-C into an orderly
// stop: the engine kills the agent's process group, unwinds, and returns
// `cancelled` — rather than leaving a bypassPermissions agent editing the
// operator's repository with nobody left to enforce the write boundary.
func engineExecutor(
	signalCtx context.Context, dataDir, repoPath, headSHA string,
	agentRuntime runtime.Runtime, capability protocol.RuntimeCapability, seeder engine.HomeSeeder,
) func(ctx context.Context, execution controlplane.DirectExecution) controlplane.DirectOutcome {
	scratchRoot := filepath.Join(dataDir, "scratch")
	return func(ctx context.Context, execution controlplane.DirectExecution) controlplane.DirectOutcome {
		infraFail := func(err error) controlplane.DirectOutcome {
			return controlplane.DirectOutcome{State: protocol.AttemptFailed, Error: err.Error()}
		}
		// The ephemeral HOME holds seeded credentials (KTD11): it dies with the
		// attempt on every exit path, including cancellation, and this defer is
		// the belt to the engine's braces.
		defer func() {
			if err := os.RemoveAll(filepath.Join(scratchRoot, execution.Claim.Attempt.ID)); err != nil {
				fmt.Fprintf(os.Stderr, "jig run: destroy attempt scratch: %v\n", err)
			}
		}()
		jsonl, err := engine.NewJSONLSink(execution.TracePath)
		if err != nil {
			return infraFail(err)
		}
		defer jsonl.Close()
		sink := engine.SinkFunc(func(event protocol.Event) error {
			return errors.Join(jsonl.Emit(event), execution.Persist(event))
		})
		runner, err := engine.New(engine.Config{
			Runtime:     agentRuntime,
			Capability:  capability,
			Sink:        sink,
			ScratchRoot: scratchRoot,
			SeedHome:    seeder,
			BaseEnv:     os.Environ(),
		})
		if err != nil {
			return infraFail(err)
		}
		// Cancellation reaches the engine through the Cancelled channel, never
		// by cancelling its context: the engine still has git to run while it
		// unwinds (rollback, boundary enforcement), and a context cancelled out
		// from under those turns an orderly stop into a torn-off phase failure.
		outcome := runner.Execute(context.WithoutCancel(ctx), engine.Attempt{
			Claim:        execution.Claim,
			WorktreePath: repoPath,
			BaseSHA:      headSHA,
			Cancelled:    signalCtx.Done(),
			FreshenLease: execution.FreshenLease,
		})
		return controlplane.DirectOutcome{
			State:  outcome.State,
			Result: outcome.Result,
			Error:  outcome.Error,
		}
	}
}

// resultSummary is the shape the engine writes into Attempt.Result: the
// evidence both output modes report from.
type resultSummary struct {
	Acceptance *struct {
		Passed bool                 `json:"passed"`
		Checks []protocol.GateCheck `json:"checks"`
	} `json:"acceptance"`
	ChangedPaths []string `json:"changed_paths"`
	Publish      string   `json:"publish"`
}

// runReport is the --json contract: one object, one line, every field a
// caller needs to decide what happened without scraping prose.
type runReport struct {
	RunID     string `json:"run_id"`
	JobID     string `json:"job_id"`
	AttemptID string `json:"attempt_id"`
	// State is the attempt's terminal state; Outcome is the verdict in the
	// harness's own vocabulary ("accepted" or "rejected").
	State        string            `json:"state"`
	Outcome      string            `json:"outcome"`
	ExitCode     int               `json:"exit_code"`
	TracePath    string            `json:"trace_path"`
	Publish      string            `json:"publish"`
	Error        string            `json:"error,omitempty"`
	Acceptance   *acceptanceReport `json:"acceptance,omitempty"`
	ChangedPaths []string          `json:"changed_paths"`
}

type acceptanceReport struct {
	Passed bool                 `json:"passed"`
	Checks []protocol.GateCheck `json:"checks"`
}

// emitJSONOutcome writes the machine-readable result: exactly one JSON
// object on stdout, so `jig run --json | jq` is the scripting surface and
// the prose report stays a human convenience.
func emitJSONOutcome(stdout io.Writer, result controlplane.DirectRunResult) error {
	summary := parseSummary(result)
	report := runReport{
		RunID:        result.RunID,
		JobID:        result.Job.ID,
		AttemptID:    result.Attempt.ID,
		State:        result.Attempt.State,
		Outcome:      "rejected",
		ExitCode:     exitRejected,
		TracePath:    result.TracePath,
		Publish:      summary.Publish,
		Error:        result.Attempt.Error,
		ChangedPaths: summary.ChangedPaths,
	}
	if report.ChangedPaths == nil {
		report.ChangedPaths = []string{}
	}
	if report.Publish == "" {
		report.Publish = "not_attempted"
	}
	if result.Attempt.State == protocol.AttemptAcceptedUnpublished {
		report.Outcome = "accepted"
		report.ExitCode = exitAccepted
	}
	if summary.Acceptance != nil {
		report.Acceptance = &acceptanceReport{
			Passed: summary.Acceptance.Passed,
			Checks: summary.Acceptance.Checks,
		}
	}
	body, err := json.Marshal(report)
	if err != nil {
		return fmt.Errorf("encode run report: %w", err)
	}
	_, err = fmt.Fprintf(stdout, "%s\n", body)
	return err
}

// parseSummary reads the attempt's result payload. A truncated or absent
// payload only degrades the report, never the verdict — the attempt state is
// the verdict.
func parseSummary(result controlplane.DirectRunResult) resultSummary {
	var summary resultSummary
	_ = json.Unmarshal([]byte(result.Attempt.Result), &summary)
	return summary
}

// printOutcome reports the run: identifiers, trace path, verdict, and the
// acceptance evidence from the attempt's result payload.
func printOutcome(stdout io.Writer, result controlplane.DirectRunResult) {
	fmt.Fprintf(stdout, "run: %s\njob: %s\nattempt: %s\ntrace: %s\n",
		result.RunID, result.Job.ID, result.Attempt.ID, result.TracePath)

	summary := parseSummary(result)

	if result.Attempt.State == protocol.AttemptAcceptedUnpublished {
		marker := summary.Publish
		if marker == "" {
			marker = "not_attempted"
		}
		fmt.Fprintf(stdout, "outcome: accepted (publish: %s)\n", marker)
	} else {
		fmt.Fprintf(stdout, "outcome: %s — %s\n", result.Attempt.State, result.Attempt.Error)
	}
	if summary.Acceptance != nil {
		fmt.Fprintln(stdout, "acceptance:")
		for _, check := range summary.Acceptance.Checks {
			verdict := "ok  "
			if !check.Ok {
				verdict = "FAIL"
			}
			fmt.Fprintf(stdout, "  %s %s — %s\n", verdict, check.Item, check.Note)
		}
	}
	if len(summary.ChangedPaths) > 0 {
		fmt.Fprintf(stdout, "changed paths: %s\n", strings.Join(summary.ChangedPaths, ", "))
	}
}

// repoHead pins the repository at its current HEAD (KTD9's direct-run
// half). A path that is not a git repository fails here, as usage.
func repoHead(repoPath string) (string, error) {
	command := exec.Command("git", "rev-parse", "HEAD")
	command.Dir = repoPath
	output, err := command.Output()
	if err != nil {
		detail := err.Error()
		var exitError *exec.ExitError
		if errors.As(err, &exitError) && len(exitError.Stderr) > 0 {
			detail = strings.TrimSpace(string(exitError.Stderr))
		}
		return "", fmt.Errorf("resolve HEAD of %s: %s", repoPath, detail)
	}
	sha := strings.TrimSpace(string(output))
	if sha == "" {
		return "", fmt.Errorf("resolve HEAD of %s: empty answer", repoPath)
	}
	return sha, nil
}

// loadScriptedRuntime reads the test hook's JSON script: an ordered step
// list in the enginetest shape ("write these files, then emit this text").
func loadScriptedRuntime(path string) (*enginetest.Runtime, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read scripted runtime %s: %w", path, err)
	}
	var script struct {
		CanResume *bool `json:"can_resume"`
		Steps     []struct {
			Files    map[string]string `json:"files"`
			Text     string            `json:"text"`
			Hang     bool              `json:"hang"`
			Crash    bool              `json:"crash"`
			IsError  bool              `json:"is_error"`
			ExitCode int               `json:"exit_code"`
			// Events are streamed before the result, so a scripted run can
			// exercise the tool-call rows the trace views fold into spans.
			Events []struct {
				Type    string          `json:"type"`
				Name    string          `json:"name"`
				Payload json.RawMessage `json:"payload"`
			} `json:"events"`
		} `json:"steps"`
	}
	if err := json.Unmarshal(body, &script); err != nil {
		return nil, fmt.Errorf("parse scripted runtime %s: %w", path, err)
	}
	steps := make([]enginetest.Step, 0, len(script.Steps))
	for _, step := range script.Steps {
		events := make([]runtime.Event, 0, len(step.Events))
		for _, event := range step.Events {
			events = append(events, runtime.Event{
				Kind:    event.Type,
				Name:    event.Name,
				Payload: event.Payload,
			})
		}
		steps = append(steps, enginetest.Step{
			Files:    step.Files,
			Events:   events,
			Text:     step.Text,
			Hang:     step.Hang,
			Crash:    step.Crash,
			IsError:  step.IsError,
			ExitCode: step.ExitCode,
		})
	}
	fake := enginetest.New(steps...)
	if script.CanResume != nil {
		fake.CanResume = *script.CanResume
	}
	return fake, nil
}
