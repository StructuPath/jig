// runs_request_id_test.go — admission that a client can repeat. A run's
// answer can be lost after it commits (ref resolution makes admission slow),
// so a repeat carrying the same key must get that run back, never a second.
package controlplane

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func postRun(t *testing.T, handler http.Handler, path string, body []byte, key string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest("POST", "http://127.0.0.1:8383"+path, bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	if key != "" {
		request.Header.Set("Idempotency-Key", key)
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

func countRuns(t *testing.T, store *Store) int {
	t.Helper()
	var runs int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM runs`).Scan(&runs); err != nil {
		t.Fatal(err)
	}
	return runs
}

func TestARepeatedInvocationWithTheSameRequestIDReturnsTheFirstRun(t *testing.T) {
	store, _ := newTriggerStore(t)
	handler := NewHandler(store, "", nil)
	definition := mustCreateDefinition(t, store, u5Definition)
	invocation := RunInvocation{
		RequestID:    "7f6c1e9a-request-1",
		DefinitionID: definition.ID,
		Instructions: "explain this repository",
		Targets:      []InvocationTarget{{Repository: "github.com/example/one"}},
	}
	body, _ := json.Marshal(invocation)

	first := postRun(t, handler, "/api/runs", body, "")
	if first.Code != http.StatusCreated {
		t.Fatalf("first invoke: %d %s", first.Code, first.Body.String())
	}
	var firstView, repeatView RunView
	_ = json.Unmarshal(first.Body.Bytes(), &firstView)

	// The same key, once in the body and once as the header, is one request.
	repeat := postRun(t, handler, "/api/runs", body, invocation.RequestID)
	if repeat.Code != http.StatusCreated {
		t.Fatalf("repeat invoke: %d %s", repeat.Code, repeat.Body.String())
	}
	_ = json.Unmarshal(repeat.Body.Bytes(), &repeatView)
	if repeatView.Run.ID != firstView.Run.ID || len(repeatView.Jobs) != 1 ||
		repeatView.Jobs[0].ID != firstView.Jobs[0].ID {
		t.Fatalf("repeat answered run %+v, want the first run %+v", repeatView, firstView)
	}
	if runs := countRuns(t, store); runs != 1 {
		t.Fatalf("store holds %d runs after a keyed repeat, want 1", runs)
	}

	// The same key on a different request is refused, not answered with a
	// run it did not ask for.
	different := invocation
	different.Instructions = "something else entirely"
	body, _ = json.Marshal(different)
	if response := postRun(t, handler, "/api/runs", body, ""); response.Code != http.StatusConflict ||
		!bytes.Contains(response.Body.Bytes(), []byte("request_id_conflict")) {
		t.Fatalf("reused key on a different request: %d %s, want 409 request_id_conflict",
			response.Code, response.Body.String())
	}
	// Body and header naming different keys is a client bug.
	if response := postRun(t, handler, "/api/runs", body, "another-key"); response.Code != http.StatusBadRequest {
		t.Fatalf("disagreeing keys: %d %s, want 400", response.Code, response.Body.String())
	}
	// Without a key, admission is as before: every request is a run.
	invocation.RequestID = ""
	body, _ = json.Marshal(invocation)
	postRun(t, handler, "/api/runs", body, "")
	if runs := countRuns(t, store); runs != 2 {
		t.Fatalf("store holds %d runs, want 2 after one unkeyed invocation", runs)
	}

	// Re-admission takes the key as a header and replays the same way.
	path := "/api/runs/" + firstView.Run.ID + "/readmit"
	readmit := postRun(t, handler, path, nil, "readmit-1")
	again := postRun(t, handler, path, nil, "readmit-1")
	var readmitView, againView RunView
	_ = json.Unmarshal(readmit.Body.Bytes(), &readmitView)
	_ = json.Unmarshal(again.Body.Bytes(), &againView)
	if readmit.Code != http.StatusCreated || again.Code != http.StatusCreated ||
		readmitView.Run.ID == "" || againView.Run.ID != readmitView.Run.ID {
		t.Fatalf("keyed readmit repeat: %d %s / %d %s", readmit.Code, readmit.Body.String(),
			again.Code, again.Body.String())
	}
	if runs := countRuns(t, store); runs != 3 {
		t.Fatalf("store holds %d runs, want 3 after one keyed readmit sent twice", runs)
	}
}

// Two requests with one key racing through ref resolution: the second to
// commit meets the unique key and answers with the first's run.
func TestConcurrentInvocationsWithOneRequestIDAdmitOneRun(t *testing.T) {
	store, _ := newTriggerStore(t)
	definition := mustCreateDefinition(t, store, u5Definition)
	release := make(chan struct{})
	store.resolveRef = func(context.Context, string, string) (string, error) {
		<-release
		return pinnedSHA, nil
	}
	invocation := RunInvocation{
		RequestID: "race-1", DefinitionID: definition.ID, Instructions: "x",
		Targets: []InvocationTarget{{Repository: "github.com/example/one"}},
	}
	views := make(chan RunView, 2)
	errs := make(chan error, 2)
	for range 2 {
		go func() {
			view, err := store.InvokeDefinition(context.Background(), invocation)
			views <- view
			errs <- err
		}()
	}
	time.Sleep(50 * time.Millisecond)
	close(release)
	first, second := <-views, <-views
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatalf("keyed concurrent invoke: %v", err)
		}
	}
	if first.Run.ID == "" || first.Run.ID != second.Run.ID {
		t.Fatalf("concurrent keyed invocations answered %q and %q, want one run", first.Run.ID, second.Run.ID)
	}
	if runs := countRuns(t, store); runs != 1 {
		t.Fatalf("store holds %d runs, want 1", runs)
	}
}

// The listener's write timeout is far shorter than ref resolution may take.
// An admitting request must lift it, or the run commits and its answer is
// dropped.
func TestAnAdmissionSlowerThanTheWriteTimeoutStillAnswers(t *testing.T) {
	store, _ := newTriggerStore(t)
	definition := mustCreateDefinition(t, store, u5Definition)
	store.resolveRef = func(context.Context, string, string) (string, error) {
		time.Sleep(400 * time.Millisecond)
		return pinnedSHA, nil
	}
	server := httptest.NewUnstartedServer(NewHandler(store, "", nil))
	server.Config.WriteTimeout = 100 * time.Millisecond
	server.Start()
	t.Cleanup(server.Close)

	body, _ := json.Marshal(RunInvocation{
		DefinitionID: definition.ID, Instructions: "x",
		Targets: []InvocationTarget{{Repository: "github.com/example/one"}},
	})
	response, err := http.Post(server.URL+"/api/runs", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("an admission slower than the write timeout lost its answer: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201", response.StatusCode)
	}
}
