// events_test.go — U8's worker-side contract: redaction before anything
// persists (R15), a raw JSONL record that says redaction happened, buffered
// replay in strict seq order across an ingest outage (KTD8), and an
// explicitly bounded buffer whose overflow is visible rather than silent.
package worker

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/StructuPath/jig/internal/protocol"
)

const traceSnapshot = `name: trace-fixture
roster:
  writer:
    model: claude-sonnet
    system_prompt: write
    user_prompt: write it
    env: [ANTHROPIC_API_KEY]
    sensitive_env: [ANTHROPIC_API_KEY]
phases:
  - name: plan
    kind: agent
    owner: writer
`

// fakeIngester records the batches a stream delivers and can be switched
// offline to simulate an ingest outage.
type fakeIngester struct {
	mutex   sync.Mutex
	offline bool
	batches [][]protocol.Event
	calls   int
}

func (f *fakeIngester) IngestEvents(
	_ context.Context, _ string, batch protocol.EventBatch,
) (protocol.EventBatchResult, error) {
	f.mutex.Lock()
	defer f.mutex.Unlock()
	f.calls++
	if f.offline {
		return protocol.EventBatchResult{}, errors.New("control plane is unreachable")
	}
	f.batches = append(f.batches, append([]protocol.Event(nil), batch.Events...))
	return protocol.EventBatchResult{Accepted: len(batch.Events)}, nil
}

func (f *fakeIngester) setOffline(offline bool) {
	f.mutex.Lock()
	defer f.mutex.Unlock()
	f.offline = offline
}

// delivered flattens every event the ingester accepted, in arrival order.
func (f *fakeIngester) delivered() []protocol.Event {
	f.mutex.Lock()
	defer f.mutex.Unlock()
	var events []protocol.Event
	for _, batch := range f.batches {
		events = append(events, batch...)
	}
	return events
}

// discardLogger throws its output away. It must be io.Discard and not
// os.NewFile(0, os.DevNull): that call does not open /dev/null, it wraps file
// descriptor 0 and merely names it. When the test binary runs with stdin
// closed, fd 0 is free, the trace file opened next takes it, and the "discard"
// logger writes slog text straight into the JSONL under test.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func newTraceFixture(t *testing.T, ingest EventIngester, maxBuffered int) (*TraceStream, string) {
	t.Helper()
	spec, err := protocol.ParseDefinition([]byte(traceSnapshot))
	if err != nil {
		t.Fatalf("parse fixture definition: %v", err)
	}
	path := filepath.Join(t.TempDir(), "attempt.jsonl")
	stream, err := NewTraceStream(TraceStreamConfig{
		AttemptID:     "attempt-1",
		LeaseToken:    strings.Repeat("a", 64),
		TracePath:     path,
		Ingest:        ingest,
		Spec:          spec,
		Environ:       []string{"ANTHROPIC_API_KEY=sk-ant-super-secret-value", "PATH=/usr/bin"},
		Logger:        discardLogger(),
		FlushInterval: 5 * time.Millisecond,
		MaxBuffered:   maxBuffered,
	})
	if err != nil {
		t.Fatalf("open trace stream: %v", err)
	}
	return stream, path
}

// readTrace parses the attempt-local JSONL raw record.
func readTrace(t *testing.T, path string) []map[string]any {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatalf("open trace: %v", err)
	}
	defer file.Close()
	var records []map[string]any
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for scanner.Scan() {
		var record map[string]any
		if err := json.Unmarshal(scanner.Bytes(), &record); err != nil {
			t.Fatalf("trace line is not JSON: %v (%s)", err, scanner.Text())
		}
		records = append(records, record)
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("read trace: %v", err)
	}
	return records
}

func event(seq int64, eventType, name string, payload any) protocol.Event {
	body, err := json.Marshal(payload)
	if err != nil {
		panic(err)
	}
	if payload == nil {
		body = nil
	}
	return protocol.Event{Seq: seq, Type: eventType, Phase: "plan", Name: name, Payload: body}
}

// Plan scenario (R15): a tool-call payload carrying a value from the role's
// sensitive env names is redacted before it leaves the process, and the JSONL
// raw record notes that redaction occurred.
func TestSensitiveValuesAreRedactedBeforeTheEventLeavesTheProcess(t *testing.T) {
	ingester := &fakeIngester{}
	stream, path := newTraceFixture(t, ingester, 0)

	if err := stream.Emit(event(1, protocol.EventToolCall, "Bash", map[string]any{
		"command": "curl -H 'authorization: Bearer sk-ant-super-secret-value' https://api.test",
		"cwd":     "/w",
	})); err != nil {
		t.Fatalf("emit: %v", err)
	}
	if err := stream.Emit(event(2, protocol.EventLog, "agent_text", map[string]any{
		"text": "I will use sk-ant-super-secret-value now",
	})); err != nil {
		t.Fatalf("emit: %v", err)
	}
	if err := stream.Close(context.Background()); err != nil {
		t.Fatalf("close stream: %v", err)
	}

	for _, delivered := range ingester.delivered() {
		body := string(delivered.Payload)
		if strings.Contains(body, "sk-ant-super-secret-value") {
			t.Fatalf("a sensitive value reached the control plane: %s", body)
		}
		if !strings.Contains(body, "[redacted:ANTHROPIC_API_KEY]") {
			t.Fatalf("expected the redaction placeholder in %s", body)
		}
		// The payload must still be valid JSON after replacement.
		var decoded map[string]any
		if err := json.Unmarshal(delivered.Payload, &decoded); err != nil {
			t.Fatalf("redaction broke the payload JSON: %v (%s)", err, body)
		}
	}

	records := readTrace(t, path)
	if len(records) != 2 {
		t.Fatalf("expected 2 raw records, got %d", len(records))
	}
	for _, record := range records {
		encoded, err := json.Marshal(record)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(encoded), "sk-ant-super-secret-value") {
			t.Fatalf("the JSONL raw record still holds the secret: %s", encoded)
		}
		names, ok := record["redacted"].([]any)
		if !ok || len(names) != 1 || names[0] != "ANTHROPIC_API_KEY" {
			t.Fatalf("the raw record must note which sensitive names were redacted: %s", encoded)
		}
	}
}

// An event with nothing sensitive in it carries no redaction note: the note's
// presence is the signal, so it must never be noise.
func TestUnaffectedEventsCarryNoRedactionNote(t *testing.T) {
	ingester := &fakeIngester{}
	stream, path := newTraceFixture(t, ingester, 0)
	if err := stream.Emit(event(1, protocol.EventToolCall, "Read", map[string]any{
		"path": "README.md",
	})); err != nil {
		t.Fatalf("emit: %v", err)
	}
	if err := stream.Close(context.Background()); err != nil {
		t.Fatalf("close: %v", err)
	}
	records := readTrace(t, path)
	if len(records) != 1 {
		t.Fatalf("expected 1 record, got %d", len(records))
	}
	if _, present := records[0]["redacted"]; present {
		t.Fatalf("an unredacted event must not claim redaction: %+v", records[0])
	}
}

// Plan scenario: events buffered during an ingest outage are replayed, in
// strict seq order, and delivery never skips ahead — which is what makes a UI
// cursor safe, because the cursor can never advance past an undelivered seq.
func TestBufferedEventsReplayInSeqOrderAfterAnOutage(t *testing.T) {
	ingester := &fakeIngester{}
	stream, path := newTraceFixture(t, ingester, 0)

	if err := stream.Emit(event(1, protocol.EventPhaseStart, "", nil)); err != nil {
		t.Fatalf("emit: %v", err)
	}
	if err := stream.Flush(context.Background()); err != nil {
		t.Fatalf("flush: %v", err)
	}

	ingester.setOffline(true)
	for seq := int64(2); seq <= 12; seq++ {
		if err := stream.Emit(event(seq, protocol.EventToolCall, "Write", map[string]any{"seq": seq})); err != nil {
			t.Fatalf("emit during outage: %v", err)
		}
	}
	// Emission never blocks on ingestion: the phase kept running, and nothing
	// beyond seq 1 has been delivered.
	pending, dropped := stream.Undelivered()
	if pending != 11 || dropped != 0 {
		t.Fatalf("expected 11 buffered and 0 dropped during the outage, got %d/%d", pending, dropped)
	}
	if got := len(ingester.delivered()); got != 1 {
		t.Fatalf("nothing may be delivered during an outage; got %d events", got)
	}

	ingester.setOffline(false)
	if err := stream.Close(context.Background()); err != nil {
		t.Fatalf("close: %v", err)
	}

	delivered := ingester.delivered()
	if len(delivered) != 12 {
		t.Fatalf("expected all 12 events delivered after recovery, got %d", len(delivered))
	}
	for index, item := range delivered {
		if item.Seq != int64(index+1) {
			t.Fatalf("delivery must be strictly seq-ordered with no gaps: %d at position %d", item.Seq, index)
		}
	}
	if records := readTrace(t, path); len(records) != 12 {
		t.Fatalf("the JSONL raw record must hold every event regardless of ingestion: %d", len(records))
	}
}

// The buffer is explicitly bounded, and overflow is visible: the oldest
// undelivered events are replaced by ONE gap marker naming the range, and the
// JSONL keeps the complete record.
func TestBufferOverflowLeavesAGapMarkerRatherThanSilence(t *testing.T) {
	ingester := &fakeIngester{offline: true}
	stream, path := newTraceFixture(t, ingester, 6)

	for seq := int64(1); seq <= 24; seq++ {
		if err := stream.Emit(event(seq, protocol.EventToolCall, "Write", map[string]any{"seq": seq})); err != nil {
			t.Fatalf("emit: %v", err)
		}
	}
	pending, dropped := stream.Undelivered()
	if pending > 6 {
		t.Fatalf("the buffer must stay bounded, got %d pending", pending)
	}
	if dropped == 0 {
		t.Fatalf("expected the overflow to be counted")
	}

	ingester.setOffline(false)
	closeErr := stream.Close(context.Background())
	if closeErr == nil || !strings.Contains(closeErr.Error(), "dropped") {
		t.Fatalf("close must report the dropped events, got %v", closeErr)
	}

	// Nothing vanishes silently: every seq the store never received is inside
	// a gap marker's declared range, and delivery stays seq-ordered.
	delivered := ingester.delivered()
	accounted := map[int64]bool{}
	markers := 0
	var lastSeq int64
	for _, item := range delivered {
		if item.Seq <= lastSeq {
			t.Fatalf("delivery must stay seq-ordered even across a gap: %+v", delivered)
		}
		lastSeq = item.Seq
		if !isGapMarker(item) {
			accounted[item.Seq] = true
			continue
		}
		markers++
		var payload struct {
			From int64 `json:"from_seq"`
			To   int64 `json:"to_seq"`
		}
		if err := json.Unmarshal(item.Payload, &payload); err != nil {
			t.Fatalf("gap marker payload: %v", err)
		}
		if payload.From == 0 || payload.To < payload.From {
			t.Fatalf("a gap marker must name the range it stands for: %s", item.Payload)
		}
		for seq := payload.From; seq <= payload.To; seq++ {
			accounted[seq] = true
		}
	}
	if markers == 0 {
		t.Fatalf("an overflow with no gap marker is silent loss")
	}
	// Markers stand for RANGES, so a long outage produces a handful of them,
	// never one per dropped event.
	if markers > 4 {
		t.Fatalf("expected gap markers to merge into a few ranges, got %d", markers)
	}
	for seq := int64(1); seq <= 24; seq++ {
		if !accounted[seq] {
			t.Fatalf("seq %d neither arrived nor was declared missing", seq)
		}
	}
	// The complete record survives on disk: 24 events plus the gap notes.
	records := readTrace(t, path)
	if len(records) < 24 {
		t.Fatalf("the JSONL must hold every emitted event, got %d records", len(records))
	}
	events := 0
	for _, record := range records {
		if record["name"] != "trace_gap" {
			events++
		}
	}
	if events != 24 {
		t.Fatalf("expected all 24 emitted events in the raw record, got %d", events)
	}
}

// A stream that cannot deliver says so at close: the JSONL still holds
// everything, and the caller is told the store's copy is short.
func TestCloseReportsUndeliveredEvents(t *testing.T) {
	ingester := &fakeIngester{offline: true}
	stream, _ := newTraceFixture(t, ingester, 0)
	for seq := int64(1); seq <= 3; seq++ {
		if err := stream.Emit(event(seq, protocol.EventLog, "note", nil)); err != nil {
			t.Fatalf("emit: %v", err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	err := stream.Close(ctx)
	if err == nil || !strings.Contains(err.Error(), "never reached the control plane") {
		t.Fatalf("expected an undelivered report at close, got %v", err)
	}
	if err := stream.Emit(event(4, protocol.EventLog, "after", nil)); err == nil {
		t.Fatalf("a closed stream must refuse further events")
	}
}

// Redaction is defined by the definition's sensitive env names, not by
// guesswork: a value present in the environment under a non-sensitive name is
// left alone, and a sensitive name with no value redacts nothing.
func TestRedactorOnlyRedactsDeclaredSensitiveNames(t *testing.T) {
	spec, err := protocol.ParseDefinition([]byte(traceSnapshot))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	redact := newRedactor(spec, []string{"ANTHROPIC_API_KEY=topsecretvalue", "PATH=/usr/bin"})
	subject := event(1, protocol.EventToolCall, "Bash", map[string]any{
		"command": "PATH=/usr/bin run --key topsecretvalue",
	})
	got, names := redact.apply(subject)
	if strings.Contains(string(got.Payload), "topsecretvalue") {
		t.Fatalf("the sensitive value survived: %s", got.Payload)
	}
	if !strings.Contains(string(got.Payload), "/usr/bin") {
		t.Fatalf("a non-sensitive value must be left alone: %s", got.Payload)
	}
	if len(names) != 1 || names[0] != "ANTHROPIC_API_KEY" {
		t.Fatalf("expected the redacted name to be reported, got %v", names)
	}

	empty := newRedactor(spec, []string{"PATH=/usr/bin"})
	unchanged, hits := empty.apply(subject)
	if len(hits) != 0 || string(unchanged.Payload) != string(subject.Payload) {
		t.Fatalf("a sensitive name with no value must change nothing")
	}
}

// A secret containing JSON metacharacters is still caught: the redactor
// matches the escaped form the payload actually carries.
func TestRedactorMatchesJSONEscapedValues(t *testing.T) {
	spec, err := protocol.ParseDefinition([]byte(traceSnapshot))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	secretValue := `line"one\two`
	redact := newRedactor(spec, []string{"ANTHROPIC_API_KEY=" + secretValue})
	subject := event(1, protocol.EventLog, "agent_text", map[string]any{"text": "using " + secretValue})
	got, names := redact.apply(subject)
	if strings.Contains(string(got.Payload), `line\"one`) {
		t.Fatalf("the escaped secret survived: %s", got.Payload)
	}
	if len(names) != 1 {
		t.Fatalf("expected the redaction to be reported, got %v", names)
	}
	var decoded map[string]any
	if err := json.Unmarshal(got.Payload, &decoded); err != nil {
		t.Fatalf("redaction broke the payload JSON: %v (%s)", err, got.Payload)
	}
}
