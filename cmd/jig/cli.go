// cli.go — argument and error conventions every subcommand shares, so that
// one command's flags, --help, server address, and exit codes behave like
// every other's.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/StructuPath/JIG-Software-Factory/internal/controlplane"
	"github.com/StructuPath/JIG-Software-Factory/internal/protocol"
)

// serverEnv names the control plane for every --server flag, so an operator
// pointing jig at a second instance sets it once per shell.
const serverEnv = "JIG_SERVER"

func defaultServer() string {
	if server := strings.TrimSpace(os.Getenv(serverEnv)); server != "" {
		return server
	}
	return "http://" + defaultServerAuthority
}

func serverFlag(flags *flag.FlagSet) *string {
	return flags.String("server", defaultServer(), "control plane URL")
}

// parseFlags parses flags wherever they appear among the positional
// arguments. Go's flag package stops at the first positional, so
// `jig def show <id> --json` would otherwise read --json as a second
// positional. Flags are pulled out in order — with the value of every
// non-boolean flag that is not written as --name=value — and parsed ahead of
// the positionals, which flags.Args() then returns. A bare `--` ends flag
// parsing: everything after it is positional, even if it starts with "-".
func parseFlags(flags *flag.FlagSet, args []string) error {
	var flagArgs, positionals []string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			positionals = append(positionals, args[i+1:]...)
			break
		}
		if len(arg) < 2 || arg[0] != '-' {
			positionals = append(positionals, arg)
			continue
		}
		flagArgs = append(flagArgs, arg)
		name := strings.TrimLeft(arg, "-")
		if strings.Contains(name, "=") {
			continue
		}
		if defined := flags.Lookup(name); defined != nil && !isBoolFlag(defined) && i+1 < len(args) {
			i++
			flagArgs = append(flagArgs, args[i])
		}
	}
	return flags.Parse(append(append(flagArgs, "--"), positionals...))
}

func isBoolFlag(defined *flag.Flag) bool {
	boolean, ok := defined.Value.(interface{ IsBoolFlag() bool })
	return ok && boolean.IsBoolFlag()
}

// flagExit is the exit code of a failed parse: --help is a successful
// request for the usage the flag set already printed, anything else a usage
// error.
func flagExit(err error) int {
	if errors.Is(err, flag.ErrHelp) {
		return exitAccepted
	}
	return exitUsage
}

// usageError names the one problem before the command's usage, so the
// operator reads the fault first rather than hunting for it in the help.
func usageError(stderr io.Writer, command, problem, usage string) int {
	fmt.Fprintf(stderr, "%s: %s\n\n%s", command, problem, usage)
	return exitUsage
}

// apiError is the control plane's error document: its status, diagnostic
// code, and message.
type apiError struct {
	Status  int
	Code    string
	Message string
}

func (e *apiError) Error() string {
	if e.Code != "" {
		return e.Code + ": " + e.Message
	}
	return e.Message
}

// apiExit is the exit code of a failed control-plane call. A 400 with an
// invalid_* code is the server's validation of what the operator typed — the
// README's exit 2 — and everything else is the request failing (exit 1).
func apiExit(err error) int {
	var rejection *apiError
	if errors.As(err, &rejection) && rejection.Status == http.StatusBadRequest &&
		strings.HasPrefix(rejection.Code, "invalid_") {
		return exitUsage
	}
	return exitInfraFailed
}

// cliRepository resolves a relative local path (".", "../repo", "repo") to
// an absolute one before it is sent: the control plane runs in its own
// working directory, so a relative path means nothing there. A remote form
// — a URL, an scp-style remote, or host/owner/repo — is sent unchanged.
func cliRepository(value string) string {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" || filepath.IsAbs(trimmed) || strings.Contains(trimmed, "://") ||
		strings.Contains(trimmed, "@") {
		return value
	}
	if host, path, found := strings.Cut(trimmed, "/"); found && path != "" &&
		strings.Contains(host, ".") && !strings.HasPrefix(host, ".") {
		return value
	}
	absolute, err := filepath.Abs(trimmed)
	if err != nil {
		return value
	}
	return absolute
}

var definitionIDPattern = regexp.MustCompile(
	`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// resolveDefinition turns an operator's definition reference — its id or
// its name, which the store keeps unique — into the id the API takes.
func resolveDefinition(ctx context.Context, server, reference string) (string, error) {
	if definitionIDPattern.MatchString(reference) {
		return reference, nil
	}
	var definitions []protocol.Definition
	if err := apiCall(ctx, server, http.MethodGet, "/api/definitions", nil, &definitions); err != nil {
		return "", err
	}
	for _, definition := range definitions {
		if definition.Name == reference {
			return definition.ID, nil
		}
	}
	return "", definitionNotFound(reference)
}

func definitionNotFound(reference string) error {
	return fmt.Errorf("no definition named or with id %q", reference)
}

// isNotFound reports a 404 from the control plane.
func isNotFound(err error) bool {
	var rejection *apiError
	return errors.As(err, &rejection) && rejection.Status == http.StatusNotFound
}

// liveWorkers counts the workers the claim transaction would serve, or -1
// when the fleet cannot be read.
func liveWorkers(ctx context.Context, server string) int {
	var fleet controlplane.FleetView
	if err := apiCall(ctx, server, http.MethodGet, "/api/workers", nil, &fleet); err != nil {
		return -1
	}
	return fleet.LiveCount
}

// runViewURL is the UI page of one run (web/src/router.ts: /runs/<id>).
func runViewURL(server, runID string) string {
	return strings.TrimRight(strings.TrimSpace(server), "/") + "/runs/" + url.PathEscape(runID)
}
