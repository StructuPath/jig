// ingest.go — the server half of trace transport plus the read-only surface
// the UI (and any non-browser caller) polls (U8, KTD8, R15).
//
// Ingestion is one fenced, idempotent batch append: the presented lease token
// must own the attempt, the batch is capped at MaxEventsPerBatch, and every
// row lands with INSERT OR IGNORE on the (attempt_id, seq) primary key — so a
// worker replaying its buffer after an outage lands exactly once, in seq
// order. Events newly inserted are projected in the SAME transaction into the
// evidence tables (gate checks, invalid envelopes); replayed rows project
// nothing, which is what keeps projection idempotent without a second unique
// index.
//
// The read routes are deliberately plain JSON GETs with no side effects. The
// Milestone 1 agent-native review found the control plane had no read-only
// surface at all: everything an operator could see, an agent could only get
// by mutating something or by reading SQLite behind the server's back. These
// routes close that — the UI is just their first caller.
package controlplane

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/StructuPath/jig/internal/protocol"
)

// defaultEventPageSize and maxEventPageSize bound one cursor page. The
// default is a live-UI page; the ceiling keeps a scripted caller from asking
// for an attempt's whole trace in one response.
const (
	defaultEventPageSize = 200
	maxEventPageSize     = 1000
)

// ---- ingestion -------------------------------------------------------------

// IngestEvents appends one worker-sent batch of trace events to the attempt's
// stored trace and returns what actually landed.
//
// The fence is token ownership (R6): the digest presented must be the one
// that owns this attempt. It deliberately does NOT require an unexpired
// lease, and it accepts batches for a terminal attempt — including `lost`.
// Events are append-only evidence keyed by a per-attempt seq; they cannot
// move a state machine, and the tail of a trace from a worker that slept, was
// swept, or has just completed is exactly the record R15 exists to preserve.
// Rejecting it would delete the evidence to protect nothing.
func (s *Store) IngestEvents(
	ctx context.Context, attemptID string, batch protocol.EventBatch,
) (protocol.EventBatchResult, error) {
	var result protocol.EventBatchResult
	if len(batch.Events) > protocol.MaxEventsPerBatch {
		return result, invalid("batch_too_large",
			fmt.Sprintf("event batches are capped at %d events", protocol.MaxEventsPerBatch))
	}
	if err := validateLeaseToken(batch.LeaseToken); err != nil {
		return result, err
	}
	for _, event := range batch.Events {
		if event.Seq < 0 {
			return result, invalid("invalid_event_seq", "event seq must not be negative")
		}
		if event.Type == "" {
			return result, invalid("invalid_event_type", "every event must carry a type")
		}
	}
	now := s.now().UnixMilli()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return result, unavailable(err)
	}
	defer tx.Rollback()

	lease, err := loadLease(ctx, tx, attemptID)
	if err != nil {
		return result, err
	}
	if !equalDigest(lease.digest, digestToken(batch.LeaseToken)) {
		return result, conflict("lease_not_owner", "the lease token does not own this attempt")
	}

	for _, event := range batch.Events {
		inserted, err := insertEvent(ctx, tx, attemptID, event, now)
		if err != nil {
			return result, err
		}
		if !inserted {
			// A replayed row: its evidence was projected the first time.
			continue
		}
		result.Accepted++
		if err := projectEvidence(ctx, tx, attemptID, event, now); err != nil {
			return result, err
		}
	}
	if err := tx.QueryRowContext(ctx, `
		SELECT COALESCE(MAX(seq), 0) FROM events WHERE attempt_id = ?
	`, attemptID).Scan(&result.HighestSeq); err != nil {
		return result, unavailable(err)
	}
	if err := tx.Commit(); err != nil {
		return result, unavailable(err)
	}
	return result, nil
}

// insertEvent stores the full event JSON under (attempt_id, seq) and reports
// whether this call is the one that stored it. The stored payload is the
// whole event — type, phase, name, payload, timestamps — so a row
// reconstructs the JSONL line exactly.
func insertEvent(
	ctx context.Context, tx *sql.Tx, attemptID string, event protocol.Event, nowMillis int64,
) (bool, error) {
	event.Payload = capIngestedPayload(event.Payload)
	body, err := json.Marshal(event)
	if err != nil {
		return false, unavailable(err)
	}
	outcome, err := tx.ExecContext(ctx, `
		INSERT OR IGNORE INTO events(attempt_id, seq, type, phase, payload, payload_bytes, server_time)
		VALUES (?, ?, ?, ?, ?, ?, ?)
	`, attemptID, event.Seq, event.Type, event.Phase, body, len(body), nowMillis)
	if err != nil {
		return false, unavailable(err)
	}
	changed, err := outcome.RowsAffected()
	if err != nil {
		return false, unavailable(err)
	}
	return changed == 1, nil
}

// capIngestedPayload bounds one event payload server-side. The engine already
// caps what it emits, but ingestion is a route: what arrives is whatever the
// caller sent, and an unbounded payload would be an unbounded row.
func capIngestedPayload(payload json.RawMessage) json.RawMessage {
	if len(payload) <= protocol.MaxEventPayloadBytes {
		return payload
	}
	capped, err := json.Marshal(map[string]any{
		"truncated":      true,
		"original_bytes": len(payload),
		"prefix":         string(payload[:protocol.MaxEventPayloadBytes/2]),
	})
	if err != nil {
		return json.RawMessage(`{"truncated":true}`)
	}
	return capped
}

// gateEventPayload is the shape the engine emits with gate_pass/gate_fail.
type gateEventPayload struct {
	Attempt int                  `json:"attempt"`
	Checks  []protocol.GateCheck `json:"checks"`
}

// invalidEnvelopePayload is the shape the engine emits with the
// `invalid_envelope` log event (R7).
type invalidEnvelopePayload struct {
	Role         string `json:"role"`
	ParseAttempt int    `json:"parse_attempt"`
	Error        string `json:"error"`
	Raw          string `json:"raw"`
}

// projectEvidence writes the queryable evidence tables from an event that was
// just stored: one gate_results row per check, and one size-capped envelopes
// row per invalid emission. Only newly inserted events reach here, so replay
// never duplicates a row — which matters because gate_results has no natural
// unique key.
//
// A payload that does not match the expected shape is not an error: the
// events row is already stored, and the trace must never fail to record
// itself because a projection could not be derived.
func projectEvidence(
	ctx context.Context, tx *sql.Tx, attemptID string, event protocol.Event, nowMillis int64,
) error {
	switch {
	case event.Type == protocol.EventGatePass || event.Type == protocol.EventGateFail:
		var payload gateEventPayload
		if err := json.Unmarshal(event.Payload, &payload); err != nil || len(payload.Checks) == 0 {
			return nil
		}
		emission := payload.Attempt
		if emission < 1 {
			emission = 1
		}
		for _, check := range payload.Checks {
			ok := 0
			if check.Ok {
				ok = 1
			}
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO gate_results(attempt_id, phase, gate, emission, item, ok, note, created_at)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?)
			`, attemptID, event.Phase, event.Name, emission, check.Item, ok, check.Note, nowMillis); err != nil {
				return unavailable(err)
			}
		}
	case event.Type == protocol.EventLog && event.Name == "invalid_envelope":
		var payload invalidEnvelopePayload
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			return nil
		}
		// The emission key is the event's seq, not the parse attempt: a phase
		// re-entered by a gate correction restarts its parse attempts at 1, so
		// parse_attempt collides within one phase while seq never does. Seq
		// also orders the rows the way they happened.
		body := payload.Raw
		if len(body) > protocol.MaxInvalidEnvelopeBytes {
			body = body[:protocol.MaxInvalidEnvelopeBytes]
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT OR IGNORE INTO envelopes(attempt_id, phase, emission, valid, body, parse_error, created_at)
			VALUES (?, ?, ?, 0, ?, ?, ?)
		`, attemptID, event.Phase, event.Seq, body, payload.Error, nowMillis); err != nil {
			return unavailable(err)
		}
	}
	return nil
}

// ---- reads ------------------------------------------------------------------

// AttemptEventPage reads one seq-cursor page of an attempt's trace: strictly
// greater than after, in seq order (KTD8 — cursors ride seq, never rowid).
func (s *Store) AttemptEventPage(
	ctx context.Context, attemptID string, after int64, limit int,
) (protocol.EventPage, error) {
	page := protocol.EventPage{AttemptID: attemptID, After: after, NextCursor: after, Events: []protocol.Event{}}
	if limit <= 0 {
		limit = defaultEventPageSize
	}
	limit = min(limit, maxEventPageSize)
	if _, err := s.Attempt(ctx, attemptID); err != nil {
		return page, err
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT payload FROM events WHERE attempt_id = ? AND seq > ? ORDER BY seq LIMIT ?
	`, attemptID, after, limit)
	if err != nil {
		return page, unavailable(err)
	}
	defer rows.Close()
	for rows.Next() {
		var body []byte
		if err := rows.Scan(&body); err != nil {
			return page, unavailable(err)
		}
		var event protocol.Event
		if err := json.Unmarshal(body, &event); err != nil {
			return page, unavailable(err)
		}
		page.Events = append(page.Events, event)
		page.NextCursor = event.Seq
	}
	if err := rows.Err(); err != nil {
		return page, unavailable(err)
	}
	return page, nil
}

// AttemptGateEvidence reads the recorded gate checks for one attempt: what
// each gate looked at and what it found, in the order the gates ran (R9).
func (s *Store) AttemptGateEvidence(ctx context.Context, attemptID string) ([]GateEvidence, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT phase, gate, emission, item, ok, note, created_at
		FROM gate_results WHERE attempt_id = ?
		ORDER BY created_at, rowid
	`, attemptID)
	if err != nil {
		return nil, unavailable(err)
	}
	defer rows.Close()
	evidence := []GateEvidence{}
	for rows.Next() {
		var value GateEvidence
		var ok int
		var created int64
		if err := rows.Scan(&value.Phase, &value.Gate, &value.Emission,
			&value.Item, &ok, &value.Note, &created); err != nil {
			return nil, unavailable(err)
		}
		value.AttemptID = attemptID
		value.Ok = ok != 0
		value.RecordedAt = fromMillis(created)
		evidence = append(evidence, value)
	}
	if err := rows.Err(); err != nil {
		return nil, unavailable(err)
	}
	return evidence, nil
}

// AttemptEnvelopes reads the persisted envelope emissions for one attempt.
// v1 stores the invalid ones (R7): they are the emissions that exist nowhere
// else, size-capped here with the full form intact in the attempt-local
// JSONL. A phase's accepted envelope rides the attempt's own result payload.
func (s *Store) AttemptEnvelopes(ctx context.Context, attemptID string) ([]EnvelopeRecord, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT phase, emission, valid, body, parse_error, created_at
		FROM envelopes WHERE attempt_id = ? ORDER BY emission
	`, attemptID)
	if err != nil {
		return nil, unavailable(err)
	}
	defer rows.Close()
	records := []EnvelopeRecord{}
	for rows.Next() {
		var value EnvelopeRecord
		var valid int
		var created int64
		if err := rows.Scan(&value.Phase, &value.Emission, &valid,
			&value.Body, &value.ParseError, &created); err != nil {
			return nil, unavailable(err)
		}
		value.AttemptID = attemptID
		value.Valid = valid != 0
		value.RecordedAt = fromMillis(created)
		records = append(records, value)
	}
	if err := rows.Err(); err != nil {
		return nil, unavailable(err)
	}
	return records, nil
}

// GateEvidence is one recorded gate check (R9).
type GateEvidence struct {
	AttemptID  string    `json:"attempt_id"`
	Phase      string    `json:"phase"`
	Gate       string    `json:"gate"`
	Emission   int       `json:"emission"`
	Item       string    `json:"item"`
	Ok         bool      `json:"ok"`
	Note       string    `json:"note,omitempty"`
	RecordedAt time.Time `json:"recorded_at"`
}

// EnvelopeRecord is one persisted envelope emission (R7).
type EnvelopeRecord struct {
	AttemptID  string    `json:"attempt_id"`
	Phase      string    `json:"phase"`
	Emission   int64     `json:"emission"`
	Valid      bool      `json:"valid"`
	Body       string    `json:"body"`
	ParseError string    `json:"parse_error,omitempty"`
	RecordedAt time.Time `json:"recorded_at"`
}

// JobDetail is one job with every attempt ever made against it, oldest first
// — the swim-lane source: one lane per attempt, and attempt 1 stays
// inspectable after attempt 2 exists (R15).
type JobDetail struct {
	Job        protocol.Job                  `json:"job"`
	RunID      string                        `json:"run_id"`
	Definition string                        `json:"definition,omitempty"`
	Attempts   []protocol.Attempt            `json:"attempts"`
	Publish    []protocol.PublishRecord      `json:"publish"`
	Worktree   *protocol.WorktreeLedgerEntry `json:"worktree,omitempty"`
}

// JobDetail reads one job, its attempts, and its publish records.
func (s *Store) JobDetail(ctx context.Context, jobID string) (JobDetail, error) {
	var detail JobDetail
	job, err := s.Job(ctx, jobID)
	if err != nil {
		return detail, err
	}
	detail.Job = job
	detail.RunID = job.RunID
	if err := s.db.QueryRowContext(ctx, `
		SELECT d.name FROM runs r JOIN definitions d ON d.id = r.definition_id WHERE r.id = ?
	`, job.RunID).Scan(&detail.Definition); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return detail, unavailable(err)
	}
	attempts, err := s.JobAttempts(ctx, jobID)
	if err != nil {
		return detail, err
	}
	detail.Attempts = attempts
	records, err := s.JobPublishRecords(ctx, jobID)
	if err != nil {
		return detail, err
	}
	if records == nil {
		records = []protocol.PublishRecord{}
	}
	detail.Publish = records
	for _, attempt := range attempts {
		entry, err := s.worktreeLedgerEntry(ctx, attempt.ID)
		if errors.Is(err, ErrNotFound) {
			continue
		}
		if err != nil {
			return detail, err
		}
		if entry.State == protocol.WorktreeRetained {
			retained := entry
			detail.Worktree = &retained
		}
	}
	return detail, nil
}

// JobAttempts lists every attempt of one job, oldest first.
func (s *Store) JobAttempts(ctx context.Context, jobID string) ([]protocol.Attempt, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, job_id, worker_id, attempt_number, state, lease_expires_at,
		       runtime_name, runtime_version, result, error, started_at, completed_at, created_at
		FROM attempts WHERE job_id = ? ORDER BY attempt_number
	`, jobID)
	if err != nil {
		return nil, unavailable(err)
	}
	defer rows.Close()
	attempts := []protocol.Attempt{}
	for rows.Next() {
		attempt, err := scanAttempt(rows)
		if err != nil {
			return nil, err
		}
		attempts = append(attempts, attempt)
	}
	if err := rows.Err(); err != nil {
		return nil, unavailable(err)
	}
	return attempts, nil
}

// QueueEntry is one row of the work queue: the job, where it came from, and
// the attempt currently holding it (nil while queued).
type QueueEntry struct {
	JobID      string            `json:"job_id"`
	RunID      string            `json:"run_id"`
	Definition string            `json:"definition,omitempty"`
	Repository string            `json:"repository"`
	BaseSHA    string            `json:"base_sha"`
	State      string            `json:"state"`
	Position   int               `json:"position,omitempty"`
	Attempt    *protocol.Attempt `json:"attempt,omitempty"`
	EnqueuedAt time.Time         `json:"enqueued_at"`
}

// QueueView is the work queue with its depth — the number every operator
// wants first and the one no run list answers.
type QueueView struct {
	Depth       int          `json:"depth"`
	ActiveCount int          `json:"active_count"`
	Entries     []QueueEntry `json:"entries"`
	ObservedAt  time.Time    `json:"observed_at"`
}

// Queue reads the work queue: queued jobs in claim (FIFO) order followed by
// the jobs an attempt currently holds. Depth counts only the queued ones —
// what is waiting, not what is running.
func (s *Store) Queue(ctx context.Context) (QueueView, error) {
	view := QueueView{Entries: []QueueEntry{}, ObservedAt: s.now().UTC()}
	rows, err := s.db.QueryContext(ctx, `
		SELECT j.id, j.run_id, j.repository, j.base_sha, j.state, j.created_at, COALESCE(d.name, '')
		FROM jobs j
		JOIN runs r ON r.id = j.run_id
		LEFT JOIN definitions d ON d.id = r.definition_id
		WHERE j.state IN ('queued', 'active')
		ORDER BY CASE j.state WHEN 'active' THEN 0 ELSE 1 END, j.created_at, j.id
	`)
	if err != nil {
		return view, unavailable(err)
	}
	defer rows.Close()
	for rows.Next() {
		var entry QueueEntry
		var created int64
		if err := rows.Scan(&entry.JobID, &entry.RunID, &entry.Repository,
			&entry.BaseSHA, &entry.State, &created, &entry.Definition); err != nil {
			return view, unavailable(err)
		}
		entry.EnqueuedAt = fromMillis(created)
		if entry.State == protocol.JobQueued {
			view.Depth++
			entry.Position = view.Depth
		} else {
			view.ActiveCount++
		}
		view.Entries = append(view.Entries, entry)
	}
	if err := rows.Err(); err != nil {
		return view, unavailable(err)
	}
	// The active attempt is what makes an active row worth showing: without
	// it the operator sees "something is running" and nothing to click.
	for index := range view.Entries {
		if view.Entries[index].State != protocol.JobActive {
			continue
		}
		attempts, err := s.JobAttempts(ctx, view.Entries[index].JobID)
		if err != nil {
			return view, err
		}
		for position := len(attempts) - 1; position >= 0; position-- {
			if isLeasedState(attempts[position].State) {
				active := attempts[position]
				view.Entries[index].Attempt = &active
				break
			}
		}
	}
	return view, nil
}

// RetainedWorktrees lists every retained ledger row across workers — the
// operator's release surface (R16), which is per-worker nowhere else.
func (s *Store) RetainedWorktrees(ctx context.Context) ([]protocol.WorktreeLedgerEntry, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT attempt_id, worker_id, repository, path, reason, state, created_at, updated_at
		FROM retained_worktrees WHERE state = ? ORDER BY created_at, attempt_id
	`, protocol.WorktreeRetained)
	if err != nil {
		return nil, unavailable(err)
	}
	defer rows.Close()
	entries := []protocol.WorktreeLedgerEntry{}
	for rows.Next() {
		entry, err := scanWorktreeLedgerEntry(rows)
		if err != nil {
			return nil, err
		}
		entries = append(entries, entry)
	}
	if err := rows.Err(); err != nil {
		return nil, unavailable(err)
	}
	return entries, nil
}

// ---- HTTP surface ----------------------------------------------------------

// registerIngestRoutes attaches trace ingestion and the read-only surface,
// following the publish ledger's pattern: one line in http.go, every handler
// beside the store methods it calls.
//
// Only the ingestion route is state-changing, so only it takes
// prepareMutation; the GETs are side-effect free by construction and answer
// any local caller — the UI's poller, `curl`, or an agent (R20's fence
// exists to stop a foreign page from DRIVING the control plane, not from
// reading a loopback server the operator already trusts).
func (a *API) registerIngestRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/attempts/{attempt_id}/events", a.ingestEvents)
	mux.HandleFunc("GET /api/attempts/{attempt_id}/events", a.attemptEvents)
	mux.HandleFunc("GET /api/attempts/{attempt_id}", a.getAttempt)
	mux.HandleFunc("GET /api/attempts/{attempt_id}/gates", a.attemptGates)
	mux.HandleFunc("GET /api/attempts/{attempt_id}/envelopes", a.attemptEnvelopes)
	mux.HandleFunc("GET /api/jobs/{job_id}", a.getJob)
	mux.HandleFunc("GET /api/queue", a.queue)
	mux.HandleFunc("GET /api/worktrees", a.retainedWorktrees)
}

func (a *API) ingestEvents(w http.ResponseWriter, r *http.Request) {
	if !a.prepareMutation(w, r) {
		return
	}
	var input protocol.EventBatch
	if !decodeJSON(w, r, &input) {
		return
	}
	result, err := a.store.IngestEvents(r.Context(), r.PathValue("attempt_id"), input)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (a *API) attemptEvents(w http.ResponseWriter, r *http.Request) {
	after, err := queryInt(r, "after", 0)
	if err != nil {
		writeError(w, err)
		return
	}
	limit, err := queryInt(r, "limit", defaultEventPageSize)
	if err != nil {
		writeError(w, err)
		return
	}
	page, err := a.store.AttemptEventPage(r.Context(), r.PathValue("attempt_id"), after, int(limit))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, page)
}

func (a *API) getAttempt(w http.ResponseWriter, r *http.Request) {
	attempt, err := a.store.Attempt(r.Context(), r.PathValue("attempt_id"))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, attempt)
}

func (a *API) attemptGates(w http.ResponseWriter, r *http.Request) {
	evidence, err := a.store.AttemptGateEvidence(r.Context(), r.PathValue("attempt_id"))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, evidence)
}

func (a *API) attemptEnvelopes(w http.ResponseWriter, r *http.Request) {
	records, err := a.store.AttemptEnvelopes(r.Context(), r.PathValue("attempt_id"))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, records)
}

func (a *API) getJob(w http.ResponseWriter, r *http.Request) {
	detail, err := a.store.JobDetail(r.Context(), r.PathValue("job_id"))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, detail)
}

func (a *API) queue(w http.ResponseWriter, r *http.Request) {
	view, err := a.store.Queue(r.Context())
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

func (a *API) retainedWorktrees(w http.ResponseWriter, r *http.Request) {
	entries, err := a.store.RetainedWorktrees(r.Context())
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, entries)
}

// queryInt reads one non-negative integer query parameter. A malformed value
// is a 400 with the parameter named, never a silent default: a cursor that
// silently resets to 0 would replay an attempt's whole trace into the UI.
func queryInt(r *http.Request, name string, fallback int64) (int64, error) {
	raw := r.URL.Query().Get(name)
	if raw == "" {
		return fallback, nil
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || value < 0 {
		return 0, invalid("invalid_query_parameter",
			fmt.Sprintf("%s must be a non-negative integer", name))
	}
	return value, nil
}
