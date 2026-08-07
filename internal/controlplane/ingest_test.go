// ingest_test.go — U8's server-side contract: replay after an ingest outage
// lands exactly once in seq order, cursors never interleave across the
// attempts of one job, evidence projection is idempotent, and the read
// surface answers a non-browser caller.
package controlplane

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/StructuPath/jig/internal/protocol"
)

// leasedAttempt seeds a run, enqueues one job, and claims it, returning the
// leased attempt and the token that fences its writes.
func leasedAttempt(t *testing.T, store *Store, runID, requestID, token string) protocol.Attempt {
	t.Helper()
	seedRun(t, store, runID, protocol.RunTarget{Repository: repoA, BaseSHA: strings.Repeat("a", 40)})
	if _, err := store.EnqueueJob(context.Background(), runID, repoA); err != nil {
		t.Fatalf("enqueue job: %v", err)
	}
	claim := mustClaim(t, store, requestID, token)
	return claim.Attempt
}

func traceEvent(seq int64, eventType, phase, name string, payload any) protocol.Event {
	body, err := json.Marshal(payload)
	if err != nil {
		panic(err)
	}
	if payload == nil {
		body = nil
	}
	return protocol.Event{Seq: seq, Type: eventType, Phase: phase, Name: name, Payload: body}
}

func ingest(t *testing.T, store *Store, attemptID, token string, events ...protocol.Event) protocol.EventBatchResult {
	t.Helper()
	result, err := store.IngestEvents(context.Background(), attemptID,
		protocol.EventBatch{LeaseToken: token, Events: events})
	if err != nil {
		t.Fatalf("ingest events: %v", err)
	}
	return result
}

// Plan scenario: events replayed after an ingest outage land exactly once in
// seq order, and a UI cursor that advanced during the outage still renders
// the replayed events in order.
func TestIngestReplayAfterOutageLandsExactlyOnceInSeqOrder(t *testing.T) {
	store, _ := newTestStore(t)
	registerTestWorker(t, store, 2)
	attempt := leasedAttempt(t, store, "run-replay", "request-replay", tokenA)
	ctx := context.Background()

	// Before the outage: the first three events land and the UI's cursor
	// advances over them.
	ingest(t, store, attempt.ID, tokenA,
		traceEvent(1, protocol.EventPhaseStart, "plan", "", map[string]any{"kind": "agent"}),
		traceEvent(2, protocol.EventAgentStart, "plan", "writer", nil),
		traceEvent(3, protocol.EventToolCall, "plan", "Write", map[string]any{"path": "PLAN.md"}))

	page, err := store.AttemptEventPage(ctx, attempt.ID, 0, 100)
	if err != nil {
		t.Fatalf("read page: %v", err)
	}
	if page.NextCursor != 3 || len(page.Events) != 3 {
		t.Fatalf("expected 3 events and cursor 3, got %d events and cursor %d",
			len(page.Events), page.NextCursor)
	}
	cursor := page.NextCursor

	// The outage: the worker buffered 4-8 and replays them together with the
	// batch it could not confirm (2-3), exactly as the send buffer would.
	replay := []protocol.Event{
		traceEvent(2, protocol.EventAgentStart, "plan", "writer", nil),
		traceEvent(3, protocol.EventToolCall, "plan", "Write", map[string]any{"path": "PLAN.md"}),
		traceEvent(4, protocol.EventToolCall, "plan", "Read", map[string]any{"path": "README.md"}),
		traceEvent(5, protocol.EventHandoff, "plan", "writer", map[string]any{"summary": "planned"}),
		traceEvent(6, protocol.EventPhaseEnd, "plan", "", map[string]any{"status": "success"}),
		traceEvent(7, protocol.EventPhaseStart, "build", "", map[string]any{"kind": "code"}),
		traceEvent(8, protocol.EventPhaseEnd, "build", "", map[string]any{"status": "success"}),
	}
	result := ingest(t, store, attempt.ID, tokenA, replay...)
	if result.Accepted != 5 {
		t.Fatalf("expected the 5 unseen events to be accepted (2 and 3 are replays), got %d", result.Accepted)
	}
	if result.HighestSeq != 8 {
		t.Fatalf("expected highest seq 8, got %d", result.HighestSeq)
	}
	// Replaying the whole batch a second time changes nothing.
	if again := ingest(t, store, attempt.ID, tokenA, replay...); again.Accepted != 0 {
		t.Fatalf("a second replay accepted %d rows; INSERT OR IGNORE must make it a no-op", again.Accepted)
	}

	// The cursor that advanced during the outage still sees every replayed
	// event, once each, in seq order.
	after, err := store.AttemptEventPage(ctx, attempt.ID, cursor, 100)
	if err != nil {
		t.Fatalf("read page after cursor: %v", err)
	}
	var seqs []int64
	for _, event := range after.Events {
		seqs = append(seqs, event.Seq)
	}
	if len(seqs) != 5 {
		t.Fatalf("expected 5 events after cursor %d, got %v", cursor, seqs)
	}
	for index, want := range []int64{4, 5, 6, 7, 8} {
		if seqs[index] != want {
			t.Fatalf("events after the cursor are out of order: %v", seqs)
		}
	}
	if after.NextCursor != 8 {
		t.Fatalf("expected the cursor to end at 8, got %d", after.NextCursor)
	}

	// And the whole trace holds each seq exactly once.
	full, err := store.AttemptEventPage(ctx, attempt.ID, 0, 100)
	if err != nil {
		t.Fatalf("read full trace: %v", err)
	}
	if len(full.Events) != 8 {
		t.Fatalf("expected 8 stored events, got %d", len(full.Events))
	}
	for index, event := range full.Events {
		if event.Seq != int64(index+1) {
			t.Fatalf("stored trace is not seq-ordered without duplicates: %+v", full.Events)
		}
	}
}

// Plan scenario (R15): attempt 2 of a retried job is its own lane, attempt 1
// stays inspectable, and the cursors never interleave.
func TestAttemptCursorsNeverInterleaveAcrossAttempts(t *testing.T) {
	store, _ := newTestStore(t)
	registerTestWorker(t, store, 2)
	ctx := context.Background()
	first := leasedAttempt(t, store, "run-lanes", "request-lane-1", tokenA)

	ingest(t, store, first.ID, tokenA,
		traceEvent(1, protocol.EventPhaseStart, "plan", "", nil),
		traceEvent(2, protocol.EventError, "plan", "engine_error", map[string]any{"error": "boom"}))
	if _, err := store.CompleteAttempt(ctx, first.ID, protocol.CompleteAttemptRequest{
		LeaseToken: tokenA, State: protocol.AttemptFailed, Error: "boom",
	}); err != nil {
		t.Fatalf("complete attempt 1: %v", err)
	}
	if _, err := store.RetryJob(ctx, first.JobID); err != nil {
		t.Fatalf("retry job: %v", err)
	}
	claim := mustClaim(t, store, "request-lane-2", tokenB)
	second := claim.Attempt
	if second.AttemptNumber != 2 {
		t.Fatalf("expected attempt 2, got %d", second.AttemptNumber)
	}

	// Attempt 2's seq restarts at 1 — the sequence is per-attempt.
	ingest(t, store, second.ID, tokenB,
		traceEvent(1, protocol.EventPhaseStart, "plan", "", nil),
		traceEvent(2, protocol.EventPhaseEnd, "plan", "", map[string]any{"status": "success"}),
		traceEvent(3, protocol.EventLog, "", "acceptance", map[string]any{"passed": true}))

	firstPage, err := store.AttemptEventPage(ctx, first.ID, 0, 100)
	if err != nil {
		t.Fatalf("read attempt 1: %v", err)
	}
	if len(firstPage.Events) != 2 {
		t.Fatalf("attempt 1's history must remain inspectable; got %d events", len(firstPage.Events))
	}
	secondPage, err := store.AttemptEventPage(ctx, second.ID, 0, 100)
	if err != nil {
		t.Fatalf("read attempt 2: %v", err)
	}
	if len(secondPage.Events) != 3 {
		t.Fatalf("attempt 2 must have its own lane; got %d events", len(secondPage.Events))
	}
	// A cursor advanced on one lane never suppresses the other lane's events.
	tail, err := store.AttemptEventPage(ctx, second.ID, 2, 100)
	if err != nil {
		t.Fatalf("read attempt 2 tail: %v", err)
	}
	if len(tail.Events) != 1 || tail.Events[0].Seq != 3 {
		t.Fatalf("expected only seq 3 after cursor 2, got %+v", tail.Events)
	}
	if again, err := store.AttemptEventPage(ctx, first.ID, 2, 100); err != nil {
		t.Fatalf("read attempt 1 tail: %v", err)
	} else if len(again.Events) != 0 || again.NextCursor != 2 {
		t.Fatalf("attempt 1's cursor must stay put at its end, got %+v cursor %d", again.Events, again.NextCursor)
	}

	detail, err := store.JobDetail(ctx, first.JobID)
	if err != nil {
		t.Fatalf("job detail: %v", err)
	}
	if len(detail.Attempts) != 2 {
		t.Fatalf("the job must expose both lanes, got %d attempts", len(detail.Attempts))
	}
	if detail.Attempts[0].AttemptNumber != 1 || detail.Attempts[1].AttemptNumber != 2 {
		t.Fatalf("attempts must be oldest-first: %+v", detail.Attempts)
	}
}

// A running attempt's tool calls are readable before any envelope exists —
// the live view the operator watches (plan scenario 3, server half).
func TestRunningAttemptExposesToolCallsBeforeAnyEnvelope(t *testing.T) {
	store, _ := newTestStore(t)
	registerTestWorker(t, store, 1)
	attempt := leasedAttempt(t, store, "run-live", "request-live", tokenA)
	ctx := context.Background()
	if _, err := store.StartAttempt(ctx, attempt.ID, protocol.StartAttemptRequest{
		LeaseToken: tokenA, RuntimeName: "claude-code", RuntimeVersion: "1.2.3",
	}); err != nil {
		t.Fatalf("start attempt: %v", err)
	}
	ingest(t, store, attempt.ID, tokenA,
		traceEvent(1, protocol.EventPhaseStart, "plan", "", nil),
		traceEvent(2, protocol.EventAgentStart, "plan", "writer", nil),
		traceEvent(3, protocol.EventToolCall, "plan", "Read", map[string]any{"path": "README.md"}),
		traceEvent(4, protocol.EventToolCall, "plan", "Write", map[string]any{"path": "PLAN.md"}))

	live, err := store.Attempt(ctx, attempt.ID)
	if err != nil {
		t.Fatalf("read attempt: %v", err)
	}
	if live.State != protocol.AttemptRunning || live.Result != "" {
		t.Fatalf("expected a running attempt with no result yet, got %s / %q", live.State, live.Result)
	}
	page, err := store.AttemptEventPage(ctx, attempt.ID, 0, 100)
	if err != nil {
		t.Fatalf("read page: %v", err)
	}
	calls := 0
	for _, event := range page.Events {
		if event.Type == protocol.EventToolCall {
			calls++
		}
	}
	if calls != 2 {
		t.Fatalf("expected 2 live tool calls before any envelope, got %d", calls)
	}
	envelopes, err := store.AttemptEnvelopes(ctx, attempt.ID)
	if err != nil {
		t.Fatalf("read envelopes: %v", err)
	}
	if len(envelopes) != 0 {
		t.Fatalf("no envelope exists yet; got %d", len(envelopes))
	}
}

// Evidence projection: gate checks and invalid envelopes become queryable
// rows exactly once, and an invalid envelope is size-capped in the store.
func TestIngestProjectsEvidenceIdempotently(t *testing.T) {
	store, _ := newTestStore(t)
	registerTestWorker(t, store, 1)
	attempt := leasedAttempt(t, store, "run-evidence", "request-evidence", tokenA)
	ctx := context.Background()

	oversize := strings.Repeat("x", protocol.MaxInvalidEnvelopeBytes+4096)
	batch := []protocol.Event{
		traceEvent(1, protocol.EventGatePass, "plan", "artifacts_exist", map[string]any{
			"attempt": 1,
			"checks": []protocol.GateCheck{
				{Item: "PLAN.md", Ok: true, Note: "exists, 2.1KB"},
				{Item: "NOTES.md", Ok: true, Note: "exists, 0.4KB"},
			},
		}),
		traceEvent(2, protocol.EventLog, "plan", "invalid_envelope", map[string]any{
			"role": "writer", "parse_attempt": 1, "error": "no JSON object found", "raw": oversize,
		}),
		traceEvent(3, protocol.EventGateFail, "plan", "files_non_empty", map[string]any{
			"attempt": 2,
			"checks":  []protocol.GateCheck{{Item: "PLAN.md", Ok: false, Note: "empty"}},
		}),
	}
	ingest(t, store, attempt.ID, tokenA, batch...)
	ingest(t, store, attempt.ID, tokenA, batch...)

	evidence, err := store.AttemptGateEvidence(ctx, attempt.ID)
	if err != nil {
		t.Fatalf("read gate evidence: %v", err)
	}
	if len(evidence) != 3 {
		t.Fatalf("expected 3 gate checks after a replayed batch, got %d", len(evidence))
	}
	if evidence[0].Gate != "artifacts_exist" || !evidence[0].Ok || evidence[0].Note == "" {
		t.Fatalf("a green gate must say what it checked: %+v", evidence[0])
	}
	if evidence[2].Ok || evidence[2].Emission != 2 {
		t.Fatalf("expected the failed check of emission 2, got %+v", evidence[2])
	}

	envelopes, err := store.AttemptEnvelopes(ctx, attempt.ID)
	if err != nil {
		t.Fatalf("read envelopes: %v", err)
	}
	if len(envelopes) != 1 {
		t.Fatalf("expected one invalid-envelope row after a replayed batch, got %d", len(envelopes))
	}
	if envelopes[0].Valid {
		t.Fatalf("the emission did not parse; it must be stored invalid")
	}
	if len(envelopes[0].Body) != protocol.MaxInvalidEnvelopeBytes {
		t.Fatalf("invalid-envelope rows must be size-capped at %d, got %d",
			protocol.MaxInvalidEnvelopeBytes, len(envelopes[0].Body))
	}
	if envelopes[0].ParseError == "" {
		t.Fatalf("the parse error is the whole point of the row")
	}
}

// The fence (R6): only the token that owns the attempt may append to its
// trace, and the batch bound is enforced.
func TestIngestIsFencedAndBounded(t *testing.T) {
	store, _ := newTestStore(t)
	registerTestWorker(t, store, 1)
	attempt := leasedAttempt(t, store, "run-fence", "request-fence", tokenA)
	ctx := context.Background()

	_, err := store.IngestEvents(ctx, attempt.ID, protocol.EventBatch{
		LeaseToken: tokenB,
		Events:     []protocol.Event{traceEvent(1, protocol.EventLog, "", "hello", nil)},
	})
	if code := serviceCode(t, err); code != "lease_not_owner" {
		t.Fatalf("expected lease_not_owner for a foreign token, got %s", code)
	}

	_, err = store.IngestEvents(ctx, "no-such-attempt", protocol.EventBatch{
		LeaseToken: tokenA,
		Events:     []protocol.Event{traceEvent(1, protocol.EventLog, "", "hello", nil)},
	})
	if code := serviceCode(t, err); code != "not_found" {
		t.Fatalf("expected not_found for an unknown attempt, got %s", code)
	}

	oversized := make([]protocol.Event, protocol.MaxEventsPerBatch+1)
	for index := range oversized {
		oversized[index] = traceEvent(int64(index+1), protocol.EventLog, "", "spam", nil)
	}
	_, err = store.IngestEvents(ctx, attempt.ID, protocol.EventBatch{LeaseToken: tokenA, Events: oversized})
	if code := serviceCode(t, err); code != "batch_too_large" {
		t.Fatalf("expected batch_too_large, got %s", code)
	}

	_, err = store.IngestEvents(ctx, attempt.ID, protocol.EventBatch{
		LeaseToken: tokenA,
		Events:     []protocol.Event{{Seq: 1, Type: ""}},
	})
	if code := serviceCode(t, err); code != "invalid_event_type" {
		t.Fatalf("expected invalid_event_type, got %s", code)
	}
}

// The tail of a trace still lands after the attempt went terminal: a
// completed, cancelled, or swept attempt's last events are exactly the
// record R15 exists to keep.
func TestIngestAcceptsTheTailOfATerminalAttempt(t *testing.T) {
	store, _ := newTestStore(t)
	registerTestWorker(t, store, 1)
	attempt := leasedAttempt(t, store, "run-tail", "request-tail", tokenA)
	ctx := context.Background()
	if _, err := store.StartAttempt(ctx, attempt.ID,
		protocol.StartAttemptRequest{LeaseToken: tokenA}); err != nil {
		t.Fatalf("start attempt: %v", err)
	}
	if _, err := store.CompleteAttempt(ctx, attempt.ID, protocol.CompleteAttemptRequest{
		LeaseToken: tokenA, State: protocol.AttemptFailed, Error: "phase failed",
	}); err != nil {
		t.Fatalf("complete attempt: %v", err)
	}
	result := ingest(t, store, attempt.ID, tokenA,
		traceEvent(9, protocol.EventPhaseEnd, "build", "", map[string]any{"status": "fail"}))
	if result.Accepted != 1 {
		t.Fatalf("expected the terminal tail to land, accepted %d", result.Accepted)
	}
}

// Queue depth is the number an operator wants first: what is waiting, not
// what is running.
func TestQueueReportsDepthAndTheActiveAttempt(t *testing.T) {
	store, _ := newTestStore(t)
	registerTestWorker(t, store, 1)
	ctx := context.Background()
	seedRun(t, store, "run-queue",
		protocol.RunTarget{Repository: repoA, BaseSHA: strings.Repeat("a", 40)},
		protocol.RunTarget{Repository: repoB, BaseSHA: strings.Repeat("b", 40)})
	if _, err := store.EnqueueJob(ctx, "run-queue", repoA); err != nil {
		t.Fatalf("enqueue job a: %v", err)
	}
	if _, err := store.EnqueueJob(ctx, "run-queue", repoB); err != nil {
		t.Fatalf("enqueue job b: %v", err)
	}
	claim := mustClaim(t, store, "request-queue", tokenA)

	view, err := store.Queue(ctx)
	if err != nil {
		t.Fatalf("queue: %v", err)
	}
	if view.Depth != 1 {
		t.Fatalf("expected queue depth 1 (one job still waiting), got %d", view.Depth)
	}
	if view.ActiveCount != 1 || len(view.Entries) != 2 {
		t.Fatalf("expected 1 active and 2 entries, got %d active and %d entries",
			view.ActiveCount, len(view.Entries))
	}
	if view.Entries[0].State != protocol.JobActive || view.Entries[0].Attempt == nil {
		t.Fatalf("the active entry must carry its attempt: %+v", view.Entries[0])
	}
	if view.Entries[0].Attempt.ID != claim.Attempt.ID {
		t.Fatalf("wrong attempt on the active entry: %+v", view.Entries[0].Attempt)
	}
	if view.Entries[0].Definition == "" {
		t.Fatalf("a queue entry without its definition name is not operable: %+v", view.Entries[0])
	}
	if view.Entries[1].Position != 1 {
		t.Fatalf("the waiting job must carry its FIFO position, got %d", view.Entries[1].Position)
	}
}

// The read surface answers a non-browser caller: plain GETs, JSON out, a
// named error for a malformed cursor. Ingestion keeps the Origin fence.
func TestReadSurfaceOverHTTP(t *testing.T) {
	store, _ := newTestStore(t)
	registerTestWorker(t, store, 1)
	attempt := leasedAttempt(t, store, "run-http", "request-http", tokenA)
	ingest(t, store, attempt.ID, tokenA,
		traceEvent(1, protocol.EventPhaseStart, "plan", "", nil),
		traceEvent(2, protocol.EventToolCall, "plan", "Write", map[string]any{"path": "PLAN.md"}))
	handler := NewHandler(store, "", discardLogger())

	get := func(path string) *httptest.ResponseRecorder {
		request := httptest.NewRequest("GET", "http://127.0.0.1:8383"+path, nil)
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		return recorder
	}

	for _, path := range []string{
		"/api/queue",
		"/api/worktrees",
		"/api/jobs/" + attempt.JobID,
		"/api/attempts/" + attempt.ID,
		"/api/attempts/" + attempt.ID + "/gates",
		"/api/attempts/" + attempt.ID + "/envelopes",
	} {
		if recorder := get(path); recorder.Code != http.StatusOK {
			t.Fatalf("GET %s: expected 200, got %d (%s)", path, recorder.Code, recorder.Body.String())
		}
	}

	recorder := get("/api/attempts/" + attempt.ID + "/events?after=1&limit=10")
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET events: expected 200, got %d (%s)", recorder.Code, recorder.Body.String())
	}
	var page protocol.EventPage
	if err := json.Unmarshal(recorder.Body.Bytes(), &page); err != nil {
		t.Fatalf("decode event page: %v", err)
	}
	if len(page.Events) != 1 || page.Events[0].Seq != 2 || page.NextCursor != 2 {
		t.Fatalf("expected only seq 2 after the cursor, got %+v", page)
	}

	if recorder := get("/api/attempts/" + attempt.ID + "/events?after=banana"); recorder.Code != http.StatusBadRequest {
		t.Fatalf("a malformed cursor must be a named 400, got %d", recorder.Code)
	}
	if recorder := get("/api/attempts/missing"); recorder.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for an unknown attempt, got %d", recorder.Code)
	}

	// Ingestion is state-changing, so it keeps the browser-origin fence (R20).
	body := `{"lease_token":"` + tokenA + `","events":[{"seq":3,"type":"log"}]}`
	request := httptest.NewRequest("POST", "http://127.0.0.1:8383/api/attempts/"+attempt.ID+"/events",
		strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", "http://evil.test")
	cross := httptest.NewRecorder()
	handler.ServeHTTP(cross, request)
	if cross.Code != http.StatusForbidden {
		t.Fatalf("expected a cross-origin ingest to be refused, got %d", cross.Code)
	}

	request = httptest.NewRequest("POST", "http://127.0.0.1:8383/api/attempts/"+attempt.ID+"/events",
		strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	same := httptest.NewRecorder()
	handler.ServeHTTP(same, request)
	if same.Code != http.StatusOK {
		t.Fatalf("expected the worker's originless ingest to succeed, got %d (%s)",
			same.Code, same.Body.String())
	}
}
