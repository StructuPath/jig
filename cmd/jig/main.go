// jig is a local-first software factory: one binary that runs repeatable,
// phased coding-agent workflows against Git repositories.
//
// One binary, no Node (R18): run is the serverless direct harness (U11),
// serve is the control plane with its embedded UI (U2/U6/U8), worker is the
// execution host (U3/U4/U7), def and trigger are the operator's surface over
// definitions and admission (U5/U6), runs and job are the operator's loop
// over what was admitted, and report reads the factory's quality over a
// window (plan 2026-09-28-001, U3).
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"runtime/debug"
	"strings"
	"syscall"

	"github.com/StructuPath/JIG-Software-Factory/internal/protocol"
)

// version is jig's release identity, stamped at build time with
// -ldflags "-X main.version=<tag>" (scripts/release.sh). Every other build
// says "dev", which is the truth: a binary that cannot name its release is
// not one.
var version = "dev"

// command is one top-level `jig <name>`. The table below is the CLI's one
// vocabulary: dispatch, the usage text, "did you mean", and `jig completion`
// all read it, so a command cannot exist in one and be missing from another.
type command struct {
	name    string
	summary string
	run     func(ctx context.Context, args []string, stdout, stderr io.Writer) int
	// verbs is the family's subcommand table, nil for a command that takes
	// none.
	verbs []subcommand
}

// commands is initialized in init: completion reads the table it is part of.
var commands []command

func init() {
	commands = []command{
		{name: "start", summary: "Start the UI and a worker together and open your browser", run: startCommand},
		{name: "serve", summary: "Start the control plane (loopback HTTP + embedded UI)", run: serveCommand},
		{name: "worker", summary: "Start the single implicit worker", run: workerCommand, verbs: workerVerbs},
		{name: "run", summary: "Run a definition directly against a local repository", run: runCommand},
		{name: "def", summary: "Validate, manage, and invoke job definitions", run: defCommand, verbs: defVerbs},
		{name: "trigger", summary: "Manage admission triggers (cron schedules, GitHub polling)", run: triggerCommand, verbs: triggerVerbs},
		{name: "runs", summary: "List, show, and re-admit runs", run: runsCommand, verbs: runsVerbs},
		{name: "job", summary: "Show, cancel, retry, or release one job", run: jobCommand, verbs: jobVerbs},
		{name: "report", summary: "Summarize the factory's quality over a time window", run: reportCommand},
		{name: "completion", summary: "Print a shell completion script (zsh, bash, fish)", run: completionCommand},
		{name: "version", summary: "Print the release identity", run: versionCommand},
	}
}

func usageText() string {
	var text strings.Builder
	text.WriteString("jig — a local-first software factory\n\nUsage:\n")
	for _, entry := range commands {
		fmt.Fprintf(&text, "  jig %-11s%s\n", entry.name, entry.summary)
	}
	text.WriteString("\nRun any subcommand with --help for its flags and exit codes.\n")
	return text.String()
}

func main() {
	// SIGINT/SIGTERM cancel the command's context rather than killing the
	// process where it stands: an interrupted run has work to finish — stop
	// the agent's process group, destroy the ephemeral HOME, and leave its
	// attempt terminal in the store. A second signal restores the default
	// disposition, so an operator who insists can always kill jig outright.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	os.Exit(run(ctx, os.Args[1:]))
}

func run(ctx context.Context, args []string) int {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, usageText())
		return 2
	}
	switch args[0] {
	case "--version":
		return versionCommand(ctx, nil, os.Stdout, os.Stderr)
	case "help", "-h", "--help":
		fmt.Fprint(os.Stdout, usageText())
		return 0
	}
	for _, entry := range commands {
		if entry.name == args[0] {
			return entry.run(ctx, args[1:], os.Stdout, os.Stderr)
		}
	}
	if suggestion := closestCommand(args[0]); suggestion != "" {
		fmt.Fprintf(os.Stderr, "jig: unknown command %q — did you mean %q?\n\n%s",
			args[0], suggestion, usageText())
		return 2
	}
	fmt.Fprintf(os.Stderr, "jig: unknown command %q\n\n%s", args[0], usageText())
	return 2
}

func versionCommand(_ context.Context, _ []string, stdout, _ io.Writer) int {
	fmt.Fprintln(stdout, versionLine())
	return exitAccepted
}

// closestCommand is the dispatch vocabulary's nearest spelling of a
// mistyped command, for "did you mean", or "".
func closestCommand(word string) string {
	names := []string{"help"}
	for _, entry := range commands {
		names = append(names, entry.name)
	}
	return protocol.ClosestMatch(word, names)
}

// versionLine names the release and, when the Go toolchain stamped it, the
// commit the binary was built from: a "dev" build is still traceable to a
// revision, and "+dirty" says it was built from uncommitted changes.
func versionLine() string {
	line := "jig " + version
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return line
	}
	var revision, built string
	dirty := false
	for _, setting := range info.Settings {
		switch setting.Key {
		case "vcs.revision":
			revision = setting.Value
		case "vcs.time":
			built = setting.Value
		case "vcs.modified":
			dirty = setting.Value == "true"
		}
	}
	if revision == "" {
		return line
	}
	if len(revision) > 12 {
		revision = revision[:12]
	}
	if dirty {
		revision += "+dirty"
	}
	line += " (" + revision
	if built != "" {
		line += ", " + built
	}
	return line + ")"
}
