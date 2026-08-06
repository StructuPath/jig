// serve_test.go — the U9 `jig serve` scenarios: it starts, it recovers
// occurrences interrupted mid-dispatch BEFORE it serves, it answers the API
// and the embedded UI on one loopback origin, and it refuses a non-loopback
// bind without the explicit opt-in (R20).
package main

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/StructuPath/jig/internal/controlplane"
	_ "modernc.org/sqlite"
)

// syncBuffer is a writer the command goroutine and the test both touch.
type syncBuffer struct {
	mutex   sync.Mutex
	content strings.Builder
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mutex.Lock()
	defer b.mutex.Unlock()
	return b.content.Write(p)
}

func (b *syncBuffer) String() string {
	b.mutex.Lock()
	defer b.mutex.Unlock()
	return b.content.String()
}

// startServe runs `jig serve` on an ephemeral loopback port and returns its
// base URL plus a stop function that interrupts it and asserts its exit code.
func startServe(t *testing.T, dataDir string, args ...string) (string, func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	stdout, stderr := &syncBuffer{}, &syncBuffer{}
	exited := make(chan int, 1)
	go func() {
		exited <- serveCommand(ctx,
			append([]string{"--data", dataDir, "--addr", "127.0.0.1:0"}, args...), stdout, stderr)
	}()

	var base string
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if _, found := strings.CutPrefix(stdout.String(), "jig serve: listening on "); found {
			base = strings.TrimSpace(strings.TrimPrefix(
				strings.Split(stdout.String(), "\n")[0], "jig serve: listening on "))
			break
		}
		select {
		case code := <-exited:
			t.Fatalf("jig serve exited %d before listening\nstderr:\n%s", code, stderr.String())
		default:
		}
		time.Sleep(10 * time.Millisecond)
	}
	if base == "" {
		cancel()
		t.Fatalf("jig serve never printed its listen address\nstdout:\n%s\nstderr:\n%s",
			stdout.String(), stderr.String())
	}
	return base, func() {
		cancel()
		select {
		case code := <-exited:
			if code != exitAccepted {
				t.Errorf("jig serve exited %d on interrupt\nstderr:\n%s", code, stderr.String())
			}
		case <-time.After(30 * time.Second):
			t.Error("jig serve did not shut down on interrupt")
		}
	}
}

func TestServeAnswersTheAPIAndTheEmbeddedUIOnOneLoopbackOrigin(t *testing.T) {
	base, stop := startServe(t, t.TempDir(), "--no-github-poll")
	defer stop()

	health, err := http.Get(base + "/api/health")
	if err != nil {
		t.Fatalf("GET /api/health: %v", err)
	}
	defer health.Body.Close()
	if health.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/health = %d, want 200", health.StatusCode)
	}

	// The embedded UI answers on the SAME origin: a UI on a second port
	// would fail the API's own Origin check on every state-changing request
	// (R20), so this is a contract, not a convenience.
	ui, err := http.Get(base + "/")
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	defer ui.Body.Close()
	if ui.StatusCode == http.StatusNotFound {
		t.Fatalf("GET / = 404: the embedded UI (web.Handler) is not mounted")
	}
}

// Covers R20: the refusal happens before any socket exists, so the address is
// still free afterwards.
func TestServeRefusesANonLoopbackBindWithoutTheExplicitOptIn(t *testing.T) {
	var stdout, stderr syncBuffer
	code := serveCommand(context.Background(),
		[]string{"--data", t.TempDir(), "--addr", "0.0.0.0:8383", "--no-github-poll"},
		&stdout, &stderr)
	if code != exitUsage {
		t.Fatalf("exit = %d, want %d (usage)\nstderr: %s", code, exitUsage, stderr.String())
	}
	if !strings.Contains(stderr.String(), "refusing to bind") {
		t.Fatalf("stderr does not name the refusal:\n%s", stderr.String())
	}
	if stdout.String() != "" {
		t.Fatalf("a refused bind still announced a listener:\n%s", stdout.String())
	}
}

// Covers R13's recovery half: an occurrence left in `dispatching` by a crash
// between commit and dispatch is re-driven at startup. Nothing in the
// periodic tick moves such a row — only the recovery pass does — so a serve
// that skipped it would strand this occurrence for the life of the process.
func TestServeRecoversOccurrencesInterruptedMidDispatchBeforeItServes(t *testing.T) {
	repo := initRepo(t)
	dataDir := t.TempDir()
	dbPath := filepath.Join(dataDir, "jig.db")

	store, err := controlplane.Open(context.Background(), dbPath)
	if err != nil {
		t.Fatal(err)
	}
	source, err := os.ReadFile(stockDefinition("smoke.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	definition, err := store.CreateDefinition(context.Background(),
		controlplane.DefinitionInput{Source: string(source)})
	if err != nil {
		t.Fatal(err)
	}
	trigger, err := store.CreateTrigger(context.Background(), controlplane.TriggerInput{
		Name:         "nightly",
		DefinitionID: definition.ID,
		Kind:         controlplane.TriggerSchedule,
		Config: controlplane.TriggerConfig{
			Instructions: "report on this repository",
			Cron:         "0 3 * * *",
			Timezone:     "UTC",
			Targets:      []controlplane.InvocationTarget{{Repository: repo}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	// The crash state itself: committed, reserved, never dispatched. There is
	// no API that produces it — a process that reached it did not survive to
	// report it — so the test writes the row the crash would have left.
	occurrenceID := "11111111-2222-3333-4444-555555555555"
	seedOccurrence(t, dbPath, occurrenceID, trigger.ID)

	base, stop := startServe(t, dataDir, "--no-github-poll")
	defer stop()

	// By the time the listener answers, recovery has already run: it happens
	// before Serve, not on the first tick.
	response, err := http.Get(base + "/api/health")
	if err != nil {
		t.Fatalf("GET /api/health: %v", err)
	}
	response.Body.Close()

	store, err = controlplane.Open(context.Background(), dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	occurrence, err := store.Occurrence(context.Background(), occurrenceID)
	if err != nil {
		t.Fatal(err)
	}
	if occurrence.State == controlplane.OccurrenceDispatching {
		t.Fatal("the occurrence is still `dispatching`: recovery did not run before serving")
	}
	if occurrence.State != controlplane.OccurrenceDispatched || occurrence.RunID == "" {
		t.Fatalf("occurrence state = %q run = %q, want a dispatched occurrence carrying its run",
			occurrence.State, occurrence.RunID)
	}
	if !strings.Contains(occurrence.Diagnostic, "recovered") {
		t.Errorf("occurrence diagnostic = %q, want it to say it was recovered",
			occurrence.Diagnostic)
	}
}

// seedOccurrence writes the row a crash between occurrence-commit and
// dispatch leaves behind.
func seedOccurrence(t *testing.T, dbPath, occurrenceID, triggerID string) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	now := time.Now().UnixMilli()
	if _, err := db.Exec(`
		INSERT INTO occurrences (id, trigger_id, request_key, state, source, scheduled_at,
			diagnostic, created_at, updated_at)
		VALUES (?, ?, ?, 'dispatching', ?, ?, '', ?, ?)`,
		occurrenceID, triggerID, "schedule:"+triggerID+":seeded",
		fmt.Sprintf(`{"kind":"schedule","scheduled_at":%q}`,
			time.Now().UTC().Format(time.RFC3339)),
		now, now, now); err != nil {
		t.Fatal(err)
	}
}
