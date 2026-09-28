// ci_repair_test.go — one CI repair round through the real `jig worker`
// wiring (plan U5's required end-to-end test). Everything is production
// except the runtime (scripted) and the GitHub gateway (a fake): a live
// `jig serve`, workerAttemptRunner with its per-attempt trace stream and
// scratch, the publishing runner, and a real git remote. It proves what the
// U3 review found broken: the continuation's scratch (handoff notes
// included) and trace stream must outlive Execute until the round is done.
package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/StructuPath/jig/internal/controlplane"
	"github.com/StructuPath/jig/internal/protocol"
	"github.com/StructuPath/jig/internal/worker"
)

const ciRepairDefinition = `name: ci-repair-e2e
roster:
  builder:
    model: test-model
    system_prompt: Build.
    user_prompt: "Build."
    env: [PATH]
    writes: ["jig-note.txt", "jig-fix.txt"]
phases:
  - {name: build, kind: agent, owner: builder}
  - name: verify
    kind: code
    command: "test -s jig-note.txt"
acceptance: [all_phases_passed]
publish:
  ci:
    wait: true
    on_fail: {run: build, budget: 1}
`

// e2eGateway is the fake GitHub: CI is red on the first head it judges and
// green on the next, and every poll runs the test's probe first.
type e2eGateway struct {
	mutex  sync.Mutex
	first  string
	probe  func(sha string, red bool)
	polled []string
}

func (g *e2eGateway) FindOrCreatePullRequest(context.Context, worker.PullRequestRequest) (worker.PullRequest, error) {
	return worker.PullRequest{Number: 1, URL: "https://github.com/example/repo/pull/1", State: "open"}, nil
}

func (g *e2eGateway) CommitChecks(_ context.Context, _ string, sha string) ([]worker.CICheck, error) {
	g.mutex.Lock()
	defer g.mutex.Unlock()
	g.polled = append(g.polled, sha)
	if g.first == "" {
		g.first = sha
	}
	red := sha == g.first
	g.probe(sha, red)
	if red {
		return []worker.CICheck{{Name: "lint", Verdict: worker.CIFail, Conclusion: "failure"}}, nil
	}
	return []worker.CICheck{{Name: "lint", Verdict: worker.CIPass, Conclusion: "success"}}, nil
}

func (g *e2eGateway) FailedCheckLogs(_ context.Context, _ string, checks []worker.CICheck) []worker.CICheck {
	return checks
}

func TestACIRepairRoundRunsThroughTheRealWorkerWiring(t *testing.T) {
	repo := initRepo(t)
	serverData, workerData := t.TempDir(), t.TempDir()
	scriptRuntime(t,
		map[string]any{"files": map[string]any{"jig-note.txt": "note\n"},
			"text": envelopeJSON(t, map[string]any{"status": "success", "summary": "wrote the note"})},
		map[string]any{"files": map[string]any{"jig-fix.txt": "fixed\n"},
			"text": envelopeJSON(t, map[string]any{"status": "success", "summary": "fixed lint"})},
	)
	base, stopServe := startServe(t, serverData, "--no-github-poll")
	defer stopServe()

	var definition protocol.Definition
	if err := apiCall(context.Background(), base, "POST", "/api/definitions",
		controlplane.DefinitionInput{Source: ciRepairDefinition}, &definition); err != nil {
		t.Fatal(err)
	}
	var view controlplane.RunView
	if err := apiCall(context.Background(), base, "POST", "/api/runs", controlplane.RunInvocation{
		DefinitionID: definition.ID, Instructions: "write the note",
		Targets: []controlplane.InvocationTarget{{Repository: repo}},
	}, &view); err != nil {
		t.Fatal(err)
	}

	// The handoff note is planted while CI is red — after Execute returned,
	// before the round — and must still be there when CI judges the fix.
	var probeErrors []string
	note := ""
	gateway := &e2eGateway{probe: func(sha string, red bool) {
		handoffs, _ := filepath.Glob(filepath.Join(workerData, "scratch", "*", "handoff"))
		if len(handoffs) != 1 {
			probeErrors = append(probeErrors, fmt.Sprintf(
				"CI poll on %.8s (red=%v) found %d scratch handoff dirs; the continuation's scratch is gone",
				sha, red, len(handoffs)))
			return
		}
		if red && note == "" {
			note = filepath.Join(handoffs[0], "plan.md")
			if err := os.WriteFile(note, []byte("the plan\n"), 0o600); err != nil {
				probeErrors = append(probeErrors, err.Error())
			}
			return
		}
		if !red {
			if _, err := os.Stat(note); err != nil {
				probeErrors = append(probeErrors, "the handoff note did not survive the round: "+err.Error())
			}
		}
	}}

	ctx := context.Background()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	selected, err := selectRuntime(ctx, "claude-code", true)
	if err != nil {
		t.Fatal(err)
	}
	var host *worker.Worker
	publisher := worker.NewPublishingRunner(
		workerAttemptRunner(ctx, workerData, selected, logger, func() *worker.Worker { return host }),
		gateway, worker.PublishOptions{
			CommitAuthorName: "jig-test", CommitAuthorEmail: "jig@test",
			CIPollInterval: 5 * time.Millisecond, CIRegistrationGrace: 20 * time.Millisecond,
		})
	host, err = worker.New(worker.Config{
		ServerURL: base, DataDir: workerData, Capacity: 1,
		Probes: []worker.RuntimeProbe{selected.Runtime}, Runner: publisher, Logger: logger,
	})
	if err != nil {
		t.Fatal(err)
	}
	publisher.Bind(host)
	if _, err := host.RegisterOnce(ctx); err != nil {
		t.Fatal(err)
	}

	attempt, err := host.ClaimOnce(ctx)
	if err != nil || attempt == nil {
		t.Fatalf("claim: attempt=%v err=%v", attempt, err)
	}
	for _, problem := range probeErrors {
		t.Error(problem)
	}
	if attempt.State != protocol.AttemptAccepted {
		t.Fatalf("attempt = %s (%s)\n%s, want accepted after one repair round", attempt.State, attempt.Error, attempt.Result)
	}
	if len(gateway.polled) < 2 || note == "" {
		t.Fatalf("CI polls = %d, note planted = %v: the round never ran", len(gateway.polled), note != "")
	}

	// The round's trace reached the control plane: the stream was still open.
	var page struct {
		Events []protocol.Event `json:"events"`
	}
	if err := apiCall(ctx, base, "GET", "/api/attempts/"+attempt.ID+"/events?limit=500", nil, &page); err != nil {
		t.Fatal(err)
	}
	repairStarted, acceptances := false, 0
	for _, event := range page.Events {
		repairStarted = repairStarted || event.Name == "ci_repair_start"
		if event.Name == "acceptance" {
			acceptances++
		}
	}
	if !repairStarted || acceptances != 2 {
		t.Fatalf("control-plane trace has ci_repair_start=%v and %d acceptance event(s), want the round's events too",
			repairStarted, acceptances)
	}

	// And once the attempt is over, its scratch is gone (KTD11).
	if entries, _ := os.ReadDir(filepath.Join(workerData, "scratch")); len(entries) != 0 {
		t.Fatalf("attempt scratch survived the attempt: %v", entries)
	}
}
