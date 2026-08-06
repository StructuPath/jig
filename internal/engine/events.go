// events.go — the engine's trace emission seam (U4 half of KTD8). The engine
// emits protocol event types with a per-attempt monotonic sequence number to
// an EventSink; the JSONL writer here is the attempt-local raw record, and
// HTTP ingestion joins in U8 behind the same interface. Emission never
// blocks or fails a phase: sink errors are the sink's problem to surface.
package engine

import (
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/StructuPath/jig/internal/protocol"
)

// EventSink receives the attempt's ordered trace events. Emit must be safe
// for concurrent use; the engine serializes its own emissions but makes no
// promise a future caller will.
type EventSink interface {
	Emit(event protocol.Event) error
}

// SinkFunc adapts a function to EventSink.
type SinkFunc func(event protocol.Event) error

// Emit implements EventSink.
func (f SinkFunc) Emit(event protocol.Event) error { return f(event) }

// JSONLSink writes one JSON line per event — the attempt-local raw record
// (KTD8). It appends, so a crashed attempt's partial trace survives.
type JSONLSink struct {
	mutex sync.Mutex
	file  *os.File
}

// NewJSONLSink opens (or creates, 0600) the trace file for appending.
func NewJSONLSink(path string) (*JSONLSink, error) {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open trace file: %w", err)
	}
	return &JSONLSink{file: file}, nil
}

// Emit writes one event line.
func (s *JSONLSink) Emit(event protocol.Event) error {
	body, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("encode trace event: %w", err)
	}
	s.mutex.Lock()
	defer s.mutex.Unlock()
	_, err = s.file.Write(append(body, '\n'))
	return err
}

// Close flushes and closes the trace file.
func (s *JSONLSink) Close() error {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	return s.file.Close()
}

// emitter assigns the per-attempt monotonic sequence and delivers to the
// sink. Sink failures are counted, never propagated into phase control flow.
type emitter struct {
	mutex     sync.Mutex
	sink      EventSink
	seq       int64
	sinkFails int
	now       func() time.Time
}

func newEmitter(sink EventSink, now func() time.Time) *emitter {
	if now == nil {
		now = time.Now
	}
	return &emitter{sink: sink, now: now}
}

// emit sends one point-in-time event. Payload is JSON-encoded and capped at
// MaxEventPayloadBytes with a truncation marker.
func (e *emitter) emit(eventType, phase, name string, payload any) {
	e.mutex.Lock()
	defer e.mutex.Unlock()
	e.seq++
	at := e.now().UTC()
	event := protocol.Event{
		Seq:       e.seq,
		Type:      eventType,
		Phase:     phase,
		Name:      name,
		Payload:   encodePayload(payload),
		StartedAt: &at,
	}
	if e.sink == nil {
		return
	}
	if err := e.sink.Emit(event); err != nil {
		e.sinkFails++
	}
}

func encodePayload(payload any) json.RawMessage {
	if payload == nil {
		return nil
	}
	if raw, ok := payload.(json.RawMessage); ok {
		return capPayload(raw)
	}
	body, err := json.Marshal(payload)
	if err != nil {
		body, _ = json.Marshal(map[string]string{"encode_error": err.Error()})
	}
	return capPayload(body)
}

func capPayload(body json.RawMessage) json.RawMessage {
	if len(body) <= protocol.MaxEventPayloadBytes {
		return body
	}
	capped, err := json.Marshal(map[string]any{
		"truncated": true,
		"prefix":    string(body[:protocol.MaxEventPayloadBytes/2]),
	})
	if err != nil {
		return json.RawMessage(`{"truncated":true}`)
	}
	return capped
}
