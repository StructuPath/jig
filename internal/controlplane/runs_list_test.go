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
	"sync/atomic"
	"testing"

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
		one[0].LatestAttempt.StartedAt == nil || one[0].LatestAttempt.CompletedAt == nil || one[0].LatestAttempt.Result == "" {
		t.Fatalf("one attempt: state, timestamps or result missing: %+v", one[0].LatestAttempt)
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
		if _, err := store.RunList(context.Background()); err != nil {
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
	if many != 4 {
		t.Fatalf("queries: %d, want 4 (runs, jobs, latest attempts, publish records)", many)
	}

	items, err := store.RunList(context.Background())
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
