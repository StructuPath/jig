// cli_test.go — the conventions every subcommand shares: flags anywhere,
// $JIG_SERVER, --help as success, a named problem before the usage, exit 2
// for what the operator typed, and definitions referenced by name.
package main

import (
	"context"
	"errors"
	"flag"
	"io"
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestFlagsParseBeforeOrAfterThePositionals(t *testing.T) {
	for _, tc := range []struct {
		name        string
		args        []string
		server      string
		json        bool
		positionals []string
	}{
		{"flags first", []string{"--server", "s", "--json", "a", "b"}, "s", true, []string{"a", "b"}},
		{"flags last", []string{"a", "b", "--json", "--server", "s"}, "s", true, []string{"a", "b"}},
		{"interleaved", []string{"a", "--server=s", "b", "--json"}, "s", true, []string{"a", "b"}},
		{"single dash", []string{"a", "-server", "s"}, "s", false, []string{"a"}},
		{"double dash ends flags", []string{"a", "--", "--json", "-x"}, "default", false, []string{"a", "--json", "-x"}},
		{"bare dash is positional", []string{"-", "--json"}, "default", true, []string{"-"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			flags := flag.NewFlagSet("test", flag.ContinueOnError)
			flags.SetOutput(io.Discard)
			server := flags.String("server", "default", "")
			asJSON := flags.Bool("json", false, "")
			if err := parseFlags(flags, tc.args); err != nil {
				t.Fatalf("parse %v: %v", tc.args, err)
			}
			if *server != tc.server || *asJSON != tc.json || !slices.Equal(flags.Args(), tc.positionals) {
				t.Fatalf("parse %v = server %q json %v positionals %q, want %q %v %q",
					tc.args, *server, *asJSON, flags.Args(), tc.server, tc.json, tc.positionals)
			}
		})
	}
}

func TestEveryServerFlagDefaultsToJIGServer(t *testing.T) {
	t.Setenv(serverEnv, "")
	if got := defaultServer(); got != "http://127.0.0.1:8383" {
		t.Fatalf("default server without %s = %q", serverEnv, got)
	}
	t.Setenv(serverEnv, "http://127.0.0.1:8594")
	flags, server, _ := defFlags("jig def list", io.Discard, defListUsage)
	if err := parseFlags(flags, nil); err != nil {
		t.Fatal(err)
	}
	if *server != "http://127.0.0.1:8594" {
		t.Fatalf("--server default = %q, want $%s", *server, serverEnv)
	}
}

func TestHelpExitsZeroOnEveryCommand(t *testing.T) {
	commands := map[string]func(context.Context, []string, io.Writer, io.Writer) int{
		"start": startCommand, "serve": serveCommand, "worker": workerCommand,
		"run": runCommand, "report": reportCommand, "job": jobCommand,
		"def": defCommand, "trigger": triggerCommand,
	}
	for name, command := range commands {
		for _, args := range [][]string{{"--help"}, {"-h"}} {
			if name == "job" || name == "def" || name == "trigger" {
				// The family's own help, and one subcommand's.
				sub := map[string]string{"job": "release", "def": "invoke", "trigger": "create"}[name]
				for _, full := range [][]string{args, append([]string{sub}, args...)} {
					var stdout, stderr strings.Builder
					if code := command(context.Background(), full, &stdout, &stderr); code != exitAccepted {
						t.Errorf("jig %s %v exited %d, want 0", name, full, code)
					}
					if stdout.Len()+stderr.Len() == 0 {
						t.Errorf("jig %s %v printed no usage", name, full)
					}
				}
				continue
			}
			var stdout, stderr strings.Builder
			if code := command(context.Background(), args, &stdout, &stderr); code != exitAccepted {
				t.Errorf("jig %s %v exited %d, want 0\nstderr: %s", name, args, code, stderr.String())
			}
			if !strings.Contains(stdout.String()+stderr.String(), "Usage: jig "+name) {
				t.Errorf("jig %s %v printed no usage:\n%s", name, args, stderr.String())
			}
		}
	}
}

func TestAUsageErrorNamesTheProblemBeforeThatCommandsUsage(t *testing.T) {
	for _, tc := range []struct {
		command func(context.Context, []string, io.Writer, io.Writer) int
		args    []string
		first   string
		usage   string
	}{
		{defCommand, []string{"invoke", "smoke", "--repo", "."}, "jig def invoke: --instructions is required", "Usage: jig def invoke"},
		{defCommand, []string{"invoke", "smoke", "--instructions", "x"}, "jig def invoke: at least one --repo is required", "Usage: jig def invoke"},
		{defCommand, []string{"show"}, "jig def show: exactly one definition id or name is required", "Usage: jig def show"},
		{triggerCommand, []string{"create", "--name", "n", "--repo", "."}, "jig trigger create: --def is required", "Usage: jig trigger create"},
		{workerCommand, []string{"--runtime", "foo"}, `jig worker: unknown runtime "foo"`, "Usage: jig worker"},
		{runCommand, []string{"--def", "d.yaml", "--runtime", "foo", ".", "p"}, `jig run: unknown runtime "foo"`, "Usage: jig run"},
		{serveCommand, []string{"extra"}, `jig serve: unexpected argument "extra"`, "Usage: jig serve"},
	} {
		var stdout, stderr strings.Builder
		code := tc.command(context.Background(), tc.args, &stdout, &stderr)
		if code != exitUsage {
			t.Errorf("%v exited %d, want %d", tc.args, code, exitUsage)
		}
		message := stderr.String()
		if !strings.HasPrefix(message, tc.first+"\n") && !strings.HasPrefix(message, tc.first+" ") {
			t.Errorf("%v: stderr does not open with %q:\n%s", tc.args, tc.first, message)
		}
		if !strings.Contains(message, tc.usage) || strings.Contains(message, "Commands:") {
			t.Errorf("%v: stderr is not that command's usage alone:\n%s", tc.args, message)
		}
	}
}

func TestAnInvalidRequestRejectionExitsTwo(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want int
	}{
		{&apiError{Status: http.StatusBadRequest, Code: "invalid_definition"}, exitUsage},
		{&apiError{Status: http.StatusBadRequest, Code: "duplicate_target"}, exitInfraFailed},
		{&apiError{Status: http.StatusNotFound, Code: "not_found"}, exitInfraFailed},
		{&apiError{Status: http.StatusInternalServerError, Code: "invalid_x"}, exitInfraFailed},
		{errors.New("connection refused"), exitInfraFailed},
	} {
		if got := apiExit(tc.err); got != tc.want {
			t.Errorf("apiExit(%v) = %d, want %d", tc.err, got, tc.want)
		}
	}
}

func TestARelativeRepositoryIsResolvedBeforeItIsSent(t *testing.T) {
	directory := t.TempDir()
	t.Chdir(directory)
	working, _ := filepath.Abs(".")
	for value, want := range map[string]string{
		".":                         working,
		"./repo":                    filepath.Join(working, "repo"),
		"../repo":                   filepath.Join(filepath.Dir(working), "repo"),
		"repo":                      filepath.Join(working, "repo"),
		"/abs/repo":                 "/abs/repo",
		"file:///abs/repo":          "file:///abs/repo",
		"github.com/owner/repo":     "github.com/owner/repo",
		"git@github.com:owner/repo": "git@github.com:owner/repo",
	} {
		if got := cliRepository(value); got != want {
			t.Errorf("cliRepository(%q) = %q, want %q", value, got, want)
		}
	}
}

func TestAMistypedCommandIsCorrected(t *testing.T) {
	for word, want := range map[string]string{"serv": "serve", "wroker": "worker", "trgger": "trigger", "zzz": ""} {
		if got := closestCommand(word); got != want {
			t.Errorf("closestCommand(%q) = %q, want %q", word, got, want)
		}
	}
}

func TestVersionNamesTheRelease(t *testing.T) {
	if line := versionLine(); !strings.HasPrefix(line, "jig "+version) {
		t.Fatalf("version line %q does not open with the release", line)
	}
}

func TestADefinitionIsReferencedByNameWithFlagsAfterIt(t *testing.T) {
	repo := initRepo(t)
	base, stop := startServe(t, t.TempDir(), "--no-github-poll")
	defer stop()
	runDef(t, exitAccepted, "create", stockDefinition("scout.yaml"), "--server", base)

	shown := runDef(t, exitAccepted, "show", "scout", "--json", "--server", base)
	if !strings.Contains(shown, `"name": "scout"`) {
		t.Fatalf("show by name with trailing flags did not print the definition:\n%s", shown)
	}

	// "." is the operator's working directory, resolved by the CLI.
	t.Chdir(repo)
	invoked := runDef(t, exitAccepted, "invoke", "scout",
		"--instructions", "survey this repository", "--repo", ".", "--server", base)
	if !strings.Contains(invoked, "queued") {
		t.Fatalf("invoke did not report the queued run:\n%s", invoked)
	}
	if !strings.Contains(invoked, "view: "+base+"/runs/") {
		t.Fatalf("invoke does not print the run's UI page:\n%s", invoked)
	}
	if !strings.Contains(invoked, "note: no worker is online; start one with: jig worker") {
		t.Fatalf("invoke does not say no worker is online:\n%s", invoked)
	}

	missing := runDefStderr(t, exitInfraFailed, "invoke", "smoke",
		"--instructions", "x", "--repo", ".", "--server", base)
	if !strings.Contains(missing, `no definition named or with id "smoke"`) {
		t.Fatalf("an unknown name is not named back:\n%s", missing)
	}

	var stdout, stderr syncBuffer
	code := triggerCommand(context.Background(), []string{"create", "--def", "scout",
		"--name", "nightly", "--cron", "0 3 * * *", "--timezone", "America/Denver", "--repo", ".",
		"--instructions", "survey", "--server", base}, &stdout, &stderr)
	if code != exitAccepted {
		t.Fatalf("trigger create --def <name> exited %d\nstderr: %s", code, stderr.String())
	}

	// The control plane's validation of what the operator typed is exit 2.
	var rejected syncBuffer
	code = triggerCommand(context.Background(), []string{"create", "--def", "scout",
		"--name", "broken", "--cron", "not a cron", "--timezone", "America/Denver", "--repo", ".",
		"--instructions", "survey", "--server", base}, &syncBuffer{}, &rejected)
	if code != exitUsage || !strings.Contains(rejected.String(), "invalid_cron") {
		t.Fatalf("a rejected cron exited %d, want %d\nstderr: %s", code, exitUsage, rejected.String())
	}
}
