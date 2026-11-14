// operator_test.go — the operator loop after admission (runs and job verbs),
// and the faults an operator types: an unknown flag or verb, a flag with no
// value, a dead server, a forgotten --param, a trigger saved disabled.
package main

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/StructuPath/JIG-Software-Factory/internal/controlplane"
	"github.com/StructuPath/JIG-Software-Factory/internal/protocol"
)

// jigCommand runs one top-level command in-process and returns its exit
// code, stdout, and stderr.
func jigCommand(
	t *testing.T, command func(context.Context, []string, io.Writer, io.Writer) int, args ...string,
) (int, string, string) {
	t.Helper()
	var stdout, stderr syncBuffer
	code := command(context.Background(), args, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

// The loop an operator runs after `def invoke`: list the run, show it, show
// its job, cancel it, be told how to bring it back, and re-admit it.
func TestTheOperatorLoopListsShowsCancelsAndReadmitsARun(t *testing.T) {
	repo := initRepo(t)
	base, stop := startServe(t, t.TempDir(), "--no-github-poll")
	defer stop()
	runDef(t, exitAccepted, "create", stockDefinition("scout.yaml"), "--server", base)
	var admitted controlplane.RunView
	invoked := runDef(t, exitAccepted, "invoke", "scout", "--instructions", "survey",
		"--repo", repo, "--server", base, "--json")
	if err := json.Unmarshal([]byte(invoked), &admitted); err != nil || len(admitted.Jobs) != 1 {
		t.Fatalf("invoke --json: %v\n%s", err, invoked)
	}
	runID, jobID := admitted.Run.ID, admitted.Jobs[0].ID

	code, listed, stderr := jigCommand(t, runsCommand, "list", "--server", base)
	if code != exitAccepted || !strings.Contains(listed, "RUN") || !strings.Contains(listed, runID) ||
		!strings.Contains(listed, "scout") || !strings.Contains(listed, repo) {
		t.Fatalf("runs list exited %d:\n%s%s", code, listed, stderr)
	}
	code, shown, stderr := jigCommand(t, runsCommand, "show", runID, "--server", base)
	if code != exitAccepted || !strings.Contains(shown, jobID) || !strings.Contains(shown, "queued") ||
		!strings.Contains(shown, "params:     prompt") {
		t.Fatalf("runs show exited %d:\n%s%s", code, shown, stderr)
	}
	code, asJSON, _ := jigCommand(t, runsCommand, "show", "--json", runID, "--server", base)
	var view controlplane.RunView
	if code != exitAccepted || json.Unmarshal([]byte(asJSON), &view) != nil || view.Run.ID != runID {
		t.Fatalf("runs show --json is not the API object:\n%s", asJSON)
	}

	code, job, stderr := jigCommand(t, jobCommand, "show", jobID, "--server", base)
	if code != exitAccepted || !strings.Contains(job, "state:      queued") || !strings.Contains(job, runID) {
		t.Fatalf("job show exited %d:\n%s%s", code, job, stderr)
	}
	code, cancelled, stderr := jigCommand(t, jobCommand, "cancel", jobID, "--server", base)
	if code != exitAccepted || !strings.Contains(cancelled, "job "+jobID+": cancelled") {
		t.Fatalf("job cancel exited %d:\n%s%s", code, cancelled, stderr)
	}
	code, _, stderr = jigCommand(t, jobCommand, "retry", jobID, "--server", base)
	want := "job is cancelled; only a failed job can be retried — re-admit its run: jig runs readmit " + runID
	if code != exitInfraFailed || !strings.Contains(stderr, want) {
		t.Fatalf("retrying a cancelled job exited %d, want %d with %q:\n%s", code, exitInfraFailed, want, stderr)
	}
	// A settled, unaccepted job: --follow returns at once with exit 3.
	code, _, stderr = jigCommand(t, jobCommand, "show", jobID, "--follow", "--server", base)
	if code != exitRejected || !strings.Contains(stderr, "job "+jobID+": cancelled") {
		t.Fatalf("job show --follow on a cancelled job exited %d, want %d:\n%s", code, exitRejected, stderr)
	}

	code, readmitted, stderr := jigCommand(t, runsCommand, "readmit", runID, "--server", base)
	if code != exitAccepted || !strings.Contains(readmitted, "re-admitted from "+runID) ||
		!strings.Contains(readmitted, "queued") {
		t.Fatalf("runs readmit exited %d:\n%s%s", code, readmitted, stderr)
	}

	for _, tc := range []struct {
		command func(context.Context, []string, io.Writer, io.Writer) int
		args    []string
		want    string
	}{
		{jobCommand, []string{"show", "badid"}, `jig job show: no job with id "badid"`},
		{jobCommand, []string{"release", "badid"}, `jig job release: no job with id "badid"`},
		{jobCommand, []string{"cancel", "badid"}, `jig job cancel: no job with id "badid"`},
		{runsCommand, []string{"show", "badid"}, `jig runs show: no run with id "badid"`},
		{triggerCommand, []string{"delete", "badid"}, `jig trigger delete: no trigger with id "badid"`},
	} {
		code, _, stderr := jigCommand(t, tc.command, append(tc.args, "--server", base)...)
		if code != exitInfraFailed || strings.TrimSpace(stderr) != tc.want {
			t.Errorf("%v exited %d with %q, want %d with %q", tc.args, code, stderr, exitInfraFailed, tc.want)
		}
	}

	again := runDefStderr(t, exitInfraFailed, "create", stockDefinition("scout.yaml"), "--server", base)
	if !strings.Contains(again, "update it with: jig def update scout <file>") {
		t.Fatalf("a duplicate definition does not name the update command:\n%s", again)
	}
}

// --follow waits through the live states and exits with the verdict.
func TestJobShowFollowExitsWithTheJobsOutcome(t *testing.T) {
	for _, tc := range []struct {
		final string
		want  int
	}{
		{protocol.JobAccepted, exitAccepted},
		{protocol.JobFailed, exitRejected},
		{protocol.JobAcceptedUnpublished, exitRejected},
	} {
		states := []string{protocol.JobQueued, protocol.JobActive, tc.final}
		var mutex sync.Mutex
		gets := 0
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mutex.Lock()
			defer mutex.Unlock()
			state := states[min(gets, len(states)-1)]
			gets++
			_ = json.NewEncoder(w).Encode(controlplane.JobDetail{Job: protocol.Job{ID: "job-1", State: state}})
		}))
		code, stdout, stderr := jigJob(t, context.Background(), "show", "job-1", "--follow", "--server", server.URL)
		server.Close()
		if code != tc.want || !strings.Contains(stdout, "state:      "+tc.final) {
			t.Errorf("follow to %s exited %d, want %d:\n%s", tc.final, code, tc.want, stdout)
		}
		for _, state := range states {
			if !strings.Contains(stderr, "job job-1: "+state) {
				t.Errorf("follow to %s did not narrate %s:\n%s", tc.final, state, stderr)
			}
		}
	}
}

func TestATriggerIsCreatedDisabledUnlessEnabledAndCanBeDeleted(t *testing.T) {
	repo := initRepo(t)
	base, stop := startServe(t, t.TempDir(), "--no-github-poll")
	defer stop()
	runDef(t, exitAccepted, "create", stockDefinition("scout.yaml"), "--server", base)
	create := func(name string, extra ...string) (int, string, string) {
		return jigCommand(t, triggerCommand, append([]string{"create", "--def", "scout", "--name", name,
			"--cron", "0 3 * * *", "--timezone", "America/Denver", "--repo", repo,
			"--instructions", "survey", "--server", base}, extra...)...)
	}

	code, stdout, stderr := create("nightly")
	if code != exitAccepted || !strings.Contains(stdout, "disabled") ||
		!strings.Contains(stdout, "created disabled — start it with: jig trigger enable ") {
		t.Fatalf("trigger create exited %d without the enable hint:\n%s%s", code, stdout, stderr)
	}
	code, stdout, stderr = create("hourly", "--enable", "--json")
	var trigger controlplane.Trigger
	if code != exitAccepted || json.Unmarshal([]byte(stdout), &trigger) != nil || !trigger.Enabled {
		t.Fatalf("trigger create --enable exited %d, trigger not enabled:\n%s%s", code, stdout, stderr)
	}
	if strings.Contains(stderr, "created disabled") {
		t.Fatalf("an enabled trigger was described as disabled: %s", stderr)
	}

	code, _, stderr = jigCommand(t, triggerCommand, "delete", trigger.ID, "--server", base)
	if code != exitInfraFailed || !strings.Contains(stderr, "stop it first with: jig trigger disable "+trigger.ID) {
		t.Fatalf("deleting an enabled trigger exited %d without the disable hint: %s", code, stderr)
	}
	runTrigger(t, exitAccepted, "disable", trigger.ID, "--server", base)
	code, stdout, stderr = jigCommand(t, triggerCommand, "delete", trigger.ID, "--server", base)
	if code != exitAccepted || !strings.Contains(stdout, "deleted trigger "+trigger.ID) {
		t.Fatalf("trigger delete exited %d:\n%s%s", code, stdout, stderr)
	}
	code, _, stderr = jigCommand(t, triggerCommand, "show", trigger.ID, "--server", base)
	if code != exitInfraFailed || !strings.Contains(stderr, "no trigger with id") {
		t.Fatalf("a deleted trigger is still shown (exit %d): %s", code, stderr)
	}

	code, _, stderr = jigCommand(t, triggerCommand, "create", "--def", "scout", "--name", "n",
		"--cron", "0 3 * * *", "--repo", repo, "--instructions", "x", "--server", base)
	if code != exitUsage || !strings.HasPrefix(stderr, "jig trigger create: --timezone is required\n") {
		t.Fatalf("a missing timezone exited %d: %s", code, stderr)
	}
}

const focusDefinition = `name: survey
roster:
  surveyor:
    model: haiku
    system_prompt: You survey a repository and change nothing.
    user_prompt: "Task: {{prompt}}. Focus on {{focus}}. Notes go in {{handoff_dir}}."
    env: [PATH]
    writes: []
phases:
  - {name: survey, kind: agent, owner: surveyor}
`

// An unset {{param}} would reach the agent literally; invoke refuses it,
// and validate says which params a definition needs. Built-in variables are
// never flagged.
func TestInvokeRefusesAPromptParameterThatWasNotPassed(t *testing.T) {
	repo := initRepo(t)
	path := filepath.Join(t.TempDir(), "survey.yaml")
	if err := os.WriteFile(path, []byte(focusDefinition), 0o644); err != nil {
		t.Fatal(err)
	}
	validated := runDef(t, exitAccepted, "validate", path)
	if !strings.Contains(validated, "  params: focus\n") {
		t.Fatalf("validate does not list the expected params:\n%s", validated)
	}
	if listed := runDef(t, exitAccepted, "validate", stockDefinition("scout.yaml")); strings.Contains(listed, "params:") {
		t.Fatalf("a definition that only uses built-ins lists params:\n%s", listed)
	}

	base, stop := startServe(t, t.TempDir(), "--no-github-poll")
	defer stop()
	runDef(t, exitAccepted, "create", path, "--server", base)
	refused := runDefStderr(t, exitUsage, "invoke", "survey", "--instructions", "look",
		"--repo", repo, "--server", base)
	want := `jig def invoke: missing_param: role "surveyor" user_prompt references {{focus}}; pass --param focus=<value>` + "\n"
	if !strings.HasPrefix(refused, want) || !strings.Contains(refused, "Usage: jig def invoke") {
		t.Fatalf("invoke without --param focus:\n%s\nwant it to open with %q", refused, want)
	}
	runDef(t, exitAccepted, "invoke", "survey", "--instructions", "look",
		"--repo", repo, "--param", "focus=tests", "--server", base)

	code, _, stderr := jigCommand(t, triggerCommand, "create", "--def", "survey", "--name", "n",
		"--cron", "0 3 * * *", "--timezone", "America/Denver", "--repo", repo, "--instructions", "x",
		"--server", base)
	if code != exitUsage || !strings.Contains(stderr, "missing_param") {
		t.Fatalf("trigger create without --param focus exited %d: %s", code, stderr)
	}
}

func TestADeadServerIsNamedWithWhereItsAddressCameFrom(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	dead := "http://" + listener.Addr().String()
	listener.Close()

	t.Setenv(serverEnv, "")
	code, _, stderr := jigCommand(t, defCommand, "list", "--server", dead)
	want := "jig def list: no control plane at " + dead + " (from --server) — start one with: jig serve\n"
	if code != exitInfraFailed || stderr != want {
		t.Fatalf("dead --server exited %d with %q, want %d with %q", code, stderr, exitInfraFailed, want)
	}
	t.Setenv(serverEnv, dead)
	code, _, stderr = jigCommand(t, runsCommand, "list")
	if code != exitInfraFailed || !strings.Contains(stderr, "(from $JIG_SERVER)") {
		t.Fatalf("dead $JIG_SERVER exited %d: %s", code, stderr)
	}
	if source := func() string { t.Setenv(serverEnv, ""); return serverSource(defaultServer()) }(); source != "default" {
		t.Fatalf("the built-in address is attributed to %q, want default", source)
	}
}

func TestATypoIsCorrectedAtEveryLevel(t *testing.T) {
	for _, tc := range []struct {
		command func(context.Context, []string, io.Writer, io.Writer) int
		args    []string
		first   string
		usage   string
	}{
		{defCommand, []string{"invoke", "x", "--parm", "a=b"},
			"jig def invoke: unknown flag --parm — did you mean --param?", "Usage: jig def invoke"},
		{defCommand, []string{"validat", "x.yaml"},
			`jig def: unknown command "validat" — did you mean: jig def validate?`, "Usage: jig def"},
		{runsCommand, []string{"lst"}, `jig runs: unknown command "lst" — did you mean: jig runs list?`, "Usage: jig runs"},
		{jobCommand, []string{"shw", "j"}, `jig job: unknown command "shw" — did you mean: jig job show?`, "Usage: jig job"},
		{runCommand, []string{"list"}, "jig run: did you mean: jig runs list?", "Usage: jig runs"},
		{defCommand, []string{"list", "--zzzzzz"}, "jig def list: unknown flag --zzzzzz", "Usage: jig def list"},
		// A value-taking flag left last must not swallow the `--` sentinel.
		{defCommand, []string{"show", "foo", "--server"}, "jig def show: flag needs an argument: --server", "Usage: jig def show"},
		{serveCommand, []string{"--data"}, "jig serve: flag needs an argument: --data", "Usage: jig serve"},
		{runCommand, []string{"--def", "d.yaml", ".", "p", "--data"}, "jig run: flag needs an argument: --data", "Usage: jig run"},
		// The #60 repro: a trailing --instructions once admitted a run with
		// the sentinel as its instructions.
		{defCommand, []string{"invoke", "x", "--repo", ".", "--instructions"},
			"jig def invoke: flag needs an argument: --instructions", "Usage: jig def invoke"},
	} {
		code, _, stderr := jigCommand(t, tc.command, tc.args...)
		if code != exitUsage {
			t.Errorf("%v exited %d, want %d\n%s", tc.args, code, exitUsage, stderr)
		}
		if !strings.HasPrefix(stderr, tc.first+"\n") || !strings.Contains(stderr, tc.usage) {
			t.Errorf("%v: stderr does not open with %q then the usage:\n%s", tc.args, tc.first, stderr)
		}
	}
	// A real `jig run` with a repository called "list" is still a run.
	if code, _, stderr := jigCommand(t, runCommand, "--def", "missing.yaml", "list", "p"); strings.Contains(stderr, "did you mean") {
		t.Fatalf("a --def run was redirected to jig runs (exit %d): %s", code, stderr)
	}
}

// Every command, verb, and flag the binary accepts is in every script: the
// scripts are generated from the command table and the commands' own flag
// sets, so this holds by construction and fails if either is bypassed.
func TestCompletionCoversEveryCommandVerbAndFlag(t *testing.T) {
	scripts := map[string]string{}
	for _, shell := range []string{"zsh", "bash", "fish"} {
		code, script, stderr := jigCommand(t, completionCommand, shell)
		if code != exitAccepted || script == "" {
			t.Fatalf("completion %s exited %d: %s", shell, code, stderr)
		}
		scripts[shell] = script
	}
	if code, _, _ := jigCommand(t, completionCommand, "powershell"); code != exitUsage {
		t.Fatalf("an unknown shell exited %d, want %d", code, exitUsage)
	}
	for _, entry := range commands {
		words := []string{entry.name}
		for _, verb := range entry.verbs {
			words = append(words, verb.name)
		}
		for shell, script := range scripts {
			for _, word := range words {
				if !strings.Contains(script, word) {
					t.Errorf("%s completion is missing %q", shell, word)
				}
			}
		}
	}
	for _, flag := range []string{"param", "follow", "enable", "limit", "no-wait", "timezone", "allow-merge", "runtime"} {
		for shell, script := range scripts {
			if !strings.Contains(script, flag) {
				t.Errorf("%s completion is missing --%s", shell, flag)
			}
		}
	}
	if !strings.Contains(scripts["bash"], `"def invoke") candidates="--instructions --json --param --repo --server"`) {
		t.Errorf("bash completion does not offer def invoke its own flags:\n%s", scripts["bash"])
	}
}

func TestStartPassesItsAddressDataAndRuntimeThrough(t *testing.T) {
	code, _, stderr := jigCommand(t, startCommand, "--runtime", "nope")
	if code != exitUsage || !strings.Contains(stderr, `jig start: unknown runtime "nope"`) {
		t.Fatalf("start --runtime nope exited %d: %s", code, stderr)
	}
	flags := probeFlags(startCommand, nil)
	for _, want := range []string{"--addr", "--data", "--runtime"} {
		if !strings.Contains(strings.Join(flags, " "), want) {
			t.Errorf("jig start has no %s flag: %v", want, flags)
		}
	}
}
