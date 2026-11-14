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
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"syscall"

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
//
// Two faults are named here rather than left to the flag package, whose
// wording ("flag provided but not defined: -parm") names neither the
// command nor the fix: an unknown flag, with the nearest defined spelling,
// and a value-taking flag with nothing after it — which would otherwise
// swallow the `--` sentinel inserted below and surface as a nonsense value.
func parseFlags(flags *flag.FlagSet, args []string) error {
	if probe := flagProbe.Load(); probe != nil {
		(*probe)(flags)
		return errFlagProbe
	}
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
		name, _, hasValue := strings.Cut(strings.TrimLeft(arg, "-"), "=")
		defined := flags.Lookup(name)
		if defined == nil {
			if name == "h" || name == "help" {
				continue // the flag package answers with the usage and ErrHelp
			}
			return flagFault(flags, unknownFlag(flags, name))
		}
		if hasValue || isBoolFlag(defined) {
			continue
		}
		if i+1 >= len(args) || args[i+1] == "--" {
			return flagFault(flags, "flag needs an argument: --"+name)
		}
		i++
		flagArgs = append(flagArgs, args[i])
	}
	return flags.Parse(append(append(flagArgs, "--"), positionals...))
}

// errFlagFault is a parse failure parseFlags has already reported.
var errFlagFault = errors.New("flag fault reported")

// flagFault reports one flag problem the way usageError does — the command,
// the problem, then that command's usage — and returns the usage error the
// caller's flagExit turns into exit 2.
func flagFault(flags *flag.FlagSet, problem string) error {
	fmt.Fprintf(flags.Output(), "%s: %s\n\n", flags.Name(), problem)
	flags.Usage()
	return errFlagFault
}

func unknownFlag(flags *flag.FlagSet, name string) string {
	var defined []string
	flags.VisitAll(func(f *flag.Flag) { defined = append(defined, f.Name) })
	problem := "unknown flag --" + name
	if suggestion := protocol.ClosestMatch(name, defined); suggestion != "" {
		problem += " — did you mean --" + suggestion + "?"
	}
	return problem
}

// flagProbe, when set, receives every flag set parseFlags is handed instead
// of parsing it, and the command stops there. It is how `jig completion`
// reads each command's real flags rather than a second list that could
// drift from them: every command builds its flag set and calls parseFlags
// before it does anything else.
var flagProbe atomic.Pointer[func(*flag.FlagSet)]

var errFlagProbe = errors.New("flag probe")

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
	if !hasParentStep(trimmed) {
		absolute, err := filepath.Abs(trimmed)
		if err != nil {
			return value
		}
		return absolute
	}
	// A ".." means what the operating system makes of it — the parent of
	// whatever the preceding component resolves to — not what lexical
	// cleaning makes of it: with link -> target/child, link/../repo is
	// target/repo, and filepath.Abs would say ./repo. Resolve symlinks first;
	// a path that does not resolve is sent uncleaned, so the control plane's
	// own EvalSymlinks fails on (or resolves) the same path the OS would.
	if resolved, err := filepath.EvalSymlinks(trimmed); err == nil {
		if absolute, err := filepath.Abs(resolved); err == nil {
			return absolute
		}
	}
	working, err := os.Getwd()
	if err != nil {
		return value
	}
	return working + string(filepath.Separator) + trimmed
}

// hasParentStep reports a ".." component, the one step lexical cleaning can
// get wrong.
func hasParentStep(path string) bool {
	for _, component := range strings.Split(filepath.ToSlash(path), "/") {
		if component == ".." {
			return true
		}
	}
	return false
}

var definitionIDPattern = regexp.MustCompile(
	`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// resolveDefinition turns an operator's definition reference — its id or
// its name, which the store keeps unique — into the id the API takes. An
// id-shaped reference is tried as an id first, then as a name: a name is
// free-form, so a definition may be named like a UUID and must still be
// reachable by that name.
func resolveDefinition(ctx context.Context, server, reference string) (string, error) {
	if definitionIDPattern.MatchString(reference) {
		var definition protocol.Definition
		err := apiCall(ctx, server, http.MethodGet,
			"/api/definitions/"+url.PathEscape(reference), nil, &definition)
		if err == nil {
			return reference, nil
		}
		if !isNotFound(err) {
			return "", err
		}
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

// subcommand is one verb of a command family (`jig def validate`, `jig job
// show`): its name and its entry point.
type subcommand struct {
	name string
	run  func(ctx context.Context, args []string, stdout, stderr io.Writer) int
}

// dispatchSubcommand runs a family's verb. The table is the family's one
// vocabulary, so dispatch, "did you mean", and `jig completion` cannot
// disagree about which verbs exist.
func dispatchSubcommand(
	ctx context.Context, family, usage string, verbs []subcommand,
	args []string, stdout, stderr io.Writer,
) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return exitUsage
	}
	if args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		fmt.Fprint(stdout, usage)
		return exitAccepted
	}
	names := make([]string, 0, len(verbs))
	for _, verb := range verbs {
		if verb.name == args[0] {
			return verb.run(ctx, args[1:], stdout, stderr)
		}
		names = append(names, verb.name)
	}
	if suggestion := protocol.ClosestMatch(args[0], names); suggestion != "" {
		fmt.Fprintf(stderr, "%s: unknown command %q — did you mean: %s %s?\n\n%s",
			family, args[0], family, suggestion, usage)
		return exitUsage
	}
	fmt.Fprintf(stderr, "%s: unknown command %q\n\n%s", family, args[0], usage)
	return exitUsage
}

// noControlPlaneError is a request that found nothing listening: the most
// common first-run fault, so it names the address, where that address came
// from, and the command that fixes it, instead of a raw dial error.
type noControlPlaneError struct {
	server string
}

func (e *noControlPlaneError) Error() string {
	return fmt.Sprintf("no control plane at %s (from %s) — start one with: jig serve",
		strings.TrimRight(strings.TrimSpace(e.server), "/"), serverSource(e.server))
}

// isNoControlPlane reports a dial that found no listener or no such host.
func isNoControlPlane(err error) bool {
	if errors.Is(err, syscall.ECONNREFUSED) {
		return true
	}
	var dnsError *net.DNSError
	return errors.As(err, &dnsError) && dnsError.IsNotFound
}

// serverSource names where a --server value came from. The flag's default is
// $JIG_SERVER when set, so a value equal to it is attributed to the
// environment (or to the built-in default when the variable is unset).
func serverSource(server string) string {
	environment := strings.TrimSpace(os.Getenv(serverEnv))
	switch {
	case environment != "" && server == environment:
		return "$" + serverEnv
	case environment == "" && server == "http://"+defaultServerAuthority:
		return "default"
	default:
		return "--server"
	}
}

// notFoundAs replaces the control plane's generic 404 with one that names
// what the operator asked for, e.g. `no job with id "badid"`.
func notFoundAs(err error, kind, id string) error {
	if isNotFound(err) {
		return fmt.Errorf("no %s with id %q", kind, id)
	}
	return err
}
