// run.go — the `jig run` direct harness (U11): the serverless day-one loop
// and the Milestone 1 exit-gate surface. It validates the definition BEFORE
// anything else can spawn a subprocess, freezes an in-process run against
// the local repository pinned at its current HEAD, executes the chain via
// the engine with the embedded store, and prints the trace path and
// outcome.
//
// Exit codes: 0 accepted, 1 failed (or infrastructure error), 2 usage or
// validation error.
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
	"github.com/StructuPath/jig/internal/runtime/claudecode"
)

const runUsage = `Usage: jig run --def <file> [--data <dir>] [--no-seed-auth] <repo-path> "<prompt>"

Run a definition directly against a local repository: validate it, freeze
an in-process run pinned at the repository's current HEAD, execute the
phase chain with the embedded store, and print the trace path and outcome.
No standing server, no publish.

Flags (before the positional arguments):
  --def <file>     definition YAML file (required)
  --data <dir>     data directory for the store, traces, and scratch
                   (default ~/.jig)
  --no-seed-auth   do not seed runtime auth material into the ephemeral HOME

Exit codes: 0 accepted, 1 failed, 2 usage or validation error.
`

// scriptedRuntimeEnv is the hidden test hook (U11): when set, it names a
// JSON script file and `jig run` substitutes the enginetest scripted fake
// for the real Claude Code adapter. Tests only; never documented in usage.
const scriptedRuntimeEnv = "JIG_SCRIPTED_RUNTIME"

func runCommand(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("jig run", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.Usage = func() { fmt.Fprint(stderr, runUsage) }
	defPath := flags.String("def", "", "definition YAML file (required)")
	defaultData := ""
	if home, err := os.UserHomeDir(); err == nil {
		defaultData = filepath.Join(home, ".jig")
	}
	dataDir := flags.String("data", defaultData, "data directory")
	noSeedAuth := flags.Bool("no-seed-auth", false, "do not seed runtime auth material")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if *defPath == "" || *dataDir == "" || flags.NArg() != 2 {
		flags.Usage()
		return 2
	}

	source, err := os.ReadFile(*defPath)
	if err != nil {
		fmt.Fprintf(stderr, "jig run: %v\n", err)
		return 2
	}
	// Validation comes FIRST — before the repository is touched, before the
	// runtime is constructed, before any subprocess can spawn (U11). The
	// error names the offending element; the operator fixes the file from
	// the message alone.
	if _, err := protocol.ParseDefinition(source); err != nil {
		fmt.Fprintf(stderr, "jig run: invalid definition: %v\n", err)
		return 2
	}

	repoPath, err := filepath.Abs(flags.Arg(0))
	if err != nil {
		fmt.Fprintf(stderr, "jig run: %v\n", err)
		return 2
	}
	prompt := flags.Arg(1)
	headSHA, err := repoHead(repoPath)
	if err != nil {
		fmt.Fprintf(stderr, "jig run: %v\n", err)
		return 2
	}

	ctx := context.Background()
	agentRuntime, capability, scripted, err := buildRuntime(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "jig run: %v\n", err)
		return 1
	}
	// Auth seeding is a real-CLI concern: the scripted fake needs nothing,
	// and --no-seed-auth is the escape hatch for a CLI already configured
	// through its environment (e.g. ANTHROPIC_API_KEY on the role's env
	// allowlist).
	var seeder engine.HomeSeeder
	if !scripted && !*noSeedAuth {
		seeder = seedClaudeAuth
	}

	result, err := controlplane.DirectRun(ctx, controlplane.DirectRunConfig{
		DataDir:    *dataDir,
		Source:     source,
		RepoPath:   repoPath,
		HeadSHA:    headSHA,
		Parameters: map[string]string{"prompt": prompt},
		Capability: capability,
		Execute:    engineExecutor(*dataDir, repoPath, headSHA, agentRuntime, capability, seeder),
	})
	if err != nil {
		fmt.Fprintf(stderr, "jig run: %v\n", err)
		return 1
	}

	printOutcome(stdout, result)
	if result.Attempt.State == protocol.AttemptAcceptedUnpublished {
		return 0
	}
	return 1
}

// engineExecutor wires the phase engine into DirectRun's executor seam:
// events dual-write to the attempt-local JSONL trace and the store's
// events table (KTD8), and lease freshening rides the engine's
// between-phase hook. The control plane stays engine-free (KTD1).
func engineExecutor(
	dataDir, repoPath, headSHA string,
	agentRuntime runtime.Runtime, capability protocol.RuntimeCapability, seeder engine.HomeSeeder,
) func(ctx context.Context, execution controlplane.DirectExecution) controlplane.DirectOutcome {
	return func(ctx context.Context, execution controlplane.DirectExecution) controlplane.DirectOutcome {
		infraFail := func(err error) controlplane.DirectOutcome {
			return controlplane.DirectOutcome{State: protocol.AttemptFailed, Error: err.Error()}
		}
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
			ScratchRoot: filepath.Join(dataDir, "scratch"),
			SeedHome:    seeder,
			BaseEnv:     os.Environ(),
		})
		if err != nil {
			return infraFail(err)
		}
		outcome := runner.Execute(ctx, engine.Attempt{
			Claim:        execution.Claim,
			WorktreePath: repoPath,
			BaseSHA:      headSHA,
			FreshenLease: execution.FreshenLease,
		})
		return controlplane.DirectOutcome{
			State:  outcome.State,
			Result: outcome.Result,
			Error:  outcome.Error,
		}
	}
}

// printOutcome reports the run: identifiers, trace path, verdict, and the
// acceptance evidence from the attempt's result payload.
func printOutcome(stdout io.Writer, result controlplane.DirectRunResult) {
	fmt.Fprintf(stdout, "run: %s\njob: %s\nattempt: %s\ntrace: %s\n",
		result.RunID, result.Job.ID, result.Attempt.ID, result.TracePath)

	var summary struct {
		Acceptance *struct {
			Passed bool                 `json:"passed"`
			Checks []protocol.GateCheck `json:"checks"`
		} `json:"acceptance"`
		ChangedPaths []string `json:"changed_paths"`
		Publish      string   `json:"publish"`
	}
	// A truncated or absent result payload only degrades the printout,
	// never the verdict — the attempt state is the verdict.
	_ = json.Unmarshal([]byte(result.Attempt.Result), &summary)

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

// buildRuntime constructs the agent runtime: the scripted fake when the
// test hook is set, the real Claude Code adapter otherwise. The returned
// flag reports which, so auth seeding can stay a real-CLI concern.
func buildRuntime(ctx context.Context) (runtime.Runtime, protocol.RuntimeCapability, bool, error) {
	if scriptPath := os.Getenv(scriptedRuntimeEnv); scriptPath != "" {
		fake, err := loadScriptedRuntime(scriptPath)
		if err != nil {
			return nil, protocol.RuntimeCapability{}, false, err
		}
		capability, err := fake.Probe(ctx)
		return fake, capability, true, err
	}
	adapter, err := claudecode.New()
	if err != nil {
		return nil, protocol.RuntimeCapability{}, false, err
	}
	capability, err := adapter.Probe(ctx)
	if err != nil {
		return nil, protocol.RuntimeCapability{}, false, err
	}
	return adapter, capability, false, nil
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
			Files map[string]string `json:"files"`
			Text  string            `json:"text"`
			Hang  bool              `json:"hang"`
			Crash bool              `json:"crash"`
		} `json:"steps"`
	}
	if err := json.Unmarshal(body, &script); err != nil {
		return nil, fmt.Errorf("parse scripted runtime %s: %w", path, err)
	}
	steps := make([]enginetest.Step, 0, len(script.Steps))
	for _, step := range script.Steps {
		steps = append(steps, enginetest.Step{
			Files: step.Files,
			Text:  step.Text,
			Hang:  step.Hang,
			Crash: step.Crash,
		})
	}
	fake := enginetest.New(steps...)
	if script.CanResume != nil {
		fake.CanResume = *script.CanResume
	}
	return fake, nil
}

// seedClaudeAuth is the default darwin-aware HomeSeeder for real-CLI runs
// (KTD11): the minimum auth material Claude Code needs inside the
// ephemeral HOME. The credentials file when it exists; otherwise the macOS
// keychain secret extracted into the ephemeral HOME's credentials file —
// the CLI's keychain lookup does not survive a HOME change. Onboarding
// state rides along so print mode skips first-run prompts. (Same seeding
// the U4 live smoke proved out.)
func seedClaudeAuth(home, _ string, _ protocol.RoleSpec) error {
	real, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o700); err != nil {
		return err
	}
	credentials, readErr := os.ReadFile(filepath.Join(real, ".claude", ".credentials.json"))
	if readErr != nil {
		extracted, keychainErr := exec.Command("security",
			"find-generic-password", "-s", "Claude Code-credentials", "-w").Output()
		if keychainErr != nil {
			return fmt.Errorf(
				"seed claude auth: no credentials file and no keychain item (%v); "+
					"use --no-seed-auth if the CLI authenticates through its environment", keychainErr)
		}
		credentials = extracted
	}
	if err := os.WriteFile(
		filepath.Join(home, ".claude", ".credentials.json"), credentials, 0o600); err != nil {
		return err
	}
	if onboarding, err := os.ReadFile(filepath.Join(real, ".claude.json")); err == nil {
		if err := os.WriteFile(filepath.Join(home, ".claude.json"), onboarding, 0o600); err != nil {
			return err
		}
	}
	return nil
}
