package controlplane

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/StructuPath/JIG-Software-Factory/internal/protocol"
)

// runListSnapshot has two phases and a publish policy, so a summary carries
// both a chain and a delivery block.
const runListSnapshot = `name: list-fixture
roster:
  builder:
    model: claude-sonnet
    system_prompt: build
    user_prompt: build it
phases:
  - name: build
    kind: agent
    owner: builder
  - name: test
    kind: code
    owner: builder
    command: go test ./...
publish:
  ci:
    wait: true
`

// seedListJob inserts one job of runID with the given attempt states, oldest
// first, so a test controls the attempt history directly.
func seedListJob(t *testing.T, store *Store, runID, jobID, jobState string, attemptStates ...string) {
	t.Helper()
	now := store.now().UnixMilli()
	if _, err := store.db.Exec(`
		INSERT INTO jobs(id, run_id, repository, base_sha, state, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
	`, jobID, runID, "github.com/acme/"+jobID, "abc123", jobState, now, now); err != nil {
		t.Fatalf("seed job %s: %v", jobID, err)
	}
	for i, state := range attemptStates {
		started := now + int64(i)*1000
		var completed any
		if state != protocol.AttemptQueued && state != "running" {
			completed = started + 500
		}
		if _, err := store.db.Exec(`
			INSERT INTO attempts(id, job_id, attempt_number, state, result, started_at, completed_at, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		`, fmt.Sprintf("%s-a%d", jobID, i+1), jobID, i+1, state,
			fmt.Sprintf(`{"phases":[{"phase":"build","status":"success","round":%d}]}`, i+1),
			started, completed, now); err != nil {
			t.Fatalf("seed attempt %d of %s: %v", i+1, jobID, err)
		}
	}
}

func seedPublishRecord(t *testing.T, store *Store, attemptID, step, prURL string) {
	t.Helper()
	if _, err := store.db.Exec(`
		INSERT INTO publish_records(attempt_id, step, branch, pr_url, completed_at)
		VALUES (?, ?, ?, ?, ?)
	`, attemptID, step, "jig/"+attemptID, prURL, store.now().UnixMilli()); err != nil {
		t.Fatalf("seed publish record: %v", err)
	}
}

func TestRunListSummarizesEachJobsLatestAttemptForZeroOneAndManyAttempts(t *testing.T) {
	store, _ := newTestStore(t)
	seedRunSnapshot(t, store, "run-empty", runListSnapshot)
	seedRunSnapshot(t, store, "run-none", runListSnapshot)
	seedListJob(t, store, "run-none", "job-none", protocol.JobQueued)
	seedRunSnapshot(t, store, "run-one", runListSnapshot)
	seedListJob(t, store, "run-one", "job-one", protocol.JobAccepted, protocol.AttemptAccepted)
	seedPublishRecord(t, store, "job-one-a1", "pull_request", "https://github.com/acme/one/pull/7")
	seedRunSnapshot(t, store, "run-many", runListSnapshot)
	seedListJob(t, store, "run-many", "job-many", protocol.JobActive,
		protocol.AttemptFailed, protocol.AttemptFailed, "running")
	// A record on an earlier attempt must not leak onto the latest one's row.
	seedPublishRecord(t, store, "job-many-a1", "pull_request", "https://github.com/acme/many/pull/1")

	handler := NewHandler(store, "", nil)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest("GET", "http://127.0.0.1:8383/api/runs", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET /api/runs: status %d body %s", recorder.Code, recorder.Body.String())
	}

	// The existing run fields stay flat on each item, so today's UI reads the
	// list unchanged.
	var flat []protocol.Run
	if err := json.Unmarshal(recorder.Body.Bytes(), &flat); err != nil {
		t.Fatalf("decode as []Run: %v", err)
	}
	if len(flat) != 4 || flat[0].Snapshot == "" || flat[0].State == "" || flat[0].DefinitionID == "" {
		t.Fatalf("flat run fields missing: %+v", flat)
	}

	var items []RunListItem
	if err := json.Unmarshal(recorder.Body.Bytes(), &items); err != nil {
		t.Fatalf("decode as []RunListItem: %v", err)
	}
	byRun := map[string]RunListItem{}
	for _, item := range items {
		byRun[item.ID] = item
	}

	if got := byRun["run-empty"].JobSummaries; got == nil || len(got) != 0 {
		t.Fatalf("run without jobs: summaries %+v, want an empty list", got)
	}

	none := byRun["run-none"].JobSummaries
	if len(none) != 1 || none[0].Job.ID != "job-none" || none[0].LatestAttempt != nil || none[0].AttemptCount != 0 {
		t.Fatalf("job without attempts: %+v, want the job with no latest attempt", none)
	}
	if none[0].Publish == nil || len(none[0].Stations) != 2 {
		t.Fatalf("job without attempts: publish %v stations %v, want [] and the chain", none[0].Publish, none[0].Stations)
	}

	one := byRun["run-one"].JobSummaries
	if len(one) != 1 || one[0].LatestAttempt == nil || one[0].LatestAttempt.ID != "job-one-a1" || one[0].AttemptCount != 1 {
		t.Fatalf("one attempt: %+v", one)
	}
	if one[0].Job.State != protocol.JobAccepted || one[0].LatestAttempt.State != protocol.AttemptAccepted ||
		one[0].LatestAttempt.StartedAt == nil || one[0].LatestAttempt.CompletedAt == nil || one[0].LatestAttempt.ResultExcerpt == "" {
		t.Fatalf("one attempt: state, timestamps or result excerpt missing: %+v", one[0].LatestAttempt)
	}
	if one[0].LatestAttempt.Result != "" || one[0].LatestAttempt.ResultTruncated {
		t.Fatalf("one attempt: summary carries the result body %q (truncated %v), want only the excerpt",
			one[0].LatestAttempt.Result, one[0].LatestAttempt.ResultTruncated)
	}
	if fmt.Sprint(one[0].Phases) != "[{build success 1}]" {
		t.Fatalf("one attempt: phases %+v, want build passed from the result", one[0].Phases)
	}
	if len(one[0].Publish) != 1 || one[0].Publish[0].PullRequestURL != "https://github.com/acme/one/pull/7" {
		t.Fatalf("one attempt: publish %+v, want the PR record", one[0].Publish)
	}
	if one[0].Delivery == nil || !one[0].Delivery.CIWait {
		t.Fatalf("one attempt: delivery %+v, want ci_wait from the snapshot", one[0].Delivery)
	}
	if one[0].Stations[0].Name != "build" || one[0].Stations[0].Model != "claude-sonnet" || one[0].Stations[1].Kind != "code" {
		t.Fatalf("one attempt: stations %+v, want the frozen chain", one[0].Stations)
	}

	many := byRun["run-many"].JobSummaries
	if len(many) != 1 || many[0].LatestAttempt == nil || many[0].LatestAttempt.ID != "job-many-a3" ||
		many[0].LatestAttempt.AttemptNumber != 3 || many[0].AttemptCount != 3 {
		t.Fatalf("many attempts: %+v, want attempt 3 of 3", many)
	}
	if many[0].LatestAttempt.State != "running" || many[0].LatestAttempt.CompletedAt != nil {
		t.Fatalf("many attempts: latest %+v, want the running attempt", many[0].LatestAttempt)
	}
	if len(many[0].Publish) != 0 {
		t.Fatalf("many attempts: publish %+v, want none — the PR belongs to attempt 1", many[0].Publish)
	}

	// The summary carries what GET /api/jobs/:id carries for the same job, so
	// the UI can fold stations from either without a second vocabulary.
	detail, err := store.JobDetail(context.Background(), "job-one")
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(detail.Stations) != fmt.Sprint(one[0].Stations) ||
		*detail.Delivery != *one[0].Delivery ||
		detail.Attempts[len(detail.Attempts)-1].ID != one[0].LatestAttempt.ID ||
		fmt.Sprint(detail.Publish) != fmt.Sprint(one[0].Publish) {
		t.Fatalf("summary differs from job detail:\n detail %+v\n summary %+v", detail, one[0])
	}
}

func TestRunListQueryCountDoesNotGrowWithTheNumberOfRuns(t *testing.T) {
	store, _ := newTestStore(t)
	countQueries := func() int64 {
		var queries atomic.Int64
		counting := countingDB(t, store, &queries)
		original := store.db
		store.db = counting
		defer func() { store.db = original }()
		if _, _, err := store.RunList(context.Background(), RunListPage{}); err != nil {
			t.Fatalf("run list: %v", err)
		}
		return queries.Load()
	}

	addRuns := func(from, to int) {
		for i := from; i < to; i++ {
			runID := fmt.Sprintf("run-%03d", i)
			seedRunSnapshot(t, store, runID, runListSnapshot)
			seedListJob(t, store, runID, fmt.Sprintf("job-%03d", i), protocol.JobFailed,
				protocol.AttemptFailed, protocol.AttemptFailed)
			seedPublishRecord(t, store, fmt.Sprintf("job-%03d-a2", i), "push", "")
		}
	}
	addRuns(0, 2)
	few := countQueries()
	addRuns(2, 150)
	many := countQueries()
	if few != many {
		t.Fatalf("queries: %d for 2 runs, %d for 150 runs; want the same count", few, many)
	}
	// Zero would mean the counter saw nothing, not that the read is cheap.
	if many != 6 {
		t.Fatalf("queries: %d, want 6 (runs, jobs, latest attempts, publish records, phase events, result phases)", many)
	}

	items, _, err := store.RunList(context.Background(), RunListPage{})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 150 {
		t.Fatalf("items: %d, want 150", len(items))
	}
	for _, item := range items {
		if len(item.JobSummaries) != 1 || item.JobSummaries[0].AttemptCount != 2 ||
			item.JobSummaries[0].LatestAttempt.AttemptNumber != 2 || len(item.JobSummaries[0].Publish) != 1 {
			t.Fatalf("run %s: summary %+v, want attempt 2 of 2 with its push record", item.ID, item.JobSummaries)
		}
	}
}

// countingDB opens a second handle on the store's database whose connections
// count every query, so a test can prove a read's query count.
func countingDB(t *testing.T, store *Store, queries *atomic.Int64) *sql.DB {
	t.Helper()
	var path string
	if err := store.db.QueryRow(`SELECT file FROM pragma_database_list WHERE name = 'main'`).Scan(&path); err != nil {
		t.Fatalf("database path: %v", err)
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		t.Fatal(err)
	}
	dsn := (&url.URL{Scheme: "file", Path: absolute}).String() + "?" + sqlitePragmas
	db := sql.OpenDB(countingConnector{base: store.db.Driver(), dsn: dsn, queries: queries})
	t.Cleanup(func() { db.Close() })
	return db
}

type countingConnector struct {
	base    driver.Driver
	dsn     string
	queries *atomic.Int64
}

func (c countingConnector) Connect(context.Context) (driver.Conn, error) {
	conn, err := c.base.Open(c.dsn)
	if err != nil {
		return nil, err
	}
	return countingConn{Conn: conn, queries: c.queries}, nil
}

func (c countingConnector) Driver() driver.Driver { return c.base }

// countingConn counts at QueryContext and PrepareContext: database/sql sends
// every query through one or the other.
type countingConn struct {
	driver.Conn
	queries *atomic.Int64
}

func (c countingConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	queryer, ok := c.Conn.(driver.QueryerContext)
	if !ok {
		return nil, driver.ErrSkip
	}
	c.queries.Add(1)
	return queryer.QueryContext(ctx, query, args)
}

func (c countingConn) PrepareContext(ctx context.Context, query string) (driver.Stmt, error) {
	c.queries.Add(1)
	if preparer, ok := c.Conn.(driver.ConnPrepareContext); ok {
		return preparer.PrepareContext(ctx, query)
	}
	return c.Conn.Prepare(query)
}

// BeginTx passes transaction options through, so the list's read-only
// transaction opens on a counting connection as it does on the store's own.
func (c countingConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	return c.Conn.(driver.ConnBeginTx).BeginTx(ctx, opts)
}

// A retry between the list's job read and its attempt read showed a job
// failed with its latest attempt queued. The list reads one snapshot, so
// every row is a state the ledger was in while retries land concurrently.
func TestRunListIsOneSnapshotWhileRetriesLand(t *testing.T) {
	store, _ := newTestStore(t)
	const jobs = 8
	seedRunSnapshot(t, store, "run-retry", runListSnapshot)
	for i := 0; i < jobs; i++ {
		seedListJob(t, store, "run-retry", fmt.Sprintf("job-%d", i), protocol.JobFailed, protocol.AttemptFailed)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	writerDone := make(chan error, 1)
	go func() {
		// Retry and fail each job in turn: the job flips failed → queued with
		// a new queued attempt, then both fail again in one transaction.
		for round := 0; round < 40 && ctx.Err() == nil; round++ {
			for i := 0; i < jobs; i++ {
				jobID := fmt.Sprintf("job-%d", i)
				if _, err := store.RetryJob(ctx, jobID); err != nil {
					writerDone <- fmt.Errorf("retry %s: %w", jobID, err)
					return
				}
				tx, err := store.db.BeginTx(ctx, nil)
				if err != nil {
					writerDone <- err
					return
				}
				now := store.now().UnixMilli()
				if _, err := tx.Exec(`UPDATE attempts SET state = 'failed', completed_at = ?
					WHERE job_id = ? AND state = 'queued'`, now, jobID); err != nil {
					tx.Rollback()
					writerDone <- err
					return
				}
				if _, err := tx.Exec(`UPDATE jobs SET state = 'failed' WHERE id = ?`, jobID); err != nil {
					tx.Rollback()
					writerDone <- err
					return
				}
				if err := tx.Commit(); err != nil {
					writerDone <- err
					return
				}
			}
		}
		writerDone <- nil
	}()

	reads := 0
	for {
		select {
		case err := <-writerDone:
			if err != nil {
				t.Fatalf("writer: %v", err)
			}
			if reads == 0 {
				t.Fatal("the writer finished before any list read; the test proved nothing")
			}
			return
		default:
		}
		items, _, err := store.RunList(ctx, RunListPage{})
		if err != nil {
			t.Fatalf("run list: %v", err)
		}
		reads++
		for _, summary := range items[0].JobSummaries {
			attempt := summary.LatestAttempt
			if attempt == nil {
				t.Fatalf("read %d: job %s has no latest attempt", reads, summary.Job.ID)
			}
			consistent := (summary.Job.State == protocol.JobFailed && attempt.State == protocol.AttemptFailed) ||
				(summary.Job.State == protocol.JobQueued && attempt.State == protocol.AttemptQueued)
			if !consistent || summary.AttemptCount != attempt.AttemptNumber {
				t.Fatalf("read %d: job %s is %s with latest attempt %d of %d %s; want one snapshot",
					reads, summary.Job.ID, summary.Job.State, attempt.AttemptNumber, summary.AttemptCount, attempt.State)
			}
		}
	}
}

// An active attempt has no result, and the stations are definitions only.
// The summary's phases come from the trace, so a running job shows how far
// it has got: build passed, test running.
func TestRunListShowsPhaseProgressOfAnActiveAttemptFromItsTrace(t *testing.T) {
	store, _ := newTestStore(t)
	seedRunSnapshot(t, store, "run-live", runListSnapshot)
	seedListJob(t, store, "run-live", "job-live", protocol.JobActive, "running")
	if _, err := store.db.Exec(`UPDATE attempts SET result = NULL WHERE id = 'job-live-a1'`); err != nil {
		t.Fatal(err)
	}

	read := func() JobSummary {
		t.Helper()
		items, _, err := store.RunList(context.Background(), RunListPage{})
		if err != nil {
			t.Fatal(err)
		}
		return items[0].JobSummaries[0]
	}
	if got := read(); got.Phases == nil || len(got.Phases) != 0 {
		t.Fatalf("before any phase: phases %+v, want an empty list", got.Phases)
	}

	status := func(value string) json.RawMessage { return json.RawMessage(fmt.Sprintf(`{"status":%q}`, value)) }
	if err := store.AppendEvents(context.Background(), "job-live-a1", []protocol.Event{
		{Seq: 1, Type: protocol.EventPhaseStart, Phase: "build"},
		{Seq: 2, Type: protocol.EventToolCall, Phase: "build", Name: "Edit"},
		{Seq: 3, Type: protocol.EventPhaseEnd, Phase: "build", Payload: status(protocol.EnvelopeSuccess)},
		{Seq: 4, Type: protocol.EventPhaseStart, Phase: "test"},
		{Seq: 5, Type: protocol.EventLog, Phase: "test", Name: "agent_text"},
	}); err != nil {
		t.Fatal(err)
	}
	live := read()
	if live.LatestAttempt == nil || live.LatestAttempt.ResultExcerpt != "" || live.LatestAttempt.ResultTruncated {
		t.Fatalf("active attempt: %+v, want no result", live.LatestAttempt)
	}
	if fmt.Sprint(live.Phases) != "[{build success 1} {test running 1}]" {
		t.Fatalf("active attempt: phases %+v, want build success then test running", live.Phases)
	}

	// A repair loop re-enters build; a died phase reads as the lane reads it.
	if err := store.AppendEvents(context.Background(), "job-live-a1", []protocol.Event{
		{Seq: 6, Type: protocol.EventPhaseEnd, Phase: "test", Payload: status(protocol.EnvelopeFail)},
		{Seq: 7, Type: protocol.EventPhaseStart, Phase: "build"},
		{Seq: 8, Type: protocol.EventPhaseDeath, Phase: "build"},
	}); err != nil {
		t.Fatal(err)
	}
	if got := read(); fmt.Sprint(got.Phases) != "[{build death 2} {test fail 1}]" {
		t.Fatalf("after repair: phases %+v, want build died in round 2, test failed", got.Phases)
	}
}

func TestRunListCutsResultAndErrorToAFirstLineExcerpt(t *testing.T) {
	store, _ := newTestStore(t)
	seedRunSnapshot(t, store, "run-big", runListSnapshot)
	seedListJob(t, store, "run-big", "job-big", protocol.JobAcceptedUnpublished, protocol.AttemptAcceptedUnpublished)
	envelope := strings.Repeat("x", 64<<10)
	result := fmt.Sprintf(`{"phases":[{"phase":"build","status":"success","envelope":{"pad":%q}},`+
		`{"phase":"test","status":"success"}],"publish":{"state":"failed","code":"ci_red","detail":"long"}}`, envelope)
	errorText := strings.Repeat("é", 400) + "\nsecond line"
	if _, err := store.db.Exec(`UPDATE attempts SET result = ?, error = ? WHERE id = 'job-big-a1'`, result, errorText); err != nil {
		t.Fatal(err)
	}

	handler := NewHandler(store, "", nil)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest("GET", "http://127.0.0.1:8383/api/runs", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status %d: %s", recorder.Code, recorder.Body.String())
	}
	if recorder.Body.Len() > 16<<10 {
		t.Fatalf("response is %d bytes; a 64 KiB result must not reach the list", recorder.Body.Len())
	}
	if strings.Contains(recorder.Body.String(), `"result":`) || strings.Contains(recorder.Body.String(), `"error":`) {
		t.Fatalf("response carries a result or error body: %s", recorder.Body.String())
	}
	var items []RunListItem
	if err := json.Unmarshal(recorder.Body.Bytes(), &items); err != nil {
		t.Fatal(err)
	}
	attempt := items[0].JobSummaries[0].LatestAttempt
	if utf8.RuneCountInString(attempt.ResultExcerpt) != ExcerptRunes || !strings.HasPrefix(result, attempt.ResultExcerpt) ||
		!attempt.ResultTruncated {
		t.Fatalf("result excerpt %q (truncated %v), want the first %d runes, flagged", attempt.ResultExcerpt,
			attempt.ResultTruncated, ExcerptRunes)
	}
	if attempt.ErrorExcerpt != strings.Repeat("é", ExcerptRunes) || !attempt.ErrorTruncated {
		t.Fatalf("error excerpt %q (truncated %v), want %d runes of the first line, flagged",
			attempt.ErrorExcerpt, attempt.ErrorTruncated, ExcerptRunes)
	}
	if attempt.PublishState != "failed" || attempt.PublishCode != "ci_red" {
		t.Fatalf("publish outcome %q/%q, want failed/ci_red from the result", attempt.PublishState, attempt.PublishCode)
	}
	if got := items[0].JobSummaries[0].Phases; fmt.Sprint(got) != "[{build success 1} {test success 1}]" {
		t.Fatalf("phases %+v, want both from the result", got)
	}

	// A short single-line body is whole, not flagged; a bare-string publish
	// outcome is its state; a result that is not JSON adds nothing.
	if _, err := store.db.Exec(`UPDATE attempts SET result = '{"publish":"held"}', error = 'boom' WHERE id = 'job-big-a1'`); err != nil {
		t.Fatal(err)
	}
	brief := runListSummary(t, store).LatestAttempt
	if brief.ResultExcerpt != `{"publish":"held"}` || brief.ResultTruncated || brief.ErrorExcerpt != "boom" ||
		brief.ErrorTruncated || brief.PublishState != "held" || brief.PublishCode != "" {
		t.Fatalf("short bodies: %+v", brief)
	}
	if _, err := store.db.Exec(`UPDATE attempts SET result = 'not json' WHERE id = 'job-big-a1'`); err != nil {
		t.Fatal(err)
	}
	if summary := runListSummary(t, store); summary.LatestAttempt.PublishState != "" || len(summary.Phases) != 0 ||
		summary.LatestAttempt.ResultExcerpt != "not json" {
		t.Fatalf("unparsable result: %+v", summary)
	}
}

func runListSummary(t *testing.T, store *Store) JobSummary {
	t.Helper()
	items, _, err := store.RunList(context.Background(), RunListPage{})
	if err != nil {
		t.Fatal(err)
	}
	return items[0].JobSummaries[0]
}

// listPage requests GET /api/runs with query and returns the run IDs and the
// next-page cursor.
func listPage(t *testing.T, handler http.Handler, query string) ([]string, string, int) {
	t.Helper()
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest("GET", "http://127.0.0.1:8383/api/runs"+query, nil))
	if recorder.Code != http.StatusOK {
		return nil, "", recorder.Code
	}
	var items []RunListItem
	if err := json.Unmarshal(recorder.Body.Bytes(), &items); err != nil {
		t.Fatal(err)
	}
	ids := make([]string, len(items))
	for i, item := range items {
		ids[i] = item.ID
	}
	return ids, recorder.Header().Get("X-Next-Before"), recorder.Code
}

func TestRunListPagesNewestFirstWithABeforeCursor(t *testing.T) {
	store, clock := newTestStore(t)
	// Runs 0-2 share a created_at, so the cursor must break ties by ID.
	for i := 0; i < 7; i++ {
		if i >= 3 {
			clock.Advance(time.Second)
		}
		seedRunSnapshot(t, store, fmt.Sprintf("run-%d", i), runListSnapshot)
		seedListJob(t, store, fmt.Sprintf("run-%d", i), fmt.Sprintf("job-%d", i), protocol.JobFailed, protocol.AttemptFailed)
	}
	handler := NewHandler(store, "", nil)
	want := []string{"run-6", "run-5", "run-4", "run-3", "run-0", "run-1", "run-2"}

	all, next, _ := listPage(t, handler, "")
	if fmt.Sprint(all) != fmt.Sprint(want) || next != "" {
		t.Fatalf("default page: %v next %q, want %v and no cursor", all, next, want)
	}
	// Exactly the remaining runs: the last page, so no cursor.
	if ids, next, _ := listPage(t, handler, "?limit=7"); len(ids) != 7 || next != "" {
		t.Fatalf("limit = count: %v next %q, want every run and no cursor", ids, next)
	}

	var walked []string
	cursor := ""
	for pages := 0; ; pages++ {
		query := "?limit=2"
		if cursor != "" {
			query += "&before=" + cursor
		}
		ids, next, code := listPage(t, handler, query)
		if code != http.StatusOK || len(ids) == 0 || len(ids) > 2 || pages > 4 {
			t.Fatalf("page %d (%s): status %d ids %v", pages, query, code, ids)
		}
		walked = append(walked, ids...)
		if next == "" {
			break
		}
		if next != ids[len(ids)-1] {
			t.Fatalf("page %d: cursor %q, want the page's last run %q", pages, next, ids[len(ids)-1])
		}
		cursor = next
	}
	if fmt.Sprint(walked) != fmt.Sprint(want) {
		t.Fatalf("paged walk %v, want %v with no run skipped or repeated", walked, want)
	}
	// The cursor at a created_at tie continues inside the tie.
	if ids, _, _ := listPage(t, handler, "?before=run-0"); fmt.Sprint(ids) != "[run-1 run-2]" {
		t.Fatalf("before run-0: %v, want [run-1 run-2]", ids)
	}
	if ids, next, _ := listPage(t, handler, "?before=run-2"); len(ids) != 0 || next != "" {
		t.Fatalf("before the oldest run: %v next %q, want an empty last page", ids, next)
	}

	for _, query := range []string{"?limit=0", "?limit=-1", "?limit=ten", "?before=run-missing"} {
		if _, _, code := listPage(t, handler, query); code != http.StatusBadRequest {
			t.Fatalf("%s: status %d, want 400", query, code)
		}
	}
}

func TestRunListCapsTheLimitAndScopesEveryQueryToThePage(t *testing.T) {
	store, clock := newTestStore(t)
	const total = MaxRunListLimit + 20
	for i := 0; i < total; i++ {
		clock.Advance(time.Millisecond)
		runID := fmt.Sprintf("run-%03d", i)
		seedRunSnapshot(t, store, runID, runListSnapshot)
		seedListJob(t, store, runID, fmt.Sprintf("job-%03d", i), protocol.JobFailed, protocol.AttemptFailed)
	}
	handler := NewHandler(store, "", nil)
	ids, next, _ := listPage(t, handler, fmt.Sprintf("?limit=%d", MaxRunListLimit*5))
	if len(ids) != MaxRunListLimit || next != ids[len(ids)-1] {
		t.Fatalf("limit above the cap: %d runs next %q, want %d and a cursor", len(ids), next, MaxRunListLimit)
	}
	rest, next, _ := listPage(t, handler, "?before="+next)
	if len(rest) != total-MaxRunListLimit || next != "" || rest[len(rest)-1] != "run-000" {
		t.Fatalf("second page: %d runs next %q, want the oldest %d", len(rest), next, total-MaxRunListLimit)
	}

	// A cursor adds one lookup; the summary queries stay fixed.
	var queries atomic.Int64
	original := store.db
	store.db = countingDB(t, store, &queries)
	defer func() { store.db = original }()
	items, _, err := store.RunList(context.Background(), RunListPage{Limit: 3, Before: "run-100"})
	if err != nil {
		t.Fatal(err)
	}
	if queries.Load() != 7 {
		t.Fatalf("queries with a cursor: %d, want 7", queries.Load())
	}
	if len(items) != 3 || items[0].ID != "run-099" || len(items[2].JobSummaries) != 1 ||
		items[2].JobSummaries[0].Job.ID != "job-097" || items[2].JobSummaries[0].LatestAttempt == nil {
		t.Fatalf("cursor page: %+v, want runs 099-097 with their summaries", items)
	}
}

// The station list is parsed once per run, not once per poll.
func TestRunListParsesEachRunSnapshotOnce(t *testing.T) {
	store, _ := newTestStore(t)
	seedRunSnapshot(t, store, "run-memo", runListSnapshot)
	seedListJob(t, store, "run-memo", "job-memo", protocol.JobQueued)
	first := runListSummary(t, store).Stations
	second := runListSummary(t, store).Stations
	if len(first) != 2 || &first[0] != &second[0] {
		t.Fatalf("stations %p then %p, want the memoized slice", first, second)
	}
}
