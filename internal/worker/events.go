// events.go — the worker half of trace transport (U8, KTD8). One TraceStream
// per attempt sits behind the engine's event sink and does three things, in
// this order, on every event:
//
//  1. redacts values matching the definition's sensitive env names, BEFORE
//     the event leaves the process (R15) — the store never sees a secret, and
//     neither does the on-disk trace;
//  2. writes the attempt-local JSONL raw record, annotated when redaction
//     fired, so an operator reading the file knows the text was altered
//     rather than silently wondering where an argument went;
//  3. hands the event to a bounded, seq-ordered send buffer that a single
//     goroutine drains into the control plane in batches.
//
// Emission never blocks a phase. When ingestion fails the events stay
// buffered and are replayed — UNIQUE(attempt_id, seq) with INSERT OR IGNORE
// makes that exactly-once server-side. The buffer is explicitly bounded; on
// overflow the OLDEST undelivered events are dropped and replaced by a single
// gap-marker event carrying the dropped range, because a UI that silently
// omits events lies, and the JSONL still holds the complete record.
//
// Delivery is strictly in seq order with one batch in flight at a time. That
// is what makes a UI cursor safe across an outage: the cursor can never
// advance past an event that has not landed, so replayed events always arrive
// after the cursor, in order.
package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/StructuPath/jig/internal/protocol"
)

// Trace transport tuning. These are worker-local rhythm knobs rather than
// protocol limits: the protocol bounds one batch (MaxEventsPerBatch) and one
// payload (MaxEventPayloadBytes); everything here is how often and how
// persistently this process tries.
const (
	// traceFlushInterval is how long the sender waits for more events before
	// sending a partial batch. Short enough that a live UI polling once a
	// second sees sub-second lag, long enough that a chatty phase coalesces.
	traceFlushInterval = 200 * time.Millisecond

	// traceRetryFloor and traceRetryCeiling bound the backoff between failed
	// ingestion attempts. The floor keeps a brief server restart nearly
	// invisible; the ceiling keeps a long outage from spinning.
	traceRetryFloor   = 250 * time.Millisecond
	traceRetryCeiling = 10 * time.Second
	traceRetryBackoff = 2

	// traceCloseGrace bounds the drain a closing stream attempts before it
	// reports what it could not deliver.
	traceCloseGrace = 5 * time.Second

	// maxBufferedEvents is the explicit bound on undelivered events held in
	// memory during an ingest outage (KTD8). Past it the oldest undelivered
	// events are dropped behind a gap marker — the JSONL keeps everything.
	maxBufferedEvents = 10000

	// minRedactableValue is the shortest sensitive value worth searching for.
	// Redacting a one-character value would blank most of a trace and hide
	// nothing worth hiding.
	minRedactableValue = 4
)

// EventIngester delivers one bounded batch of an attempt's trace events to
// the control plane. *Worker implements it over the worker's HTTP client;
// tests substitute a fake that can be made to fail.
type EventIngester interface {
	IngestEvents(ctx context.Context, attemptID string, batch protocol.EventBatch) (protocol.EventBatchResult, error)
}

// IngestEvents posts one seq-ordered batch (POST
// /api/attempts/{id}/events). The batch carries the lease token because
// ingestion is a fenced attempt write like every other (R6).
func (w *Worker) IngestEvents(
	ctx context.Context, attemptID string, batch protocol.EventBatch,
) (protocol.EventBatchResult, error) {
	var result protocol.EventBatchResult
	err := w.client.call(ctx, http.MethodPost,
		"/api/attempts/"+url.PathEscape(attemptID)+"/events", batch, &result)
	return result, err
}

// secret is one sensitive env var: the NAME is what the annotation reports,
// the VALUE is what is searched for and never recorded anywhere.
type secret struct {
	name    string
	value   string
	encoded string // the value as it appears inside a JSON string
}

// redactor replaces sensitive env values wherever they appear in an event.
// It works on the marshalled payload bytes, matching both the literal value
// and its JSON-escaped form, so a secret cannot survive by being escaped.
type redactor struct {
	secrets []secret
}

// newRedactor builds a redactor over the sensitive env names of every role in
// the frozen definition, resolved against the worker environment. Names with
// no value in this environment contribute nothing (there is no value to
// find), and values shorter than minRedactableValue are ignored: redacting
// "1" would blank out most of the trace and hide nothing worth hiding.
func newRedactor(spec *protocol.DefinitionSpec, environ []string) *redactor {
	if spec == nil {
		return &redactor{}
	}
	values := make(map[string]string, len(environ))
	for _, entry := range environ {
		name, value, found := strings.Cut(entry, "=")
		if found {
			values[name] = value
		}
	}
	seen := make(map[string]bool)
	var secrets []secret
	for _, role := range spec.Roster {
		for _, name := range role.SensitiveEnv {
			if seen[name] {
				continue
			}
			seen[name] = true
			value := values[name]
			if len(value) < minRedactableValue {
				continue
			}
			secrets = append(secrets, secret{name: name, value: value, encoded: jsonInner(value)})
		}
	}
	// Longest value first: a secret that contains another secret must be
	// replaced whole, or the inner replacement would leave a readable tail.
	sort.SliceStable(secrets, func(i, j int) bool {
		return len(secrets[i].value) > len(secrets[j].value)
	})
	return &redactor{secrets: secrets}
}

// jsonInner renders value the way encoding/json would inside a string,
// without the surrounding quotes.
func jsonInner(value string) string {
	encoded, err := json.Marshal(value)
	if err != nil || len(encoded) < 2 {
		return value
	}
	return string(encoded[1 : len(encoded)-1])
}

// apply returns the event with every sensitive value replaced by
// "[redacted:NAME]", plus the names that actually fired. The placeholder
// contains no JSON metacharacters, so replacing inside encoded payload bytes
// keeps the payload valid JSON.
func (r *redactor) apply(event protocol.Event) (protocol.Event, []string) {
	if len(r.secrets) == 0 {
		return event, nil
	}
	var hits []string
	record := func(name string) {
		for _, existing := range hits {
			if existing == name {
				return
			}
		}
		hits = append(hits, name)
	}
	for _, item := range r.secrets {
		placeholder := "[redacted:" + item.name + "]"
		if strings.Contains(event.Name, item.value) {
			event.Name = strings.ReplaceAll(event.Name, item.value, placeholder)
			record(item.name)
		}
		if len(event.Payload) == 0 {
			continue
		}
		body := string(event.Payload)
		replaced := strings.ReplaceAll(body, item.value, placeholder)
		if item.encoded != item.value {
			replaced = strings.ReplaceAll(replaced, item.encoded, placeholder)
		}
		if replaced != body {
			event.Payload = json.RawMessage(replaced)
			record(item.name)
		}
	}
	return event, hits
}

// traceRecord is one attempt-local JSONL line: the event exactly as the
// engine emitted it (post-redaction), plus the redaction annotation. The
// embedded Event inlines its fields, so a line stays a superset of the
// protocol event and parses straight back into one.
type traceRecord struct {
	protocol.Event
	// Redacted names the sensitive env vars whose values were replaced in
	// this event (R15). Its presence is the note that the raw record is not
	// verbatim — absence means the line is exactly what the engine emitted.
	Redacted []string `json:"redacted,omitempty"`
}

// TraceStreamConfig wires one attempt's stream. AttemptID, TracePath, and
// Ingest are required; the rest defaults.
type TraceStreamConfig struct {
	AttemptID  string
	LeaseToken string
	// TracePath is the attempt-local JSONL raw record (KTD8).
	TracePath string
	// Ingest delivers batches to the control plane.
	Ingest EventIngester
	// Spec is the frozen definition; its roles' sensitive env names drive
	// redaction (R15). Nil redacts nothing.
	Spec *protocol.DefinitionSpec
	// Environ is the environment sensitive names resolve against — values
	// stay in this process, only names are ever recorded.
	Environ []string
	Logger  *slog.Logger

	// Test seams. Zero means the constant above.
	FlushInterval time.Duration
	MaxBuffered   int
}

// TraceStream is one attempt's event sink. It satisfies the engine's
// EventSink interface structurally (Emit(protocol.Event) error), which is how
// the worker feeds the engine without importing it — engine imports worker,
// never the other way round (KTD1).
type TraceStream struct {
	attemptID   string
	leaseToken  string
	ingest      EventIngester
	redact      *redactor
	logger      *slog.Logger
	flushEvery  time.Duration
	maxBuffered int

	mutex   sync.Mutex
	file    *os.File
	pending []protocol.Event
	// sending is true while one batch is out at the control plane. That batch
	// has already left pending, so the bound below applies to buffered events
	// exactly; a failed batch is put back at the head.
	sending   bool
	dropped   int64
	closed    bool
	writeErrs int

	wake   chan struct{}
	done   chan struct{}
	stop   context.CancelFunc
	closer sync.Once
}

// OpenTrace opens the trace stream for one prepared attempt: the JSONL raw
// record under the worker's data directory, redaction built from the frozen
// definition's sensitive env names, and streaming ingestion fenced by the
// attempt's own lease token.
//
// It is the whole wiring `jig worker` needs to give the engine a sink. The
// runner closure is constructed before the worker exists, so capture the
// variable:
//
//	var w *worker.Worker
//	w, err = worker.New(worker.Config{Runner: worker.RunnerFunc(
//	    func(ctx context.Context, prepared *worker.PreparedAttempt) worker.Outcome {
//	        trace, err := w.OpenTrace(prepared)
//	        if err != nil { return worker.Outcome{State: protocol.AttemptFailed, Error: err.Error()} }
//	        defer trace.Close(ctx)
//	        runner, err := engine.New(engine.Config{Runtime: rt, Sink: trace, ScratchRoot: scratch})
//	        if err != nil { return worker.Outcome{State: protocol.AttemptFailed, Error: err.Error()} }
//	        return runner.Run(ctx, prepared)
//	    })})
//
// Close before the attempt completes: it drains the buffer so the terminal
// events reach the store before the operator sees a terminal attempt.
func (w *Worker) OpenTrace(prepared *PreparedAttempt) (*TraceStream, error) {
	if prepared == nil {
		return nil, errors.New("open trace: a prepared attempt is required")
	}
	// A snapshot that does not parse is the engine's failure to report, not
	// the trace's: the stream still opens, with nothing to redact, so the
	// operator can see the engine say why.
	spec, err := protocol.ParseDefinition([]byte(prepared.Claim.Snapshot))
	if err != nil {
		spec = nil
	}
	w.stateMutex.Lock()
	environ := append([]string(nil), w.environ...)
	w.stateMutex.Unlock()
	traceDir := filepath.Join(w.config.DataDir, "traces")
	if err := os.MkdirAll(traceDir, 0o700); err != nil {
		return nil, fmt.Errorf("create trace directory: %w", err)
	}
	return NewTraceStream(TraceStreamConfig{
		AttemptID:  prepared.Claim.Attempt.ID,
		LeaseToken: prepared.lease.token,
		TracePath:  filepath.Join(traceDir, prepared.Claim.Attempt.ID+".jsonl"),
		Ingest:     w,
		Spec:       spec,
		Environ:    environ,
		Logger:     w.logger,
	})
}

// NewTraceStream opens the JSONL record and starts the sender goroutine.
func NewTraceStream(config TraceStreamConfig) (*TraceStream, error) {
	if config.AttemptID == "" {
		return nil, errors.New("trace stream: attempt id is required")
	}
	if config.TracePath == "" {
		return nil, errors.New("trace stream: trace path is required")
	}
	if config.Ingest == nil {
		return nil, errors.New("trace stream: an ingester is required")
	}
	if config.Logger == nil {
		config.Logger = slog.Default()
	}
	if config.FlushInterval <= 0 {
		config.FlushInterval = traceFlushInterval
	}
	if config.MaxBuffered <= 0 {
		config.MaxBuffered = maxBufferedEvents
	}
	// Append, so an attempt that crashed and left a partial trace keeps it.
	file, err := os.OpenFile(config.TracePath, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open trace file: %w", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	stream := &TraceStream{
		attemptID:   config.AttemptID,
		leaseToken:  config.LeaseToken,
		ingest:      config.Ingest,
		redact:      newRedactor(config.Spec, config.Environ),
		logger:      config.Logger,
		flushEvery:  config.FlushInterval,
		maxBuffered: config.MaxBuffered,
		file:        file,
		wake:        make(chan struct{}, 1),
		done:        make(chan struct{}),
		stop:        cancel,
	}
	go stream.sendLoop(ctx)
	return stream, nil
}

// Emit records one event: redact, write the raw record, buffer for delivery.
// It returns an error only when the JSONL write failed — the raw record is
// the one thing this layer owes the operator synchronously. Ingestion
// failures are this stream's problem to retry, never a phase's to handle.
func (s *TraceStream) Emit(event protocol.Event) error {
	redacted, names := s.redact.apply(event)
	s.mutex.Lock()
	defer s.mutex.Unlock()
	if s.closed {
		return errors.New("trace stream is closed")
	}
	writeErr := s.writeRecord(traceRecord{Event: redacted, Redacted: names})
	s.buffer(redacted)
	s.nudge()
	return writeErr
}

// writeRecord appends one JSONL line. Caller holds the mutex.
func (s *TraceStream) writeRecord(record traceRecord) error {
	body, err := json.Marshal(record)
	if err != nil {
		s.writeErrs++
		return fmt.Errorf("encode trace record: %w", err)
	}
	if _, err := s.file.Write(append(body, '\n')); err != nil {
		s.writeErrs++
		return fmt.Errorf("write trace record: %w", err)
	}
	return nil
}

// buffer appends one event to the send buffer, dropping the oldest
// undelivered run when the bound is reached. Caller holds the mutex.
func (s *TraceStream) buffer(event protocol.Event) {
	if len(s.pending) >= s.maxBuffered {
		s.dropOldest()
	}
	s.pending = append(s.pending, event)
}

// enforceBound drops the oldest undelivered events until the buffer is back
// within its cap. It exists for the one path that can exceed the cap in a
// single step — a failed batch restored at the head — where dropping once is
// not necessarily enough.
//
// The loop stops if a drop frees nothing rather than spinning: dropOldest
// declines to act on a buffer too short to leave a gap marker behind, which
// is reachable only with a cap below two. Caller holds the mutex.
func (s *TraceStream) enforceBound() {
	for len(s.pending) > s.maxBuffered {
		before := len(s.pending)
		s.dropOldest()
		if len(s.pending) >= before {
			return
		}
	}
}

// gapCoveredTo reads the upper bound of the range a gap marker declares. The
// marker's own Seq is its lower bound, so an unreadable payload degrades to
// "covers itself only" — narrower than the truth, never wider, which keeps a
// merge from silently shrinking a declared range.
func gapCoveredTo(event protocol.Event) int64 {
	var declared struct {
		ToSeq int64 `json:"to_seq"`
	}
	if err := json.Unmarshal(event.Payload, &declared); err != nil {
		return event.Seq
	}
	return declared.ToSeq
}

// isGapMarker reports whether an event is one this stream synthesized for a
// dropped run, so consecutive overflows merge into one marker instead of
// producing a flood of them.
func isGapMarker(event protocol.Event) bool {
	return event.Type == protocol.EventError && event.Name == "trace_gap"
}

// dropOldest frees room by discarding the oldest undelivered events and
// leaving one gap marker in their place. The marker reuses the seq of the
// first dropped event, so it inserts under the same primary key that event
// would have used and the stored trace stays ordered and gap-honest — the
// complete record is still in the JSONL.
//
// The drop always frees at least one slot: when the head is already a gap
// marker it is discarded with the run it covers and re-emitted as one merged
// marker, so consecutive overflows never ping-pong without progress. Caller
// holds the mutex.
func (s *TraceStream) dropOldest() {
	if len(s.pending) == 0 {
		return
	}
	chunk := max(2, s.maxBuffered/10)
	merging := isGapMarker(s.pending[0])
	if merging {
		chunk++
	}
	drop := min(chunk, len(s.pending))
	if drop < 2 {
		return
	}
	discarded := s.pending[:drop]
	first := discarded[0]
	lost := drop
	// The range the replacement marker must declare is the widest any
	// discarded element covered — not simply the last one's seq. A marker
	// swallowed by this drop already stood for events that are gone, and
	// letting the new marker end short of that range would leave them
	// undeclared: silent loss, which is the one outcome this whole mechanism
	// exists to prevent. Markers can sit anywhere in the run, not only at the
	// head, because a failed batch is restored in front of whatever was
	// buffered while it was in flight.
	covered := discarded[drop-1].Seq
	for _, event := range discarded {
		if !isGapMarker(event) {
			continue
		}
		lost-- // our own marker, not a lost event
		if to := gapCoveredTo(event); to > covered {
			covered = to
		}
	}
	s.dropped += int64(lost)

	marker := protocol.Event{
		Seq:       first.Seq,
		Type:      protocol.EventError,
		Phase:     first.Phase,
		Name:      "trace_gap",
		StartedAt: first.StartedAt,
	}
	payload, err := json.Marshal(map[string]any{
		"reason":     "the worker's event buffer overflowed while ingestion was unavailable",
		"dropped":    s.dropped,
		"from_seq":   first.Seq,
		"to_seq":     covered,
		"buffer_cap": s.maxBuffered,
		"note":       "the complete trace is in the attempt-local JSONL record",
	})
	if err == nil {
		marker.Payload = payload
	}
	kept := make([]protocol.Event, 0, len(s.pending)-drop+1)
	kept = append(kept, marker)
	kept = append(kept, s.pending[drop:]...)
	s.pending = kept
	// The gap belongs in the raw record too: the JSONL is complete, so it is
	// the only place that can say which events never reached the store.
	if err := s.writeRecord(traceRecord{Event: marker}); err != nil {
		s.logger.Warn("trace_gap_record_failed", "attempt_id", s.attemptID, "error", err)
	}
	s.logger.Warn("trace_buffer_overflow", "attempt_id", s.attemptID,
		"dropped", lost, "from_seq", first.Seq, "to_seq", covered)
}

// nudge wakes the sender without blocking. Caller holds the mutex.
func (s *TraceStream) nudge() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// sendLoop is the single sender: one batch in flight, strictly in seq order,
// retried with backoff until it lands or the stream closes. Strict ordering
// is the property UI cursors depend on — a cursor can never advance past an
// event that has not been delivered, so replay after an outage always arrives
// after the cursor.
func (s *TraceStream) sendLoop(ctx context.Context) {
	defer close(s.done)
	backoff := traceRetryFloor
	timer := time.NewTimer(s.flushEvery)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.wake:
		case <-timer.C:
		}
		delivered, err := s.sendOnce(ctx)
		switch {
		case err != nil:
			resetTraceTimer(timer, backoff)
			backoff = min(backoff*traceRetryBackoff, traceRetryCeiling)
		case delivered > 0:
			// More may already be waiting: come straight back.
			backoff = traceRetryFloor
			resetTraceTimer(timer, 0)
		default:
			backoff = traceRetryFloor
			resetTraceTimer(timer, s.flushEvery)
		}
	}
}

func resetTraceTimer(timer *time.Timer, delay time.Duration) {
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
	if delay <= 0 {
		delay = time.Nanosecond
	}
	timer.Reset(delay)
}

// sendOnce delivers at most MaxEventsPerBatch buffered events. The batch
// leaves the buffer for the duration of the send — so the bound applies to
// buffered events exactly — and a failed batch goes back at the head, where
// the next attempt replays it. Replay is what INSERT OR IGNORE on
// (attempt_id, seq) turns into exactly-once.
func (s *TraceStream) sendOnce(ctx context.Context) (int, error) {
	s.mutex.Lock()
	if s.sending {
		// One batch at a time is the ordering guarantee; a second concurrent
		// sender would break it.
		s.mutex.Unlock()
		return 0, nil
	}
	count := min(len(s.pending), protocol.MaxEventsPerBatch)
	if count == 0 {
		s.mutex.Unlock()
		return 0, nil
	}
	batch := append([]protocol.Event(nil), s.pending[:count]...)
	s.pending = append([]protocol.Event(nil), s.pending[count:]...)
	s.sending = true
	s.mutex.Unlock()

	_, err := s.ingest.IngestEvents(ctx, s.attemptID, protocol.EventBatch{
		LeaseToken: s.leaseToken,
		Events:     batch,
	})

	s.mutex.Lock()
	defer s.mutex.Unlock()
	s.sending = false
	if err != nil {
		if ctx.Err() == nil {
			s.logger.Warn("trace_ingest_failed", "attempt_id", s.attemptID,
				"events", count, "error", err)
		}
		// Back at the head, where they were: the batch is the oldest run, so
		// restoring it in front of whatever arrived meanwhile keeps the buffer
		// in seq order.
		//
		// Then re-apply the bound. Emit kept buffering while this batch was
		// out — the bound it enforced counted only what was still pending —
		// so restoring the batch can push the buffer past its cap. Without
		// this the cap is really maxBuffered plus one batch, and "explicitly
		// bounded" would be a promise the code does not keep.
		s.pending = append(batch, s.pending...)
		s.enforceBound()
		return 0, err
	}
	return count, nil
}

// Undelivered reports how many events are still buffered and how many were
// dropped on overflow. It is what a caller checks to know whether the store's
// copy of this attempt's trace is complete.
func (s *TraceStream) Undelivered() (pending int, dropped int64) {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	return len(s.pending), s.dropped
}

// Flush drains the buffer until it is empty or ctx ends. It is what an
// attempt calls before reporting a terminal state, so the operator never sees
// a finished attempt whose last events are still in flight.
func (s *TraceStream) Flush(ctx context.Context) error {
	for {
		s.mutex.Lock()
		remaining := len(s.pending)
		s.mutex.Unlock()
		if remaining == 0 {
			return nil
		}
		delivered, err := s.sendOnce(ctx)
		if err == nil && delivered > 0 {
			continue
		}
		// Either the send failed or the background sender owns the current
		// batch. Both are waits, not spins.
		select {
		case <-ctx.Done():
			cause := ctx.Err()
			if err != nil {
				cause = err
			}
			return fmt.Errorf("flush trace: %d event(s) undelivered: %w", remaining, cause)
		case <-time.After(s.flushEvery):
		}
	}
}

// Close drains what it can within traceCloseGrace, stops the sender, and
// closes the JSONL file. Undelivered events are reported, never hidden: the
// JSONL still holds them, and the operator is told the store's copy is short.
func (s *TraceStream) Close(ctx context.Context) error {
	var result error
	s.closer.Do(func() {
		if ctx == nil {
			ctx = context.Background()
		}
		// Detached from the caller's CANCELLATION: an attempt stopped by
		// Ctrl-C is exactly when the last events matter most, and the
		// caller's context is already dead. An explicit deadline is still
		// honoured — that is a caller saying how long it can wait, not a
		// caller having gone away.
		grace := traceCloseGrace
		if deadline, ok := ctx.Deadline(); ok {
			if remaining := time.Until(deadline); remaining > 0 && remaining < grace {
				grace = remaining
			}
		}
		drainCtx, cancelDrain := context.WithTimeout(context.WithoutCancel(ctx), grace)
		flushErr := s.Flush(drainCtx)
		cancelDrain()

		s.stop()
		<-s.done

		s.mutex.Lock()
		s.closed = true
		remaining := len(s.pending)
		dropped := s.dropped
		closeErr := s.file.Close()
		s.mutex.Unlock()

		var problems []error
		if flushErr != nil {
			problems = append(problems, flushErr)
		}
		if remaining > 0 {
			problems = append(problems, fmt.Errorf(
				"%d trace event(s) never reached the control plane; the full record is in the attempt JSONL", remaining))
		}
		if dropped > 0 {
			problems = append(problems, fmt.Errorf(
				"%d trace event(s) were dropped on buffer overflow and are marked by a gap event", dropped))
		}
		if closeErr != nil {
			problems = append(problems, closeErr)
		}
		result = errors.Join(problems...)
	})
	return result
}
