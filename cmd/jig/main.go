// jig is a local-first software factory: one binary that runs repeatable,
// phased coding-agent workflows against Git repositories.
//
// Five subcommands, one binary, no Node (R18): run is the serverless direct
// harness (U11), serve is the control plane with its embedded UI (U2/U6/U8),
// worker is the execution host (U3/U4/U7), and def and trigger are the
// operator's surface over definitions and admission (U5/U6).
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
)

// version is jig's release identity, stamped at build time with
// -ldflags "-X main.version=<tag>" (scripts/release.sh). Every other build
// says "dev", which is the truth: a binary that cannot name its release is
// not one.
var version = "dev"

const usage = `jig — a local-first software factory

Usage:
  jig serve    Start the control plane (loopback HTTP + embedded UI)
  jig worker   Start the single implicit worker
  jig run      Run a definition directly against a local repository
  jig def      Validate, manage, and invoke job definitions
  jig trigger  Manage admission triggers (cron schedules, GitHub polling)
  jig version  Print the release identity

Run any subcommand with --help for its flags and exit codes.
`

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
		fmt.Fprint(os.Stderr, usage)
		return 2
	}
	switch args[0] {
	case "run":
		return runCommand(ctx, args[1:], os.Stdout, os.Stderr)
	case "serve":
		return serveCommand(ctx, args[1:], os.Stdout, os.Stderr)
	case "worker":
		return workerCommand(ctx, args[1:], os.Stdout, os.Stderr)
	case "def":
		return defCommand(ctx, args[1:], os.Stdout, os.Stderr)
	case "trigger":
		return triggerCommand(ctx, args[1:], os.Stdout, os.Stderr)
	case "version", "--version":
		fmt.Fprintf(os.Stdout, "jig %s\n", version)
		return 0
	case "help", "-h", "--help":
		fmt.Fprint(os.Stdout, usage)
		return 0
	default:
		fmt.Fprintf(os.Stderr, "jig: unknown command %q\n\n%s", args[0], usage)
		return 2
	}
}
