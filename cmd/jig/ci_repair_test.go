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
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
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

// ActionsJobRun and RerunFailedJobs are never reached: the definition
// declares no re-runs, and its checks are not Actions jobs.
func (g *e2eGateway) ActionsJobRun(context.Context, string, int64) (int64, error) {
	return 0, fmt.Errorf("unexpected workflow run lookup")
}

func (g *e2eGateway) RerunFailedJobs(context.Context, string, int64) error {
	return fmt.Errorf("unexpected re-run request")
}

// scriptCIRepairRuntime scripts the chain's build and the repair round's.
func scriptCIRepairRuntime(t *testing.T) {
	t.Helper()
	scriptRuntime(t,
		map[string]any{"files": map[string]any{"jig-note.txt": "note\n"},
			"text": envelopeJSON(t, map[string]any{"status": "success", "summary": "wrote the note"})},
		map[string]any{"files": map[string]any{"jig-fix.txt": "fixed\n"},
			"text": envelopeJSON(t, map[string]any{"status": "success", "summary": "fixed lint"})},
	)
}

// submitCIRepairRun registers ciRepairDefinition and starts one run on repo.
func submitCIRepairRun(t *testing.T, base, repo string) {
	t.Helper()
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
}

// startCIRepairHost builds the worker exactly as `jig worker` does —
// workerAttemptRunner inside the publishing runner — talking to serverURL,
// and registers it.
func startCIRepairHost(t *testing.T, serverURL, workerData string, gateway worker.PullRequestGateway) *worker.Worker {
	t.Helper()
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
		ServerURL: serverURL, DataDir: workerData, Capacity: 1,
		Probes: []worker.RuntimeProbe{selected.Runtime}, Runner: publisher, Logger: logger,
	})
	if err != nil {
		t.Fatal(err)
	}
	publisher.Bind(host)
	if _, err := host.RegisterOnce(ctx); err != nil {
		t.Fatal(err)
	}
	return host
}

func TestACIRepairRoundRunsThroughTheRealWorkerWiring(t *testing.T) {
	repo := initRepo(t)
	serverData, workerData := t.TempDir(), t.TempDir()
	scriptCIRepairRuntime(t)
	base, stopServe := startServe(t, serverData, "--no-github-poll")
	defer stopServe()
	submitCIRepairRun(t, base, repo)

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
	host := startCIRepairHost(t, base, workerData, gateway)
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

// ingestGate fronts the control plane for the worker: while it is closed,
// trace ingestion is refused as if the control plane were unreachable, and
// every other request passes through.
type ingestGate struct {
	open  atomic.Bool
	proxy *httputil.ReverseProxy
}

func (g *ingestGate) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !g.open.Load() && r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/events") {
		http.Error(w, "trace ingestion is down", http.StatusServiceUnavailable)
		return
	}
	g.proxy.ServeHTTP(w, r)
}

// attemptEvents pages through every event the control plane holds for an
// attempt.
func attemptEvents(t *testing.T, base, attemptID string) []protocol.Event {
	t.Helper()
	var events []protocol.Event
	after := int64(0)
	for {
		var page protocol.EventPage
		if err := apiCall(context.Background(), base, "GET",
			fmt.Sprintf("/api/attempts/%s/events?after=%d&limit=500", attemptID, after), nil, &page); err != nil {
			t.Fatal(err)
		}
		events = append(events, page.Events...)
		if len(page.Events) == 0 || page.NextCursor <= after {
			return events
		}
		after = page.NextCursor
	}
}

// Releasing the repair continuation is what drains and closes the trace
// (R13). The stream nudges its sender on every emit, so with a healthy
// control plane the events land long before release whether or not the host
// closes the trace, and comparing the two records afterwards would prove
// nothing. So ingestion is refused from the start until CI goes green on the
// round's fix: every event, the chain's and the round's, is still buffered
// when the continuation is released, and the background sender is deep in
// its retry backoff. Close drains synchronously, and the host's release runs
// before the attempt completes, so when ClaimOnce returns the control plane
// holds exactly the JSONL raw record — with no waiting and no polling.
func TestReleasingTheRepairContinuationDrainsTheTraceBeforeTheAttemptCompletes(t *testing.T) {
	repo := initRepo(t)
	serverData, workerData := t.TempDir(), t.TempDir()
	scriptCIRepairRuntime(t)
	base, stopServe := startServe(t, serverData, "--no-github-poll")
	defer stopServe()
	submitCIRepairRun(t, base, repo)

	target, err := url.Parse(base)
	if err != nil {
		t.Fatal(err)
	}
	gate := &ingestGate{proxy: httputil.NewSingleHostReverseProxy(target)}
	proxy := httptest.NewServer(gate)
	defer proxy.Close()

	var probeErrors []string
	gateway := &e2eGateway{probe: func(_ string, red bool) {
		if red || gate.open.Load() {
			return
		}
		// CI is green on the fix: the round is over, the continuation not
		// yet released. Nothing may have reached the store yet, or the drain
		// has nothing left to prove.
		traces, _ := filepath.Glob(filepath.Join(workerData, "traces", "*.jsonl"))
		if len(traces) != 1 {
			probeErrors = append(probeErrors, fmt.Sprintf("found %d attempt traces, want 1", len(traces)))
		} else if held := attemptEvents(t, base, strings.TrimSuffix(filepath.Base(traces[0]), ".jsonl")); len(held) != 0 {
			probeErrors = append(probeErrors, fmt.Sprintf(
				"%d event(s) reached the store while ingestion was refused", len(held)))
		}
		gate.open.Store(true)
	}}

	host := startCIRepairHost(t, proxy.URL, workerData, gateway)
	attempt, err := host.ClaimOnce(context.Background())
	if err != nil || attempt == nil {
		t.Fatalf("claim: attempt=%v err=%v", attempt, err)
	}
	for _, problem := range probeErrors {
		t.Error(problem)
	}
	if attempt.State != protocol.AttemptAccepted || !gate.open.Load() {
		t.Fatalf("attempt = %s (%s), gate opened = %v: want accepted after a repair round",
			attempt.State, attempt.Error, gate.open.Load())
	}

	raw, err := os.ReadFile(filepath.Join(workerData, "traces", attempt.ID+".jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var recorded []protocol.Event
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		var event protocol.Event
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatalf("parse JSONL line %q: %v", line, err)
		}
		recorded = append(recorded, event)
	}
	held := attemptEvents(t, base, attempt.ID)
	if len(held) != len(recorded) {
		t.Fatalf("the control plane holds %d event(s), the JSONL raw record %d: releasing the continuation did not drain the trace",
			len(held), len(recorded))
	}
	repairStarted := false
	for i := range recorded {
		want, got := recorded[i], held[i]
		if got.Seq != want.Seq || got.Type != want.Type || got.Phase != want.Phase || got.Name != want.Name ||
			!sameJSON(t, got.Payload, want.Payload) {
			t.Fatalf("event %d: control plane %+v, JSONL %+v", i, got, want)
		}
		repairStarted = repairStarted || want.Name == "ci_repair_start"
	}
	if !repairStarted {
		t.Fatal("the JSONL raw record has no ci_repair_start: the round's events are not in it")
	}
}

// sameJSON compares two JSON documents by value, so the store's re-encoding
// of a payload does not read as a difference.
func sameJSON(t *testing.T, a, b json.RawMessage) bool {
	t.Helper()
	if len(a) == 0 || len(b) == 0 {
		return len(a) == len(b)
	}
	var left, right any
	if err := json.Unmarshal(a, &left); err != nil {
		t.Fatalf("decode %s: %v", a, err)
	}
	if err := json.Unmarshal(b, &right); err != nil {
		t.Fatalf("decode %s: %v", b, err)
	}
	return reflect.DeepEqual(left, right)
}
