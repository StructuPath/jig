package controlplane

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/StructuPath/jig/internal/protocol"
)

const workerB = "worker-b"

// registerFleetWorker registers one worker with a probed runtime, which is
// what makes a fleet row answer "which agent CLI would this worker run my
// roster on" (KTD4).
func registerFleetWorker(t *testing.T, store *Store, id, name string, capacity int) {
	t.Helper()
	_, err := store.RegisterWorker(context.Background(), id, protocol.WorkerRegistration{
		Name:          name,
		WorkerVersion: "dev",
		Capacity:      capacity,
		EnvNames:      []string{"GITHUB_TOKEN", "HOME", "PATH"},
		Runtimes: []protocol.RuntimeCapability{
			{Name: "claude-code", Version: "2.1.4", CanResume: true, ReportsCost: true},
		},
	})
	if err != nil {
		t.Fatalf("register worker %s: %v", id, err)
	}
}

// The fleet view exists to explain a queue that is not moving, so its
// liveness must be the claim transaction's liveness — not a lookalike. This
// test asserts both halves at once: the view calls the silent worker stale,
// and that worker's claim really does come back empty.
func TestFleetLivenessPredictsTheClaimTransaction(t *testing.T) {
	store, clock := newTestStore(t)
	ctx := context.Background()
	registerFleetWorker(t, store, workerA, "local", 2)
	clock.Advance(protocol.WorkerLivenessWindow + time.Second)
	registerFleetWorker(t, store, workerB, "spare", 1)

	view, err := store.Fleet(ctx)
	if err != nil {
		t.Fatalf("fleet: %v", err)
	}
	if len(view.Workers) != 2 {
		t.Fatalf("expected both workers listed, got %d", len(view.Workers))
	}
	if view.Workers[0].ID != workerA || view.Workers[1].ID != workerB {
		t.Fatalf("expected name order (local, spare), got %s then %s",
			view.Workers[0].Name, view.Workers[1].Name)
	}
	if view.Workers[0].Live {
		t.Fatalf("a worker silent past the liveness window must read stale: %+v", view.Workers[0])
	}
	if !view.Workers[1].Live {
		t.Fatalf("a worker that just registered must read live: %+v", view.Workers[1])
	}
	if view.LiveCount != 1 || view.StaleCount != 1 {
		t.Fatalf("expected 1 live and 1 stale, got %d and %d", view.LiveCount, view.StaleCount)
	}
	// Only the live worker's slot is capacity: the stale worker's two are
	// exactly the phantom the operator must not add up.
	if view.AvailableSlots != 1 {
		t.Fatalf("stale capacity must not count as available, got %d slots", view.AvailableSlots)
	}

	seedRun(t, store, "run-fleet", protocol.RunTarget{Repository: repoA, BaseSHA: strings.Repeat("a", 40)})
	if _, err := store.EnqueueJob(ctx, "run-fleet", repoA); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	claim, err := store.Claim(ctx, workerA, protocol.ClaimRequest{
		RequestID: "request-stale", LeaseToken: tokenA,
	})
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if claim != nil {
		t.Fatalf("the view said stale; the claim transaction must agree and answer empty, got %+v", claim)
	}
	// The positive control: the job really was claimable, so the emptiness
	// above was about liveness and not about an ineligible job.
	live, err := store.Claim(ctx, workerB, protocol.ClaimRequest{
		RequestID: "request-live", LeaseToken: tokenB,
	})
	if err != nil || live == nil {
		t.Fatalf("the view said live; that worker must be able to claim (claim=%+v err=%v)", live, err)
	}
}

// ActiveCount is computed from leased attempts rather than the stored
// counter, so a fleet row and the queue can never disagree about what is
// running on a worker.
func TestFleetCountsLeasedAttemptsAgainstCapacity(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()
	registerFleetWorker(t, store, workerA, "local", 2)
	seedRun(t, store, "run-busy", protocol.RunTarget{Repository: repoA, BaseSHA: strings.Repeat("a", 40)})
	claimAndStart(t, store, "run-busy", repoA)

	view, err := store.Fleet(ctx)
	if err != nil {
		t.Fatalf("fleet: %v", err)
	}
	member := view.Workers[0]
	if member.ActiveCount != 1 || member.Available != 1 {
		t.Fatalf("expected 1 of 2 slots busy, got active=%d available=%d",
			member.ActiveCount, member.Available)
	}
	if view.AvailableSlots != 1 {
		t.Fatalf("expected 1 free slot across the fleet, got %d", view.AvailableSlots)
	}
	if len(member.Runtimes) != 1 || member.Runtimes[0].Name != "claude-code" {
		t.Fatalf("a fleet row without its runtime cannot answer what would run: %+v", member.Runtimes)
	}
	if len(member.EnvNames) != 3 {
		t.Fatalf("expected the advertised env NAMES on the row, got %v", member.EnvNames)
	}
}

func TestFleetOverHTTP(t *testing.T) {
	store, _ := newTestStore(t)
	registerFleetWorker(t, store, workerA, "local", 1)
	handler := NewHandler(store, "", discardLogger())

	request := httptest.NewRequest("GET", "http://127.0.0.1:8383/api/workers", nil)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET /api/workers: expected 200, got %d (%s)", recorder.Code, recorder.Body.String())
	}
	var view FleetView
	if err := json.Unmarshal(recorder.Body.Bytes(), &view); err != nil {
		t.Fatalf("decode fleet: %v", err)
	}
	if view.LiveCount != 1 || len(view.Workers) != 1 {
		t.Fatalf("expected one live worker, got %+v", view)
	}
	if !view.Workers[0].Live || view.Workers[0].Name != "local" {
		t.Fatalf("the flattened worker record must survive the wire: %+v", view.Workers[0])
	}
	if view.ObservedAt.IsZero() {
		t.Fatalf("a polled view without its observation time cannot be read as stale")
	}
	// An empty fleet is a real answer, not a null the UI has to defend
	// against: a control plane with no worker is exactly when this is read.
	empty, _ := newTestStore(t)
	recorder = httptest.NewRecorder()
	NewHandler(empty, "", discardLogger()).ServeHTTP(recorder,
		httptest.NewRequest("GET", "http://127.0.0.1:8383/api/workers", nil))
	if body := strings.TrimSpace(recorder.Body.String()); !strings.Contains(body, `"workers":[]`) {
		t.Fatalf("an empty fleet must serialize as an empty array, got %s", body)
	}
}
