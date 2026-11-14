package controlplane

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/StructuPath/JIG-Software-Factory/internal/protocol"
)

// GET /api/runs is polled every few seconds, so it reads one bounded page of
// runs. The default is the cap: today's list and floor read the whole
// response with no cursor, and a default below the cap would hide runs the
// UI shows now. The cap is what bounds a response — with result and error
// cut to excerpts, a page is a few hundred KiB, not every result ever
// written (U8 read surface).
const (
	DefaultRunListLimit = 200
	MaxRunListLimit     = 200
	// ExcerptRunes bounds the first-line excerpt a summary carries of an
	// attempt's result and error. The full bodies stay on GET /api/jobs/:id.
	ExcerptRunes = 300
)

// RunListPage selects one page of GET /api/runs, newest first. Before is a
// run ID: the page starts at the run listed after it, so a client pages by
// passing the last ID it holds. Limit 0 means DefaultRunListLimit; a limit
// above MaxRunListLimit is capped there.
type RunListPage struct {
	Limit  int
	Before string
}

// RunListItem is one row of GET /api/runs: the run exactly as it was listed
// before, plus a summary of each of its jobs. The summary exists because the
// jobs list lit every row with two more requests (GET /api/runs/:id, then
// GET /api/jobs/:id) — 84 requests for 40 jobs, and rows that flickered
// unlit until both returned (U8 read surface).
//
// The run is embedded so its JSON fields stay flat and unchanged: a UI that
// does not know job_summaries reads the list as it always did.
type RunListItem struct {
	protocol.Run
	JobSummaries []JobSummary `json:"job_summaries"`
}

// JobSummary is the slice of JobDetail a list row needs: the job, the frozen
// chain and publish policy, and the latest attempt with that attempt's
// publish records. It reuses JobDetail's field names and types on purpose —
// the UI folds stations from (stations, delivery, attempt, publish) with one
// function, and a second server-side vocabulary for station states would
// drift from it. Earlier attempts and the retained worktree stay on the job
// page; a row shows only where the job stands now.
type JobSummary struct {
	Job      protocol.Job `json:"job"`
	Stations []Station    `json:"stations,omitempty"`
	Delivery *Delivery    `json:"delivery,omitempty"`
	// AttemptCount says how many attempts the job has had, so a row can mark
	// a retried job without the attempt history.
	AttemptCount  int           `json:"attempt_count"`
	LatestAttempt *AttemptBrief `json:"latest_attempt,omitempty"`
	// Phases is where each phase of the latest attempt stands. Stations are
	// definitions only, and an active attempt has no result until it
	// completes, so without this a running job reads the same before its
	// first phase as with build passed and test running.
	Phases  []PhaseProgress          `json:"phases"`
	Publish []protocol.PublishRecord `json:"publish"`
}

// AttemptBrief is the latest attempt without its result and error bodies: a
// result can be 64 KiB and the list is polled, so a row gets a first-line
// excerpt of each, a flag when it was cut, and the publish outcome the row's
// lamps read out of the result. The attempt's own fields stay flat.
type AttemptBrief struct {
	protocol.Attempt
	ResultExcerpt   string `json:"result_excerpt,omitempty"`
	ResultTruncated bool   `json:"result_truncated,omitempty"`
	ErrorExcerpt    string `json:"error_excerpt,omitempty"`
	ErrorTruncated  bool   `json:"error_truncated,omitempty"`
	// PublishState and PublishCode are the result's `publish` outcome: the
	// bare string form, or the object form's state and code.
	PublishState string `json:"publish_state,omitempty"`
	PublishCode  string `json:"publish_code,omitempty"`
}

// PhaseProgress is one phase of an attempt in the trace's own vocabulary —
// the statuses the job page's lane reads from the same events: "running"
// for a phase_start with no end yet, "death" for phase_death, otherwise
// phase_end's status (success, fail, skipped). Once the attempt has a
// result, the result's status for the phase wins, as it does on the job
// page. Rounds counts phase_start events: a repair loop re-runs a phase.
type PhaseProgress struct {
	Phase  string `json:"phase"`
	Status string `json:"status"`
	Rounds int    `json:"rounds"`
}

// RunList reads one page of runs, newest first, with their job summaries,
// and returns the run ID to pass as Before for the next page ("" on the last
// page).
//
// Every read runs in one read-only transaction. The summary joins a job's
// state to its latest attempt, and separate reads let a retry land between
// them: job failed, latest attempt queued. One snapshot makes each row a
// state the ledger was actually in.
//
// The query count is fixed — runs, jobs, latest attempts, their publish
// records, their phase events, their result phases — whatever the number of
// runs: the point of the summary is to end a fetch per row, and a query per
// row would only move that loop server-side. Every summary query is scoped
// to the page.
func (s *Store) RunList(ctx context.Context, page RunListPage) ([]RunListItem, string, error) {
	limit := page.Limit
	if limit <= 0 {
		limit = DefaultRunListLimit
	}
	if limit > MaxRunListLimit {
		limit = MaxRunListLimit
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, "", unavailable(err)
	}
	// Read-only: nothing to commit, and rollback ends the snapshot.
	defer tx.Rollback()

	runs, next, err := runPage(ctx, tx, page.Before, limit)
	if err != nil {
		return nil, "", err
	}
	runIDs := make([]string, len(runs))
	for i, run := range runs {
		runIDs[i] = run.ID
	}
	jobsByRun, err := pageJobs(ctx, tx, runIDs)
	if err != nil {
		return nil, "", err
	}
	latest, counts, err := pageLatestAttempts(ctx, tx, runIDs)
	if err != nil {
		return nil, "", err
	}
	attemptIDs := make([]string, 0, len(latest))
	for _, attempt := range latest {
		attemptIDs = append(attemptIDs, attempt.ID)
	}
	idList := jsonList(attemptIDs)
	// Only the latest attempt's records: the row's PR link and the publish
	// and CI lamps are all read for that attempt alone.
	records, err := queryPublishRecords(ctx, tx, `p.attempt_id IN (SELECT value FROM json_each(?))`, idList)
	if err != nil {
		return nil, "", err
	}
	publishByAttempt := map[string][]protocol.PublishRecord{}
	for _, record := range records {
		publishByAttempt[record.AttemptID] = append(publishByAttempt[record.AttemptID], record)
	}
	phases, err := pagePhaseProgress(ctx, tx, idList)
	if err != nil {
		return nil, "", err
	}

	items := make([]RunListItem, 0, len(runs))
	for _, run := range runs {
		item := RunListItem{Run: run, JobSummaries: []JobSummary{}}
		jobs := jobsByRun[run.ID]
		if len(jobs) > 0 {
			stations, delivery := s.runStations.stations(run.ID, run.Snapshot)
			for _, job := range jobs {
				summary := JobSummary{
					Job:          job,
					Stations:     stations,
					Delivery:     delivery,
					AttemptCount: counts[job.ID],
					Phases:       []PhaseProgress{},
					Publish:      []protocol.PublishRecord{},
				}
				if attempt, ok := latest[job.ID]; ok {
					summary.LatestAttempt = &attempt
					if progress := phases[attempt.ID]; progress != nil {
						summary.Phases = progress
					}
					if records := publishByAttempt[attempt.ID]; records != nil {
						summary.Publish = records
					}
				}
				item.JobSummaries = append(item.JobSummaries, summary)
			}
		}
		items = append(items, item)
	}
	return items, next, nil
}

// runPage reads limit runs after the cursor run in list order (created_at
// descending, then ID), plus one more to learn whether a next page exists.
// An unknown cursor is a client error, not an empty page: an empty page
// would read as "no older runs".
func runPage(ctx context.Context, tx *sql.Tx, before string, limit int) ([]protocol.Run, string, error) {
	const columns = `id, definition_id, definition_generation, snapshot, parameters, targets,
		state, created_at, updated_at`
	var rows *sql.Rows
	var err error
	if before == "" {
		rows, err = tx.QueryContext(ctx, `SELECT `+columns+` FROM runs
			ORDER BY created_at DESC, id LIMIT ?`, limit+1)
	} else {
		var createdAt int64
		err = tx.QueryRowContext(ctx, `SELECT created_at FROM runs WHERE id = ?`, before).Scan(&createdAt)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, "", invalid("invalid_cursor", "before must be the ID of a listed run")
		}
		if err != nil {
			return nil, "", unavailable(err)
		}
		rows, err = tx.QueryContext(ctx, `SELECT `+columns+` FROM runs
			WHERE created_at < ? OR (created_at = ? AND id > ?)
			ORDER BY created_at DESC, id LIMIT ?`, createdAt, createdAt, before, limit+1)
	}
	if err != nil {
		return nil, "", unavailable(err)
	}
	defer rows.Close()
	runs := []protocol.Run{}
	for rows.Next() {
		run, err := scanRun(rows)
		if err != nil {
			return nil, "", unavailable(err)
		}
		runs = append(runs, run)
	}
	if err := rows.Err(); err != nil {
		return nil, "", unavailable(err)
	}
	if len(runs) <= limit {
		return runs, "", nil
	}
	runs = runs[:limit]
	return runs, runs[limit-1].ID, nil
}

// pageJobs reads the page's jobs in one query, grouped by run in the
// creation order RunJobs uses.
func pageJobs(ctx context.Context, tx *sql.Tx, runIDs []string) (map[string][]protocol.Job, error) {
	rows, err := tx.QueryContext(ctx, `SELECT `+jobColumns+` FROM jobs
		WHERE run_id IN (SELECT value FROM json_each(?))
		ORDER BY run_id, created_at, id`, jsonList(runIDs))
	if err != nil {
		return nil, unavailable(err)
	}
	defer rows.Close()
	byRun := map[string][]protocol.Job{}
	for rows.Next() {
		job, err := scanJob(rows)
		if err != nil {
			return nil, unavailable(err)
		}
		byRun[job.RunID] = append(byRun[job.RunID], job)
	}
	if err := rows.Err(); err != nil {
		return nil, unavailable(err)
	}
	return byRun, nil
}

// pageLatestAttempts reads each page job's highest-numbered attempt and its
// attempt count in one query. UNIQUE(job_id, attempt_number) makes the join
// pick exactly one attempt per job.
//
// The result and error bodies never leave SQLite: the query selects a
// prefix long enough for the excerpt, the full length for the truncation
// flag, and the publish outcome via json_extract. The column list mirrors
// attemptColumns with NULL for result and error, so scanAttempt still reads
// it; a change to attemptColumns fails this scan in the tests.
func pageLatestAttempts(ctx context.Context, tx *sql.Tx, runIDs []string) (map[string]AttemptBrief, map[string]int, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT latest.attempt_count,
		       length(a.result), substr(a.result, 1, ?), length(a.error), substr(a.error, 1, ?),
		       CASE WHEN json_valid(a.result) THEN
		         CASE json_type(a.result, '$.publish')
		           WHEN 'text' THEN json_extract(a.result, '$.publish')
		           WHEN 'object' THEN json_extract(a.result, '$.publish.state')
		         END
		       END,
		       CASE WHEN json_valid(a.result) AND json_type(a.result, '$.publish') = 'object'
		         THEN json_extract(a.result, '$.publish.code') END,
		       a.id, a.job_id, a.worker_id, a.attempt_number, a.state, a.lease_expires_at,
		       a.runtime_name, a.runtime_version, NULL, NULL, a.started_at, a.completed_at,
		       a.created_at, a.publish_hold
		FROM attempts a JOIN (
			SELECT job_id AS latest_job, MAX(attempt_number) AS latest_number, COUNT(*) AS attempt_count
			FROM attempts
			WHERE job_id IN (SELECT id FROM jobs WHERE run_id IN (SELECT value FROM json_each(?)))
			GROUP BY job_id
		) latest ON latest.latest_job = a.job_id AND latest.latest_number = a.attempt_number
	`, ExcerptRunes, ExcerptRunes, jsonList(runIDs))
	if err != nil {
		return nil, nil, unavailable(err)
	}
	defer rows.Close()
	latest := map[string]AttemptBrief{}
	counts := map[string]int{}
	for rows.Next() {
		var count int
		var resultLength, errorLength sql.NullInt64
		var resultHead, errorHead, publishState, publishCode sql.NullString
		attempt, err := scanAttempt(prefixedScanner{rows: rows, prefix: []any{
			&count, &resultLength, &resultHead, &errorLength, &errorHead, &publishState, &publishCode,
		}})
		if err != nil {
			return nil, nil, unavailable(err)
		}
		brief := AttemptBrief{Attempt: attempt, PublishState: publishState.String, PublishCode: publishCode.String}
		brief.ResultExcerpt, brief.ResultTruncated = excerpt(resultHead.String, resultLength.Int64)
		brief.ErrorExcerpt, brief.ErrorTruncated = excerpt(errorHead.String, errorLength.Int64)
		latest[attempt.JobID] = brief
		counts[attempt.JobID] = count
	}
	if err := rows.Err(); err != nil {
		return nil, nil, unavailable(err)
	}
	return latest, counts, nil
}

// excerpt cuts head — a prefix of a body of length characters — to its first
// line and at most ExcerptRunes runes, and reports whether anything of the
// body was left out.
func excerpt(head string, length int64) (string, bool) {
	line, _, _ := strings.Cut(head, "\n")
	line = strings.TrimRight(line, "\r")
	if utf8.RuneCountInString(line) > ExcerptRunes {
		line = string([]rune(line)[:ExcerptRunes])
	}
	return line, int64(utf8.RuneCountInString(line)) < length
}

// pagePhaseProgress reads where each phase of the given attempts stands: the
// last phase event per (attempt, phase) from the trace, then the statuses in
// each attempt's result, which win once the attempt has one — the job page
// reads them in that same order. Phases keep the order they first appeared
// in, trace first. A phase whose last event is phase_start reads "running"
// while its attempt is open and "stopped" once the attempt has completed_at
// — the sweeper closes a lost attempt that way, with no result and no
// phase_end — as the lane's completion fallback does (web/src/lanes.ts). Two
// queries whatever the number of attempts.
func pagePhaseProgress(ctx context.Context, tx *sql.Tx, attemptIDs string) (map[string][]PhaseProgress, error) {
	progress := map[string][]PhaseProgress{}
	index := map[string]map[string]int{}
	set := func(attemptID, phase, status string, rounds int) {
		if index[attemptID] == nil {
			index[attemptID] = map[string]int{}
		}
		if i, ok := index[attemptID][phase]; ok {
			progress[attemptID][i].Status = status
			progress[attemptID][i].Rounds = max(progress[attemptID][i].Rounds, rounds)
			return
		}
		index[attemptID][phase] = len(progress[attemptID])
		progress[attemptID] = append(progress[attemptID], PhaseProgress{Phase: phase, Status: status, Rounds: rounds})
	}

	rows, err := tx.QueryContext(ctx, `
		SELECT e.attempt_id, e.phase, e.type, e.payload, last.rounds, a.completed_at IS NOT NULL
		FROM events e JOIN attempts a ON a.id = e.attempt_id JOIN (
			SELECT attempt_id, phase, MAX(seq) AS last_seq, MIN(seq) AS first_seq,
			       SUM(type = 'phase_start') AS rounds
			FROM events
			WHERE attempt_id IN (SELECT value FROM json_each(?))
			  AND type IN ('phase_start', 'phase_end', 'phase_death') AND phase != ''
			GROUP BY attempt_id, phase
		) last ON last.attempt_id = e.attempt_id AND last.last_seq = e.seq
		ORDER BY e.attempt_id, last.first_seq
	`, attemptIDs)
	if err != nil {
		return nil, unavailable(err)
	}
	defer rows.Close()
	for rows.Next() {
		var attemptID, phase, kind string
		var body []byte
		var rounds int
		var completed bool
		if err := rows.Scan(&attemptID, &phase, &kind, &body, &rounds, &completed); err != nil {
			return nil, unavailable(err)
		}
		status := "running"
		if completed {
			status = "stopped"
		}
		switch kind {
		case protocol.EventPhaseDeath:
			status = "death"
		case protocol.EventPhaseEnd:
			// The lane reads phase_end's payload status and shows "unknown"
			// when it is missing; so does the list.
			var event struct {
				Payload struct {
					Status string `json:"status"`
				} `json:"payload"`
			}
			status = "unknown"
			if json.Unmarshal(body, &event) == nil && event.Payload.Status != "" {
				status = event.Payload.Status
			}
		}
		set(attemptID, phase, status, rounds)
	}
	if err := rows.Err(); err != nil {
		return nil, unavailable(err)
	}

	// p.type filters out a `phases` that is not an array of objects: a result
	// that does not parse, or parses to some other shape, adds nothing.
	results, err := tx.QueryContext(ctx, `
		SELECT a.id, json_extract(p.value, '$.phase'), json_extract(p.value, '$.status')
		FROM attempts a,
		     json_each(CASE WHEN json_valid(a.result) THEN a.result ELSE '{}' END, '$.phases') p
		WHERE a.id IN (SELECT value FROM json_each(?)) AND p.type = 'object'
		ORDER BY a.id, p.id
	`, attemptIDs)
	if err != nil {
		return nil, unavailable(err)
	}
	defer results.Close()
	for results.Next() {
		var attemptID string
		var phase, status sql.NullString
		if err := results.Scan(&attemptID, &phase, &status); err != nil {
			return nil, unavailable(err)
		}
		if phase.String == "" || status.String == "" {
			continue
		}
		set(attemptID, phase.String, status.String, 1)
	}
	if err := results.Err(); err != nil {
		return nil, unavailable(err)
	}
	return progress, nil
}

// jsonList encodes IDs as one JSON array argument for `IN (SELECT value FROM
// json_each(?))`: one bound parameter however long the page, so the query
// text — and SQLite's variable limit — never depend on the page size.
func jsonList(ids []string) string {
	if len(ids) == 0 {
		return "[]"
	}
	body, _ := json.Marshal(ids)
	return string(body)
}

// prefixedScanner lets scanAttempt read a row that carries extra leading
// columns, so the attempt column list and its scan stay defined once.
type prefixedScanner struct {
	rows   *sql.Rows
	prefix []any
}

func (p prefixedScanner) Scan(dest ...any) error {
	return p.rows.Scan(append(append([]any{}, p.prefix...), dest...)...)
}

// runStationMemo caches each run's parsed chain and publish policy. A run's
// snapshot is written once at admission and never updated, so the parse is
// a pure function of the run ID; without the memo every poll of the list
// re-parsed every listed run's YAML. The memo is cleared whole when it
// reaches maxMemoRuns — the list reads at most MaxRunListLimit runs a page,
// so a clear costs one re-parse per run on the next poll, and the memo
// never grows with the ledger.
type runStationMemo struct {
	mu   sync.Mutex
	runs map[string]memoStations
}

type memoStations struct {
	stations []Station
	delivery *Delivery
}

const maxMemoRuns = 4 * MaxRunListLimit

// stations returns the run's chain and policy. Callers share the returned
// slice and pointer and must not modify them; the list only encodes them.
func (m *runStationMemo) stations(runID, snapshot string) ([]Station, *Delivery) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if cached, ok := m.runs[runID]; ok {
		return cached.stations, cached.delivery
	}
	stations, delivery := stationsFromSnapshot(snapshot)
	if m.runs == nil || len(m.runs) >= maxMemoRuns {
		m.runs = map[string]memoStations{}
	}
	m.runs[runID] = memoStations{stations: stations, delivery: delivery}
	return stations, delivery
}
