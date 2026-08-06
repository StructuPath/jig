// jig is a local-first software factory: one binary that runs repeatable,
// phased coding-agent workflows against Git repositories.
//
// run (U11) is wired: the serverless direct harness. serve (U2/U6/U8),
// worker (U3/U4), and def (U5) wire in with their units.
package main

import (
	"fmt"
	"os"
)

const usage = `jig — a local-first software factory

Usage:
  jig serve    Start the control plane (loopback HTTP + embedded UI)
  jig worker   Start the single implicit worker
  jig run      Run a definition directly against a local repository
  jig def      Validate and manage job definitions
`

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, usage)
		return 2
	}
	switch args[0] {
	case "run":
		return runCommand(args[1:], os.Stdout, os.Stderr)
	case "serve", "worker", "def":
		fmt.Fprintf(os.Stderr, "jig %s: not implemented\n", args[0])
		return 1
	case "help", "-h", "--help":
		fmt.Fprint(os.Stdout, usage)
		return 0
	default:
		fmt.Fprintf(os.Stderr, "jig: unknown command %q\n\n%s", args[0], usage)
		return 2
	}
}
